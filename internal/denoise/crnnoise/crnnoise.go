// Package crnnoise is the cgo bridge to the vendored RNNoise C library (Xiph /
// Gregor Richards 2018 model, BSD-licensed — see LICENSE in this directory).
//
// It is a thin, allocation-aware wrapper exposing exactly what the audio suite
// needs: create a per-stream denoiser state, process one fixed 480-sample (10ms
// @48kHz) MONO frame, and destroy it. The trained model is COMPILED IN
// (rnn_data.c) so there is no runtime model file to ship or load.
//
// SCALING TRAP: rnnoise_process_frame expects float samples in the +/-32768
// range (it was written for 16-bit PCM amplitudes), NOT normalized [-1,1]. The
// Go layer that calls Process is responsible for multiplying by 32768 on the way
// in and dividing by 32768 on the way out; if you feed it [-1,1] it silently
// no-ops (every input rounds to ~0 energy and the network passes it through).
//
// The C sources are compiled by cgo directly from this directory; the rnn_reader
// model-from-file path is deliberately NOT vendored (it pulls in an unconditional
// config.h), since the bundled model is all we use.
package crnnoise

/*
#cgo CFLAGS: -I${SRCDIR} -O2 -DNDEBUG
#cgo LDFLAGS: -lm

#include <stdlib.h>
#include "rnnoise.h"
*/
import "C"

// FrameSize is the exact number of MONO samples RNNoise processes per call
// (480 == 10ms at 48kHz). Process panics if given a different length, because
// the C call would read/write out of bounds otherwise.
const FrameSize = 480

// scale converts the engine's normalized [-1,1] float samples to the +/-32768
// range RNNoise expects (and back). Exported so callers and tests use the exact
// same constant the trap requires.
const scale = 32768.0

// State wraps a single RNNoise DenoiseState. One State is NOT safe for
// concurrent use; create one per stream (here: one per mono mic path) and call
// Process from a single goroutine (the DSP worker).
type State struct {
	st *C.DenoiseState
	// in/out are reused C-range scratch buffers so Process is allocation-free in
	// steady state. They hold the *32768-scaled samples handed to the C call.
	in  [FrameSize]C.float
	out [FrameSize]C.float
}

// New creates a denoiser state using the compiled-in trained model. Returns nil
// only if the C allocation fails. Call Destroy when done to free the C state.
func New() *State {
	st := C.rnnoise_create(nil)
	if st == nil {
		return nil
	}
	return &State{st: st}
}

// Process denoises one 480-sample MONO frame IN PLACE. frame must be exactly
// FrameSize samples in normalized [-1,1]; it is scaled to +/-32768 for the C
// call and scaled back into frame on return. The return value is RNNoise's
// voice-activity probability for this frame in [0,1] (the caller may use it as a
// VAD hint; the suite's gate has its own RMS path and does not require it).
//
// Allocation-free and lock-free: the scaled samples live in the reused C-range
// scratch arrays on the State. Safe only from the single goroutine that owns
// this State.
func (s *State) Process(frame []float32) float32 {
	if s == nil || s.st == nil {
		return 0
	}
	if len(frame) != FrameSize {
		panic("crnnoise: Process requires exactly 480 samples")
	}
	for i := 0; i < FrameSize; i++ {
		s.in[i] = C.float(frame[i] * scale)
	}
	vad := C.rnnoise_process_frame(s.st, &s.out[0], &s.in[0])
	for i := 0; i < FrameSize; i++ {
		frame[i] = float32(s.out[i]) / scale
	}
	return float32(vad)
}

// Destroy frees the underlying C state. Safe to call once; using the State after
// Destroy is a no-op (Process returns 0).
func (s *State) Destroy() {
	if s == nil || s.st == nil {
		return
	}
	C.rnnoise_destroy(s.st)
	s.st = nil
}
