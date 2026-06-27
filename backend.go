// backend.go owns the REAL backend the Wails App binds to: the malgo duplex
// engine, the sound catalog, device enumeration, the VB-CABLE setup/routing
// controller, global hotkeys, and the persisted settings. It is the Wails
// counterpart of the bootstrap that the legacy Fyne main.go performs inline:
// the SAME internal/* packages, wired the SAME way (auto-engage routing before
// resolving the mic, capture the previous default mic, mix into CABLE Input,
// monitor on the real speakers, seed gains + processing from saved settings).
//
// Nothing here changes internal/* behavior: it reuses the existing engine,
// catalog, setup, devices, hotkeys, and config APIs exactly as the Fyne build
// does. Only the OWNER of the wiring moves from main() into a Backend the App
// can reach from its bound methods.
package main

import (
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gen2brain/malgo"

	"soundboard/internal/apm"
	"soundboard/internal/audio"
	"soundboard/internal/catalog"
	"soundboard/internal/config"
	"soundboard/internal/devices"
	"soundboard/internal/hotkeys"
	"soundboard/internal/setup"
	"soundboard/internal/winaudio"
)

// Backend bundles the live backend objects the bound App methods drive. It is
// constructed once at process start (newBackend) and torn down once on quit
// (close). The engine's setters are atomic/RT-safe so the App may call them from
// the webview/bound-method goroutine without locking; settings mutation + Save
// is serialized by the App's own mutex (see app.go).
type Backend struct {
	settings *config.Settings
	lib      *catalog.Library
	engine   *audio.Engine
	setup    *routingController
	hotkeys  *hotkeys.Manager

	ctx *malgo.AllocatedContext // malgo audio context; freed in close

	// audioRunning reports whether the duplex engine actually started (cable
	// present + Configure/Start succeeded). When false the app runs in setup mode:
	// the UI renders and routing can be installed, but triggering a clip is a no-op
	// because there is no audio path yet. The events loop only emits gate levels
	// while this is true.
	audioRunning bool

	closeOnce sync.Once
}

// newBackend performs the full backend bootstrap, mirroring the Fyne main(): load
// settings, build the catalog from sounds/, init the audio context, enumerate
// devices, detect + auto-engage VB-CABLE routing, resolve the real mic + cable +
// monitor, configure/start the duplex engine, seed gains + processing, and
// register hotkeys. It NEVER fatals on a degraded path: a missing cable, a
// failed Configure, or a hotkey error leaves the app running in setup mode so the
// window still opens and the user can install/relaunch, exactly like the Fyne
// build's degraded behavior.
func newBackend() *Backend {
	b := &Backend{}

	settings, err := config.Load()
	if err != nil {
		log.Printf("load settings: %v (continuing with defaults)", err)
		settings = &config.Settings{}
	}
	b.settings = settings

	root, base := soundsRootW()
	soundsDir := filepath.Join(base, "sounds")
	lib, err := catalog.New(root)
	if err != nil {
		log.Printf("build library from %s: %v (continuing with empty catalog)", soundsDir, err)
		lib, _ = catalog.New(emptySoundsFS{})
	}
	b.lib = lib
	clipCount := 0
	for _, c := range lib.Categories {
		clipCount += len(c.Clips)
	}
	log.Printf("library: %d categories, %d clips indexed from %s (decoded on demand)", len(lib.Categories), clipCount, soundsDir)

	ctx, err := malgo.InitContext(
		[]malgo.Backend{malgo.BackendWasapi, malgo.BackendDsound},
		malgo.ContextConfig{},
		nil,
	)
	if err != nil {
		log.Printf("init audio context: %v (running without audio routing)", err)
		b.setup = &routingController{}
		b.engine = audio.NewEngine(nil, lib)
		b.hotkeys = hotkeys.New()
		b.hotkeys.Run()
		return b
	}
	b.ctx = ctx

	playback, capture, err := devices.Enumerate(ctx)
	if err != nil {
		log.Printf("enumerate devices: %v (running without audio routing)", err)
		playback, capture = nil, nil
	}
	status := setup.Detect(playback, capture)
	if !status.CableInputPresent {
		log.Printf("VB-CABLE not detected. Install from %s", setup.DownloadURL())
	}
	b.setup = &routingController{status: status}

	// Auto-engage routing at startup when the cable is present so Discord needs
	// ZERO changes immediately. Engaging hijacks the default capture endpoint, so
	// this MUST happen before resolving the mic (capture the PREVIOUS default).
	if status.CanEngage {
		if err := b.setup.Engage(); err != nil {
			// One non-error outcome wears an error coat: CABLE Output is ALREADY the
			// Windows default mic (a prior session engaged it, or the user set it
			// manually). Routing is then effectively live — Discord set to "CABLE
			// Output" already hears the board — but we own no restore closure, so we
			// must NOT revert that default on quit. Reflect this as engaged (without
			// a restore) so the banner/pill tell the truth instead of inviting the
			// user to click "Engage" only to hit the same already-default condition.
			if b.setup.alreadyDefaultRouted() {
				b.setup.markEngagedNoRestore()
				log.Printf("routing already engaged: CABLE Output is already the default mic (left as-is on quit)")
			} else {
				log.Printf("auto-engage routing: %v (you can retry from the window banner)", err)
			}
		} else {
			log.Printf("routing engaged: Windows default mic now points at CABLE Output (restored on quit)")
		}
	}

	engine := audio.NewEngine(ctx, lib)
	b.engine = engine
	applyVolumesW(engine, settings)
	applyProcessingW(engine, settings)
	if settings.Processing.NoiseSuppression && !apm.Available() {
		log.Printf("noise suppression enabled in settings but the WebRTC APM is unavailable (%v); it will be a no-op", apm.LoadError())
	}

	mic, _ := resolveMicW(capture, settings.MicName)
	cable, cableOK := resolveCableW(playback, settings.CableName)

	if cableOK {
		log.Printf("audio endpoints: capturing mic %q -> playing into cable %q", mic.Name, cable.Name)
		if err := engine.Configure(mic, cable); err != nil {
			log.Printf("configure engine: %v (running without audio routing)", err)
		} else if err := engine.Start(); err != nil {
			log.Printf("start engine: %v (running without audio routing)", err)
		} else {
			b.audioRunning = true
			if spk, ok := resolveSpeakersW(playback); ok {
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

	hk := hotkeys.New()
	hk.OnTrigger(func(clipID string) { engine.TriggerGain(clipID, clipGainW(settings, clipID)) })
	for combo, clipID := range settings.Hotkeys {
		if lib.Get(clipID) == nil {
			log.Printf("hotkey %q: clip %q not found in library", combo, clipID)
		}
		if err := hk.Register(combo, clipID); err != nil {
			log.Printf("hotkey %q: %v", combo, err)
		}
	}
	if combo := settings.Processing.PTTHotkey; combo != "" {
		if err := hk.RegisterPTT(combo,
			func() { engine.SetPTTDown(true) },
			func() { engine.SetPTTDown(false) },
		); err != nil {
			log.Printf("PTT hotkey %q: %v", combo, err)
		} else {
			log.Printf("push-to-talk bound to %q (used in \"ptt\" mic mode)", combo)
		}
	}
	hk.Run()
	b.hotkeys = hk

	return b
}

// close runs the real cleanup ONCE: stop hotkeys, stop the engine, restore the
// hijacked default mic, save settings, and free the audio context. Order mirrors
// the Fyne main()'s deferred teardown. closeOnce guarantees it runs exactly once.
func (b *Backend) close() {
	b.closeOnce.Do(func() {
		if b.hotkeys != nil {
			b.hotkeys.Close()
		}
		if b.engine != nil {
			_ = b.engine.Stop()
		}
		if b.setup != nil {
			b.setup.Restore()
		}
		if b.settings != nil {
			if err := b.settings.Save(); err != nil {
				log.Printf("save settings: %v", err)
			}
		}
		if b.ctx != nil {
			_ = b.ctx.Uninit()
			b.ctx.Free()
			b.ctx = nil
		}
	})
}

// initLogging mirrors the Fyne main()'s log setup: route diagnostics to the
// per-user config-dir log file AND mirror to stderr. Returns a closer for the
// file handle, or a no-op if the log file could not be opened.
func initLogging() (closeLog func()) {
	if logPath, err := config.LogPath(); err == nil {
		if f, ferr := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); ferr == nil {
			log.SetOutput(io.MultiWriter(os.Stderr, f))
			return func() { _ = f.Close() }
		}
	}
	return func() {}
}

// ---------------------------------------------------------------------------
// routingController — the Wails twin of the Fyne build's setupController. It
// adapts internal/setup to the App: it tracks whether routing is ENGAGED (not
// just whether the cable is present), captures the restore closure EngageRouting
// returns, and maps that state to the {state,detail,canEngage} the UI binds.
// ---------------------------------------------------------------------------

type routingController struct {
	mu      sync.Mutex
	status  setup.Status
	engaged bool
	restore func() // reverts the default mic; nil until Engage succeeds
}

// snapshot maps the live routing state to the binding contract's RoutingStatus.
// state is "engaged" once the Windows default mic points at CABLE Output;
// "present" when both cable endpoints exist but routing is not engaged; "absent"
// otherwise. canEngage mirrors setup.Status.CanEngage (both endpoints present).
func (r *routingController) snapshot() RoutingStatus {
	r.mu.Lock()
	engaged := r.engaged
	st := r.status
	r.mu.Unlock()

	switch {
	case engaged:
		return RoutingStatus{State: "engaged", Detail: "Discord hears the soundboard — no Discord changes needed.", CanEngage: st.CanEngage}
	case st.CanEngage:
		return RoutingStatus{State: "present", Detail: "VB-CABLE detected — click Engage routing.", CanEngage: true}
	case st.CableInputPresent:
		return RoutingStatus{State: "present", Detail: "VB-CABLE Input found, but CABLE Output is missing.", CanEngage: false}
	default:
		return RoutingStatus{State: "absent", Detail: "VB-CABLE not detected — install it to route audio into Discord.", CanEngage: false}
	}
}

// CanEngage reports whether both cable endpoints are present.
func (r *routingController) CanEngage() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.status.CanEngage
}

// Engaged reports whether routing is currently engaged.
func (r *routingController) Engaged() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.engaged
}

// Install downloads and runs the VB-CABLE silent installer ELEVATED. It blocks
// until the installer exits, so the App calls it from a goroutine.
func (r *routingController) Install() error { return setup.InstallCable(nil) }

// Engage points the Windows default mic at CABLE Output and STORES the restore
// closure so close() can revert it on quit.
func (r *routingController) Engage() error {
	restore, err := setup.EngageRouting()
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.restore = restore
	r.engaged = true
	r.mu.Unlock()
	return nil
}

// Redetect re-enumerates the cable endpoints (e.g. after an install completes)
// and updates the cached status so snapshot() reflects the new reality.
func (r *routingController) Redetect(playback, capture []devices.Device) {
	status := setup.Detect(playback, capture)
	r.mu.Lock()
	r.status = status
	r.mu.Unlock()
}

// alreadyDefaultRouted reports whether the Windows default capture endpoint is
// ALREADY "CABLE Output" — the one non-error outcome of Engage() that actually
// means routing is live (Discord hears the board) even though EngageRouting
// returned an error because there was nothing to hijack. Uses the same public
// winaudio API setup itself uses, comparing the current default-capture id to
// the CABLE Output endpoint id; any lookup failure is reported as "not routed"
// so the caller falls back to the plain not-engaged banner.
func (r *routingController) alreadyDefaultRouted() bool {
	defID, err := winaudio.DefaultCaptureID()
	if err != nil {
		return false
	}
	cableID, err := winaudio.FindCaptureEndpointID("CABLE Output")
	if err != nil {
		return false
	}
	return defID != "" && defID == cableID
}

// markEngagedNoRestore flags routing as engaged WITHOUT a restore closure. Used
// only for the already-default case above: routing is effectively engaged, but
// because we did not change the default ourselves we deliberately leave restore
// nil so quit does not revert a default the user/previous session established.
func (r *routingController) markEngagedNoRestore() {
	r.mu.Lock()
	r.engaged = true
	r.restore = nil
	r.mu.Unlock()
}

// Restore reverts the default-mic hijack if routing was engaged. Safe to call
// when not engaged (no-op).
func (r *routingController) Restore() {
	r.mu.Lock()
	restore := r.restore
	r.restore = nil
	r.engaged = false
	r.mu.Unlock()
	if restore != nil {
		restore()
	}
}

// ---------------------------------------------------------------------------
// Bootstrap helpers — the Wails twins of main.go's resolve/apply functions (the
// fyne build keeps its own copies behind the fyne tag). The trailing "W"
// disambiguates them from the fyne-tagged originals so the two never collide.
// ---------------------------------------------------------------------------

func applyVolumesW(engine *audio.Engine, s *config.Settings) {
	engine.SetMicGain(s.Volumes.MicGain())
	engine.SetMasterGain(s.Volumes.MasterGain())
	engine.SetMonitorGain(s.Volumes.MonitorGain())
}

func applyProcessingW(engine *audio.Engine, s *config.Settings) {
	engine.SetMicMode(s.Processing.MicMode)
	engine.SetGateSensitivity(s.Processing.GateSensitivity)
	engine.SetNoiseSuppression(s.Processing.NoiseSuppression)
	engine.SetAGC(s.Processing.AGC)
	engine.SetDucking(s.Processing.Ducking)
	engine.SetForceThrough(s.Processing.ForceThrough)
	engine.SetMonitorSource(s.Processing.MonitorSource)
}

func clipGainW(s *config.Settings, id string) float32 {
	if s.Volumes.PerClip != nil {
		if g, ok := s.Volumes.PerClip[id]; ok {
			return g
		}
	}
	return 1
}

func soundsRootW() (fs.FS, string) {
	var bases []string
	if exe, err := os.Executable(); err == nil {
		bases = append(bases, filepath.Dir(exe))
	}
	if wd, err := os.Getwd(); err == nil {
		bases = append(bases, wd)
	}
	for _, bs := range bases {
		if st, err := os.Stat(filepath.Join(bs, "sounds")); err == nil && st.IsDir() {
			return os.DirFS(bs), bs
		}
	}
	base := "."
	if len(bases) > 0 {
		base = bases[0]
	}
	_ = os.MkdirAll(filepath.Join(base, "sounds"), 0o755)
	return os.DirFS(base), base
}

func isCableNameW(name string) bool {
	return strings.Contains(strings.ToUpper(name), "CABLE")
}

func resolveMicW(capture []devices.Device, name string) (devices.Device, bool) {
	if name != "" && !isCableNameW(name) {
		if d, ok := devices.FindByName(capture, name); ok {
			return d, true
		}
	}
	if d, ok := setup.PreviousDefaultMic(); ok && !isCableNameW(d.Name) {
		return d, true
	}
	if d, ok := devices.DefaultMic(capture); ok && !isCableNameW(d.Name) {
		return d, true
	}
	for _, d := range capture {
		if !isCableNameW(d.Name) {
			return d, true
		}
	}
	return devices.Device{}, false
}

func resolveCableW(playback []devices.Device, name string) (devices.Device, bool) {
	if name != "" {
		if d, ok := devices.FindByName(playback, name); ok {
			return d, true
		}
	}
	return devices.FindCableInput(playback)
}

func resolveSpeakersW(playback []devices.Device) (devices.Device, bool) {
	var fallback devices.Device
	var haveFallback bool
	for _, d := range playback {
		if isCableNameW(d.Name) {
			continue
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

// emptySoundsFS is a minimal fs.FS exposing only an empty sounds/ directory so
// catalog.New succeeds (returning an empty library) when the real sounds folder
// could not be walked. Used only on the degraded bootstrap path.
type emptySoundsFS struct{}

func (emptySoundsFS) Open(name string) (fs.File, error) {
	if name == "sounds" || name == "." {
		return emptyDir{name: name}, nil
	}
	return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
}

type emptyDir struct{ name string }

func (d emptyDir) Stat() (fs.FileInfo, error)       { return emptyDirInfo{name: d.name}, nil }
func (emptyDir) Read([]byte) (int, error)           { return 0, io.EOF }
func (emptyDir) Close() error                       { return nil }
func (emptyDir) ReadDir(int) ([]fs.DirEntry, error) { return nil, io.EOF }

type emptyDirInfo struct{ name string }

func (i emptyDirInfo) Name() string     { return i.name }
func (emptyDirInfo) Size() int64        { return 0 }
func (emptyDirInfo) Mode() fs.FileMode  { return fs.ModeDir | 0o555 }
func (emptyDirInfo) ModTime() time.Time { return time.Time{} }
func (emptyDirInfo) IsDir() bool        { return true }
func (emptyDirInfo) Sys() any           { return nil }
