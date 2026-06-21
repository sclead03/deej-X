package deej

import (
	"fmt"
	"net"

	"github.com/jfreymuth/pulse/proto"
	"go.uber.org/zap"
)

type paSessionFinder struct {
	logger        *zap.SugaredLogger
	sessionLogger *zap.SugaredLogger

	client *proto.Client
	conn   net.Conn

	// pushes live master sink volume changes (see MasterVolumeWatcher)
	masterVolumeChanges chan float32
}

// PulseAudio native protocol subscription mask/event bits (pulse/def.h). The
// proto wrapper library doesn't define these as constants, so they're spelled
// out here.
const (
	paSubscriptionMaskSink = 0x0001

	paSubscriptionEventFacilityMask = 0x000f
	paSubscriptionEventSink         = 0x0000

	paSubscriptionEventTypeMask = 0x0030
	paSubscriptionEventChange   = 0x0010
)

func newSessionFinder(logger *zap.SugaredLogger) (SessionFinder, error) {
	client, conn, err := proto.Connect("")
	if err != nil {
		logger.Warnw("Failed to establish PulseAudio connection", "error", err)
		return nil, fmt.Errorf("establish PulseAudio connection: %w", err)
	}

	request := proto.SetClientName{
		Props: proto.PropList{
			"application.name": proto.PropListString("deej"),
		},
	}
	reply := proto.SetClientNameReply{}

	if err := client.Request(&request, &reply); err != nil {
		return nil, err
	}

	sf := &paSessionFinder{
		logger:              logger.Named("session_finder"),
		sessionLogger:       logger.Named("sessions"),
		client:              client,
		conn:                conn,
		masterVolumeChanges: make(chan float32, 8),
	}

	// live-track external changes to the master sink volume (another app,
	// pavucontrol, etc.) via PulseAudio's native event subscription - best
	// effort: a failure here just means the OLED won't live-sync.
	client.Callback = sf.handlePulseEvent
	if err := client.Request(&proto.Subscribe{Mask: paSubscriptionMaskSink}, nil); err != nil {
		sf.logger.Warnw("Failed to subscribe to PulseAudio sink events", "error", err)
	}

	sf.logger.Debug("Created PA session finder instance")

	return sf, nil
}

// SubscribeToMasterVolumeChanges implements MasterVolumeWatcher.
func (sf *paSessionFinder) SubscribeToMasterVolumeChanges() <-chan float32 {
	return sf.masterVolumeChanges
}

// handlePulseEvent is proto.Client's Callback, invoked on the client's readLoop
// goroutine for every unsolicited server message, including our sink-change
// subscription. It must never issue a synchronous client.Request itself (that
// would deadlock readLoop waiting on its own reply), so the actual volume
// re-read happens on a spawned goroutine.
func (sf *paSessionFinder) handlePulseEvent(msg interface{}) {
	event, ok := msg.(*proto.SubscribeEvent)
	if !ok {
		return
	}

	if event.Event&paSubscriptionEventFacilityMask != paSubscriptionEventSink {
		return
	}

	if event.Event&paSubscriptionEventTypeMask != paSubscriptionEventChange {
		return
	}

	go sf.forwardMasterSinkVolume()
}

// forwardMasterSinkVolume re-reads the current default sink's volume and pushes
// it to subscribers. Triggered only by a genuine PulseAudio sink-change event -
// never polled.
func (sf *paSessionFinder) forwardMasterSinkVolume() {
	request := proto.GetSinkInfo{SinkIndex: proto.Undefined}
	reply := proto.GetSinkInfoReply{}

	if err := sf.client.Request(&request, &reply); err != nil {
		sf.logger.Debugw("Failed to read master sink volume after subscribe event", "error", err)
		return
	}

	vol := parseChannelVolumes(reply.ChannelVolumes)

	select {
	case sf.masterVolumeChanges <- vol:
	default:
		sf.logger.Debug("Dropped master volume change notification, consumer not keeping up")
	}
}

func (sf *paSessionFinder) GetAllSessions() ([]Session, error) {
	sessions := []Session{}

	// get the master sink session
	masterSink, err := sf.getMasterSinkSession()
	if err == nil {
		sessions = append(sessions, masterSink)
	} else {
		sf.logger.Warnw("Failed to get master audio sink session", "error", err)
	}

	// get the master source session
	masterSource, err := sf.getMasterSourceSession()
	if err == nil {
		sessions = append(sessions, masterSource)
	} else {
		sf.logger.Warnw("Failed to get master audio source session", "error", err)
	}

	// enumerate sink inputs and add sessions along the way
	if err := sf.enumerateAndAddSessions(&sessions); err != nil {
		sf.logger.Warnw("Failed to enumerate audio sessions", "error", err)
		return nil, fmt.Errorf("enumerate audio sessions: %w", err)
	}

	return sessions, nil
}

func (sf *paSessionFinder) Release() error {
	if err := sf.conn.Close(); err != nil {
		sf.logger.Warnw("Failed to close PulseAudio connection", "error", err)
		return fmt.Errorf("close PulseAudio connection: %w", err)
	}

	sf.logger.Debug("Released PA session finder instance")

	return nil
}

func (sf *paSessionFinder) getMasterSinkSession() (Session, error) {
	request := proto.GetSinkInfo{
		SinkIndex: proto.Undefined,
	}
	reply := proto.GetSinkInfoReply{}

	if err := sf.client.Request(&request, &reply); err != nil {
		sf.logger.Warnw("Failed to get master sink info", "error", err)
		return nil, fmt.Errorf("get master sink info: %w", err)
	}

	// create the master sink session
	sink := newMasterSession(sf.sessionLogger, sf.client, reply.SinkIndex, reply.Channels, true)

	return sink, nil
}

func (sf *paSessionFinder) getMasterSourceSession() (Session, error) {
	request := proto.GetSourceInfo{
		SourceIndex: proto.Undefined,
	}
	reply := proto.GetSourceInfoReply{}

	if err := sf.client.Request(&request, &reply); err != nil {
		sf.logger.Warnw("Failed to get master source info", "error", err)
		return nil, fmt.Errorf("get master source info: %w", err)
	}

	// create the master source session
	source := newMasterSession(sf.sessionLogger, sf.client, reply.SourceIndex, reply.Channels, false)

	return source, nil
}

func (sf *paSessionFinder) enumerateAndAddSessions(sessions *[]Session) error {
	request := proto.GetSinkInputInfoList{}
	reply := proto.GetSinkInputInfoListReply{}

	if err := sf.client.Request(&request, &reply); err != nil {
		sf.logger.Warnw("Failed to get sink input list", "error", err)
		return fmt.Errorf("get sink input list: %w", err)
	}

	for _, info := range reply {
		name, ok := info.Properties["application.process.binary"]

		if !ok {
			sf.logger.Warnw("Failed to get sink input's process name",
				"sinkInputIndex", info.SinkInputIndex)

			continue
		}

		// create the deej session object
		newSession := newPASession(sf.sessionLogger, sf.client, info.SinkInputIndex, info.Channels, name.String())

		// add it to our slice
		*sessions = append(*sessions, newSession)

	}

	return nil
}
