package audio

import (
	"math"
	"testing"
)

// sineFrame fills a fresh dspFrame-length mono buffer with a sine of the given
// frequency and peak amplitude at the canonical sample rate, continuing the phase
// from ph and returning the new phase so successive frames are continuous.
func sineFrame(freq, amp float64, ph float64) ([]float32, float64) {
	f := make([]float32, dspFrame)
	inc := 2 * math.Pi * freq / fs
	for i := range f {
		f[i] = float32(amp * math.Sin(ph))
		ph += inc
	}
	return f, ph
}

// --- Gate ----------------------------------------------------------------------
//
// The high-pass filter, noise suppression, and automatic gain control are now done
// by the real WebRTC APM (internal/apm), covered by that package's tests. What
// remains in dsp.go is the post-APM hard gate, exercised here.

// TestGateOpensOnLoudClosesOnQuiet drives the gate with a run of loud frames then
// a run of quiet frames and checks the gain ramps up toward 1 then back toward 0
// (after the hold), never hard-jumping.
func TestGateOpensOnLoudClosesOnQuiet(t *testing.T) {
	g := newGate()
	thr := thresholdFor(0.15) // default sensitivity

	// Loud frames: RMS well above threshold. Gain should climb toward 1.
	var lastGain float32
	ph := 0.0
	for i := 0; i < 20; i++ {
		var frame []float32
		frame, ph = sineFrame(200, 0.2, ph) // RMS ~0.14 >> thr
		g.updateLatch(rms(frame), thr)
		lastGain = g.apply(frame, false, false)
	}
	if lastGain < 0.9 {
		t.Fatalf("gate should be near-open after loud run, gain = %v", lastGain)
	}
	if !g.open {
		t.Fatal("gate latch should be open after loud run")
	}

	// Quiet frames (near silence): after the hold window the latch closes and the
	// gain ramps down toward 0.
	for i := 0; i < 200; i++ {
		frame := make([]float32, dspFrame) // silence -> RMS 0 < closeThresh
		g.updateLatch(rms(frame), thr)
		lastGain = g.apply(frame, false, false)
	}
	if lastGain > 0.05 {
		t.Fatalf("gate should be near-closed after quiet run, gain = %v", lastGain)
	}
	if g.open {
		t.Fatal("gate latch should be closed after quiet run")
	}
}

// TestGateHysteresis confirms the open/close thresholds differ: a frame between the
// close and open thresholds holds the current latch state rather than toggling it.
func TestGateHysteresis(t *testing.T) {
	g := newGate()
	openThr := float32(0.02)
	closeThr := openThr * gateHysteresis // 0.012

	// Start closed; a mid-band frame (between close and open) must NOT open it.
	mid := openThr * 0.8 // 0.016, in [closeThr, openThr)
	g.updateLatch(mid, openThr)
	if g.open {
		t.Fatal("mid-band energy must not open a closed gate (hysteresis)")
	}

	// Open it with a loud frame, then a mid-band frame must keep it OPEN.
	g.updateLatch(openThr*2, openThr)
	if !g.open {
		t.Fatal("loud frame should open the gate")
	}
	// Drain the hold so the latch can actually be re-evaluated, but keep energy in
	// the hysteresis band so it stays open.
	for i := 0; i < gateHoldFrames+2; i++ {
		g.updateLatch(mid, openThr)
	}
	if !g.open {
		t.Fatal("mid-band energy must keep an open gate open (hysteresis)")
	}
	_ = closeThr

	// Below the close threshold (and past hold) it finally closes.
	for i := 0; i < gateHoldFrames+2; i++ {
		g.updateLatch(closeThr*0.5, openThr)
	}
	if g.open {
		t.Fatal("energy below close threshold should close the gate")
	}
}

// TestGateRampNoClick verifies the gate gain moves SMOOTHLY (no single-sample jump
// to full) when opening: the first loud frame's per-sample gain rises gradually,
// staying below 1 for the whole first frame given the ~3ms attack.
func TestGateRampNoClick(t *testing.T) {
	g := newGate()
	thr := thresholdFor(0.15)
	frame, _ := sineFrame(200, 0.2, 0)
	g.updateLatch(rms(frame), thr)
	endGain := g.apply(frame, false, false)
	// One 10ms frame with a 3ms attack should be partway open but not slammed to 1.
	if endGain <= 0 || endGain >= 1 {
		t.Fatalf("first-frame gate gain should ramp in (0,1), got %v", endGain)
	}
}

// TestGateForceModes confirms the force flags override the latch: forceOpen ramps
// toward 1 regardless of energy, forceClosed ramps toward 0.
func TestGateForceModes(t *testing.T) {
	// forceOpen on silence -> gain climbs toward 1.
	g := newGate()
	var gain float32
	for i := 0; i < 50; i++ {
		frame := make([]float32, dspFrame)
		gain = g.apply(frame, true, false)
	}
	if gain < 0.9 {
		t.Fatalf("forceOpen should drive gain toward 1, got %v", gain)
	}

	// forceClosed on a loud signal -> gain falls toward 0.
	g2 := newGate()
	g2.gain = 1 // start open
	ph := 0.0
	for i := 0; i < 400; i++ {
		var frame []float32
		frame, ph = sineFrame(200, 0.3, ph)
		gain = g2.apply(frame, false, true)
	}
	if gain > 0.05 {
		t.Fatalf("forceClosed should drive gain toward 0, got %v", gain)
	}
}

// TestRMS confirms the shared RMS helper the gate/meter key off computes the
// expected root-mean-square: 0 for an empty/silent frame and the sine RMS
// (amp/sqrt2) for a full-amplitude tone.
func TestRMS(t *testing.T) {
	if r := rms(nil); r != 0 {
		t.Fatalf("rms(nil) = %v, want 0", r)
	}
	if r := rms(make([]float32, dspFrame)); r != 0 {
		t.Fatalf("rms(silence) = %v, want 0", r)
	}
	frame, _ := sineFrame(200, 0.5, 0)
	want := float32(0.5 / math.Sqrt2)
	if r := rms(frame); math.Abs(float64(r-want)) > 0.02 {
		t.Fatalf("rms(0.5 sine) = %v, want ~%v", r, want)
	}
}

// --- Advanced (RNNoise) VAD latch ----------------------------------------------

// TestVADLatchOpensOnSpeechClosesOnNonVoice drives the speech-probability latch with
// a high probability (speech-like) then a low one (non-voice) and checks it opens
// then, after the hold window, closes — the core VAD gate decision the breathing fix
// relies on. It keys purely on the probability, so a non-voice frame WITH energy
// (breath) would never open it.
func TestVADLatchOpensOnSpeechClosesOnNonVoice(t *testing.T) {
	g := newGate()
	openProb := vadOpenProbFor(0.15) // 0.6 open, ~0.35 close

	// Speech-like probability opens the latch immediately and arms the hold.
	g.updateLatchVAD(0.9, openProb)
	if !g.open {
		t.Fatal("a high speech probability should open the VAD latch")
	}
	if g.holdLeft != vadHoldFrames {
		t.Fatalf("opening should arm the hold to %d frames, got %d", vadHoldFrames, g.holdLeft)
	}

	// A non-voice probability (below close) must NOT drop the latch until the whole
	// hold window has elapsed — this is the anti-chop hold that bridges syllables.
	for i := 0; i < vadHoldFrames-1; i++ {
		g.updateLatchVAD(0.05, openProb)
		if !g.open {
			t.Fatalf("latch closed after only %d hold frames; should hold for %d", i+1, vadHoldFrames)
		}
	}
	g.updateLatchVAD(0.05, openProb) // the final hold frame elapses
	if g.open {
		t.Fatal("latch should close once the hold window fully elapses on non-voice")
	}
}

// TestVADLatchHysteresis confirms the open/close probabilities differ: a probability
// in the band [closeProb, openProb) holds the latch's current state rather than
// toggling it, so a voice hovering near threshold does not chatter the gate.
func TestVADLatchHysteresis(t *testing.T) {
	g := newGate()
	openProb := float32(0.6)
	closeProb := openProb * vadCloseRatio // ~0.35
	mid := (openProb + closeProb) / 2     // ~0.475, inside the hysteresis band

	// From closed, a mid-band probability must NOT open it.
	g.updateLatchVAD(mid, openProb)
	if g.open {
		t.Fatal("mid-band probability must not open a closed VAD latch (hysteresis)")
	}

	// Open it, drain the hold while staying in the band, and it must remain open.
	g.updateLatchVAD(0.9, openProb)
	for i := 0; i < vadHoldFrames+2; i++ {
		g.updateLatchVAD(mid, openProb)
	}
	if !g.open {
		t.Fatal("mid-band probability must keep an open VAD latch open (hysteresis)")
	}

	// Below the close probability (and past the hold) it finally closes.
	for i := 0; i < vadHoldFrames+2; i++ {
		g.updateLatchVAD(closeProb*0.5, openProb)
	}
	if g.open {
		t.Fatal("a probability below the close threshold should close the VAD latch")
	}
}

// TestVADOpenProbForMapping pins the sensitivity -> VAD open-probability mapping: the
// default sensitivity yields the design's 0.6 open point, higher sensitivity demands
// a higher probability (more aggressive gating), and the result is clamped.
func TestVADOpenProbForMapping(t *testing.T) {
	if got := vadOpenProbFor(0.15); math.Abs(float64(got)-0.6) > 1e-5 {
		t.Fatalf("vadOpenProbFor(0.15) = %v, want ~0.6 (the design open point)", got)
	}
	low := vadOpenProbFor(0.0)
	high := vadOpenProbFor(0.9)
	if !(low < vadOpenProbFor(0.15) && vadOpenProbFor(0.15) < high) {
		t.Fatalf("higher sensitivity should raise the open probability: low=%v mid=%v high=%v", low, vadOpenProbFor(0.15), high)
	}
	if got := vadOpenProbFor(2); got > vadOpenProbMax+1e-6 {
		t.Fatalf("vadOpenProbFor must clamp above to %v, got %v", vadOpenProbMax, got)
	}
	if got := vadOpenProbFor(-1); got < vadOpenProbMin-1e-6 {
		t.Fatalf("vadOpenProbFor must clamp below to %v, got %v", vadOpenProbMin, got)
	}
}
