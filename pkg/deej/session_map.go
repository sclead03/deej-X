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
}

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
		deej:          deej,
		logger:        logger,
		m:             make(map[string][]Session),
		lock:          &sync.Mutex{},
		sessionFinder: sessionFinder,
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
// watcher to our own subscribers. Unlike master volume, no echo-suppression
// window is needed here: changes caused by SERENITY's own RGB button are
// already filtered out at the platform layer by GUID (see
// wcaSessionFinder.micMuteNotifyCallback), since deej never writes mic mute
// any other way.
func (m *sessionMap) setupMicMuteWatcher(watcher MicMuteWatcher) {
	changes := watcher.SubscribeToMicMuteChanges()

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

			// ignore special transforms
			if m.targetHasSpecialTransform(target) {
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
	}

	return nil
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
