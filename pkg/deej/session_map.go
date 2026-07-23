package deej

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/sclead03/deej-x/pkg/deej/util"
	"github.com/thoas/go-funk"
	"go.uber.org/zap"
)

type sessionMap struct {
	deej   *Deej
	logger *zap.SugaredLogger

	m    map[string][]Session
	lock sync.Locker

	sessionFinder SessionFinder

	lastSessionRefresh time.Time
	unmappedSessions   []Session

	// lastMasterWriteFromDeej records when deej itself last wrote the master
	// session's volume (currently: only the SERENITY encoder, via a slider move
	// event targeting "master"). Used to keep the live master volume watcher
	// from echoing the encoder's own change back down to the firmware as if it
	// were an external (e.g. Windows volume mixer) change.
	lastMasterWriteFromDeej time.Time

	// lastMicMuteWriteFromButton records when SERENITY's RGB button last
	// triggered a mic mute toggle (HIDManager.handleReport). Used the same way
	// as lastMasterWriteFromDeej, but via a plain time window instead of a COM
	// eventContext GUID - tagging windowsMicMuter's SetMute call with a real
	// GUID reproduced a hard crash inside that specific syscall (see
	// hid_windows.go's ToggleMute), so this filtering happens here instead,
	// entirely in plain Go with no COM/syscall involvement.
	lastMicMuteWriteFromButton time.Time

	masterVolumeChangeConsumers []chan MasterVolumeUpdate
	micMuteChangeConsumers      []chan bool

	// takeoverMu guards awaitingPositionBaseline and channelTakeover below. Separate
	// from lock (which guards the session map m itself) since the beacon-reset
	// goroutine (setupOnBeacon) and the slider-move goroutine (setupOnSliderMove)
	// touch these two maps independently of any session map access.
	takeoverMu sync.Mutex

	// awaitingPositionBaseline tracks, per fader slider ID (1..NumSliders), whether
	// we're still waiting for a trustworthy first physical-position reading since
	// the last (re)connect - see setupOnBeacon/evaluatePositionBaseline. A slider
	// stuck at 0 (which may just mean muted, or genuinely resting at 0 - the host
	// can't tell the two apart, see CLAUDE.md "Soft Takeover at Connect") keeps
	// this true until it reports a real nonzero value.
	awaitingPositionBaseline map[int]bool

	// channelTakeover holds the in-progress per-app position sync state for any
	// fader slider currently showing the "move up"/"move down" screen. Absence
	// from this map means the slider is operating normally.
	channelTakeover map[int]*channelTakeoverState
}

// takeoverPhase is which direction a channel's position sync check is currently
// soliciting movement in.
type takeoverPhase uint8

const (
	takeoverPhaseUp takeoverPhase = iota
	takeoverPhaseDown
)

// takeoverEntry is one app on a slider's target list, and whether the physical
// slider has crossed its volume yet during a position sync check.
type takeoverEntry struct {
	session  Session
	target   float32
	captured bool
}

// channelTakeoverState is the in-progress position sync check for one fader
// slider - see CLAUDE.md "Soft Takeover at Connect". Apps are split into two
// piles at arm time (above the slider's starting position, and below it) and
// captured one at a time as the physical slider actually crosses each app's
// current volume; captured apps track the slider live from that point on,
// uncaptured apps are left completely untouched until their turn.
type channelTakeoverState struct {
	phase takeoverPhase
	up    []*takeoverEntry
	down  []*takeoverEntry
}

// takeoverMatchTolerance is how close a physical slider reading has to be to an
// app's current volume to be considered "already matching" - no need to wait
// for an exact bit-for-bit match. Mirrors the ~1% noise floor SERENITY's own
// firmware uses for its send-gating and mute/unmute takeover (kMinChange).
const takeoverMatchTolerance = 0.01

// MasterVolumeUpdate is delivered to SubscribeToMasterVolumeChanges subscribers.
// ForceSync marks the periodic settle push (see masterVolumeSettleDelay) that
// re-sends the authoritative current volume after a quiet period - subscribers
// should apply it even if it looks like a no-op/near-duplicate of the last
// value they pushed, since its entire purpose is correcting for a live update
// that may have been dropped along the way. Muted carries the master output's
// real WASAPI mute state alongside the volume, since the underlying OS
// notification always reports them together (see MasterVolumeNotification).
type MasterVolumeUpdate struct {
	Volume    float32
	Muted     bool
	ForceSync bool
}

// masterVolumeEchoSuppressWindow is how long after deej itself last wrote the
// master volume that a further observed change is assumed to be an echo of
// that same write, rather than a genuinely external change.
const masterVolumeEchoSuppressWindow = 500 * time.Millisecond

// masterVolumeSettleDelay is how long to wait after the last observed external
// master volume change before re-pushing the authoritative current volume.
// The live forwarding path is coalescing (latest-value-wins, see
// masterVolumeNotifyCallback/setupMasterVolumeWatcher) to keep up with rapid
// scrolling, which means the very last intermediate notification during a fast
// change can occasionally be the one that gets dropped. This settle push reads
// the true current volume directly rather than relying on whatever value made
// it through the live path, so the final state is always correct even if a
// live update along the way wasn't.
const masterVolumeSettleDelay = 100 * time.Millisecond

// micMuteEchoSuppressWindow is how long after SERENITY's RGB button last
// triggered a mic mute toggle that a further observed change is assumed to be
// an echo of that same write, rather than a genuinely external change. Same
// reasoning and magnitude as masterVolumeEchoSuppressWindow.
const micMuteEchoSuppressWindow = 500 * time.Millisecond

const (
	masterSessionName = "master" // master device volume
	systemSessionName = "system" // system sounds volume
	inputSessionName  = "mic"    // microphone input level

	// some targets need to be transformed before their correct audio sessions can be accessed.
	// this prefix identifies those targets to ensure they don't contradict with another similarly-named process
	specialTargetTransformPrefix = "deej."

	// targets the currently active window (Windows-only, experimental)
	specialTargetCurrentWindow = "current"

	// targets all currently unmapped sessions (experimental)
	specialTargetAllUnmapped = "unmapped"

	// this threshold constant assumes that re-acquiring all sessions is a kind of expensive operation,
	// and needs to be limited in some manner. this value was previously user-configurable through a config
	// key "process_refresh_frequency", but exposing this type of implementation detail seems wrong now
	minTimeBetweenSessionRefreshes = time.Second * 5

	// determines whether the map should be refreshed when a slider moves.
	// this is a bit greedy but allows us to ensure sessions are always re-acquired, which is
	// especially important for process groups (because you can have one ongoing session
	// always preventing lookup of other processes bound to its slider, which forces the user
	// to manually refresh sessions). a cleaner way to do this down the line is by registering to notifications
	// whenever a new session is added, but that's too hard to justify for how easy this solution is
	maxTimeBetweenSessionRefreshes = time.Second * 45
)

// this matches friendly device names (on Windows), e.g. "Headphones (Realtek Audio)"
var deviceSessionKeyPattern = regexp.MustCompile(`^.+ \(.+\)$`)

func newSessionMap(deej *Deej, logger *zap.SugaredLogger, sessionFinder SessionFinder) (*sessionMap, error) {
	logger = logger.Named("sessions")

	m := &sessionMap{
		deej:                     deej,
		logger:                   logger,
		m:                        make(map[string][]Session),
		lock:                     &sync.Mutex{},
		sessionFinder:            sessionFinder,
		awaitingPositionBaseline: make(map[int]bool),
		channelTakeover:          make(map[int]*channelTakeoverState),
	}

	logger.Debug("Created session map instance")

	return m, nil
}

func (m *sessionMap) initialize() error {
	if err := m.getAndAddSessions(); err != nil {
		m.logger.Warnw("Failed to get all sessions during session map initialization", "error", err)
		return fmt.Errorf("get all sessions during init: %w", err)
	}

	m.setupOnConfigReload()
	m.setupOnSliderMove()
	m.setupOnBeacon()

	if watcher, ok := m.sessionFinder.(MasterVolumeWatcher); ok {
		m.setupMasterVolumeWatcher(watcher)
	}

	if watcher, ok := m.sessionFinder.(MicMuteWatcher); ok {
		m.setupMicMuteWatcher(watcher)
	}

	return nil
}

func (m *sessionMap) release() error {
	if err := m.sessionFinder.Release(); err != nil {
		m.logger.Warnw("Failed to release session finder during session map release", "error", err)
		return fmt.Errorf("release session finder during release: %w", err)
	}

	return nil
}

// assumes the session map is clean!
// only call on a new session map or as part of refreshSessions which calls reset
func (m *sessionMap) getAndAddSessions() error {

	// mark that we're refreshing before anything else
	m.lastSessionRefresh = time.Now()
	m.unmappedSessions = nil

	sessions, err := m.sessionFinder.GetAllSessions()
	if err != nil {
		m.logger.Warnw("Failed to get sessions from session finder", "error", err)
		return fmt.Errorf("get sessions from SessionFinder: %w", err)
	}

	for _, session := range sessions {
		m.add(session)

		if !m.sessionMapped(session) {
			m.logger.Debugw("Tracking unmapped session", "session", session)
			m.unmappedSessions = append(m.unmappedSessions, session)
		}
	}

	m.logger.Infow("Got all audio sessions successfully", "sessionMap", m)

	return nil
}

func (m *sessionMap) setupOnConfigReload() {
	configReloadedChannel := m.deej.config.SubscribeToChanges()

	go func() {
		for {
			select {
			case <-configReloadedChannel:
				m.logger.Info("Detected config reload, attempting to re-acquire all audio sessions")
				m.refreshSessions(false)
			}
		}
	}()
}

func (m *sessionMap) setupOnSliderMove() {
	sliderEventsChannel := m.deej.serial.SubscribeToSliderMoveEvents()

	go func() {
		for {
			select {
			case event := <-sliderEventsChannel:
				m.handleSliderMoveEvent(event)
			}
		}
	}()
}

// setupOnBeacon resets the connect-time position sync check on every (re)connect
// - first launch, USB replug, or an app-level reconnect all re-run it the same
// way, matching how the display push already re-syncs everything on any
// (re)connect (see CLAUDE.md "Soft Takeover at Connect"). Any takeover left
// mid-sequence from a previous connection is dropped and its screen restored,
// since the physical/session state on the other side of a reconnect can't be
// trusted to still match what was being tracked.
func (m *sessionMap) setupOnBeacon() {
	beaconChannel := m.deej.serial.SubscribeToBeaconEvents()

	go func() {
		for range beaconChannel {
			writer := m.deej.serial.Writer()

			m.takeoverMu.Lock()
			for i := range m.channelTakeover {
				delete(m.channelTakeover, i)
				if writer != nil {
					if err := writer.SendChannelTakeoverDisplay(byte(i-1), TakeoverDisplayRestore); err != nil {
						m.logger.Warnw("Failed to restore channel display after reconnect", "slider", i, "error", err)
					}
				}
			}

			m.awaitingPositionBaseline = make(map[int]bool, m.deej.config.NumSliders)
			for i := 1; i <= m.deej.config.NumSliders; i++ {
				m.awaitingPositionBaseline[i] = true
			}
			m.takeoverMu.Unlock()
		}
	}()
}

// setupMasterVolumeWatcher forwards live, externally-sourced master volume
// changes (Windows volume mixer, media keys, another app) from the platform's
// push-based watcher to our own subscribers, filtering out deej's own writes
// (the SERENITY encoder) so they aren't echoed back down to the firmware. A
// settle timer re-pushes the authoritative current volume masterVolumeSettleDelay
// after the last observed change, to correct for the rare case where the live
// (coalescing, best-effort) path above dropped the actual final value.
func (m *sessionMap) setupMasterVolumeWatcher(watcher MasterVolumeWatcher) {
	changes := watcher.SubscribeToMasterVolumeChanges()

	go func() {
		settleTimer := time.NewTimer(masterVolumeSettleDelay)
		settleTimer.Stop()

		// lastMuted tracks the most recently observed mute state, for use by the
		// settle/ForceSync branch below - mute is a discrete flag with no
		// jitter/precision-loss concern the way the live volume path has, so
		// there's no need to re-derive it the way getMasterVolume() re-derives
		// volume; the last live observation is already authoritative.
		var lastMuted bool

		for {
			select {
			case notification, ok := <-changes:
				if !ok {
					return
				}

				if m.masterVolumeRecentlySetByDeej(masterVolumeEchoSuppressWindow) {
					m.logger.Debugw("Suppressed master volume change as deej's own echo", "notification", notification)
					continue
				}

				lastMuted = notification.Muted

				m.logger.Debugw("Forwarding external master volume change", "notification", notification, "consumers", len(m.masterVolumeChangeConsumers))
				m.forwardMasterVolumeChange(MasterVolumeUpdate{Volume: notification.Volume, Muted: notification.Muted})

				settleTimer.Reset(masterVolumeSettleDelay)

			case <-settleTimer.C:
				if vol, ok := m.getMasterVolume(); ok {
					m.logger.Debugw("Settling external master volume change", "volume", vol, "muted", lastMuted)
					m.forwardMasterVolumeChange(MasterVolumeUpdate{Volume: vol, Muted: lastMuted, ForceSync: true})
				}
			}
		}
	}()
}

// setupMicMuteWatcher forwards live, externally-sourced mic mute changes
// (Windows mic settings/taskbar, another app) from the platform's push-based
// watcher to our own subscribers. Unlike master volume there is no GUID-based
// own-write filter here — SetMute always passes nil eventContext for mic mute
// (a real GUID caused a hard crash, see hid_windows.go), so
// micMuteRecentlySetByButton's time-window is the only echo suppression against
// RGB button presses and encoder gestures that trigger mic mute actions.
func (m *sessionMap) setupMicMuteWatcher(watcher MicMuteWatcher) {
	changes := watcher.SubscribeToMicMuteChanges()

	// If the watcher supports early suppression, register the check so it can skip
	// the expensive allCaptureDevicesMuted query for button-press echoes before they
	// are even queued. The session_map check below remains as a belt-and-suspenders
	// fallback for watchers that don't implement SetMicMuteSuppressCheck (e.g. Linux).
	type suppressSetter interface {
		SetMicMuteSuppressCheck(func() bool)
	}
	if setter, ok := watcher.(suppressSetter); ok {
		setter.SetMicMuteSuppressCheck(func() bool {
			return m.micMuteRecentlySetByButton(micMuteEchoSuppressWindow)
		})
	}

	go func() {
		for muted := range changes {
			if m.micMuteRecentlySetByButton(micMuteEchoSuppressWindow) {
				m.logger.Debugw("Suppressed mic mute change as RGB button's own echo", "muted", muted)
				continue
			}

			m.logger.Debugw("Forwarding external mic mute change", "muted", muted, "consumers", len(m.micMuteChangeConsumers))
			m.forwardMicMuteChange(muted)
		}
	}()
}

// forwardMicMuteChange hands muted to every subscriber registered via
// SubscribeToMicMuteChanges, with the same latest-value-wins coalescing
// reasoning as forwardMasterVolumeChange.
func (m *sessionMap) forwardMicMuteChange(muted bool) {
	for _, consumer := range m.micMuteChangeConsumers {
		select {
		case consumer <- muted:
		default:
			select {
			case <-consumer:
			default:
			}
			select {
			case consumer <- muted:
			default:
			}
		}
	}
}

// SubscribeToMicMuteChanges returns a channel that receives mic mute updates
// whenever the mute state changes externally (not via SERENITY's RGB button).
// Nothing is ever sent if the current platform has no MicMuteWatcher.
func (m *sessionMap) SubscribeToMicMuteChanges() chan bool {
	ch := make(chan bool, 1)
	m.micMuteChangeConsumers = append(m.micMuteChangeConsumers, ch)
	return ch
}

// forwardMasterVolumeChange hands update to every subscriber registered via
// SubscribeToMasterVolumeChanges. Each consumer channel is capacity 1 with
// latest-value-wins semantics (same reasoning as masterVolumeNotifyCallback) -
// a consumer that's mid-serial-write when this fires gets the freshest value
// waiting for it, not a stale one stuck ahead of it in a queue. Note this means
// a ForceSync update could theoretically be evicted by a live update arriving
// a moment later before the consumer reads it - acceptable, since the live
// update carries an equally current (or newer) volume anyway.
func (m *sessionMap) forwardMasterVolumeChange(update MasterVolumeUpdate) {
	for _, consumer := range m.masterVolumeChangeConsumers {
		select {
		case consumer <- update:
		default:
			select {
			case <-consumer:
			default:
			}
			select {
			case consumer <- update:
			default:
			}
		}
	}
}

// SubscribeToMasterVolumeChanges returns a channel that receives master volume
// updates whenever the volume changes externally (not via deej's own writes),
// plus periodic settle corrections (see MasterVolumeUpdate.ForceSync). Nothing
// is ever sent if the current platform has no MasterVolumeWatcher.
func (m *sessionMap) SubscribeToMasterVolumeChanges() chan MasterVolumeUpdate {
	ch := make(chan MasterVolumeUpdate, 1)
	m.masterVolumeChangeConsumers = append(m.masterVolumeChangeConsumers, ch)
	return ch
}

// performance: explain why force == true at every such use to avoid unintended forced refresh spams
func (m *sessionMap) refreshSessions(force bool) {

	// make sure enough time passed since the last refresh, unless force is true in which case always clear
	if !force && m.lastSessionRefresh.Add(minTimeBetweenSessionRefreshes).After(time.Now()) {
		return
	}

	// clear and release sessions first
	m.clear()

	if err := m.getAndAddSessions(); err != nil {
		m.logger.Warnw("Failed to re-acquire all audio sessions", "error", err)
	} else {
		m.logger.Debug("Re-acquired sessions successfully")
	}
}

// returns true if a session is not currently mapped to any slider, false otherwise
// special sessions (master, system, mic) and device-specific sessions always count as mapped,
// even when absent from the config. this makes sense for every current feature that uses "unmapped sessions"
func (m *sessionMap) sessionMapped(session Session) bool {

	// count master/system/mic as mapped
	if funk.ContainsString([]string{masterSessionName, systemSessionName, inputSessionName}, session.Key()) {
		return true
	}

	// count device sessions as mapped
	if deviceSessionKeyPattern.MatchString(session.Key()) {
		return true
	}

	matchFound := false

	// look through the actual mappings
	m.deej.config.SliderMapping.iterate(func(sliderIdx int, targets []string) {
		for _, target := range targets {

			if m.targetHasSpecialTransform(target) {
				// group targets: a session is "mapped" if it's listed in the group's definition
				specialName := strings.TrimPrefix(strings.ToLower(target), specialTargetTransformPrefix)
				if members, ok := m.deej.config.ProcessGroups[specialName]; ok {
					for _, member := range members {
						if member == session.Key() {
							matchFound = true
							return
						}
					}
				}
				continue
			}

			// safe to assume this has a single element because we made sure there's no special transform
			target = m.resolveTarget(target)[0]

			if target == session.Key() {
				matchFound = true
				return
			}
		}
	})

	return matchFound
}

func (m *sessionMap) handleSliderMoveEvent(event SliderMoveEvent) {

	// Fader sliders (1..NumSliders) run through the connect-time position sync
	// check first. Slider 0 (the master encoder) has its own separate sync
	// mechanism (SET_MASTER_VOLUME/pushMasterState in display.go) and never
	// participates here. See CLAUDE.md "Soft Takeover at Connect".
	if event.SliderID != 0 && m.handlePositionSyncEvent(event) {
		return
	}

	// first of all, ensure our session map isn't moldy
	if m.lastSessionRefresh.Add(maxTimeBetweenSessionRefreshes).Before(time.Now()) {
		m.logger.Debug("Stale session map detected on slider move, refreshing")
		m.refreshSessions(true)
	}

	// get the targets mapped to this slider from the config
	targets, ok := m.deej.config.SliderMapping.get(event.SliderID)

	// if slider not found in config, silently ignore
	if !ok {
		return
	}

	targetFound := false
	adjustmentFailed := false

	// for each possible target for this slider...
	for _, target := range targets {

		// resolve the target name by cleaning it up and applying any special transformations.
		// depending on the transformation applied, this can result in more than one target name
		resolvedTargets := m.resolveTarget(target)

		// for each resolved target...
		for _, resolvedTarget := range resolvedTargets {

			// check the map for matching sessions
			sessions, ok := m.get(resolvedTarget)

			// no sessions matching this target - move on
			if !ok {
				continue
			}

			targetFound = true

			// iterate all matching sessions and adjust the volume of each one
			for _, session := range sessions {
				if session.GetVolume() != event.PercentValue {
					if err := session.SetVolume(event.PercentValue); err != nil {
						m.logger.Warnw("Failed to set target session volume", "error", err)
						adjustmentFailed = true
					} else if resolvedTarget == masterSessionName {
						m.markMasterVolumeSetByDeej()
					}
				}
			}
		}
	}

	// if we still haven't found a target or the volume adjustment failed, maybe look for the target again.
	// processes could've opened since the last time this slider moved.
	// if they haven't, the cooldown will take care to not spam it up
	if !targetFound {
		m.refreshSessions(false)
	} else if adjustmentFailed {

		// performance: the reason that forcing a refresh here is okay is that we'll only get here
		// when a session's SetVolume call errored, such as in the case of a stale master session
		// (or another, more catastrophic failure happens)
		m.refreshSessions(true)
	}
}

// handlePositionSyncEvent intercepts fader slider events for the connect-time
// position sync check (see CLAUDE.md "Soft Takeover at Connect"). Returns true
// if it fully handled this event - an in-progress takeover, or a baseline
// reading that either needs to keep waiting or just armed a new takeover - in
// which case the caller must not fall through to the normal per-target apply.
// Returns false when there's nothing to intercept (steady-state operation, or
// a baseline reading that found everything already matching), so the event
// should be applied normally like any other slider move.
func (m *sessionMap) handlePositionSyncEvent(event SliderMoveEvent) bool {
	m.takeoverMu.Lock()
	state, active := m.channelTakeover[event.SliderID]
	awaiting := m.awaitingPositionBaseline[event.SliderID]
	m.takeoverMu.Unlock()

	if active {
		m.advanceTakeover(event.SliderID, state, event.PercentValue)
		return true
	}

	if !awaiting {
		return false
	}

	// Still waiting for a trustworthy baseline. A reading of exactly 0 might just
	// mean the slider is muted - firmware fakes 0 on the wire while muted,
	// regardless of true physical position - and the host has no separate signal
	// to tell the two apart. Keep waiting rather than risk zeroing out apps that
	// aren't actually at 0; the first nonzero reading (whether from an unmute or
	// genuine movement) becomes the trustworthy baseline instead.
	if event.PercentValue == 0 {
		return true
	}

	m.takeoverMu.Lock()
	delete(m.awaitingPositionBaseline, event.SliderID)
	m.takeoverMu.Unlock()

	return m.armPositionSync(event.SliderID, event.PercentValue)
}

// armPositionSync compares slider sliderID's just-established physical position
// against the current volume of every app mapped to it. Apps within
// takeoverMatchTolerance are left alone; anything further off is split into an
// "above" and "below" pile and a takeover sequence is armed, prioritizing the
// "above" pile first per the design decision in CLAUDE.md. Returns true if it
// armed (this event is fully consumed) or false if everything already matched
// closely enough (let the event apply normally, same as any other move).
func (m *sessionMap) armPositionSync(sliderID int, position float32) bool {
	targets, ok := m.deej.config.SliderMapping.get(sliderID)
	if !ok {
		return false
	}

	var up, down []*takeoverEntry
	for _, target := range targets {
		for _, resolvedTarget := range m.resolveTarget(target) {
			sessions, ok := m.get(resolvedTarget)
			if !ok {
				continue
			}

			for _, session := range sessions {
				vol := session.GetVolume()
				diff := vol - position
				switch {
				case diff > takeoverMatchTolerance:
					up = append(up, &takeoverEntry{session: session, target: vol})
				case diff < -takeoverMatchTolerance:
					down = append(down, &takeoverEntry{session: session, target: vol})
				}
				// within tolerance: already matches, nothing to capture
			}
		}
	}

	if len(up) == 0 && len(down) == 0 {
		return false
	}

	state := &channelTakeoverState{up: up, down: down}
	if len(up) > 0 {
		state.phase = takeoverPhaseUp
	} else {
		state.phase = takeoverPhaseDown
	}

	m.takeoverMu.Lock()
	m.channelTakeover[sliderID] = state
	m.takeoverMu.Unlock()

	mode := TakeoverDisplayMoveUp
	if state.phase == takeoverPhaseDown {
		mode = TakeoverDisplayMoveDown
	}
	if writer := m.deej.serial.Writer(); writer != nil {
		if err := writer.SendChannelTakeoverDisplay(byte(sliderID-1), mode); err != nil {
			m.logger.Warnw("Failed to show takeover display", "slider", sliderID, "error", err)
		}
	}

	m.logger.Infow("Armed connect-time position sync", "slider", sliderID, "position", position, "up", len(up), "down", len(down))

	return true
}

// advanceTakeover applies one incoming slider reading to an in-progress
// position sync sequence: captures any not-yet-captured app in the current
// phase whose volume the slider has now reached, keeps every already-captured
// app (from this or an earlier phase) tracking the slider live, and flips to
// the down phase - or clears the takeover and restores the display, if there's
// no down phase left to run - once the current phase's apps are all captured.
func (m *sessionMap) advanceTakeover(sliderID int, state *channelTakeoverState, value float32) {
	entries := state.up
	if state.phase == takeoverPhaseDown {
		entries = state.down
	}

	allCaptured := true
	for _, e := range entries {
		if e.captured {
			continue
		}

		crossed := value >= e.target
		if state.phase == takeoverPhaseDown {
			crossed = value <= e.target
		}

		if crossed {
			e.captured = true
		} else {
			allCaptured = false
		}
	}

	for _, pile := range [][]*takeoverEntry{state.up, state.down} {
		for _, e := range pile {
			if e.captured && e.session.GetVolume() != value {
				if err := e.session.SetVolume(value); err != nil {
					m.logger.Warnw("Failed to set target session volume during position sync", "error", err)
				}
			}
		}
	}

	if !allCaptured {
		return
	}

	writer := m.deej.serial.Writer()

	if state.phase == takeoverPhaseUp && len(state.down) > 0 {
		state.phase = takeoverPhaseDown
		if writer != nil {
			if err := writer.SendChannelTakeoverDisplay(byte(sliderID-1), TakeoverDisplayMoveDown); err != nil {
				m.logger.Warnw("Failed to show takeover display", "slider", sliderID, "error", err)
			}
		}
		return
	}

	m.takeoverMu.Lock()
	delete(m.channelTakeover, sliderID)
	m.takeoverMu.Unlock()

	if writer != nil {
		if err := writer.SendChannelTakeoverDisplay(byte(sliderID-1), TakeoverDisplayRestore); err != nil {
			m.logger.Warnw("Failed to restore channel display", "slider", sliderID, "error", err)
		}
	}

	m.logger.Infow("Position sync complete", "slider", sliderID)
}

func (m *sessionMap) targetHasSpecialTransform(target string) bool {
	return strings.HasPrefix(target, specialTargetTransformPrefix)
}

func (m *sessionMap) resolveTarget(target string) []string {

	// start by ignoring the case
	target = strings.ToLower(target)

	// look for any special targets first, by examining the prefix
	if m.targetHasSpecialTransform(target) {
		return m.applyTargetTransform(strings.TrimPrefix(target, specialTargetTransformPrefix))
	}

	return []string{target}
}

func (m *sessionMap) applyTargetTransform(specialTargetName string) []string {

	// select the transformation based on its name
	switch specialTargetName {

	// get current active window
	case specialTargetCurrentWindow:
		currentWindowProcessNames, err := util.GetCurrentWindowProcessNames()

		// silently ignore errors here, as this is on deej's "hot path" (and it could just mean the user's running linux)
		if err != nil {
			return nil
		}

		// we could have gotten a non-lowercase names from that, so let's ensure we return ones that are lowercase
		for targetIdx, target := range currentWindowProcessNames {
			currentWindowProcessNames[targetIdx] = strings.ToLower(target)
		}

		// remove dupes
		return funk.UniqString(currentWindowProcessNames)

	// get currently unmapped sessions
	case specialTargetAllUnmapped:
		targetKeys := make([]string, len(m.unmappedSessions))
		for sessionIdx, session := range m.unmappedSessions {
			targetKeys[sessionIdx] = session.Key()
		}

		return targetKeys

	default:
		if members, ok := m.deej.config.ProcessGroups[specialTargetName]; ok {
			return m.filterGroupMembers(members)
		}
	}

	return nil
}

// filterGroupMembers returns the group's process list with any process that is
// explicitly named in slider_mapping removed — explicit assignments take priority
// over group membership, matching the deej.unmapped exclusion behavior.
func (m *sessionMap) filterGroupMembers(members []string) []string {
	explicit := make(map[string]bool)
	m.deej.config.SliderMapping.iterate(func(_ int, targets []string) {
		for _, t := range targets {
			if !m.targetHasSpecialTransform(t) {
				explicit[strings.ToLower(t)] = true
			}
		}
	})

	var result []string
	for _, member := range members {
		if !explicit[member] {
			result = append(result, member)
		}
	}
	return result
}

func (m *sessionMap) add(value Session) {
	m.lock.Lock()
	defer m.lock.Unlock()

	key := value.Key()

	existing, ok := m.m[key]
	if !ok {
		m.m[key] = []Session{value}
	} else {
		m.m[key] = append(existing, value)
	}
}

func (m *sessionMap) get(key string) ([]Session, bool) {
	m.lock.Lock()
	defer m.lock.Unlock()

	value, ok := m.m[key]
	return value, ok
}

// getMasterVolume returns the current master output volume scalar (0.0–1.0),
// or false if the master session isn't currently available.
func (m *sessionMap) getMasterVolume() (float32, bool) {
	sessions, ok := m.get(masterSessionName)
	if !ok || len(sessions) == 0 {
		return 0, false
	}

	return sessions[0].GetVolume(), true
}

// getMasterMuted returns the master output session's current real WASAPI mute
// state, or false/false if the master session isn't available or doesn't
// support mute queries (e.g. the Linux master session, which has no GetMuted
// method - per-platform mute support is optional, mirroring how
// MasterVolumeWatcher/MicMuteWatcher are optional session finder interfaces).
func (m *sessionMap) getMasterMuted() (bool, bool) {
	sessions, ok := m.get(masterSessionName)
	if !ok || len(sessions) == 0 {
		return false, false
	}

	type muteGetter interface {
		GetMuted() (bool, error)
	}

	mg, ok := sessions[0].(muteGetter)
	if !ok {
		return false, false
	}

	muted, err := mg.GetMuted()
	if err != nil {
		return false, false
	}

	return muted, true
}

// toggleMasterMuted flips the master output session's real WASAPI mute state
// and returns the resulting value. Used by SERENITY's encoder mute button
// (CMD_REQUEST_MASTER_MUTE_TOGGLE, see display.go's
// handleMasterMuteToggleRequest) - mirrors windowsMicMuter.ToggleMute, but
// goes through masterSession.SetMuted's eventCtx-tagged SetMute call so the
// resulting notification is filtered out by GUID at the source
// (session_finder_windows.go's masterVolumeNotifyCallback) rather than relying
// only on the time-window backstop mic mute needs.
func (m *sessionMap) toggleMasterMuted() (bool, error) {
	sessions, ok := m.get(masterSessionName)
	if !ok || len(sessions) == 0 {
		return false, errors.New("master session not available")
	}

	type muteToggler interface {
		GetMuted() (bool, error)
		SetMuted(bool) error
	}

	mt, ok := sessions[0].(muteToggler)
	if !ok {
		return false, errors.New("master session does not support mute toggling")
	}

	muted, err := mt.GetMuted()
	if err != nil {
		return false, fmt.Errorf("get current master mute state: %w", err)
	}

	nowMuted := !muted
	if err := mt.SetMuted(nowMuted); err != nil {
		return false, fmt.Errorf("set master mute state: %w", err)
	}

	m.markMasterVolumeSetByDeej()

	return nowMuted, nil
}

// markMasterVolumeSetByDeej records that deej itself just wrote the master
// session's volume (see lastMasterWriteFromDeej).
func (m *sessionMap) markMasterVolumeSetByDeej() {
	m.lock.Lock()
	defer m.lock.Unlock()

	m.lastMasterWriteFromDeej = time.Now()
}

// masterVolumeRecentlySetByDeej reports whether deej itself wrote the master
// session's volume within the given window.
func (m *sessionMap) masterVolumeRecentlySetByDeej(window time.Duration) bool {
	m.lock.Lock()
	defer m.lock.Unlock()

	return time.Since(m.lastMasterWriteFromDeej) < window
}

// markMicMuteSetByButton records that SERENITY's RGB button just triggered a
// mic mute toggle (see lastMicMuteWriteFromButton).
func (m *sessionMap) markMicMuteSetByButton() {
	m.lock.Lock()
	defer m.lock.Unlock()

	m.lastMicMuteWriteFromButton = time.Now()
}

// micMuteRecentlySetByButton reports whether SERENITY's RGB button triggered
// a mic mute toggle within the given window.
func (m *sessionMap) micMuteRecentlySetByButton(window time.Duration) bool {
	m.lock.Lock()
	defer m.lock.Unlock()

	return time.Since(m.lastMicMuteWriteFromButton) < window
}

func (m *sessionMap) clear() {
	m.lock.Lock()
	defer m.lock.Unlock()

	m.logger.Debug("Releasing and clearing all audio sessions")

	for key, sessions := range m.m {
		for _, session := range sessions {
			session.Release()
		}

		delete(m.m, key)
	}

	m.logger.Debug("Session map cleared")
}

func (m *sessionMap) String() string {
	m.lock.Lock()
	defer m.lock.Unlock()

	sessionCount := 0

	for _, value := range m.m {
		sessionCount += len(value)
	}

	return fmt.Sprintf("<%d audio sessions>", sessionCount)
}
