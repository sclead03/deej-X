package deej

import (
	"fmt"
	"io"
	"sync"
	"time"

	"go.uber.org/zap"
)

const (
	hidVendorID  = 0x1209
	hidProductID = 0x0001

	hidReconnectDelay = 2 * time.Second

	// micMuteReportID is the report ID SERENITY's RGB button press sends
	// (firmware's kMicMuteDesc, report ID 4) - distinct from the Consumer
	// Control Play/Pause report (ID 3) the encoder double-click sends, which
	// arrives on this same shared HID interface and must be ignored here.
	micMuteReportID = 0x04

	// Sentinels used in mic_mute.mute_action / mic_mute.unmute_action config lists.
	micMuteSentinelAll   = "mute.all"
	micUnmuteSentinelAll = "unmute.all"
)

// MicMuter applies mute/unmute to configured capture devices and reports current state.
type MicMuter interface {
	MuteDevices(targets []string) error
	UnmuteDevices(targets []string) error
	IsMuted() (bool, error) // true only if ALL active capture devices are muted
}

// HIDManager reads reports from the SERENITY HID interface and dispatches actions.
type HIDManager struct {
	deej   *Deej
	logger *zap.SugaredLogger
	muter  MicMuter
	stopCh chan struct{}

	// stateMu protects currentMuted and currentMutedKnown: the HID readLoop
	// goroutine writes them (handleReport, applyMicMuteAction) while the
	// display.go goroutine also writes (SetCurrentMuteState) and reads (IsMicMuted)
	// them concurrently.
	stateMu           sync.Mutex
	currentMuted      bool
	currentMutedKnown bool
}

// NewHIDManager creates a HIDManager.
func NewHIDManager(deej *Deej, logger *zap.SugaredLogger) (*HIDManager, error) {
	logger = logger.Named("hid")

	muter, err := newMicMuter(logger)
	if err != nil {
		return nil, fmt.Errorf("create mic muter: %w", err)
	}

	return &HIDManager{
		deej:   deej,
		logger: logger,
		muter:  muter,
		stopCh: make(chan struct{}),
	}, nil
}

// Start begins watching for the SERENITY HID device in the background.
func (h *HIDManager) Start() {
	go h.run()
}

// Stop shuts down the HID manager.
func (h *HIDManager) Stop() {
	close(h.stopCh)
}

// IsMicMuted returns the current mic mute state. Uses the tracked state if a
// button press or external change has been observed; otherwise queries the
// default capture device (connect-time init path).
func (h *HIDManager) IsMicMuted() (bool, error) {
	h.stateMu.Lock()
	known, muted := h.currentMutedKnown, h.currentMuted
	h.stateMu.Unlock()
	if known {
		return muted, nil
	}
	return h.muter.IsMuted()
}

// SetCurrentMuteState records an externally-observed mic mute state change
// (e.g. from the Windows volume mixer) so that the next button press
// correctly toggles from the real current state rather than a stale one.
func (h *HIDManager) SetCurrentMuteState(muted bool) {
	h.stateMu.Lock()
	h.currentMuted = muted
	h.currentMutedKnown = true
	h.stateMu.Unlock()
}

func (h *HIDManager) run() {
	h.logger.Debug("HID manager started")

	for {
		select {
		case <-h.stopCh:
			h.logger.Debug("HID manager stopping")
			return
		default:
		}

		dev, err := openSERENITY()
		if err != nil {
			h.logger.Debugw("SERENITY HID device not found, retrying", "delay", hidReconnectDelay)
			select {
			case <-h.stopCh:
				return
			case <-time.After(hidReconnectDelay):
			}
			continue
		}

		h.logger.Info("SERENITY HID device connected")
		h.readLoop(dev)
		h.logger.Info("SERENITY HID device disconnected")
	}
}

func (h *HIDManager) readLoop(r io.ReadCloser) {
	type result struct {
		data []byte
		err  error
	}

	done := make(chan struct{})
	ch := make(chan result)

	go func() {
		buf := make([]byte, 64)
		for {
			n, err := r.Read(buf)
			if err != nil {
				select {
				case ch <- result{err: err}:
				case <-done:
				}
				return
			}
			if n > 0 {
				data := make([]byte, n)
				copy(data, buf[:n])
				select {
				case ch <- result{data: data}:
				case <-done:
					return
				}
			}
		}
	}()

	defer close(done)
	defer r.Close()

	for {
		select {
		case <-h.stopCh:
			return
		case res := <-ch:
			if res.err != nil {
				return
			}
			h.handleReport(res.data)
		}
	}
}

// applyMicMuteAction force-mutes or unmutes per the configured mute_action /
// unmute_action targets, updates tracked state, and pushes the new state to
// SERENITY. Called from handleReport (RGB button) and display.go's
// handleMicMuteActionRequest (encoder gesture 0x0A).
func (h *HIDManager) applyMicMuteAction(muted bool) {
	// Mark before applying: SetMute's COM notification can fire synchronously
	// on this call stack before MuteDevices/UnmuteDevices returns, so the
	// suppression window must already be open (see session_map.go).
	h.deej.sessions.markMicMuteSetByButton()

	cfg := h.deej.config.MicMute
	var err error
	if muted {
		err = h.muter.MuteDevices(cfg.MuteAction)
	} else {
		err = h.muter.UnmuteDevices(cfg.UnmuteAction)
	}
	if err != nil {
		h.logger.Warnw("Failed to apply mic mute action", "muting", muted, "error", err)
		return
	}

	// Read back the true aggregate state rather than trusting the intent —
	// mute_action may target a subset of devices, so the post-apply aggregate
	// is what the OLED icon should reflect.
	if readback, err := h.muter.IsMuted(); err != nil {
		h.logger.Warnw("Failed to read back aggregate mic mute state, using intent", "error", err)
	} else {
		muted = readback
	}

	h.stateMu.Lock()
	h.currentMuted = muted
	h.currentMutedKnown = true
	h.stateMu.Unlock()

	writer := h.deej.serial.Writer()
	if writer == nil {
		return
	}
	if err := writer.SendMicMuteState(muted); err != nil {
		h.logger.Warnw("Failed to push mic mute state", "error", err)
		return
	}
	h.logger.Debugw("Pushed mic mute state", "muted", muted)
}

func (h *HIDManager) handleReport(report []byte) {
	if len(report) == 0 || report[0] != micMuteReportID {
		h.logger.Debugw("Ignoring HID report not for mic mute", "report", report)
		return
	}

	h.logger.Debug("Received mic-mute HID report")

	switch h.deej.config.RGBButtonAction {
	case "mute_mic":
		h.applyMicMuteAction(true)
	case "unmute_mic":
		h.applyMicMuteAction(false)
	case "masterVol_mute":
		h.deej.display.handleMasterMuteToggleRequest()
	default: // "mic_mute_toggle"
		// Toggle from the last known state; query the OS if we haven't seen a
		// change yet so we start in the right direction.
		h.stateMu.Lock()
		known := h.currentMutedKnown
		h.stateMu.Unlock()
		if !known {
			if queried, err := h.muter.IsMuted(); err == nil {
				h.stateMu.Lock()
				h.currentMuted = queried
				h.currentMutedKnown = true
				h.stateMu.Unlock()
			} else {
				h.stateMu.Lock()
				h.currentMutedKnown = true
				h.stateMu.Unlock()
			}
		}
		h.stateMu.Lock()
		nowMuted := !h.currentMuted
		h.stateMu.Unlock()
		h.applyMicMuteAction(nowMuted)
	}
}
