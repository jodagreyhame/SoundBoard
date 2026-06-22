package winaudio

import (
	"errors"
	"fmt"
	"strings"
	"syscall"
	"unsafe"

	ole "github.com/go-ole/go-ole"
)

// iPolicyConfigVtbl mirrors the UNDOCUMENTED IPolicyConfig vtable. After the
// three IUnknown slots come the interface's own methods in this exact order
// (confirmed against the widely-used IPolicyConfig.h definition shipped with
// audioswitch / SoundSwitch / EndPointController):
//
//	0 QueryInterface  1 AddRef  2 Release    (IUnknown)
//	3 GetMixFormat
//	4 GetDeviceFormat
//	5 ResetDeviceFormat
//	6 SetDeviceFormat
//	7 GetProcessingPeriod
//	8 SetProcessingPeriod
//	9 GetShareMode
//	10 SetShareMode
//	11 GetPropertyValue
//	12 SetPropertyValue
//	13 SetDefaultEndpoint   <-- the only method we call
//	14 SetEndpointVisibility
//
// SetDefaultEndpoint(wszDeviceId LPCWSTR, eRole ERole) HRESULT.
type iPolicyConfigVtbl struct {
	ole.IUnknownVtbl
	GetMixFormat          uintptr
	GetDeviceFormat       uintptr
	ResetDeviceFormat     uintptr
	SetDeviceFormat       uintptr
	GetProcessingPeriod   uintptr
	SetProcessingPeriod   uintptr
	GetShareMode          uintptr
	SetShareMode          uintptr
	GetPropertyValue      uintptr
	SetPropertyValue      uintptr
	SetDefaultEndpoint    uintptr
	SetEndpointVisibility uintptr
}

func policyVtbl(u *ole.IUnknown) *iPolicyConfigVtbl {
	return (*iPolicyConfigVtbl)(unsafe.Pointer(u.RawVTable))
}

// setDefaultEndpoint sets the endpoint identified by endpointID as the Windows
// default for BOTH the console and communications roles. Discord follows the
// communications default; system/most apps follow the console default — setting
// both means no leak path remains to the real mic. eMultimedia is intentionally
// left alone (it tracks the console default by policy).
func setDefaultEndpoint(endpointID string) error {
	if strings.TrimSpace(endpointID) == "" {
		return errors.New("winaudio: empty endpoint id")
	}

	s := enterCOM()
	defer s.Close()

	unk, err := ole.CreateInstance(clsidPolicyConfigClient, iidIPolicyConfig)
	if err != nil {
		return fmt.Errorf("winaudio: create PolicyConfigClient: %w", err)
	}
	if unk == nil {
		return errors.New("winaudio: PolicyConfig is nil")
	}
	defer unk.Release()

	idPtr, err := syscall.UTF16PtrFromString(endpointID)
	if err != nil {
		return fmt.Errorf("winaudio: encode endpoint id: %w", err)
	}

	for _, role := range []ERole{EConsole, ECommunications} {
		hr, _, _ := syscall.SyscallN(
			policyVtbl(unk).SetDefaultEndpoint,
			uintptr(unsafe.Pointer(unk)),
			uintptr(unsafe.Pointer(idPtr)),
			uintptr(role),
		)
		if hr != 0 {
			return fmt.Errorf("winaudio: SetDefaultEndpoint(role=%d): %w", role, ole.NewError(hr))
		}
	}
	return nil
}

// SetDefaultCapture makes the recording endpoint identified by endpointID the
// Windows default for BOTH the console and communications roles via
// IPolicyConfig::SetDefaultEndpoint. endpointID must be a value previously
// obtained from DefaultCaptureID or FindCaptureEndpointID.
func SetDefaultCapture(endpointID string) error {
	return setDefaultEndpoint(endpointID)
}

// SetDefaultRender makes the playback endpoint endpointID the Windows default
// for the console and communications roles. Used to undo VB-CABLE installs that
// hijack default playback to "CABLE Input". IPolicyConfig::SetDefaultEndpoint
// keys purely on the endpoint ID, so render and capture share one code path.
func SetDefaultRender(endpointID string) error {
	return setDefaultEndpoint(endpointID)
}
