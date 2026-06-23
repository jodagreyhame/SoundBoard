package audio

import (
	"math"
	"testing"
	"time"
)

// newWorkerFor builds a micWorker bound to an engine with rings, WITHOUT starting
// its goroutine, so tests can call process() deterministically frame by frame. It
// mirrors what startWorker constructs.
func newWorkerFor(e *Engine) *micWorker {
	return &micWorker{
		e:    e,
		hpf:  newHPF(80),
		agc:  newAGC(),
		gate: newGate(),
		den:  passthroughDenoiser{},
		stop: make(chan struct{}),
	}
}

// passthroughDenoiser is a local no-op denoiser so worker tests never depend on
// whether RNNoise (cgo) is linked; the chain is identical either way.
type passthroughDenoiser struct{}

func (passthroughDenoiser) Process(frame []float32) float32 { return 0 }
func (passthroughDenoiser) Close()                          {}

// loudMonoFrame returns a dspFrame-length mono sine well above the gate threshold.
func loudMonoFrame(ph float64) ([]float32, float64) { return sineFrame(220, 0.25, ph) }

// TestWorkerMuteModeSilences confirms MicMode "mute" forces the gate closed: after
// a run of loud frames the worker output ramps to silence and GateLevel reports ~0.
func TestWorkerMuteModeSilences(t *testing.T) {
	e := NewEngine(nil, nil)
	e.SetMicMode("mute")
	w := newWorkerFor(e)

	ph := 0.0
	var last []float32
	for i := 0; i < 400; i++ {
		var frame []float32
		frame, ph = loudMonoFrame(ph)
		w.process(frame)
		last = frame
	}
	if r := rms(last); r > 1e-3 {
		t.Fatalf("mute mode should silence the mic, output RMS = %v", r)
	}
	if gl := e.GateLevel(); gl > 0.05 {
		t.Fatalf("mute mode GateLevel should be ~0, got %v", gl)
	}
}

// TestWorkerAlwaysModePasses confirms MicMode "always" forces the gate open: a
// loud signal passes through (non-silent) and GateLevel climbs toward 1, even
// though no VAD decision is made.
func TestWorkerAlwaysModePasses(t *testing.T) {
	e := NewEngine(nil, nil)
	e.SetMicMode("always")
	w := newWorkerFor(e)

	ph := 0.0
	var last []float32
	for i := 0; i < 100; i++ {
		var frame []float32
		frame, ph = loudMonoFrame(ph)
		w.process(frame)
		last = frame
	}
	if r := rms(last); r < 0.05 {
		t.Fatalf("always mode should pass the mic, output RMS = %v", r)
	}
	if gl := e.GateLevel(); gl < 0.9 {
		t.Fatalf("always mode GateLevel should be ~1, got %v", gl)
	}
}

// TestWorkerVADGates confirms VAD mode opens on speech and closes on silence,
// publishing the gate level for the UI meter.
func TestWorkerVADGates(t *testing.T) {
	e := NewEngine(nil, nil)
	e.SetMicMode("vad")
	e.SetGateSensitivity(0.15)
	w := newWorkerFor(e)

	// Loud run -> gate opens, level high.
	ph := 0.0
	for i := 0; i < 50; i++ {
		var frame []float32
		frame, ph = loudMonoFrame(ph)
		w.process(frame)
	}
	if gl := e.GateLevel(); gl < 0.9 {
		t.Fatalf("VAD should open on loud speech, GateLevel = %v", gl)
	}

	// Silent run -> gate closes, level drops.
	for i := 0; i < 300; i++ {
		frame := make([]float32, dspFrame)
		w.process(frame)
	}
	if gl := e.GateLevel(); gl > 0.05 {
		t.Fatalf("VAD should close on silence, GateLevel = %v", gl)
	}
}

// TestWorkerPTTMode confirms PTT mode gates on the held state, not energy: with PTT
// up the gate is closed even on a loud signal; with PTT down it opens.
func TestWorkerPTTMode(t *testing.T) {
	e := NewEngine(nil, nil)
	e.SetMicMode("ptt")
	w := newWorkerFor(e)

	// PTT up + loud signal -> stays closed.
	e.SetPTTDown(false)
	ph := 0.0
	for i := 0; i < 200; i++ {
		var frame []float32
		frame, ph = loudMonoFrame(ph)
		w.process(frame)
	}
	if gl := e.GateLevel(); gl > 0.05 {
		t.Fatalf("PTT up should keep the gate closed, GateLevel = %v", gl)
	}

	// PTT down -> opens even on the same loud signal.
	e.SetPTTDown(true)
	for i := 0; i < 50; i++ {
		var frame []float32
		frame, ph = loudMonoFrame(ph)
		w.process(frame)
	}
	if gl := e.GateLevel(); gl < 0.9 {
		t.Fatalf("PTT down should open the gate, GateLevel = %v", gl)
	}
}

// TestWorkerAGCToggle confirms AGC only boosts when enabled: a quiet frame run with
// AGC on raises the output RMS above the same run with AGC off.
func TestWorkerAGCToggle(t *testing.T) {
	run := func(agcOn bool) float32 {
		e := NewEngine(nil, nil)
		e.SetMicMode("always") // remove the gate from the equation
		e.SetAGC(agcOn)
		w := newWorkerFor(e)
		ph := 0.0
		var last []float32
		for i := 0; i < 400; i++ {
			var frame []float32
			frame, ph = sineFrame(300, 0.02, ph) // quiet
			w.process(frame)
			last = frame
		}
		return rms(last)
	}
	off := run(false)
	on := run(true)
	if on <= off {
		t.Fatalf("AGC on should boost a quiet mic above AGC off: on %v, off %v", on, off)
	}
}

// --- Full ring + worker goroutine integration ----------------------------------

// TestWorkerGoroutineRoundTrip wires the real rings and worker goroutine (as
// Start/Stop would) and pushes mono frames through the input ring, asserting that
// processed mono comes back out the output ring. Drives the worker exactly as the
// RT callback would, but on a test goroutine.
func TestWorkerGoroutineRoundTrip(t *testing.T) {
	e := NewEngine(nil, nil)
	e.inRing = newRing(ringCapFrames * dspFrame)
	e.outRing = newRing(ringCapFrames * dspFrame)
	e.SetMicMode("always") // force open so output is non-silent

	e.startWorker()
	defer e.stopWorker()

	// Push a few frames of a loud tone, then drain the output until we have at least
	// one full frame back (or time out).
	ph := 0.0
	out := make([]float32, dspFrame)
	deadline := time.Now().Add(2 * time.Second)
	var got int
	for time.Now().Before(deadline) {
		frame, np := sineFrame(220, 0.3, ph)
		ph = np
		// Convert mono frame straight into the input ring (already mono).
		e.inRing.push(frame)
		if n := e.outRing.pull(out); n > 0 {
			got = n
			break
		}
		time.Sleep(time.Millisecond)
	}
	if got == 0 {
		t.Fatal("worker produced no output within the deadline")
	}
	if rms(out[:got]) < 0.01 {
		t.Fatalf("worker output should be non-silent in always mode, RMS = %v", rms(out[:got]))
	}
}

// --- Duplex callback integration: carrier + underrun ---------------------------

// configuredEngine builds an engine with the rings/scratch/carrier allocated as
// Configure would, but WITHOUT a real device or worker goroutine, so duplexCallback
// can be driven directly in a test.
func configuredEngine() *Engine {
	e := NewEngine(nil, nil)
	e.inRing = newRing(ringCapFrames * dspFrame)
	e.outRing = newRing(ringCapFrames * dspFrame)
	e.monIn = make([]float32, periodFrames)
	e.monOut = make([]float32, periodFrames)
	e.carrier = newCarrier()
	return e
}

// TestCarrierAbsentWhenForceThroughOff confirms the carrier is NOT added to the
// cable output when ForceThrough is off: with a silent mic and no clips the output
// is pure silence.
func TestCarrierAbsentWhenForceThroughOff(t *testing.T) {
	e := configuredEngine()
	e.SetForceThrough(false)

	out := make([]byte, periodFrames*channels*4)
	mic := make([]byte, periodFrames*channels*4) // silent mic
	e.duplexCallback(out, mic, periodFrames)

	for i, s := range bytesAsF32(out) {
		if s != 0 {
			t.Fatalf("ForceThrough off: cable sample %d should be silent, got %v", i, s)
		}
	}
}

// TestCarrierPresentWhenForceThroughOn confirms the carrier IS added to the cable
// output when ForceThrough is on (silent mic, no clips -> the only thing present is
// the carrier bed, which must be non-zero).
func TestCarrierPresentWhenForceThroughOn(t *testing.T) {
	e := configuredEngine()
	e.SetForceThrough(true)

	out := make([]byte, periodFrames*channels*4)
	mic := make([]byte, periodFrames*channels*4) // silent mic
	e.duplexCallback(out, mic, periodFrames)

	nonZero := false
	var peak float32
	for _, s := range bytesAsF32(out) {
		if s != 0 {
			nonZero = true
		}
		a := s
		if a < 0 {
			a = -a
		}
		if a > peak {
			peak = a
		}
	}
	if !nonZero {
		t.Fatal("ForceThrough on: carrier should make the cable output non-silent")
	}
	if peak > 0.2 {
		t.Fatalf("carrier bed too loud on the cable: peak %v", peak)
	}
}

// TestDuplexUnderrunPassthrough confirms the RT callback never blocks and falls
// back to mic passthrough when the worker has produced nothing yet: with the output
// ring empty (no worker running), a gained mic must still reach the cable
// unprocessed.
func TestDuplexUnderrunPassthrough(t *testing.T) {
	e := configuredEngine()
	// No worker is running, so the output ring stays empty -> pure underrun.

	// A constant mic signal; with underrun the output should equal the mic (no
	// processing applied) for the whole buffer.
	micF := make([]float32, periodFrames*channels)
	for i := range micF {
		micF[i] = 0.3
	}
	mic := f32bytes(micF)
	out := make([]byte, len(mic))
	e.duplexCallback(out, mic, periodFrames)

	for i, s := range bytesAsF32(out) {
		if math.Abs(float64(s-0.3)) > 1e-4 {
			t.Fatalf("underrun should pass the mic through unchanged at sample %d: got %v want 0.3", i, s)
		}
	}
	// The mic samples should now be queued in the input ring for the worker.
	if e.inRing.length() == 0 {
		t.Fatal("underrun path should still have pushed mic to the input ring")
	}
}

// TestDuplexMuteSilencesOnFullUnderrun confirms mute is AUTHORITATIVE on the RT
// thread: with MicMode "mute" and the output ring empty (the worker produced
// nothing, the worst-case underrun), a loud live mic must NOT leak to the cable.
// This is the regression for the "mute = silent must not depend on the worker" fix.
func TestDuplexMuteSilencesOnFullUnderrun(t *testing.T) {
	e := configuredEngine()
	e.SetMicMode("mute")
	// No worker running -> the output ring stays empty -> pure underrun.

	micF := make([]float32, periodFrames*channels)
	for i := range micF {
		micF[i] = 0.3 // a clearly-audible live mic
	}
	mic := f32bytes(micF)
	out := make([]byte, len(mic))
	e.duplexCallback(out, mic, periodFrames)

	for i, s := range bytesAsF32(out) {
		if s != 0 {
			t.Fatalf("mute must silence the cable even on underrun: sample %d = %v, want 0", i, s)
		}
	}
}

// TestDuplexPTTUpSilencesOnUnderrun confirms PTT mode with the key UP is treated as
// force-closed on the RT thread, so the live mic does not leak on underrun either.
func TestDuplexPTTUpSilencesOnUnderrun(t *testing.T) {
	e := configuredEngine()
	e.SetMicMode("ptt")
	e.SetPTTDown(false) // key up -> gate force-closed

	micF := make([]float32, periodFrames*channels)
	for i := range micF {
		micF[i] = 0.4
	}
	mic := f32bytes(micF)
	out := make([]byte, len(mic))
	e.duplexCallback(out, mic, periodFrames)

	for i, s := range bytesAsF32(out) {
		if s != 0 {
			t.Fatalf("PTT-up must silence the cable on underrun: sample %d = %v, want 0", i, s)
		}
	}
}

// TestDuplexPartialUnderrunSeamIsContinuous confirms the partial-underrun splice is
// cross-faded, not hard-cut: we pre-load the output ring with a small run of a
// constant processed value (so got < frames), then assert the boundary between the
// processed head and the passthrough tail has no large instantaneous jump. This is
// the regression for the "audible splice/click on partial underrun" fix.
func TestDuplexPartialUnderrunSeamIsContinuous(t *testing.T) {
	e := configuredEngine()
	e.SetMicMode("vad") // not force-closed: the tail is raw mic passthrough

	// Pre-load the output ring with `got` processed samples at a level FAR from the
	// raw mic, so a hard cut would produce a large step at the seam.
	const got = 64
	const processed float32 = -0.5
	pre := make([]float32, got)
	for i := range pre {
		pre[i] = processed
	}
	if n := e.outRing.push(pre); n != got {
		t.Fatalf("test setup: pushed %d processed samples, want %d", n, got)
	}

	const rawMic float32 = 0.5
	micF := make([]float32, periodFrames*channels)
	for i := range micF {
		micF[i] = rawMic
	}
	mic := f32bytes(micF)
	out := make([]byte, len(mic))
	e.duplexCallback(out, mic, periodFrames)

	// Inspect the per-frame mono level (both channels equal here) and find the
	// largest sample-to-sample jump across the whole buffer. A hard cut would jump
	// |processed - rawMic| = 1.0 at the seam; the cross-fade must keep every step
	// well under that.
	got32 := bytesAsF32(out)
	var maxJump float32
	for f := 1; f < periodFrames; f++ {
		d := got32[f*channels] - got32[(f-1)*channels]
		if d < 0 {
			d = -d
		}
		if d > maxJump {
			maxJump = d
		}
	}
	// With a 16-frame ramp over a 1.0 span the per-step delta is ~1/16 ≈ 0.0625.
	// Allow generous headroom but stay far below the hard-cut 1.0.
	if maxJump > 0.2 {
		t.Fatalf("partial-underrun seam jump = %v, want a smooth cross-fade (<=0.2, hard cut would be ~1.0)", maxJump)
	}
	// Sanity: the head really was the processed value and the tail really reached
	// the raw mic (the fade completed), so we tested the actual splice.
	if !approx(got32[0], processed) {
		t.Fatalf("processed head not applied: out[0] = %v, want %v", got32[0], processed)
	}
	if !approx(got32[(periodFrames-1)*channels], rawMic) {
		t.Fatalf("tail did not reach raw mic passthrough: last = %v, want %v", got32[(periodFrames-1)*channels], rawMic)
	}
}

// TestDuplexMutePartialUnderrunTailIsSilent confirms that on a PARTIAL underrun in
// mute mode the tail fades toward SILENCE (not raw mic), so even the cross-fade
// region never leaks live voice and the tail ends fully silent.
func TestDuplexMutePartialUnderrunTailIsSilent(t *testing.T) {
	e := configuredEngine()
	e.SetMicMode("mute")

	const got = 32
	pre := make([]float32, got) // processed head is silence (gate closed)
	if n := e.outRing.push(pre); n != got {
		t.Fatalf("test setup: pushed %d samples, want %d", n, got)
	}

	micF := make([]float32, periodFrames*channels)
	for i := range micF {
		micF[i] = 0.6 // loud live mic that must NOT leak
	}
	mic := f32bytes(micF)
	out := make([]byte, len(mic))
	e.duplexCallback(out, mic, periodFrames)

	// The whole buffer must be silent: head is processed silence, tail fades from
	// silence toward the silent (muted) target.
	for i, s := range bytesAsF32(out) {
		if s != 0 {
			t.Fatalf("mute partial underrun must stay silent: sample %d = %v, want 0", i, s)
		}
	}
}

// TestDuplexDuckingReducesClips confirms ducking lowers the soundboard level while
// the mic gate is open. We publish a high gate level, enable ducking, and check the
// effective ducked master is below the raw master.
func TestDuplexDuckingReducesClips(t *testing.T) {
	e := configuredEngine()
	e.SetMasterGain(1)
	e.setGateLevel(1) // mic fully open

	// Ducking off: ducked master tracks master (envelope decays to 0).
	e.SetDucking(false)
	for i := 0; i < 50; i++ {
		e.duckedMaster()
	}
	if got := e.duckedMaster(); !approx(got, 1) {
		t.Fatalf("ducking off should leave master unchanged, got %v", got)
	}

	// Ducking on with the gate open: the envelope ramps up and attenuates master.
	e.SetDucking(true)
	for i := 0; i < 50; i++ {
		e.duckedMaster()
	}
	if got := e.duckedMaster(); got >= 1 {
		t.Fatalf("ducking on with open gate should reduce master below 1, got %v", got)
	}
}
