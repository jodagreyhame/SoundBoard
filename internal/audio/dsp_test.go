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

// --- AGC -----------------------------------------------------------------------

// TestAGCBoostsQuietTowardTarget feeds a steady quiet signal and confirms the AGC
// raises its output RMS toward the target over time (gain increases), without
// exceeding the configured max boost.
func TestAGCBoostsQuietTowardTarget(t *testing.T) {
	a := newAGC()
	const quietAmp = 0.02 // RMS ~0.014, well below the ~0.158 target
	ph := 0.0

	var firstRMS, lastRMS float32
	for i := 0; i < 400; i++ { // ~4s of frames -> AGC fully slewed
		var frame []float32
		frame, ph = sineFrame(300, quietAmp, ph)
		a.process(frame)
		r := rms(frame)
		if i == 0 {
			firstRMS = r
		}
		lastRMS = r
	}
	if lastRMS <= firstRMS {
		t.Fatalf("AGC should raise output RMS over time: first %v, last %v", firstRMS, lastRMS)
	}
	// Output should approach the target (within a generous band — soft clip and
	// slew make it approximate) and not blow past it wildly.
	if lastRMS < 0.05 || lastRMS > agcTargetRMS*1.3 {
		t.Fatalf("AGC output RMS %v not in a reasonable band around target %v", lastRMS, agcTargetRMS)
	}
	// Gain must respect the max-boost cap.
	if a.gain > agcMaxGain+1e-3 {
		t.Fatalf("AGC gain %v exceeded cap %v", a.gain, agcMaxGain)
	}
}

// TestAGCAttenuatesLoud feeds a loud signal and confirms the AGC pulls its gain
// below unity to bring the level down toward target.
func TestAGCAttenuatesLoud(t *testing.T) {
	a := newAGC()
	ph := 0.0
	for i := 0; i < 400; i++ {
		var frame []float32
		frame, ph = sineFrame(300, 0.6, ph) // RMS ~0.42 > target
		a.process(frame)
	}
	if a.gain >= 1 {
		t.Fatalf("AGC should attenuate a loud signal (gain < 1), got %v", a.gain)
	}
}

// TestAGCHoldsThroughSilence confirms the AGC does NOT inflate the gain during near
// silence (so the noise floor between words is not amplified to a roar).
func TestAGCHoldsThroughSilence(t *testing.T) {
	a := newAGC()
	a.gain = 1
	for i := 0; i < 200; i++ {
		frame := make([]float32, dspFrame) // pure silence, RMS 0 < agcFloorRMS
		a.process(frame)
	}
	if a.gain > 1.01 {
		t.Fatalf("AGC must not boost during silence, gain crept to %v", a.gain)
	}
}

// TestSoftClipBounded confirms softClip keeps output within [-1,1] for all inputs
// (tanh saturates to exactly +/-1 only in the extreme), compresses moderate
// overshoots BELOW full scale, and is roughly linear near zero.
func TestSoftClipBounded(t *testing.T) {
	// Output is never outside full scale, even for huge inputs.
	for _, x := range []float32{-100, -2, -1, -0.3, 0, 0.3, 1, 2, 100} {
		y := softClip(x)
		if y > 1 || y < -1 {
			t.Errorf("softClip(%v) = %v outside [-1,1]", x, y)
		}
	}
	// A moderate overshoot (1.0) is compressed strictly below full scale (the knee
	// rounds it off) rather than hard-clipped — that is the point of the soft clip.
	if y := softClip(1.0); y >= 1 {
		t.Errorf("softClip(1.0) should compress below 1, got %v", y)
	}
	if !approx(softClip(0.1), 0.1) {
		t.Errorf("softClip should be ~linear near 0, got %v", softClip(0.1))
	}
}

// --- HPF -----------------------------------------------------------------------

// TestHPFRemovesDC confirms the high-pass strips a DC offset (its steady-state
// output tends to 0 for a constant input).
func TestHPFRemovesDC(t *testing.T) {
	h := newHPF(80)
	frame := make([]float32, dspFrame)
	for i := range frame {
		frame[i] = 0.5 // constant DC
	}
	// Run several frames so the filter settles.
	for i := 0; i < 10; i++ {
		f := make([]float32, dspFrame)
		for j := range f {
			f[j] = 0.5
		}
		h.process(f)
		frame = f
	}
	if math.Abs(float64(frame[len(frame)-1])) > 0.05 {
		t.Fatalf("HPF should strip DC, tail sample = %v", frame[len(frame)-1])
	}
}

// TestHPFPassesHighAttenuatesLow compares the filter's effect on a high-frequency
// tone vs a very low one: the low tone (below cutoff) is attenuated far more.
func TestHPFPassesHighAttenuatesLow(t *testing.T) {
	measure := func(freq float64) float32 {
		h := newHPF(80)
		var ph float64
		var last []float32
		for i := 0; i < 20; i++ { // settle then measure
			var f []float32
			f, ph = sineFrame(freq, 0.5, ph)
			h.process(f)
			last = f
		}
		return rms(last)
	}
	low := measure(20)    // well below 80Hz cutoff -> heavily attenuated
	high := measure(2000) // well above -> passes
	if high <= low {
		t.Fatalf("HPF should pass high (%v) more than low (%v)", high, low)
	}
	// The 2kHz tone should pass nearly unchanged (input RMS ~0.354).
	if high < 0.3 {
		t.Fatalf("HPF over-attenuated the 2kHz passband tone: RMS %v", high)
	}
}

// --- Carrier -------------------------------------------------------------------

// TestCarrierBoundedAndAudible confirms the carrier sums a small, bounded voiced
// bed into a buffer (non-zero but well under full scale).
func TestCarrierBoundedAndAudible(t *testing.T) {
	o := newCarrier()
	out := make([]float32, periodFrames*channels)
	o.addInto(out)
	var peak float32
	nonZero := false
	for _, s := range out {
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
		t.Fatal("carrier produced silence")
	}
	// Sum of partial amplitudes * carrierLevel is the theoretical peak; assert the
	// bed is quiet (well below full scale).
	if peak > 0.1 {
		t.Fatalf("carrier peak %v too loud (should be a quiet bed)", peak)
	}
}

// TestCarrierStereoEqual confirms the carrier writes the SAME value to both
// channels of each frame (a centered mono bed).
func TestCarrierStereoEqual(t *testing.T) {
	o := newCarrier()
	out := make([]float32, periodFrames*channels)
	o.addInto(out)
	for f := 0; f < periodFrames; f++ {
		l := out[f*channels]
		r := out[f*channels+1]
		if l != r {
			t.Fatalf("frame %d: carrier L=%v R=%v should be equal", f, l, r)
		}
	}
}

// TestCarrierPhaseContinuous is the anti-click guarantee: across TWO successive
// buffers the carrier's last-sample -> first-sample step is no larger than the
// largest step WITHIN a buffer. A per-buffer phase reset would create a
// discontinuity (a click) at the boundary; phase continuity prevents it.
func TestCarrierPhaseContinuous(t *testing.T) {
	o := newCarrier()
	b1 := make([]float32, periodFrames*channels)
	b2 := make([]float32, periodFrames*channels)
	o.addInto(b1)
	o.addInto(b2)

	// Largest absolute step between consecutive frames within b1 (use channel 0).
	maxInner := float32(0)
	for f := 1; f < periodFrames; f++ {
		d := b1[f*channels] - b1[(f-1)*channels]
		if d < 0 {
			d = -d
		}
		if d > maxInner {
			maxInner = d
		}
	}
	// Step across the boundary: last frame of b1 -> first frame of b2.
	boundary := b2[0] - b1[(periodFrames-1)*channels]
	if boundary < 0 {
		boundary = -boundary
	}
	// Allow a small epsilon for float rounding; the boundary step must not exceed
	// the inner step materially (which it would if phase reset to 0).
	if boundary > maxInner+1e-4 {
		t.Fatalf("carrier discontinuity at buffer boundary: step %v > max inner step %v", boundary, maxInner)
	}
}

// TestCarrierReset confirms reset returns the oscillator to phase 0.
func TestCarrierReset(t *testing.T) {
	o := newCarrier()
	out := make([]float32, periodFrames*channels)
	o.addInto(out)
	o.reset()
	if o.phase != 0 {
		t.Fatalf("reset should zero phase, got %v", o.phase)
	}
}
