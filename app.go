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
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"

	"github.com/jodagreyhame/SoundBoard/internal/config"
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

	// lastPlaying is the most recent now-playing clip set the events loop emitted,
	// used the same way as lastRouting: re-emit only on an actual change so the
	// frontend is not re-rendering the chip row 20 times a second. A nil value
	// means "never emitted", which is distinct from an emitted empty set. Guarded
	// by lcMu.
	lastPlaying []string

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

	// Debounced persistence. A slider drag fires dozens of SetVolume/
	// SetGateSensitivity calls per second; each pushes to the engine immediately
	// (RT-safe) but only MARKS settings dirty and arms a trailing timer instead of
	// performing a full atomic config.Save() on the UI thread. The timer coalesces
	// a whole drag into a SINGLE disk write on the trailing edge, mirroring the
	// Fyne build's "applies instantly · saved on exit" model while still persisting
	// promptly after the user stops moving the control. All three fields are
	// guarded by lcMu. persistTimer is created/reset under the lock; persistDirty
	// records that settings changed since the last successful flush. (The
	// definitive save on quit still happens via Backend.close, and runCleanup
	// flushes any pending write first so nothing is lost on a fast exit.)
	persistTimer *time.Timer
	persistDirty bool
}

// persistDebounce is the trailing-edge delay before a settings mutation is
// written to disk. Long enough that a continuous slider drag (dozens of input
// events/sec) collapses to one write, short enough that the change is durable
// almost immediately after the user lets go.
const persistDebounce = 400 * time.Millisecond

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

		// Drain any pending debounced settings write BEFORE the backend tears down:
		// stop the trailing timer so it can't fire mid-teardown, then flush so the
		// latest in-memory settings hit disk. (Backend.close saves again, but a
		// debounce window could otherwise leave the most recent slider value unsaved
		// if a fast quit lands between the last 'input' and the timer.)
		a.lcMu.Lock()
		if a.persistTimer != nil {
			a.persistTimer.Stop()
		}
		a.lcMu.Unlock()
		a.flushPersist()

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
	Version    string             `json:"version"`
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

// AudioState is the mic-processing suite snapshot — the Discord Voice & Video
// parity control set. The legacy NoiseSuppression bool is retained for back-compat;
// NoiseSuppressionTier is the authoritative noise-suppression control.
type AudioState struct {
	MicMode          string  `json:"micMode"`
	GateSensitivity  float64 `json:"gateSensitivity"`
	NoiseSuppression bool    `json:"noiseSuppression"`
	AGC              bool    `json:"agc"`
	Ducking          bool    `json:"ducking"`
	ForceThrough     bool    `json:"forceThrough"`
	// MonitorSource is what the local monitor (the user's headset) plays: "clips"
	// (default — clean clips only) or "transmitted" (the confidence monitor — the
	// exact mix sent to the cable, so the user can audit what Discord hears).
	MonitorSource string `json:"monitorSource"`

	// --- Discord Voice & Video parity fields ---
	// NoiseSuppressionTier: "none" | "standard" | "high" | "strong".
	NoiseSuppressionTier string `json:"noiseSuppressionTier"`
	// EchoCancellation toggle (parity; documented as inert without a render ref).
	EchoCancellation bool `json:"echoCancellation"`
	// AdvancedVoiceActivity: the real (RNNoise) VAD gate — the breathing fix.
	AdvancedVoiceActivity bool `json:"advancedVoiceActivity"`
	// AutoSensitivity: "Automatically determine input sensitivity".
	AutoSensitivity bool `json:"autoSensitivity"`
	// AttenuationAmount: ducking depth [0,1] (Discord's attenuation amount).
	AttenuationAmount float64 `json:"attenuationAmount"`
	// AudioSubsystem: "standard" | "legacy" | "experimental" (cosmetic).
	AudioSubsystem string `json:"audioSubsystem"`
	// PTTHotkey is the push-to-talk key combo bound in "ptt" mic mode (e.g.
	// "ctrl+a"); empty means unbound. Surfaced so the UI can DISPLAY the live
	// binding and re-bind it via SetPTTHotkey instead of misrepresenting a fixed key.
	PTTHotkey string `json:"pttHotkey"`
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
			Version:    appVersion,
			Theme:      "dark",
			Routing:    RoutingStatus{State: "absent", Detail: "Backend not initialized.", CanEngage: false},
			Categories: []Category{},
			Clips:      []Clip{},
			Favorites:  []string{},
			Volumes:    Volumes{Mic: 1, Master: 1, Monitor: 1},
			PerClip:    map[string]float64{},
			Audio: AudioState{
				MicMode:               config.MicModeVAD,
				GateSensitivity:       0.15,
				MonitorSource:         config.MonitorSourceClips,
				NoiseSuppressionTier:  config.NSModeHigh,
				AdvancedVoiceActivity: true,
				AutoSensitivity:       true,
				AttenuationAmount:     0.5,
				AudioSubsystem:        config.AudioSubsystemStandard,
			},
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

	// Read the library through the engine, which is its single owner, so a
	// reload or clip-folder change repaints the grid instead of leaving it
	// showing clips the engine can no longer play.
	lib := b.currentLibrary()
	categories := make([]Category, 0, len(lib.Categories))
	clips := []Clip{}
	for _, cat := range lib.Categories {
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
		MicMode:               s.Processing.MicMode,
		GateSensitivity:       float64(s.Processing.GateSensitivity),
		NoiseSuppression:      s.Processing.NoiseSuppression,
		AGC:                   s.Processing.AGC,
		Ducking:               s.Processing.Ducking,
		ForceThrough:          s.Processing.ForceThrough,
		MonitorSource:         s.Processing.MonitorSource,
		NoiseSuppressionTier:  s.Processing.NoiseSuppressionTier,
		EchoCancellation:      s.Processing.EchoCancellation,
		AdvancedVoiceActivity: s.Processing.AdvancedVAD(),
		AutoSensitivity:       s.Processing.AutoSens(),
		AttenuationAmount:     float64(s.Processing.AttenuationAmount),
		AudioSubsystem:        s.Processing.AudioSubsystem,
		PTTHotkey:             s.Processing.PTTHotkey,
	}
	theme := s.Theme
	if theme == "" {
		theme = "dark"
	}
	a.lcMu.Unlock()

	return State{
		Version:    appVersion,
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

// StopClip silences one clip on both paths — the action behind a NOW PLAYING
// chip's ✕. Without it the ✕ would only hide the chip locally and the next
// nowPlaying event would immediately bring it back, because the clip really is
// still playing. A missing clip or a not-yet-running engine is a safe no-op.
func (a *App) StopClip(clipID string) {
	if b := a.getBackend(); b != nil && b.engine != nil {
		b.engine.StopClip(clipID)
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

// SetMonitorSource selects what the local monitor (the user's headset) plays:
// "clips" (default — clean clips only, you hear your own voice acoustically) or
// "transmitted" (the confidence monitor — the EXACT mix sent to the cable, so you
// can audit what Discord receives). It pushes the choice into the engine
// immediately (atomic/RT-safe) and records it in settings so it persists, mirroring
// the other audio setters. It NEVER changes what is transmitted to Discord — only
// what you hear locally for auditing.
func (a *App) SetMonitorSource(mode string) {
	bk := a.getBackend()
	if bk == nil {
		return
	}
	// Normalize unknown values to "clips" so a bad UI value can never leave the
	// engine in an undefined state or persist garbage.
	if mode != config.MonitorSourceTransmitted {
		mode = config.MonitorSourceClips
	}
	a.lcMu.Lock()
	if bk.engine != nil {
		bk.engine.SetMonitorSource(mode)
	}
	bk.settings.Processing.MonitorSource = mode
	a.lcMu.Unlock()
	a.persist()
}

// ---------------------------------------------------------------------------
// Audio — Discord Voice & Video parity setters
// ---------------------------------------------------------------------------
//
// Each follows the same push-to-engine-then-persist pattern as the setters above.
// The engine setters are atomic/RT-safe; the worker reads the new value on its next
// frame. Settings mutation is serialized by lcMu and persisted via the debounced
// writer.

// SetNoiseSuppressionTier selects the noise-suppression strength: "none" |
// "standard" | "high" | "strong" (Discord's None/Standard/Krisp parity). An unknown
// value is coerced to "high" so the engine and config never store garbage. It also
// keeps the legacy NoiseSuppression bool in sync (true iff tier != none) so older
// readers stay consistent.
func (a *App) SetNoiseSuppressionTier(tier string) {
	bk := a.getBackend()
	if bk == nil {
		return
	}
	switch tier {
	case config.NSModeNone, config.NSModeStandard, config.NSModeHigh, config.NSModeStrong:
		// valid
	default:
		tier = config.NSModeHigh
	}
	a.lcMu.Lock()
	if bk.engine != nil {
		bk.engine.SetNoiseSuppressionTier(tier)
	}
	bk.settings.Processing.NoiseSuppressionTier = tier
	bk.settings.Processing.NoiseSuppression = tier != config.NSModeNone
	a.lcMu.Unlock()
	a.persist()
}

// SetEchoCancellation toggles the APM echo canceller (Discord parity; inert without
// a far-end render reference, documented in the UI).
func (a *App) SetEchoCancellation(b bool) {
	bk := a.getBackend()
	if bk == nil {
		return
	}
	a.lcMu.Lock()
	if bk.engine != nil {
		bk.engine.SetEchoCancellation(b)
	}
	bk.settings.Processing.EchoCancellation = b
	a.lcMu.Unlock()
	a.persist()
}

// SetAdvancedVoiceActivity toggles the real (RNNoise speech-probability) VAD gate in
// VAD mode — the breathing fix. Stored as an explicit *bool so a deliberate OFF
// round-trips (the field defaults ON).
func (a *App) SetAdvancedVoiceActivity(b bool) {
	bk := a.getBackend()
	if bk == nil {
		return
	}
	a.lcMu.Lock()
	if bk.engine != nil {
		bk.engine.SetAdvancedVoiceActivity(b)
	}
	bk.settings.Processing.AdvancedVoiceActivity = config.BoolPtr(b)
	a.lcMu.Unlock()
	a.persist()
}

// SetAutoSensitivity toggles automatic input sensitivity (Discord's "Automatically
// determine input sensitivity"). Explicit *bool so a deliberate OFF round-trips.
func (a *App) SetAutoSensitivity(b bool) {
	bk := a.getBackend()
	if bk == nil {
		return
	}
	a.lcMu.Lock()
	if bk.engine != nil {
		bk.engine.SetAutoSensitivity(b)
	}
	bk.settings.Processing.AutoSensitivity = config.BoolPtr(b)
	a.lcMu.Unlock()
	a.persist()
}

// SetAttenuationAmount sets the ducking depth in [0,1] (Discord's attenuation
// amount). The value is clamped engine-side; we persist the clamped value too.
func (a *App) SetAttenuationAmount(v float64) {
	bk := a.getBackend()
	if bk == nil {
		return
	}
	if v < 0 {
		v = 0
	} else if v > 1 {
		v = 1
	}
	amt := float32(v)
	a.lcMu.Lock()
	if bk.engine != nil {
		bk.engine.SetAttenuationAmount(amt)
	}
	bk.settings.Processing.AttenuationAmount = amt
	a.lcMu.Unlock()
	a.persist()
}

// SetInputVolume sets the live mic-passthrough gain (Discord's "Input Volume"),
// mapping to the same engine mic gain + persisted mixer level as SetVolume("mic").
// A separate bound method so the audio panel can expose it independently of the
// soundboard mixer dock.
func (a *App) SetInputVolume(value float64) { a.SetVolume("mic", value) }

// SetOutputVolume sets the local monitor gain (Discord's "Output Volume"), mapping
// to the engine monitor gain + persisted mixer level as SetVolume("monitor").
func (a *App) SetOutputVolume(value float64) { a.SetVolume("monitor", value) }

// SetAudioSubsystem persists the cosmetic Audio Subsystem selector ("standard" |
// "legacy" | "experimental"). It has NO engine effect (a single malgo/WASAPI
// backend); it is stored for Discord parity only. An unknown value coerces to
// "standard".
func (a *App) SetAudioSubsystem(sub string) {
	bk := a.getBackend()
	if bk == nil {
		return
	}
	switch sub {
	case config.AudioSubsystemStandard, config.AudioSubsystemLegacy, config.AudioSubsystemExperimental:
		// valid
	default:
		sub = config.AudioSubsystemStandard
	}
	a.lcMu.Lock()
	bk.settings.Processing.AudioSubsystem = sub
	a.lcMu.Unlock()
	a.persist()
}

// SetPTTHotkey re-binds the push-to-talk hotkey LIVE (Discord's PTT keybind) and
// persists it, completing the "ptt" mic mode in the UI (previously PTT was only
// configurable by hand-editing config.json). The combo (e.g. "ctrl+a") is bound
// through the hotkey manager's unified PTT callback wired once at startup, so the
// new key takes effect immediately — no restart. An empty combo clears the binding.
//
// A parse/registration error (an unsupported key, or a combo another app already
// owns) leaves the PRIOR binding in place, is logged, and is NOT persisted, so the
// config never stores a combo the backend rejected. The setting is only written on a
// successful (re)bind, mirroring the other audio setters' push-then-persist order.
func (a *App) SetPTTHotkey(combo string) {
	bk := a.getBackend()
	if bk == nil {
		return
	}
	combo = strings.TrimSpace(combo)
	if bk.hotkeys != nil {
		if err := bk.hotkeys.SetPTT(combo); err != nil {
			if ctx := a.context(); ctx != nil {
				runtime.LogErrorf(ctx, "set PTT hotkey %q: %v", combo, err)
			}
			return
		}
	}
	a.lcMu.Lock()
	bk.settings.Processing.PTTHotkey = combo
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
		// Re-detect BEFORE choosing install-vs-engage. The cached status is written
		// at process start and, without this, nowhere else — so the branch was
		// picked from state that could predate an install earlier in this session,
		// the user enabling a disabled endpoint in Sound settings, or a transient
		// enumeration failure during login autostart. Each of those ran the elevated
		// installer — and raised a UAC prompt — on a machine whose cable was already
		// there, which is the reinstall loop users actually hit.
		b.redetect()
		if b.setup.CanEngage() && !b.setup.Engaged() {
			// These messages describe only what THIS process did to the Windows
			// default recording device. They must never claim anything about
			// Discord: its input-device selection and noise-suppression settings
			// are not readable from here.
			a.emitInstallProgress("Engaging routing — pointing your default mic at CABLE Output…", false, "")
			if err := b.setup.Engage(); err != nil {
				a.emitInstallProgress("Could not engage routing.", true, err.Error())
				a.emitRoutingStatus(b.setup.snapshot())
				return
			}
			a.emitInstallProgress("Routing engaged — your default mic now points at CABLE Output.", true, "")
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
		// VERIFY, do not assume. The installer exiting 0 is necessary but not
		// sufficient: the driver may need a reboot before Windows publishes the
		// endpoints, and endpoints can exist yet be DISABLED (invisible to every
		// enumeration path we have). Re-detect and let the endpoints themselves
		// decide the message, so we never again report "installed" for a machine
		// that will still say "absent" on the next launch — the reinstall loop.
		st := b.redetect()
		if st.CanEngage {
			a.emitInstallProgress("VB-CABLE installed and detected — engage routing to point your default mic at it.", true, "")
		} else {
			a.emitInstallProgress("The VB-CABLE installer finished, but Windows has not published the CABLE devices yet. Restart Windows, then launch SoundBoard again.\n\nIf it still says VB-CABLE is missing after that restart, do NOT reinstall — it will not help. Open Windows Sound settings (mmsys.cpl), right-click inside the Playback and Recording tabs and tick \"Show Disabled Devices\": if CABLE Input / CABLE Output appear greyed out, enable them.", true, "")
		}
		a.emitRoutingStatus(st)
	}()
}

// GetRoutingStatus returns the current routing status without recomputing the
// whole snapshot.
func (a *App) GetRoutingStatus() RoutingStatus {
	if b := a.getBackend(); b != nil {
		return b.setup.snapshot()
	}
	return RoutingStatus{State: "unavailable", Detail: "Backend not initialized.", CanEngage: false}
}

// RedetectRouting re-enumerates audio devices and returns (and broadcasts) the
// refreshed routing status. It exists so "VB-CABLE not detected" is RECOVERABLE
// within a session: the status used to be read once at startup and never again,
// so enabling a disabled endpoint in Sound settings, or a one-off enumeration
// failure during login autostart, left the app insisting on an install until the
// user restarted it — and reinstalling is exactly what cannot fix either case.
func (a *App) RedetectRouting() RoutingStatus {
	b := a.getBackend()
	if b == nil {
		return RoutingStatus{State: "unavailable", Detail: "Backend not initialized.", CanEngage: false}
	}
	st := b.redetect()
	a.emitRoutingStatus(st)
	return st
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

// Restart launches a FRESH instance of SoundBoard and then quits this one. It
// is the in-window "Restart app" action offered after a successful VB-CABLE
// install: the running process initialized its audio context BEFORE the cable
// endpoints existed, so it cannot route to a cable that appeared mid-session
// (newBackend enumerates devices and auto-engages routing only at startup). A
// fresh process initializes a clean audio context that sees the just-added CABLE
// Input/Output and auto-engages routing — an APP restart, NOT a Windows reboot.
//
// It mirrors the Fyne build's App.restart (internal/ui/ui.go) exactly: re-exec
// os.Executable() in the current working directory, then exit via the single
// Quit choke point so OnShutdown still runs the backend teardown (engine stop,
// restore default mic, flush + save config) exactly once. If the new process
// cannot be launched it falls back to just quitting, so the button is never a
// dead end. (WASAPI capture is shared-mode, so the brief overlap while this
// process tears down its engine and the new one bootstraps does not contend for
// the devices — the same launch-then-quit ordering the Fyne build has shipped.)
func (a *App) Restart() {
	if exe, err := os.Executable(); err == nil {
		cmd := exec.Command(exe)
		if wd, werr := os.Getwd(); werr == nil {
			cmd.Dir = wd
		}
		if err := cmd.Start(); err != nil {
			if ctx := a.context(); ctx != nil {
				runtime.LogErrorf(ctx, "restart: launch new instance: %v", err)
			}
		}
	}
	a.Quit()
}

// persist requests that the current settings be written to disk. It does NOT
// write synchronously: it marks settings dirty and (re)arms a short trailing
// timer, so a burst of setter calls — e.g. a continuous slider drag wiring the
// 'input' event to SetVolume/SetGateSensitivity dozens of times a second —
// coalesces into a SINGLE atomic config.Save() once the burst stops, instead of
// performing tens of full json.MarshalIndent + temp-file write + rename cycles
// on the UI thread. The in-memory settings were already mutated synchronously by
// the caller (under lcMu) and the engine was already updated, so the change is
// live instantly; only the disk write is deferred. Definitive persistence on
// quit is still guaranteed by Backend.close (and runCleanup flushes any pending
// write first), matching the Fyne build's "applies instantly · saved on exit".
func (a *App) persist() {
	b := a.getBackend()
	if b == nil || b.settings == nil {
		return
	}
	a.lcMu.Lock()
	a.persistDirty = true
	if a.persistTimer == nil {
		a.persistTimer = time.AfterFunc(persistDebounce, a.flushPersist)
	} else {
		a.persistTimer.Reset(persistDebounce)
	}
	a.lcMu.Unlock()
}

// flushPersist writes the current settings to disk if they are dirty, clearing
// the dirty flag on success. It is the single place that calls Settings.Save for
// the debounced path: the trailing timer fires it after a burst of mutations,
// and runCleanup calls it directly on shutdown so a pending write is never lost.
// It takes lcMu so a concurrent settings mutation (another bound method) cannot
// race the marshal; a failure is surfaced to the diagnostics log and leaves the
// dirty flag set so the next flush (timer or shutdown) retries. Save is fast
// enough that holding lcMu across it is acceptable here, and it matches the
// original synchronous persist's locking discipline.
func (a *App) flushPersist() {
	b := a.getBackend()
	if b == nil || b.settings == nil {
		return
	}
	a.lcMu.Lock()
	if !a.persistDirty {
		a.lcMu.Unlock()
		return
	}
	err := b.settings.Save()
	if err == nil {
		a.persistDirty = false
	}
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

			a.pollNowPlaying(b)
		}
	}
}

// pollNowPlaying reads the engine's published now-playing set and emits it when
// it differs from the last one emitted, so the frontend's NOW PLAYING chips and
// "Stop · N" counter clear themselves when a clip finishes.
//
// This is the UI half of the fix for chips that never went away: the RT mix
// callback republishes the live cursor set every buffer (see
// internal/audio/nowplaying.go) because it may not emit an event itself, and
// this poll — on the SAME 20 Hz ticker that already drives gateLevel — is what
// turns that into a frontend event. The event carries the whole current set, not
// an "ended" pulse, so it is idempotent and a dropped event self-heals on the
// next tick.
//
// An inconsistent snapshot (the seqlock reader losing every retry) or a stopped
// engine is NOT reported as an empty set: the former keeps the previous view, the
// latter genuinely has nothing playing and emits empty exactly once.
func (a *App) pollNowPlaying(b *Backend) {
	var ids []string
	if b.audioRunning && b.engine != nil {
		got, ok := b.engine.PlayingClips()
		if !ok {
			return // snapshot was torn; keep the current view and retry next tick.
		}
		ids = got
	}

	a.lcMu.Lock()
	changed := a.lastPlaying == nil || !sameIDs(a.lastPlaying, ids)
	if changed {
		a.lastPlaying = ids
	}
	a.lcMu.Unlock()
	if changed {
		a.emitNowPlaying(ids)
	}
}

// sameIDs reports whether two clip-ID sets are identical, order included. The
// engine returns them in trigger order, so an order change is a real change.
func sameIDs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// emitGateLevel pushes the live mic-open level [0..1].
func (a *App) emitGateLevel(level float64) {
	if ctx := a.context(); ctx != nil {
		runtime.EventsEmit(ctx, "gateLevel", map[string]any{"level": level})
	}
}

// emitNowPlaying pushes the current set of playing clip IDs. The payload is
// always the FULL set (never a delta), so the frontend can reconcile its chip
// row against it and any dropped event is corrected by the next one. clips is
// emitted as a non-nil slice so the JS side always sees an array.
func (a *App) emitNowPlaying(clips []string) {
	if clips == nil {
		clips = []string{}
	}
	if ctx := a.context(); ctx != nil {
		runtime.EventsEmit(ctx, "nowPlaying", map[string]any{"clips": clips})
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
