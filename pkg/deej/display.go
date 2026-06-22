package deej

import (
	"bytes"

	"go.uber.org/zap"

	"github.com/sclead03/deej-x/pkg/deej/icon"
	"github.com/sclead03/deej-x/pkg/deej/util"
)

// numChannels is the number of channel OLEDs on SERENITY (indices 0–4, mapped to faders 1–5).
// The master OLED is firmware-controlled and receives no host commands.
const numChannels = 5

// cmdRequestIconRedraw is the device->host command SERENITY sends during its
// channel screensaver tick, asking for the channel's icon to be re-rendered at
// a new bounce offset. Payload: [channel_idx][x_offset][y_offset].
const cmdRequestIconRedraw = byte(0x06)

// cmdRequestMasterMuteToggle is the device->host command SERENITY's encoder
// sends on a confirmed single-click, asking the host to perform a real
// OS-level master mute toggle. No payload - see handleMasterMuteToggleRequest.
const cmdRequestMasterMuteToggle = byte(0x08)

// DisplayManager handles the SERENITY connection handshake and channel display push sequencing.
type DisplayManager struct {
	deej   *Deej
	logger *zap.SugaredLogger

	// last successfully sent state per channel; used to skip unchanged channels on manual push
	lastSentNames [numChannels]string
	lastSentIcons [numChannels][]byte

	// last master volume scalar successfully pushed to SERENITY; used to dedupe
	// redundant pushes triggered by the live master volume watcher.
	lastPushedMasterVolume float32
	havePushedMasterVolume bool

	// last master mute (volMuted) state successfully pushed to SERENITY; used to
	// dedupe redundant pushes triggered by the live master volume watcher.
	lastPushedVolMuted bool
	havePushedVolMuted bool
}

// NewDisplayManager creates a DisplayManager and wires it to SerialIO connection and beacon events.
func NewDisplayManager(deej *Deej, logger *zap.SugaredLogger) (*DisplayManager, error) {
	logger = logger.Named("display")

	dm := &DisplayManager{
		deej:   deej,
		logger: logger,
	}

	dm.subscribeToSerialEvents()

	logger.Debug("Created display manager")
	return dm, nil
}

// TriggerPush sends all channel names and icons to SERENITY, skipping channels
// whose state hasn't changed since the last successful push.
func (dm *DisplayManager) TriggerPush() {
	writer := dm.deej.serial.Writer()
	if writer == nil {
		dm.logger.Warn("Push requested but serial is not connected")
		return
	}
	dm.pushAll(writer, false)
}

func (dm *DisplayManager) subscribeToSerialEvents() {
	connectCh := dm.deej.serial.SubscribeToConnectEvents()
	beaconCh := dm.deej.serial.SubscribeToBeaconEvents()
	deviceCmdCh := dm.deej.serial.SubscribeToDeviceCommands()
	masterVolCh := dm.deej.sessions.SubscribeToMasterVolumeChanges()
	micMuteCh := dm.deej.sessions.SubscribeToMicMuteChanges()

	go func() {
		for {
			select {
			case <-connectCh:
				dm.logger.Debug("Serial connected, sending CMD_QUERY")
				writer := dm.deej.serial.Writer()
				if writer == nil {
					dm.logger.Warn("Connect event received but writer is nil")
					continue
				}
				if err := writer.SendQuery(); err != nil {
					dm.logger.Warnw("Failed to send CMD_QUERY", "error", err)
				}

			case <-beaconCh:
				dm.logger.Info("Beacon received, pushing display data")
				writer := dm.deej.serial.Writer()
				if writer == nil {
					dm.logger.Warn("Beacon received but writer is nil")
					continue
				}
				dm.pushMasterState(writer)
				dm.pushAll(writer, true)

			case cmd := <-deviceCmdCh:
				dm.handleDeviceCommand(cmd)

			case update := <-masterVolCh:
				dm.handleExternalMasterVolumeChange(update)

			case muted := <-micMuteCh:
				dm.handleExternalMicMuteChange(muted)
			}
		}
	}()
}

// handleExternalMasterVolumeChange pushes a master volume and/or mute change
// down to SERENITY in response to a live, externally-sourced change (Windows
// volume mixer, media keys, another app) reported by the platform's
// MasterVolumeWatcher. Changes the encoder itself caused are already filtered
// out before reaching this point - see sessionMap.setupMasterVolumeWatcher.
// Volume and mute are independent pushes (separate commands, separate dedupe
// state) even though they arrive together in one update, since either can
// change without the other (e.g. muting via the mixer's mute button alone
// leaves the volume level untouched).
func (dm *DisplayManager) handleExternalMasterVolumeChange(update MasterVolumeUpdate) {
	writer := dm.deej.serial.Writer()
	if writer == nil {
		return
	}

	// ForceSync (the periodic settle correction) always sends, bypassing the
	// noise-threshold dedup below - its entire purpose is correcting a final
	// value the live path may have gotten slightly wrong, so a small/no-op-
	// looking difference from the last pushed value is exactly the case it
	// needs to still go through.
	if update.ForceSync || !dm.havePushedMasterVolume ||
		util.SignificantlyDifferent(dm.lastPushedMasterVolume, update.Volume, dm.deej.config.NoiseReductionLevel) {

		raw := uint16(update.Volume*1023 + 0.5)
		if err := writer.SendMasterVolume(raw); err != nil {
			dm.logger.Warnw("Failed to send live master volume update", "error", err)
		} else {
			dm.lastPushedMasterVolume = update.Volume
			dm.havePushedMasterVolume = true
			dm.logger.Debugw("Pushed live master volume update", "raw", raw, "forceSync", update.ForceSync)
		}
	}

	if update.ForceSync || !dm.havePushedVolMuted || update.Muted != dm.lastPushedVolMuted {
		if err := writer.SendMasterMuteState(update.Muted); err != nil {
			dm.logger.Warnw("Failed to send live master mute update", "error", err)
		} else {
			dm.lastPushedVolMuted = update.Muted
			dm.havePushedVolMuted = true
			dm.logger.Debugw("Pushed live master mute update", "muted", update.Muted, "forceSync", update.ForceSync)
		}
	}
}

// handleExternalMicMuteChange pushes a mic mute change down to SERENITY in
// response to a live, externally-sourced change (Windows mic settings/taskbar,
// another app) reported by the platform's MicMuteWatcher. Changes from
// SERENITY's own RGB button are already filtered out before reaching this
// point and pushed directly by HIDManager.handleReport instead - see
// wcaSessionFinder.micMuteNotifyCallback.
func (dm *DisplayManager) handleExternalMicMuteChange(muted bool) {
	writer := dm.deej.serial.Writer()
	if writer == nil {
		return
	}

	if err := writer.SendMicMuteState(muted); err != nil {
		dm.logger.Warnw("Failed to send live mic mute update", "error", err)
		return
	}

	dm.logger.Debugw("Pushed live mic mute update", "muted", muted)
}

// handleDeviceCommand dispatches a binary command received from SERENITY.
func (dm *DisplayManager) handleDeviceCommand(cmd DeviceCommand) {
	switch cmd.CmdID {
	case cmdRequestIconRedraw:
		dm.handleIconRedrawRequest(cmd.Payload)
	case cmdRequestMasterMuteToggle:
		dm.handleMasterMuteToggleRequest()
	default:
		dm.logger.Debugw("Received unhandled device command", "cmdID", cmd.CmdID)
	}
}

// handleMasterMuteToggleRequest performs the real OS-level master mute toggle
// requested by SERENITY's encoder click and pushes the result back down
// directly - the same "act now, push the result myself" pattern HIDManager
// uses for the RGB button's mic mute, rather than waiting on the generic
// external-change watcher. That watcher will also observe this same change,
// but drops it as deej's own write (see sessionMap.toggleMasterMuted), so it
// won't push a redundant/racing second copy of the same state.
func (dm *DisplayManager) handleMasterMuteToggleRequest() {
	writer := dm.deej.serial.Writer()
	if writer == nil {
		dm.logger.Warn("Master mute toggle requested but writer is nil")
		return
	}

	nowMuted, err := dm.deej.sessions.toggleMasterMuted()
	if err != nil {
		dm.logger.Warnw("Failed to toggle master mute", "error", err)
		return
	}

	if err := writer.SendMasterMuteState(nowMuted); err != nil {
		dm.logger.Warnw("Failed to push master mute toggle result", "error", err)
		return
	}

	dm.lastPushedVolMuted = nowMuted
	dm.havePushedVolMuted = true

	dm.logger.Debugw("Toggled master mute via SERENITY encoder", "muted", nowMuted)
}

// handleIconRedrawRequest re-renders a channel's icon at a new bounce offset and
// pushes it, in response to SERENITY's channel screensaver tick.
func (dm *DisplayManager) handleIconRedrawRequest(payload []byte) {
	if len(payload) < 3 {
		dm.logger.Warnw("Malformed icon redraw request", "payloadLen", len(payload))
		return
	}
	channel, xOffset, yOffset := int(payload[0]), int(payload[1]), int(payload[2])
	if channel >= numChannels {
		dm.logger.Warnw("Icon redraw request for out-of-range channel", "channel", channel)
		return
	}

	writer := dm.deej.serial.Writer()
	if writer == nil {
		dm.logger.Warn("Icon redraw requested but writer is nil")
		return
	}

	targets, ok := dm.deej.config.SliderMapping.get(channel + 1)
	if !ok || len(targets) == 0 || targets[0] == "master" {
		return
	}
	processName := targets[0]
	iconConversion := dm.deej.config.IconConversion[channel]

	bitmap, err := icon.LoadAt(processName, dm.deej.config.IconDir, iconConversion, xOffset, yOffset)
	if err != nil {
		dm.logger.Debugw("No icon for channel during screensaver redraw", "channel", channel, "process", processName, "error", err)
		return
	}

	if err := writer.SendChannelIcon(byte(channel), bitmap); err != nil {
		dm.logger.Warnw("Failed to send screensaver icon redraw", "channel", channel, "error", err)
		return
	}

	// Off-center bytes won't match a future centered Load() result, so the next
	// manual/connect push correctly notices the mismatch and re-centers.
	dm.lastSentIcons[channel] = bitmap
}

// pushMasterState sends the current master volume and mic mute state, so SERENITY
// can sync its encoder/display/RGB state instead of booting to hard-coded defaults.
func (dm *DisplayManager) pushMasterState(writer *SerialWriter) {
	if vol, ok := dm.deej.sessions.getMasterVolume(); ok {
		raw := uint16(vol*1023 + 0.5)
		if err := writer.SendMasterVolume(raw); err != nil {
			dm.logger.Warnw("Failed to send master volume", "error", err)
		} else {
			dm.logger.Debugw("Sent master volume", "raw", raw)
			dm.lastPushedMasterVolume = vol
			dm.havePushedMasterVolume = true
		}
	} else {
		dm.logger.Debug("Master session not available, skipping master volume push")
	}

	if muted, err := dm.deej.hid.IsMicMuted(); err != nil {
		dm.logger.Debugw("Failed to get mic mute state, skipping push", "error", err)
	} else if err := writer.SendMicMuteState(muted); err != nil {
		dm.logger.Warnw("Failed to send mic mute state", "error", err)
	} else {
		dm.logger.Debugw("Sent mic mute state", "muted", muted)
	}

	if muted, ok := dm.deej.sessions.getMasterMuted(); ok {
		if err := writer.SendMasterMuteState(muted); err != nil {
			dm.logger.Warnw("Failed to send master mute state", "error", err)
		} else {
			dm.logger.Debugw("Sent master mute state", "muted", muted)
			dm.lastPushedVolMuted = muted
			dm.havePushedVolMuted = true
		}
	} else {
		dm.logger.Debug("Master mute state not available, skipping push")
	}
}

// pushAll sends names and icons for all channels. force=true (connection event) sends
// everything regardless of change tracking; force=false (manual push) skips unchanged channels.
func (dm *DisplayManager) pushAll(writer *SerialWriter, force bool) {
	names := dm.deej.config.ChannelNames
	iconDir := dm.deej.config.IconDir

	for i := 0; i < numChannels; i++ {
		iconConversion := dm.deej.config.IconConversion[i]

		// name
		name := names[i]
		if force || name != dm.lastSentNames[i] {
			if err := writer.SendChannelName(byte(i), name); err != nil {
				dm.logger.Warnw("Failed to send channel name", "channel", i, "error", err)
			} else {
				dm.lastSentNames[i] = name
				dm.logger.Debugw("Sent channel name", "channel", i, "name", name)
			}
		}

		// icon: channel i (0-based) maps to slider index i+1; master is at 0 and skipped
		targets, ok := dm.deej.config.SliderMapping.get(i + 1)
		if !ok || len(targets) == 0 {
			continue
		}
		processName := targets[0]
		if processName == "master" {
			continue
		}

		bitmap, err := icon.Load(processName, iconDir, iconConversion)
		if err != nil {
			dm.logger.Debugw("No icon for channel", "channel", i, "process", processName, "error", err)
			continue
		}

		if !force && bytes.Equal(bitmap, dm.lastSentIcons[i]) {
			continue
		}

		if err := writer.SendChannelIcon(byte(i), bitmap); err != nil {
			dm.logger.Warnw("Failed to send channel icon", "channel", i, "error", err)
			continue
		}

		dm.lastSentIcons[i] = bitmap
		dm.logger.Debugw("Sent channel icon", "channel", i, "process", processName)
	}
}
