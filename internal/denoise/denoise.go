// Package denoise provides the mic-path noise suppressor for the audio suite,
// behind a small Denoiser interface so the rest of the engine never depends on
// whether RNNoise actually built.
//
// Two implementations exist:
//   - rnnoise (cgo, internal/denoise/crnnoise): the real Xiph RNNoise network.
//     Selected by New() when this binary was built with cgo (CGO_ENABLED=1).
//   - Passthrough: a zero-cost no-op used as the fallback when cgo is OFF, and
//     used directly whenever NoiseSuppression is disabled at runtime.
//
// EVERYTHING in the suite is built to work with Passthrough: noise suppression
// is the only piece that depends on RNNoise, and toggling it off (or building
// without cgo) degrades to clean passthrough, never a broken pipeline.
//
// FrameSize is fixed at 480 mono samples (10ms @48kHz) because that is RNNoise's
// hard requirement; the engine's DSP worker is responsible for chopping the mic
// stream into exactly-480 frames before calling Process.
package denoise

// FrameSize is the exact number of MONO samples a Denoiser processes per call
// (480 == 10ms at 48kHz). The worker must hand Process frames of this length.
const FrameSize = 480

// Denoiser suppresses noise on one 480-sample MONO frame IN PLACE. Implementations
// are single-goroutine objects (create one per mic stream; call Process only from
// the DSP worker). Process returns a voice-activity probability in [0,1] when the
// implementation provides one (RNNoise does); Passthrough returns 0 and leaves the
// frame untouched. Implementations must be allocation-free and lock-free in
// Process so they are safe to drive from the near-real-time worker.
type Denoiser interface {
	// Process denoises frame (len == FrameSize) in place and returns a VAD hint in
	// [0,1] (0 when not available).
	Process(frame []float32) float32
	// Close releases any native resources. Safe to call exactly once; further use
	// of the Denoiser after Close is a no-op.
	Close()
}

// Passthrough is the no-op Denoiser: it leaves the frame unchanged and reports no
// voice-activity estimate. It is the fallback when cgo/RNNoise is unavailable and
// the active denoiser whenever the user has NoiseSuppression turned off, so the
// rest of the processing chain (HPF, AGC, gate) runs identically either way.
type Passthrough struct{}

// Process returns 0 and does not touch frame.
func (Passthrough) Process(frame []float32) float32 { return 0 }

// Close is a no-op; Passthrough holds no resources.
func (Passthrough) Close() {}

// Available reports whether a real RNNoise-backed Denoiser can be constructed in
// this build (true only when compiled with cgo). When false, New always returns
// Passthrough and the UI/engine should treat NoiseSuppression as a no-op. It lets
// main log an honest "noise suppression unavailable (built without cgo)" line
// instead of silently degrading.
func Available() bool { return rnnoiseAvailable }

// New returns a Denoiser. When suppress is true AND a real RNNoise implementation
// is available in this build, it returns the RNNoise-backed denoiser; otherwise
// it returns Passthrough. The engine constructs both states up front and chooses
// which to run per buffer from the NoiseSuppression atomic, so this factory is
// mainly for callers that want a single denoiser honoring the current setting.
func New(suppress bool) Denoiser {
	if suppress && rnnoiseAvailable {
		if d := newRNNoise(); d != nil {
			return d
		}
	}
	return Passthrough{}
}
