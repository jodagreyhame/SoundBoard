// Package setup owns the one-click VB-CABLE provisioning and the auto-route
// that makes Discord need ZERO configuration.
//
// Flow:
//  1. Detect inspects enumerated devices for the CABLE Input (playback) and
//     CABLE Output (capture) endpoints, and caches the capture list so
//     PreviousDefaultMic can later re-resolve the user's mic by name.
//  2. If the cable is absent, InstallCable downloads VB-CABLE and runs its
//     silent installer elevated (see install.go).
//  3. Once the cable exists, EngageRouting saves the user's current default
//     recording endpoint, then sets the default capture device (console +
//     communications roles) to "CABLE Output (VB-Audio Virtual Cable)" via the
//     undocumented IPolicyConfig interface (see internal/winaudio). The returned
//     restore closure puts the previous default back, so the mic is only
//     hijacked while SoundBoard runs.
//
// The audio engine still captures the user's REAL mic (the previous default,
// surfaced by PreviousDefaultMic) and mixes the soundboard into CABLE Input; the
// cable forwards that to CABLE Output, which is now Windows' default mic, which
// Discord reads — with nothing changed by the user inside Discord.
package setup

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/jodagreyhame/SoundBoard/internal/devices"
	"github.com/jodagreyhame/SoundBoard/internal/winaudio"
)

// primaryDownloadURL is the official VB-CABLE driver pack direct link (current
// release). install.go tries this plus an older fallback at install time.
var primaryDownloadURL = cableDownloadURLs[0]

// state holds the engage/restore data shared between EngageRouting and
// PreviousDefaultMic. It is process-global because there is exactly one routing
// engagement per app run, and main calls these from different points.
var state struct {
	mu sync.Mutex

	prevCaptureID   string           // endpoint ID saved before hijack
	prevCaptureName string           // friendly name of the previous default mic
	prevMic         devices.Device   // resolved malgo device for the previous mic
	prevMicOK       bool             // whether prevMic was resolved
	captureList     []devices.Device // last capture list seen by Detect
	engaged         bool
}

// Status summarizes what auto-route can do right now. CableInputPresent /
// CableOutputPresent mirror the two VB-CABLE endpoints; CanEngage is true when
// both endpoints exist so EngageRouting can proceed without installing.
type Status struct {
	CableInputPresent  bool
	CableOutputPresent bool
	CanEngage          bool
}

// cableAdapterName is the device-interface (adapter) friendly name VB-CABLE's
// driver INF registers for every one of its endpoints. Unlike the endpoint
// friendly name, a user rename never touches it, so it is the cable's IDENTITY.
const cableAdapterName = "VB-Audio Virtual Cable"

// Detect inspects enumerated playback/capture devices for the VB-CABLE
// endpoints BY NAME and reports whether routing can be engaged. It also caches
// the capture list so PreviousDefaultMic can re-resolve the user's mic by name.
// It is pure (no COM), so tests drive it with synthetic device lists; runtime
// callers should prefer DetectSystem, which adds identity-based matching.
func Detect(playback, capture []devices.Device) Status {
	_, in := devices.FindCableInput(playback)
	_, out := devices.FindCableOutput(capture)

	state.mu.Lock()
	state.captureList = capture
	state.mu.Unlock()

	return Status{
		CableInputPresent:  in,
		CableOutputPresent: out,
		CanEngage:          in && out,
	}
}

// DetectSystem is Detect plus IDENTITY matching: any endpoint whose Windows
// device-interface (adapter) property is "VB-Audio Virtual Cable" counts as
// present, whatever it is currently named. Identity can only ADD presence on
// top of the name-based result, never subtract, so a COM failure or a non-
// WASAPI backend (where endpoint IDs don't bridge) degrades exactly to Detect.
// This is what makes detection rename-proof: the first real bug report was a
// machine whose cable input had been renamed "Speakers (VB-Audio Virtual
// Cable)", invisible to every name needle.
func DetectSystem(playback, capture []devices.Device) Status {
	st := Detect(playback, capture)
	if st.CanEngage {
		return st
	}
	if !st.CableInputPresent {
		if _, ok := cableByIdentity(winaudio.ERender, playback); ok {
			st.CableInputPresent = true
		}
	}
	if !st.CableOutputPresent {
		if _, ok := cableByIdentity(winaudio.ECapture, capture); ok {
			st.CableOutputPresent = true
		}
	}
	st.CanEngage = st.CableInputPresent && st.CableOutputPresent
	return st
}

// ResolveCableDevice returns the malgo playback device the engine should play
// into: the name-matched cable endpoint if any, else the endpoint identified by
// the cable's adapter property. ok is false only when neither path finds one.
func ResolveCableDevice(playback []devices.Device) (devices.Device, bool) {
	if d, ok := devices.FindCableInput(playback); ok {
		return d, true
	}
	return cableByIdentity(winaudio.ERender, playback)
}

// cableByIdentity maps the ACTIVE endpoints carrying the VB-CABLE adapter
// property into the given malgo device list by endpoint ID. Best-effort: any
// COM error reads as "not found" so callers fall back to name matching.
func cableByIdentity(flow winaudio.EDataFlow, list []devices.Device) (devices.Device, bool) {
	eps, err := winaudio.EndpointsByAdapter(flow, cableAdapterName)
	if err != nil || len(eps) == 0 {
		return devices.Device{}, false
	}
	ids := make([]string, 0, len(eps))
	for _, ep := range eps {
		ids = append(ids, ep.ID)
	}
	return devices.FindCableByEndpointIDs(list, ids)
}

// DownloadURL returns the official VB-CABLE driver pack download URL.
func DownloadURL() string { return primaryDownloadURL }

// InstallCable downloads VB-CABLE and runs its silent installer ELEVATED
// (VBCABLE_Setup_x64.exe -i -h via ShellExecuteEx "runas"). It blocks until the
// installer process exits or ctx is cancelled. A reboot may still be required
// before the endpoints appear. Returns ErrInstallDeclined if the user dismisses
// the UAC prompt, ErrNoNetwork if the download fails, or a wrapped error
// otherwise. A nil ctx is treated as context.Background().
func InstallCable(ctx context.Context) error {
	return installCable(ctx)
}

// EngageRouting saves the current Windows default recording endpoint, then sets
// the default capture device (console + communications roles) to
// "CABLE Output (VB-Audio Virtual Cable)". It returns a restore closure that
// reinstates the saved default; call it on quit. If the cable is absent or the
// endpoint switch fails, err is non-nil and restore is a safe no-op.
func EngageRouting() (restore func(), err error) {
	noop := func() {}

	// Save the previous default capture endpoint and its friendly name BEFORE
	// changing anything, so we can both restore it and tell the engine which
	// real mic to capture.
	prevID, err := winaudio.DefaultCaptureID()
	if err != nil {
		return noop, fmt.Errorf("setup: read current default mic: %w", err)
	}
	prevName, nameErr := winaudio.DefaultCaptureName() // best-effort

	// Locate the CABLE Output capture endpoint Discord will read from —
	// identity first (adapter property, rename-proof), name as fallback.
	cableID := ""
	if eps, aerr := winaudio.EndpointsByAdapter(winaudio.ECapture, cableAdapterName); aerr == nil && len(eps) > 0 {
		cableID = eps[0].ID
	}
	if cableID == "" {
		var nerr error
		cableID, nerr = winaudio.FindCaptureEndpointID("CABLE Output")
		if nerr != nil {
			return noop, fmt.Errorf("setup: locate CABLE Output endpoint: %w", nerr)
		}
	}
	if cableID == prevID {
		// Already routed (e.g. user set it manually); nothing to hijack and
		// nothing safe to restore to.
		return noop, errors.New("setup: CABLE Output is already the default mic")
	}

	if err := winaudio.SetDefaultCapture(cableID); err != nil {
		return noop, fmt.Errorf("setup: set default mic to CABLE Output: %w", err)
	}

	state.mu.Lock()
	state.prevCaptureID = prevID
	state.prevCaptureName = prevName
	state.engaged = true
	captureList := state.captureList
	state.mu.Unlock()

	// Resolve the previous default mic to a malgo device by friendly name so
	// PreviousDefaultMic can hand it to the engine. Best-effort: a miss just
	// means main falls back to its own default-mic resolution.
	resolvePrevMic(prevName, captureList)
	_ = nameErr

	restore = func() {
		state.mu.Lock()
		id := state.prevCaptureID
		state.engaged = false
		state.mu.Unlock()
		if id == "" {
			return
		}
		// Best-effort restore; if it fails the user can reset the default mic in
		// Windows Sound settings. We deliberately swallow the error because this
		// runs on quit where there is nothing left to surface it to.
		_ = winaudio.SetDefaultCapture(id)
	}
	return restore, nil
}

// resolvePrevMic matches the previous default mic's friendly name against the
// enumerated capture devices and records the result for PreviousDefaultMic.
// malgo friendly names and MMDevice friendly names are the same WASAPI endpoint
// strings, so an exact-then-contains match is reliable.
func resolvePrevMic(name string, capture []devices.Device) {
	state.mu.Lock()
	defer state.mu.Unlock()
	if name == "" {
		state.prevMicOK = false
		return
	}
	d, ok := devices.FindByName(capture, name)
	state.prevMic = d
	state.prevMicOK = ok
}

// PreviousDefaultMic returns the user's real microphone — the default recording
// endpoint captured BEFORE EngageRouting hijacked it to CABLE Output. The audio
// engine captures from this device, never from the cable. ok is false before
// EngageRouting has run (or if the previous default could not be resolved to an
// enumerated device).
func PreviousDefaultMic() (devices.Device, bool) {
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.prevMic, state.prevMicOK
}
