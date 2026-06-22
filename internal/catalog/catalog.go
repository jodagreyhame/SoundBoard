// Package catalog holds the sound-library domain model plus decode/cache.
//
// All audio in soundboard is float32 PCM, 48000 Hz, 2 channels interleaved.
// Clips are decoded and resampled to that canonical format at load time
// (never in the real-time callback) and cached in Clip.PCM.
package catalog

import (
	"io/fs"

	// Decoders used by Load(). Imported here so the module pins them; the
	// implementation will use these packages directly.
	_ "github.com/gopxl/beep/v2"
	_ "github.com/gopxl/beep/v2/flac"
	_ "github.com/gopxl/beep/v2/vorbis"
	_ "github.com/gopxl/beep/v2/wav"
	_ "github.com/hajimehoshi/go-mp3"
)

// Canonical audio format used everywhere in the app.
const (
	SampleRate = 48000
	Channels   = 2
)

// Clip is a single playable sound. PCM is filled by Library.Load() with
// interleaved float32 samples at SampleRate/Channels.
type Clip struct {
	ID       string // "<category>/<filename>"
	Name     string // display name (filename without extension)
	Category string // category directory name
	Path     string // path within the embedded fs, e.g. "sounds/memes/x.mp3"
	PCM      []float32
}

// Category groups clips that share a sounds/<category>/ directory.
type Category struct {
	Name  string
	Clips []*Clip
}

// Library is the in-memory catalog of all embedded clips.
type Library struct {
	Categories []Category
	byID       map[string]*Clip
}

// New walks the "sounds/<category>/<file>.<ext>" tree in fsys, builds the
// category/clip structure, and indexes clips by ID. It does NOT decode audio;
// call Load for that. Files named ".keep" are skipped.
func New(fsys fs.FS) (*Library, error) {
	panic("todo")
}

// Load decodes every clip to float32/48k/2ch, resampling at load time, and
// fills each Clip.PCM. mp3 via go-mp3 (or beep/v2/mp3); wav/flac/ogg via
// beep/v2/{wav,flac,vorbis}. ".keep" files are skipped.
func (l *Library) Load() error {
	panic("todo")
}

// Get returns the clip with the given ID, or nil if not present.
func (l *Library) Get(id string) *Clip {
	panic("todo")
}
