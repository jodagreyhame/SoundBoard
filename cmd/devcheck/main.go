// Command devcheck is a headless diagnostic that initializes the real malgo
// (miniaudio) WASAPI context and enumerates the system's audio devices, then
// reports whether the VB-CABLE endpoints and a default microphone were found.
//
// It exists to smoke-test that the cgo/malgo audio backend actually links and
// runs on this machine WITHOUT needing the system tray or a GUI. It does not
// open or start any audio device, so it is safe to run on a build server or
// over a remote session; it just lists what the engine would see at startup.
//
//	go run ./cmd/devcheck
//
// Exit code 0 means the context initialized and enumeration succeeded (whether
// or not VB-CABLE is installed); non-zero means the audio backend failed to
// initialize or enumerate, which is a real linkage/runtime problem.
package main

import (
	"fmt"
	"os"

	"github.com/gen2brain/malgo"

	"github.com/jodagreyhame/SoundBoard/internal/devices"
	"github.com/jodagreyhame/SoundBoard/internal/setup"
	"github.com/jodagreyhame/SoundBoard/internal/winaudio"
	"github.com/jodagreyhame/SoundBoard/internal/wizard"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "devcheck: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	// Same backend order the app uses: WASAPI first, DirectSound fallback.
	ctx, err := malgo.InitContext(
		[]malgo.Backend{malgo.BackendWasapi, malgo.BackendDsound},
		malgo.ContextConfig{},
		nil,
	)
	if err != nil {
		return fmt.Errorf("init audio context: %w", err)
	}
	defer func() {
		_ = ctx.Uninit()
		ctx.Free()
	}()

	playback, capture, err := devices.Enumerate(ctx)
	if err != nil {
		return fmt.Errorf("enumerate devices: %w", err)
	}

	fmt.Printf("Playback devices (%d):\n", len(playback))
	for _, d := range playback {
		fmt.Printf("  %s%s\n", d.Name, defaultTag(d.IsDefault))
	}
	fmt.Printf("\nCapture devices (%d):\n", len(capture))
	for _, d := range capture {
		fmt.Printf("  %s%s\n", d.Name, defaultTag(d.IsDefault))
	}

	fmt.Println()
	if mic, ok := devices.DefaultMic(capture); ok {
		fmt.Printf("Default mic        : %s\n", mic.Name)
	} else {
		fmt.Printf("Default mic        : (none found)\n")
	}

	status := wizard.Check(playback, capture)
	fmt.Printf("CABLE Input present (by name) : %t\n", status.CableInputPresent)
	fmt.Printf("CABLE Output present (by name): %t\n", status.CableOutputPresent)

	// Identity path: endpoints carrying the VB-CABLE adapter property, mapped
	// into the malgo lists by endpoint ID — proves the winaudio<->malgo ID
	// bridge on this machine and shows what detection sees after a rename.
	fmt.Println()
	for _, side := range []struct {
		label string
		flow  winaudio.EDataFlow
		list  []devices.Device
	}{
		{"render ", winaudio.ERender, playback},
		{"capture", winaudio.ECapture, capture},
	} {
		eps, err := winaudio.EndpointsByAdapter(side.flow, "VB-Audio Virtual Cable")
		if err != nil {
			fmt.Printf("identity %s: winaudio error: %v\n", side.label, err)
			continue
		}
		ids := make([]string, 0, len(eps))
		for _, ep := range eps {
			fmt.Printf("identity %s: %s  name=%q\n", side.label, ep.ID, ep.Name)
			ids = append(ids, ep.ID)
		}
		if d, ok := devices.FindCableByEndpointIDs(side.list, ids); ok {
			fmt.Printf("identity %s: -> malgo device %q (ID bridge OK)\n", side.label, d.Name)
		} else if len(ids) > 0 {
			fmt.Printf("identity %s: -> NO malgo device matched (ID bridge FAILED)\n", side.label)
		} else {
			fmt.Printf("identity %s: no endpoints with the VB-CABLE adapter property\n", side.label)
		}
	}

	st := setup.DetectSystem(playback, capture)
	fmt.Printf("\nDetectSystem: input=%t output=%t canEngage=%t\n", st.CableInputPresent, st.CableOutputPresent, st.CanEngage)
	if !st.CableInputPresent {
		fmt.Printf("\nVB-CABLE not detected. Install it from %s\n", wizard.DownloadURL())
	}

	return nil
}

func defaultTag(isDefault bool) string {
	if isDefault {
		return "  [default]"
	}
	return ""
}
