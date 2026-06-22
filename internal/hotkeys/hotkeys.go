// Package hotkeys registers global system hotkeys via golang.design/x/hotkey
// (pure-Go user32 syscalls on Windows) and forwards triggered clip IDs.
//
// Register and the event pump run on a single runtime.LockOSThread goroutine.
package hotkeys

import (
	"fmt"
	"runtime"
	"strings"
	"sync"

	"golang.design/x/hotkey"
)

// binding pairs a registered hotkey with the clip it triggers.
type binding struct {
	hk     *hotkey.Hotkey
	clipID string
}

// Manager owns all registered hotkeys and the trigger callback.
type Manager struct {
	mu       sync.Mutex
	fn       func(clipID string)
	bindings []*binding
	done     chan struct{}
	closed   bool
}

// New creates an empty hotkey manager.
func New() *Manager {
	return &Manager{done: make(chan struct{})}
}

// OnTrigger sets the callback invoked (with the clip ID) when a hotkey fires.
func (m *Manager) OnTrigger(fn func(clipID string)) {
	m.mu.Lock()
	m.fn = fn
	m.mu.Unlock()
}

// Register parses a combo like "ctrl+alt+1" into modifiers + key, registers the
// hotkey, and arranges for fn(clipID) to be called when it fires. Registration
// errors (e.g. the combo is already owned by another app) are returned.
func (m *Manager) Register(combo string, clipID string) error {
	mods, key, err := parseCombo(combo)
	if err != nil {
		return fmt.Errorf("hotkeys: parse %q: %w", combo, err)
	}
	hk := hotkey.New(mods, key)
	if err := hk.Register(); err != nil {
		return fmt.Errorf("hotkeys: register %q: %w", combo, err)
	}
	m.mu.Lock()
	m.bindings = append(m.bindings, &binding{hk: hk, clipID: clipID})
	m.mu.Unlock()
	return nil
}

// Run starts the event pump. It runs the register + event pump on a single
// runtime.LockOSThread goroutine and returns immediately (the pump runs in the
// background). Each registered hotkey's Keydown channel is drained and the
// OnTrigger callback is invoked with the bound clip ID.
func (m *Manager) Run() {
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()

		for {
			m.mu.Lock()
			bindings := make([]*binding, len(m.bindings))
			copy(bindings, m.bindings)
			fn := m.fn
			m.mu.Unlock()

			// Fan-in: one lightweight reader per binding feeding a shared
			// channel, so newly registered hotkeys are picked up on the next
			// loop iteration after a fire.
			fired := make(chan string, len(bindings)+1)
			stop := make(chan struct{})
			var wg sync.WaitGroup
			for _, b := range bindings {
				wg.Add(1)
				go func(b *binding) {
					defer wg.Done()
					select {
					case <-b.hk.Keydown():
						select {
						case fired <- b.clipID:
						case <-stop:
						}
					case <-stop:
					}
				}(b)
			}

			select {
			case clipID := <-fired:
				if fn != nil {
					fn(clipID)
				}
				close(stop)
				wg.Wait()
				// Drain any other presses that landed in the buffer before we
				// tore the readers down, so simultaneous distinct hotkeys are
				// not dropped when the channel is recreated next iteration.
				drainFired(fired, fn)
			case <-m.done:
				close(stop)
				wg.Wait()
				return
			}
		}
	}()
}

// drainFired empties any presses still buffered in ch (after the readers have
// stopped) and dispatches each through fn, so simultaneous distinct hotkeys are
// not dropped when the channel is recreated on the next pump iteration.
func drainFired(ch <-chan string, fn func(clipID string)) {
	for {
		select {
		case clipID := <-ch:
			if fn != nil {
				fn(clipID)
			}
		default:
			return
		}
	}
}

// Close unregisters all hotkeys and stops the pump.
func (m *Manager) Close() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	close(m.done)
	bindings := m.bindings
	m.bindings = nil
	m.mu.Unlock()

	for _, b := range bindings {
		_ = b.hk.Unregister()
	}
}

// parseCombo turns a string like "ctrl+alt+1" or "ctrl+shift+f5" into the
// modifier set and key understood by golang.design/x/hotkey. Tokens are
// case-insensitive and separated by '+'. Exactly one non-modifier token (the
// key) is required.
func parseCombo(combo string) ([]hotkey.Modifier, hotkey.Key, error) {
	parts := strings.Split(combo, "+")
	var mods []hotkey.Modifier
	var key hotkey.Key
	haveKey := false
	seen := map[hotkey.Modifier]bool{}

	for _, raw := range parts {
		tok := strings.ToLower(strings.TrimSpace(raw))
		if tok == "" {
			return nil, 0, fmt.Errorf("empty token in combo")
		}
		if mod, ok := modifierFor(tok); ok {
			if seen[mod] {
				return nil, 0, fmt.Errorf("duplicate modifier %q", tok)
			}
			seen[mod] = true
			mods = append(mods, mod)
			continue
		}
		k, ok := keyFor(tok)
		if !ok {
			return nil, 0, fmt.Errorf("unknown token %q", tok)
		}
		if haveKey {
			return nil, 0, fmt.Errorf("more than one key in combo")
		}
		key = k
		haveKey = true
	}

	if !haveKey {
		return nil, 0, fmt.Errorf("no key in combo")
	}
	return mods, key, nil
}

// modifierFor maps a modifier token to its hotkey.Modifier.
func modifierFor(tok string) (hotkey.Modifier, bool) {
	switch tok {
	case "ctrl", "control":
		return hotkey.ModCtrl, true
	case "alt":
		return hotkey.ModAlt, true
	case "shift":
		return hotkey.ModShift, true
	case "win", "super", "meta", "cmd":
		return hotkey.ModWin, true
	}
	return 0, false
}

// keyFor maps a key token (digit 0-9, letter a-z, or f1-f12) to its hotkey.Key.
func keyFor(tok string) (hotkey.Key, bool) {
	if len(tok) == 1 {
		c := tok[0]
		switch {
		case c >= '0' && c <= '9':
			return hotkey.Key0 + hotkey.Key(c-'0'), true
		case c >= 'a' && c <= 'z':
			return hotkey.KeyA + hotkey.Key(c-'a'), true
		}
		return 0, false
	}
	if len(tok) >= 2 && tok[0] == 'f' {
		n := 0
		for _, ch := range tok[1:] {
			if ch < '0' || ch > '9' {
				return 0, false
			}
			n = n*10 + int(ch-'0')
		}
		if n >= 1 && n <= 12 {
			return hotkey.KeyF1 + hotkey.Key(n-1), true
		}
	}
	return 0, false
}
