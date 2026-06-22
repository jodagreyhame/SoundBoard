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
}

// VolumeController is the UI's view of the mixer. Setters push a new level into
// the engine immediately; getters seed the sliders at startup. All levels are
// linear amplitudes (1.0 = unchanged).
type VolumeController interface {
	SetMic(gain float32)
	SetMaster(gain float32)
	SetClip(id string, gain float32)

	Mic() float32
	Master() float32
	Clip(id string) float32
}

// SetupController exposes the VB-CABLE / auto-route state to the banner.
type SetupController interface {
	// Status reports whether routing is ready (cable present + engaged) and a
	// short human-readable detail line for the banner.
	Status() (ready bool, detail string)
	// Install runs the one-click VB-CABLE download + silent elevated install.
	Install() error
	// Engage (re)asserts the default-capture routing to CABLE Output.
	Engage() error
}

// App owns the Fyne application, the main window, and the wired controllers.
type App struct {
	lib    *catalog.Library
	player Player
	vol    VolumeController
	setup  SetupController

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
	a.win.Resize(fyne.NewSize(760, 600))
	a.win.CenterOnScreen()

	// Closing the window hides to tray rather than quitting, so the soundboard
	// and hotkeys keep running in the background.
	a.win.SetCloseIntercept(func() { a.win.Hide() })

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
// hides; this is the single path that ends the process.
func (a *App) quit() {
	if a.fyneApp != nil {
		a.fyneApp.Quit()
	}
}
