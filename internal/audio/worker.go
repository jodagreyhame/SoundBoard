package audio

// worker.go runs the heavy mic-path DSP off the real-time audio thread.
//
// WHY a worker: the WebRTC APM (like RNNoise before it) only accepts exactly
// 480-sample (10ms) MONO frames and is far too expensive to run inside the malgo
// callback (whose period is 192 STEREO frames and which must never allocate, lock,
// or block). So duplexCallback only shoves mono mic samples into the input ring and
// pulls processed mono out of the output ring; THIS goroutine does the real work in
// between:
//
//	input ring --pull 480--> [ APM ProcessCapture -> hard gate ] --push--> output ring
//
// The chain used to be a hand-rolled HPF -> RNNoise -> AGC -> gate. It is now the
// REAL WebRTC AudioProcessingModule at Discord's exact config (high-pass filter +
// noise suppression at Moderate + adaptive-digital gain control, echo cancellation
// off) — a single ProcessCapture call per frame that does the HPF, NS, and AGC the
// hand-rolled stages approximated. The only thing kept AFTER the APM is the HARD
// gate: the APM has no hard mute, so the engine's PTT/Mute/MicMode gating and the
// VAD latch still run on the post-APM signal and ramp the frame to silence when the
// mic must be closed.
//
// The worker may briefly sleep when the input ring has less than a full frame
// (bounded wait, never a hard busy-spin), but it NEVER blocks the RT callback: the
// rings are lock-free and the callback's push/pull are non-blocking. If the worker
// ever falls behind, the output ring simply runs dry and the callback emits mic
// passthrough for that period (see duplexCallback) — added latency, never a stall.
//
// Lifecycle: startWorker (from Engine.Start) launches the goroutine; stopWorker
// (from Engine.Stop) signals it to exit and waits for it, then frees the native APM.
// Both are called under ctrlMu, off the audio thread.

import (
	"sync"
	"time"

	"soundboard/internal/apm"
	"soundboard/internal/denoise"
)

const (
	// dspFrame is the worker's fixed processing block: 480 mono samples (10ms at
	// 48kHz), the WebRTC APM's mandatory frame size. Everything in the chain runs on
	// exactly this length.
	dspFrame = apm.FrameSize

	// ringCapFrames sizes each ring to hold a handful of DSP frames so a late
	// worker wakeup or a jittery callback has slack without the producer ever
	// having to drop samples in steady state. 8 frames == 80ms of mono headroom.
	ringCapFrames = 8

	// workerIdleSleep is how long the worker naps when the input ring does not yet
	// hold a full frame. Short enough to keep latency near one frame, long enough
	// that an idle mic does not pin a core. The added end-to-end latency is about
	// one period + one frame (~14ms), as designed.
	workerIdleSleep = 2 * time.Millisecond
)

// micWorker is the DSP worker's state: the real WebRTC APM plus the post-APM hard
// gate. All scratch is preallocated so the hot loop never allocates. One micWorker
// exists per running duplex stream; it is created in startWorker and discarded in
// stopWorker.
//
// The APM does the HPF + noise suppression + automatic gain control in ONE
// ProcessCapture call; the gate is kept only to enforce the PTT/Mute/MicMode hard
// gating (the APM has no hard mute) and the VAD latch, ramping the post-APM frame to
// silence when the mic must be closed. The old onePoleHPF / agcLeveler / denoise
// fields are gone — those stages are now inside the APM.
type micWorker struct {
	e *Engine

	apm  apm.Processor    // real WebRTC APM (HPF+NS+AGC), or no-op when unavailable
	gate *noiseGate       // post-APM hard gate / VAD latch (APM has no hard mute)
	den  denoise.Denoiser // RNNoise (Strong-tier denoiser + speech-probability VAD source)

	// denVAD reports whether den actually provides a trained speech probability:
	// true only when a real RNNoise denoiser was constructed (cgo build AND the
	// native state allocated successfully), false when den fell back to Passthrough.
	// When false the advanced VAD gate falls back to the energy latch, since
	// Passthrough returns p==0 and would otherwise hold the gate permanently shut.
	denVAD bool

	// nsTierApplied / agcApplied / aecApplied track the APM submodule state currently
	// applied so process() re-applies config ONLY when a user toggle changes, never
	// per frame. They mirror the engine's nsTier()/agc()/echoCancellation() atomics.
	nsTierApplied int32
	agcApplied    bool
	aecApplied    bool

	// vadScratch is a preallocated copy buffer: in the None/Standard/High tiers the
	// worker copies the post-APM frame here and runs RNNoise on the COPY purely to
	// read its speech probability, leaving the APM output as the transmitted signal.
	// In the Strong tier RNNoise runs on the real frame instead and this is unused.
	vadScratch [dspFrame]float32

	// noiseFloor / floorSeeded back the automatic-input-sensitivity follower (Discord
	// "Automatically determine input sensitivity"): when auto-sensitivity is on the
	// energy gate's open threshold tracks this slow noise-floor estimate instead of
	// the manual slider. Worker-owned; updated once per frame on the energy path.
	noiseFloor  float32
	floorSeeded bool

	frame [dspFrame]float32 // reused mono scratch, never reallocated

	stop chan struct{}
	wg   sync.WaitGroup
}

// startWorker constructs the DSP worker, wires it to the engine's rings, and
// launches its goroutine. Called from Engine.Start under ctrlMu. The WebRTC APM is
// built once here (off the audio thread, like the old denoise.New) at Discord's
// exact config with the NS/AGC submodules pre-set to the user's current toggles. If
// the APM is unavailable (non-Windows build, or the DLL failed to load) New returns
// a no-op Processor, so the chain degrades to a clean passthrough + the hard gate,
// never a broken pipeline.
func (e *Engine) startWorker() {
	// Build the APM config from the live noise-suppression tier plus the AGC/echo
	// toggles. The tier maps to the APM noise-suppression submodule (none->off,
	// standard->Moderate, high->High, strong->off because RNNoise denoises instead).
	cfg := e.buildAPMConfig()

	proc, err := apm.New(cfg)
	if err != nil {
		// Non-fatal: New returns a no-op Processor alongside the error, so the worker
		// still runs (clean passthrough + hard gate). main logs availability honestly
		// via apm.Available()/apm.LoadError(); we keep the no-op proc here.
		_ = err
	}

	// Build the RNNoise denoiser once, here, OFF the audio thread (like the APM). It
	// serves two roles: the Strong-tier denoiser, and the speech-probability source
	// for the advanced VAD gate in every tier. denoise.New(true) returns the real
	// RNNoise when this binary built with cgo, else Passthrough (p==0). The denoiser
	// runs only on THIS goroutine, never the malgo callback.
	den := denoise.New(true)

	// denVAD MUST reflect the denoiser that was ACTUALLY constructed, not the
	// build-time constant denoise.Available(): even in a cgo build, denoise.New(true)
	// falls back to Passthrough when the native RNNoise state cannot be allocated at
	// runtime (crnnoise.New/rnnoise_create failure). Passthrough.Process always
	// returns p==0, so if denVAD were left true on that fallback the advanced VAD
	// latch would see every frame below closeProb, never open, and silently mute the
	// mic to Discord with no energy-gate safety net. Deriving it from the concrete
	// type means a runtime allocation failure correctly drops the advanced VAD gate
	// back to the energy latch (Passthrough is the only non-VAD Denoiser, so a type
	// assertion is the exact, allocation-free test).
	_, denIsPassthrough := den.(denoise.Passthrough)
	denVAD := denoise.Available() && !denIsPassthrough

	w := &micWorker{
		e:             e,
		apm:           proc,
		gate:          newGate(),
		den:           den,
		denVAD:        denVAD,
		nsTierApplied: e.nsTier(),
		agcApplied:    e.agc(),
		aecApplied:    e.echoCancellation(),
		stop:          make(chan struct{}),
	}
	e.worker = w
	w.wg.Add(1)
	go w.run()
}

// stopWorker signals the worker to exit, waits for it, and frees its native APM.
// Called from Engine.Stop under ctrlMu after the duplex device is uninitialized (so
// no callback is still pushing/pulling). Safe when no worker is running.
func (e *Engine) stopWorker() {
	w := e.worker
	if w == nil {
		return
	}
	close(w.stop)
	w.wg.Wait()
	if w.apm != nil {
		w.apm.Close()
	}
	if w.den != nil {
		w.den.Close()
	}
	e.worker = nil
}

// run is the worker goroutine. It repeatedly tries to pull one full 480-sample
// frame from the input ring; on success it runs the chain and pushes the result to
// the output ring; otherwise it naps briefly and retries. It exits when stop is
// closed.
//
// The idle nap reuses a SINGLE timer created once here rather than calling
// time.After per iteration: time.After allocates a fresh *time.Timer and channel
// on every call, and on an idle/silent mic this loop wakes every ~2ms, so that
// per-iteration allocation is steady-state GC pressure exactly when the mic is
// quiet (the common case). One Reset-able timer keeps the idle path allocation-free.
func (w *micWorker) run() {
	defer w.wg.Done()

	// idle is reused across all idle iterations so the idle path never allocates a
	// timer+channel. Created stopped: this module targets Go 1.23+, whose timer
	// channels are UNBUFFERED and guarantee no stale value survives a Stop/Reset, so
	// we must NOT use the old `if !Stop() { <-C }` drain idiom (it can deadlock).
	// Stop() with no drain is the correct modern pattern.
	idle := time.NewTimer(workerIdleSleep)
	idle.Stop()
	defer idle.Stop()

	for {
		select {
		case <-w.stop:
			return
		default:
		}

		// Pull exactly one frame. The ring's pull is non-blocking; if a full frame
		// is not yet available we nap and retry (bounded wait — never a hard spin).
		if w.e.inRing.length() < dspFrame {
			idle.Reset(workerIdleSleep)
			select {
			case <-w.stop:
				// Go 1.23+ guarantees Stop() drops any pending fire with no stale send,
				// so no channel drain is needed (draining would risk a deadlock).
				idle.Stop()
				return
			case <-idle.C:
				continue
			}
		}
		n := w.e.inRing.pull(w.frame[:])
		if n < dspFrame {
			// Lost the race for the last samples (shouldn't happen with a single
			// consumer, but be defensive): zero the tail so we never process stale
			// scratch, then continue.
			for i := n; i < dspFrame; i++ {
				w.frame[i] = 0
			}
		}

		w.process(w.frame[:])

		// Push the processed frame to the output ring. If the callback is behind and
		// the output ring is full, drop this frame rather than block — the callback
		// will have emitted passthrough anyway, and dropping keeps us from building
		// unbounded latency.
		w.e.outRing.push(w.frame[:])
	}
}

// buildAPMConfig assembles the APM Config from the engine's live noise-suppression
// tier and AGC/echo toggles. It is the single place the tier -> APM noise-suppression
// mapping lives, shared by startWorker (first apply) and process (runtime re-apply).
func (e *Engine) buildAPMConfig() apm.Config {
	cfg := apm.DiscordConfig()
	cfg.GainControlEnabled = e.agc()
	cfg.EchoCancellationEnabled = e.echoCancellation()
	enabled, level := nsConfigForTier(e.nsTier())
	cfg.NoiseSuppressionEnabled = enabled
	cfg.NoiseSuppressionLevel = level
	return cfg
}

// nsConfigForTier maps a noise-suppression tier to the APM noise-suppression
// submodule state. The Strong tier disables APM NS because RNNoise denoises the
// signal instead — the two are NEVER stacked. None also disables it. Standard and
// High map to the WebRTC Moderate/High aggressiveness levels.
func nsConfigForTier(tier int32) (enabled bool, level apm.NSLevel) {
	switch tier {
	case nsTierStandard:
		return true, apm.NSLevelModerate
	case nsTierHigh:
		return true, apm.NSLevelHigh
	default: // nsTierNone, nsTierStrong -> APM NS off
		return false, apm.NSLevelModerate
	}
}

// process runs the mic chain on one 480-sample MONO frame in place, reading every
// control via a single atomic load so a setting change takes effect on the next
// frame. The chain is now:
//
//  1. WebRTC APM ProcessCapture: HPF + noise suppression (at the tier's level, OFF
//     in the Strong tier) + automatic gain control + (parity) echo cancellation. A
//     tier/AGC/echo change is re-applied here (off the audio thread) before the call.
//  2. RNNoise: in the Strong tier it denoises the transmitted signal IN PLACE (our
//     Krisp analog, never stacked with APM NS); in every tier it also yields a trained
//     SPEECH PROBABILITY used by the advanced VAD gate. Outside Strong it runs on a
//     COPY so the APM output stays the transmitted signal.
//  3. Hard gate / VAD: the APM has NO hard mute, so the engine's PTT/Mute/MicMode
//     gating and the gate latch run on the POST-APM signal and ramp the frame to
//     silence when the mic must be closed. In VAD mode the latch is driven by the
//     RNNoise speech probability when Advanced Voice Activity is on (breathing, which
//     has energy but is not speech, keeps the probability low so the gate stays shut),
//     and by the energy/RMS latch otherwise.
//
// Everything here runs on the worker goroutine (never the malgo callback) and uses
// only preallocated scratch — no alloc, lock, or IO in the hot loop.
func (w *micWorker) process(frame []float32) {
	// 1) Re-apply the APM config only when the tier/AGC/echo toggles changed since the
	//    last frame — never per frame. This runs on THIS goroutine (the same one that
	//    calls ProcessCapture), satisfying WebRTC's "no config setter concurrent with
	//    ProcessStream" rule.
	tier := w.e.nsTier()
	ag := w.e.agc()
	aec := w.e.echoCancellation()
	if tier != w.nsTierApplied || ag != w.agcApplied || aec != w.aecApplied {
		w.apm.Reconfigure(w.e.buildAPMConfig())
		w.nsTierApplied = tier
		w.agcApplied = ag
		w.aecApplied = aec
	}

	// 2) Run the real WebRTC capture chain (HPF + NS(tier) + AGC) in place. On a no-op
	//    Processor (APM unavailable) this leaves the frame untouched — clean passthrough.
	w.apm.ProcessCapture(frame)

	// 3) RNNoise. In the Strong tier it denoises the TRANSMITTED frame in place and
	//    returns the speech probability; otherwise it runs on a preallocated COPY purely
	//    to read the probability, leaving the APM output as the transmitted signal. It
	//    is only run when its result is actually needed (Strong always; the VAD gate
	//    when it is the active decision), so quiet/forced modes stay cheap.
	forceOpen, forceClosed := w.gateOverride()
	strong := tier == nsTierStrong
	useVAD := !forceOpen && !forceClosed && w.e.advancedVAD() && w.denVAD
	var p float32
	if w.den != nil {
		if strong {
			p = w.den.Process(frame) // denoise applied to the transmitted signal + p
		} else if useVAD {
			copy(w.vadScratch[:], frame)
			p = w.den.Process(w.vadScratch[:]) // probability only; output discarded
		}
	}

	// 4) Gate latch (skipped when the mode forces the gate open/closed). VAD mode with
	//    Advanced Voice Activity uses the speech-probability latch; everything else uses
	//    the energy latch, whose open threshold is either the manual sensitivity slider
	//    or the automatic noise-floor follower.
	if !forceOpen && !forceClosed {
		if useVAD {
			w.gate.updateLatchVAD(p, vadOpenProbFor(w.e.gateSensitivity()))
		} else {
			energy := rms(frame)
			w.gate.updateLatch(energy, w.energyThreshold(energy))
		}
	}

	// 5) Apply the gate (per-sample ramp, click-free) and publish the open level for
	//    the UI mic-open meter — derived from the signal Discord actually receives.
	level := w.gate.apply(frame, forceOpen, forceClosed)
	w.e.setGateLevel(level)
}

// energyThreshold returns the energy-gate OPEN threshold for this frame. With
// automatic input sensitivity off it is the manual sensitivity slider mapped through
// thresholdFor. With it on, the threshold tracks a slow noise-floor follower (Discord
// "Automatically determine input sensitivity"): open at autoSensMargin times the
// estimated floor, clamped to a sane band so a dead-silent or very loud room still
// gates sensibly. Called only on the energy path; updates the floor as a side effect.
func (w *micWorker) energyThreshold(energy float32) float32 {
	if !w.e.autoSensitivity() {
		return thresholdFor(w.e.gateSensitivity())
	}
	w.updateNoiseFloor(energy)
	auto := w.noiseFloor * autoSensMargin
	if auto < autoSensMinThresh {
		auto = autoSensMinThresh
	}
	if auto > gateMaxThreshold {
		auto = gateMaxThreshold
	}
	return auto
}

// noise-floor follower tuning. The floor adopts a LOWER energy quickly (so it finds
// the true room noise) and creeps UPWARD slowly (so speech does not drag it up and
// jam the gate). The open threshold is autoSensMargin times the floor, never below
// autoSensMinThresh.
const (
	noiseFloorFall    = 0.5
	noiseFloorRise    = 0.0015
	autoSensMargin    = 4.0
	autoSensMinThresh = 0.0015
)

// updateNoiseFloor advances the worker's noise-floor estimate by one frame. Seeded
// from the first observed energy, then tracked with the fast-down / slow-up follower.
// Worker-owned float state; allocation- and lock-free.
func (w *micWorker) updateNoiseFloor(energy float32) {
	if !w.floorSeeded {
		w.noiseFloor = energy
		w.floorSeeded = true
		return
	}
	if energy < w.noiseFloor {
		w.noiseFloor += noiseFloorFall * (energy - w.noiseFloor)
	} else {
		w.noiseFloor += noiseFloorRise * (energy - w.noiseFloor)
	}
}

// gateOverride maps the current MicMode to the gate's force flags:
//
//	vad    -> neither (energy/hysteresis decides)
//	ptt    -> open while the PTT key is held, else closed
//	always -> forced open
//	mute   -> forced closed
//
// Read once per frame via atomic loads so a mode change applies on the next frame.
func (w *micWorker) gateOverride() (forceOpen, forceClosed bool) {
	switch w.e.micMode() {
	case micModeAlways:
		return true, false
	case micModeMute:
		return false, true
	case micModePTT:
		if w.e.pttIsDown() {
			return true, false
		}
		return false, true
	default: // micModeVAD
		return false, false
	}
}
