package hotkeys

import (
	"testing"

	"golang.design/x/hotkey"
)

func modSet(mods []hotkey.Modifier) map[hotkey.Modifier]bool {
	m := make(map[hotkey.Modifier]bool, len(mods))
	for _, x := range mods {
		m[x] = true
	}
	return m
}

func TestParseComboValid(t *testing.T) {
	cases := []struct {
		combo    string
		wantMods []hotkey.Modifier
		wantKey  hotkey.Key
	}{
		{"ctrl+alt+1", []hotkey.Modifier{hotkey.ModCtrl, hotkey.ModAlt}, hotkey.Key1},
		{"ctrl+shift+f5", []hotkey.Modifier{hotkey.ModCtrl, hotkey.ModShift}, hotkey.KeyF5},
		{"a", nil, hotkey.KeyA},
		{"z", nil, hotkey.KeyZ},
		{"0", nil, hotkey.Key0},
		{"9", nil, hotkey.Key9},
		{"f12", nil, hotkey.KeyF12},
		{"f1", nil, hotkey.KeyF1},
		{"win+q", []hotkey.Modifier{hotkey.ModWin}, hotkey.KeyQ},
		{"CTRL+ALT+SHIFT+W", []hotkey.Modifier{hotkey.ModCtrl, hotkey.ModAlt, hotkey.ModShift}, hotkey.KeyW},
		{"control+ b ", []hotkey.Modifier{hotkey.ModCtrl}, hotkey.KeyB},
		{"super+5", []hotkey.Modifier{hotkey.ModWin}, hotkey.Key5},
	}
	for _, c := range cases {
		mods, key, err := parseCombo(c.combo)
		if err != nil {
			t.Errorf("parseCombo(%q) unexpected error: %v", c.combo, err)
			continue
		}
		if key != c.wantKey {
			t.Errorf("parseCombo(%q) key = %#x, want %#x", c.combo, key, c.wantKey)
		}
		got := modSet(mods)
		want := modSet(c.wantMods)
		if len(got) != len(want) {
			t.Errorf("parseCombo(%q) mods = %v, want %v", c.combo, mods, c.wantMods)
			continue
		}
		for m := range want {
			if !got[m] {
				t.Errorf("parseCombo(%q) missing modifier %v", c.combo, m)
			}
		}
	}
}

func TestParseComboInvalid(t *testing.T) {
	bad := []string{
		"",            // empty
		"ctrl",        // no key
		"ctrl+alt",    // no key
		"ctrl+1+2",    // two keys
		"a+b",         // two keys
		"ctrl++1",     // empty token
		"ctrl+x+",     // trailing empty token
		"foo",         // unknown token
		"f13",         // F-key out of range
		"f0",          // F-key out of range
		"ctrl+ctrl+a", // duplicate modifier
		"!",           // unknown key symbol
		"ab",          // not a single key / not f-key
	}
	for _, combo := range bad {
		if _, _, err := parseCombo(combo); err == nil {
			t.Errorf("parseCombo(%q) expected error, got nil", combo)
		}
	}
}
