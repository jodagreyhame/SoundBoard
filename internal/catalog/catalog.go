// Package catalog holds the sound-library domain model plus decode/cache.
//
// All audio in soundboard is float32 PCM, 48000 Hz, 2 channels interleaved.
// Clips are decoded and resampled to that canonical format at load time
// (never in the real-time callback) and cached in Clip.PCM.
package catalog

import (
	"encoding/binary"
	"fmt"
	"io"
	"io/fs"
	"path"
	"sort"
	"strings"
	"sync"

	"github.com/gopxl/beep/v2"
	"github.com/gopxl/beep/v2/flac"
	"github.com/gopxl/beep/v2/vorbis"
	"github.com/gopxl/beep/v2/wav"
	"github.com/hajimehoshi/go-mp3"
)

// Canonical audio format used everywhere in the app.
const (
	SampleRate = 48000
	Channels   = 2
)

// resampleQuality is beep's sinc resampler quality (higher = better, slower).
// Resampling happens once at load time, so we can afford a high setting.
const resampleQuality = 4

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

// Library is the in-memory catalog of all clips found under the sounds/ folder.
type Library struct {
	Categories []Category
	byID       map[string]*Clip

	fsys fs.FS

	// decMu guards lazy decode (EnsureDecoded). Decoding happens off the
	// real-time audio path, on whatever goroutine triggers a clip.
	decMu sync.Mutex
}

// supported maps a lowercase file extension (with dot) to whether catalog can
// decode it.
var supported = map[string]bool{
	".mp3":  true,
	".wav":  true,
	".flac": true,
	".ogg":  true,
}

// New walks the "sounds/<category>/<file>.<ext>" tree in fsys, builds the
// category/clip structure, and indexes clips by ID. It does NOT decode audio;
// call Load for that. Files named ".keep" and dotfiles are skipped.
func New(fsys fs.FS) (*Library, error) {
	l := &Library{
		byID: make(map[string]*Clip),
		fsys: fsys,
	}

	// Collect clips per category name so we can build ordered Category structs.
	byCat := make(map[string][]*Clip)

	err := fs.WalkDir(fsys, "sounds", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		name := d.Name()
		// Skip .keep markers and any dotfile.
		if strings.HasPrefix(name, ".") {
			return nil
		}
		ext := strings.ToLower(path.Ext(name))
		if !supported[ext] {
			return nil
		}
		// Expect exactly sounds/<category>/<file>. Anything shallower or
		// deeper than that is ignored.
		rel := strings.TrimPrefix(p, "sounds/")
		parts := strings.Split(rel, "/")
		if len(parts) != 2 {
			return nil
		}
		category := parts[0]
		// Skip clips under a hidden/dot category directory.
		if strings.HasPrefix(category, ".") {
			return nil
		}
		base := strings.TrimSuffix(name, path.Ext(name))

		clip := &Clip{
			ID:       category + "/" + base,
			Name:     prettify(base),
			Category: category,
			Path:     p,
		}
		byCat[category] = append(byCat[category], clip)
		l.byID[clip.ID] = clip
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("catalog: walk sounds: %w", err)
	}

	// Build Categories in stable (sorted) order, clips sorted by name.
	cats := make([]string, 0, len(byCat))
	for c := range byCat {
		cats = append(cats, c)
	}
	sort.Strings(cats)
	for _, c := range cats {
		clips := byCat[c]
		sort.Slice(clips, func(i, j int) bool { return clips[i].ID < clips[j].ID })
		l.Categories = append(l.Categories, Category{Name: c, Clips: clips})
	}
	return l, nil
}

// EnsureDecoded decodes and caches a clip's PCM on first use and returns it.
// Subsequent calls return the cached PCM. It is safe for concurrent callers and
// MUST NOT be called from the real-time audio callback — call it from the
// (non-RT) goroutine that triggers a clip, before handing the clip to the
// engine. Decoding the whole library up front is avoided so startup is instant
// and idle memory stays proportional to the clips actually played.
func (l *Library) EnsureDecoded(clip *Clip) ([]float32, error) {
	l.decMu.Lock()
	defer l.decMu.Unlock()
	if clip.PCM != nil {
		return clip.PCM, nil
	}
	pcm, err := l.decodeClip(clip)
	if err != nil {
		return nil, fmt.Errorf("catalog: decode %q: %w", clip.Path, err)
	}
	clip.PCM = pcm
	return pcm, nil
}

// Load eagerly decodes every clip to float32/48k/2ch (resampling at load time)
// and fills each Clip.PCM. It is optional — the app decodes lazily via
// EnsureDecoded — but is kept for callers/tests that want everything in memory.
// mp3 via go-mp3; wav/flac/ogg via beep/v2/{wav,flac,vorbis}.
func (l *Library) Load() error {
	for _, cat := range l.Categories {
		for _, clip := range cat.Clips {
			pcm, err := l.decodeClip(clip)
			if err != nil {
				return fmt.Errorf("catalog: load %q: %w", clip.Path, err)
			}
			clip.PCM = pcm
		}
	}
	return nil
}

// Get returns the clip with the given ID, or nil if not present. Clip IDs are
// canonically extensionless ("<category>/<basename>"), but a configured hotkey
// may reference a clip WITH its file extension ("memes/airhorn.mp3"). To make
// both forms resolve, Get falls back to stripping a trailing extension when the
// exact ID is not found.
func (l *Library) Get(id string) *Clip {
	if c, ok := l.byID[id]; ok {
		return c
	}
	if ext := path.Ext(id); ext != "" {
		if c, ok := l.byID[strings.TrimSuffix(id, ext)]; ok {
			return c
		}
	}
	return nil
}

// decodeClip opens, decodes, and resamples a single clip to interleaved
// float32 at SampleRate/Channels.
func (l *Library) decodeClip(clip *Clip) ([]float32, error) {
	f, err := l.fsys.Open(clip.Path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var (
		stream beep.Streamer
		format beep.Format
	)

	ext := strings.ToLower(path.Ext(clip.Path))
	switch ext {
	case ".mp3":
		stream, format, err = decodeMP3(f)
	case ".wav":
		stream, format, err = wav.Decode(f)
	case ".flac":
		stream, format, err = flac.Decode(f)
	case ".ogg":
		// vorbis.Decode wants an io.ReadCloser; fs.File satisfies it.
		stream, format, err = vorbis.Decode(f)
	default:
		return nil, fmt.Errorf("unsupported extension %q", ext)
	}
	if err != nil {
		return nil, err
	}

	// Resample to the canonical rate if the source differs.
	if int(format.SampleRate) != SampleRate {
		stream = beep.Resample(resampleQuality, format.SampleRate, SampleRate, stream)
	}

	return drainToFloat32(stream)
}

// drainToFloat32 pulls every sample from a beep.Streamer (always [2]float64)
// and returns interleaved float32 at Channels channels, clamped to [-1, 1].
func drainToFloat32(s beep.Streamer) ([]float32, error) {
	const block = 4096
	buf := make([][2]float64, block)
	out := make([]float32, 0, block*Channels)
	for {
		n, ok := s.Stream(buf)
		for i := 0; i < n; i++ {
			out = append(out, clampF32(buf[i][0]), clampF32(buf[i][1]))
		}
		if !ok {
			break
		}
	}
	if streamer, isErrer := s.(interface{ Err() error }); isErrer {
		if err := streamer.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func clampF32(v float64) float32 {
	if v > 1 {
		v = 1
	} else if v < -1 {
		v = -1
	}
	return float32(v)
}

// prettify turns a file basename into a display name: replaces underscores and
// dashes with spaces and trims surrounding whitespace.
func prettify(base string) string {
	r := strings.NewReplacer("_", " ", "-", " ")
	return strings.TrimSpace(r.Replace(base))
}

// --- mp3 support via hajimehoshi/go-mp3 -------------------------------------

// mp3Streamer adapts a go-mp3 decoder (16-bit LE stereo PCM) to a
// beep.Streamer of [2]float64 at the decoder's native sample rate.
type mp3Streamer struct {
	dec *mp3.Decoder
	err error
	buf []byte // scratch for raw PCM bytes
}

// decodeMP3 wraps a go-mp3 decoder as a beep.Streamer + beep.Format.
func decodeMP3(r io.Reader) (beep.Streamer, beep.Format, error) {
	dec, err := mp3.NewDecoder(r)
	if err != nil {
		return nil, beep.Format{}, err
	}
	format := beep.Format{
		SampleRate:  beep.SampleRate(dec.SampleRate()),
		NumChannels: 2, // go-mp3 always emits 2 channels.
		Precision:   2, // 16-bit samples.
	}
	return &mp3Streamer{dec: dec}, format, nil
}

func (m *mp3Streamer) Stream(samples [][2]float64) (int, bool) {
	if m.err != nil {
		return 0, false
	}
	// Each output sample is 2 channels * 2 bytes = 4 bytes.
	need := len(samples) * 4
	if cap(m.buf) < need {
		m.buf = make([]byte, need)
	}
	p := m.buf[:need]

	// go-mp3's Read may return short reads; fill as much as we can.
	got := 0
	for got < need {
		n, err := m.dec.Read(p[got:])
		got += n
		if err == io.EOF {
			break
		}
		if err != nil {
			m.err = err
			break
		}
		if n == 0 {
			break
		}
	}

	// Whole frames only; ignore any trailing partial frame.
	frames := got / 4
	for i := 0; i < frames; i++ {
		off := i * 4
		left := int16(binary.LittleEndian.Uint16(p[off : off+2]))
		right := int16(binary.LittleEndian.Uint16(p[off+2 : off+4]))
		samples[i][0] = float64(left) / 32768.0
		samples[i][1] = float64(right) / 32768.0
	}

	ok := frames > 0
	return frames, ok
}

func (m *mp3Streamer) Err() error { return m.err }
