// Command soundboard (Wails v2 build — the DEFAULT/shipping entrypoint).
//
// This is the SoundBoard GUI rebuilt on Wails v2 (Go + WebView2) with the new
// design. It is the sole entrypoint and default build; the legacy Fyne UI it
// replaced has been removed.
//
// Architecture:
//   - The frontend (frontend/dist) is a vanilla HTML/CSS/JS shell reproducing
//     the design comp; it is embedded into the binary via go:embed and served by
//     the Wails asset server. No JS framework, no npm build step.
//   - The window is FRAMELESS (the design draws its own titlebar) and drags via
//     the -webkit-app-region/--wails-draggable contract on the titlebar.
//   - The bound Go App (app.go) exposes the method contract the frontend calls
//     as window.go.main.App.<Method> and emits live events the frontend
//     subscribes to (gateLevel / routingStatus / installProgress).
//   - A companion system tray (getlantern/systray, systray.go) runs on its own
//     goroutine alongside the Wails main loop: icon + Open/Quit menu. Closing the
//     window hides to tray (OnBeforeClose); tray Open reshows it; tray Quit ends
//     the process after the Wails shutdown cleanup.
//
// The bound App methods are wired to the real backend (internal/audio,
// internal/setup, internal/config, internal/catalog, internal/hotkeys) via the
// Backend constructed in main and injected with app.setBackend, without changing
// any bound-method signature from the original binding contract.
package main

import (
	"context"
	"embed"
	"log"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/windows"
	wailsruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// assets embeds the built frontend. With a vanilla (no-build) frontend the
// source files live directly under frontend/dist, so the embed is the shipped
// asset tree as-is.
//
//go:embed all:frontend/dist
var assets embed.FS

func main() {
	// Route diagnostics to the per-user config-dir log file (the shipping
	// -H=windowsgui build detaches stderr) and mirror to stderr for dev runs.
	closeLog := initLogging()
	defer closeLog()

	app := NewApp()

	// Bootstrap the REAL backend (engine, catalog, VB-CABLE routing, hotkeys,
	// settings) synchronously BEFORE wails.Run — mirroring the Fyne main's
	// startup ordering, where routing auto-engages and the mic is resolved before
	// the UI loop runs. Injecting it now means GetState and the live-events loop
	// see a fully wired backend from the first frame. setBackend also registers
	// backend.close as the App's cleanup hook, so the OnShutdown choke point below
	// performs the real engine.Stop / mic restore / config save.
	backend := newBackend()
	app.setBackend(backend)

	// Launch the companion system tray on its own goroutine BEFORE wails.Run so
	// the tray is up alongside the Wails main loop. The tray's Open/Quit actions
	// call back into the App (show window / quit) once the runtime context exists.
	startTray(app)

	err := wails.Run(&options.App{
		Title:  "SoundBoard",
		Width:  1160,
		Height: 760,

		// The design comp targets a 1160x760 shell; keep a sensible floor so the
		// sidebar + content never collapse. Resizable (DisableResize stays false).
		MinWidth:  900,
		MinHeight: 620,

		// Frameless: the HTML titlebar IS the window chrome and drag region.
		Frameless: true,

		// Tray app lifecycle: closing the window hides it (handled in OnBeforeClose)
		// rather than quitting, so the soundboard/hotkeys keep running. We manage
		// the hide ourselves to also keep the tray in sync, so HideWindowOnClose is
		// left false and OnBeforeClose returns prevent=true.
		HideWindowOnClose: false,

		AssetServer: &assetserver.Options{
			Assets: assets,
		},

		// Bind the App so Wails injects its exported methods into the webview as
		// window.go.main.App.<Method>. WITHOUT this the runtime never exposes the
		// bound methods, every frontend call() resolves to null, and the UI silently
		// falls back to its placeholder data — disconnected from the real backend.
		// This is the line that connects the design frontend to the live engine/
		// catalog/setup/config.
		Bind: []interface{}{app},

		// Match the design's dark, near-black backdrop behind the rounded shell.
		BackgroundColour: &options.RGBA{R: 13, G: 13, B: 15, A: 1},

		OnStartup: func(ctx context.Context) {
			app.startup(ctx)
		},
		// Close-to-tray: prevent the real close, hide the window instead. The tray
		// (or the in-window Quit) is the only path that truly exits.
		OnBeforeClose: func(ctx context.Context) (prevent bool) {
			// Wails calls this from inside Frontend.Quit() as well as on a real
			// close, and abandons the exit if we veto. So ask the App whether a
			// quit is already under way: if it is, let the close through or the
			// process can never exit.
			if !app.ShouldPreventClose() {
				return false
			}
			wailsruntime.WindowHide(ctx)
			return true
		},
		OnShutdown: func(ctx context.Context) {
			// Single shutdown choke point. Every exit path (tray Quit, in-window
			// Quit, OS-level close that escaped OnBeforeClose) unwinds through here.
			// Run the backend teardown FIRST — engine.Stop, restore the default mic,
			// save config — via the cleanup hook the backend registered with
			// app.OnCleanup (Backend.close, guarded to run exactly once), THEN stop
			// the tray goroutine so its message loop returns and the process can exit
			// cleanly.
			app.runCleanup()
			stopTray()
		},

		Windows: &windows.Options{
			// Dark titlebar/controls for the frameless edges; the page paints the rest.
			Theme: windows.Dark,
			// Keep the webview opaque (the page draws its own background); enabling
			// transparency/translucency (Mica/Acrylic) is a phase-2 polish option.
			WebviewIsTransparent: false,
			WindowIsTranslucent:  false,
		},
	})
	if err != nil {
		log.Fatalf("wails run: %v", err)
	}
}
