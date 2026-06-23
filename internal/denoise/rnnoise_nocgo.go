//go:build !cgo

package denoise

// rnnoiseAvailable is false without cgo: RNNoise cannot be linked, so New always
// returns Passthrough and NoiseSuppression is a no-op. The shipping build is
// CGO_ENABLED=1, so this path only matters for cgo-less tooling/builds.
const rnnoiseAvailable = false

// newRNNoise never constructs anything without cgo; it exists only so the
// cgo-tagged and non-cgo builds share the same New/Available surface.
func newRNNoise() Denoiser { return nil }
