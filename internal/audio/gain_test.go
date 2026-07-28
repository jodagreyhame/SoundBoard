package audio

import (
	"math"
	"testing"
	"testing/fstest"

	"github.com/jodagreyhame/SoundBoard/internal/catalog"
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

// TestSetMonitorGainClamps mirrors TestSetMasterGainClamps for the independent
// monitor ("what you hear") gain.
func TestSetMonitorGainClamps(t *testing.T) {
	e := NewEngine(nil, nil)
	e.SetMonitorGain(-1)
	if got := e.monitorGain(); got != 0 {
		t.Errorf("SetMonitorGain(-1): got %v want 0", got)
	}
	e.SetMonitorGain(99)
	if got := e.monitorGain(); !approx(got, maxGain) {
		t.Errorf("SetMonitorGain(99): got %v want %v", got, maxGain)
	}
	e.SetMonitorGain(0.75)
	if got := e.monitorGain(); !approx(got, 0.75) {
		t.Errorf("SetMonitorGain(0.75): got %v want 0.75", got)
	}
}

// TestNewEngineDefaultsMonitorUnity confirms a fresh engine starts with the
// monitor gain at unity so the user hears clips at full level by default.
func TestNewEngineDefaultsMonitorUnity(t *testing.T) {
	e := NewEngine(nil, nil)
	if got := e.monitorGain(); !approx(got, 1) {
		t.Errorf("NewEngine monitor gain = %v, want 1 (unity)", got)
	}
}

// TestDuplexAndMonitorGainsAreIndependent is the core regression for the
// volume-clarity redesign: the DUPLEX path (what Discord hears) must scale clips
// by the MASTER gain and the MONITOR path (what the user hears) by the MONITOR
// gain, with no cross-talk. We trigger ONE clip, fan it to both queues, then set
// master=0 / monitor=non-zero and assert the duplex output is silent while the
// monitor output is audible — then flip them and assert the opposite. The clip
// is positioned past the fade-in ramp so fadeGain==1 and the only attenuation is
// the per-path soundboard level.
func TestDuplexAndMonitorGainsAreIndependent(t *testing.T) {
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
	clip.PCM = flat(0.5, fadeFrames*8)

	// trigger fans a fresh clip to both the duplex and monitor queues, advancing
	// each enqueued cursor past the fade-in ramp so the steady-state gain is the
	// pure per-path soundboard level (fadeGain==1, per-clip==unity).
	frames := uint32(4)
	n := int(frames) * channels
	fanAndPositionPastFade := func(e *Engine) {
		e.monitorActive.Store(true)
		e.TriggerGain(id, 1)
		e.cursors = drainInto(e.pending, e.cursors[:0])
		e.monCursors = drainInto(e.monPending, e.monCursors[:0])
		for _, c := range e.cursors {
			c.pos = fadeFrames * channels
		}
		for _, c := range e.monCursors {
			c.pos = fadeFrames * channels
		}
	}

	// Case 1: master muted, monitor audible. Discord hears silence; user hears it.
	e := NewEngine(nil, lib)
	e.SetMasterGain(0)
	e.SetMonitorGain(1)
	fanAndPositionPastFade(e)

	dup := make([]byte, n*4)
	mon := make([]byte, n*4)
	e.duplexCallback(dup, make([]byte, n*4), frames)
	e.monitorCallback(mon, nil, frames)

	for i, s := range bytesAsF32(dup) {
		if !approx(s, 0) {
			t.Errorf("master=0: duplex (Discord) sample %d should be silent, got %v", i, s)
		}
	}
	monAudible := false
	for _, s := range bytesAsF32(mon) {
		if !approx(s, 0) {
			monAudible = true
		}
	}
	if !monAudible {
		t.Error("master=0, monitor=1: monitor (you hear) output should be audible, was silent")
	}

	// Case 2: monitor muted, master audible. User hears silence; Discord hears it.
	e2 := NewEngine(nil, lib)
	e2.SetMasterGain(1)
	e2.SetMonitorGain(0)
	fanAndPositionPastFade(e2)

	dup2 := make([]byte, n*4)
	mon2 := make([]byte, n*4)
	e2.duplexCallback(dup2, make([]byte, n*4), frames)
	e2.monitorCallback(mon2, nil, frames)

	for i, s := range bytesAsF32(mon2) {
		if !approx(s, 0) {
			t.Errorf("monitor=0: monitor (you hear) sample %d should be silent, got %v", i, s)
		}
	}
	dupAudible := false
	for _, s := range bytesAsF32(dup2) {
		if !approx(s, 0) {
			dupAudible = true
		}
	}
	if !dupAudible {
		t.Error("monitor=0, master=1: duplex (Discord) output should be audible, was silent")
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

// TestSoundboardTimesClipGainScalesClip checks that mixInto scales a cursor's
// samples by (per-clip gain * the per-buffer soundboard level) in the flat
// (full-fade) region. The cursor is positioned past the fade-in ramp so
// fadeGain==1. 0.4 (clip pcm) * 0.5 (per-clip gain) * 0.5 (soundboard) == 0.1.
func TestSoundboardTimesClipGainScalesClip(t *testing.T) {
	out := make([]float32, channels)
	c := &clipCursor{pcm: flat(0.4, fadeFrames*4), gain: 0.5}
	c.pos = fadeFrames * channels // past fade-in -> fade gain 1.0

	cursors := mixInto(out, nil, []*clipCursor{c}, 0.5)
	if len(cursors) != 1 {
		t.Fatalf("clip should still be active, got %d cursors", len(cursors))
	}
	want := float32(0.4 * 0.5 * 0.5)
	if !approx(out[0], want) || !approx(out[1], want) {
		t.Errorf("clip gain: got (%v,%v) want %v", out[0], out[1], want)
	}
}

// TestTriggerGainCapturesPerClipOnly verifies the gain capture: TriggerGain(id,
// clipGain) enqueues a cursor whose gain equals clamp(clipGain) — the PER-CLIP
// gain ONLY. The master/monitor levels are deliberately NOT folded in at trigger
// time (they are applied per buffer in mixInto so the two paths stay
// independent), so the captured cursor gain is unaffected by the master gain.
func TestTriggerGainCapturesPerClipOnly(t *testing.T) {
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
	// A non-unity master must NOT change the captured cursor gain.
	e.SetMasterGain(0.5)
	e.TriggerGain(id, 0.8)

	cursors := drainInto(e.pending, nil)
	if len(cursors) != 1 {
		t.Fatalf("expected 1 queued cursor, got %d", len(cursors))
	}
	want := float32(0.8) // clamp(0.8); master NOT folded in
	if got := cursors[0].gain; !approx(got, want) {
		t.Errorf("cursor gain: got %v want %v (per-clip only)", got, want)
	}

	// A per-clip gain above maxGain is clamped to maxGain (still no master).
	e.TriggerGain(id, 10)
	cursors = drainInto(e.pending, nil)
	if len(cursors) != 1 {
		t.Fatalf("expected 1 queued cursor, got %d", len(cursors))
	}
	want = maxGain
	if got := cursors[0].gain; !approx(got, want) {
		t.Errorf("clamped cursor gain: got %v want %v", got, want)
	}
}

// TestPerClipZeroDropsTriggerButMasterMuteDoesNot guards the trigger-drop logic
// under the independent-levels model. A zero PER-CLIP gain is silence on every
// path and must DROP the trigger (so gainOf's "zero == unity" sentinel never
// sees a genuine zero). But a muted MASTER must NOT drop the trigger: the master
// level is applied per buffer in mixInto and the monitor ("you hear") path may
// still be audible, so the cursor must still be enqueued.
func TestPerClipZeroDropsTriggerButMasterMuteDoesNot(t *testing.T) {
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
	clip.PCM = flat(0.9, fadeFrames*4)

	e := NewEngine(nil, lib)

	// A per-clip gain of 0 is silence on every path and must be dropped.
	e.TriggerGain(id, 0)
	if cursors := drainInto(e.pending, nil); len(cursors) != 0 {
		t.Fatalf("zero per-clip trigger enqueued %d cursor(s); want 0", len(cursors))
	}

	// A muted master must NOT drop the trigger any more: the cursor is enqueued so
	// the monitor path can still play it, and the duplex path scales by master==0
	// in mixInto (audible silence on that path only).
	e.SetMasterGain(0)
	e.TriggerGain(id, 1)
	if cursors := drainInto(e.pending, nil); len(cursors) != 1 {
		t.Fatalf("master-muted trigger enqueued %d cursor(s); want 1 (monitor still hears it)", len(cursors))
	}

	// Sanity: a normal trigger enqueues exactly one cursor.
	e.SetMasterGain(1)
	e.TriggerGain(id, 0.5)
	if cursors := drainInto(e.pending, nil); len(cursors) != 1 {
		t.Fatalf("audible trigger enqueued %d cursor(s); want 1", len(cursors))
	}
}

// TestGainStillClamps confirms that boosting a clip past unity does not break
// the [-1,1] output clamp: clip 0.9 * gain 1.5 == 1.35 -> clamped to 1.0.
func TestGainStillClamps(t *testing.T) {
	out := make([]float32, channels)
	pcm := flat(0.9, fadeFrames*4)
	c := &clipCursor{pcm: pcm, gain: maxGain}
	c.pos = fadeFrames * channels // past fade-in -> fade gain 1.0

	cursors := mixInto(out, nil, []*clipCursor{c}, 1)
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
