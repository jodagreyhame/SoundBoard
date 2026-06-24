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

	"github.com/wailsapp/wails/v2/pkg/runtime"
)

// App is the Wails application object. ctx is captured in OnStartup (via
// startup) so the bound methods can emit events and drive the window. Phase 2
// adds the real dependencies (engine, setup controller, settings, catalog) as
// fields; the bound-method signatures do not change.
type App struct {
	ctx context.Context
}

// NewApp constructs the bound App. The real dependencies are injected in phase 2.
func NewApp() *App {
	return &App{}
}

// startup is wired to Wails' OnStartup. It captures the context the runtime
// helpers (events, window control) require.
func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
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

// GetState returns the full UI snapshot. SKELETON: returns representative sample
// data (the real 212-clip / 12-category catalog shape) so the shell renders.
// Phase 2 reads the live catalog, settings, engine, and setup controller.
func (a *App) GetState() State {
	cats := []Category{
		{"game-clips", 6}, {"games", 39}, {"memes", 12}, {"movies", 36},
		{"game-clips", 2}, {"reactions", 14}, {"game-clips", 9}, {"scifi", 28},
		{"game-clips", 6}, {"films", 35}, {"tv", 12}, {"wow", 13},
	}
	return State{
		Theme: "dark",
		Routing: RoutingStatus{
			State:     "absent",
			Detail:    "VB-CABLE not detected — install it to route audio into Discord.",
			CanEngage: false,
		},
		Categories: cats,
		Clips:      []Clip{},
		Favorites:  []string{},
		Volumes:    Volumes{Mic: 1, Master: 1, Monitor: 1},
		PerClip:    map[string]float64{},
		Audio: AudioState{
			MicMode:          "vad",
			GateSensitivity:  0.15,
			NoiseSuppression: false,
			AGC:              false,
			Ducking:          false,
			ForceThrough:     false,
		},
	}
}

// ---------------------------------------------------------------------------
// Soundboard
// ---------------------------------------------------------------------------

// Play fires the clip at its saved per-clip gain. SKELETON: no-op.
func (a *App) Play(clipID string) {}

// StopAll silences every playing clip on both paths. SKELETON: no-op.
func (a *App) StopAll() {}

// ToggleFavorite flips the favourite state of clipID and returns the new state.
// SKELETON: always reports false.
func (a *App) ToggleFavorite(clipID string) bool { return false }

// ---------------------------------------------------------------------------
// Volumes
// ---------------------------------------------------------------------------

// SetVolume sets a top-level mixer level. kind is "mic" | "master" | "monitor";
// value is a linear gain. SKELETON: no-op.
func (a *App) SetVolume(kind string, value float64) {}

// SetClipVolume sets the per-clip multiplier for clipID. SKELETON: no-op.
func (a *App) SetClipVolume(clipID string, value float64) {}

// ---------------------------------------------------------------------------
// Audio (mic-processing suite)
// ---------------------------------------------------------------------------

// SetMicMode selects the gate mode: "vad" | "ptt" | "always" | "mute". SKELETON: no-op.
func (a *App) SetMicMode(mode string) {}

// SetGateSensitivity sets the gate threshold in [0,1]. SKELETON: no-op.
func (a *App) SetGateSensitivity(v float64) {}

// SetNoiseSuppression toggles RNNoise on the mic. SKELETON: no-op.
func (a *App) SetNoiseSuppression(b bool) {}

// SetAGC toggles the automatic gain control. SKELETON: no-op.
func (a *App) SetAGC(b bool) {}

// SetDucking toggles ducking clips while the mic gate is open. SKELETON: no-op.
func (a *App) SetDucking(b bool) {}

// SetForceThrough toggles the voiced carrier on the cable path. SKELETON: no-op.
func (a *App) SetForceThrough(b bool) {}

// ---------------------------------------------------------------------------
// Setup / routing
// ---------------------------------------------------------------------------

// InstallRouting installs OR engages VB-CABLE routing as appropriate, async.
// It emits "installProgress" updates and a "routingStatus" change. SKELETON:
// emits a single done-with-no-op progress event so the frontend wiring can be
// exercised end to end.
func (a *App) InstallRouting() {
	a.emitInstallProgress("Routing is stubbed in the skeleton build.", true, "")
}

// GetRoutingStatus returns the current routing status without recomputing the
// whole snapshot. SKELETON: mirrors GetState's routing.
func (a *App) GetRoutingStatus() RoutingStatus {
	return a.GetState().Routing
}

// ---------------------------------------------------------------------------
// App / window
// ---------------------------------------------------------------------------

// SetTheme persists the chosen theme ("dark" | "light"). SKELETON: no-op (the
// frontend applies the class itself; phase 2 persists it to config).
func (a *App) SetTheme(t string) {}

// Minimize minimizes the window via the Wails runtime.
func (a *App) Minimize() {
	if a.ctx != nil {
		runtime.WindowMinimise(a.ctx)
	}
}

// HideToTray hides the window; the systray "Open SoundBoard" item restores it.
// Closing the window also routes here via OnBeforeClose in main.
func (a *App) HideToTray() {
	if a.ctx != nil {
		runtime.WindowHide(a.ctx)
	}
}

// Quit really exits the app: it runs the existing cleanup (engine stop, restore
// default mic, save config — wired in phase 2) then ends the process. SKELETON:
// quits the Wails runtime, which triggers OnShutdown for cleanup.
func (a *App) Quit() {
	if a.ctx != nil {
		runtime.Quit(a.ctx)
	}
}

// ---------------------------------------------------------------------------
// Event emitters (Go -> JS)
// ---------------------------------------------------------------------------

// emitGateLevel pushes the live mic-open level [0..1]. Phase 2 calls this from a
// ~15-30 Hz ticker while the engine runs.
func (a *App) emitGateLevel(level float64) {
	if a.ctx != nil {
		runtime.EventsEmit(a.ctx, "gateLevel", map[string]any{"level": level})
	}
}

// emitRoutingStatus pushes a routing-status change so the banner/pill update live.
func (a *App) emitRoutingStatus(s RoutingStatus) {
	if a.ctx != nil {
		runtime.EventsEmit(a.ctx, "routingStatus", s)
	}
}

// emitInstallProgress pushes an install/engage progress update.
func (a *App) emitInstallProgress(msg string, done bool, errMsg string) {
	if a.ctx != nil {
		runtime.EventsEmit(a.ctx, "installProgress", map[string]any{
			"msg": msg, "done": done, "err": errMsg,
		})
	}
}
