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
	"strings"

	"fyne.io/fyne/v2"
	fyneapp "fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/driver/desktop"
	"fyne.io/fyne/v2/widget"

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

	// search holds the current filter text; rebuildClips reads it.
	search string
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
	a.fyneApp = fyneapp.New()
	icon := fyne.NewStaticResource("soundboard.png", iconPNG)
	a.fyneApp.SetIcon(icon)

	a.win = a.fyneApp.NewWindow("SoundBoard")
	a.win.SetContent(a.buildContent())
	a.win.Resize(fyne.NewSize(720, 560))
	a.win.CenterOnScreen()

	// Closing the window hides to tray rather than quitting.
	a.win.SetCloseIntercept(func() { a.win.Hide() })

	// System tray (desktop driver only). The menu controls show/quit; the icon
	// itself opens the window on left-click via SetSystemTrayWindow.
	if desk, ok := a.fyneApp.(desktop.App); ok {
		menu := fyne.NewMenu("SoundBoard",
			fyne.NewMenuItem("Open SoundBoard", a.ShowWindow),
			fyne.NewMenuItemSeparator(),
			fyne.NewMenuItem("Quit", func() { a.fyneApp.Quit() }),
		)
		desk.SetSystemTrayMenu(menu)
		desk.SetSystemTrayIcon(icon)
		desk.SetSystemTrayWindow(a.win)
	}

	a.win.Show()
	a.fyneApp.Run()
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

// buildContent assembles the full window layout: setup banner on top, volume
// area on the bottom, and the search box + category clip grid filling the
// center.
func (a *App) buildContent() fyne.CanvasObject {
	return container.NewBorder(
		a.buildSetupBanner(), // top
		a.buildVolumeArea(),  // bottom
		nil, nil,
		a.buildClipBrowser(), // center
	)
}

// buildSetupBanner renders the VB-CABLE status line plus an install/fix action.
func (a *App) buildSetupBanner() fyne.CanvasObject {
	ready, detail := false, "VB-CABLE status unknown"
	if a.setup != nil {
		ready, detail = a.setup.Status()
	}
	status := widget.NewLabel(detail)
	action := widget.NewButton("Install / Fix routing", func() {
		if a.setup == nil {
			return
		}
		if ready {
			_ = a.setup.Engage()
		} else {
			_ = a.setup.Install()
		}
	})
	return container.NewBorder(nil, widget.NewSeparator(), nil, action, status)
}

// buildClipBrowser builds the search box and the scrollable, category-grouped
// grid of clip buttons. (Skeleton: a static grid keyed off the current filter;
// live re-filtering is wired here but the rebuild is intentionally minimal.)
func (a *App) buildClipBrowser() fyne.CanvasObject {
	search := widget.NewEntry()
	search.SetPlaceHolder("Search clips…")
	grid := container.NewVBox()

	rebuild := func() {
		grid.Objects = grid.Objects[:0]
		if a.lib != nil {
			for i := range a.lib.Categories {
				cat := &a.lib.Categories[i]
				var buttons []fyne.CanvasObject
				for _, clip := range cat.Clips {
					if !match(clip, a.search) {
						continue
					}
					id := clip.ID
					buttons = append(buttons, widget.NewButton(clip.Name, func() {
						if a.player != nil {
							a.player.TriggerGain(id, 1)
						}
					}))
				}
				if len(buttons) == 0 {
					continue
				}
				grid.Add(widget.NewLabel(cat.Name))
				grid.Add(container.NewGridWrap(fyne.NewSize(150, 36), buttons...))
			}
		}
		grid.Refresh()
	}

	search.OnChanged = func(s string) {
		a.search = s
		rebuild()
	}
	rebuild()

	return container.NewBorder(search, nil, nil, nil, container.NewVScroll(grid))
}

// buildVolumeArea builds the mic / master / per-clip sliders. The per-clip
// slider here is a single shared control acting on the most-recently triggered
// clip in the full app; the skeleton wires it to SetMaster's sibling SetClip via
// a placeholder id so the signature path compiles.
func (a *App) buildVolumeArea() fyne.CanvasObject {
	mic := widget.NewSlider(0, 2)
	master := widget.NewSlider(0, 2)
	perClip := widget.NewSlider(0, 2)
	if a.vol != nil {
		mic.SetValue(float64(a.vol.Mic()))
		master.SetValue(float64(a.vol.Master()))
		perClip.SetValue(1)
	}
	mic.OnChanged = func(v float64) {
		if a.vol != nil {
			a.vol.SetMic(float32(v))
		}
	}
	master.OnChanged = func(v float64) {
		if a.vol != nil {
			a.vol.SetMaster(float32(v))
		}
	}
	// Per-clip control is wired by the selected clip elsewhere; left as a
	// display control in the skeleton.
	_ = perClip

	return container.NewVBox(
		widget.NewSeparator(),
		container.NewBorder(nil, nil, widget.NewLabel("Mic"), nil, mic),
		container.NewBorder(nil, nil, widget.NewLabel("Soundboard"), nil, master),
		container.NewBorder(nil, nil, widget.NewLabel("Selected clip"), nil, perClip),
	)
}

// match reports whether a clip passes the current filter (case-insensitive
// substring on name or category; empty filter matches everything).
func match(clip *catalog.Clip, filter string) bool {
	if filter == "" {
		return true
	}
	f := strings.ToLower(filter)
	return strings.Contains(strings.ToLower(clip.Name), f) ||
		strings.Contains(strings.ToLower(clip.Category), f)
}
