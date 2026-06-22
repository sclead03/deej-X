package deej

import (
	"encoding/binary"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
)

// Command IDs for the host → firmware binary protocol.
// Frame format: [0x00][cmdID][lenLo][lenHi][...payload...]
const (
	cmdPrefix          = byte(0x00)
	cmdQuery           = byte(0x01)
	cmdSetChName       = byte(0x02)
	cmdSetChIcon       = byte(0x03)
	cmdSetMasterVol    = byte(0x04)
	cmdSetMicMuteState = byte(0x05)
	cmdSetMasterMute   = byte(0x07) // 0x06 and 0x08 are reserved device->host (CMD_REQUEST_ICON_REDRAW, CMD_REQUEST_MASTER_MUTE_TOGGLE - see display.go)

	// MaxChannelNameLength is the maximum number of characters in a channel display
	// name (excluding the null terminator). Revisit when firmware font size is finalized.
	MaxChannelNameLength = 15
)

// SerialWriter frames and sends host→firmware commands over the open serial connection.
// All public methods are safe for concurrent use.
type SerialWriter struct {
	w      io.Writer
	mu     sync.Mutex
	logger *zap.SugaredLogger

	// lastSentMasterVolumeRaw/haveSentMasterVolume/lastSentMasterVolumeAtNano
	// record the most recent push via SendMasterVolume, so SerialIO.handleLine
	// can recognize when an incoming "masterVol" reading is just SERENITY
	// echoing back a value we pushed (not a real encoder move) and avoid
	// re-deriving a SliderMoveEvent from our own echo - see handleLine in
	// serial.go. Exact-value match alone isn't enough: a fast scroll can have
	// multiple pushes in flight before their echoes return, so an echo of an
	// older, already-superseded push won't match the latest value anymore -
	// the time-window fallback (TimeSinceLastSentMasterVolume) catches that.
	lastSentMasterVolumeRaw    atomic.Uint32
	haveSentMasterVolume       atomic.Bool
	lastSentMasterVolumeAtNano atomic.Int64
}

// NewSerialWriter creates a SerialWriter that writes framed commands to w.
func NewSerialWriter(w io.Writer, logger *zap.SugaredLogger) *SerialWriter {
	return &SerialWriter{
		w:      w,
		logger: logger.Named("serial_writer"),
	}
}

// SendQuery sends CMD_QUERY, asking SERENITY to emit its ready beacon.
func (sw *SerialWriter) SendQuery() error {
	return sw.send(cmdQuery, nil)
}

// SendChannelName pushes a display name for channel idx (0–4).
// Names longer than MaxChannelNameLength are silently truncated.
func (sw *SerialWriter) SendChannelName(idx byte, name string) error {
	if len(name) > MaxChannelNameLength {
		name = name[:MaxChannelNameLength]
	}
	payload := make([]byte, 0, 1+len(name)+1)
	payload = append(payload, idx)
	payload = append(payload, name...)
	payload = append(payload, 0x00)
	return sw.send(cmdSetChName, payload)
}

// SendChannelIcon pushes a raw 1-bit bitmap for channel idx (0–4).
func (sw *SerialWriter) SendChannelIcon(idx byte, bitmap []byte) error {
	payload := make([]byte, 0, 1+len(bitmap))
	payload = append(payload, idx)
	payload = append(payload, bitmap...)
	return sw.send(cmdSetChIcon, payload)
}

// SendMasterVolume pushes the current master volume, raw 0–1023 (same domain as
// the firmware's own masterVol), so SERENITY can sync its encoder/display state
// instead of booting hard-coded.
func (sw *SerialWriter) SendMasterVolume(raw uint16) error {
	payload := make([]byte, 2)
	binary.LittleEndian.PutUint16(payload, raw)
	if err := sw.send(cmdSetMasterVol, payload); err != nil {
		return err
	}

	sw.lastSentMasterVolumeRaw.Store(uint32(raw))
	sw.haveSentMasterVolume.Store(true)
	sw.lastSentMasterVolumeAtNano.Store(time.Now().UnixNano())
	return nil
}

// TimeSinceLastSentMasterVolume returns how long ago SendMasterVolume last
// succeeded, and whether it's ever been called at all.
func (sw *SerialWriter) TimeSinceLastSentMasterVolume() (time.Duration, bool) {
	if !sw.haveSentMasterVolume.Load() {
		return 0, false
	}
	return time.Since(time.Unix(0, sw.lastSentMasterVolumeAtNano.Load())), true
}

// LastSentMasterVolumeRaw returns the most recent raw value successfully sent
// via SendMasterVolume, and whether one has been sent yet at all.
func (sw *SerialWriter) LastSentMasterVolumeRaw() (uint16, bool) {
	if !sw.haveSentMasterVolume.Load() {
		return 0, false
	}
	return uint16(sw.lastSentMasterVolumeRaw.Load()), true
}

// SendMicMuteState pushes the current system microphone mute state so SERENITY's
// RGB button LED can sync to it instead of booting unmuted.
func (sw *SerialWriter) SendMicMuteState(muted bool) error {
	payload := []byte{0x00}
	if muted {
		payload[0] = 0x01
	}
	return sw.send(cmdSetMicMuteState, payload)
}

// SendMasterMuteState pushes the master output's real WASAPI mute state
// (Windows volume mixer mute button, media key mute, etc.) - distinct from the
// SERENITY encoder's own local single-click mute, which never needs this command
// since it already round-trips correctly via the existing fader/ASCII channel.
func (sw *SerialWriter) SendMasterMuteState(muted bool) error {
	payload := []byte{0x00}
	if muted {
		payload[0] = 0x01
	}
	return sw.send(cmdSetMasterMute, payload)
}

func (sw *SerialWriter) send(cmdID byte, payload []byte) error {
	frame := make([]byte, 4+len(payload))
	frame[0] = cmdPrefix
	frame[1] = cmdID
	binary.LittleEndian.PutUint16(frame[2:4], uint16(len(payload)))
	copy(frame[4:], payload)

	sw.mu.Lock()
	defer sw.mu.Unlock()

	if _, err := sw.w.Write(frame); err != nil {
		sw.logger.Warnw("Failed to send command", "cmdID", fmt.Sprintf("0x%02x", cmdID), "error", err)
		return fmt.Errorf("send command 0x%02x: %w", cmdID, err)
	}

	sw.logger.Debugw("Sent command", "cmdID", fmt.Sprintf("0x%02x", cmdID), "payloadLen", len(payload))
	return nil
}
