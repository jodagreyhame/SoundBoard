// Package setup owns the one-click VB-CABLE provisioning and the auto-route
// that makes Discord need ZERO configuration.
//
// Flow:
//  1. Detect inspects enumerated devices for the CABLE Input (playback) and
//     CABLE Output (capture) endpoints.
//  2. If the cable is absent, InstallCable downloads VB-CABLE and runs its
//     silent installer elevated.
//  3. Once the cable exists, EngageRouting saves the user's current default
//     recording endpoint, then sets the default capture device (console +
//     communications roles) to "CABLE Output (VB-Audio Virtual Cable)". The
//     returned restore closure puts the previous default back, so the mic is
//     only hijacked while SoundBoard runs.
//
// The audio engine still captures the user's REAL mic (the previous default,
// surfaced by PreviousDefaultMic) and mixes the soundboard into CABLE Input; the
// cable forwards that to CABLE Output, which is now Windows' default mic, which
// Discord reads — with nothing changed by the user inside Discord.
//
// Bodies here are STUBS for the v2 skeleton; signatures are final.
package setup

import (
	"context"
	"errors"

	"soundboard/internal/devices"
)

// errStub marks a not-yet-implemented setup step.
var errStub = errors.New("setup: not implemented")

// downloadURL is the VB-CABLE distribution page. The actual zip/exe URL is
// resolved at install time (it changes between releases) rather than hardcoded.
const downloadURL = "https://vb-audio.com/Cable/"

// Status summarizes what auto-route can do right now. CableInputPresent /
// CableOutputPresent mirror the two VB-CABLE endpoints; CanEngage is true when
// both endpoints exist so EngageRouting can proceed without installing.
type Status struct {
	CableInputPresent  bool
	CableOutputPresent bool
	CanEngage          bool
}

// Detect inspects enumerated playback/capture devices for the VB-CABLE
// endpoints and reports whether routing can be engaged.
func Detect(playback, capture []devices.Device) Status {
	_, in := devices.FindCableInput(playback)
	_, out := devices.FindCableOutput(capture)
	return Status{
		CableInputPresent:  in,
		CableOutputPresent: out,
		CanEngage:          in && out,
	}
}

// DownloadURL returns the VB-CABLE download page.
func DownloadURL() string { return downloadURL }

// InstallCable downloads VB-CABLE and runs its silent installer ELEVATED
// (VBCABLE_Setup_x64.exe -i -h via ShellExecute "runas"). It blocks until the
// installer process exits or ctx is cancelled. A reboot may still be required
// before the endpoints appear. Returns an error if the download, elevation, or
// install fails.
func InstallCable(ctx context.Context) error {
	_ = ctx
	return errStub
}

// EngageRouting saves the current Windows default recording endpoint, then sets
// the default capture device (console + communications roles) to
// "CABLE Output (VB-Audio Virtual Cable)". It returns a restore closure that
// reinstates the saved default; call it on quit. If the cable is absent or the
// endpoint switch fails, err is non-nil and restore is a safe no-op.
func EngageRouting() (restore func(), err error) {
	return func() {}, errStub
}

// PreviousDefaultMic returns the user's real microphone — the default recording
// endpoint captured BEFORE EngageRouting hijacked it to CABLE Output. The audio
// engine captures from this device, never from the cable. ok is false before
// EngageRouting has run (or if the previous default could not be resolved to an
// enumerated device).
func PreviousDefaultMic() (devices.Device, bool) {
	return devices.Device{}, false
}
