// Package devices wraps malgo device enumeration and provides matching
// helpers for the VB-CABLE endpoints and the user's microphone.
//
// Persist devices by Name string (RawID is unstable across reboots/replug);
// re-resolve names to IDs at every startup.
package devices

import (
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

// Enumerate lists all playback and capture devices for the given context.
func Enumerate(ctx *malgo.AllocatedContext) (playback []Device, capture []Device, err error) {
	panic("todo")
}

// FindCableInput returns the VB-CABLE playback endpoint we play into.
// Prefers the exact "CABLE Input (VB-Audio Virtual Cable)" name, else any
// device whose name Contains "CABLE Input".
func FindCableInput(playback []Device) (Device, bool) {
	panic("todo")
}

// FindCableOutput returns the VB-CABLE capture endpoint Discord listens to.
// Used only to warn the user if it is absent.
func FindCableOutput(capture []Device) (Device, bool) {
	panic("todo")
}

// FindByName returns the device with an exact matching Name.
func FindByName(list []Device, name string) (Device, bool) {
	panic("todo")
}

// DefaultMic returns the default capture device (IsDefault first, else the
// first device in the list).
func DefaultMic(capture []Device) (Device, bool) {
	panic("todo")
}
