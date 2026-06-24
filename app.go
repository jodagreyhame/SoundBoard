//go:build !fyne

// app.go defines the Wails-bound Go App struct: the single object whose exported
// methods the WebView2 frontend calls as window.go.main.App.<Method>, and the
// source of the live events the frontend subscribes to via runtime.EventsOn.
//
// This is the SKELETON phase: method bodies return zero/sample data and the
// event plumbing is stubbed. The signatures here are the contract everything
// else builds on — phase 2 wires these to the real engine/setup/config without
// changing a single signature. Only JSON-friendly types cross the boundary.
package main

import (
	"context"
	"sync"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"

	"soundboard/internal/config"
)

// App is the Wails application object. ctx is captured in OnStartup (via
// startup) so the bound methods can emit events and drive the window. Phase 2
// adds the real dependencies (engine, setup controller, settings, catalog) as
// fields; the bound-method signatures do not change.
//
// Thread-safety: ctx and the lifecycle bookkeeping are touched from TWO OS
// threads — the Wails main loop (OnStartup/OnShutdown, bound method calls from
// the webview) AND the companion systray goroutine (tray Open/Quit clicks). All
// shared mutable lifecycle state below is guarded by lcMu so the tray and the
// window never race on ctx or the shutdown flag. (lcMu is named distinctly so a
// phase-2 backend mutex on this same struct does not collide.)
type App struct {
	lcMu sync.Mutex
	ctx  context.Context

	// backend holds the live engine/catalog/setup/config the bound methods
	// operate on. main injects it via setBackend right after NewApp and before
	// wails.Run, so it is always present by the time a bound method or the events
	// loop runs. Guarded by lcMu (read through getBackend).
	backend *Backend

	// lastRouting is the most recent RoutingStatus the events loop emitted, so it
	// re-emits only on an actual change (the sidebar pill must not flicker every
	// tick). Guarded by lcMu.
	lastRouting RoutingStatus

	// cleanup is the backend teardown registered via OnCleanup (engine.Stop,
	// restore the default mic, save config — exactly backend.go's Backend.close).
	// The lifecycle calls it through runCleanup, which fires it exactly once no
	// matter which path reaches shutdown (tray Quit, in-window Quit, OS close).
	cleanup        func()
	runCleanupOnce sync.Once

	// quitOnce guards the real-exit path so a tray Quit and an in-window Quit
	// arriving together only trigger one runtime.Quit.
	quitOnce sync.Once

	// eventsOnce guards the live-events goroutine so startup launches it exactly
	// once; stopEvents signals it to exit (closed by runCleanup).
	eventsOnce sync.Once
	stopEvents chan struct{}
}

// NewApp constructs the bound App. The backend is injected by main via
// setBackend before wails.Run starts.
func NewApp() *App {
	return &App{stopEvents: make(chan struct{})}
}

// setBackend injects the live backend and registers its teardown as the App's
// cleanup hook, so the single OnShutdown choke point (runCleanup) performs the
// real engine.Stop / mic restore / config save. Called by main once, before
// wails.Run.
func (a *App) setBackend(b *Backend) {
	a.lcMu.Lock()
	a.backend = b
	a.lcMu.Unlock()
	a.OnCleanup(b.close)
}

// getBackend returns the injected backend under the lock.
func (a *App) getBackend() *Backend {
	a.lcMu.Lock()
	defer a.lcMu.Unlock()
	return a.backend
}

// startup is wired to Wails' OnStartup. It captures the runtime context and
// launches the live-events goroutine (gateLevel + routingStatus). Called on the
// Wails main thread; the lock keeps the systray goroutine from reading a
// half-written ctx.
func (a *App) startup(ctx context.Context) {
	a.lcMu.Lock()
	a.ctx = ctx
	a.lcMu.Unlock()
	a.startEvents()
}

// context returns the captured Wails context, or nil before OnStartup has run.
// Every runtime.* call goes through this so the systray goroutine never reads
// ctx without the lock.
func (a *App) context() context.Context {
	a.lcMu.Lock()
	defer a.lcMu.Unlock()
	return a.ctx
}

// OnCleanup registers the backend teardown to run once on shutdown. Phase 2
// calls this from main with a closure that stops the engine, restores the
// default mic, and saves config — the exact deferred-cleanup sequence the
// legacy Fyne main runs on exit (and which backend.go's Backend.close already
// implements). Registering here (rather than hard-coding it in OnShutdown)
// keeps the lifecycle/tray code free of backend imports.
func (a *App) OnCleanup(fn func()) {
	a.lcMu.Lock()
	a.cleanup = fn
	a.lcMu.Unlock()
}

// runCleanup runs the registered backend teardown exactly once. Wails'
// OnShutdown calls it; it is the single choke point through which every exit
// path (tray Quit, in-window Quit, OS-level close) drains, so the engine stop /
// mic restore / config save can never run twice nor be skipped.
func (a *App) runCleanup() {
	a.runCleanupOnce.Do(func() {
		// Stop the events goroutine first so it never emits onto a torn-down
		// runtime while the backend teardown runs.
		close(a.stopEvents)
		a.lcMu.Lock()
		fn := a.cleanup
		a.lcMu.Unlock()
		if fn != nil {
			fn()
		}
	})
}

// ---------------------------------------------------------------------------
// JSON DTOs — the snapshot shape returned by GetState and the live event
// payloads. Field names are lower-camelCase JSON so the frontend reads them as
// plain object keys (state.routing.canEngage, etc.).
// ---------------------------------------------------------------------------

// State is the full snapshot the frontend renders from on boot.
type State struct {
	Theme      string             `json:"theme"`
	Routing    RoutingStatus      `json:"routing"`
	Categories []Category         `json:"categories"`
	Clips      []Clip             `json:"clips"`
	Favorites  []string           `json:"favorites"`
	Volumes    Volumes            `json:"volumes"`
	PerClip    map[string]float64 `json:"perClip"`
	Audio      AudioState         `json:"audio"`
}

// RoutingStatus mirrors SetupController: state is one of "absent" | "present" |
// "engaged"; detail is a human-readable banner line; canEngage reports whether
// the cable is present so routing can be engaged without installing.
type RoutingStatus struct {
	State     string `json:"state"`
	Detail    string `json:"detail"`
	CanEngage bool   `json:"canEngage"`
}

// Category is a sidebar/section entry: the category key and its clip count.
type Category struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// Clip is one playable tile. ID is "<category>/<basename>"; Name is the
// prettified display label; Favorite mirrors the favourites list.
type Clip struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Category string `json:"category"`
	Favorite bool   `json:"favorite"`
}

// Volumes are the three independent mixer levels as linear gains (1.0 = unity,
// 0..1.5 range in the UI).
type Volumes struct {
	Mic     float64 `json:"mic"`
	Master  float64 `json:"master"`
	Monitor float64 `json:"monitor"`
}

// AudioState is the mic-processing suite snapshot.
type AudioState struct {
	MicMode          string  `json:"micMode"`
	GateSensitivity  float64 `json:"gateSensitivity"`
	NoiseSuppression bool    `json:"noiseSuppression"`
	AGC              bool    `json:"agc"`
	Ducking          bool    `json:"ducking"`
	ForceThrough     bool    `json:"forceThrough"`
}

// ---------------------------------------------------------------------------
// State / read
// ---------------------------------------------------------------------------

// GetState assembles the full UI snapshot from the live backend: categories +
// clips from the catalog, favourites + volumes + per-clip + processing settings
// from config, the current routing status from the setup controller, and the
// persisted theme (defaulting to dark when unset). Each clip's Favorite flag and
// the favourites list reflect the saved order.
func (a *App) GetState() State {
	b := a.getBackend()
	if b == nil {
		// No backend (e.g. a unit test constructed a bare App): return a minimal,
		// well-formed snapshot so the frontend still renders without panicking.
		return State{
			Theme:      "dark",
			Routing:    RoutingStatus{State: "absent", Detail: "Backend not initialized.", CanEngage: false},
			Categories: []Category{},
			Clips:      []Clip{},
			Favorites:  []string{},
			Volumes:    Volumes{Mic: 1, Master: 1, Monitor: 1},
			PerClip:    map[string]float64{},
			Audio:      AudioState{MicMode: config.MicModeVAD, GateSensitivity: 0.15},
		}
	}

	a.lcMu.Lock()
	s := b.settings

	// Fast favourite-lookup set so each clip's Favorite flag is O(1).
	favSet := make(map[string]bool, len(s.Favorites))
	for _, id := range s.Favorites {
		favSet[id] = true
	}
	favorites := append([]string{}, s.Favorites...)

	categories := make([]Category, 0, len(b.lib.Categories))
	clips := []Clip{}
	for _, cat := range b.lib.Categories {
		categories = append(categories, Category{Name: cat.Name, Count: len(cat.Clips)})
		for _, c := range cat.Clips {
			clips = append(clips, Clip{
				ID:       c.ID,
				Name:     c.Name,
				Category: c.Category,
				Favorite: favSet[c.ID],
			})
		}
	}

	perClip := make(map[string]float64, len(s.Volumes.PerClip))
	for id, g := range s.Volumes.PerClip {
		perClip[id] = float64(g)
	}

	vols := Volumes{
		Mic:     float64(s.Volumes.MicGain()),
		Master:  float64(s.Volumes.MasterGain()),
		Monitor: float64(s.Volumes.MonitorGain()),
	}
	audioState := AudioState{
		MicMode:          s.Processing.MicMode,
		GateSensitivity:  float64(s.Processing.GateSensitivity),
		NoiseSuppression: s.Processing.NoiseSuppression,
		AGC:              s.Processing.AGC,
		Ducking:          s.Processing.Ducking,
		ForceThrough:     s.Processing.ForceThrough,
	}
	theme := s.Theme
	if theme == "" {
		theme = "dark"
	}
	a.lcMu.Unlock()

	return State{
		Theme:      theme,
		Routing:    b.setup.snapshot(),
		Categories: categories,
		Clips:      clips,
		Favorites:  favorites,
		Volumes:    vols,
		PerClip:    perClip,
		Audio:      audioState,
	}
}

// ---------------------------------------------------------------------------
// Soundboard
// ---------------------------------------------------------------------------

// Play fires the clip at its saved per-clip gain via the engine. The engine's
// trigger handoff is lock-free and non-blocking; a missing clip or a not-yet-
// running engine is a safe no-op.
func (a *App) Play(clipID string) {
	b := a.getBackend()
	if b == nil || b.engine == nil {
		return
	}
	a.lcMu.Lock()
	gain := clipGainW(b.settings, clipID)
	a.lcMu.Unlock()
	b.engine.TriggerGain(clipID, gain)
}

// StopAll immediately silences every playing clip on both the cable and monitor
// paths. The live mic passthrough is unaffected.
func (a *App) StopAll() {
	if b := a.getBackend(); b != nil && b.engine != nil {
		b.engine.StopAll()
	}
}

// ToggleFavorite flips the favourite state of clipID, persists it, and returns
// the new state. Mirrors the Fyne favController: a newly favourited clip is
// appended (preserving order); an unfavourited one is spliced out.
func (a *App) ToggleFavorite(clipID string) bool {
	b := a.getBackend()
	if b == nil {
		return false
	}
	a.lcMu.Lock()
	s := b.settings
	for i, fid := range s.Favorites {
		if fid == clipID {
			s.Favorites = append(s.Favorites[:i], s.Favorites[i+1:]...)
			a.lcMu.Unlock()
			a.persist()
			return false
		}
	}
	s.Favorites = append(s.Favorites, clipID)
	a.lcMu.Unlock()
	a.persist()
	return true
}

// ---------------------------------------------------------------------------
// Volumes
// ---------------------------------------------------------------------------

// SetVolume sets a top-level mixer level (kind "mic" | "master" | "monitor") on
// the engine (atomic, RT-safe) AND records it in settings so it persists. An
// EXPLICIT pointer is stored — including a deliberate 0 mute — so it round-trips
// instead of being coerced back to unity on the next launch.
func (a *App) SetVolume(kind string, value float64) {
	b := a.getBackend()
	if b == nil {
		return
	}
	g := float32(value)
	a.lcMu.Lock()
	switch kind {
	case "mic":
		if b.engine != nil {
			b.engine.SetMicGain(g)
		}
		b.settings.Volumes.Mic = config.FloatPtr(g)
	case "master":
		if b.engine != nil {
			b.engine.SetMasterGain(g)
		}
		b.settings.Volumes.Master = config.FloatPtr(g)
	case "monitor":
		if b.engine != nil {
			b.engine.SetMonitorGain(g)
		}
		b.settings.Volumes.Monitor = config.FloatPtr(g)
	}
	a.lcMu.Unlock()
	a.persist()
}

// SetClipVolume sets the per-clip multiplier for clipID and persists it. The
// value takes effect on the NEXT trigger (clips already in flight keep the gain
// captured when they were triggered), matching the engine's trigger-time gain
// capture and the Fyne volController.SetClip behavior.
func (a *App) SetClipVolume(clipID string, value float64) {
	b := a.getBackend()
	if b == nil {
		return
	}
	a.lcMu.Lock()
	if b.settings.Volumes.PerClip == nil {
		b.settings.Volumes.PerClip = map[string]float32{}
	}
	b.settings.Volumes.PerClip[clipID] = float32(value)
	a.lcMu.Unlock()
	a.persist()
}

// ---------------------------------------------------------------------------
// Audio (mic-processing suite)
// ---------------------------------------------------------------------------

// Each audio setter pushes the new value into the engine immediately (atomic/
// RT-safe) AND records it in settings.Processing so it persists, mirroring the
// Fyne audioController exactly. The local backend handle is named bk so the
// contract's bool parameter name b is preserved verbatim.

// SetMicMode selects the gate mode: "vad" | "ptt" | "always" | "mute".
func (a *App) SetMicMode(mode string) {
	bk := a.getBackend()
	if bk == nil {
		return
	}
	a.lcMu.Lock()
	if bk.engine != nil {
		bk.engine.SetMicMode(mode)
	}
	bk.settings.Processing.MicMode = mode
	a.lcMu.Unlock()
	a.persist()
}

// SetGateSensitivity sets the VAD/RMS gate threshold in [0,1].
func (a *App) SetGateSensitivity(v float64) {
	bk := a.getBackend()
	if bk == nil {
		return
	}
	t := float32(v)
	a.lcMu.Lock()
	if bk.engine != nil {
		bk.engine.SetGateSensitivity(t)
	}
	bk.settings.Processing.GateSensitivity = t
	a.lcMu.Unlock()
	a.persist()
}

// SetNoiseSuppression toggles RNNoise on the mic path.
func (a *App) SetNoiseSuppression(b bool) {
	bk := a.getBackend()
	if bk == nil {
		return
	}
	a.lcMu.Lock()
	if bk.engine != nil {
		bk.engine.SetNoiseSuppression(b)
	}
	bk.settings.Processing.NoiseSuppression = b
	a.lcMu.Unlock()
	a.persist()
}

// SetAGC toggles the RMS-target automatic gain leveler on the mic path.
func (a *App) SetAGC(b bool) {
	bk := a.getBackend()
	if bk == nil {
		return
	}
	a.lcMu.Lock()
	if bk.engine != nil {
		bk.engine.SetAGC(b)
	}
	bk.settings.Processing.AGC = b
	a.lcMu.Unlock()
	a.persist()
}

// SetDucking toggles soundboard ducking under an open mic gate.
func (a *App) SetDucking(b bool) {
	bk := a.getBackend()
	if bk == nil {
		return
	}
	a.lcMu.Lock()
	if bk.engine != nil {
		bk.engine.SetDucking(b)
	}
	bk.settings.Processing.Ducking = b
	a.lcMu.Unlock()
	a.persist()
}

// SetForceThrough toggles the continuous voiced carrier on the cable path.
func (a *App) SetForceThrough(b bool) {
	bk := a.getBackend()
	if bk == nil {
		return
	}
	a.lcMu.Lock()
	if bk.engine != nil {
		bk.engine.SetForceThrough(b)
	}
	bk.settings.Processing.ForceThrough = b
	a.lcMu.Unlock()
	a.persist()
}

// ---------------------------------------------------------------------------
// Setup / routing
// ---------------------------------------------------------------------------

// InstallRouting installs OR engages VB-CABLE routing as appropriate, async. If
// the cable is already present it ENGAGES routing (points Windows' default mic at
// CABLE Output); otherwise it runs the elevated VB-CABLE installer (which blocks
// and may require a reboot). It runs on a goroutine and emits "installProgress"
// updates plus a "routingStatus" change so the frontend dialog/pill update live.
func (a *App) InstallRouting() {
	b := a.getBackend()
	if b == nil {
		a.emitInstallProgress("Backend not initialized.", true, "backend unavailable")
		return
	}
	go func() {
		if b.setup.CanEngage() && !b.setup.Engaged() {
			a.emitInstallProgress("Engaging routing — pointing Discord at the soundboard…", false, "")
			if err := b.setup.Engage(); err != nil {
				a.emitInstallProgress("Could not engage routing.", true, err.Error())
				a.emitRoutingStatus(b.setup.snapshot())
				return
			}
			a.emitInstallProgress("Routing engaged — Discord now hears the soundboard.", true, "")
			a.emitRoutingStatus(b.setup.snapshot())
			return
		}

		// Cable absent (or output missing): run the elevated installer. It blocks
		// until the installer process exits; a reboot may still be needed before
		// the endpoints appear, which the final message surfaces.
		a.emitInstallProgress("Downloading and installing VB-CABLE (approve the Windows prompt)…", false, "")
		if err := b.setup.Install(); err != nil {
			a.emitInstallProgress("VB-CABLE install did not complete.", true, err.Error())
			a.emitRoutingStatus(b.setup.snapshot())
			return
		}
		a.emitInstallProgress("VB-CABLE installed. A reboot may be required, then relaunch SoundBoard to engage routing.", true, "")
		a.emitRoutingStatus(b.setup.snapshot())
	}()
}

// GetRoutingStatus returns the current routing status without recomputing the
// whole snapshot.
func (a *App) GetRoutingStatus() RoutingStatus {
	if b := a.getBackend(); b != nil {
		return b.setup.snapshot()
	}
	return RoutingStatus{State: "absent", Detail: "Backend not initialized.", CanEngage: false}
}

// ---------------------------------------------------------------------------
// App / window
// ---------------------------------------------------------------------------

// SetTheme persists the chosen theme ("dark" | "light"). The frontend applies
// the class itself; this records the choice so it reloads on the next launch.
func (a *App) SetTheme(t string) {
	b := a.getBackend()
	if b == nil {
		return
	}
	a.lcMu.Lock()
	b.settings.Theme = t
	a.lcMu.Unlock()
	a.persist()
}

// Minimize minimizes the window via the Wails runtime.
func (a *App) Minimize() {
	if ctx := a.context(); ctx != nil {
		runtime.WindowMinimise(ctx)
	}
}

// HideToTray hides the window; the systray "Open SoundBoard" item restores it.
// Closing the window also routes here via OnBeforeClose in main.
func (a *App) HideToTray() {
	if ctx := a.context(); ctx != nil {
		runtime.WindowHide(ctx)
	}
}

// Quit really exits the app. It is the single user-facing exit entry point —
// reached from the in-window Quit button AND from the systray "Quit" item — so
// it is guarded to fire once. It asks the Wails runtime to quit, which unwinds
// the window and fires OnShutdown, where runCleanup() performs the backend
// teardown (engine stop, restore default mic, save config) and stopTray() ends
// the tray loop.
//
// Edge case: if Quit is reached before OnStartup captured ctx (e.g. the user
// clicks the tray Quit during the brief launch window), there is no runtime to
// ask, so we fall back to running cleanup directly and tearing the tray down so
// the process can exit instead of hanging with a live tray and no window.
func (a *App) Quit() {
	a.quitOnce.Do(func() {
		if ctx := a.context(); ctx != nil {
			runtime.Quit(ctx)
			return
		}
		a.runCleanup()
		stopTray()
	})
}

// persist writes the current settings to disk. It takes lcMu so a concurrent
// settings mutation (another bound method) cannot race the marshal. Called after
// every setter that changed settings; a failure is surfaced to the diagnostics
// log via the Wails logger.
func (a *App) persist() {
	b := a.getBackend()
	if b == nil || b.settings == nil {
		return
	}
	a.lcMu.Lock()
	err := b.settings.Save()
	ctx := a.ctx
	a.lcMu.Unlock()
	if err != nil && ctx != nil {
		runtime.LogErrorf(ctx, "save settings: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Live events (Go -> JS)
// ---------------------------------------------------------------------------

// startEvents launches the single live-events goroutine exactly once. It emits
// "gateLevel" (the mic-open meter level) at ~20 Hz while the engine is running,
// and "routingStatus" whenever the routing state changes (so the sidebar pill
// updates without the frontend polling). The goroutine exits when runCleanup
// closes stopEvents.
func (a *App) startEvents() {
	a.eventsOnce.Do(func() {
		go a.eventsLoop()
	})
}

// eventsLoop is the body of the live-events goroutine. A 50 ms ticker (~20 Hz)
// drives the gate-level meter; routing changes are detected on the same tick and
// emitted only on an actual transition. Both reads are cheap (a single atomic
// load for the gate level, a mutexed snapshot for routing), so polling on one
// timer keeps the wiring simple and avoids a second goroutine. It returns when
// runCleanup closes stopEvents.
func (a *App) eventsLoop() {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	// Seed lastRouting and push the initial status once so the frontend reflects
	// auto-engage-at-startup without waiting for a transition.
	if b := a.getBackend(); b != nil {
		rs := b.setup.snapshot()
		a.lcMu.Lock()
		a.lastRouting = rs
		a.lcMu.Unlock()
		a.emitRoutingStatus(rs)
	}

	for {
		select {
		case <-a.stopEvents:
			return
		case <-ticker.C:
			b := a.getBackend()
			if b == nil {
				continue
			}
			// gateLevel is only meaningful while the duplex engine is running; emit
			// 0 otherwise so a live meter rests at the floor.
			var level float64
			if b.audioRunning && b.engine != nil {
				level = float64(b.engine.GateLevel())
			}
			a.emitGateLevel(level)

			// routingStatus: emit only on a real change so the pill never flickers.
			rs := b.setup.snapshot()
			a.lcMu.Lock()
			changed := rs != a.lastRouting
			if changed {
				a.lastRouting = rs
			}
			a.lcMu.Unlock()
			if changed {
				a.emitRoutingStatus(rs)
			}
		}
	}
}

// emitGateLevel pushes the live mic-open level [0..1].
func (a *App) emitGateLevel(level float64) {
	if ctx := a.context(); ctx != nil {
		runtime.EventsEmit(ctx, "gateLevel", map[string]any{"level": level})
	}
}

// emitRoutingStatus pushes a routing-status change so the banner/pill update live.
func (a *App) emitRoutingStatus(s RoutingStatus) {
	if ctx := a.context(); ctx != nil {
		runtime.EventsEmit(ctx, "routingStatus", s)
	}
}

// emitInstallProgress pushes an install/engage progress update.
func (a *App) emitInstallProgress(msg string, done bool, errMsg string) {
	if ctx := a.context(); ctx != nil {
		runtime.EventsEmit(ctx, "installProgress", map[string]any{
			"msg": msg, "done": done, "err": errMsg,
		})
	}
}
