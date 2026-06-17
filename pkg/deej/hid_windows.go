package deej

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"syscall"
	"unsafe"

	ole "github.com/go-ole/go-ole"
	wca "github.com/moutend/go-wca"
	"golang.org/x/sys/windows"
	"go.uber.org/zap"
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
)

const (
	digcfPresent         = 0x00000002
	digcfDeviceInterface = 0x00000010
	invalidHandleValue   = ^uintptr(0)
)

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
				return nil, fmt.Errorf("open HID device: %w", err)
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

func (m *windowsMicMuter) ToggleMute() error {
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

	var dd *wca.IMMDevice
	if err := de.GetDefaultAudioEndpoint(wca.ECapture, wca.EConsole, &dd); err != nil {
		return fmt.Errorf("get default capture endpoint: %w", err)
	}
	defer dd.Release()

	var aev *wca.IAudioEndpointVolume
	if err := dd.Activate(wca.IID_IAudioEndpointVolume, wca.CLSCTX_ALL, nil, &aev); err != nil {
		return fmt.Errorf("activate IAudioEndpointVolume: %w", err)
	}
	defer aev.Release()

	var muted bool
	if err := aev.GetMute(&muted); err != nil {
		return fmt.Errorf("get mute state: %w", err)
	}

	if err := aev.SetMute(!muted, nil); err != nil {
		return fmt.Errorf("set mute state: %w", err)
	}

	m.logger.Debugw("Toggled mic mute", "nowMuted", !muted)
	return nil
}
