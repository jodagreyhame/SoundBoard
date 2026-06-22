// Package hotkeys registers global system hotkeys via golang.design/x/hotkey
// (pure-Go user32 syscalls on Windows) and forwards triggered clip IDs.
//
// Register and the event pump run on a single runtime.LockOSThread goroutine.
package hotkeys

import (
	"golang.design/x/hotkey"
)

// binding pairs a registered hotkey with the clip it triggers.
type binding struct {
	hk     *hotkey.Hotkey
	clipID string
}

// Manager owns all registered hotkeys and the trigger callback.
type Manager struct {
	fn       func(clipID string)
	bindings []*binding
}

// New creates an empty hotkey manager.
func New() *Manager {
	panic("todo")
}

// OnTrigger sets the callback invoked (with the clip ID) when a hotkey fires.
func (m *Manager) OnTrigger(fn func(clipID string)) {
	panic("todo")
}

// Register parses a combo like "ctrl+alt+1" into modifiers + key, registers the
// hotkey, and arranges for fn(clipID) to be called when it fires.
func (m *Manager) Register(combo string, clipID string) error {
	panic("todo")
}

// Run starts the event pump. It runs the register + event pump on a single
// runtime.LockOSThread goroutine and returns immediately (the pump runs in the
// background).
func (m *Manager) Run() {
	panic("todo")
}

// Close unregisters all hotkeys.
func (m *Manager) Close() {
	panic("todo")
}
