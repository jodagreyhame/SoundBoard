package audio

import (
	"math"
	"testing"
)

// flat builds a clip whose every sample is v, long enough that the fade ramps
// at both ends don't meet (so the middle reaches full gain).
func flat(v float32, frames int) []float32 {
	pcm := make([]float32, frames*channels)
	for i := range pcm {
		pcm[i] = v
	}
	return pcm
}

const fEps = 1e-5

func approx(a, b float32) bool { return math.Abs(float64(a-b)) <= fEps }

// TestMicPassthroughNoCursors: with no active cursors the mic is copied through
// unchanged (within clamp range).
func TestMicPassthroughNoCursors(t *testing.T) {
	out := make([]float32, 8)
	mic := []float32{0.1, -0.1, 0.2, -0.2, 0.3, -0.3, 0.4, -0.4}
	got := mixInto(out, mic, nil, 1)
	if len(got) != 0 {
		t.Fatalf("expected no surviving cursors, got %d", len(got))
	}
	for i := range mic {
		if !approx(out[i], mic[i]) {
			t.Errorf("sample %d: got %v want %v", i, out[i], mic[i])
		}
	}
}

// TestClamp: mic + clip sum past 1.0 must clamp to exactly 1.0 (and -1.0).
func TestClamp(t *testing.T) {
	out := make([]float32, channels) // exactly one frame
	// Channel 0 sums to +1.8 (-> clamp to +1.0); channel 1 sums to -1.8
	// (-> clamp to -1.0).
	mic := []float32{0.9, -0.9}
	// Clip with channel 0 = +0.9, channel 1 = -0.9, long enough that we can
	// advance past the fade-in ramp to reach full gain (1.0) in the flat middle.
	pcm := make([]float32, fadeFrames*4*channels)
	for f := 0; f < fadeFrames*4; f++ {
		pcm[f*channels] = 0.9
		pcm[f*channels+1] = -0.9
	}
	c := &clipCursor{pcm: pcm, pos: fadeFrames * channels} // past fade-in -> gain 1.0
	cursors := mixInto(out, mic, []*clipCursor{c}, 1)
	if len(cursors) != 1 {
		t.Fatalf("clip should still be active, got %d cursors", len(cursors))
	}
	if !approx(out[0], 1.0) {
		t.Errorf("positive clamp: got %v want 1.0", out[0])
	}
	if !approx(out[1], -1.0) {
		t.Errorf("negative clamp: got %v want -1.0", out[1])
	}
}

// TestOverlapSummation: two cursors of the same clip sum (overlap), each at full
// gain in the flat region.
func TestOverlapSummation(t *testing.T) {
	out := make([]float32, channels)
	mk := func() *clipCursor {
		c := &clipCursor{pcm: flat(0.2, fadeFrames*4)}
		c.pos = fadeFrames * channels
		return c
	}
	cursors := mixInto(out, nil, []*clipCursor{mk(), mk()}, 1)
	if len(cursors) != 2 {
		t.Fatalf("both cursors should survive, got %d", len(cursors))
	}
	want := float32(0.4) // 0.2 + 0.2
	if !approx(out[0], want) || !approx(out[1], want) {
		t.Errorf("overlap sum: got (%v,%v) want %v", out[0], out[1], want)
	}
}

// TestCursorRetirement: a cursor shorter than the buffer is fully consumed and
// removed from the returned slice.
func TestCursorRetirement(t *testing.T) {
	// Clip of 2 frames; buffer of 4 frames -> consumed and retired.
	c := &clipCursor{pcm: flat(0.5, 2)}
	out := make([]float32, 4*channels)
	cursors := mixInto(out, nil, []*clipCursor{c}, 1)
	if len(cursors) != 0 {
		t.Fatalf("short cursor should be retired, got %d", len(cursors))
	}
	if !c.done() {
		t.Errorf("cursor not marked done: pos=%d len=%d", c.pos, len(c.pcm))
	}
	// Frames beyond the clip length must be silent.
	for i := 2 * channels; i < len(out); i++ {
		if !approx(out[i], 0) {
			t.Errorf("sample %d past clip should be 0, got %v", i, out[i])
		}
	}
}

// TestFadeInRamp: the first frame is attenuated and gain rises monotonically
// over the fade-in window, reaching ~full at fadeFrames.
func TestFadeInRamp(t *testing.T) {
	if got := fadeGain(0, fadeFrames*4*channels); got >= 1.0 {
		t.Errorf("fade-in first frame gain should be < 1, got %v", got)
	}
	want := float32(1) / float32(fadeFrames)
	if got := fadeGain(0, fadeFrames*4*channels); !approx(got, want) {
		t.Errorf("fade-in first-frame gain: got %v want %v", got, want)
	}
	// Monotonic non-decreasing across the ramp.
	prev := float32(-1)
	for f := 0; f < fadeFrames; f++ {
		g := fadeGain(f*channels, fadeFrames*4*channels)
		if g < prev-fEps {
			t.Fatalf("fade-in not monotonic at frame %d: %v < %v", f, g, prev)
		}
		prev = g
	}
	// Full gain reached in the flat middle.
	if got := fadeGain(fadeFrames*channels, fadeFrames*4*channels); !approx(got, 1.0) {
		t.Errorf("mid-clip gain should be 1.0, got %v", got)
	}
}

// TestFadeOutRamp: gain falls to ~0 at the very end of the clip.
func TestFadeOutRamp(t *testing.T) {
	total := fadeFrames * 4 * channels
	totalFrames := total / channels
	// Last frame -> rem == 1 -> gain == 1/fadeFrames (near silence, not below 0).
	last := (totalFrames - 1) * channels
	g := fadeGain(last, total)
	want := float32(1) / float32(fadeFrames)
	if !approx(g, want) {
		t.Errorf("fade-out last-frame gain: got %v want %v", g, want)
	}
	// Decreasing across the fade-out window.
	prev := float32(2)
	for f := totalFrames - fadeFrames; f < totalFrames; f++ {
		gg := fadeGain(f*channels, total)
		if gg > prev+fEps {
			t.Fatalf("fade-out not monotonic decreasing at frame %d: %v > %v", f, gg, prev)
		}
		prev = gg
	}
}

// TestShortClipFadeOverlap: a clip shorter than 2*fadeFrames never reaches full
// gain, but stays within [0,1].
func TestShortClipFadeOverlap(t *testing.T) {
	total := fadeFrames * channels // exactly fadeFrames frames long
	for f := 0; f < fadeFrames; f++ {
		g := fadeGain(f*channels, total)
		if g < 0 || g > 1 {
			t.Fatalf("short-clip gain out of range at frame %d: %v", f, g)
		}
	}
}
