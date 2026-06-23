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

	// ptt is the optional push-to-talk binding registered via RegisterPTT. It is
	// driven by its OWN dedicated goroutine (pttLoop) reading both Keydown and
	// Keyup, so a HELD key produces a down->up pair without disturbing the
	// clip-trigger pump (which only cares about Keydown). nil when no PTT is set.
	ptt *pttBinding
}

// pttBinding holds the push-to-talk hotkey and its down/up callbacks. Unlike the
// trigger bindings, the PTT key needs key-UP too (to close the mic gate when the
// user releases it), so it has its own long-lived reader goroutine.
type pttBinding struct {
	hk     *hotkey.Hotkey
	onDown func()
	onUp   func()
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

// RegisterPTT registers a push-to-talk hotkey from a combo like "ctrl+grave" and
// arranges for onDown to be called when the key is pressed and onUp when it is
// released. It uses REAL key up/down: golang.design/x/hotkey exposes a Keyup
// channel, and on Windows it is synthesized by polling GetAsyncKeyState while the
// hotkey is held, so a held key reliably yields a down THEN an up. The PTT binding
// runs on its OWN goroutine, independent of the clip-trigger pump, so holding the
// key does not interfere with (or get torn down by) clip hotkeys.
//
// At most one PTT binding exists; calling RegisterPTT again replaces the previous
// one (the old hotkey is unregistered). An empty combo unregisters any PTT binding
// and returns nil. Registration errors (e.g. the combo is already owned) are
// returned and leave any prior PTT binding in place.
//
// NOTE on approximation: the Windows backend derives key-up from AsyncKeyState
// polling at 100Hz, so an extremely brief tap may coalesce; for push-to-talk
// (held while speaking) this is not a concern. This is real up/down, not a
// hold-with-timeout hack.
func (m *Manager) RegisterPTT(combo string, onDown, onUp func()) error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return fmt.Errorf("hotkeys: manager closed")
	}
	m.mu.Unlock()

	// Empty combo => clear any existing PTT binding.
	if strings.TrimSpace(combo) == "" {
		m.clearPTT()
		return nil
	}

	mods, key, err := parseCombo(combo)
	if err != nil {
		return fmt.Errorf("hotkeys: parse PTT %q: %w", combo, err)
	}
	hk := hotkey.New(mods, key)
	if err := hk.Register(); err != nil {
		return fmt.Errorf("hotkeys: register PTT %q: %w", combo, err)
	}

	// Swap in the new binding, unregistering and stopping the old one first.
	m.clearPTT()
	pb := &pttBinding{hk: hk, onDown: onDown, onUp: onUp}
	m.mu.Lock()
	m.ptt = pb
	done := m.done
	m.mu.Unlock()

	go m.pttLoop(pb, done)
	return nil
}

// pttLoop drives one PTT binding: it blocks on Keydown (fires onDown) then on the
// matching Keyup (fires onUp), looping until the hotkey is unregistered (its
// channels close) or the manager is closed. It never holds m.mu while invoking a
// callback, so a callback may safely call back into the engine.
func (m *Manager) pttLoop(pb *pttBinding, done <-chan struct{}) {
	for {
		select {
		case _, ok := <-pb.hk.Keydown():
			if !ok {
				return // hotkey unregistered
			}
			if pb.onDown != nil {
				pb.onDown()
			}
			// Wait for the release (or shutdown) before accepting the next press,
			// so onDown/onUp always pair up and the gate cannot get stuck open.
			select {
			case _, ok := <-pb.hk.Keyup():
				if !ok {
					return
				}
				if pb.onUp != nil {
					pb.onUp()
				}
			case <-done:
				if pb.onUp != nil {
					pb.onUp() // ensure the mic is not left open on shutdown
				}
				return
			}
		case <-done:
			return
		}
	}
}

// clearPTT unregisters and forgets any current PTT binding. The pttLoop goroutine
// observes the hotkey's channels closing (Unregister) and returns on its own.
func (m *Manager) clearPTT() {
	m.mu.Lock()
	pb := m.ptt
	m.ptt = nil
	m.mu.Unlock()
	if pb != nil {
		_ = pb.hk.Unregister()
	}
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
	ptt := m.ptt
	m.ptt = nil
	m.mu.Unlock()

	for _, b := range bindings {
		_ = b.hk.Unregister()
	}
	// Unregister the PTT hotkey too; its pttLoop goroutine returns on close(done).
	if ptt != nil {
		_ = ptt.hk.Unregister()
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
