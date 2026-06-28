package deej

import (
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	ole "github.com/go-ole/go-ole"
	wca "github.com/moutend/go-wca"
	"go.uber.org/zap"
)

type wcaSessionFinder struct {
	logger        *zap.SugaredLogger
	sessionLogger *zap.SugaredLogger

	eventCtx *ole.GUID // needed for some session actions to successfully notify other audio consumers

	// needed for device change notifications
	mmDeviceEnumerator      *wca.IMMDeviceEnumerator
	mmNotificationClient    *wca.IMMNotificationClient
	lastDefaultDeviceChange time.Time

	// our master input and output sessions
	masterOut *masterSession
	masterIn  *masterSession

	// pushes live master output volume/mute changes (see MasterVolumeWatcher);
	// aevCallback is built once and re-registered against each new masterOut's
	// IAudioEndpointVolume as it's (re)created in GetAllSessions.
	// Capacity 1, latest-value-wins (see masterVolumeNotifyCallback) - a fast
	// volume scroll fires far more notifications than the serial link can drain,
	// so this is intentionally a coalescing slot, not a queue: an unconsumed
	// stale value gets overwritten rather than blocking out the newest one.
	masterVolumeChanges chan MasterVolumeNotification
	aevCallback         *iAudioEndpointVolumeCallback

	// pushes live mic mute aggregate changes (see MicMuteWatcher); micMuteCallback
	// is registered against every active capture device's IAudioEndpointVolume in
	// registerMicMuteChangeCallback. A single shared callback instance handles all
	// devices — on any notification it re-queries all devices and pushes true only
	// if every one is muted. Capacity 1, latest-value-wins (same as masterVolumeChanges).
	micMuteChanges  chan bool
	micMuteCallback *iAudioEndpointVolumeCallback

	// captureAevs holds the IAudioEndpointVolume references for every active capture
	// device that has micMuteCallback registered. They must be kept alive for the
	// duration of the registration — releasing a reference destroys the COM object
	// and silently invalidates RegisterControlChangeNotify. Rebuilt (old refs
	// released first) on each registerMicMuteChangeCallback call and on each
	// handleDeviceStateChanged call. captureAevsMu must be held when reading or
	// writing this slice, as both callers run on different goroutines.
	captureAevsMu sync.Mutex
	captureAevs   []*wca.IAudioEndpointVolume

	// micMuteSuppressCheck, if set, is called at the start of handleMicMuteNotification
	// before the expensive allCaptureDevicesMuted query. Returns true when the
	// notification is a self-triggered echo of a button press and should be skipped.
	// Wired up by session_map.go's setupMicMuteWatcher via SetMicMuteSuppressCheck.
	micMuteSuppressCheck func() bool
}

const (

	// there's no real mystery here, it's just a random GUID
	myteriousGUID = "{1ec920a1-7db8-44ba-9779-e5d28ed9f330}"

	// the notification client will call this multiple times in quick succession based on the
	// default device's assigned media roles, so we need to filter out the extraneous calls
	minDefaultDeviceChangeThreshold = 100 * time.Millisecond

	// prefix for device sessions in logger
	deviceSessionFormat = "device.%s"
)

func newSessionFinder(logger *zap.SugaredLogger) (SessionFinder, error) {
	sf := &wcaSessionFinder{
		logger:              logger.Named("session_finder"),
		sessionLogger:       logger.Named("sessions"),
		eventCtx:            ole.NewGUID(myteriousGUID),
		masterVolumeChanges: make(chan MasterVolumeNotification, 1),
		micMuteChanges:      make(chan bool, 1),
	}

	sf.logger.Debug("Created WCA session finder instance")

	return sf, nil
}

// SubscribeToMasterVolumeChanges implements MasterVolumeWatcher.
func (sf *wcaSessionFinder) SubscribeToMasterVolumeChanges() <-chan MasterVolumeNotification {
	return sf.masterVolumeChanges
}

// SubscribeToMicMuteChanges implements MicMuteWatcher.
func (sf *wcaSessionFinder) SubscribeToMicMuteChanges() <-chan bool {
	return sf.micMuteChanges
}

// SetMicMuteSuppressCheck registers a function that returns true when a WASAPI
// mic mute notification should be skipped without querying the aggregate — i.e.
// the notification is an echo of a button press, not an external OS change.
// Called from session_map.go's setupMicMuteWatcher.
func (sf *wcaSessionFinder) SetMicMuteSuppressCheck(f func() bool) {
	sf.micMuteSuppressCheck = f
}

func (sf *wcaSessionFinder) GetAllSessions() ([]Session, error) {
	sessions := []Session{}

	// Pin this goroutine to its current OS thread before touching COM. This
	// function registers long-lived notification sinks (default device change,
	// master volume change) against the STA apartment created below; if the
	// calling goroutine were later rescheduled onto a different OS thread by the
	// Go scheduler, the apartment those sinks belong to would be orphaned.
	// LockOSThread is cheap to call repeatedly (refcounted per goroutine) and is
	// never unlocked here by design - see the CoUninitialize comment below.
	runtime.LockOSThread()

	// we must call this every time we're about to list devices, i think. could be wrong
	if err := ole.CoInitializeEx(0, ole.COINIT_APARTMENTTHREADED); err != nil {

		// if the error is "Incorrect function" that corresponds to 0x00000001,
		// which represents E_FALSE in COM error handling. this is fine for this function,
		// and just means that the call was redundant.
		const eFalse = 1
		oleError := &ole.OleError{}

		if errors.As(err, &oleError) {
			if oleError.Code() == eFalse {
				sf.logger.Warn("CoInitializeEx failed with E_FALSE due to redundant invocation")
			} else {
				sf.logger.Warnw("Failed to call CoInitializeEx",
					"isOleError", true,
					"error", err,
					"oleError", oleError)

				return nil, fmt.Errorf("call CoInitializeEx: %w", err)
			}
		} else {
			sf.logger.Warnw("Failed to call CoInitializeEx",
				"isOleError", false,
				"error", err,
				"oleError", nil)

			return nil, fmt.Errorf("call CoInitializeEx: %w", err)
		}

	}

	// Deliberately not calling ole.CoUninitialize() here (it used to be deferred).
	// This was the actual bug behind master volume live-tracking never firing:
	// registerDefaultDeviceChangeCallback/registerMasterVolumeChangeCallback below
	// register notification sinks against the apartment just created on this
	// thread, then the deferred CoUninitialize tore that same apartment down the
	// instant this function returned - before the audio engine ever got a chance
	// to dispatch a callback into it. The registrations are meant to live for the
	// life of the process, so the apartment needs to outlive this function call.
	// Letting COM stay initialized on this (now permanently locked, see above)
	// thread until process exit is the standard pattern for a long-running
	// service and costs nothing meaningful here.

	// ensure we have a device enumerator
	if err := sf.getDeviceEnumerator(); err != nil {
		sf.logger.Warnw("Failed to get device enumerator", "error", err)
		return nil, fmt.Errorf("get device enumerator: %w", err)
	}

	// get the currently active default output and input devices.
	// please note that this can return a nil defaultInputEndpoint, in case there are no input devices connected.
	// you must check it for non-nil
	defaultOutputEndpoint, defaultInputEndpoint, err := sf.getDefaultAudioEndpoints()
	if err != nil {
		sf.logger.Warnw("Failed to get default audio endpoints", "error", err)
		return nil, fmt.Errorf("get default audio endpoints: %w", err)
	}
	defer defaultOutputEndpoint.Release()

	if defaultInputEndpoint != nil {
		defer defaultInputEndpoint.Release()
	}

	// receive notifications whenever the default device changes (only do this once)
	if sf.mmNotificationClient == nil {
		if err := sf.registerDefaultDeviceChangeCallback(); err != nil {
			sf.logger.Warnw("Failed to register default device change callback", "error", err)
			return nil, fmt.Errorf("register default device change callback: %w", err)
		}
	}

	// get the master output session
	sf.masterOut, err = sf.getMasterSession(defaultOutputEndpoint, masterSessionName, masterSessionName)
	if err != nil {
		sf.logger.Warnw("Failed to get master audio output session", "error", err)
		return nil, fmt.Errorf("get master audio output session: %w", err)
	}

	sessions = append(sessions, sf.masterOut)

	// live-track external changes to the master output volume (Windows volume
	// mixer, media keys, another app). best-effort: a failure here just means
	// the OLED won't live-sync, so it's logged but not fatal.
	if err := sf.registerMasterVolumeChangeCallback(); err != nil {
		sf.logger.Warnw("Failed to register master volume change callback", "error", err)
	}

	// get the master input session, if a default input device exists
	if defaultInputEndpoint != nil {
		sf.masterIn, err = sf.getMasterSession(defaultInputEndpoint, inputSessionName, inputSessionName)
		if err != nil {
			sf.logger.Warnw("Failed to get master audio input session", "error", err)
			return nil, fmt.Errorf("get master audio input session: %w", err)
		}

		sessions = append(sessions, sf.masterIn)
	}

	// Register mic mute callbacks on all active capture devices (not just the default).
	// Best-effort, same reasoning as registerMasterVolumeChangeCallback above.
	if err := sf.registerMicMuteChangeCallback(); err != nil {
		sf.logger.Warnw("Failed to register mic mute change callbacks", "error", err)
	}

	// enumerate all devices and make their "master" sessions bindable by friendly name;
	// for output devices, this is also where we enumerate process sessions
	if err := sf.enumerateAndAddSessions(&sessions); err != nil {
		sf.logger.Warnw("Failed to enumerate device sessions", "error", err)
		return nil, fmt.Errorf("enumerate device sessions: %w", err)
	}

	return sessions, nil
}

func (sf *wcaSessionFinder) Release() error {

	// skip unregistering the mmnotificationclient, as it's not implemented in go-wca
	if sf.mmDeviceEnumerator != nil {
		sf.mmDeviceEnumerator.Release()
	}

	sf.logger.Debug("Released WCA session finder instance")

	return nil
}

func (sf *wcaSessionFinder) getDeviceEnumerator() error {

	// get the IMMDeviceEnumerator (only once)
	if sf.mmDeviceEnumerator == nil {
		if err := wca.CoCreateInstance(
			wca.CLSID_MMDeviceEnumerator,
			0,
			wca.CLSCTX_ALL,
			wca.IID_IMMDeviceEnumerator,
			&sf.mmDeviceEnumerator,
		); err != nil {
			sf.logger.Warnw("Failed to call CoCreateInstance", "error", err)
			return fmt.Errorf("call CoCreateInstance: %w", err)
		}
	}

	return nil
}

func (sf *wcaSessionFinder) getDefaultAudioEndpoints() (*wca.IMMDevice, *wca.IMMDevice, error) {

	// get the default audio endpoints as IMMDevice instances
	var mmOutDevice *wca.IMMDevice
	var mmInDevice *wca.IMMDevice

	if err := sf.mmDeviceEnumerator.GetDefaultAudioEndpoint(wca.ERender, wca.EConsole, &mmOutDevice); err != nil {
		sf.logger.Warnw("Failed to call GetDefaultAudioEndpoint (out)", "error", err)
		return nil, nil, fmt.Errorf("call GetDefaultAudioEndpoint (out): %w", err)
	}

	// allow this call to fail (not all users have a microphone connected)
	if err := sf.mmDeviceEnumerator.GetDefaultAudioEndpoint(wca.ECapture, wca.EConsole, &mmInDevice); err != nil {
		sf.logger.Warn("No default input device detected, proceeding without it (\"mic\" will not work)")
		mmInDevice = nil
	}

	return mmOutDevice, mmInDevice, nil
}

func (sf *wcaSessionFinder) registerDefaultDeviceChangeCallback() error {
	sf.mmNotificationClient = &wca.IMMNotificationClient{}
	sf.mmNotificationClient.VTable = &wca.IMMNotificationClientVtbl{}

	sf.mmNotificationClient.VTable.QueryInterface = syscall.NewCallback(sf.noopCallback)
	sf.mmNotificationClient.VTable.AddRef = syscall.NewCallback(sf.noopCallback)
	sf.mmNotificationClient.VTable.Release = syscall.NewCallback(sf.noopCallback)
	sf.mmNotificationClient.VTable.OnDeviceAdded = syscall.NewCallback(sf.noopCallback)
	sf.mmNotificationClient.VTable.OnDeviceRemoved = syscall.NewCallback(sf.noopCallback)
	sf.mmNotificationClient.VTable.OnPropertyValueChanged = syscall.NewCallback(sf.noopCallback)

	sf.mmNotificationClient.VTable.OnDefaultDeviceChanged = syscall.NewCallback(sf.defaultDeviceChangedCallback)
	// OnDeviceStateChanged fires when a device becomes active or inactive (e.g. USB mic plugged/unplugged).
	// We need to re-register callbacks on newly-active devices and push the updated aggregate.
	sf.mmNotificationClient.VTable.OnDeviceStateChanged = syscall.NewCallback(sf.deviceStateChangedCallback)

	if err := sf.mmDeviceEnumerator.RegisterEndpointNotificationCallback(sf.mmNotificationClient); err != nil {
		sf.logger.Warnw("Failed to call RegisterEndpointNotificationCallback", "error", err)
		return fmt.Errorf("call RegisterEndpointNotificationCallback: %w", err)
	}

	return nil
}

func (sf *wcaSessionFinder) getMasterSession(mmDevice *wca.IMMDevice, key string, loggerKey string) (*masterSession, error) {

	var audioEndpointVolume *wca.IAudioEndpointVolume

	if err := mmDevice.Activate(wca.IID_IAudioEndpointVolume, wca.CLSCTX_ALL, nil, &audioEndpointVolume); err != nil {
		sf.logger.Warnw("Failed to activate AudioEndpointVolume for master session", "error", err)
		return nil, fmt.Errorf("activate master session: %w", err)
	}

	// create the master session
	master, err := newMasterSession(sf.sessionLogger, audioEndpointVolume, sf.eventCtx, key, loggerKey)
	if err != nil {
		sf.logger.Warnw("Failed to create master session instance", "error", err)
		return nil, fmt.Errorf("create master session: %w", err)
	}

	return master, nil
}

func (sf *wcaSessionFinder) enumerateAndAddSessions(sessions *[]Session) error {

	// get list of devices
	var deviceCollection *wca.IMMDeviceCollection

	if err := sf.mmDeviceEnumerator.EnumAudioEndpoints(wca.EAll, wca.DEVICE_STATE_ACTIVE, &deviceCollection); err != nil {
		sf.logger.Warnw("Failed to enumerate active audio endpoints", "error", err)
		return fmt.Errorf("enumerate active audio endpoints: %w", err)
	}

	// check how many devices there are
	var deviceCount uint32

	if err := deviceCollection.GetCount(&deviceCount); err != nil {
		sf.logger.Warnw("Failed to get device count from device collection", "error", err)
		return fmt.Errorf("get device count from device collection: %w", err)
	}

	// for each device:
	for deviceIdx := uint32(0); deviceIdx < deviceCount; deviceIdx++ {

		// get its IMMDevice instance
		var endpoint *wca.IMMDevice

		if err := deviceCollection.Item(deviceIdx, &endpoint); err != nil {
			sf.logger.Warnw("Failed to get device from device collection",
				"deviceIdx", deviceIdx,
				"error", err)

			return fmt.Errorf("get device %d from device collection: %w", deviceIdx, err)
		}
		defer endpoint.Release()

		// get its IMMEndpoint instance to figure out if it's an output device (and we need to enumerate its process sessions later)
		dispatch, err := endpoint.QueryInterface(wca.IID_IMMEndpoint)
		if err != nil {
			sf.logger.Warnw("Failed to query IMMEndpoint for device",
				"deviceIdx", deviceIdx,
				"error", err)

			return fmt.Errorf("query device %d IMMEndpoint: %w", deviceIdx, err)
		}

		// get the device's property store
		var propertyStore *wca.IPropertyStore

		if err := endpoint.OpenPropertyStore(wca.STGM_READ, &propertyStore); err != nil {
			sf.logger.Warnw("Failed to open property store for endpoint",
				"deviceIdx", deviceIdx,
				"error", err)

			return fmt.Errorf("open endpoint %d property store: %w", deviceIdx, err)
		}
		defer propertyStore.Release()

		// query the property store for the device's description and friendly name
		value := &wca.PROPVARIANT{}

		if err := propertyStore.GetValue(&wca.PKEY_Device_DeviceDesc, value); err != nil {
			sf.logger.Warnw("Failed to get description for device",
				"deviceIdx", deviceIdx,
				"error", err)

			return fmt.Errorf("get device %d description: %w", deviceIdx, err)
		}

		// device description i.e. "Headphones"
		endpointDescription := strings.ToLower(value.String())

		if err := propertyStore.GetValue(&wca.PKEY_Device_FriendlyName, value); err != nil {
			sf.logger.Warnw("Failed to get friendly name for device",
				"deviceIdx", deviceIdx,
				"error", err)

			return fmt.Errorf("get device %d friendly name: %w", deviceIdx, err)
		}

		// device friendly name i.e. "Headphones (Realtek Audio)"
		endpointFriendlyName := value.String()

		// receive a useful object instead of our dispatch
		endpointType := (*wca.IMMEndpoint)(unsafe.Pointer(dispatch))
		defer endpointType.Release()

		var dataFlow uint32
		if err := endpointType.GetDataFlow(&dataFlow); err != nil {
			sf.logger.Warnw("Failed to get data flow for endpoint",
				"deviceIdx", deviceIdx,
				"error", err)

			return fmt.Errorf("get device %d data flow: %w", deviceIdx, err)
		}

		sf.logger.Debugw("Enumerated device info",
			"deviceIdx", deviceIdx,
			"deviceDescription", endpointDescription,
			"deviceFriendlyName", endpointFriendlyName,
			"dataFlow", dataFlow)

		// if the device is an output device, enumerate and add its per-process audio sessions
		if dataFlow == wca.ERender {
			if err := sf.enumerateAndAddProcessSessions(endpoint, endpointFriendlyName, sessions); err != nil {
				sf.logger.Warnw("Failed to enumerate and add process sessions for device",
					"deviceIdx", deviceIdx,
					"error", err)

				return fmt.Errorf("enumerate and add device %d process sessions: %w", deviceIdx, err)
			}
		}

		// for all devices (both input and output), add a named "master" session that can be addressed
		// by using the device's friendly name (as appears when the user left-clicks the speaker icon in the tray)
		newSession, err := sf.getMasterSession(endpoint,
			endpointFriendlyName,
			fmt.Sprintf(deviceSessionFormat, endpointDescription))

		if err != nil {
			sf.logger.Warnw("Failed to get master session for device",
				"deviceIdx", deviceIdx,
				"error", err)

			return fmt.Errorf("get device %d master session: %w", deviceIdx, err)
		}

		// add it to our slice
		*sessions = append(*sessions, newSession)
	}

	return nil
}

func (sf *wcaSessionFinder) enumerateAndAddProcessSessions(
	endpoint *wca.IMMDevice,
	endpointFriendlyName string,
	sessions *[]Session,
) error {

	sf.logger.Debugw("Enumerating and adding process sessions for audio output device",
		"deviceFriendlyName", endpointFriendlyName)

	// query the given IMMDevice's IAudioSessionManager2 interface
	var audioSessionManager2 *wca.IAudioSessionManager2

	if err := endpoint.Activate(
		wca.IID_IAudioSessionManager2,
		wca.CLSCTX_ALL,
		nil,
		&audioSessionManager2,
	); err != nil {

		sf.logger.Warnw("Failed to activate endpoint as IAudioSessionManager2", "error", err)
		return fmt.Errorf("activate endpoint: %w", err)
	}
	defer audioSessionManager2.Release()

	// get its IAudioSessionEnumerator
	var sessionEnumerator *wca.IAudioSessionEnumerator

	if err := audioSessionManager2.GetSessionEnumerator(&sessionEnumerator); err != nil {
		return err
	}
	defer sessionEnumerator.Release()

	// check how many audio sessions there are
	var sessionCount int

	if err := sessionEnumerator.GetCount(&sessionCount); err != nil {
		sf.logger.Warnw("Failed to get session count from session enumerator", "error", err)
		return fmt.Errorf("get session count: %w", err)
	}

	sf.logger.Debugw("Got session count from session enumerator", "count", sessionCount)

	// for each session:
	for sessionIdx := 0; sessionIdx < sessionCount; sessionIdx++ {

		// get the IAudioSessionControl
		var audioSessionControl *wca.IAudioSessionControl
		if err := sessionEnumerator.GetSession(sessionIdx, &audioSessionControl); err != nil {
			sf.logger.Warnw("Failed to get session from session enumerator",
				"error", err,
				"sessionIdx", sessionIdx)

			return fmt.Errorf("get session %d from enumerator: %w", sessionIdx, err)
		}

		// query its IAudioSessionControl2
		dispatch, err := audioSessionControl.QueryInterface(wca.IID_IAudioSessionControl2)
		if err != nil {
			sf.logger.Warnw("Failed to query session's IAudioSessionControl2",
				"error", err,
				"sessionIdx", sessionIdx)

			return fmt.Errorf("query session %d IAudioSessionControl2: %w", sessionIdx, err)
		}

		// we no longer need the IAudioSessionControl, release it
		audioSessionControl.Release()

		// receive a useful object instead of our dispatch
		audioSessionControl2 := (*wca.IAudioSessionControl2)(unsafe.Pointer(dispatch))

		var pid uint32

		// get the session's PID
		if err := audioSessionControl2.GetProcessId(&pid); err != nil {

			// if this is the system sounds session, GetProcessId will error with an undocumented
			// AUDCLNT_S_NO_CURRENT_PROCESS (0x889000D) - this is fine, we actually want to treat it a bit differently
			// The first part of this condition will be true if the call to IsSystemSoundsSession fails
			// The second part will be true if the original error mesage from GetProcessId doesn't contain this magical
			// error code (in decimal format).
			isSystemSoundsErr := audioSessionControl2.IsSystemSoundsSession()
			if isSystemSoundsErr != nil && !strings.Contains(err.Error(), "143196173") {

				// of course, if it's not the system sounds session, we got a problem
				sf.logger.Warnw("Failed to query session's pid",
					"error", err,
					"isSystemSoundsError", isSystemSoundsErr,
					"sessionIdx", sessionIdx)

				return fmt.Errorf("query session %d pid: %w", sessionIdx, err)
			}

			// update 2020/08/31: this is also the exact case for UWP applications, so we should no longer override the PID.
			// it will successfully update whenever we call GetProcessId for e.g. Video.UI.exe, despite the error being non-nil.
		}

		// get its ISimpleAudioVolume
		dispatch, err = audioSessionControl2.QueryInterface(wca.IID_ISimpleAudioVolume)
		if err != nil {
			sf.logger.Warnw("Failed to query session's ISimpleAudioVolume",
				"error", err,
				"sessionIdx", sessionIdx)

			return fmt.Errorf("query session %d ISimpleAudioVolume: %w", sessionIdx, err)
		}

		// make it useful, again
		simpleAudioVolume := (*wca.ISimpleAudioVolume)(unsafe.Pointer(dispatch))

		// create the deej session object
		newSession, err := newWCASession(sf.sessionLogger, audioSessionControl2, simpleAudioVolume, pid, sf.eventCtx)
		if err != nil {

			// this could just mean this process is already closed by now, and the session will be cleaned up later by the OS
			if !errors.Is(err, errNoSuchProcess) {
				sf.logger.Warnw("Failed to create new WCA session instance",
					"error", err,
					"sessionIdx", sessionIdx)

				return fmt.Errorf("create wca session for session %d: %w", sessionIdx, err)
			}

			// in this case, log it and release the session's handles, then skip to the next one
			sf.logger.Debugw("Process already exited, skipping session and releasing handles", "pid", pid)

			audioSessionControl2.Release()
			simpleAudioVolume.Release()

			continue
		}

		// add it to our slice
		*sessions = append(*sessions, newSession)
	}

	return nil
}

func (sf *wcaSessionFinder) defaultDeviceChangedCallback(
	this *wca.IMMNotificationClient,
	EDataFlow, eRole uint32,
	lpcwstr uintptr,
) (hResult uintptr) {

	// filter out calls that happen in rapid succession
	now := time.Now()

	if sf.lastDefaultDeviceChange.Add(minDefaultDeviceChangeThreshold).After(now) {
		return
	}

	sf.lastDefaultDeviceChange = now

	sf.logger.Debug("Default audio device changed, marking master sessions as stale")
	if sf.masterOut != nil {
		sf.masterOut.markAsStale()
	}

	if sf.masterIn != nil {
		sf.masterIn.markAsStale()
	}

	return
}
func (sf *wcaSessionFinder) noopCallback() (hResult uintptr) {
	return
}

// iAudioEndpointVolumeCallback and its vtable are hand-rolled because go-wca only
// defines the interface's IID (wca.IID_IAudioEndpointVolumeCallback), not the
// struct/vtable types - mirrors the IMMNotificationClient pattern above. Likewise,
// go-wca's own IAudioEndpointVolume.RegisterControlChangeNotify wrapper is stubbed
// to E_NOTIMPL even on Windows (the real vtable slot is there, just unwired), so
// registerMasterVolumeChangeCallback below calls that slot directly via syscall,
// same as every aev* helper in the go-wca package itself does for other AEV methods.
type iAudioEndpointVolumeCallback struct {
	VTable *iAudioEndpointVolumeCallbackVtbl
}

type iAudioEndpointVolumeCallbackVtbl struct {
	QueryInterface uintptr
	AddRef         uintptr
	Release        uintptr
	OnNotify       uintptr
}

// audioVolumeNotificationData mirrors the fixed-size prefix of Win32's
// AUDIO_VOLUME_NOTIFICATION_DATA; the trailing per-channel volume array is unused
// here and omitted.
type audioVolumeNotificationData struct {
	GuidEventContext ole.GUID
	BMuted           int32
	FMasterVolume    float32
	NChannels        uint32
}

// registerMasterVolumeChangeCallback (re)registers our IAudioEndpointVolumeCallback
// against the current sf.masterOut's IAudioEndpointVolume. Safe to call repeatedly
// across session refreshes - the callback object is built once and reused; there's
// no UnregisterControlChangeNotify call on the old endpoint because go-wca stubs
// that too, but it's moot since releasing the old IAudioEndpointVolume (handled
// elsewhere via masterSession.Release) drops the registration along with it -
// the same reasoning already applied to mmNotificationClient in Release() below.
func (sf *wcaSessionFinder) registerMasterVolumeChangeCallback() error {
	if sf.masterOut == nil {
		return errors.New("no master output session to register against")
	}

	if sf.aevCallback == nil {
		sf.aevCallback = &iAudioEndpointVolumeCallback{
			VTable: &iAudioEndpointVolumeCallbackVtbl{
				QueryInterface: syscall.NewCallback(sf.noopCallback),
				AddRef:         syscall.NewCallback(sf.noopCallback),
				Release:        syscall.NewCallback(sf.noopCallback),
				OnNotify:       syscall.NewCallback(sf.masterVolumeNotifyCallback),
			},
		}
	}

	aev := sf.masterOut.volume

	hr, _, _ := syscall.Syscall(
		aev.VTable().RegisterControlChangeNotify,
		2,
		uintptr(unsafe.Pointer(aev)),
		uintptr(unsafe.Pointer(sf.aevCallback)),
		0)
	if hr != 0 {
		return ole.NewError(hr)
	}

	sf.logger.Debug("Registered master volume change callback")

	return nil
}

// masterVolumeNotifyCallback is invoked by the Windows audio engine (on its own
// thread) whenever the registered endpoint's volume or mute state changes -
// whether caused by deej itself or externally (Windows volume mixer, media keys,
// another app). It only reads plain memory and does a non-blocking channel send,
// so it's safe to run without CoInitializeEx and without blocking the audio engine.
//
// pNotify is declared as a typed pointer (not uintptr) so syscall.NewCallback
// marshals it directly - same approach already used for "this" in
// defaultDeviceChangedCallback above. Declaring it uintptr and converting via
// unsafe.Pointer would trip go vet's unsafeptr check (fabricating a pointer from
// an arbitrary integer), since vet can't know this uintptr is actually a valid
// pointer handed to us by the COM callback ABI.
func (sf *wcaSessionFinder) masterVolumeNotifyCallback(this uintptr, pNotify *audioVolumeNotificationData) (hResult uintptr) {
	if pNotify == nil {
		return
	}

	sf.logger.Debugw("Master volume notify callback fired",
		"volume", pNotify.FMasterVolume,
		"muted", pNotify.BMuted != 0,
		"isOwnWrite", pNotify.GuidEventContext == *sf.eventCtx)

	// deej's own writes (currently: only the SERENITY encoder, via SetVolume with
	// this exact context) are filtered here precisely; sessionMap also applies a
	// time-window filter as a platform-agnostic backstop for watchers that can't
	// distinguish by context (e.g. the Linux PulseAudio watcher).
	if pNotify.GuidEventContext == *sf.eventCtx {
		return
	}

	notification := MasterVolumeNotification{
		Volume: pNotify.FMasterVolume,
		Muted:  pNotify.BMuted != 0,
	}

	// Latest-value-wins: if the consumer hasn't drained the previous value yet,
	// evict it and replace it with this one instead of dropping this one. A
	// fast volume change fires many more of these than the serial link can
	// drain; we'd rather the consumer eventually see the final settled value
	// than get stuck behind a queue of stale intermediate ones.
	select {
	case sf.masterVolumeChanges <- notification:
	default:
		select {
		case <-sf.masterVolumeChanges:
		default:
		}
		select {
		case sf.masterVolumeChanges <- notification:
		default:
		}
	}

	return
}

// registerMicMuteChangeCallback registers our IAudioEndpointVolumeCallback against
// every currently-active capture device's IAudioEndpointVolume. A single shared
// callback object handles all devices; on any notification it re-queries the full
// aggregate state. Re-registration of the same callback on the same endpoint is a
// no-op per WASAPI, so this is safe to call on each GetAllSessions refresh and
// from the hotplug handler. The aev reference is released immediately after
// registration — the audio engine holds its own reference and the registration
// persists until UnregisterControlChangeNotify is called. Assumes COM is already
// initialized on the calling thread (must be called from GetAllSessions context).
func (sf *wcaSessionFinder) registerMicMuteChangeCallback() error {
	if sf.micMuteCallback == nil {
		sf.micMuteCallback = &iAudioEndpointVolumeCallback{
			VTable: &iAudioEndpointVolumeCallbackVtbl{
				QueryInterface: syscall.NewCallback(sf.noopCallback),
				AddRef:         syscall.NewCallback(sf.noopCallback),
				Release:        syscall.NewCallback(sf.noopCallback),
				OnNotify:       syscall.NewCallback(sf.micMuteNotifyCallback),
			},
		}
	}

	var dc *wca.IMMDeviceCollection
	if err := sf.mmDeviceEnumerator.EnumAudioEndpoints(wca.ECapture, wca.DEVICE_STATE_ACTIVE, &dc); err != nil {
		return fmt.Errorf("enum capture endpoints: %w", err)
	}
	defer dc.Release()

	var count uint32
	if err := dc.GetCount(&count); err != nil {
		return fmt.Errorf("get device count: %w", err)
	}

	// Lock before touching captureAevs: handleDeviceStateChanged runs on a
	// goroutine spawned by the Windows audio callback and can run concurrently
	// with this function.
	sf.captureAevsMu.Lock()
	defer sf.captureAevsMu.Unlock()

	// Release previously-held aevs from the last registration cycle before
	// repopulating. Releasing an aev would normally destroy the COM object and
	// kill the registration, but since we're rebuilding the full list right now
	// that's intentional — stale devices get cleaned up, active ones get fresh refs.
	for _, old := range sf.captureAevs {
		old.Release()
	}
	sf.captureAevs = sf.captureAevs[:0]

	registered := 0
	for i := uint32(0); i < count; i++ {
		var dev *wca.IMMDevice
		if err := dc.Item(i, &dev); err != nil {
			continue
		}

		var aev *wca.IAudioEndpointVolume
		if err := dev.Activate(wca.IID_IAudioEndpointVolume, wca.CLSCTX_ALL, nil, &aev); err != nil {
			dev.Release()
			continue
		}
		dev.Release()

		hr, _, _ := syscall.Syscall(
			aev.VTable().RegisterControlChangeNotify,
			2,
			uintptr(unsafe.Pointer(aev)),
			uintptr(unsafe.Pointer(sf.micMuteCallback)),
			0)

		if hr != 0 {
			sf.logger.Warnw("Failed to register mic mute callback on device", "deviceIdx", i, "hr", hr)
			aev.Release()
			continue
		}

		// Keep aev alive — releasing it would destroy the COM object and silently
		// invalidate the RegisterControlChangeNotify registration.
		sf.captureAevs = append(sf.captureAevs, aev)
		registered++
	}

	sf.logger.Debugw("Registered mic mute change callbacks on capture devices", "registered", registered, "total", count)
	return nil
}

// allCaptureDevicesMuted reports whether every currently-active capture device is
// muted. Returns false if there are no active capture devices. Initializes its own
// COM apartment — safe to call from any goroutine (e.g. micMuteNotifyCallback's
// spawned goroutine or handleDeviceStateChanged). Must NOT be called from a context
// that already holds a CoInitializeEx without CoUninitialize (would decrement the
// refcount prematurely); use the enumerator directly in that case.
func (sf *wcaSessionFinder) allCaptureDevicesMuted() (bool, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	if err := ole.CoInitializeEx(0, ole.COINIT_APARTMENTTHREADED); err != nil {
		const eFalse = 1
		oleError := &ole.OleError{}
		if errors.As(err, &oleError) {
			if oleError.Code() != eFalse {
				return false, fmt.Errorf("CoInitializeEx: %w", err)
			}
		} else {
			return false, fmt.Errorf("CoInitializeEx: %w", err)
		}
	}
	defer ole.CoUninitialize()

	var de *wca.IMMDeviceEnumerator
	if err := wca.CoCreateInstance(
		wca.CLSID_MMDeviceEnumerator, 0, wca.CLSCTX_ALL,
		wca.IID_IMMDeviceEnumerator, &de,
	); err != nil {
		return false, fmt.Errorf("create IMMDeviceEnumerator: %w", err)
	}
	defer de.Release()

	return queryCaptureAllMuted(de, sf.logger)
}

// micMuteNotifyCallback is invoked by the Windows audio engine whenever any
// registered capture endpoint's volume or mute state changes — registered on
// all active capture devices (not just the default), so it fires for any of them.
// It does nothing but copy the notification by value and hand off to a goroutine;
// the goroutine re-queries all capture devices for the aggregate muted state
// rather than forwarding pNotify.BMuted from whichever single device fired.
//
// No GUID-based own-write filter: SetMute always passes nil eventContext (a real
// GUID caused a hard crash — see prior incident in memory). Self-triggered changes
// are filtered downstream by sessionMap's micMuteRecentlySetByButton time-window.
func (sf *wcaSessionFinder) micMuteNotifyCallback(this uintptr, pNotify *audioVolumeNotificationData) (hResult uintptr) {
	if pNotify == nil {
		return
	}

	notification := *pNotify
	go sf.handleMicMuteNotification(notification)

	return
}

func (sf *wcaSessionFinder) handleMicMuteNotification(pNotify audioVolumeNotificationData) {
	sf.logger.Debugw("Mic mute notify callback fired", "deviceMuted", pNotify.BMuted != 0)

	// Skip the expensive aggregate query when this notification is a self-triggered
	// echo of a button press — applyMicMuteAction already reads back and pushes the
	// authoritative state via Path A (hid.go IsMuted → SendMicMuteState). Running
	// allCaptureDevicesMuted here would be wasted work; the result gets suppressed
	// downstream anyway by session_map's micMuteRecentlySetByButton check.
	if sf.micMuteSuppressCheck != nil && sf.micMuteSuppressCheck() {
		sf.logger.Debug("Mic mute notify: suppressing echo of button press, skipping aggregate query")
		return
	}

	muted, err := sf.allCaptureDevicesMuted()
	if err != nil {
		sf.logger.Warnw("Failed to query all-capture-muted aggregate, using notified value", "error", err)
		muted = pNotify.BMuted != 0
	}

	sf.logger.Debugw("Mic mute aggregate computed", "allMuted", muted)

	select {
	case sf.micMuteChanges <- muted:
	default:
		select {
		case <-sf.micMuteChanges:
		default:
		}
		select {
		case sf.micMuteChanges <- muted:
		default:
		}
	}
}

// deviceStateChangedCallback is invoked by Windows when any audio endpoint's state
// changes (e.g. a USB microphone is plugged in or unplugged). Immediately hands off
// to a goroutine to avoid blocking the audio engine's notification thread.
func (sf *wcaSessionFinder) deviceStateChangedCallback(
	this *wca.IMMNotificationClient,
	pwstrDeviceId uintptr,
	dwNewState uint32,
) (hResult uintptr) {
	sf.logger.Debugw("Device state changed callback fired", "newState", dwNewState)
	go sf.handleDeviceStateChanged()
	return
}

// handleDeviceStateChanged runs on its own goroutine when a capture device's state
// changes. It fully rebuilds the captureAevs list under captureAevsMu (releasing
// stale refs, re-registering micMuteCallback on all currently-active devices), then
// pushes the updated aggregate mute state to the firmware. A full rebuild rather than
// an append-only update avoids accumulating duplicate aev entries: the previous
// append-only approach added ALL active devices on every state-change event, not
// just newly-active ones, causing unbounded growth between session refreshes and an
// unprotected concurrent write against registerMicMuteChangeCallback.
func (sf *wcaSessionFinder) handleDeviceStateChanged() {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	if err := ole.CoInitializeEx(0, ole.COINIT_APARTMENTTHREADED); err != nil {
		const eFalse = 1
		oleError := &ole.OleError{}
		if errors.As(err, &oleError) {
			if oleError.Code() != eFalse {
				sf.logger.Warnw("CoInitializeEx failed in handleDeviceStateChanged", "error", err)
				return
			}
		} else {
			sf.logger.Warnw("CoInitializeEx failed in handleDeviceStateChanged", "error", err)
			return
		}
	}
	defer ole.CoUninitialize()

	var de *wca.IMMDeviceEnumerator
	if err := wca.CoCreateInstance(
		wca.CLSID_MMDeviceEnumerator, 0, wca.CLSCTX_ALL,
		wca.IID_IMMDeviceEnumerator, &de,
	); err != nil {
		sf.logger.Warnw("Failed to create IMMDeviceEnumerator in handleDeviceStateChanged", "error", err)
		return
	}
	defer de.Release()

	// Full rebuild of captureAevs under lock. Release old aev refs first so stale
	// devices are cleaned up, then enumerate and register fresh for all current
	// active capture devices. captureAevsMu is also held by registerMicMuteChangeCallback
	// on the session map goroutine, which may run concurrently with this function.
	sf.captureAevsMu.Lock()
	if sf.micMuteCallback != nil {
		for _, old := range sf.captureAevs {
			old.Release()
		}
		sf.captureAevs = sf.captureAevs[:0]

		var dc *wca.IMMDeviceCollection
		if err := de.EnumAudioEndpoints(wca.ECapture, wca.DEVICE_STATE_ACTIVE, &dc); err == nil {
			var count uint32
			if dc.GetCount(&count) == nil {
				for i := uint32(0); i < count; i++ {
					var dev *wca.IMMDevice
					if err := dc.Item(i, &dev); err != nil {
						continue
					}
					var aev *wca.IAudioEndpointVolume
					if err := dev.Activate(wca.IID_IAudioEndpointVolume, wca.CLSCTX_ALL, nil, &aev); err != nil {
						dev.Release()
						continue
					}
					dev.Release()
					hr, _, _ := syscall.Syscall(
						aev.VTable().RegisterControlChangeNotify,
						2,
						uintptr(unsafe.Pointer(aev)),
						uintptr(unsafe.Pointer(sf.micMuteCallback)),
						0)
					if hr != 0 {
						aev.Release()
						continue
					}
					sf.captureAevs = append(sf.captureAevs, aev)
				}
			}
			dc.Release()
		}
	}
	sf.logger.Debugw("Rebuilt captureAevs after device state change", "registered", len(sf.captureAevs))
	sf.captureAevsMu.Unlock()

	// Push the updated aggregate state (using the same enumerator, already on this COM-initialized thread).
	muted, err := queryCaptureAllMuted(de, sf.logger)
	if err != nil {
		sf.logger.Warnw("Failed to query aggregate after device state change", "error", err)
		return
	}

	sf.logger.Debugw("Device state changed, pushing updated aggregate", "allMuted", muted)

	select {
	case sf.micMuteChanges <- muted:
	default:
		select {
		case <-sf.micMuteChanges:
		default:
		}
		select {
		case sf.micMuteChanges <- muted:
		default:
		}
	}
}
