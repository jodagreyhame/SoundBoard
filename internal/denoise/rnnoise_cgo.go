//go:build cgo

package denoise

import "soundboard/internal/denoise/crnnoise"

// rnnoiseAvailable is true in cgo builds: the vendored RNNoise C library is
// linked in, so a real denoiser can be constructed. The !cgo build sets this
// false and New degrades to Passthrough.
const rnnoiseAvailable = true

// rnnoise adapts a crnnoise.State to the Denoiser interface. One per mic stream.
type rnnoise struct {
	st *crnnoise.State
}

// newRNNoise creates an RNNoise-backed denoiser, or nil if the native state
// could not be allocated (caller falls back to Passthrough).
func newRNNoise() Denoiser {
	st := crnnoise.New()
	if st == nil {
		return nil
	}
	return &rnnoise{st: st}
}

// Process denoises one 480-sample mono frame in place via RNNoise. The +/-32768
// scaling trap is handled inside crnnoise.State.Process; callers pass normalized
// [-1,1] samples.
func (r *rnnoise) Process(frame []float32) float32 {
	if r.st == nil {
		return 0
	}
	return r.st.Process(frame)
}

// Close frees the native RNNoise state.
func (r *rnnoise) Close() {
	if r.st != nil {
		r.st.Destroy()
		r.st = nil
	}
}
