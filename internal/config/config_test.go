package config

import (
	"os"
	"reflect"
	"runtime"
	"testing"
)

// pointConfigDir points os.UserConfigDir() at a temporary directory by setting
// the platform-appropriate environment variable for the duration of the test.
func pointConfigDir(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	switch runtime.GOOS {
	case "windows":
		t.Setenv("APPDATA", tmp)
	case "darwin":
		t.Setenv("HOME", tmp)
	default:
		t.Setenv("XDG_CONFIG_HOME", tmp)
	}
	return tmp
}

func TestLoadMissingReturnsDefaults(t *testing.T) {
	pointConfigDir(t)

	s, err := Load()
	if err != nil {
		t.Fatalf("Load() of missing file returned error: %v", err)
	}
	if s == nil {
		t.Fatal("Load() returned nil Settings")
	}
	if s.Hotkeys == nil {
		t.Error("Load() defaults: Hotkeys map should be initialized, got nil")
	}
	if s.MicName != "" || s.CableName != "" || s.MonitorName != "" || s.Monitor {
		t.Errorf("Load() defaults: expected zero-value fields, got %+v", s)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	pointConfigDir(t)

	want := &Settings{
		MicName:     "Microphone (Realtek)",
		CableName:   "CABLE Input (VB-Audio Virtual Cable)",
		MonitorName: "Speakers (Realtek)",
		Monitor:     true,
		Hotkeys: map[string]string{
			"ctrl+alt+1": "memes/airhorn.mp3",
			"ctrl+alt+2": "games/level-up.wav",
		},
	}

	if err := want.Save(); err != nil {
		t.Fatalf("Save() error: %v", err)
	}

	got, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if !reflect.DeepEqual(want, got) {
		t.Errorf("round-trip mismatch:\n want %+v\n got  %+v", want, got)
	}
}

func TestSaveOverwriteAtomic(t *testing.T) {
	pointConfigDir(t)

	first := &Settings{MicName: "mic-a", Hotkeys: map[string]string{"ctrl+1": "a"}}
	if err := first.Save(); err != nil {
		t.Fatalf("first Save() error: %v", err)
	}

	second := &Settings{MicName: "mic-b", Hotkeys: map[string]string{"ctrl+2": "b"}}
	if err := second.Save(); err != nil {
		t.Fatalf("second Save() error: %v", err)
	}

	got, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if !reflect.DeepEqual(second, got) {
		t.Errorf("overwrite mismatch:\n want %+v\n got  %+v", second, got)
	}

	// Ensure no stray temp files were left behind in the config dir.
	d, err := dir()
	if err != nil {
		t.Fatalf("dir() error: %v", err)
	}
	entries, err := os.ReadDir(d)
	if err != nil {
		t.Fatalf("ReadDir error: %v", err)
	}
	for _, e := range entries {
		if e.Name() != configFile {
			t.Errorf("unexpected leftover file in config dir: %s", e.Name())
		}
	}
}
