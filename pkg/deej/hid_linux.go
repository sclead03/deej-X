package deej

import (
	"errors"
	"fmt"
	"io"
	"os/exec"

	"go.uber.org/zap"
)

// openSERENITY on Linux is not yet implemented.
// A future implementation would enumerate /dev/hidraw* and match by VID/PID
// via /sys/class/hidraw/<dev>/device/uevent.
func openSERENITY() (io.ReadCloser, error) {
	return nil, errors.New("HID enumeration not yet implemented on Linux")
}

type linuxMicMuter struct {
	logger *zap.SugaredLogger
}

func newMicMuter(logger *zap.SugaredLogger) (MicMuter, error) {
	return &linuxMicMuter{logger: logger.Named("mic_muter")}, nil
}

func (m *linuxMicMuter) ToggleMute() error {
	cmd := exec.Command("pactl", "set-source-mute", "@DEFAULT_SOURCE@", "toggle")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("pactl toggle mic mute: %w", err)
	}

	m.logger.Debug("Toggled mic mute via pactl")
	return nil
}
