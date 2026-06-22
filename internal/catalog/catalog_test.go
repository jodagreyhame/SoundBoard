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
		"sounds/memes/airhorn.wav":      {Data: []byte("ignored-not-decoded")},
		"sounds/memes/sad-trombone.mp3": {Data: []byte("ignored")},
		"sounds/games/level_up.ogg":     {Data: []byte("ignored")},
		"sounds/games/.keep":            {Data: []byte("")},
		"sounds/games/readme.txt":       {Data: []byte("not audio")},
		"sounds/.hidden/x.wav":          {Data: []byte("dotfile dir clip still under 2 parts? no")},
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
	if c.Category != "memes" || c.Path != "sounds/memes/sad-trombone.mp3" {
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
	fsys := os.DirFS("testdata")

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

// TestSampleRateConstants pins the canonical format.
func TestSampleRateConstants(t *testing.T) {
	if SampleRate != 48000 || Channels != 2 {
		t.Fatalf("canonical format changed: %d/%d", SampleRate, Channels)
	}
}
