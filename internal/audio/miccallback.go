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
//     by the DUCKING envelope when the mic gate is open.
//
// Every control (gains, ducking, gate level) is read once here via a single atomic
// load. monitorCallback is deliberately untouched: the monitor path plays clips
// only — no mic, no gate, no denoise.
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
	//
	// Mute is made AUTHORITATIVE here, on the RT thread, rather than relying on the
	// worker to keep up: when the effective gate is force-closed (Mute mode, or PTT
	// mode with the key UP), the live mic must NEVER reach the cable even if the
	// worker underruns. We pass that state into the worker bridge so the underrun
	// tail is silenced (not raw passthrough). When the rings are NOT wired we silence
	// the whole mic view directly so the degenerate passthrough path is muted too.
	if e.inRing != nil && e.outRing != nil && frames <= len(e.monIn) {
		e.processMicThroughWorker(mic, frames, e.micForceClosed())
	} else if e.micForceClosed() {
		for i := range mic {
			mic[i] = 0
		}
	}

	// Compute the per-buffer soundboard level for the cable path: the MASTER gain,
	// optionally ducked down while the mic gate is open so clips sit under speech.
	soundboard := e.duckedMaster()

	e.cursors = mixInto(out[:n], mic, e.cursors, soundboard)

	// CONFIDENCE MONITOR TAP. `out[:n]` now holds the EXACT signal going to the
	// cable (processedMic + clips) — what Discord receives. When the monitor is in
	// "transmitted" mode AND actually active (a monitor device is open to consume),
	// PUSH a copy of that mix into the tap ring so the monitor callback can play it
	// back. push is non-blocking and allocation-free: if the monitor is momentarily
	// behind and the ring is full it drops these samples (the monitor will hold-last
	// over the gap), never blocking the audio thread. Gating on monitorActive keeps
	// the ring empty in clips-mode or when no monitor is open, so a later enable
	// starts from the primed lead rather than a stale backlog. The cable write itself
	// is untouched — tapping is a pure read of `out`.
	if e.tapRing != nil && e.monitorTransmitting() && e.monitorActive.Load() {
		e.tapRing.push(out[:n])
	}
}

// spliceRampFrames is the length, in frames, of the short linear ramp laid across
// a PARTIAL-underrun seam (see processMicThroughWorker). At the boundary frame
// `got` the worker's PROCESSED mono ends; rather than splice the RAW (full-level)
// mic tail in after it — which collides processed-vs-raw levels and is the original
// 50 Hz buzz mechanism — we HOLD the last processed value and zero-ramp it down to
// silence over this many frames. ~16 frames (~0.33ms at 48kHz) is long enough to be
// click-free yet far shorter than a period, so it never masks real audio.
const spliceRampFrames = 16

// processMicThroughWorker downmixes the gained stereo mic to mono, hands it to the
// DSP worker via the input ring, pulls the processed mono back from the output
// ring, and upmixes it back over the stereo mic view IN PLACE.
//
// forceClosed is the effective mute state read once on the RT thread (Mute mode or
// PTT-up). When it is true the mic must be SILENT to the cable regardless of worker
// timing, so the underrun tail is zeroed rather than passed through — mute does not
// depend on the worker keeping up.
//
// On PARTIAL underrun (0 < got < frames) the head is the worker's processed mono.
// The tail is NOT spliced to the raw mic: that would collide the processed level
// against the full-level live mic and, because such partial pulls recur on the
// LCM(192,480)=960-sample beat, modulate the seam at 48000/960 = 50 Hz — the buzz.
// Instead we HOLD the last processed sample and zero-ramp it down to silence over
// spliceRampFrames, then stay silent for the rest of the tail. This keeps the seam
// click-free AND removes the processed-vs-raw collision even if a dip slips through
// (e.g. before the output-ring prime takes effect). On FULL underrun (got == 0)
// there is no processed sample to hold from, so the documented fallback stands: raw
// mic passthrough for the whole buffer (added latency, never a stall), or silence
// when forceClosed. When forceClosed, the tail is always silent regardless.
//
// All buffers are preallocated; this is allocation- and lock-free.
func (e *Engine) processMicThroughWorker(mic []float32, frames int, forceClosed bool) {
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

	// Head: samples the worker produced. Replace the mic with the processed value
	// on both channels.
	for f := 0; f < got; f++ {
		base := f * channels
		v := e.monOut[f]
		for ch := 0; ch < channels; ch++ {
			mic[base+ch] = v
		}
	}

	// Tail: [got, frames). Nothing to do when the worker produced the whole buffer.
	if got >= frames {
		return
	}

	// PARTIAL underrun with a processed head (0 < got < frames): HOLD the last
	// processed sample and zero-ramp it down to silence over spliceRampFrames, then
	// stay silent. We deliberately do NOT splice the raw mic tail in — that level
	// collision recurring on the 960-sample beat is the 50 Hz buzz. Holding-and-
	// ramping the processed seam toward silence is click-free and leaks no raw mic.
	if got > 0 {
		seam := e.monOut[got-1]
		for f := got; f < frames; f++ {
			var v float32
			if step := f - got; step < spliceRampFrames {
				// Linear ramp from the seam value down to silence.
				a := float32(step+1) / float32(spliceRampFrames)
				v = seam * (1 - a)
			}
			base := f * channels
			for ch := 0; ch < channels; ch++ {
				mic[base+ch] = v
			}
		}
		return
	}

	// FULL underrun (got == 0): no processed sample to hold from. Documented
	// fallback — raw mic passthrough for the whole buffer (added latency, never a
	// stall), or silence when force-closed so mute stays authoritative.
	if forceClosed {
		for f := 0; f < frames; f++ {
			base := f * channels
			for ch := 0; ch < channels; ch++ {
				mic[base+ch] = 0
			}
		}
	}
	// When not force-closed, the mic view already holds the gained raw passthrough,
	// so there is nothing to write.
}

// micForceClosed reports whether the live mic must be silenced to the cable on this
// buffer regardless of the worker — i.e. the gate is force-closed. It mirrors the
// force-closed half of the worker's gateOverride but is read on the RT thread so
// MUTE (and PTT with the key up) is authoritative even when the DSP worker
// underruns. VAD and Always never force the gate closed here; the worker's gate
// ramps handle those.
func (e *Engine) micForceClosed() bool {
	switch e.micMode() {
	case micModeMute:
		return true
	case micModePTT:
		return !e.pttIsDown()
	default: // micModeVAD, micModeAlways
		return false
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
