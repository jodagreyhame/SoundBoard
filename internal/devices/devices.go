// Package devices wraps malgo device enumeration and provides matching
// helpers for the VB-CABLE endpoints and the user's microphone.
//
// Persist devices by Name string (RawID is unstable across reboots/replug);
// re-resolve names to IDs at every startup.
package devices

import (
	"strings"

	"github.com/gen2brain/malgo"
)

// Kind distinguishes playback from capture devices.
type Kind int

const (
	Playback Kind = iota
	Capture
)

// Device is a flattened view of a malgo.DeviceInfo. RawID is the raw malgo
// device identifier used to target a specific device in a DeviceConfig.
type Device struct {
	Name      string
	RawID     malgo.DeviceID
	IsDefault bool
}

// Exact / contains match strings for the VB-CABLE endpoints. Matching is
// case-INSENSITIVE, which also makes this agree with the engage-time endpoint
// lookup in internal/winaudio (winaudio.FindCaptureEndpointID lowercases both
// sides); the two disagreeing meant a name could satisfy one and not the other.
const (
	cableInputExact     = "CABLE Input (VB-Audio Virtual Cable)"
	cableInputContains  = "CABLE Input"
	cableOutputExact    = "CABLE Output (VB-Audio Virtual Cable)"
	cableOutputContains = "CABLE Output"

	// cableAdapter is the adapter half of a VB-CABLE endpoint's friendly name.
	// Windows lets a user rename an endpoint (Sound settings -> Properties), which
	// rewrites only the leading half — "<NewName> (VB-Audio Virtual Cable)" — and
	// that rename persists across reboots AND driver reinstalls. A renamed endpoint
	// is invisible to the needles above but still carries this, so it is used as a
	// LAST-RESORT match: without it a rename is indistinguishable from "not
	// installed", which puts the user in a reinstall loop that can never succeed.
	cableAdapter = "VB-Audio Virtual Cable"

	// cableMultiChannel marks the 16-channel playback variant ("CABLE In 16ch"),
	// which shares the adapter name. It is de-prioritised in the last-resort match
	// so a renamed plain CABLE Input wins over it.
	cableMultiChannel = "16ch"
)

// Enumerate lists all playback and capture devices for the given context.
func Enumerate(ctx *malgo.AllocatedContext) (playback []Device, capture []Device, err error) {
	playbackInfos, err := ctx.Devices(malgo.Playback)
	if err != nil {
		return nil, nil, err
	}
	captureInfos, err := ctx.Devices(malgo.Capture)
	if err != nil {
		return nil, nil, err
	}
	return toDevices(playbackInfos), toDevices(captureInfos), nil
}

// toDevices maps malgo DeviceInfo values to our flattened Device type.
func toDevices(infos []malgo.DeviceInfo) []Device {
	out := make([]Device, 0, len(infos))
	for i := range infos {
		info := &infos[i] // Name() has a pointer receiver.
		out = append(out, Device{
			Name:      info.Name(),
			RawID:     info.ID,
			IsDefault: info.IsDefault != 0,
		})
	}
	return out
}

// FindCableInput returns the VB-CABLE playback endpoint we play into.
// Prefers the exact "CABLE Input (VB-Audio Virtual Cable)" name, else any
// device whose name contains "CABLE Input", else a renamed VB-CABLE endpoint.
func FindCableInput(playback []Device) (Device, bool) {
	return findCableEndpoint(playback, cableInputExact, cableInputContains)
}

// FindCableOutput returns the VB-CABLE capture endpoint Discord listens to.
func FindCableOutput(capture []Device) (Device, bool) {
	return findCableEndpoint(capture, cableOutputExact, cableOutputContains)
}

// findCableEndpoint resolves a VB-CABLE endpoint in three passes: exact friendly
// name, then the "CABLE Input"/"CABLE Output" needle, then — only if both miss —
// any device carrying the VB-CABLE adapter name, which is how a user-renamed
// endpoint is still recognised. All comparisons are case-insensitive.
func findCableEndpoint(list []Device, exact, contains string) (Device, bool) {
	if d, ok := findExactThenContains(list, exact, contains); ok {
		return d, true
	}
	return findAdapter(list)
}

// findExactThenContains returns the first device whose Name equals exact; if
// none match, the first whose Name contains the substring. Case-insensitive.
func findExactThenContains(list []Device, exact, contains string) (Device, bool) {
	for _, d := range list {
		if strings.EqualFold(d.Name, exact) {
			return d, true
		}
	}
	needle := strings.ToLower(contains)
	for _, d := range list {
		if strings.Contains(strings.ToLower(d.Name), needle) {
			return d, true
		}
	}
	return Device{}, false
}

// findAdapter returns a device whose name carries the VB-CABLE adapter string,
// preferring the plain endpoint over the 16-channel variant that shares it. It
// runs only after the named matches miss, so on a normal install it never fires.
func findAdapter(list []Device) (Device, bool) {
	adapter := strings.ToLower(cableAdapter)
	var fallback Device
	var haveFallback bool
	for _, d := range list {
		name := strings.ToLower(d.Name)
		if !strings.Contains(name, adapter) {
			continue
		}
		if !strings.Contains(name, cableMultiChannel) {
			return d, true
		}
		if !haveFallback {
			fallback, haveFallback = d, true
		}
	}
	return fallback, haveFallback
}

// FindByName returns the device with an exact matching Name, falling back to
// the first device whose Name contains the given string. An empty name never
// matches (strings.Contains(_, "") is always true, which would otherwise
// silently resolve to the first device in the list).
func FindByName(list []Device, name string) (Device, bool) {
	if name == "" {
		return Device{}, false
	}
	for _, d := range list {
		if d.Name == name {
			return d, true
		}
	}
	for _, d := range list {
		if strings.Contains(d.Name, name) {
			return d, true
		}
	}
	return Device{}, false
}

// DefaultMic returns the default capture device (IsDefault first, else the
// first device in the list). ok is false when the list is empty.
func DefaultMic(capture []Device) (Device, bool) {
	if len(capture) == 0 {
		return Device{}, false
	}
	for _, d := range capture {
		if d.IsDefault {
			return d, true
		}
	}
	return capture[0], true
}
