package audio

// dsp.go holds the mic-path DSP primitives run on the WORKER goroutine (see
// worker.go): a high-pass filter, an RMS-target AGC leveler, and a VAD/RMS gate;
// plus the voiced "carrier" oscillator run on the RT thread. Every type here is a
// plain struct of float state with in-place, allocation-free, lock-free methods so
// they are safe to drive from the near-real-time worker (and, for the carrier,
// from the audio callback itself).
//
// These operate on MONO float32 in normalized [-1,1] at 48kHz. The worker chops
// the mic stream into exactly denoise.FrameSize (480) sample frames before running
// HPF -> denoise -> AGC -> gate; the carrier is summed per stereo buffer on the
// cable path only.
//
// All coefficients are derived from the canonical 48kHz sample rate (catalog
// SampleRate) and the documented time constants. They are computed once at
// construction, never per sample.

import "math"

// fs is the canonical sample rate as a float for coefficient math.
const fs = float64(sampleRate)

// --- High-pass filter (~80 Hz) -------------------------------------------------

// onePoleHPF is a first-order high-pass that removes DC, mic rumble, and
// sub-bass room noise below ~80Hz before denoise/AGC/gate see the signal. The
// recurrence is the standard RC high-pass:
//
//	y[n] = a*(y[n-1] + x[n] - x[n-1])
//
// with a = RC/(RC+dt), RC = 1/(2*pi*fc). One multiply-add per sample, no alloc.
type onePoleHPF struct {
	a     float32
	prevX float32
	prevY float32
}

// newHPF builds a high-pass at cutoff fc Hz.
func newHPF(fc float64) *onePoleHPF {
	rc := 1.0 / (2.0 * math.Pi * fc)
	dt := 1.0 / fs
	a := rc / (rc + dt)
	return &onePoleHPF{a: float32(a)}
}

// process filters frame in place.
func (h *onePoleHPF) process(frame []float32) {
	a := h.a
	px, py := h.prevX, h.prevY
	for i, x := range frame {
		y := a * (py + x - px)
		frame[i] = y
		px = x
		py = y
	}
	h.prevX = px
	h.prevY = py
}

// reset clears the filter memory (e.g. on stream restart) so a stale sample does
// not bleed across a gap.
func (h *onePoleHPF) reset() { h.prevX, h.prevY = 0, 0 }

// --- Automatic gain control (RMS-target leveler) -------------------------------

const (
	// agcTargetRMS is the leveler's target loudness, ~-16 dBFS in linear RMS
	// (10^(-16/20) ~= 0.1585). Speech is driven toward this so a quiet talker is
	// boosted and a loud one is tamed before the gate.
	agcTargetRMS = 0.158

	// agcMaxGain is the largest boost the AGC may apply (~+18 dB == 10^(18/20)).
	// Capping the boost stops it from amplifying near-silence (and its noise floor)
	// into a roar between words.
	agcMaxGain = 7.94

	// agcMinGain floors the attenuation so a momentary loud transient cannot drive
	// the gain to zero and swallow the following speech.
	agcMinGain = 0.1

	// agcFloorRMS is the minimum frame RMS for which the AGC computes a target
	// gain. Below it the frame is treated as silence and the gain is held (not
	// boosted), so room hiss between words is never inflated to target level.
	agcFloorRMS = 0.0008
)

// agcLeveler is a smoothed RMS-target automatic gain control with a soft limiter.
// It measures the frame RMS, computes the gain that would bring it to
// agcTargetRMS (clamped to [agcMinGain, agcMaxGain]), slews the applied gain
// toward that target one-pole per sample (no per-frame jump -> no zipper noise),
// and finally runs a tanh soft limiter so the boosted peaks round off instead of
// hard-clipping.
type agcLeveler struct {
	gain   float32 // current applied gain, slewed toward target
	slewUp float32 // per-sample smoothing coef when increasing gain (slower)
	slewDn float32 // per-sample smoothing coef when decreasing gain (faster)
}

// smoothingCoef returns the one-pole coefficient that reaches ~63% of a step in
// tau seconds at the canonical sample rate: exp(-1/(tau*fs)).
func smoothingCoef(tau float64) float32 {
	return float32(math.Exp(-1.0 / (tau * fs)))
}

// newAGC builds the leveler at unity gain. Gain RISES slowly (~200ms) so a quiet
// passage is brought up gently, and FALLS faster (~50ms) so a sudden loud sound is
// tamed before it clips.
func newAGC() *agcLeveler {
	return &agcLeveler{
		gain:   1,
		slewUp: smoothingCoef(0.200),
		slewDn: smoothingCoef(0.050),
	}
}

// reset returns the leveler to unity gain.
func (a *agcLeveler) reset() { a.gain = 1 }

// rms returns the root-mean-square of frame (0 for an empty frame).
func rms(frame []float32) float32 {
	if len(frame) == 0 {
		return 0
	}
	var sum float64
	for _, x := range frame {
		sum += float64(x) * float64(x)
	}
	return float32(math.Sqrt(sum / float64(len(frame))))
}

// process levels frame in place. It returns the frame RMS measured BEFORE gain is
// applied, which the gate reuses so the energy measurement is on the clean
// (denoised, pre-AGC) signal rather than the artificially boosted one.
func (a *agcLeveler) process(frame []float32) (preRMS float32) {
	preRMS = rms(frame)

	// Decide the target gain for this frame. Hold the current gain through near
	// silence so the noise floor is never inflated.
	target := a.gain
	if preRMS > agcFloorRMS {
		target = agcTargetRMS / preRMS
		if target > agcMaxGain {
			target = agcMaxGain
		} else if target < agcMinGain {
			target = agcMinGain
		}
	}

	coef := a.slewUp
	if target < a.gain {
		coef = a.slewDn
	}
	g := a.gain
	for i, x := range frame {
		g = coef*g + (1-coef)*target
		y := x * g
		frame[i] = softClip(y)
	}
	a.gain = g
	return preRMS
}

// softClip rounds off peaks above ~unity with a tanh-like curve so AGC boosts do
// not hard-clip. Below |0.7| it is essentially linear; beyond that it compresses
// toward +/-1. Pure and cheap.
func softClip(x float32) float32 {
	const knee = 0.7
	if x > knee {
		return knee + (1-knee)*float32(math.Tanh(float64((x-knee)/(1-knee))))
	}
	if x < -knee {
		return -knee + (1-knee)*float32(math.Tanh(float64((x+knee)/(1-knee))))
	}
	return x
}

// --- Noise gate / VAD ----------------------------------------------------------

const (
	// gateOpenRamp / gateReleaseRamp are the per-sample one-pole coefficients for
	// the gate gain ramp: attack ~3ms (open) and release ~120ms (close). Ramping
	// the gain (never hard-muting) is what keeps the gate click-free.
	gateAttackTau  = 0.003
	gateReleaseTau = 0.120

	// gateHoldFrames keeps the gate open for ~30ms after energy drops below the
	// close threshold, so brief inter-syllable dips do not chop a word. The gate is
	// evaluated once per 480-sample (10ms) frame, so 3 frames == 30ms.
	gateHoldFrames = 3

	// gateMaxThreshold maps gate sensitivity 1.0 to this RMS open threshold. The
	// configured sensitivity in [0,1] scales linearly to [0, gateMaxThreshold];
	// the default 0.15 -> ~0.0075 RMS, which opens on a normal speaking voice but
	// rejects idle room tone.
	gateMaxThreshold = 0.05

	// gateHysteresis is the close-threshold fraction of the open threshold (the
	// gate closes at 0.6x where it opened) so a voice hovering near the threshold
	// does not chatter the gate open/closed.
	gateHysteresis = 0.6
)

// noiseGate ramps a 0..1 gain toward fully-open (1) when the frame RMS exceeds the
// open threshold and toward closed (0) after it falls below the close threshold
// and the hold time elapses. The gain is slewed per sample with attack/release
// one-pole coefficients so transitions never click. It carries only float/int
// state and is allocation-free.
type noiseGate struct {
	gain     float32 // current 0..1 gate gain, slewed
	open     bool    // hysteresis latch: true while above-close-threshold
	holdLeft int     // frames remaining in the post-trigger hold window

	attack  float32 // per-sample coef toward open
	release float32 // per-sample coef toward closed
}

// newGate builds a closed gate.
func newGate() *noiseGate {
	return &noiseGate{
		attack:  smoothingCoef(gateAttackTau),
		release: smoothingCoef(gateReleaseTau),
	}
}

// reset closes the gate immediately.
func (g *noiseGate) reset() {
	g.gain = 0
	g.open = false
	g.holdLeft = 0
}

// thresholdFor maps a [0,1] sensitivity to the linear RMS open threshold.
func thresholdFor(sensitivity float32) float32 {
	if sensitivity < 0 {
		sensitivity = 0
	} else if sensitivity > 1 {
		sensitivity = 1
	}
	return sensitivity * gateMaxThreshold
}

// updateLatch advances the hysteresis open/close latch for one frame given the
// frame's RMS and the open threshold. It does NOT touch the gain (that is ramped
// per sample in apply); it only decides the target the ramp aims at.
func (g *noiseGate) updateLatch(frameRMS, openThresh float32) {
	closeThresh := openThresh * gateHysteresis
	if frameRMS >= openThresh {
		g.open = true
		g.holdLeft = gateHoldFrames
	} else if frameRMS < closeThresh {
		// Below the close threshold: count down the hold window one frame at a time,
		// and only drop the latch once it has fully elapsed.
		if g.holdLeft > 0 {
			g.holdLeft--
		}
		if g.holdLeft == 0 {
			g.open = false
		}
	}
	// In the band [closeThresh, openThresh) the latch holds its current state
	// (hysteresis), so nothing changes here.
}

// apply ramps the gate gain toward the latched target (1 if open, 0 if closed)
// and multiplies frame in place. It returns the gain value at the END of the frame
// for the UI meter. forceOpen / forceClosed override the latch for the "always"
// and "mute"/PTT-up modes.
func (g *noiseGate) apply(frame []float32, forceOpen, forceClosed bool) float32 {
	target := float32(0)
	switch {
	case forceClosed:
		target = 0
	case forceOpen:
		target = 1
	case g.open:
		target = 1
	}
	coef := g.release
	if target > g.gain {
		coef = g.attack
	}
	gg := g.gain
	for i, x := range frame {
		gg = coef*gg + (1-coef)*target
		frame[i] = x * gg
	}
	g.gain = gg
	return gg
}

// --- Voiced carrier ("force through Discord's voice-activity gate") ------------

const (
	// carrierF0 is the carrier's fundamental (~130 Hz, a low male voiced pitch).
	carrierF0 = 130.0
	// carrierPartials is the fixed number of harmonics summed (fundamental + 3).
	carrierPartials = 4
	// carrierLevel is the overall carrier amplitude (~-38 dBFS == 10^(-38/20)).
	// Loud enough to keep Discord's voice-activity gate latched open and to bridge
	// the gaps between clip onsets, quiet enough to be inaudible under speech.
	carrierLevel = 0.0126
)

// carrierAmps are the per-partial amplitudes, formant-shaped so the bed reads as a
// voiced "ahh" rather than a buzzer: strong fundamental, gently decaying
// harmonics. Fixed-size so the oscillator never allocates.
var carrierAmps = [carrierPartials]float32{1.0, 0.5, 0.33, 0.2}

// carrierOsc is a phase-continuous additive oscillator that emits a quiet voiced
// bed. Phase is kept across buffers (never reset per callback) so there is no
// discontinuity -> no click at buffer boundaries. It runs on the RT thread and is
// allocation-free (fixed partial count, no slices grown).
type carrierOsc struct {
	phase float64 // fundamental phase in radians, wrapped to [0, 2pi)
	inc   float64 // per-sample phase increment for the fundamental
}

// newCarrier builds the oscillator at the canonical sample rate.
func newCarrier() *carrierOsc {
	return &carrierOsc{inc: 2 * math.Pi * carrierF0 / fs}
}

// addInto sums the carrier into an INTERLEAVED stereo buffer (the same value into
// both channels of each frame), advancing the phase. It is the only DSP that runs
// on the audio thread besides the cheap gain/mix work, so it is deliberately tiny:
// a fixed-partial sine sum per frame. Phase continuity across calls is preserved
// because phase lives on the struct.
func (o *carrierOsc) addInto(out []float32) {
	frames := len(out) / channels
	ph := o.phase
	for f := 0; f < frames; f++ {
		var s float64
		for k := 0; k < carrierPartials; k++ {
			s += float64(carrierAmps[k]) * math.Sin(ph*float64(k+1))
		}
		v := float32(s) * carrierLevel
		base := f * channels
		for ch := 0; ch < channels; ch++ {
			out[base+ch] += v
		}
		ph += o.inc
		if ph >= 2*math.Pi {
			ph -= 2 * math.Pi
		}
	}
	o.phase = ph
}

// reset zeroes the carrier phase. Used on stream teardown so a re-enable starts
// from a known phase (still click-free because it starts from 0 == silence point).
func (o *carrierOsc) reset() { o.phase = 0 }
