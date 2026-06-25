//go:build !windows

package apm

// apm_other.go is the non-Windows fallback. The WebRTC APM is shipped only as a
// Windows DLL, so on any other OS no real Processor exists: Available() is false
// and New returns the no-op Processor (clean passthrough), mirroring how the
// denoise package degrades when RNNoise is unavailable. The whole module still
// compiles and the audio engine still runs — it just does not apply the APM chain.

import "errors"

// errUnavailable is returned by New/LoadError off Windows.
var errUnavailable = errors.New("apm: WebRTC APM is only available on Windows")

// Available always reports false off Windows.
func Available() bool { return false }

// LoadError reports why the APM is unavailable off Windows.
func LoadError() error { return errUnavailable }

// New returns the no-op Processor and the unavailable error off Windows. Callers
// fall back to passthrough.
func New(cfg Config) (Processor, error) {
	_ = cfg
	return noopProcessor{}, errUnavailable
}
