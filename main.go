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
	"strings"
	"sync"

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

	// The setup controller owns the engage/restore state the UI banner reads and
	// the action button drives. Build it up front so we can auto-engage routing
	// before resolving the mic (engaging hijacks the default capture endpoint, so
	// the engine must capture the PREVIOUS default, not the cable).
	setupCtl := &setupController{status: status}

	// Auto-engage routing at startup when the cable is present, so Discord needs
	// ZERO changes immediately rather than waiting for a manual button press.
	// EngageRouting saves and later restores the user's real default mic; we defer
	// that restore so the system-wide default capture endpoint is always put back
	// on quit (even on the degraded/early-exit paths below).
	defer setupCtl.Restore()
	if status.CanEngage {
		if err := setupCtl.Engage(); err != nil {
			log.Printf("auto-engage routing: %v (you can retry from the window banner)", err)
		} else {
			log.Printf("routing engaged: Windows default mic now points at CABLE Output (restored on quit)")
		}
	}

	// Resolve the REAL mic to capture and the cable to play into. Once routing is
	// engaged the Windows default capture endpoint IS the cable, so prefer the
	// previous default mic that EngageRouting saved; never capture the cable.
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
		// Log the resolved endpoints right before Configure so a wrong mic (e.g.
		// the cable accidentally picked as the capture device) is immediately
		// visible in the diagnostics log instead of manifesting as a silent echo.
		log.Printf("audio endpoints: capturing mic %q -> playing into cable %q", mic.Name, cable.Name)
		if err := engine.Configure(mic, cable); err != nil {
			log.Printf("configure engine: %v (running without audio routing)", err)
		} else if err := engine.Start(); err != nil {
			log.Printf("start engine: %v (running without audio routing)", err)
		} else {
			defer func() { _ = engine.Stop() }()
			// Local monitor: also play triggered clips to the user's real
			// speakers/headphones so they actually HEAR the soundboard. The duplex
			// path only sends the mix to the cable (-> Discord); without a monitor
			// the user hears nothing. Pick the default output device, never the
			// virtual cable (monitoring into the cable would be silent + loop).
			// applyVolumes above already seeded the monitor gain (default unity), so
			// the moment the monitor device is enabled the user hears clips at the
			// saved "what you hear" level — independent of the master "what others
			// hear" level on the duplex path.
			if spk, ok := resolveSpeakers(playback); ok {
				if err := engine.SetMonitor(&spk); err != nil {
					log.Printf("enable local monitor: %v", err)
				} else {
					log.Printf("monitor: clips also play on %q", spk.Name)
				}
			} else {
				log.Printf("no local output device found; you will not hear your own clips")
			}
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

	// Build the remaining controller the UI talks to, then run the Fyne main loop
	// (blocks until Quit). setupCtl was built earlier so routing could auto-engage
	// before mic resolution. The window store restores/persists the last window
	// size via the deferred settings Save above.
	vol := &volController{engine: engine, settings: settings}
	app := ui.New(lib, engine, vol, setupCtl).
		WithWindowStore(&winController{settings: settings}).
		WithFavorites(&favController{settings: settings})
	app.Run()
}

// applyVolumes seeds the engine's mic/master/monitor gains from saved settings,
// defaulting missing (zero) values to unity. The monitor seed matters most: at
// unity the user HEARS clips on their local monitor by default.
func applyVolumes(engine *audio.Engine, s *config.Settings) {
	engine.SetMicGain(orUnity(s.Volumes.Mic))
	engine.SetMasterGain(orUnity(s.Volumes.Master))
	engine.SetMonitorGain(orUnity(s.Volumes.Monitor))
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
	_ ui.Player              = (*audio.Engine)(nil)
	_ ui.VolumeController    = (*volController)(nil)
	_ ui.SetupController     = (*setupController)(nil)
	_ ui.WindowStore         = (*winController)(nil)
	_ ui.FavoritesController = (*favController)(nil)
)

// favController adapts config.Settings.Favorites to ui.FavoritesController. A
// toggle mutates the in-memory Favorites slice (added at the end when newly
// favourited, removed otherwise); main's deferred settings.Save() persists it on
// exit, matching how volumes/window size are saved. It satisfies
// ui.FavoritesController.
type favController struct {
	settings *config.Settings
}

func (f *favController) IsFavorite(id string) bool {
	for _, fid := range f.settings.Favorites {
		if fid == id {
			return true
		}
	}
	return false
}

func (f *favController) ToggleFavorite(id string) {
	for i, fid := range f.settings.Favorites {
		if fid == id {
			// Remove: splice it out, preserving the order of the rest.
			f.settings.Favorites = append(f.settings.Favorites[:i], f.settings.Favorites[i+1:]...)
			return
		}
	}
	f.settings.Favorites = append(f.settings.Favorites, id)
}

// Favorites returns the favourited clip IDs in their pinned display order. It
// returns the live slice; the UI only reads it.
func (f *favController) Favorites() []string { return f.settings.Favorites }

// winController adapts config.WindowPrefs to ui.WindowStore: the UI reads the
// saved size on build and writes the latest size back here, which is persisted
// by main's deferred settings.Save(). It satisfies ui.WindowStore.
type winController struct {
	settings *config.Settings
}

func (w *winController) WindowSize() (float32, float32, bool) {
	p := w.settings.Window
	return p.Width, p.Height, p.Width > 0 && p.Height > 0
}

func (w *winController) SetWindowSize(width, height float32) {
	w.settings.Window.Width = width
	w.settings.Window.Height = height
}

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

// SetMonitor sets the local monitor level — the soundboard volume the USER hears
// in their own headset — independent of SetMaster (what Discord hears). It pushes
// the new level to the engine's monitor path and persists it.
func (v *volController) SetMonitor(g float32) {
	v.engine.SetMonitorGain(g)
	v.settings.Volumes.Monitor = g
}

func (v *volController) SetClip(id string, g float32) {
	if v.settings.Volumes.PerClip == nil {
		v.settings.Volumes.PerClip = map[string]float32{}
	}
	v.settings.Volumes.PerClip[id] = g
}

func (v *volController) Mic() float32           { return orUnity(v.settings.Volumes.Mic) }
func (v *volController) Master() float32        { return orUnity(v.settings.Volumes.Master) }
func (v *volController) Monitor() float32       { return orUnity(v.settings.Volumes.Monitor) }
func (v *volController) Clip(id string) float32 { return clipGain(v.settings, id) }

// setupController adapts internal/setup to ui.SetupController. It tracks not just
// whether the cable is PRESENT (status.CanEngage) but whether routing has been
// ENGAGED, and captures the restore closure returned by EngageRouting so the
// user's default mic can be put back on quit. It satisfies ui.SetupController.
type setupController struct {
	mu      sync.Mutex
	status  setup.Status
	engaged bool
	restore func() // reverts the default mic; nil until Engage succeeds
}

// Status reports ready only when routing is actually ENGAGED (the Windows
// default mic is pointed at CABLE Output), not merely when the cable exists.
// Reporting ready on cable-present alone would make the banner claim "routing
// active" while Discord still hears the real mic.
func (s *setupController) Status() (bool, string) {
	s.mu.Lock()
	engaged := s.engaged
	s.mu.Unlock()

	if engaged {
		return true, "Discord hears the soundboard — no Discord changes needed"
	}
	if s.status.CanEngage {
		return false, "VB-CABLE detected — click Engage routing"
	}
	if s.status.CableInputPresent {
		return false, "VB-CABLE Input found, but CABLE Output is missing"
	}
	return false, "VB-CABLE NOT detected — click Install / Fix routing"
}

// CanEngage reports whether both cable endpoints are present so routing can be
// engaged without installing.
func (s *setupController) CanEngage() bool { return s.status.CanEngage }

func (s *setupController) Install() error { return setup.InstallCable(nil) }

// Engage points the Windows default mic at CABLE Output and STORES the restore
// closure so main can revert it on quit. Without capturing restore, the user's
// system-wide default microphone would stay on CABLE Output forever after the
// app exits.
func (s *setupController) Engage() error {
	restore, err := setup.EngageRouting()
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.restore = restore
	s.engaged = true
	s.mu.Unlock()
	return nil
}

// Restore reverts the default-mic hijack if routing was engaged. Safe to call
// when not engaged (no-op). main defers this so the real mic is always put back.
func (s *setupController) Restore() {
	s.mu.Lock()
	restore := s.restore
	s.restore = nil
	s.engaged = false
	s.mu.Unlock()
	if restore != nil {
		restore()
	}
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

// isCableName reports whether a device name belongs to the VB-CABLE virtual
// audio device (CABLE Input / CABLE Output). Shared by resolveMic and
// resolveSpeakers so neither ever picks the cable as a real endpoint.
func isCableName(name string) bool {
	return strings.Contains(strings.ToUpper(name), "CABLE")
}

func resolveMic(capture []devices.Device, name string) (devices.Device, bool) {
	// An explicit saved mic name always wins — but NEVER resolve to the cable: once
	// routing is engaged the default/ saved capture endpoint may be CABLE Output,
	// and capturing it would feed the cable back into itself (an echo/loop with no
	// real voice). A cable name here is treated as "unset" and we fall through.
	if name != "" && !isCableName(name) {
		if d, ok := devices.FindByName(capture, name); ok {
			return d, true
		}
	}
	// If routing was engaged, the live Windows default capture endpoint is now
	// the cable — capturing it would feed the cable back into itself. Prefer the
	// real mic that EngageRouting saved BEFORE the hijack.
	if d, ok := setup.PreviousDefaultMic(); ok && !isCableName(d.Name) {
		return d, true
	}
	// Fall back to the system default mic, but skip it if it is the cable (after
	// routing engages the default capture IS the cable). In that case pick the
	// first non-cable capture device so we never hand the engine the cable as its
	// microphone. Mirrors resolveSpeakers' non-cable fallback.
	if d, ok := devices.DefaultMic(capture); ok && !isCableName(d.Name) {
		return d, true
	}
	for _, d := range capture {
		if !isCableName(d.Name) {
			return d, true
		}
	}
	return devices.Device{}, false
}

func resolveCable(playback []devices.Device, name string) (devices.Device, bool) {
	if name != "" {
		if d, ok := devices.FindByName(playback, name); ok {
			return d, true
		}
	}
	return devices.FindCableInput(playback)
}

// resolveSpeakers picks the real output device to monitor clips on — the user's
// actual speakers/headphones — explicitly EXCLUDING the virtual cable (playing
// the monitor into the cable would be silent to the user and just loop back to
// Discord). It prefers the default playback device; if that is the cable (the
// VB-CABLE installer sometimes makes CABLE Input the default output), it falls
// back to the first non-cable playback device.
func resolveSpeakers(playback []devices.Device) (devices.Device, bool) {
	var fallback devices.Device
	var haveFallback bool
	for _, d := range playback {
		if isCableName(d.Name) {
			continue // never monitor into the virtual cable
		}
		if !haveFallback {
			fallback, haveFallback = d, true
		}
		if d.IsDefault {
			return d, true
		}
	}
	return fallback, haveFallback
}
