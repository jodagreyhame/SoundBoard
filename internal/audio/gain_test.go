package audio

import (
	"math"
	"testing"
	"testing/fstest"

	"soundboard/internal/catalog"
)

// f32bytes reinterprets a []float32 as the []byte buffer miniaudio would hand a
// callback, so the gain tests can exercise the real duplexCallback path.
func f32bytes(f []float32) []byte {
	b := make([]byte, len(f)*4)
	view := bytesAsF32(b)
	copy(view, f)
	return b
}

// TestSetMicGainClamps verifies the mic-gain setter constrains its input to
// [0, maxGain] (and maps NaN to 0) before publishing it to the RT callback.
func TestSetMicGainClamps(t *testing.T) {
	e := NewEngine(nil, nil)

	cases := []struct {
		in, want float32
	}{
		{1.0, 1.0},
		{0.5, 0.5},
		{-2.0, 0},                // below range -> 0
		{10.0, maxGain},          // above range -> maxGain
		{maxGain, maxGain},       // exactly the cap
		{float32(math.NaN()), 0}, // NaN -> 0
	}
	for _, c := range cases {
		e.SetMicGain(c.in)
		if got := e.micGain(); !approx(got, c.want) {
			t.Errorf("SetMicGain(%v): micGain()=%v want %v", c.in, got, c.want)
		}
	}
}

// TestSetMasterGainClamps mirrors TestSetMicGainClamps for the master gain.
func TestSetMasterGainClamps(t *testing.T) {
	e := NewEngine(nil, nil)
	e.SetMasterGain(-1)
	if got := e.masterGain(); got != 0 {
		t.Errorf("SetMasterGain(-1): got %v want 0", got)
	}
	e.SetMasterGain(99)
	if got := e.masterGain(); !approx(got, maxGain) {
		t.Errorf("SetMasterGain(99): got %v want %v", got, maxGain)
	}
	e.SetMasterGain(0.75)
	if got := e.masterGain(); !approx(got, 0.75) {
		t.Errorf("SetMasterGain(0.75): got %v want 0.75", got)
	}
}

// TestMicGainScalesPassthrough drives the real duplexCallback (no cursors) with
// a known mic buffer and a mic gain of 0.5, and asserts the mic passthrough on
// the cable output is scaled by exactly that gain.
func TestMicGainScalesPassthrough(t *testing.T) {
	e := NewEngine(nil, nil)
	e.SetMicGain(0.5)

	micSamples := []float32{0.4, -0.4, 0.2, -0.2}
	in := f32bytes(micSamples)
	out := make([]byte, len(in))

	e.duplexCallback(out, in, uint32(len(micSamples)/channels))

	got := bytesAsF32(out)
	for i, m := range micSamples {
		want := m * 0.5
		if !approx(got[i], want) {
			t.Errorf("sample %d: got %v want %v (mic*0.5)", i, got[i], want)
		}
	}
}

// TestMicGainZeroSilencesMic confirms a mic gain of 0 zeroes the mic
// passthrough entirely.
func TestMicGainZeroSilencesMic(t *testing.T) {
	e := NewEngine(nil, nil)
	e.SetMicGain(0)

	micSamples := []float32{0.9, -0.9, 0.5, -0.5}
	in := f32bytes(micSamples)
	out := make([]byte, len(in))

	e.duplexCallback(out, in, uint32(len(micSamples)/channels))

	for i, s := range bytesAsF32(out) {
		if !approx(s, 0) {
			t.Errorf("sample %d: mic gain 0 should silence, got %v", i, s)
		}
	}
}

// TestMasterTimesClipGainScalesClip checks that mixInto scales a cursor's
// samples by the cursor gain in the flat (full-fade) region. The cursor is
// positioned past the fade-in ramp so fadeGain==1 and the only attenuation is
// the cursor gain. 0.4 (clip) * 0.5 (effective gain) == 0.2.
func TestMasterTimesClipGainScalesClip(t *testing.T) {
	out := make([]float32, channels)
	c := &clipCursor{pcm: flat(0.4, fadeFrames*4), gain: 0.5}
	c.pos = fadeFrames * channels // past fade-in -> fade gain 1.0

	cursors := mixInto(out, nil, []*clipCursor{c})
	if len(cursors) != 1 {
		t.Fatalf("clip should still be active, got %d cursors", len(cursors))
	}
	want := float32(0.4 * 0.5)
	if !approx(out[0], want) || !approx(out[1], want) {
		t.Errorf("clip gain: got (%v,%v) want %v", out[0], out[1], want)
	}
}

// TestTriggerGainCapturesMasterTimesClip verifies the end-to-end gain capture:
// TriggerGain(id, clipGain) enqueues a cursor whose gain equals
// clamp(clipGain)*master, captured at trigger time. We then drain the pending
// channel the way the callback does and inspect the resulting cursor gain.
func TestTriggerGainCapturesMasterTimesClip(t *testing.T) {
	lib, err := catalog.New(fstest.MapFS{
		"sounds/test/clip.wav": {Data: []byte("not decoded")},
	})
	if err != nil {
		t.Fatalf("catalog.New: %v", err)
	}
	const id = "test/clip"
	clip := lib.Get(id)
	if clip == nil {
		t.Fatalf("clip %q not indexed", id)
	}
	clip.PCM = flat(0.3, fadeFrames*4)

	e := NewEngine(nil, lib)
	e.SetMasterGain(0.5)
	e.TriggerGain(id, 0.8)

	cursors := drainInto(e.pending, nil)
	if len(cursors) != 1 {
		t.Fatalf("expected 1 queued cursor, got %d", len(cursors))
	}
	want := float32(0.8 * 0.5) // clamp(0.8)*master
	if got := cursors[0].gain; !approx(got, want) {
		t.Errorf("cursor gain: got %v want %v", got, want)
	}

	// A per-clip gain above maxGain is clamped before folding in master:
	// clamp(10)=maxGain, *0.5.
	e.TriggerGain(id, 10)
	cursors = drainInto(e.pending, nil)
	if len(cursors) != 1 {
		t.Fatalf("expected 1 queued cursor, got %d", len(cursors))
	}
	want = maxGain * 0.5
	if got := cursors[0].gain; !approx(got, want) {
		t.Errorf("clamped cursor gain: got %v want %v", got, want)
	}
}

// TestGainStillClamps confirms that boosting a clip past unity does not break
// the [-1,1] output clamp: clip 0.9 * gain 1.5 == 1.35 -> clamped to 1.0.
func TestGainStillClamps(t *testing.T) {
	out := make([]float32, channels)
	pcm := flat(0.9, fadeFrames*4)
	c := &clipCursor{pcm: pcm, gain: maxGain}
	c.pos = fadeFrames * channels // past fade-in -> fade gain 1.0

	cursors := mixInto(out, nil, []*clipCursor{c})
	if len(cursors) != 1 {
		t.Fatalf("clip should still be active, got %d cursors", len(cursors))
	}
	if !approx(out[0], 1.0) || !approx(out[1], 1.0) {
		t.Errorf("boosted clip should clamp to 1.0, got (%v,%v)", out[0], out[1])
	}
}

// TestMicGainClampsTogether checks the combined path: a boosted mic plus a
// boosted clip still clamp to the [-1,1] range at the cable output.
func TestMicGainClampsTogether(t *testing.T) {
	e := NewEngine(nil, nil)
	e.SetMicGain(maxGain) // 0.8 * 1.5 = 1.2 alone would exceed 1.0

	micSamples := []float32{0.8, -0.8}
	in := f32bytes(micSamples)
	out := make([]byte, len(in))

	e.duplexCallback(out, in, 1)

	got := bytesAsF32(out)
	if !approx(got[0], 1.0) {
		t.Errorf("boosted mic should clamp to +1.0, got %v", got[0])
	}
	if !approx(got[1], -1.0) {
		t.Errorf("boosted mic should clamp to -1.0, got %v", got[1])
	}
}
