package deej

// SessionFinder represents an entity that can find all current audio sessions
type SessionFinder interface {
	GetAllSessions() ([]Session, error)

	Release() error
}

// MasterVolumeWatcher is implemented by session finders that can push live
// notifications when the master output volume changes, sourced from a real
// OS-level event/callback mechanism (WASAPI's RegisterControlChangeNotify on
// Windows, PulseAudio's native Subscribe mechanism on Linux) - never polling.
// The returned channel receives every observed change, including ones caused
// by deej itself (e.g. the SERENITY encoder); the caller is responsible for
// filtering those out via sessionMap.masterVolumeRecentlySetByDeej.
type MasterVolumeWatcher interface {
	SubscribeToMasterVolumeChanges() <-chan float32
}
