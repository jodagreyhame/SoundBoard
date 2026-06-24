//go:build !fyne

// Command soundboard (Wails v2 build — the DEFAULT/shipping entrypoint).
//
// This is the migration target: the SoundBoard GUI rebuilt on Wails v2
// (Go + WebView2) with the new design, replacing the legacy Fyne UI. The Fyne
// entrypoint is preserved on disk behind the `fyne` build tag (main.go) and is
// no longer the default build.
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
// SKELETON phase: the App methods are stubbed (return zero/sample data) and the
// real backend (internal/audio, internal/setup, internal/config, …) is not yet
// wired. Phase 2 injects those dependencies WITHOUT changing any bound-method
// signature.
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
	app := NewApp()

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

		// Match the design's dark, near-black backdrop behind the rounded shell.
		BackgroundColour: &options.RGBA{R: 13, G: 13, B: 15, A: 1},

		OnStartup: func(ctx context.Context) {
			app.startup(ctx)
		},
		// Close-to-tray: prevent the real close, hide the window instead. The tray
		// (or the in-window Quit) is the only path that truly exits.
		OnBeforeClose: func(ctx context.Context) (prevent bool) {
			wailsruntime.WindowHide(ctx)
			return true
		},
		OnShutdown: func(ctx context.Context) {
			// Phase 2 runs the real cleanup here (engine.Stop, restore default mic,
			// save config). For now, tear the tray down so the goroutine exits.
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
