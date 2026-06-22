// Package tray is the system-tray UI built on energye/systray. It renders the
// embedded sound library as category submenus, a monitor toggle, and quit.
package tray

import (
	_ "embed"

	"github.com/energye/systray"

	"soundboard/internal/catalog"
)

// iconICO is the tray icon (32x32 ICO). energye/systray.SetIcon expects ICO
// bytes on Windows.
//
//go:embed icon.ico
var iconICO []byte

// Player is the minimal interface the tray needs to fire a clip. Implemented by
// *audio.Engine.
type Player interface {
	Trigger(id string)
}

// UI owns the tray menu state and the registered callbacks.
type UI struct {
	lib    *catalog.Library
	player Player

	monitorInitiallyOn bool

	cableReady     bool
	onInstallCable func()
	onDiscordSteps func()

	onMonitorToggle func(bool)
	onQuit          func()
}

// New creates a tray UI bound to the library and player.
func New(lib *catalog.Library, player Player) *UI {
	return &UI{
		lib:    lib,
		player: player,
	}
}

// SetMonitorInitialState records whether the monitor is already enabled so the
// tray checkbox renders consistent with the engine's real state on launch.
// Without this the checkbox always starts unchecked even when the engine booted
// with monitoring on, forcing a double-click to actually turn it off.
func (u *UI) SetMonitorInitialState(on bool) {
	u.monitorInitiallyOn = on
}

// SetSetup configures the always-visible Setup section. cableReady controls the
// status line; onInstall opens the VB-CABLE download page; onSteps shows the
// Discord configuration steps.
func (u *UI) SetSetup(cableReady bool, onInstall, onSteps func()) {
	u.cableReady = cableReady
	u.onInstallCable = onInstall
	u.onDiscordSteps = onSteps
}

// OnMonitorToggle registers the callback fired when the user toggles the
// monitor menu item (true = enabled).
func (u *UI) OnMonitorToggle(fn func(bool)) {
	u.onMonitorToggle = fn
}

// OnQuit registers the callback fired when the user selects Quit.
func (u *UI) OnQuit(fn func()) {
	u.onQuit = fn
}

// Run wraps systray.Run: it builds category submenus from lib.Categories
// (one menu item per category, one sub-item per clip whose click calls
// player.Trigger(clip.ID)), plus a Monitor toggle and Quit. onReady is invoked
// once the tray is initialized. This blocks until the tray exits.
func (u *UI) Run(onReady func()) {
	systray.Run(u.build(onReady), u.onExit)
}

// build returns the systray onReady closure that constructs the whole menu.
func (u *UI) build(onReady func()) func() {
	return func() {
		systray.SetIcon(iconICO)
		systray.SetTitle("SoundBoard")
		systray.SetTooltip("SoundBoard")

		// Setup section (always visible). Audio routing requires VB-CABLE plus a
		// one-time Discord configuration, so surface both right at the top with a
		// clear status line and clickable actions.
		statusLabel := "✓ VB-CABLE detected"
		if !u.cableReady {
			statusLabel = "⚠ VB-CABLE NOT detected — click 'Install VB-CABLE' below"
		}
		status := systray.AddMenuItem(statusLabel, "Sound is sent over your mic through VB-CABLE")
		status.Disable()

		install := systray.AddMenuItem("Install VB-CABLE (open download)…", "Open https://vb-audio.com/Cable/ in your browser")
		install.Click(func() {
			if u.onInstallCable != nil {
				u.onInstallCable()
			}
		})

		steps := systray.AddMenuItem("Discord setup steps…", "Show the exact Discord Voice settings to use")
		steps.Click(func() {
			if u.onDiscordSteps != nil {
				u.onDiscordSteps()
			}
		})

		systray.AddSeparator()

		// One top-level menu item per category; one sub-item per clip.
		if u.lib != nil {
			for i := range u.lib.Categories {
				cat := &u.lib.Categories[i]
				catItem := systray.AddMenuItem(cat.Name, cat.Name)
				for _, clip := range cat.Clips {
					clip := clip // capture per-iteration for the closure
					item := catItem.AddSubMenuItem(clip.Name, clip.ID)
					item.Click(func() {
						if u.player != nil {
							u.player.Trigger(clip.ID)
						}
					})
				}
			}
		}

		systray.AddSeparator()

		// Monitor toggle: lets the user hear triggered sounds locally. The
		// initial checked state mirrors the engine's real monitor state so UI
		// and engine do not desync at launch.
		monitor := systray.AddMenuItemCheckbox(
			"Monitor (hear sounds yourself)",
			"Play triggered sounds through your own output too",
			u.monitorInitiallyOn,
		)
		monitor.Click(func() {
			var enabled bool
			if monitor.Checked() {
				monitor.Uncheck()
				enabled = false
			} else {
				monitor.Check()
				enabled = true
			}
			if u.onMonitorToggle != nil {
				u.onMonitorToggle(enabled)
			}
		})

		systray.AddSeparator()

		// Quit: invoke the user callback, then tear down the tray.
		quit := systray.AddMenuItem("Quit", "Exit SoundBoard")
		quit.Click(func() {
			if u.onQuit != nil {
				u.onQuit()
			}
			systray.Quit()
		})

		if onReady != nil {
			onReady()
		}
	}
}

// onExit runs when the systray event loop terminates. The quit callback already
// fired on the Quit click; this is a no-op kept as the explicit exit hook.
func (u *UI) onExit() {}
