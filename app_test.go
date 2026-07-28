// app_test.go exercises the Wails-bound App's read path (GetState) against a
// fake, hardware-free backend: an in-memory catalog built from an fstest.MapFS
// and a hand-built config.Settings. No audio context, engine, or real devices
// are constructed, so the test runs anywhere `go test` runs. It pins that
// GetState assembles categories/clips from the catalog, favourites/volumes/
// per-clip/processing from settings, the routing snapshot from the controller,
// and the theme (defaulting to dark when unset).
package main

import (
	"testing"
	"testing/fstest"

	"github.com/jodagreyhame/SoundBoard/internal/catalog"
	"github.com/jodagreyhame/SoundBoard/internal/config"
	"github.com/jodagreyhame/SoundBoard/internal/setup"
)

// fakeBackend builds a Backend with a fake catalog + the given settings and an
// absent-cable routing controller, with NO audio context/engine. Only the
// fields GetState reads (settings, lib, setup) are populated.
func fakeBackend(t *testing.T, fsys fstest.MapFS, s *config.Settings) *Backend {
	t.Helper()
	lib, err := catalog.New(fsys)
	if err != nil {
		t.Fatalf("catalog.New: %v", err)
	}
	return &Backend{
		settings: s,
		lib:      lib,
		setup:    &routingController{status: setup.Status{}}, // cable absent
	}
}

// newAppWithBackend constructs a bare App (no Wails runtime) wired to b. It does
// NOT call startup, so no events goroutine runs and ctx stays nil — GetState and
// the other read/setter methods are all safe to call directly.
func newAppWithBackend(b *Backend) *App {
	a := NewApp()
	a.backend = b
	return a
}

// TestGetStateAssemblesFromCatalog verifies the full assembly: categories sorted
// with correct counts, clips carrying prettified names + favourite flags, the
// favourites list preserved in order, volumes/per-clip/processing from settings,
// theme defaulting to dark, and routing reported absent.
func TestGetStateAssemblesFromCatalog(t *testing.T) {
	fsys := fstest.MapFS{
		"memes/airhorn.wav":      {Data: []byte("x")},
		"memes/sad-trombone.mp3": {Data: []byte("x")},
		"games/level_up.ogg":     {Data: []byte("x")},
		"games/.keep":            {Data: []byte("")},
	}
	s := &config.Settings{
		Favorites: []string{"memes/airhorn"},
		Volumes: config.Volumes{
			Master:  config.FloatPtr(0.5),
			PerClip: map[string]float32{"memes/airhorn": 1.25},
		},
		Processing: config.AudioProcessing{
			MicMode:         config.MicModePTT,
			GateSensitivity: 0.42,
			AGC:             true,
			MonitorSource:   config.MonitorSourceTransmitted,
		},
	}
	app := newAppWithBackend(fakeBackend(t, fsys, s))

	st := app.GetState()

	// Theme defaults to dark when unset.
	if st.Theme != "dark" {
		t.Errorf("theme = %q, want dark (default)", st.Theme)
	}

	// Categories: sorted games, memes with correct counts (.keep skipped).
	if len(st.Categories) != 2 {
		t.Fatalf("categories = %d, want 2: %+v", len(st.Categories), st.Categories)
	}
	if st.Categories[0].Name != "games" || st.Categories[0].Count != 1 {
		t.Errorf("categories[0] = %+v, want {games 1}", st.Categories[0])
	}
	if st.Categories[1].Name != "memes" || st.Categories[1].Count != 2 {
		t.Errorf("categories[1] = %+v, want {memes 2}", st.Categories[1])
	}

	// Clips: 3 total, each with prettified name + correct favourite flag.
	if len(st.Clips) != 3 {
		t.Fatalf("clips = %d, want 3: %+v", len(st.Clips), st.Clips)
	}
	byID := map[string]Clip{}
	for _, c := range st.Clips {
		byID[c.ID] = c
	}
	airhorn, ok := byID["memes/airhorn"]
	if !ok {
		t.Fatal("clip memes/airhorn missing")
	}
	if airhorn.Name != "airhorn" || airhorn.Category != "memes" || !airhorn.Favorite {
		t.Errorf("memes/airhorn = %+v, want name=airhorn category=memes favorite=true", airhorn)
	}
	if trombone := byID["memes/sad-trombone"]; trombone.Name != "sad trombone" || trombone.Favorite {
		t.Errorf("memes/sad-trombone = %+v, want name=%q favorite=false", trombone, "sad trombone")
	}

	// Favourites list preserved in order.
	if len(st.Favorites) != 1 || st.Favorites[0] != "memes/airhorn" {
		t.Errorf("favorites = %v, want [memes/airhorn]", st.Favorites)
	}

	// Volumes: explicit Master 0.5, Mic/Monitor default to unity.
	if st.Volumes.Master != 0.5 {
		t.Errorf("volumes.master = %v, want 0.5", st.Volumes.Master)
	}
	if st.Volumes.Mic != 1 || st.Volumes.Monitor != 1 {
		t.Errorf("volumes mic/monitor = %v/%v, want 1/1 (unity default)", st.Volumes.Mic, st.Volumes.Monitor)
	}

	// Per-clip multiplier carried through as float64.
	if st.PerClip["memes/airhorn"] != 1.25 {
		t.Errorf("perClip[memes/airhorn] = %v, want 1.25", st.PerClip["memes/airhorn"])
	}

	// Processing suite carried from settings.
	if st.Audio.MicMode != config.MicModePTT {
		t.Errorf("audio.micMode = %q, want %q", st.Audio.MicMode, config.MicModePTT)
	}
	// GateSensitivity is stored as float32 and widened to float64 in the snapshot,
	// so compare with a tolerance rather than exact equality (0.42 is not exactly
	// representable in float32).
	if d := st.Audio.GateSensitivity - 0.42; d > 1e-6 || d < -1e-6 {
		t.Errorf("audio.gateSensitivity = %v, want ~0.42", st.Audio.GateSensitivity)
	}
	if !st.Audio.AGC || st.Audio.NoiseSuppression || st.Audio.Ducking || st.Audio.ForceThrough {
		t.Errorf("audio toggles = %+v, want only AGC true", st.Audio)
	}
	// MonitorSource (the confidence-monitor setting) carried through to the snapshot.
	if st.Audio.MonitorSource != config.MonitorSourceTransmitted {
		t.Errorf("audio.monitorSource = %q, want %q", st.Audio.MonitorSource, config.MonitorSourceTransmitted)
	}

	// Routing: cable absent.
	if st.Routing.State != "absent" || st.Routing.CanEngage {
		t.Errorf("routing = %+v, want state=absent canEngage=false", st.Routing)
	}
}

// TestGetStateEmptyLibrary verifies an EMPTY catalog yields non-nil empty slices
// /maps (never JSON null), so the frontend can iterate without guards.
func TestGetStateEmptyLibrary(t *testing.T) {
	fsys := fstest.MapFS{".keep": {Data: []byte("")}}
	app := newAppWithBackend(fakeBackend(t, fsys, &config.Settings{}))

	st := app.GetState()

	if st.Categories == nil {
		t.Error("categories is nil, want empty non-nil slice")
	}
	if st.Clips == nil {
		t.Error("clips is nil, want empty non-nil slice")
	}
	if st.Favorites == nil {
		t.Error("favorites is nil, want empty non-nil slice")
	}
	if st.PerClip == nil {
		t.Error("perClip is nil, want empty non-nil map")
	}
	if len(st.Categories) != 0 || len(st.Clips) != 0 {
		t.Errorf("empty library should yield 0 categories/clips, got %d/%d", len(st.Categories), len(st.Clips))
	}
	// Unset volumes default to unity via the config accessors.
	if st.Volumes.Mic != 1 || st.Volumes.Master != 1 || st.Volumes.Monitor != 1 {
		t.Errorf("unset volumes = %+v, want all unity", st.Volumes)
	}
}

// TestGetStateNilBackend verifies the bare-App fallback returns a well-formed,
// non-nil snapshot rather than panicking (used by tooling/preview paths).
func TestGetStateNilBackend(t *testing.T) {
	app := NewApp() // no backend injected
	st := app.GetState()
	if st.Theme != "dark" {
		t.Errorf("theme = %q, want dark", st.Theme)
	}
	if st.Categories == nil || st.Clips == nil || st.Favorites == nil || st.PerClip == nil {
		t.Error("nil-backend snapshot must have non-nil empty slices/maps")
	}
	if st.Routing.State != "absent" {
		t.Errorf("routing.state = %q, want absent", st.Routing.State)
	}
}

// Quitting must run the backend teardown exactly once, no matter how many quit
// requests arrive. That teardown is what stops the engine, RESTORES THE USER'S
// DEFAULT MICROPHONE and saves settings, so skipping or double-running it is
// user-visible: the mic stays hijacked to the virtual cable after exit.
//
// Tray Quit and the in-window Quit button can fire together, hence quitOnce.
func TestQuitRunsCleanupExactlyOnce(t *testing.T) {
	app := NewApp()

	var calls int
	app.OnCleanup(func() { calls++ })

	// No Wails context in a unit test, so Quit takes the direct cleanup path.
	app.Quit()
	app.Quit()

	if calls != 1 {
		t.Fatalf("cleanup ran %d times, want exactly 1 (the mic restore must run, and must not run twice)", calls)
	}
}
