package hotkeys

import "testing"

// TestRegisterPTTParseError confirms an invalid combo returns an error and does
// NOT install a PTT binding (so a typo in the config never half-registers a key).
// This path never touches the OS hotkey registration, so it is deterministic.
func TestRegisterPTTParseError(t *testing.T) {
	m := New()
	defer m.Close()

	if err := m.RegisterPTT("not a combo", func() {}, func() {}); err == nil {
		t.Fatal("RegisterPTT with an invalid combo should return an error")
	}
	m.mu.Lock()
	ptt := m.ptt
	m.mu.Unlock()
	if ptt != nil {
		t.Fatal("a failed RegisterPTT must not leave a PTT binding installed")
	}
}

// TestRegisterPTTEmptyClears confirms an empty combo clears any PTT binding and
// returns nil. With no prior binding it is a harmless no-op; the deterministic
// part (no OS registration) is what we assert here.
func TestRegisterPTTEmptyClears(t *testing.T) {
	m := New()
	defer m.Close()

	if err := m.RegisterPTT("", func() {}, func() {}); err != nil {
		t.Fatalf("RegisterPTT(\"\") should clear and return nil, got %v", err)
	}
	m.mu.Lock()
	ptt := m.ptt
	m.mu.Unlock()
	if ptt != nil {
		t.Fatal("RegisterPTT(\"\") must leave no PTT binding")
	}
}

// TestRegisterPTTAfterCloseErrors confirms registering a PTT on a closed manager
// fails fast rather than spinning up an orphan goroutine.
func TestRegisterPTTAfterCloseErrors(t *testing.T) {
	m := New()
	m.Close()
	if err := m.RegisterPTT("ctrl+grave", func() {}, func() {}); err == nil {
		t.Fatal("RegisterPTT on a closed manager should return an error")
	}
}

// TestCloseWithoutPTTSafe confirms Close is safe when no PTT was ever registered
// (the nil-PTT branch in Close).
func TestCloseWithoutPTTSafe(t *testing.T) {
	m := New()
	m.Close() // must not panic with m.ptt == nil
}
