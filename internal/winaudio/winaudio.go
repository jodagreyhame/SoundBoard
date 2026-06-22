// Package winaudio controls the Windows default audio endpoints via the COM
// MMDevice API plus the undocumented IPolicyConfig interface.
//
// SoundBoard's auto-route feature makes Discord need ZERO configuration: on
// engage it sets the Windows DEFAULT RECORDING (capture) endpoint to
// "CABLE Output (VB-Audio Virtual Cable)" for both the console and
// communications roles, after saving the user's previous default so it can be
// restored on quit. Setting the default capture endpoint is the one operation
// the public MMDevice API does NOT expose, so we drive the undocumented
// IPolicyConfig::SetDefaultEndpoint vtable directly.
//
// COM approach: go-ole (github.com/go-ole/go-ole) handles CoInitializeEx,
// CoCreateInstance, IUnknown/Release and GUID parsing — all well-trodden. The
// undocumented IPolicyConfig has no typed wrapper anywhere, so its single method
// we need is invoked by computing the vtable slot and calling it through
// syscall.SyscallN on the raw interface pointer go-ole hands back.
//
// Every exported function is Windows-only in effect; the bodies here are STUBS
// (the v2 skeleton wires the signatures). They return errStub until implemented.
package winaudio

import (
	"errors"

	ole "github.com/go-ole/go-ole"
)

// errStub marks a not-yet-implemented endpoint-control call.
var errStub = errors.New("winaudio: not implemented")

// Well-known COM identifiers for the MMDevice API and IPolicyConfig.
//
//   - CLSID_MMDeviceEnumerator / IID_IMMDeviceEnumerator are the public,
//     documented MMDevice enumeration interfaces (mmdeviceapi.h).
//   - CLSID_PolicyConfigClient / IID_IPolicyConfig are the UNDOCUMENTED audio
//     policy interface used by SndVol and AudioEndpointBuilder. These exact
//     GUIDs are stable across Vista..Windows 11 and are how every third-party
//     "set default device" tool (NirCmd, SoundVolumeView, EarTrumpet's older
//     builds) flips the default endpoint.
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

// DefaultCaptureID returns the Windows endpoint ID string (the stable
// "{0.0.1.00000000}.{guid}" device path) of the current default recording
// device for the console role, so the caller can save and later restore it.
func DefaultCaptureID() (string, error) {
	return "", errStub
}

// DefaultRenderID returns the endpoint ID of the current default playback
// device for the console role. Provided for symmetry / diagnostics.
func DefaultRenderID() (string, error) {
	return "", errStub
}

// SetDefaultCapture makes the recording endpoint identified by endpointID the
// Windows default for BOTH the console and communications roles via
// IPolicyConfig::SetDefaultEndpoint. endpointID must be a value previously
// obtained from DefaultCaptureID or FindCaptureEndpointID.
func SetDefaultCapture(endpointID string) error {
	return errStub
}

// SetDefaultRender makes the playback endpoint endpointID the Windows default
// for the console and communications roles. Used to undo VB-CABLE installs that
// hijack default playback to "CABLE Input".
func SetDefaultRender(endpointID string) error {
	return errStub
}

// FindRenderEndpointID returns the endpoint ID of the first ACTIVE playback
// device whose friendly name contains nameSubstr (e.g. "CABLE Input"). An empty
// nameSubstr or no match yields an error.
func FindRenderEndpointID(nameSubstr string) (string, error) {
	return "", errStub
}

// FindCaptureEndpointID returns the endpoint ID of the first ACTIVE recording
// device whose friendly name contains nameSubstr (e.g. "CABLE Output").
func FindCaptureEndpointID(nameSubstr string) (string, error) {
	return "", errStub
}
