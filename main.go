// Command soundboard is a Windows 11 soundboard that mixes sound clips over
// your live microphone via the VB-CABLE virtual audio cable, so anyone in
// Discord (or any voice app) hears them as if you spoke.
//
// v2 architecture:
//   - UI is a Fyne main window with a clip browser and volume sliders, plus a
//     Fyne system-tray icon; closing the window hides it to the tray.
//   - The malgo duplex engine captures the user's REAL mic and mixes the
//     soundboard into CABLE Input. Software gains (mic, master, per-clip) are
//     applied in the real-time callback.
//   - Auto-route (internal/setup) detects VB-CABLE, offers a one-click install,
//     and makes Discord need ZERO changes by setting the Windows default
//     recording endpoint to "CABLE Output" while SoundBoard runs (restored on
//     quit). The engine still captures the previous default mic, not the cable.
//
// Sounds are NOT embedded: at launch the app reads the sounds/ folder next to
// the executable, so dropping new clips into sounds/<category>/ and relaunching
// needs no rebuild.
package main

import (
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"

	"github.com/gen2brain/malgo"

	"soundboard/internal/audio"
	"soundboard/internal/catalog"
	"soundboard/internal/config"
	"soundboard/internal/devices"
	"soundboard/internal/hotkeys"
	"soundboard/internal/setup"
	"soundboard/internal/ui"
)

func main() {
	// Route diagnostics to a log file under the config dir. The shipping build
	// uses -H=windowsgui, which detaches stderr, so plain log.* output would be
	// invisible. Mirror to stderr too for console/dev builds.
	if logPath, err := config.LogPath(); err == nil {
		if f, ferr := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); ferr == nil {
			defer f.Close()
			log.SetOutput(io.MultiWriter(os.Stderr, f))
		}
	}

	// Settings.
	settings, err := config.Load()
	if err != nil {
		log.Fatalf("load settings: %v", err)
	}

	// Catalog: load clips from the sounds/ folder next to the executable at
	// runtime (plug-and-play — nothing is embedded in the binary). Clips decode
	// lazily on first play, so startup is instant.
	root, base := soundsRoot()
	soundsDir := filepath.Join(base, "sounds")
	lib, err := catalog.New(root)
	if err != nil {
		log.Fatalf("build library from %s: %v", soundsDir, err)
	}
	var clipCount int
	for _, c := range lib.Categories {
		clipCount += len(c.Clips)
	}
	log.Printf("library: %d categories, %d clips indexed from %s (decoded on demand)", len(lib.Categories), clipCount, soundsDir)

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

	// Enumerate devices and detect the VB-CABLE endpoints.
	playback, capture, err := devices.Enumerate(ctx)
	if err != nil {
		log.Fatalf("enumerate devices: %v", err)
	}
	status := setup.Detect(playback, capture)
	if !status.CableInputPresent {
		log.Printf("VB-CABLE not detected. Install from %s", setup.DownloadURL())
	}

	// Resolve the REAL mic to capture and the cable to play into. The engine
	// captures the user's previous default mic (PreviousDefaultMic once routing
	// is engaged), never the cable.
	mic, micOK := resolveMic(capture, settings.MicName)
	cable, cableOK := resolveCable(playback, settings.CableName)
	if !micOK {
		log.Printf("no microphone resolved; using the system default capture device")
	}

	engine := audio.NewEngine(ctx, lib)

	// Seed the engine gains from saved volumes (default to unity).
	applyVolumes(engine, settings)

	// Without the VB-CABLE playback endpoint there is nowhere to route the mix.
	// Run in degraded mode: keep the window alive so the user can install
	// VB-CABLE and relaunch, but do not start the duplex engine.
	if cableOK {
		if err := engine.Configure(mic, cable); err != nil {
			log.Printf("configure engine: %v (running without audio routing)", err)
		} else if err := engine.Start(); err != nil {
			log.Printf("start engine: %v (running without audio routing)", err)
		} else {
			defer func() { _ = engine.Stop() }()
		}
	} else {
		log.Printf("VB-CABLE absent: starting in setup mode (no audio routing). Install VB-CABLE and relaunch.")
	}

	// Hotkeys fire clips at their saved per-clip volume.
	hk := hotkeys.New()
	hk.OnTrigger(func(clipID string) { engine.TriggerGain(clipID, clipGain(settings, clipID)) })
	for combo, clipID := range settings.Hotkeys {
		if lib.Get(clipID) == nil {
			log.Printf("hotkey %q: clip %q not found in library", combo, clipID)
		}
		if err := hk.Register(combo, clipID); err != nil {
			log.Printf("hotkey %q: %v", combo, err)
		}
	}
	hk.Run()
	defer hk.Close()

	// Save settings on exit.
	defer func() {
		if err := settings.Save(); err != nil {
			log.Printf("save settings: %v", err)
		}
	}()

	// Build the controllers the UI talks to, then run the Fyne main loop
	// (blocks until Quit).
	vol := &volController{engine: engine, settings: settings}
	setupCtl := &setupController{status: status}
	app := ui.New(lib, engine, vol, setupCtl)
	app.Run()
}

// applyVolumes seeds the engine's mic/master gains from saved settings,
// defaulting missing (zero) values to unity.
func applyVolumes(engine *audio.Engine, s *config.Settings) {
	engine.SetMicGain(orUnity(s.Volumes.Mic))
	engine.SetMasterGain(orUnity(s.Volumes.Master))
}

// clipGain returns the saved per-clip gain for id, defaulting to unity.
func clipGain(s *config.Settings, id string) float32 {
	if s.Volumes.PerClip != nil {
		if g, ok := s.Volumes.PerClip[id]; ok {
			return g
		}
	}
	return 1
}

// orUnity maps a zero (unset) gain to 1.0.
func orUnity(g float32) float32 {
	if g == 0 {
		return 1
	}
	return g
}

// Compile-time checks that the wiring types satisfy the UI's interfaces, so a
// signature drift fails the build here rather than silently.
var (
	_ ui.Player           = (*audio.Engine)(nil)
	_ ui.VolumeController = (*volController)(nil)
	_ ui.SetupController  = (*setupController)(nil)
)

// volController adapts the engine + settings to ui.VolumeController. Setters
// push the new level to the engine and persist it in settings; getters seed the
// sliders. It satisfies ui.VolumeController.
type volController struct {
	engine   *audio.Engine
	settings *config.Settings
}

func (v *volController) SetMic(g float32) {
	v.engine.SetMicGain(g)
	v.settings.Volumes.Mic = g
}

func (v *volController) SetMaster(g float32) {
	v.engine.SetMasterGain(g)
	v.settings.Volumes.Master = g
}

func (v *volController) SetClip(id string, g float32) {
	if v.settings.Volumes.PerClip == nil {
		v.settings.Volumes.PerClip = map[string]float32{}
	}
	v.settings.Volumes.PerClip[id] = g
}

func (v *volController) Mic() float32           { return orUnity(v.settings.Volumes.Mic) }
func (v *volController) Master() float32        { return orUnity(v.settings.Volumes.Master) }
func (v *volController) Clip(id string) float32 { return clipGain(v.settings, id) }

// setupController adapts internal/setup to ui.SetupController. It satisfies
// ui.SetupController.
type setupController struct {
	status setup.Status
}

func (s *setupController) Status() (bool, string) {
	if s.status.CanEngage {
		return true, "VB-CABLE detected — routing ready"
	}
	if s.status.CableInputPresent {
		return false, "VB-CABLE Input found, but CABLE Output is missing"
	}
	return false, "VB-CABLE NOT detected — click Install / Fix routing"
}

func (s *setupController) Install() error { return setup.InstallCable(nil) }

func (s *setupController) Engage() error {
	_, err := setup.EngageRouting()
	return err
}

// soundsRoot locates the directory that contains the sounds/ folder and returns
// an fs.FS rooted there plus the chosen base path. It prefers the directory of
// the running executable, then the current working directory. If no sounds/
// exists yet, it creates an empty one next to the exe so first run is clean.
func soundsRoot() (fs.FS, string) {
	var bases []string
	if exe, err := os.Executable(); err == nil {
		bases = append(bases, filepath.Dir(exe))
	}
	if wd, err := os.Getwd(); err == nil {
		bases = append(bases, wd)
	}
	for _, b := range bases {
		if st, err := os.Stat(filepath.Join(b, "sounds")); err == nil && st.IsDir() {
			return os.DirFS(b), b
		}
	}
	base := "."
	if len(bases) > 0 {
		base = bases[0]
	}
	_ = os.MkdirAll(filepath.Join(base, "sounds"), 0o755)
	return os.DirFS(base), base
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
