// Package winaudio controls the Windows default audio endpoints via the COM
// MMDevice API plus the undocumented IPolicyConfig interface.
//
// SoundBoard's auto-route feature makes Discord need ZERO configuration: on
// engage it sets the Windows DEFAULT RECORDING (capture) endpoint to
// "CABLE Output (VB-Audio Virtual Cable)" for both the console and
// communications roles, after saving the user's previous default so it can be
// restored on quit. Setting the default capture endpoint is the one operation
// the public MMDevice API does NOT expose, so we drive the undocumented
// IPolicyConfig::SetDefaultEndpoint vtable directly (see policyconfig.go).
//
// COM approach: go-ole (github.com/go-ole/go-ole) handles CoInitializeEx,
// CoCreateInstance, IUnknown/Release and GUID parsing — all well-trodden. The
// MMDevice and IPolicyConfig interfaces have no typed wrapper in go-ole, so the
// few methods we need are invoked through syscall on their vtable slots. The
// vtable layouts here mirror the well-tested github.com/moutend/go-wca
// definitions and the public mmdeviceapi.h header.
//
// Every exported function CoInitializes on the calling goroutine and pins it to
// its OS thread for the duration of the COM work (apartment-threaded COM
// requires all calls on one thread). This package is Windows-only in effect.
package winaudio

import (
	"errors"
	"fmt"
	"runtime"
	"strings"
	"syscall"
	"unsafe"

	ole "github.com/go-ole/go-ole"
)

// Well-known COM identifiers for the MMDevice API and IPolicyConfig.
//
//   - CLSID_MMDeviceEnumerator / IID_IMMDeviceEnumerator are the public,
//     documented MMDevice enumeration interfaces (mmdeviceapi.h).
//   - CLSID_PolicyConfigClient / IID_IPolicyConfig are the UNDOCUMENTED audio
//     policy interface used by SndVol and AudioEndpointBuilder. These exact
//     GUIDs are stable across Vista..Windows 11 and are how every third-party
//     "set default device" tool (NirCmd, SoundVolumeView, SoundSwitch,
//     EarTrumpet's older builds) flips the default endpoint.
var (
	clsidMMDeviceEnumerator = ole.NewGUID("{BCDE0395-E52F-467C-8E3D-C4579291692E}")
	iidIMMDeviceEnumerator  = ole.NewGUID("{A95664D2-9614-4F35-A746-DE8DB63617E6}")

	clsidPolicyConfigClient = ole.NewGUID("{870AF99C-171D-4F9E-AF0D-E63DF40C2BC9}")
	iidIPolicyConfig        = ole.NewGUID("{F8679F50-850A-41CF-9C72-430F290290C8}")
)

// EDataFlow / ERole mirror the MMDevice enumerations. The auto-route sets both
// roles so neither "default device" nor "default communications device" leaks
// the real mic to Discord.
type EDataFlow int32

const (
	ERender  EDataFlow = 0 // playback endpoints
	ECapture EDataFlow = 1 // recording endpoints
	EAll     EDataFlow = 2
)

// ERole selects which "default" slot to read or assign.
type ERole int32

const (
	EConsole        ERole = 0 // games, system sounds, most apps
	EMultimedia     ERole = 1
	ECommunications ERole = 2 // VoIP — Discord uses this one
)

// deviceStateActive is DEVICE_STATE_ACTIVE; we only enumerate plugged-in,
// enabled endpoints so a disabled "CABLE Output" never matches.
const deviceStateActive uint32 = 0x00000001

// COM HRESULTs from CoInitializeEx that we special-case. S_OK and S_FALSE are
// BOTH successes that add a refcount the caller must balance with
// CoUninitialize; only RPC_E_CHANGED_MODE means "already initialized in another
// apartment, no refcount added" so we must NOT CoUninitialize for it.
const (
	sFalse          uintptr = 0x00000001
	rpcEChangedMode uintptr = 0x80010106
)

// stgmRead is STGM_READ for IMMDevice::OpenPropertyStore (read-only access).
const stgmRead uint32 = 0x00000000

// pkeyDeviceFriendlyName is PKEY_Device_FriendlyName
// {a45c254e-df1c-4efd-8020-67d146a850e0}, PID 14 — the human-readable endpoint
// name such as "CABLE Output (VB-Audio Virtual Cable)".
var pkeyDeviceFriendlyName = propertyKey{
	fmtid: ole.GUID{
		Data1: 0xa45c254e, Data2: 0xdf1c, Data3: 0x4efd,
		Data4: [8]byte{0x80, 0x20, 0x67, 0xd1, 0x46, 0xa8, 0x50, 0xe0},
	},
	pid: 14,
}

// vtLPWSTR is VARTYPE VT_LPWSTR (0x001F); PKEY_Device_FriendlyName comes back as
// an LPWSTR-typed PROPVARIANT.
const vtLPWSTR uint16 = 0x001F

// propertyKey mirrors the Win32 PROPERTYKEY struct (a GUID + a uint32 PID).
type propertyKey struct {
	fmtid ole.GUID
	pid   uint32
}

// propVariant mirrors the Win32 PROPVARIANT layout on amd64. For our single
// property (a friendly name) the union holds an LPWSTR pointer; we only read
// vt and that pointer, so the trailing union padding is opaque. Typing the
// union field as *uint16 (rather than uintptr) keeps the GC aware of the
// pointer and avoids a uintptr->unsafe.Pointer round-trip that vet flags.
type propVariant struct {
	vt        uint16
	reserved1 uint16
	reserved2 uint16
	reserved3 uint16
	lpwstr    *uint16 // union; for VT_LPWSTR this is the wchar_t* pointer
	_         uintptr // remaining union padding
}

// imm* / iPolicyConfigVtbl mirror the COM vtable layouts. Each interface begins
// with IUnknown's three slots (QueryInterface, AddRef, Release) followed by its
// own methods in declaration order — matching go-wca and mmdeviceapi.h.
type immDeviceEnumeratorVtbl struct {
	ole.IUnknownVtbl
	EnumAudioEndpoints                     uintptr
	GetDefaultAudioEndpoint                uintptr
	GetDevice                              uintptr
	RegisterEndpointNotificationCallback   uintptr
	UnregisterEndpointNotificationCallback uintptr
}

type immDeviceCollectionVtbl struct {
	ole.IUnknownVtbl
	GetCount uintptr
	Item     uintptr
}

type immDeviceVtbl struct {
	ole.IUnknownVtbl
	Activate          uintptr
	OpenPropertyStore uintptr
	GetID             uintptr
	GetState          uintptr
}

type propertyStoreVtbl struct {
	ole.IUnknownVtbl
	GetCount uintptr
	GetAt    uintptr
	GetValue uintptr
	SetValue uintptr
	Commit   uintptr
}

// vtbl reinterprets a COM object pointer's first field (its vtable pointer) as
// the typed vtable so we can read method slots.
func enumVtbl(u *ole.IUnknown) *immDeviceEnumeratorVtbl {
	return (*immDeviceEnumeratorVtbl)(unsafe.Pointer(u.RawVTable))
}
func collVtbl(u *ole.IUnknown) *immDeviceCollectionVtbl {
	return (*immDeviceCollectionVtbl)(unsafe.Pointer(u.RawVTable))
}
func devVtbl(u *ole.IUnknown) *immDeviceVtbl {
	return (*immDeviceVtbl)(unsafe.Pointer(u.RawVTable))
}
func storeVtbl(u *ole.IUnknown) *propertyStoreVtbl {
	return (*propertyStoreVtbl)(unsafe.Pointer(u.RawVTable))
}

// comSession owns COM initialization on the current (locked) goroutine. Callers
// defer Close to balance CoUninitialize and unlock the OS thread.
type comSession struct {
	uninit bool
}

// enterCOM pins the goroutine to its OS thread and initializes COM in the
// apartment-threaded model. CoInitializeEx returns one of:
//
//   - S_OK (err == nil): we initialized COM and OWN a refcount.
//   - S_FALSE (0x1): COM was already initialized in the SAME apartment on this
//     thread; this STILL adds a refcount we must balance with CoUninitialize.
//   - RPC_E_CHANGED_MODE (0x80010106): COM was already initialized in a
//     DIFFERENT apartment; no refcount was added, so we must NOT CoUninitialize.
//
// go-ole reports any non-zero HRESULT (including the S_FALSE success) as a
// non-nil *OleError, so we cannot use `err == nil` alone — that would skip the
// CoUninitialize for the S_FALSE path and leak a COM init refcount on every
// call (Fyne/malgo also init COM and Go reuses LockOSThread threads, so S_FALSE
// is hit routinely). We inspect the HRESULT and balance unless it is exactly
// RPC_E_CHANGED_MODE.
func enterCOM() *comSession {
	runtime.LockOSThread()
	// COINIT_APARTMENTTHREADED (0x2) — the model the MMDevice API documents.
	err := ole.CoInitializeEx(0, ole.COINIT_APARTMENTTHREADED)
	return &comSession{uninit: ownsCOMInit(err)}
}

// ownsCOMInit reports whether the CoInitializeEx result means THIS call added a
// COM init refcount that Close must balance with CoUninitialize. It is true for
// S_OK (err==nil) and S_FALSE (already-initialized in the same apartment — still
// refcounted), and false ONLY for RPC_E_CHANGED_MODE (different apartment, no
// refcount added). Unknown non-nil results default to true so we err toward
// balancing rather than leaking. Pure, so the refcount classification is unit-
// testable without a live COM apartment.
func ownsCOMInit(err error) bool {
	if err == nil {
		return true // S_OK
	}
	var oerr *ole.OleError
	if errors.As(err, &oerr) && oerr.Code() == rpcEChangedMode {
		return false // RPC_E_CHANGED_MODE: no refcount added
	}
	return true // S_FALSE and any other success-wrapped HRESULT: we own an init
}

func (s *comSession) Close() {
	if s.uninit {
		ole.CoUninitialize()
	}
	runtime.UnlockOSThread()
}

// createEnumerator instantiates an IMMDeviceEnumerator.
func createEnumerator() (*ole.IUnknown, error) {
	unk, err := ole.CreateInstance(clsidMMDeviceEnumerator, iidIMMDeviceEnumerator)
	if err != nil {
		return nil, fmt.Errorf("winaudio: create MMDeviceEnumerator: %w", err)
	}
	if unk == nil {
		return nil, errors.New("winaudio: MMDeviceEnumerator is nil")
	}
	return unk, nil
}

// getDefaultEndpoint returns the IMMDevice of the current default device for the
// given flow + role. The caller owns the returned pointer and must Release it.
// The COM session must already be entered.
func getDefaultEndpoint(enum *ole.IUnknown, flow EDataFlow, role ERole) (*ole.IUnknown, error) {
	var dev *ole.IUnknown
	hr, _, _ := syscall.SyscallN(
		enumVtbl(enum).GetDefaultAudioEndpoint,
		uintptr(unsafe.Pointer(enum)),
		uintptr(flow),
		uintptr(role),
		uintptr(unsafe.Pointer(&dev)),
	)
	if hr != 0 {
		return nil, fmt.Errorf("winaudio: GetDefaultAudioEndpoint: %w", ole.NewError(hr))
	}
	if dev == nil {
		return nil, errors.New("winaudio: no default endpoint")
	}
	return dev, nil
}

// getDefaultEndpointID returns the endpoint ID string of the current default
// device for the given flow + role.
func getDefaultEndpointID(flow EDataFlow, role ERole) (string, error) {
	s := enterCOM()
	defer s.Close()

	enum, err := createEnumerator()
	if err != nil {
		return "", err
	}
	defer enum.Release()

	dev, err := getDefaultEndpoint(enum, flow, role)
	if err != nil {
		return "", err
	}
	defer dev.Release()

	return deviceID(dev)
}

// deviceID calls IMMDevice::GetId and copies the returned LPWSTR into a Go
// string, then frees the COM-allocated buffer.
func deviceID(dev *ole.IUnknown) (string, error) {
	var idPtr *uint16
	hr, _, _ := syscall.SyscallN(
		devVtbl(dev).GetID,
		uintptr(unsafe.Pointer(dev)),
		uintptr(unsafe.Pointer(&idPtr)),
	)
	if hr != 0 {
		return "", fmt.Errorf("winaudio: IMMDevice.GetId: %w", ole.NewError(hr))
	}
	if idPtr == nil {
		return "", errors.New("winaudio: empty endpoint id")
	}
	id := utf16PtrToString(idPtr)
	ole.CoTaskMemFree(uintptr(unsafe.Pointer(idPtr)))
	return id, nil
}

// deviceFriendlyName opens the device's property store and reads
// PKEY_Device_FriendlyName.
func deviceFriendlyName(dev *ole.IUnknown) (string, error) {
	var store *ole.IUnknown
	hr, _, _ := syscall.SyscallN(
		devVtbl(dev).OpenPropertyStore,
		uintptr(unsafe.Pointer(dev)),
		uintptr(stgmRead),
		uintptr(unsafe.Pointer(&store)),
	)
	if hr != 0 {
		return "", fmt.Errorf("winaudio: OpenPropertyStore: %w", ole.NewError(hr))
	}
	if store == nil {
		return "", errors.New("winaudio: nil property store")
	}
	defer store.Release()

	var pv propVariant
	hr, _, _ = syscall.SyscallN(
		storeVtbl(store).GetValue,
		uintptr(unsafe.Pointer(store)),
		uintptr(unsafe.Pointer(&pkeyDeviceFriendlyName)),
		uintptr(unsafe.Pointer(&pv)),
	)
	if hr != 0 {
		return "", fmt.Errorf("winaudio: GetValue(FriendlyName): %w", ole.NewError(hr))
	}
	if pv.vt != vtLPWSTR || pv.lpwstr == nil {
		// Nothing was allocated into the union for a non-LPWSTR / null result, so
		// there is nothing to free; just skip this endpoint.
		return "", nil
	}
	// For a VT_LPWSTR PROPVARIANT the property store CoTaskMemAlloc'd the string
	// buffer and handed us ownership. Copy it to a Go string, then free the
	// COM allocation (mirroring deviceID's GetId handling) so enumerating every
	// active endpoint on each engage/restore/detect does not leak a wide string
	// per device.
	name := utf16PtrToString(pv.lpwstr)
	ole.CoTaskMemFree(uintptr(unsafe.Pointer(pv.lpwstr)))
	return name, nil
}

// findEndpointID enumerates ACTIVE endpoints of the given flow and returns the
// ID of the first whose friendly name contains nameSubstr (case-insensitive).
func findEndpointID(flow EDataFlow, nameSubstr string) (string, error) {
	if strings.TrimSpace(nameSubstr) == "" {
		return "", errors.New("winaudio: empty name substring")
	}
	want := strings.ToLower(nameSubstr)

	s := enterCOM()
	defer s.Close()

	enum, err := createEnumerator()
	if err != nil {
		return "", err
	}
	defer enum.Release()

	var coll *ole.IUnknown
	hr, _, _ := syscall.SyscallN(
		enumVtbl(enum).EnumAudioEndpoints,
		uintptr(unsafe.Pointer(enum)),
		uintptr(flow),
		uintptr(deviceStateActive),
		uintptr(unsafe.Pointer(&coll)),
	)
	if hr != 0 {
		return "", fmt.Errorf("winaudio: EnumAudioEndpoints: %w", ole.NewError(hr))
	}
	if coll == nil {
		return "", errors.New("winaudio: nil endpoint collection")
	}
	defer coll.Release()

	var count uint32
	hr, _, _ = syscall.SyscallN(
		collVtbl(coll).GetCount,
		uintptr(unsafe.Pointer(coll)),
		uintptr(unsafe.Pointer(&count)),
	)
	if hr != 0 {
		return "", fmt.Errorf("winaudio: collection GetCount: %w", ole.NewError(hr))
	}

	for i := uint32(0); i < count; i++ {
		var dev *ole.IUnknown
		hr, _, _ = syscall.SyscallN(
			collVtbl(coll).Item,
			uintptr(unsafe.Pointer(coll)),
			uintptr(i),
			uintptr(unsafe.Pointer(&dev)),
		)
		if hr != 0 || dev == nil {
			continue
		}
		name, nerr := deviceFriendlyName(dev)
		if nerr == nil && name != "" && strings.Contains(strings.ToLower(name), want) {
			id, ierr := deviceID(dev)
			dev.Release()
			if ierr != nil {
				return "", ierr
			}
			return id, nil
		}
		dev.Release()
	}
	return "", fmt.Errorf("winaudio: no active endpoint matching %q", nameSubstr)
}

// utf16PtrToString reads a NUL-terminated UTF-16 string from a raw pointer.
func utf16PtrToString(p *uint16) string {
	if p == nil {
		return ""
	}
	var out []uint16
	for ptr := unsafe.Pointer(p); ; ptr = unsafe.Add(ptr, 2) {
		ch := *(*uint16)(ptr)
		if ch == 0 {
			break
		}
		out = append(out, ch)
	}
	return syscall.UTF16ToString(out)
}

// DefaultCaptureID returns the Windows endpoint ID string (the stable
// "{0.0.1.00000000}.{guid}" device path) of the current default recording
// device for the console role, so the caller can save and later restore it.
func DefaultCaptureID() (string, error) {
	return getDefaultEndpointID(ECapture, EConsole)
}

// DefaultRenderID returns the endpoint ID of the current default playback
// device for the console role. Provided for symmetry / diagnostics.
func DefaultRenderID() (string, error) {
	return getDefaultEndpointID(ERender, EConsole)
}

// DefaultCaptureName returns the friendly name of the current default recording
// device for the console role (e.g. "Microphone (Realtek Audio)"). The setup
// package uses this to re-resolve the user's previous default mic to a malgo
// device by name after the cable hijacks the default endpoint, since endpoint
// IDs and malgo device IDs are not interchangeable.
func DefaultCaptureName() (string, error) {
	s := enterCOM()
	defer s.Close()

	enum, err := createEnumerator()
	if err != nil {
		return "", err
	}
	defer enum.Release()

	dev, err := getDefaultEndpoint(enum, ECapture, EConsole)
	if err != nil {
		return "", err
	}
	defer dev.Release()

	return deviceFriendlyName(dev)
}

// FindRenderEndpointID returns the endpoint ID of the first ACTIVE playback
// device whose friendly name contains nameSubstr (e.g. "CABLE Input"). An empty
// nameSubstr or no match yields an error.
func FindRenderEndpointID(nameSubstr string) (string, error) {
	return findEndpointID(ERender, nameSubstr)
}

// FindCaptureEndpointID returns the endpoint ID of the first ACTIVE recording
// device whose friendly name contains nameSubstr (e.g. "CABLE Output").
func FindCaptureEndpointID(nameSubstr string) (string, error) {
	return findEndpointID(ECapture, nameSubstr)
}
