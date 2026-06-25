package audio

// dsp.go holds the mic-path gate run on the WORKER goroutine (see worker.go) AFTER
// the WebRTC APM. The heavy DSP — high-pass filter, noise suppression, and automatic
// gain control — is now done by the real WebRTC AudioProcessingModule in a single
// ProcessCapture call (internal/apm), so the hand-rolled onePoleHPF, agcLeveler, and
// the RNNoise denoiser were RETIRED. What remains here is the HARD gate: the APM has
// no hard mute, so the engine's PTT/Mute/MicMode gating and the VAD latch run on the
// post-APM signal and ramp the frame to silence when the mic must be closed.
//
// Every type here is a plain struct of float state with in-place, allocation-free,
// lock-free methods so they are safe to drive from the near-real-time worker. They
// operate on MONO float32 in normalized [-1,1] at 48kHz; the worker chops the mic
// stream into exactly apm.FrameSize (480) sample frames before processing.
//
// Coefficients are derived from the canonical 48kHz sample rate (catalog SampleRate)
// and the documented time constants, computed once at construction, never per sample.

import "math"

// fs is the canonical sample rate as a float for coefficient math.
const fs = float64(sampleRate)

// smoothingCoef returns the one-pole coefficient that reaches ~63% of a step in
// tau seconds at the canonical sample rate: exp(-1/(tau*fs)). Shared by the gate's
// attack/release ramps.
func smoothingCoef(tau float64) float32 {
	return float32(math.Exp(-1.0 / (tau * fs)))
}

// rms returns the root-mean-square of frame (0 for an empty frame). The worker uses
// it to key the gate/VAD off the POST-APM energy — the level Discord actually
// receives — and to publish the UI mic-open meter.
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

// --- Noise gate / VAD ----------------------------------------------------------

const (
	// gateAttackTau / gateReleaseTau are the one-pole time constants for the gate
	// gain ramp: attack ~3ms (open) and release ~120ms (close). Ramping the gain
	// (never hard-muting) is what keeps the gate click-free.
	gateAttackTau  = 0.003
	gateReleaseTau = 0.120

	// gateHoldFrames keeps the gate open for ~30ms after energy drops below the
	// close threshold, so brief inter-syllable dips do not chop a word. The gate is
	// evaluated once per 480-sample (10ms) frame, so 3 frames == 30ms.
	gateHoldFrames = 3

	// gateMaxThreshold maps gate sensitivity 1.0 to this RMS open threshold. The
	// configured sensitivity in [0,1] scales linearly to [0, gateMaxThreshold];
	// the default 0.15 -> ~0.0075 RMS, which opens on a normal speaking voice but
	// rejects idle room tone. The energy keyed is the post-APM RMS.
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
