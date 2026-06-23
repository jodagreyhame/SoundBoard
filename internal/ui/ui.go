// Package ui is the SoundBoard v2 front end: a Fyne main window plus a Fyne
// system-tray icon.
//
// The window has a live-filtering search box, clips grouped by category as a
// scrollable grid of buttons (click to play), a volume area (mic level,
// soundboard master, and a per-clip control), and a setup banner showing the
// VB-CABLE status with an install/fix-routing action. The system-tray icon's
// menu offers "Open SoundBoard" (show + raise) and "Quit"; closing the window
// HIDES it to the tray instead of quitting.
//
// ui deliberately depends on neither internal/audio nor internal/setup: it talks
// to small local interfaces (Player, VolumeController, SetupController) that
// main.go wires to the real engine and setup controller. That keeps the Fyne
// layer free of audio/COM concerns and trivially testable with fakes.
package ui

import (
	_ "embed"
	"os"
	"os/exec"

	"fyne.io/fyne/v2"
	fyneapp "fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/driver/desktop"

	"soundboard/internal/catalog"
)

// iconPNG is the window + tray icon. Fyne accepts a PNG fyne.Resource directly.
//
//go:embed icon.png
var iconPNG []byte

// Player fires clips. The UI never imports audio; *audio.Engine satisfies this
// (TriggerGain(id, 1) == Trigger(id)).
type Player interface {
	// TriggerGain plays the clip id at the given linear gain (1.0 = the clip's
	// own configured per-clip volume * master, unchanged).
	TriggerGain(id string, gain float32)
	// StopAll immediately silences every clip currently playing on both the
	// duplex (-> Discord) and monitor (-> headset) paths. The user's live mic
	// passthrough is unaffected. Drives the window's "Stop" button.
	StopAll()
}

// FavoritesController is the UI's view of the user's favourited clips. The UI
// uses it to render a star toggle on every clip and a pinned "★ Favourites"
// section at the top of the browser. main.go wires it to config.Settings so a
// toggle mutates the persisted favourites list (saved on exit).
type FavoritesController interface {
	// IsFavorite reports whether the clip id is currently favourited.
	IsFavorite(id string) bool
	// ToggleFavorite flips the favourite state of clip id, adding it (appended to
	// the end) when it was not favourited and removing it otherwise.
	ToggleFavorite(id string)
	// Favorites returns the favourited clip IDs in pinned display order.
	Favorites() []string
}

// VolumeController is the UI's view of the mixer. Setters push a new level into
// the engine immediately; getters seed the sliders at startup. All levels are
// linear amplitudes (1.0 = unchanged). The three top-level levels are
// INDEPENDENT and each has a clear "who hears it" meaning:
//   - Mic     : how loud the user's real voice is to Discord.
//   - Master  : the soundboard level OTHERS hear in Discord (clips -> cable).
//   - Monitor : the soundboard level the USER hears locally (clips -> headset).
//
// SetClip / Clip are the per-clip multiplier applied on top of both Master and
// Monitor for the currently-selected clip.
type VolumeController interface {
	SetMic(gain float32)
	SetMaster(gain float32)
	// SetMonitor sets the local monitor level — the soundboard volume the USER
	// hears in their own headset, independent of what Discord hears (SetMaster).
	SetMonitor(gain float32)
	SetClip(id string, gain float32)

	Mic() float32
	Master() float32
	// Monitor returns the local "what you hear" soundboard level.
	Monitor() float32
	Clip(id string) float32
}

// AudioController is the UI's view of the mic-path processing suite for the
// "Audio" settings panel. It mirrors config.AudioProcessing: a getter and setter
// for each independent control, plus GateLevel for a live mic-open meter. main
// wires it to the engine + settings so a setter pushes the change into the engine
// immediately AND persists it; getters seed the panel's controls at startup.
//
// The UI never imports internal/audio (or any cgo): it only talks to this small
// interface, exactly like Player/VolumeController/SetupController, keeping the
// Fyne layer free of audio/COM/cgo concerns and testable with a fake.
//
// Every control applies ONLY to the live mic before it is mixed into the cable;
// soundboard clips bypass all of it. MicMode is one of "vad", "ptt", "always",
// "mute" (the panel offers these as a choice); GateSensitivity is in [0,1].
type AudioController interface {
	// NoiseSuppression / SetNoiseSuppression toggle RNNoise on the mic. A no-op
	// effect when the build lacks RNNoise, so the panel can always offer it.
	NoiseSuppression() bool
	SetNoiseSuppression(on bool)
	// AGC / SetAGC toggle the RMS-target leveler on the mic.
	AGC() bool
	SetAGC(on bool)
	// Ducking / SetDucking toggle lowering clips while the mic gate is open.
	Ducking() bool
	SetDucking(on bool)
	// MicMode / SetMicMode read/select the gate mode ("vad"|"ptt"|"always"|"mute").
	MicMode() string
	SetMicMode(mode string)
	// GateSensitivity / SetGateSensitivity read/set the gate threshold in [0,1].
	GateSensitivity() float32
	SetGateSensitivity(t float32)
	// ForceThrough / SetForceThrough toggle the voiced carrier on the cable path.
	ForceThrough() bool
	SetForceThrough(on bool)
	// PTTHotkey / SetPTTHotkey read/set the push-to-talk combo (empty = none).
	PTTHotkey() string
	SetPTTHotkey(combo string)
	// GateLevel returns the current mic-gate open level in [0,1] for a live meter
	// (0 = closed, 1 = open). Polled by the UI; cheap to call.
	GateLevel() float32
}

// SetupController exposes the VB-CABLE / auto-route state to the banner.
type SetupController interface {
	// Status reports whether routing is ready (cable present AND engaged — i.e.
	// the Windows default mic is actually pointed at CABLE Output) and a short
	// human-readable detail line for the banner. ready is false when the cable is
	// missing OR present-but-not-yet-engaged.
	Status() (ready bool, detail string)
	// CanEngage reports whether the cable is present so routing CAN be engaged
	// without installing first. The banner uses it to choose between the Install
	// and Engage actions when Status is not yet ready.
	CanEngage() bool
	// Install runs the one-click VB-CABLE download + silent elevated install.
	Install() error
	// Engage asserts the default-capture routing to CABLE Output and records that
	// routing is now engaged (so Status flips to ready).
	Engage() error
}

// WindowStore persists the main window's size so the app reopens at the size the
// user left it. Optional: when nil the window uses its default size. Width/Height
// are in Fyne logical pixels; a zero saved size means "use the default".
type WindowStore interface {
	// WindowSize returns the saved window size. ok is false (or w/h are 0) when no
	// size has been saved yet, in which case the default size is used.
	WindowSize() (w, h float32, ok bool)
	// SetWindowSize records the latest window size to be persisted on save.
	SetWindowSize(w, h float32)
}

// App owns the Fyne application, the main window, and the wired controllers.
type App struct {
	lib    *catalog.Library
	player Player
	vol    VolumeController
	setup  SetupController

	// window persists/restores the main window size. Optional; nil = default size.
	window WindowStore

	// favs is the favourites view-model: star toggles and the pinned Favourites
	// section read it. Optional; nil = no favourites UI (stars/section hidden).
	favs FavoritesController

	// audio is the mic-processing view-model for the "Audio" settings panel.
	// Optional; nil = the panel is omitted (the rest of the UI is unaffected).
	// Wired via WithAudio so existing call sites that do not pass it keep working.
	audio AudioController

	fyneApp fyne.App
	win     fyne.Window

	// search holds the current filter text; the clip browser reads it.
	search string

	// selected is the clip ID currently bound to the per-clip volume slider in
	// the volume panel (empty = none selected).
	selected string

	// rebuildBrowser re-renders the category sections from the current filter.
	// Assigned in buildClipBrowser; nil before Run.
	rebuildBrowser func()

	// selectClip binds a clip to the per-clip volume slider. Assigned in
	// buildVolumeArea; nil before Run.
	selectClip func(clip *catalog.Clip)
}

// New constructs the App with its dependencies. It does not build any Fyne
// objects yet — that happens in Run, on the goroutine that owns the Fyne main
// loop.
func New(lib *catalog.Library, player Player, vol VolumeController, setup SetupController) *App {
	return &App{
		lib:    lib,
		player: player,
		vol:    vol,
		setup:  setup,
	}
}

// WithWindowStore attaches an optional WindowStore so the window size is
// restored on build and recorded on close/quit. Returns the App for chaining.
// Call before Run/build.
func (a *App) WithWindowStore(w WindowStore) *App {
	a.window = w
	return a
}

// WithFavorites attaches an optional FavoritesController so each clip gets a star
// toggle and a pinned "★ Favourites" section is shown at the top of the browser.
// When nil the favourites UI is omitted entirely. Returns the App for chaining.
// Call before Run/build.
func (a *App) WithFavorites(f FavoritesController) *App {
	a.favs = f
	return a
}

// WithAudio attaches an optional AudioController so the window gains an "Audio"
// settings panel for the mic-processing suite (noise suppression, AGC, gate mode
// + sensitivity, ducking, force-through carrier, PTT hotkey) and a live mic-open
// meter. When nil the panel is omitted entirely and the rest of the UI is
// unchanged. Returns the App for chaining. Call before Run/build.
//
// This is a foundation stub: it stores the controller so main can wire the engine
// + settings against a stable signature now. The panel widgets themselves are
// built in a later phase; storing the controller here does not yet add any Fyne
// objects, so existing UI tests are unaffected.
func (a *App) WithAudio(c AudioController) *App {
	a.audio = c
	return a
}

// Run builds the window and system tray, then blocks running the Fyne main
// loop. It must be called on the main goroutine. Closing the window hides it to
// the tray (SetCloseIntercept); the tray's Quit item exits the app.
func (a *App) Run() {
	a.build(fyneapp.New())
	a.win.Show()
	a.fyneApp.Run()
}

// build wires the given fyne.App into a full window + system tray. It is split
// out of Run so tests can drive it with a headless test app and never call
// Run (which would block on the GUI event loop).
func (a *App) build(app fyne.App) {
	a.fyneApp = app
	icon := fyne.NewStaticResource("soundboard.png", iconPNG)
	a.fyneApp.SetIcon(icon)

	a.win = a.fyneApp.NewWindow("SoundBoard")
	a.win.SetContent(a.buildContent())

	// Restore the saved window size when one was persisted; otherwise use the
	// default. Center either way so a restored size still opens on-screen.
	w, h := float32(760), float32(600)
	if a.window != nil {
		if sw, sh, ok := a.window.WindowSize(); ok && sw > 0 && sh > 0 {
			w, h = sw, sh
		}
	}
	a.win.Resize(fyne.NewSize(w, h))
	a.win.CenterOnScreen()

	// Closing the window hides to tray rather than quitting, so the soundboard
	// and hotkeys keep running in the background. Record the current size first so
	// it is persisted on the next settings Save (window close or app quit).
	a.win.SetCloseIntercept(func() {
		a.recordWindowSize()
		a.win.Hide()
	})

	// System tray (desktop driver only). The menu controls show/quit; the icon
	// itself reopens the window on click via SetSystemTrayWindow.
	if desk, ok := a.fyneApp.(desktop.App); ok {
		menu := fyne.NewMenu("SoundBoard",
			fyne.NewMenuItem("Open SoundBoard", a.ShowWindow),
			fyne.NewMenuItemSeparator(),
			fyne.NewMenuItem("Quit", a.quit),
		)
		desk.SetSystemTrayMenu(menu)
		desk.SetSystemTrayIcon(icon)
		desk.SetSystemTrayWindow(a.win)
	}
}

// ShowWindow shows the main window and raises/focuses it. Safe to call from the
// tray menu.
func (a *App) ShowWindow() {
	if a.win == nil {
		return
	}
	a.win.Show()
	a.win.RequestFocus()
}

// quit really exits the application (the tray "Quit" item). Window close only
// hides; this is the single path that ends the process. It records the final
// window size first so it is persisted by main's deferred settings Save.
func (a *App) quit() {
	a.recordWindowSize()
	if a.fyneApp != nil {
		a.fyneApp.Quit()
	}
}

// restart launches a fresh instance of SoundBoard and then quits this one.
// Used after a VB-CABLE install so the new process initializes a clean audio
// context that enumerates the just-added cable endpoints and auto-engages
// routing — an APP restart, NOT a Windows reboot. If the new process cannot be
// launched it falls back to just quitting.
func (a *App) restart() {
	if exe, err := os.Executable(); err == nil {
		cmd := exec.Command(exe)
		if wd, werr := os.Getwd(); werr == nil {
			cmd.Dir = wd
		}
		_ = cmd.Start()
	}
	a.quit()
}

// recordWindowSize pushes the current window content size into the WindowStore
// (if any) so it is persisted on the next settings Save. No-op when no store is
// attached or the window has not been built. Uses the canvas size, which is the
// content size Resize takes, so a later restore round-trips.
func (a *App) recordWindowSize() {
	if a.window == nil || a.win == nil {
		return
	}
	sz := a.win.Canvas().Size()
	if sz.Width > 0 && sz.Height > 0 {
		a.window.SetWindowSize(sz.Width, sz.Height)
	}
}
