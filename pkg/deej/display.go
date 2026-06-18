package deej

import (
	"bytes"

	"go.uber.org/zap"

	"github.com/sclead03/deej-x/pkg/deej/icon"
)

// numChannels is the number of channel OLEDs on SERENITY (indices 0–4, mapped to faders 1–5).
// The master OLED is firmware-controlled and receives no host commands.
const numChannels = 5

// DisplayManager handles the SERENITY connection handshake and channel display push sequencing.
type DisplayManager struct {
	deej   *Deej
	logger *zap.SugaredLogger

	// last successfully sent state per channel; used to skip unchanged channels on manual push
	lastSentNames [numChannels]string
	lastSentIcons [numChannels][]byte
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
				dm.pushAll(writer, true)
			}
		}
	}()
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
