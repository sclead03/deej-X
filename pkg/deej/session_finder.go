package deej

// SessionFinder represents an entity that can find all current audio sessions
type SessionFinder interface {
	GetAllSessions() ([]Session, error)

	Release() error
}

// MasterVolumeNotification is delivered by MasterVolumeWatcher whenever the
// master output's volume or mute state changes. Both are reported together
// since the underlying OS notification (WASAPI's AUDIO_VOLUME_NOTIFICATION_DATA,
// PulseAudio's sink info) always carries them as one unit.
type MasterVolumeNotification struct {
	Volume float32
	Muted  bool
}

// MasterVolumeWatcher is implemented by session finders that can push live
// notifications when the master output volume or mute state changes, sourced
// from a real OS-level event/callback mechanism (WASAPI's RegisterControlChangeNotify
// on Windows, PulseAudio's native Subscribe mechanism on Linux) - never polling.
// The returned channel receives every observed change, including ones caused
// by deej itself (e.g. the SERENITY encoder); the caller is responsible for
// filtering those out via sessionMap.masterVolumeRecentlySetByDeej.
type MasterVolumeWatcher interface {
	SubscribeToMasterVolumeChanges() <-chan MasterVolumeNotification
}

// MicMuteWatcher is implemented by session finders that can push live notifications
// when the aggregate capture mute state changes — true only if every active capture
// device is muted. Fires on changes to any capture device, not just the default.
// Windows-only today (see hid_windows.go) — no Linux implementation exists.
type MicMuteWatcher interface {
	SubscribeToMicMuteChanges() <-chan bool
}
