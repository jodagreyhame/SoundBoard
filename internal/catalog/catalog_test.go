package catalog

import (
	"os"
	"testing"
	"testing/fstest"
)

// TestNewBuildsTree verifies New() walks sounds/<category>/<file> correctly,
// skips .keep/dotfiles and unsupported extensions, and indexes by ID. This case
// is fully in-memory and hardware-free.
func TestNewBuildsTree(t *testing.T) {
	fsys := fstest.MapFS{
		"memes/airhorn.wav":      {Data: []byte("ignored-not-decoded")},
		"memes/sad-trombone.mp3": {Data: []byte("ignored")},
		"games/level_up.ogg":     {Data: []byte("ignored")},
		"games/.keep":            {Data: []byte("")},
		"games/readme.txt":       {Data: []byte("not audio")},
		".hidden/x.wav":          {Data: []byte("dotfile dir clip still under 2 parts? no")},
	}

	lib, err := New(fsys)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if len(lib.Categories) != 2 {
		t.Fatalf("want 2 categories, got %d: %+v", len(lib.Categories), lib.Categories)
	}
	// Sorted: games before memes.
	if lib.Categories[0].Name != "games" || lib.Categories[1].Name != "memes" {
		t.Fatalf("categories not sorted: %q, %q", lib.Categories[0].Name, lib.Categories[1].Name)
	}

	// memes has 2 supported clips; games has 1 (txt and .keep skipped).
	if got := len(lib.Categories[1].Clips); got != 2 {
		t.Fatalf("memes: want 2 clips, got %d", got)
	}
	if got := len(lib.Categories[0].Clips); got != 1 {
		t.Fatalf("games: want 1 clip, got %d", got)
	}

	// ID and Name shape.
	c := lib.Get("memes/sad-trombone")
	if c == nil {
		t.Fatal("Get(memes/sad-trombone) = nil")
	}
	if c.Category != "memes" || c.Path != "memes/sad-trombone.mp3" {
		t.Fatalf("clip fields wrong: %+v", c)
	}
	if c.Name != "sad trombone" {
		t.Fatalf("prettify: want %q, got %q", "sad trombone", c.Name)
	}

	// .keep must not become a clip.
	if lib.Get("games/") != nil {
		t.Fatal(".keep leaked into catalog")
	}
	if lib.Get("missing/clip") != nil {
		t.Fatal("Get(missing) should be nil")
	}
}

// TestLoadDecodesPCM verifies Load() decodes real wav+mp3 fixtures to
// non-empty interleaved float32 at 48k/2ch, exercising the resample path
// (44.1k -> 48k) and the native-48k path.
func TestLoadDecodesPCM(t *testing.T) {
	// The clip folder is the root of the FS handed to New, so point at the
	// directory that directly contains the category folders.
	fsys := os.DirFS("testdata/sounds")

	lib, err := New(fsys)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if len(lib.Categories) != 1 || lib.Categories[0].Name != "beeps" {
		t.Fatalf("unexpected tree: %+v", lib.Categories)
	}

	if err := lib.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}

	for _, id := range []string{"beeps/tone44k", "beeps/tone48k", "beeps/tone"} {
		c := lib.Get(id)
		if c == nil {
			t.Fatalf("missing clip %q", id)
		}
		if len(c.PCM) == 0 {
			t.Fatalf("%s: empty PCM", id)
		}
		// Interleaved stereo => even sample count.
		if len(c.PCM)%Channels != 0 {
			t.Fatalf("%s: PCM len %d not divisible by %d channels", id, len(c.PCM), Channels)
		}
		// All samples must be within [-1, 1].
		for i, v := range c.PCM {
			if v < -1 || v > 1 {
				t.Fatalf("%s: sample %d out of range: %v", id, i, v)
			}
		}
		// A 0.1s clip at 48k/2ch should be roughly 9600 frames.
		frames := len(c.PCM) / Channels
		if frames < 3000 {
			t.Fatalf("%s: only %d frames decoded, expected ~9600", id, frames)
		}
	}
}

// TestGetExtensionFallback verifies Get resolves both the canonical
// extensionless ID and an ID written with the source file extension, so a
// hotkey config in either form fires the clip.
func TestGetExtensionFallback(t *testing.T) {
	fsys := fstest.MapFS{
		"memes/airhorn.mp3":  {Data: []byte("ignored")},
		"games/level-up.wav": {Data: []byte("ignored")},
	}
	lib, err := New(fsys)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Canonical extensionless form.
	if c := lib.Get("memes/airhorn"); c == nil {
		t.Fatal("Get(memes/airhorn) = nil")
	}
	// With-extension form (as documented in older configs) resolves too.
	if c := lib.Get("memes/airhorn.mp3"); c == nil || c.ID != "memes/airhorn" {
		t.Fatalf("Get(memes/airhorn.mp3) failed to resolve: %+v", c)
	}
	if c := lib.Get("games/level-up.wav"); c == nil || c.ID != "games/level-up" {
		t.Fatalf("Get(games/level-up.wav) failed to resolve: %+v", c)
	}
	// A genuinely missing clip (even after stripping) is still nil.
	if c := lib.Get("memes/nope.mp3"); c != nil {
		t.Fatalf("Get(memes/nope.mp3) should be nil, got %+v", c)
	}
	// A wrong-extension form still resolves because we strip whatever ext is
	// present before falling back.
	if c := lib.Get("memes/airhorn.wav"); c == nil || c.ID != "memes/airhorn" {
		t.Fatalf("Get(memes/airhorn.wav) failed to resolve: %+v", c)
	}
}

// TestSampleRateConstants pins the canonical format.
func TestSampleRateConstants(t *testing.T) {
	if SampleRate != 48000 || Channels != 2 {
		t.Fatalf("canonical format changed: %d/%d", SampleRate, Channels)
	}
}
