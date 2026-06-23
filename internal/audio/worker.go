package audio

// worker.go runs the heavy mic-path DSP off the real-time audio thread.
//
// WHY a worker: RNNoise only accepts exactly 480-sample (10ms) MONO frames and is
// far too expensive to run inside the malgo callback (whose period is 192 STEREO
// frames and which must never allocate, lock, or block). So duplexCallback only
// shoves mono mic samples into the input ring and pulls processed mono out of the
// output ring; THIS goroutine does the real work in between:
//
//	input ring  --pull 480--> [ HPF -> denoise -> AGC -> gate ] --push--> output ring
//
// The worker may briefly sleep when the input ring has less than a full frame
// (bounded wait, never a hard busy-spin), but it NEVER blocks the RT callback: the
// rings are lock-free and the callback's push/pull are non-blocking. If the worker
// ever falls behind, the output ring simply runs dry and the callback emits mic
// passthrough for that period (see duplexCallback) — added latency, never a stall.
//
// Lifecycle: startWorker (from Engine.Start) launches the goroutine; stopWorker
// (from Engine.Stop) signals it to exit and waits for it, then frees the native
// denoiser. Both are called under ctrlMu, off the audio thread.

import (
	"sync"
	"time"

	"soundboard/internal/denoise"
)

const (
	// dspFrame is the worker's fixed processing block: 480 mono samples (10ms at
	// 48kHz), RNNoise's mandatory frame size. Everything in the chain runs on
	// exactly this length.
	dspFrame = denoise.FrameSize

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

// micWorker is the DSP worker's state: the per-stream filter/leveler/gate plus a
// real RNNoise denoiser (or Passthrough if cgo/RNNoise is unavailable). All
// scratch is preallocated so the hot loop never allocates. One micWorker exists
// per running duplex stream; it is created in startWorker and discarded in
// stopWorker.
type micWorker struct {
	e *Engine

	hpf  *onePoleHPF
	agc  *agcLeveler
	gate *noiseGate
	den  denoise.Denoiser // RNNoise when available, else Passthrough

	frame [dspFrame]float32 // reused mono scratch, never reallocated

	stop chan struct{}
	wg   sync.WaitGroup
}

// startWorker constructs the DSP worker, wires it to the engine's rings, and
// launches its goroutine. Called from Engine.Start under ctrlMu. The denoiser is
// built once here (off the audio thread); it is a real RNNoise network in cgo
// builds and a no-op Passthrough otherwise, so the chain is identical either way
// and toggling NoiseSuppression at runtime just decides whether den.Process is
// called per frame.
func (e *Engine) startWorker() {
	w := &micWorker{
		e:    e,
		hpf:  newHPF(80),
		agc:  newAGC(),
		gate: newGate(),
		// Build the strongest denoiser this binary supports; the per-frame
		// NoiseSuppression atomic decides whether we actually run it.
		den:  denoise.New(true),
		stop: make(chan struct{}),
	}
	e.worker = w
	w.wg.Add(1)
	go w.run()
}

// stopWorker signals the worker to exit, waits for it, and frees its native
// denoiser. Called from Engine.Stop under ctrlMu after the duplex device is
// uninitialized (so no callback is still pushing/pulling). Safe when no worker is
// running.
func (e *Engine) stopWorker() {
	w := e.worker
	if w == nil {
		return
	}
	close(w.stop)
	w.wg.Wait()
	w.den.Close()
	e.worker = nil
}

// run is the worker goroutine. It repeatedly tries to pull one full 480-sample
// frame from the input ring; on success it runs the chain and pushes the result to
// the output ring; otherwise it naps briefly and retries. It exits when stop is
// closed.
func (w *micWorker) run() {
	defer w.wg.Done()
	for {
		select {
		case <-w.stop:
			return
		default:
		}

		// Pull exactly one frame. The ring's pull is non-blocking; if a full frame
		// is not yet available we nap and retry (bounded wait — never a hard spin).
		if w.e.inRing.length() < dspFrame {
			select {
			case <-w.stop:
				return
			case <-time.After(workerIdleSleep):
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

// process runs the full mic chain on one 480-sample MONO frame in place, reading
// every control via a single atomic load so a setting change takes effect on the
// next frame. Order: HPF -> denoise (optional) -> AGC (optional) -> gate. It
// publishes the resulting gate-open level for the UI meter.
func (w *micWorker) process(frame []float32) {
	// 1) High-pass: always on. Removes DC/rumble below ~80Hz so denoise and the
	//    gate's RMS see a clean band.
	w.hpf.process(frame)

	// 2) Denoise: only when NoiseSuppression is enabled (and a real RNNoise is
	//    linked). Passthrough is a no-op, so skipping the call when off is purely an
	//    optimization. RNNoise handles its own +/-32768 scaling internally.
	if w.e.noiseSuppression() {
		w.den.Process(frame)
	}

	// 3) AGC: only when enabled. process returns the pre-gain RMS so the gate keys
	//    off the clean energy, not the boosted one. When AGC is off, measure RMS
	//    directly for the gate.
	var energy float32
	if w.e.agc() {
		energy = w.agc.process(frame)
	} else {
		energy = rms(frame)
	}

	// 4) Gate / VAD: behavior depends on MicMode. The energy/hysteresis latch is
	//    only consulted in VAD mode; the other modes force the gate open or closed.
	forceOpen, forceClosed := w.gateOverride()
	if !forceOpen && !forceClosed {
		w.gate.updateLatch(energy, thresholdFor(w.e.gateSensitivity()))
	}
	level := w.gate.apply(frame, forceOpen, forceClosed)

	// Publish the gate-open level for the UI mic-open meter.
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
