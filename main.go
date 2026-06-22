// Command soundboard is a Windows 11 system-tray soundboard that mixes
// bundled sound clips over your live microphone via the VB-CABLE virtual audio
// cable, so anyone in Discord (or any voice app) hears them as if you spoke.
//
// The embed of the sounds/ tree MUST live at the repo root, because go:embed
// cannot reference parent directories ("..").
package main

import (
	"embed"
	"io/fs"
	"log"

	"github.com/gen2brain/malgo"

	"soundboard/internal/audio"
	"soundboard/internal/catalog"
	"soundboard/internal/config"
	"soundboard/internal/devices"
	"soundboard/internal/hotkeys"
	"soundboard/internal/tray"
	"soundboard/internal/wizard"
)

//go:embed all:sounds
var soundsFS embed.FS

func main() {
	// Settings.
	settings, err := config.Load()
	if err != nil {
		log.Fatalf("load settings: %v", err)
	}

	// Catalog: walk the embedded sounds/ tree and decode all clips.
	sub, err := fs.Sub(soundsFS, ".")
	if err != nil {
		log.Fatalf("sub fs: %v", err)
	}
	lib, err := catalog.New(sub)
	if err != nil {
		log.Fatalf("build library: %v", err)
	}
	if err := lib.Load(); err != nil {
		log.Fatalf("decode library: %v", err)
	}

	// Audio context (WASAPI first, DirectSound fallback).
	ctx, err := malgo.InitContext(
		[]malgo.Backend{malgo.BackendWasapi, malgo.BackendDsound},
		malgo.ContextConfig{},
		nil,
	)
	if err != nil {
		log.Fatalf("init audio context: %v", err)
	}
	defer func() {
		_ = ctx.Uninit()
		ctx.Free()
	}()

	// Enumerate devices and resolve the mic + cable endpoints.
	playback, capture, err := devices.Enumerate(ctx)
	if err != nil {
		log.Fatalf("enumerate devices: %v", err)
	}

	status := wizard.Check(playback, capture)
	if !status.CableInputPresent {
		log.Printf("VB-CABLE not detected. Download: %s", wizard.DownloadURL())
		log.Print(wizard.DiscordChecklist())
	}

	mic, _ := resolveMic(capture, settings.MicName)
	cable, _ := resolveCable(playback, settings.CableName)

	// Engine.
	engine := audio.NewEngine(ctx, lib)
	if err := engine.Configure(mic, cable); err != nil {
		log.Fatalf("configure engine: %v", err)
	}
	if settings.Monitor && settings.MonitorName != "" {
		if mon, ok := devices.FindByName(playback, settings.MonitorName); ok {
			_ = engine.SetMonitor(&mon)
		}
	}
	if err := engine.Start(); err != nil {
		log.Fatalf("start engine: %v", err)
	}
	defer func() { _ = engine.Stop() }()

	// Hotkeys.
	hk := hotkeys.New()
	hk.OnTrigger(func(clipID string) { engine.Trigger(clipID) })
	for combo, clipID := range settings.Hotkeys {
		if err := hk.Register(combo, clipID); err != nil {
			log.Printf("hotkey %q: %v", combo, err)
		}
	}
	hk.Run()
	defer hk.Close()

	// Tray UI (blocks until quit).
	ui := tray.New(lib, engine)
	ui.OnMonitorToggle(func(on bool) {
		if !on {
			_ = engine.SetMonitor(nil)
			return
		}
		if mon, ok := devices.FindByName(playback, settings.MonitorName); ok {
			_ = engine.SetMonitor(&mon)
		}
	})
	ui.OnQuit(func() {
		_ = engine.Stop()
		hk.Close()
	})
	ui.Run(func() {})
}

func resolveMic(capture []devices.Device, name string) (devices.Device, bool) {
	if name != "" {
		if d, ok := devices.FindByName(capture, name); ok {
			return d, true
		}
	}
	return devices.DefaultMic(capture)
}

func resolveCable(playback []devices.Device, name string) (devices.Device, bool) {
	if name != "" {
		if d, ok := devices.FindByName(playback, name); ok {
			return d, true
		}
	}
	return devices.FindCableInput(playback)
}
