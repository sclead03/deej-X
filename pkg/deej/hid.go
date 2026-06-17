package deej

import (
	"fmt"
	"io"
	"time"

	"go.uber.org/zap"
)

const (
	hidVendorID  = 0x1209
	hidProductID = 0x0001

	hidReconnectDelay = 2 * time.Second
)

// MicMuter toggles the system microphone mute state.
type MicMuter interface {
	ToggleMute() error
}

// HIDManager reads reports from the SERENITY HID interface and dispatches actions.
type HIDManager struct {
	deej   *Deej
	logger *zap.SugaredLogger
	muter  MicMuter
	stopCh chan struct{}
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

func (h *HIDManager) handleReport(report []byte) {
	// TODO: validate report bytes against the firmware HID descriptor once finalized.
	// For now any report on this VID/PID triggers mic mute toggle.
	h.logger.Debug("Received HID report, toggling mic mute")

	if err := h.muter.ToggleMute(); err != nil {
		h.logger.Warnw("Failed to toggle mic mute", "error", err)
	}
}
