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
	// Volumes default to unity gain with a non-nil PerClip map so the in-app
	// mixer starts at full volume and is never nil-dereferenced.
	if s.Volumes.Mic != 1 {
		t.Errorf("Load() defaults: Volumes.Mic = %v, want 1", s.Volumes.Mic)
	}
	if s.Volumes.Master != 1 {
		t.Errorf("Load() defaults: Volumes.Master = %v, want 1", s.Volumes.Master)
	}
	if s.Volumes.PerClip == nil {
		t.Error("Load() defaults: Volumes.PerClip should be initialized, got nil")
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	pointConfigDir(t)

	want := &Settings{
		MicName:     "Microphone (Realtek)",
		CableName:   "CABLE Input (VB-Audio Virtual Cable)",
		MonitorName: "Speakers (Realtek)",
		Monitor:     true,
		// Clip IDs are canonically extensionless ("<category>/<basename>");
		// see catalog.Library.Get, which also accepts an extension suffix.
		Hotkeys: map[string]string{
			"ctrl+alt+1": "memes/airhorn",
			"ctrl+alt+2": "games/level-up",
		},
		Volumes: Volumes{
			Mic:    0.8,
			Master: 0.5,
			PerClip: map[string]float32{
				"memes/airhorn": 1.25,
			},
		},
		Window: WindowPrefs{Width: 800, Height: 600, Maximized: true},
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

func TestLoadNormalizesPartialVolumes(t *testing.T) {
	pointConfigDir(t)

	// A saved config that set only Master (e.g. an older write path) must load
	// back with Mic defaulted to unity, a non-nil PerClip map, and the stored
	// Master preserved.
	stored := &Settings{
		Hotkeys: map[string]string{},
		Volumes: Volumes{Master: 0.3},
	}
	if err := stored.Save(); err != nil {
		t.Fatalf("Save() error: %v", err)
	}

	got, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if got.Volumes.Mic != 1 {
		t.Errorf("Volumes.Mic = %v, want 1 (defaulted)", got.Volumes.Mic)
	}
	if got.Volumes.Master != 0.3 {
		t.Errorf("Volumes.Master = %v, want 0.3 (preserved)", got.Volumes.Master)
	}
	if got.Volumes.PerClip == nil {
		t.Error("Volumes.PerClip is nil, want initialized empty map")
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
	// Load normalizes Volumes to unity defaults; mirror that on the expected
	// value so the structural comparison reflects what a fresh Load produces.
	second.normalize()
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
