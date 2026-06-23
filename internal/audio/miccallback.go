package audio

// miccallback.go is the real-time mic->cable path: the malgo DUPLEX data callback
// and the two cheap helpers it calls. It does ONLY bounded, allocation-free,
// lock-free work on the audio thread; the heavy DSP (HPF/denoise/AGC/gate) lives on
// the worker goroutine (worker.go) and is reached through the lock-free SPSC rings
// (ring.go). The carrier oscillator and DSP primitives live in dsp.go. The engine
// lifecycle, mixer, and monitor callback stay in audio.go.

// duplexCallback is the real-time mixer for the mic->cable path (what Discord
// hears). It is the hot RT path and does ONLY cheap, bounded, allocation-free,
// lock-free work; all heavy DSP (HPF/denoise/AGC/gate) runs on the worker
// goroutine and is reached through the two lock-free rings. Per buffer it:
//
//  1. drains newly-triggered clips and consumes the stop flag (unchanged),
//  2. applies the live mic-passthrough gain in place,
//  3. DOWNMIXES the stereo mic to mono and PUSHES it to the input ring,
//  4. PULLS the processed mono back from the output ring — on UNDERRUN it falls
//     back to the (gained) mic passthrough for the missing samples and NEVER
//     blocks — then UPMIXES the processed mono back over the stereo mic view,
//  5. mixes the clips on top via mixInto, scaling them by the MASTER gain reduced
//     by the DUCKING envelope when the mic gate is open,
//  6. adds the voiced CARRIER to the cable output only when ForceThrough is on.
//
// Every control (gains, ducking, forceThrough, gate level) is read once here via a
// single atomic load. monitorCallback is deliberately untouched: the monitor path
// plays clips only — no mic, no gate, no denoise, no carrier.
func (e *Engine) duplexCallback(pOutput, pInput []byte, frameCount uint32) {
	e.cursors = drainInto(e.pending, e.cursors)
	e.cursors = clearOnStop(&e.stopFlag, e.cursors, e.pending)

	out := bytesAsF32(pOutput)
	mic := bytesAsF32(pInput)
	n := int(frameCount) * channels
	if n > len(out) {
		n = len(out)
	}
	frames := n / channels

	// Apply the live mic-passthrough gain in place on the input view before any
	// processing. miniaudio hands us a fresh input buffer each call, so scaling it
	// here is safe and allocation-free. Skip the loop at unity to stay cheap.
	if g := e.micGain(); g != 1 {
		for i := range mic {
			mic[i] *= g
		}
	}

	// Run the mic through the worker DSP chain (rings + worker). When the rings are
	// not wired (e.g. tests that never call Configure), this degrades to plain
	// passthrough: mic stays as-is and we mix directly, preserving the old behavior.
	if e.inRing != nil && e.outRing != nil && frames <= len(e.monIn) {
		e.processMicThroughWorker(mic, frames)
	}

	// Compute the per-buffer soundboard level for the cable path: the MASTER gain,
	// optionally ducked down while the mic gate is open so clips sit under speech.
	soundboard := e.duckedMaster()

	e.cursors = mixInto(out[:n], mic, e.cursors, soundboard)

	// Add the voiced carrier to the CABLE output only (never the monitor) to keep
	// Discord's voice-activity gate latched open. Phase is continuous across buffers
	// because it lives on e.carrier. Re-clamp after, since the carrier sums on top
	// of an already-clamped buffer.
	if e.carrier != nil && e.forceThrough() {
		e.carrier.addInto(out[:n])
		for i := 0; i < n; i++ {
			if out[i] > 1 {
				out[i] = 1
			} else if out[i] < -1 {
				out[i] = -1
			}
		}
	}
}

// processMicThroughWorker downmixes the gained stereo mic to mono, hands it to the
// DSP worker via the input ring, pulls the processed mono back from the output
// ring, and upmixes it back over the stereo mic view IN PLACE. On underrun (the
// worker is behind) the missing tail keeps the original mic passthrough, so the
// callback never blocks and the user still gets (unprocessed) voice for that
// period. All buffers are preallocated; this is allocation- and lock-free.
func (e *Engine) processMicThroughWorker(mic []float32, frames int) {
	// Downmix stereo -> mono (average the two channels) into the preallocated
	// scratch, then push to the input ring. A full ring just drops these samples
	// (the worker is behind); we still pull whatever IS ready below.
	for f := 0; f < frames; f++ {
		base := f * channels
		var sum float32
		for ch := 0; ch < channels; ch++ {
			sum += mic[base+ch]
		}
		e.monIn[f] = sum / float32(channels)
	}
	e.inRing.push(e.monIn[:frames])

	// Pull processed mono back. got may be < frames on underrun.
	got := e.outRing.pull(e.monOut[:frames])

	// Upmix mono -> stereo over the mic view in place. For samples the worker
	// produced, replace the mic with the processed value on both channels; for the
	// underrun tail, leave the original (gained) mic passthrough untouched.
	for f := 0; f < got; f++ {
		base := f * channels
		v := e.monOut[f]
		for ch := 0; ch < channels; ch++ {
			mic[base+ch] = v
		}
	}
}

// duckedMaster returns the master ("others hear") gain reduced by the ducking
// envelope when ducking is enabled and the mic gate is open, so soundboard clips
// drop slightly under live speech. The envelope follows the published gate level
// with a fast attack / slow release so the duck is smooth (no audible pumping). It
// is callback-owned state (duckEnv) updated once per buffer. When ducking is off
// the envelope decays back to 0 and the master gain is returned unchanged.
func (e *Engine) duckedMaster() float32 {
	master := e.masterGain()
	target := float32(0)
	if e.ducking() {
		target = e.GateLevel() // 0..1: how open the mic gate is
	}
	// One-pole envelope toward target: fast attack (duck quickly when speech
	// starts), slower release (ease the clips back up). Coefficients are per-buffer
	// (one update per ~4ms period), chosen for a smooth, musical duck.
	const attack float32 = 0.5
	const release float32 = 0.05
	coef := release
	if target > e.duckEnv {
		coef = attack
	}
	e.duckEnv += coef * (target - e.duckEnv)
	// duckDepth is the maximum attenuation at a fully-open gate (~-9 dB == 0.35).
	const duckDepth float32 = 0.65
	return master * (1 - duckDepth*e.duckEnv)
}
