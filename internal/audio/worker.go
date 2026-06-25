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

	apm  apm.Processor // real WebRTC APM (HPF+NS+AGC), or no-op when unavailable
	gate *noiseGate    // post-APM hard gate / VAD latch (APM has no hard mute)

	// nsApplied / agcApplied track the NS/AGC submodule state currently applied to
	// the APM so process() re-applies config ONLY when the user toggles change,
	// never per frame. They mirror the engine's noiseSuppression()/agc() atomics.
	nsApplied  bool
	agcApplied bool

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
	// Build the Discord-exact config, then fold in the user's live NS/AGC toggles
	// (the UI "Noise suppression" -> APM NS at Moderate, "AGC" -> APM gain control).
	cfg := apm.DiscordConfig()
	ns := e.noiseSuppression()
	ag := e.agc()
	cfg.NoiseSuppressionEnabled = ns
	cfg.GainControlEnabled = ag

	proc, err := apm.New(cfg)
	if err != nil {
		// Non-fatal: New returns a no-op Processor alongside the error, so the worker
		// still runs (clean passthrough + hard gate). main logs availability honestly
		// via apm.Available()/apm.LoadError(); we keep the no-op proc here.
		_ = err
	}

	w := &micWorker{
		e:          e,
		apm:        proc,
		gate:       newGate(),
		nsApplied:  ns,
		agcApplied: ag,
		stop:       make(chan struct{}),
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

// process runs the mic chain on one 480-sample MONO frame in place, reading every
// control via a single atomic load so a setting change takes effect on the next
// frame. The chain is now:
//
//  1. WebRTC APM ProcessCapture: HPF + noise suppression + automatic gain control
//     in one call, at Discord's exact config. The UI's "Noise suppression" and
//     "AGC" toggles map to the APM's NS and gain-control submodules; a toggle change
//     is re-applied here (off the audio thread) before processing the frame.
//  2. Hard gate: the APM has NO hard mute, so the engine's PTT/Mute/MicMode gating
//     and VAD latch run on the POST-APM signal and ramp the frame to silence when
//     the mic must be closed. The VAD energy is the post-APM RMS (after NS/AGC), so
//     the UI GateLevel reflects the signal Discord actually receives.
func (w *micWorker) process(frame []float32) {
	// 1) Re-apply the APM config only when the NS/AGC toggles changed since the last
	//    frame — never per frame. This runs on THIS goroutine (the same one that calls
	//    ProcessCapture), satisfying WebRTC's "no config setter concurrent with
	//    ProcessStream" rule. UI "Noise suppression" -> APM NS (Moderate); UI "AGC" ->
	//    APM gain control.
	ns := w.e.noiseSuppression()
	ag := w.e.agc()
	if ns != w.nsApplied || ag != w.agcApplied {
		cfg := apm.DiscordConfig()
		cfg.NoiseSuppressionEnabled = ns
		cfg.GainControlEnabled = ag
		w.apm.Reconfigure(cfg)
		w.nsApplied = ns
		w.agcApplied = ag
	}

	// 2) Run the real WebRTC capture chain (HPF + NS + AGC) in place. On a no-op
	//    Processor (APM unavailable) this leaves the frame untouched — clean
	//    passthrough — and the hard gate below still applies.
	w.apm.ProcessCapture(frame)

	// 3) Measure the POST-APM energy for the gate's VAD latch and the UI meter. The
	//    APM has already done HPF/NS/AGC, so this RMS is the level Discord receives.
	energy := rms(frame)

	// 4) Hard gate / VAD: behavior depends on MicMode. The energy/hysteresis latch is
	//    only consulted in VAD mode; the other modes force the gate open or closed.
	//    This is the PTT/Mute/MicMode hard gating the APM cannot do.
	forceOpen, forceClosed := w.gateOverride()
	if !forceOpen && !forceClosed {
		w.gate.updateLatch(energy, thresholdFor(w.e.gateSensitivity()))
	}
	level := w.gate.apply(frame, forceOpen, forceClosed)

	// Publish the gate-open level (derived from the post-APM RMS) for the UI meter.
	w.e.setGateLevel(level)
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
