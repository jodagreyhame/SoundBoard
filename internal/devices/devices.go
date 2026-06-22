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

// Exact / contains match strings for the VB-CABLE endpoints.
const (
	cableInputExact     = "CABLE Input (VB-Audio Virtual Cable)"
	cableInputContains  = "CABLE Input"
	cableOutputExact    = "CABLE Output (VB-Audio Virtual Cable)"
	cableOutputContains = "CABLE Output"
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
// device whose name Contains "CABLE Input".
func FindCableInput(playback []Device) (Device, bool) {
	return findExactThenContains(playback, cableInputExact, cableInputContains)
}

// FindCableOutput returns the VB-CABLE capture endpoint Discord listens to.
// Used only to warn the user if it is absent.
func FindCableOutput(capture []Device) (Device, bool) {
	return findExactThenContains(capture, cableOutputExact, cableOutputContains)
}

// findExactThenContains returns the first device whose Name equals exact; if
// none match, the first whose Name contains the substring.
func findExactThenContains(list []Device, exact, contains string) (Device, bool) {
	for _, d := range list {
		if d.Name == exact {
			return d, true
		}
	}
	for _, d := range list {
		if strings.Contains(d.Name, contains) {
			return d, true
		}
	}
	return Device{}, false
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
