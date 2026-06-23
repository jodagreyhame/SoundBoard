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

// TestSetPTTParseError confirms SetPTT with an invalid combo returns an error and
// installs no binding. The unified OnPTT callback is set first to exercise the
// fn!=nil branch in SetPTT, but the parse failure happens before any OS
// registration, so this stays deterministic (no real key loop).
func TestSetPTTParseError(t *testing.T) {
	m := New()
	defer m.Close()

	var got []bool
	m.OnPTT(func(down bool) { got = append(got, down) })

	if err := m.SetPTT("ctrl+nope"); err == nil {
		t.Fatal("SetPTT with an invalid combo should return an error")
	}
	m.mu.Lock()
	ptt := m.ptt
	fn := m.pttFn
	m.mu.Unlock()
	if ptt != nil {
		t.Fatal("a failed SetPTT must not leave a PTT binding installed")
	}
	if fn == nil {
		t.Fatal("OnPTT callback must remain set after a failed SetPTT")
	}
	if len(got) != 0 {
		t.Fatalf("no callback should fire on a parse failure, got %v", got)
	}
}

// TestSetPTTEmptyClears confirms SetPTT("") clears any binding and returns nil,
// without touching OS registration.
func TestSetPTTEmptyClears(t *testing.T) {
	m := New()
	defer m.Close()

	m.OnPTT(func(down bool) {})
	if err := m.SetPTT(""); err != nil {
		t.Fatalf("SetPTT(\"\") should clear and return nil, got %v", err)
	}
	m.mu.Lock()
	ptt := m.ptt
	m.mu.Unlock()
	if ptt != nil {
		t.Fatal("SetPTT(\"\") must leave no PTT binding")
	}
}

// TestSetPTTAfterCloseErrors confirms SetPTT on a closed manager fails fast.
func TestSetPTTAfterCloseErrors(t *testing.T) {
	m := New()
	m.Close()
	m.OnPTT(func(down bool) {})
	if err := m.SetPTT("ctrl+grave"); err == nil {
		t.Fatal("SetPTT on a closed manager should return an error")
	}
}

// TestOnPTTNilCallbackSafe confirms SetPTT with no OnPTT callback set still parses
// (and rejects a bad combo) without panicking on the nil fn path.
func TestOnPTTNilCallbackSafe(t *testing.T) {
	m := New()
	defer m.Close()
	if err := m.SetPTT("definitely+not+valid+combo+x+y"); err == nil {
		t.Fatal("SetPTT with an invalid combo should error even with a nil callback")
	}
}
