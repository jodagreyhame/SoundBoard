//go:build !fyne

// systray.go runs the companion system-tray icon + menu on its own goroutine,
// coexisting with the Wails main loop.
//
// Why a companion tray: Wails v2 has no robust built-in native systray, so we
// run getlantern/systray (the same library Fyne's tray wraps) on a dedicated
// goroutine. systray.Run owns its own OS message loop and blocks, so it MUST run
// off the Wails main thread.
//
// Coordination: the tray's menu items need the Wails runtime context to show the
// window / quit, but that context only exists after Wails' OnStartup. The tray
// therefore calls back into the App (app.ShowFromTray / app.Quit), which no-op
// safely until app.ctx is set. This keeps the two main loops decoupled.
package main

import (
	_ "embed"

	"github.com/getlantern/systray"
	wailsruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// trayIcon is the tray + window icon (Windows .ico). Embedded so the single
// binary needs no sidecar icon file. Matches the app's blurple brand mark.
//
//go:embed build/windows/icon.ico
var trayIcon []byte

// startTray launches the system tray on its own goroutine. systray.Run blocks
// running the tray's message loop, so it never returns until stopTray (or app
// quit) tears it down.
func startTray(app *App) {
	go systray.Run(func() { onTrayReady(app) }, func() {})
}

// stopTray quits the tray message loop. Safe to call once on shutdown; the
// getlantern library guards against a double-quit panic in practice, and we only
// call it from OnShutdown.
func stopTray() {
	systray.Quit()
}

// onTrayReady builds the tray icon + menu and wires the menu actions back into
// the App. It runs on the systray goroutine.
func onTrayReady(app *App) {
	if len(trayIcon) > 0 {
		systray.SetIcon(trayIcon)
	}
	systray.SetTitle("SoundBoard")
	systray.SetTooltip("SoundBoard — soundboard for Discord")

	mOpen := systray.AddMenuItem("Open SoundBoard", "Show the SoundBoard window")
	systray.AddSeparator()
	mQuit := systray.AddMenuItem("Quit", "Exit SoundBoard")

	// One reader goroutine drains both click channels for the life of the tray.
	go func() {
		for {
			select {
			case <-mOpen.ClickedCh:
				app.showFromTray()
			case <-mQuit.ClickedCh:
				app.Quit()
				return
			}
		}
	}()
}

// showFromTray shows and focuses the window. It is the tray "Open" action;
// it no-ops safely until the Wails context exists (app.ctx set in startup).
func (a *App) showFromTray() {
	if a.ctx == nil {
		return
	}
	wailsruntime.WindowShow(a.ctx)
	// Unminimise in case it was minimised rather than hidden.
	wailsruntime.WindowUnminimise(a.ctx)
}
