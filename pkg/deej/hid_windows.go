package deej

import (
	"errors"
	"fmt"
	"io"
	"runtime"
	"strings"
	"syscall"
	"unsafe"

	ole "github.com/go-ole/go-ole"
	wca "github.com/moutend/go-wca"
	"go.uber.org/zap"
	"golang.org/x/sys/windows"
)

// setupapi.dll and hid.dll — loaded lazily, no CGO required
var (
	modSetupAPI = syscall.NewLazyDLL("setupapi.dll")
	modHID      = syscall.NewLazyDLL("hid.dll")

	procSetupDiGetClassDevs             = modSetupAPI.NewProc("SetupDiGetClassDevsW")
	procSetupDiEnumDeviceInterfaces     = modSetupAPI.NewProc("SetupDiEnumDeviceInterfaces")
	procSetupDiGetDeviceInterfaceDetail = modSetupAPI.NewProc("SetupDiGetDeviceInterfaceDetailW")
	procSetupDiDestroyDeviceInfoList    = modSetupAPI.NewProc("SetupDiDestroyDeviceInfoList")
	procHidDGetHidGuid                  = modHID.NewProc("HidD_GetHidGuid")
	procHidDGetPreparsedData            = modHID.NewProc("HidD_GetPreparsedData")
	procHidDFreePreparsedData           = modHID.NewProc("HidD_FreePreparsedData")
	procHidPGetCaps                     = modHID.NewProc("HidP_GetCaps")
)

const (
	digcfPresent         = 0x00000002
	digcfDeviceInterface = 0x00000010
	invalidHandleValue   = ^uintptr(0)

	hidpStatusSuccess = 0x00110000

	// micMuteUsagePage/micMuteUsage identify the RGB button's dedicated
	// top-level collection (firmware's kMicMuteDesc in main.cpp) - SERENITY's
	// composite HID interface also exposes a separate Consumer Control
	// collection (Play/Pause) on the same VID/PID, as its own top-level
	// collection (Windows splits each into its own device path, e.g.
	// HID\VID_xxxx&PID_xxxx&MI_02&COL01 vs &COL02). Matching on VID/PID alone
	// picks whichever collection enumerates first, which silently grabbed the
	// wrong one once a second collection existed - must check actual usage.
	micMuteUsagePage = 0xFF00
	micMuteUsage     = 0x01
)

// hidpCaps mirrors the fixed-size HIDP_CAPS struct (hidpi.h) - must be the
// exact real size (62 bytes) since HidP_GetCaps writes into it directly;
// only Usage/UsagePage are actually read here.
type hidpCaps struct {
	Usage                     uint16
	UsagePage                 uint16
	InputReportByteLength     uint16
	OutputReportByteLength    uint16
	FeatureReportByteLength   uint16
	Reserved                  [17]uint16
	NumberLinkCollectionNodes uint16
	NumberInputButtonCaps     uint16
	NumberInputValueCaps      uint16
	NumberInputDataIndices    uint16
	NumberOutputButtonCaps    uint16
	NumberOutputValueCaps     uint16
	NumberOutputDataIndices   uint16
	NumberFeatureButtonCaps   uint16
	NumberFeatureValueCaps    uint16
	NumberFeatureDataIndices  uint16
}

// matchesMicMuteCollection reports whether the opened HID handle is the RGB
// button's vendor-defined top-level collection (vs. e.g. the Consumer Control
// one sharing the same VID/PID).
func matchesMicMuteCollection(handle windows.Handle) bool {
	var preparsedData uintptr

	ret, _, _ := procHidDGetPreparsedData.Call(uintptr(handle), uintptr(unsafe.Pointer(&preparsedData)))
	if ret == 0 {
		return false
	}
	defer procHidDFreePreparsedData.Call(preparsedData)

	var caps hidpCaps
	status, _, _ := procHidPGetCaps.Call(preparsedData, uintptr(unsafe.Pointer(&caps)))

	return status == hidpStatusSuccess && caps.UsagePage == micMuteUsagePage && caps.Usage == micMuteUsage
}

type hidGUID struct {
	Data1 uint32
	Data2 uint16
	Data3 uint16
	Data4 [8]byte
}

type spDeviceInterfaceData struct {
	CbSize             uint32
	InterfaceClassGuid hidGUID
	Flags              uint32
	Reserved           uintptr
}

// spDeviceInterfaceDetailHeader mirrors the fixed part of SP_DEVICE_INTERFACE_DETAIL_DATA_W.
// unsafe.Sizeof of this struct gives the correct cbSize value for SetupDiGetDeviceInterfaceDetailW.
type spDeviceInterfaceDetailHeader struct {
	CbSize     uint32
	DevicePath [1]uint16
}

// spDeviceInterfaceDetailData holds the header plus a large enough path buffer.
type spDeviceInterfaceDetailData struct {
	CbSize     uint32
	DevicePath [2048]uint16
}

func getHIDClassGUID() hidGUID {
	var guid hidGUID
	procHidDGetHidGuid.Call(uintptr(unsafe.Pointer(&guid)))
	return guid
}

// openSERENITY enumerates HID devices and returns the SERENITY device as an io.ReadCloser.
func openSERENITY() (io.ReadCloser, error) {
	guid := getHIDClassGUID()

	hDevInfo, _, _ := procSetupDiGetClassDevs.Call(
		uintptr(unsafe.Pointer(&guid)),
		0,
		0,
		digcfPresent|digcfDeviceInterface,
	)
	if hDevInfo == invalidHandleValue {
		return nil, errors.New("SetupDiGetClassDevs returned invalid handle")
	}
	defer procSetupDiDestroyDeviceInfoList.Call(hDevInfo)

	vidStr := fmt.Sprintf("vid_%04x", hidVendorID)
	pidStr := fmt.Sprintf("pid_%04x", hidProductID)
	cbSize := uint32(unsafe.Sizeof(spDeviceInterfaceDetailHeader{}))

	for i := uint32(0); ; i++ {
		var ifaceData spDeviceInterfaceData
		ifaceData.CbSize = uint32(unsafe.Sizeof(ifaceData))

		ret, _, _ := procSetupDiEnumDeviceInterfaces.Call(
			hDevInfo,
			0,
			uintptr(unsafe.Pointer(&guid)),
			uintptr(i),
			uintptr(unsafe.Pointer(&ifaceData)),
		)
		if ret == 0 {
			break
		}

		var detail spDeviceInterfaceDetailData
		detail.CbSize = cbSize

		procSetupDiGetDeviceInterfaceDetail.Call(
			hDevInfo,
			uintptr(unsafe.Pointer(&ifaceData)),
			uintptr(unsafe.Pointer(&detail)),
			uintptr(unsafe.Sizeof(detail)),
			0,
			0,
		)

		path := syscall.UTF16ToString(detail.DevicePath[:])
		lower := strings.ToLower(path)

		if strings.Contains(lower, vidStr) && strings.Contains(lower, pidStr) {
			pathPtr, err := syscall.UTF16PtrFromString(path)
			if err != nil {
				return nil, fmt.Errorf("convert device path: %w", err)
			}

			handle, err := windows.CreateFile(
				pathPtr,
				windows.GENERIC_READ,
				windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
				nil,
				windows.OPEN_EXISTING,
				0,
				0,
			)
			if err != nil {
				// this VID/PID can have multiple top-level collections (e.g. the
				// Consumer Control one) - a share violation on one candidate
				// shouldn't abort the search for the right one.
				continue
			}

			if !matchesMicMuteCollection(handle) {
				windows.CloseHandle(handle)
				continue
			}

			return &winHIDHandle{handle: handle}, nil
		}
	}

	return nil, errors.New("SERENITY HID device not found")
}

type winHIDHandle struct {
	handle windows.Handle
}

func (h *winHIDHandle) Read(p []byte) (int, error) {
	var n uint32
	err := windows.ReadFile(h.handle, p, &n, nil)
	return int(n), err
}

func (h *winHIDHandle) Close() error {
	return windows.CloseHandle(h.handle)
}

// windowsMicMuter implements MicMuter via WASAPI/MMDeviceAPI.
type windowsMicMuter struct {
	logger *zap.SugaredLogger
}

func newMicMuter(logger *zap.SugaredLogger) (MicMuter, error) {
	return &windowsMicMuter{logger: logger.Named("mic_muter")}, nil
}

// queryCaptureAllMuted enumerates all active capture devices via de and returns
// true only if every one of them is muted. Returns false if there are no active
// capture devices. Assumes COM is already initialized on the calling thread.
func queryCaptureAllMuted(de *wca.IMMDeviceEnumerator) (bool, error) {
	var dc *wca.IMMDeviceCollection
	if err := de.EnumAudioEndpoints(wca.ECapture, wca.DEVICE_STATE_ACTIVE, &dc); err != nil {
		return false, fmt.Errorf("enum capture endpoints: %w", err)
	}
	defer dc.Release()

	var count uint32
	if err := dc.GetCount(&count); err != nil {
		return false, fmt.Errorf("get device count: %w", err)
	}

	if count == 0 {
		return false, nil
	}

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

		var muted bool
		getMuteErr := aev.GetMute(&muted)
		aev.Release()
		dev.Release()

		if getMuteErr != nil {
			continue
		}
		if !muted {
			return false, nil
		}
	}

	return true, nil
}

// applyToDevices enumerates active capture devices and sets mute on those whose
// friendly name contains any of the targets (case-insensitive substring). If
// targets contains the sentinel mute.all (muted=true) or unmute.all (muted=false)
// every active capture device is affected.
//
// Same LockOSThread/CoInitializeEx threading discipline as withCaptureVolume —
// see that function for the full rationale. SetMute eventContext is always nil
// for the same crash-avoidance reason documented on the old ToggleMute.
func (m *windowsMicMuter) applyToDevices(muted bool, targets []string) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	if err := ole.CoInitializeEx(0, ole.COINIT_APARTMENTTHREADED); err != nil {
		const eFalse = 1
		oleError := &ole.OleError{}
		if errors.As(err, &oleError) {
			if oleError.Code() != eFalse {
				return fmt.Errorf("CoInitializeEx: %w", err)
			}
		} else {
			return fmt.Errorf("CoInitializeEx: %w", err)
		}
	}
	defer ole.CoUninitialize()

	var de *wca.IMMDeviceEnumerator
	if err := wca.CoCreateInstance(
		wca.CLSID_MMDeviceEnumerator, 0, wca.CLSCTX_ALL,
		wca.IID_IMMDeviceEnumerator, &de,
	); err != nil {
		return fmt.Errorf("create IMMDeviceEnumerator: %w", err)
	}
	defer de.Release()

	var dc *wca.IMMDeviceCollection
	if err := de.EnumAudioEndpoints(wca.ECapture, wca.DEVICE_STATE_ACTIVE, &dc); err != nil {
		return fmt.Errorf("enum capture endpoints: %w", err)
	}
	defer dc.Release()

	var count uint32
	if err := dc.GetCount(&count); err != nil {
		return fmt.Errorf("get device count: %w", err)
	}

	sentinel := micMuteSentinelAll
	if !muted {
		sentinel = micUnmuteSentinelAll
	}
	applyAll := false
	for _, t := range targets {
		if strings.EqualFold(t, sentinel) {
			applyAll = true
			break
		}
	}

	applied := 0
	for i := uint32(0); i < count; i++ {
		var dev *wca.IMMDevice
		if err := dc.Item(i, &dev); err != nil {
			continue
		}

		matches := applyAll
		if !matches {
			var ps *wca.IPropertyStore
			if err := dev.OpenPropertyStore(wca.STGM_READ, &ps); err == nil {
				value := &wca.PROPVARIANT{}
				if err := ps.GetValue(&wca.PKEY_Device_FriendlyName, value); err == nil {
					lowerName := strings.ToLower(value.String())
					for _, t := range targets {
						if !strings.EqualFold(t, sentinel) && strings.Contains(lowerName, strings.ToLower(t)) {
							matches = true
							break
						}
					}
				}
				ps.Release()
			}
		}

		if matches {
			var aev *wca.IAudioEndpointVolume
			if err := dev.Activate(wca.IID_IAudioEndpointVolume, wca.CLSCTX_ALL, nil, &aev); err == nil {
				if err := aev.SetMute(muted, nil); err == nil {
					applied++
				}
				aev.Release()
			}
		}

		dev.Release()
	}

	m.logger.Debugw("Applied mute to capture devices", "muted", muted, "applied", applied, "total", count)
	return nil
}

func (m *windowsMicMuter) MuteDevices(targets []string) error {
	return m.applyToDevices(true, targets)
}

func (m *windowsMicMuter) UnmuteDevices(targets []string) error {
	return m.applyToDevices(false, targets)
}

// IsMuted reports whether all active capture devices are muted. Returns false
// if no capture devices are active. Same LockOSThread/CoInitializeEx threading
// discipline as applyToDevices — see that function for the full rationale.
func (m *windowsMicMuter) IsMuted() (bool, error) {
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

	return queryCaptureAllMuted(de)
}
