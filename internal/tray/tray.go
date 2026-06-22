// Package tray is the system-tray UI built on energye/systray. It renders the
// embedded sound library as category submenus, a monitor toggle, and quit.
package tray

import (
	_ "embed"

	// The tray UI is built on energye/systray; Run() wraps systray.Run.
	// Imported here so the module pins it.
	_ "github.com/energye/systray"

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

	onMonitorToggle func(bool)
	onQuit          func()
}

// New creates a tray UI bound to the library and player.
func New(lib *catalog.Library, player Player) *UI {
	panic("todo")
}

// OnMonitorToggle registers the callback fired when the user toggles the
// monitor menu item (true = enabled).
func (u *UI) OnMonitorToggle(fn func(bool)) {
	panic("todo")
}

// OnQuit registers the callback fired when the user selects Quit.
func (u *UI) OnQuit(fn func()) {
	panic("todo")
}

// Run wraps systray.Run: it builds category submenus from lib.Categories
// (one menu item per category, one sub-item per clip whose click calls
// player.Trigger(clip.ID)), plus a Monitor toggle and Quit. onReady is invoked
// once the tray is initialized. This blocks until the tray exits.
func (u *UI) Run(onReady func()) {
	panic("todo")
}
