package audio

import (
	"math"
	"testing"
	"time"

	"soundboard/internal/apm"
	"soundboard/internal/denoise"
)

// newWorkerFor builds a micWorker bound to an engine, WITHOUT starting its goroutine
// or a denoiser, so tests can call process() deterministically frame by frame. It
// mirrors what startWorker constructs (the real WebRTC APM at the engine's tier/AGC/
// echo config, or a no-op Processor when the APM is unavailable, plus the post-APM
// hard gate) but leaves den nil: with no denoiser the worker uses the ENERGY gate and
// allocates no RNNoise C state, which is what the APM/energy-path tests want. Tests
// that exercise the advanced VAD or Strong tier inject a denoiser via newWorkerWithDen.
func newWorkerFor(e *Engine) *micWorker {
	proc, _ := apm.New(e.buildAPMConfig())
	return &micWorker{
		e:             e,
		apm:           proc,
		gate:          newGate(),
		nsTierApplied: e.nsTier(),
		agcApplied:    e.agc(),
		aecApplied:    e.echoCancellation(),
		stop:          make(chan struct{}),
	}
}

// fakeDenoiser is a test Denoiser that returns a scripted speech probability and,
// optionally, applies an in-place effect so a test can prove RNNoise ran (Strong
// tier). It lets the VAD tests drive the gate from a KNOWN probability rather than
// depending on what the real RNNoise network makes of a synthetic signal.
type fakeDenoiser struct {
	prob    float32
	applied func([]float32)
	calls   int
}

func (f *fakeDenoiser) Process(frame []float32) float32 {
	f.calls++
	if f.applied != nil {
		f.applied(frame)
	}
	return f.prob
}
func (f *fakeDenoiser) Close() {}

// newWorkerWithDen builds a worker with an injected denoiser and the denVAD flag set,
// so process() takes the advanced-VAD / Strong-tier paths deterministically.
func newWorkerWithDen(e *Engine, den denoise.Denoiser) *micWorker {
	w := newWorkerFor(e)
	w.den = den
	w.denVAD = true
	return w
}

// loudMonoFrame returns a dspFrame-length mono sine well above the gate threshold.
func loudMonoFrame(ph float64) ([]float32, float64) { return sineFrame(220, 0.25, ph) }

// TestWorkerMuteModeSilences confirms MicMode "mute" forces the gate closed: after
// a run of loud frames the worker output ramps to silence and GateLevel reports ~0.
func TestWorkerMuteModeSilences(t *testing.T) {
	e := NewEngine(nil, nil)
	e.SetMicMode("mute")
	w := newWorkerFor(e)
	defer w.apm.Close()

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
	defer w.apm.Close()

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
	defer w.apm.Close()

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
	defer w.apm.Close()

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

// TestWorkerRoutesTogglesToAPM confirms the worker maps the UI NS/AGC toggles to the
// APM submodules and re-applies them only on change: process() reconfigures the APM
// when noiseSuppression()/agc() differ from the last-applied state, and the worker's
// tracked nsApplied/agcApplied follow the engine atomics. This is the worker's actual
// responsibility (routing), independent of the DLL being present — the no-op
// Processor's Reconfigure is a harmless no-op, so the bookkeeping is asserted either
// way.
func TestWorkerRoutesTogglesToAPM(t *testing.T) {
	e := NewEngine(nil, nil)
	e.SetMicMode("always")
	e.SetNoiseSuppressionTier("none")
	e.SetAGC(false)
	e.SetEchoCancellation(false)
	w := newWorkerFor(e)
	defer w.apm.Close()

	if w.nsTierApplied != nsTierNone || w.agcApplied || w.aecApplied {
		t.Fatalf("worker should start matching engine config, got tier=%d agc=%v aec=%v", w.nsTierApplied, w.agcApplied, w.aecApplied)
	}

	// Flip tier, AGC, and echo; the next process() must re-apply and update the
	// tracked state in one Reconfigure.
	e.SetNoiseSuppressionTier("high")
	e.SetAGC(true)
	e.SetEchoCancellation(true)
	frame, _ := sineFrame(220, 0.2, 0)
	w.process(frame)
	if w.nsTierApplied != nsTierHigh || !w.agcApplied || !w.aecApplied {
		t.Fatalf("worker should re-apply APM config after toggles flip, got tier=%d agc=%v aec=%v", w.nsTierApplied, w.agcApplied, w.aecApplied)
	}

	// Flip back; the next process() re-applies again.
	e.SetNoiseSuppressionTier("none")
	e.SetAGC(false)
	e.SetEchoCancellation(false)
	frame2, _ := sineFrame(220, 0.2, 0)
	w.process(frame2)
	if w.nsTierApplied != nsTierNone || w.agcApplied || w.aecApplied {
		t.Fatalf("worker should re-apply APM config after toggles clear, got tier=%d agc=%v aec=%v", w.nsTierApplied, w.agcApplied, w.aecApplied)
	}
}

// TestWorkerAdvancedVADGatesOnProbability is the core breathing-fix test at the
// worker level: with Advanced Voice Activity on, the gate is driven by the RNNoise
// SPEECH PROBABILITY, not energy. We feed SILENT frames (zero energy — the energy
// gate could never open on them) and inject a HIGH probability via a fake denoiser:
// the gate must OPEN, proving the decision is the probability. Then we drop the
// probability LOW (still silent) and, after the VAD hold elapses, the gate must
// CLOSE. This is exactly "opens on a speech-like frame, closes on a non-voice frame
// with correct hold", decoupled from what real RNNoise makes of a synthetic signal.
func TestWorkerAdvancedVADGatesOnProbability(t *testing.T) {
	e := NewEngine(nil, nil)
	e.SetMicMode("vad")
	e.SetAdvancedVoiceActivity(true)
	e.SetNoiseSuppressionTier("none") // APM NS off so the silent frame stays silent
	e.SetGateSensitivity(0.15)        // -> VAD open prob 0.6

	fake := &fakeDenoiser{prob: 0.95} // speech-like: well above the 0.6 open threshold
	w := newWorkerWithDen(e, fake)
	defer w.apm.Close()

	// Silent frames + high speech probability -> the gate opens despite ZERO energy.
	for i := 0; i < 30; i++ {
		frame := make([]float32, dspFrame)
		w.process(frame)
	}
	if gl := e.GateLevel(); gl < 0.9 {
		t.Fatalf("advanced VAD should open on high speech probability even with silent energy, GateLevel = %v", gl)
	}

	// Drop the probability below the close threshold (still silent). After the VAD
	// hold (~25 frames) plus the release ramp, the gate must close.
	fake.prob = 0.05 // non-voice: below the ~0.35 close threshold
	for i := 0; i < 300; i++ {
		frame := make([]float32, dspFrame)
		w.process(frame)
	}
	if gl := e.GateLevel(); gl > 0.05 {
		t.Fatalf("advanced VAD should close on low speech probability, GateLevel = %v", gl)
	}
}

// TestWorkerAdvancedVADIgnoresBreathEnergy proves the fix's intent directly: a
// LOUD-but-non-speech frame (energy that the old energy gate WOULD open on) keeps
// the gate CLOSED when the injected speech probability is low. This is the breathing
// case — breath has energy but is not speech.
func TestWorkerAdvancedVADIgnoresBreathEnergy(t *testing.T) {
	e := NewEngine(nil, nil)
	e.SetMicMode("vad")
	e.SetAdvancedVoiceActivity(true)
	e.SetNoiseSuppressionTier("none")
	e.SetGateSensitivity(0.15)

	fake := &fakeDenoiser{prob: 0.1} // breath-like: has energy, not speech
	w := newWorkerWithDen(e, fake)
	defer w.apm.Close()

	ph := 0.0
	for i := 0; i < 80; i++ {
		var frame []float32
		frame, ph = loudMonoFrame(ph) // plenty of energy
		w.process(frame)
	}
	if gl := e.GateLevel(); gl > 0.05 {
		t.Fatalf("advanced VAD must keep the gate closed on energetic non-speech (breath), GateLevel = %v", gl)
	}
}

// TestWorkerStrongTierAppliesRNNoiseNotAPM confirms the Strong tier (a) runs the
// RNNoise denoiser on the TRANSMITTED frame, and (b) does NOT stack it with APM NS.
// We inject a fake denoiser whose apply effect zeroes the frame; in Strong tier the
// worker must call it on the real frame (output goes silent), and the APM config the
// worker builds for that tier must have noise suppression OFF.
func TestWorkerStrongTierAppliesRNNoiseNotAPM(t *testing.T) {
	e := NewEngine(nil, nil)
	e.SetMicMode("always") // gate open so we can see the denoised signal
	e.SetNoiseSuppressionTier("strong")

	// The Strong tier must disable APM noise suppression (never stacked with RNNoise).
	if enabled, _ := nsConfigForTier(e.nsTier()); enabled {
		t.Fatal("Strong tier must build an APM config with noise suppression OFF (no stacking)")
	}

	marker := func(frame []float32) {
		for i := range frame {
			frame[i] = 0 // unmistakable in-place effect so we can detect RNNoise ran
		}
	}
	fake := &fakeDenoiser{prob: 0.9, applied: marker}
	w := newWorkerWithDen(e, fake)
	defer w.apm.Close()

	frame, _ := sineFrame(220, 0.3, 0)
	w.process(frame)
	if fake.calls == 0 {
		t.Fatal("Strong tier must run the RNNoise denoiser on the transmitted frame")
	}
	if r := rms(frame); r > 1e-6 {
		t.Fatalf("Strong tier should apply the denoiser in place (marker zeroes it), output RMS = %v", r)
	}
}

// TestWorkerNonStrongTierDoesNotApplyRNNoise confirms that in the None/Standard/High
// tiers the denoiser runs (for the VAD probability) on a COPY only — the transmitted
// frame is the APM output, never the denoised one. The fake's apply effect zeroes its
// input; if it touched the real frame the (always-open) output would be silent.
func TestWorkerNonStrongTierDoesNotApplyRNNoise(t *testing.T) {
	e := NewEngine(nil, nil)
	e.SetMicMode("vad")
	e.SetAdvancedVoiceActivity(true)
	e.SetNoiseSuppressionTier("high") // APM NS on; RNNoise used only for the probability
	e.SetGateSensitivity(0.15)

	marker := func(frame []float32) {
		for i := range frame {
			frame[i] = 0
		}
	}
	fake := &fakeDenoiser{prob: 0.95, applied: marker}
	w := newWorkerWithDen(e, fake)
	defer w.apm.Close()

	var last []float32
	ph := 0.0
	for i := 0; i < 30; i++ {
		var frame []float32
		frame, ph = loudMonoFrame(ph)
		w.process(frame)
		last = frame
	}
	if fake.calls == 0 {
		t.Fatal("non-Strong tier should still run the denoiser to read the VAD probability")
	}
	// The gate is open (high prob) and the transmitted frame is the APM output, NOT the
	// zeroed copy — so it must be non-silent.
	if r := rms(last); r < 0.01 {
		t.Fatalf("non-Strong tier must transmit the APM output, not the denoised copy; output RMS = %v", r)
	}
}

// TestWorkerAutoSensitivityTracksNoiseFloor exercises the automatic input-sensitivity
// path (energy gate): with auto-sensitivity on, a signal well above the tracked noise
// floor opens the gate while steady low-level noise near the floor does not. It also
// confirms the manual slider is bypassed (a very high manual sensitivity would block
// the open if it were used).
func TestWorkerAutoSensitivityTracksNoiseFloor(t *testing.T) {
	e := NewEngine(nil, nil)
	e.SetMicMode("vad")
	e.SetAdvancedVoiceActivity(false) // energy path
	e.SetAutoSensitivity(true)
	e.SetNoiseSuppressionTier("none")
	e.SetAGC(false)
	e.SetGateSensitivity(1.0) // manual threshold 0.05; must be BYPASSED in auto mode
	w := newWorkerFor(e)
	defer w.apm.Close()

	// Establish a low noise floor with steady low-level tone (a sine survives the APM
	// high-pass filter, unlike a DC constant). RMS ~0.0021 — quiet room tone.
	ph := 0.0
	for i := 0; i < 80; i++ {
		var frame []float32
		frame, ph = sineFrame(300, 0.003, ph)
		w.process(frame)
	}
	if gl := e.GateLevel(); gl > 0.2 {
		t.Fatalf("auto-sensitivity should keep the gate closed on steady near-floor noise, GateLevel = %v", gl)
	}

	// A MEDIUM signal (RMS ~0.028): above the tracked noise floor (so the AUTO
	// threshold opens it) but BELOW the maxed manual threshold of 0.05 (so the manual
	// slider, if it were in force, would keep it shut). The gate opening therefore
	// proves the automatic threshold — not the slider — is driving the decision.
	for i := 0; i < 60; i++ {
		var frame []float32
		frame, ph = sineFrame(300, 0.04, ph)
		w.process(frame)
	}
	if gl := e.GateLevel(); gl < 0.9 {
		t.Fatalf("auto-sensitivity should open above the floor even with the manual slider maxed, GateLevel = %v", gl)
	}
}

// TestWorkerNoiseSuppressionAttenuates confirms that, with the real APM available,
// enabling the worker's Noise suppression toggle reduces broadband noise reaching the
// cable. It drives white noise through the worker in "always" mode (gate open) with
// NS off vs NS on and asserts the NS-on output is quieter. Skipped when the APM is
// unavailable (the no-op passthrough cannot suppress noise, which is the documented
// degraded behavior).
func TestWorkerNoiseSuppressionAttenuates(t *testing.T) {
	if !apm.Available() {
		t.Skip("APM unavailable in this build; NS degrades to passthrough")
	}
	run := func(nsOn bool) float32 {
		e := NewEngine(nil, nil)
		e.SetMicMode("always") // gate open so the gate does not mask the comparison
		e.SetNoiseSuppression(nsOn)
		e.SetAGC(false)
		w := newWorkerFor(e)
		defer w.apm.Close()
		rng := newDeterministicRand()
		var last []float32
		for i := 0; i < 200; i++ {
			frame := make([]float32, dspFrame)
			for j := range frame {
				frame[j] = float32((rng()*2 - 1) * 0.1)
			}
			w.process(frame)
			last = frame
		}
		return rms(last)
	}
	off := run(false)
	on := run(true)
	if on >= off {
		t.Fatalf("worker NS on should attenuate noise below NS off: on %v, off %v", on, off)
	}
}

// newDeterministicRand returns a tiny deterministic LCG-backed [0,1) generator so
// the NS test needs no extra import and is reproducible.
func newDeterministicRand() func() float64 {
	var s uint64 = 0x9e3779b97f4a7c15
	return func() float64 {
		s = s*6364136223846793005 + 1442695040888963407
		return float64(s>>11) / float64(1<<53)
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

// --- Duplex callback integration: no-carrier + underrun ------------------------

// configuredEngine builds an engine with the rings/scratch allocated as Configure
// would, but WITHOUT a real device or worker goroutine, so duplexCallback can be
// driven directly in a test. The voiced carrier was removed (framing-buzz fix), so
// there is nothing carrier-shaped to allocate here.
func configuredEngine() *Engine {
	e := NewEngine(nil, nil)
	e.inRing = newRing(ringCapFrames * dspFrame)
	e.outRing = newRing(ringCapFrames * dspFrame)
	e.monIn = make([]float32, periodFrames)
	e.monOut = make([]float32, periodFrames)
	// Confidence-monitor tap plumbing, allocated exactly as Configure does so the
	// monitor-tap tests can drive duplexCallback/monitorCallback directly.
	e.tapRing = newRing(tapRingCapPeriods * periodFrames * channels)
	e.monTapHold = make([]float32, channels)
	return e
}

// TestNoCarrierWhenForceThroughOff confirms that with a silent mic and no clips the
// cable output is pure silence when ForceThrough is off.
func TestNoCarrierWhenForceThroughOff(t *testing.T) {
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

// TestForceThroughIsInert confirms SetForceThrough(true) is now a NO-OP: the static
// voiced carrier (a buzz by construction) was removed, so enabling ForceThrough must
// NOT add any tone to the cable. With a silent mic and no clips the output stays
// pure silence regardless of the toggle. This is the regression guarding against the
// carrier ever coming back.
func TestForceThroughIsInert(t *testing.T) {
	e := configuredEngine()
	e.SetForceThrough(true)

	out := make([]byte, periodFrames*channels*4)
	mic := make([]byte, periodFrames*channels*4) // silent mic
	e.duplexCallback(out, mic, periodFrames)

	for i, s := range bytesAsF32(out) {
		if s != 0 {
			t.Fatalf("ForceThrough must be inert: cable sample %d should be silent, got %v", i, s)
		}
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

// TestDuplexPartialUnderrunSeamIsContinuous confirms the partial-underrun tail is a
// HOLD-LAST zero-ramp, not a splice into the raw mic: we pre-load the output ring
// with a small run of a constant processed value (so got < frames), drive a loud
// raw mic, and assert (a) the seam has no large instantaneous jump, (b) the raw mic
// NEVER appears in the tail (no processed-vs-raw collision), and (c) the tail ends
// at silence. This is the regression for the 50 Hz framing buzz: the old code
// cross-faded into the full-level raw mic, which on the 960-sample beat modulated
// the cable at 50 Hz.
func TestDuplexPartialUnderrunSeamIsContinuous(t *testing.T) {
	e := configuredEngine()
	e.SetMicMode("vad") // not force-closed

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
	// |processed| = 0.5 at the seam; the hold-last ramp keeps every step well under.
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
	// With a 16-frame ramp over a 0.5 span the per-step delta is ~0.5/16 ≈ 0.031.
	if maxJump > 0.2 {
		t.Fatalf("partial-underrun seam jump = %v, want a smooth hold-last ramp (<=0.2, hard cut would be ~0.5)", maxJump)
	}
	// The head really was the processed value.
	if !approx(got32[0], processed) {
		t.Fatalf("processed head not applied: out[0] = %v, want %v", got32[0], processed)
	}
	// The raw mic must NEVER leak into the tail — that collision is the buzz. Every
	// tail sample must lie within [min(seam,0), max(seam,0)] (the ramp toward
	// silence), and in particular never reach the positive raw-mic level.
	for f := got; f < periodFrames; f++ {
		s := got32[f*channels]
		if s > 1e-4 {
			t.Fatalf("tail sample %d = %v leaked the raw mic (must hold-last toward silence, never go positive)", f, s)
		}
		if s < processed-1e-4 {
			t.Fatalf("tail sample %d = %v overshot below the seam level %v", f, s, processed)
		}
	}
	// And the tail ends fully at silence (the ramp completed well before buffer end).
	if last := got32[(periodFrames-1)*channels]; !approx(last, 0) {
		t.Fatalf("tail did not reach silence: last = %v, want 0", last)
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
