package deej

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jacobsa/go-serial/serial"
	"go.uber.org/zap"

	"github.com/sclead03/deej-x/pkg/deej/util"
)

// SerialIO provides a deej-aware abstraction layer to managing serial I/O
type SerialIO struct {
	comPort  string
	baudRate uint

	deej   *Deej
	logger *zap.SugaredLogger

	stopChannel chan bool
	connected   bool
	connOptions serial.OpenOptions
	conn        io.ReadWriteCloser

	lastKnownNumSliders        int
	currentSliderPercentValues []float32

	sliderMoveConsumers []chan SliderMoveEvent
	connectedConsumers  []chan struct{}
	beaconConsumers     []chan struct{}
	deviceCmdConsumers  []chan DeviceCommand

	writer *SerialWriter
}

// SliderMoveEvent represents a single slider move captured by deej
type SliderMoveEvent struct {
	SliderID     int
	PercentValue float32
}

// DeviceCommand represents a single binary device->host command frame received
// from SERENITY (escape-prefixed, mirroring the host->firmware protocol in serial_writer.go).
type DeviceCommand struct {
	CmdID   byte
	Payload []byte
}

// inboundMessage is either a complete ASCII line (fader data / beacon) or a
// parsed binary device->host command frame, produced by readFrames.
type inboundMessage struct {
	line    string
	isCmd   bool
	cmdID   byte
	payload []byte
}

var expectedLinePattern = regexp.MustCompile(`^\d{1,4}(\|\d{1,4})*\r\n$`)

// NewSerialIO creates a SerialIO instance that uses the provided deej
// instance's connection info to establish communications with SERENITY
func NewSerialIO(deej *Deej, logger *zap.SugaredLogger) (*SerialIO, error) {
	logger = logger.Named("serial")

	sio := &SerialIO{
		deej:                deej,
		logger:              logger,
		stopChannel:         make(chan bool),
		connected:           false,
		conn:                nil,
		sliderMoveConsumers: []chan SliderMoveEvent{},
		connectedConsumers:  []chan struct{}{},
		beaconConsumers:     []chan struct{}{},
	}

	logger.Debug("Created serial i/o instance")

	// respond to config changes
	sio.setupOnConfigReload()

	return sio, nil
}

// Start attempts to connect to SERENITY
func (sio *SerialIO) Start() error {

	// don't allow multiple concurrent connections
	if sio.connected {
		sio.logger.Warn("Already connected, can't start another without closing first")
		return errors.New("serial: connection already active")
	}

	// set minimum read size according to platform (0 for windows, 1 for linux)
	// this prevents a rare bug on windows where serial reads get congested,
	// resulting in significant lag
	minimumReadSize := 0
	if util.Linux() {
		minimumReadSize = 1
	}

	sio.connOptions = serial.OpenOptions{
		PortName:        sio.deej.config.ConnectionInfo.COMPort,
		BaudRate:        uint(sio.deej.config.ConnectionInfo.BaudRate),
		DataBits:        8,
		StopBits:        1,
		MinimumReadSize: uint(minimumReadSize),
	}

	sio.logger.Debugw("Attempting serial connection",
		"comPort", sio.connOptions.PortName,
		"baudRate", sio.connOptions.BaudRate,
		"minReadSize", minimumReadSize)

	var err error
	sio.conn, err = serial.Open(sio.connOptions)
	if err != nil {

		sio.logger.Warnw("Failed to open serial connection", "error", err)
		return fmt.Errorf("open serial connection: %w", err)
	}

	namedLogger := sio.logger.Named(strings.ToLower(sio.connOptions.PortName))

	namedLogger.Infow("Connected", "conn", sio.conn)
	sio.connected = true
	sio.writer = NewSerialWriter(sio.conn, sio.logger)

	for _, consumer := range sio.connectedConsumers {
		go func(ch chan struct{}) { ch <- struct{}{} }(consumer)
	}

	// read frames (lines or binary commands) or await a stop
	go func() {
		connReader := bufio.NewReader(sio.conn)
		msgChannel := sio.readFrames(namedLogger, connReader)

		for {
			select {
			case <-sio.stopChannel:
				sio.close(namedLogger)
				return
			case msg, ok := <-msgChannel:
				if !ok {
					// Read goroutine exited — device was unplugged.
					namedLogger.Info("Serial device disconnected")
					sio.close(namedLogger)
					go sio.reconnect()
					return
				}
				if msg.isCmd {
					sio.handleDeviceCommand(namedLogger, msg.cmdID, msg.payload)
				} else {
					sio.handleLine(namedLogger, msg.line)
				}
			}
		}
	}()

	return nil
}

// Stop signals us to shut down our serial connection, if one is active
func (sio *SerialIO) Stop() {
	if sio.connected {
		sio.logger.Debug("Shutting down serial connection")
		sio.stopChannel <- true
	} else {
		sio.logger.Debug("Not currently connected, nothing to stop")
	}
}

// SubscribeToSliderMoveEvents returns an unbuffered channel that receives
// a sliderMoveEvent struct every time a slider moves
func (sio *SerialIO) SubscribeToSliderMoveEvents() chan SliderMoveEvent {
	ch := make(chan SliderMoveEvent)
	sio.sliderMoveConsumers = append(sio.sliderMoveConsumers, ch)

	return ch
}

// Writer returns the active SerialWriter, or nil if not connected.
func (sio *SerialIO) Writer() *SerialWriter {
	return sio.writer
}

// SubscribeToConnectEvents returns a channel that receives a signal each time
// a serial connection is successfully opened.
func (sio *SerialIO) SubscribeToConnectEvents() chan struct{} {
	ch := make(chan struct{})
	sio.connectedConsumers = append(sio.connectedConsumers, ch)
	return ch
}

// SubscribeToBeaconEvents returns a channel that receives a signal each time
// the SERENITY ready beacon is received.
func (sio *SerialIO) SubscribeToBeaconEvents() chan struct{} {
	ch := make(chan struct{})
	sio.beaconConsumers = append(sio.beaconConsumers, ch)
	return ch
}

// SubscribeToDeviceCommands returns a channel that receives every binary
// device->host command frame SERENITY sends (e.g. CMD_REQUEST_ICON_REDRAW).
func (sio *SerialIO) SubscribeToDeviceCommands() chan DeviceCommand {
	ch := make(chan DeviceCommand)
	sio.deviceCmdConsumers = append(sio.deviceCmdConsumers, ch)
	return ch
}

func (sio *SerialIO) setupOnConfigReload() {
	configReloadedChannel := sio.deej.config.SubscribeToChanges()

	const stopDelay = 50 * time.Millisecond

	go func() {
		for {
			select {
			case <-configReloadedChannel:

				// make any config reload unset our slider number to ensure process volumes are being re-set
				// (the next read line will emit SliderMoveEvent instances for all sliders)\
				// this needs to happen after a small delay, because the session map will also re-acquire sessions
				// whenever the config file is reloaded, and we don't want it to receive these move events while the map
				// is still cleared. this is kind of ugly, but shouldn't cause any issues
				go func() {
					<-time.After(stopDelay)
					sio.lastKnownNumSliders = 0
				}()

				// if connection params have changed, attempt to stop and start the connection
				if sio.deej.config.ConnectionInfo.COMPort != sio.connOptions.PortName ||
					uint(sio.deej.config.ConnectionInfo.BaudRate) != sio.connOptions.BaudRate {

					sio.logger.Info("Detected change in connection parameters, attempting to renew connection")
					sio.Stop()

					// let the connection close
					<-time.After(stopDelay)

					if err := sio.Start(); err != nil {
						sio.logger.Warnw("Failed to renew connection after parameter change", "error", err)
					} else {
						sio.logger.Debug("Renewed connection successfully")
					}
				}
			}
		}
	}()
}

func (sio *SerialIO) close(logger *zap.SugaredLogger) {
	if err := sio.conn.Close(); err != nil {
		logger.Warnw("Failed to close serial connection", "error", err)
	} else {
		logger.Debug("Serial connection closed")
	}

	sio.conn = nil
	sio.writer = nil
	sio.connected = false
}

func (sio *SerialIO) reconnect() {
	sio.logger.Info("Waiting for serial device to reappear")
	for {
		waitForSerialDevice(sio.logger)
		if err := sio.Start(); err == nil {
			return
		}
		sio.logger.Debug("Reconnect attempt failed, waiting for next device arrival")
	}
}

// readFrames reads the serial stream byte by byte, distinguishing ASCII lines
// (fader data, the SERENITY beacon) from binary device->host command frames.
// 0x00 is an unambiguous escape byte — it never appears in ASCII fader data —
// mirroring the escape-prefixed framing firmware already uses for host->firmware
// commands (see serial_writer.go / processIncomingSerial in main.cpp).
//
// Frame format: [0x00][cmdID][lenLo][lenHi][...payload bytes...]. The payload
// is read by exact length via io.ReadFull, so embedded \n/\r bytes can't
// truncate it the way they would a naive line-based reader.
func (sio *SerialIO) readFrames(logger *zap.SugaredLogger, reader *bufio.Reader) chan inboundMessage {
	ch := make(chan inboundMessage)

	go func() {
		defer close(ch)
		var lineBuf []byte

		for {
			b, err := reader.ReadByte()
			if err != nil {
				if sio.deej.Verbose() {
					logger.Warnw("Failed to read from serial", "error", err)
				}
				return
			}

			if b == 0x00 {
				cmdID, err := reader.ReadByte()
				if err != nil {
					return
				}
				lenLo, err := reader.ReadByte()
				if err != nil {
					return
				}
				lenHi, err := reader.ReadByte()
				if err != nil {
					return
				}
				length := int(lenLo) | int(lenHi)<<8

				payload := make([]byte, length)
				if length > 0 {
					if _, err := io.ReadFull(reader, payload); err != nil {
						return
					}
				}

				if sio.deej.Verbose() {
					logger.Debugw("Read device command", "cmdID", cmdID, "payloadLen", length)
				}

				ch <- inboundMessage{isCmd: true, cmdID: cmdID, payload: payload}
				continue
			}

			lineBuf = append(lineBuf, b)
			if b == '\n' {
				line := string(lineBuf)
				lineBuf = lineBuf[:0]

				if sio.deej.Verbose() {
					logger.Debugw("Read new line", "line", line)
				}

				ch <- inboundMessage{line: line}
			}
		}
	}()

	return ch
}

// handleDeviceCommand dispatches a parsed device->host command frame to all subscribers.
func (sio *SerialIO) handleDeviceCommand(logger *zap.SugaredLogger, cmdID byte, payload []byte) {
	for _, consumer := range sio.deviceCmdConsumers {
		go func(ch chan DeviceCommand) { ch <- DeviceCommand{CmdID: cmdID, Payload: payload} }(consumer)
	}
}

func (sio *SerialIO) handleLine(logger *zap.SugaredLogger, line string) {

	if line == "SERENITY\r\n" {
		logger.Info("Received SERENITY beacon")
		for _, consumer := range sio.beaconConsumers {
			go func(ch chan struct{}) { ch <- struct{}{} }(consumer)
		}
		return
	}

	// this function receives an unsanitized line which is guaranteed to end with LF,
	// but most lines will end with CRLF. it may also have garbage instead of
	// deej-formatted values, so we must check for that! just ignore bad ones
	if !expectedLinePattern.MatchString(line) {
		return
	}

	// trim the suffix
	line = strings.TrimSuffix(line, "\r\n")

	// split on pipe (|), this gives a slice of numerical strings between "0" and "1023"
	splitLine := strings.Split(line, "|")
	numSliders := len(splitLine)

	// update our slider count, if needed - this will send slider move events for all
	if numSliders != sio.lastKnownNumSliders {
		logger.Infow("Detected sliders", "amount", numSliders)
		sio.lastKnownNumSliders = numSliders
		sio.currentSliderPercentValues = make([]float32, numSliders)

		// reset everything to be an impossible value to force the slider move event later
		for idx := range sio.currentSliderPercentValues {
			sio.currentSliderPercentValues[idx] = -1.0
		}
	}

	// for each slider:
	moveEvents := []SliderMoveEvent{}
	for sliderIdx, stringValue := range splitLine {

		// convert string values to integers ("1023" -> 1023)
		number, _ := strconv.Atoi(stringValue)

		// turns out the first line could come out dirty sometimes (i.e. "4558|925|41|643|220")
		// so let's check the first number for correctness just in case
		if sliderIdx == 0 && number > 1023 {
			sio.logger.Debugw("Got malformed line from serial, ignoring", "line", line)
			return
		}

		// map the value from raw to a "dirty" float between 0 and 1 (e.g. 0.15451...)
		dirtyFloat := float32(number) / 1023.0

		// normalize it to an actual volume scalar between 0.0 and 1.0 with 2 points of precision
		normalizedScalar := util.NormalizeScalar(dirtyFloat)

		// if sliders are inverted, take the complement of 1.0
		if sio.deej.config.InvertSliders {
			normalizedScalar = 1 - normalizedScalar
		}

		// check if it changes the desired state (could just be a jumpy raw slider value)
		if util.SignificantlyDifferent(sio.currentSliderPercentValues[sliderIdx], normalizedScalar, sio.deej.config.NoiseReductionLevel) {

			// slider 0 is SERENITY's master volume encoder, not a physical-position slider -
			// its value on the very first read is just whatever masterVol the firmware booted
			// with, not a real user-set position. Applying it would stomp the actual Windows
			// volume right as pushMasterState is trying to sync the real value down. Prime the
			// baseline silently here and let the encoder/pushMasterState drive it from now on.
			primingMasterSlider := sliderIdx == 0 && sio.currentSliderPercentValues[sliderIdx] < 0

			// if it does, update the saved value and create a move event
			sio.currentSliderPercentValues[sliderIdx] = normalizedScalar

			if !primingMasterSlider {
				moveEvents = append(moveEvents, SliderMoveEvent{
					SliderID:     sliderIdx,
					PercentValue: normalizedScalar,
				})

				if sio.deej.Verbose() {
					logger.Debugw("Slider moved", "event", moveEvents[len(moveEvents)-1])
				}
			}
		}
	}

	// deliver move events if there are any, towards all potential consumers
	if len(moveEvents) > 0 {
		for _, consumer := range sio.sliderMoveConsumers {
			for _, moveEvent := range moveEvents {
				consumer <- moveEvent
			}
		}
	}
}
