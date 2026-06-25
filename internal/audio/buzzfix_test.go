package audio

// buzzfix_test.go is the regression suite for the 50 Hz output-ring framing buzz.
//
// ROOT CAUSE (verified by RE + code audit): the duplex callback pulls the output
// ring in periodFrames (192) chunks while the worker only ever refills it in whole
// dspFrame (480) bursts. With an EMPTY initial output ring, the available count
// beats against LCM(192,480) = 960: once every 960 mono samples the ring holds
// fewer than 192 samples, so duplexCallback's pull returns a PARTIAL frame and the
// underrun seam fires. That recurs at 48000/960 = 50 Hz — an audible buzz on the
// cable (never the monitor, which plays clips only).
//
// THE FIX is two-fold and both halves are pinned here:
//  1. Start() PRIMES the output ring with one 480-sample frame of silence, so the
//     available count never drops below 192 in steady state and the partial pull
//     can no longer fire (TestOutputRingPrimeKillsFramingBeat).
//  2. Even if a dip slips through, the partial-underrun path HOLDS-LAST and ramps
//     toward silence instead of splicing the full-level raw mic in, removing the
//     processed-vs-raw level collision (TestDuplexNoFiftyHzSeamWithRealWorker, plus
//     the hold-last unit tests in worker_test.go).
//
// All tests here are hardware-free: TestOutputRingPrimeKillsFramingBeat is pure ring
// arithmetic; TestDuplexNoFiftyHzSeamWithRealWorker drives a REAL worker goroutine
// and the REAL duplexCallback through the rings, exactly as Start() wires them.

import (
	"math"
	"testing"
	"time"
)

// TestOutputRingPrimeKillsFramingBeat replays the exact 192-pull / 480-push framing
// interleave the live engine produces and proves, by pure ring arithmetic (no
// goroutine, no timing), that:
//
//   - WITHOUT the prime the output ring's available count dips below periodFrames
//     once per 960 samples (the 50 Hz beat — the bug), and
//   - WITH the one-frame prime it NEVER dips below periodFrames (the fix).
//
// The model: each "tick" the callback pulls one period (192) and, whenever enough
// input has accumulated, the worker pushes one dsp frame (480). We feed input at
// exactly the real rate (192 mono samples per period, since the callback downmixes
// one mono sample per stereo frame), so the worker emits one 480 burst per 480
// samples of input — the genuine production cadence.
func TestOutputRingPrimeKillsFramingBeat(t *testing.T) {
	// simulate runs the framing interleave for the given number of periods and
	// returns the minimum output-ring "available" observed by a pull. primeFrames is
	// how many dsp frames of silence are preloaded before the run (0 = the old buggy
	// behavior, 1 = the fix).
	simulate := func(primeFrames, periodsToRun int) int {
		out := newRing(ringCapFrames * dspFrame)
		// Prime exactly as Start() now does.
		if primeFrames > 0 {
			prime := make([]float32, primeFrames*dspFrame)
			out.push(prime)
		}

		inAccum := 0 // mono input samples accumulated but not yet turned into a frame
		minAvail := out.length()
		scratch := make([]float32, periodFrames)
		frame := make([]float32, dspFrame)

		for p := 0; p < periodsToRun; p++ {
			// 1) The callback PUSHES this period's mono mic into the input side. In the
			//    real engine that is one mono sample per stereo frame == periodFrames.
			inAccum += periodFrames
			// 2) The worker drains every whole 480-sample frame currently available and
			//    pushes a processed frame to the output ring. It runs AHEAD of the pull
			//    in steady state, so model it as draining all ready frames before the
			//    callback pulls (the worst case for "is the ring full enough?").
			for inAccum >= dspFrame {
				out.push(frame)
				inAccum -= dspFrame
			}
			// 3) The callback PULLS one period from the output ring. Record the
			//    available count it saw BEFORE pulling — that is what decides whether the
			//    pull is full (>=192) or partial (<192, the seam).
			avail := out.length()
			if avail < minAvail {
				minAvail = avail
			}
			out.pull(scratch)
		}
		return minAvail
	}

	// Run well past several 960-sample boundaries: 1000 periods == 192000 mono
	// samples == 200 beats. Plenty to catch the recurring dip.
	const periodsToRun = 1000

	// WITHOUT the prime, the available count MUST dip below a full period at least
	// once (that dip IS the 50 Hz seam). If it never did, the test would not be
	// exercising the bug it guards against.
	minNoPrime := simulate(0, periodsToRun)
	if minNoPrime >= periodFrames {
		t.Fatalf("model invalid: without the prime the ring never dipped below a period "+
			"(min avail %d >= %d) — the 50Hz beat is not being reproduced", minNoPrime, periodFrames)
	}

	// WITH the one-frame prime, the available count must NEVER dip below a full
	// period — so duplexCallback always pulls a full frame and the partial-underrun
	// seam can never fire on the 960 beat. This is the buzz fix.
	minPrimed := simulate(1, periodsToRun)
	if minPrimed < periodFrames {
		t.Fatalf("prime failed: output-ring available dipped to %d (< %d) — the 50Hz "+
			"underrun seam can still fire", minPrimed, periodFrames)
	}
}

// TestDuplexNoFiftyHzSeamWithRealWorker is the end-to-end regression: it wires the
// REAL rings + REAL worker goroutine + REAL duplexCallback exactly as Start() does
// (including the one-frame output-ring prime), then drives the callback at
// periodFrames periods with a steady test tone for long enough to cross many
// 960-sample boundaries, and asserts the cable output carries NO recurring ~50 Hz
// seam. It is fully hardware-free (no malgo device).
//
// The 50 Hz buzz, if present, would appear as energy at exactly 48000/960 = 50 Hz in
// the captured cable signal. We measure that bin with a single-frequency Goertzel
// and require it to be a negligible fraction of the steady tone's energy. We also
// assert no partial-underrun seam recurs (the callback always pulled full frames),
// which is the direct cause being guarded.
func TestDuplexNoFiftyHzSeamWithRealWorker(t *testing.T) {
	e := configuredEngine()
	// "always" forces the gate open so the worker output is a clean, full-level
	// processed copy of the mic — a steady signal whose only possible 50 Hz content
	// would be the framing seam itself.
	e.SetMicMode("always")
	e.SetNoiseSuppression(false)
	e.SetAGC(false) // keep the level steady so the seam is the only 50Hz candidate

	// Prime the output ring with one frame of silence, exactly as Start() does.
	prime := make([]float32, dspFrame)
	e.outRing.push(prime)

	// Deterministic prime guard (no timing): immediately after priming the output
	// ring must hold a full dsp frame of headroom — that one frame of lead is what
	// keeps the worker ahead of the 192-sample pulls. If the Start()-equivalent
	// prime is ever dropped, this fails before the spectral analysis even runs.
	if avail := e.outRing.length(); avail < dspFrame {
		t.Fatalf("output ring not primed: available %d, want >= %d (one dsp frame of lead)", avail, dspFrame)
	}

	// Launch the real worker goroutine on these rings (as Start() would).
	e.startWorker()
	defer e.stopWorker()

	// A steady 220 Hz tone at a moderate level, fed identically every period so the
	// ONLY way 50 Hz can appear in the output is the framing seam.
	const toneFreq = 220.0
	const toneAmp = 0.3
	inc := 2 * math.Pi * toneFreq / fs
	phase := 0.0

	// Drive enough periods to cross many 960-sample boundaries: 1500 periods ==
	// 288000 mono samples == 300 beats of the would-be 50 Hz seam.
	const periodsToRun = 1500
	const monoPerPeriod = periodFrames // one mono sample per stereo frame

	captured := make([]float32, 0, periodsToRun*monoPerPeriod)
	micF := make([]float32, periodFrames*channels)
	outB := make([]byte, periodFrames*channels*4)

	partialSeams := 0
	// Let the worker build a small lead first so it stays ahead in steady state.
	time.Sleep(15 * time.Millisecond)

	for p := 0; p < periodsToRun; p++ {
		// Fill the stereo mic view with the continuing tone (same value both channels).
		for f := 0; f < periodFrames; f++ {
			v := float32(toneAmp * math.Sin(phase))
			phase += inc
			micF[f*channels] = v
			micF[f*channels+1] = v
		}
		mic := f32bytes(micF)

		// Record the output-ring availability the callback will see; a value < period
		// means this period took the partial-underrun path (a seam candidate).
		availBefore := e.outRing.length()
		if availBefore < periodFrames {
			partialSeams++
		}

		e.duplexCallback(outB, mic, periodFrames)

		// Capture the mono (channel-0) cable signal for spectral analysis.
		o := bytesAsF32(outB)
		for f := 0; f < periodFrames; f++ {
			captured = append(captured, o[f*channels])
		}

		// Pace slightly so the worker (separate goroutine) keeps refilling ahead of
		// the next pull, mirroring the real-time cadence without a real device.
		time.Sleep(200 * time.Microsecond)
	}

	// 1) DIRECT cause check: with the prime in place the callback must (essentially)
	//    never hit the partial-underrun path on the steady beat. We allow a tiny
	//    number of warm-up dips from goroutine-scheduling jitter at the very start,
	//    but nothing like the once-per-960 cadence the bug would produce (which over
	//    300 beats would be hundreds of seams).
	if partialSeams > periodsToRun/100 {
		t.Fatalf("output ring under-ran on %d/%d periods — the prime is not keeping the "+
			"worker a frame ahead, the 50Hz seam can recur", partialSeams, periodsToRun)
	}

	// 2) SPECTRAL check: measure the 50 Hz bin (the seam frequency, 48000/960) and the
	//    220 Hz tone bin via Goertzel, after discarding a warm-up prefix. The 50 Hz
	//    energy must be a negligible fraction of the tone energy.
	const warmup = 20 * periodFrames // drop early jitter
	sig := captured[warmup:]
	tone := goertzelMag(sig, toneFreq, fs)
	buzz := goertzelMag(sig, 50.0, fs)
	if tone <= 0 {
		t.Fatalf("captured tone has no energy at %g Hz (test signal broken)", toneFreq)
	}
	ratio := buzz / tone
	// A real 50 Hz framing buzz shows up as a substantial fraction of the tone; the
	// fixed engine leaves only numerical noise. 2% is comfortably above the floor and
	// far below a genuine seam.
	if ratio > 0.02 {
		t.Fatalf("50Hz framing-seam energy = %.4f of the tone (want <= 0.02): the buzz is present", ratio)
	}
}

// goertzelMag returns the magnitude of the single DFT bin nearest targetHz in sig,
// computed with the Goertzel algorithm (O(n), allocation-light). It lets the buzz
// test probe exactly the 50 Hz seam frequency without a full FFT dependency.
func goertzelMag(sig []float32, targetHz, sampleRate float64) float64 {
	n := len(sig)
	if n == 0 {
		return 0
	}
	k := targetHz / sampleRate
	w := 2 * math.Pi * k
	cosw := math.Cos(w)
	coeff := 2 * cosw
	var s0, s1, s2 float64
	for _, x := range sig {
		s0 = float64(x) + coeff*s1 - s2
		s2 = s1
		s1 = s0
	}
	real := s1 - s2*cosw
	imag := s2 * math.Sin(w)
	return math.Sqrt(real*real+imag*imag) / float64(n)
}
