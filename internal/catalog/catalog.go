// Package catalog holds the sound-library domain model plus decode/cache.
//
// All audio in soundboard is float32 PCM, 48000 Hz, 2 channels interleaved.
// Clips are decoded and resampled to that canonical format at load time
// (never in the real-time callback) and cached in Clip.PCM.
package catalog

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"io/fs"
	"log"
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

// Empty returns a valid, empty Library.
//
// Used when the clip folder cannot be read at all, so the app starts with an
// explained empty grid instead of crashing. It carries no clips, so the nil
// fsys is never dereferenced — decodeClip is only reachable through a Clip.
func Empty() *Library {
	return &Library{byID: make(map[string]*Clip)}
}

// New walks the "<category>/<file>.<ext>" tree at the root of fsys, builds the
// category/clip structure, and indexes clips by ID. It does NOT decode audio;
// call Load for that. Files named ".keep" and dotfiles are skipped.
//
// fsys is rooted at the clip folder itself, so callers pass
// os.DirFS(clipFolder) and categories are its immediate subdirectories.
func New(fsys fs.FS) (*Library, error) {
	return NewContext(context.Background(), fsys)
}

// NewContext is New with cancellation. The context is checked between entries,
// which bounds a scan of an unexpectedly large tree; it cannot interrupt a
// single blocking directory read on an unresponsive network share.
func NewContext(ctx context.Context, fsys fs.FS) (*Library, error) {
	l := &Library{
		byID: make(map[string]*Clip),
		fsys: fsys,
	}

	// Collect clips per category name so we can build ordered Category structs.
	byCat := make(map[string][]*Clip)

	// Diagnostics gathered during the walk. A user pointing the app at their own
	// folder hits cases a curated app-owned directory never did, and every one of
	// them used to fail silently.
	var rootAudioFiles int

	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if err != nil {
			// One unreadable entry must not destroy the whole library. A
			// user-chosen folder produces these routinely: OneDrive cloud-only
			// placeholders, an ACL'd subfolder, an antivirus lock, a directory
			// deleted mid-reorganisation, a transient network blip. Log it and
			// carry on; only a failure to open the root aborts the walk.
			if p == "." {
				return err
			}
			log.Printf("catalog: skipping %q: %v", p, err)
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}

		depth := 0
		if p != "." {
			depth = strings.Count(p, "/") + 1
		}

		if d.IsDir() {
			if p == "." {
				return nil
			}
			// Prune before descending: dot-directories are not categories, and
			// nothing below a category is indexable. Without this the walk
			// recurses the entire subtree of whatever folder the user picked -
			// a real Documents folder is thousands of entries.
			if strings.HasPrefix(d.Name(), ".") || depth >= 2 {
				return fs.SkipDir
			}
			return nil
		}

		name := d.Name()

		// A junction or symlink reports as neither a directory nor a regular
		// file, so WalkDir will not descend it and the extension check below
		// would drop it without a word. Say so instead.
		if depth == 1 && d.Type()&(fs.ModeSymlink|fs.ModeIrregular) != 0 {
			log.Printf("catalog: skipping %q: junctions and symlinks are not followed; point SoundBoard at the real folder or copy the files in", p)
			return nil
		}

		// Skip .keep markers and any dotfile.
		if strings.HasPrefix(name, ".") {
			return nil
		}
		ext := strings.ToLower(path.Ext(name))
		if !supported[ext] {
			return nil
		}

		// Expect exactly <category>/<file>. Anything shallower or deeper is
		// ignored; depth 1 means the user dropped clips straight into the clip
		// folder, which is the single most likely first-run mistake.
		parts := strings.Split(p, "/")
		if len(parts) != 2 {
			if len(parts) == 1 {
				rootAudioFiles++
			}
			return nil
		}
		category := parts[0]
		base := strings.TrimSuffix(name, path.Ext(name))

		clip := &Clip{
			ID:       category + "/" + base,
			Name:     prettify(base),
			Category: category,
			Path:     p,
		}
		if prev, dup := l.byID[clip.ID]; dup {
			// IDs drop the extension, so "airhorn.wav" and "airhorn.mp3" in one
			// category collide. byCat keeps both (two tiles) while byID keeps
			// the last, so one tile fires the other's audio.
			log.Printf("catalog: %q and %q share the id %q; only %q will play - rename one of them", prev.Path, clip.Path, clip.ID, clip.Path)
		}
		byCat[category] = append(byCat[category], clip)
		l.byID[clip.ID] = clip
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("catalog: walk clip folder: %w", err)
	}

	if rootAudioFiles > 0 && len(byCat) == 0 {
		log.Printf("catalog: found %d audio file(s) directly in the clip folder but no category folders; clips must live in <category>/<file>, e.g. memes/airhorn.wav", rootAudioFiles)
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
			// Via EnsureDecoded rather than decodeClip directly: writing
			// clip.PCM unlocked here races any concurrent EnsureDecoded, which
			// holds decMu for exactly this reason.
			if _, err := l.EnsureDecoded(clip); err != nil {
				return fmt.Errorf("catalog: load %q: %w", clip.Path, err)
			}
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
