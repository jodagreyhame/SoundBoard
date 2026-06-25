// Package apm wraps the real WebRTC AudioProcessingModule (APM) and exposes a
// tiny Go surface — a single ProcessCapture call over one 480-sample mono frame —
// so the mic-path worker can match Discord's capture chain EXACTLY (high-pass
// filter, noise suppression, and automatic gain control) with the same DSP
// library Discord itself uses.
//
// HOW IT LINKS: the APM is a self-contained WebRTC build shipped as a Windows DLL
// (webrtc-apm.dll, BSD-3-Clause, the SoundFlow.Extensions.WebRtc.Apm /
// LSXPrime/webrtc-audio-processing lineage). It is embedded into the binary and
// loaded at RUNTIME via windows.LoadDLL — the C++ APM is therefore NEVER linked at
// cgo compile time, so the whole module keeps building with the default MinGW gcc
// toolchain (`CGO_ENABLED=1 go build ./...`), untouched. There is no clang/MSVC
// requirement and no loose DLL to ship: the embedded copy is extracted to a
// per-process temp file the first time New is called.
//
// AVAILABILITY: Available() reports whether a real Processor can be built in this
// binary on this OS. On non-Windows builds (and if the DLL ever fails to load) New
// returns a no-op Processor and Available() is false, so the worker degrades to
// clean passthrough rather than a broken pipeline — exactly the contract the
// denoise package already follows for RNNoise.
package apm

// FrameSize is the exact number of MONO samples ProcessCapture consumes per call:
// 480 == 10 ms at 48 kHz, which is what WebRTC APM's GetFrameSize(48000) returns
// and the frame size the worker already chops the mic stream into. ProcessCapture
// requires frames of exactly this length.
const FrameSize = 480

// SampleRate is the canonical capture sample rate (48 kHz) the APM is initialized
// for. The whole engine runs at 48 kHz, so the APM never resamples.
const SampleRate = 48000

// NSLevel is the WebRTC noise-suppression aggressiveness. The values mirror the C
// ABI enum webrtc_apm_ns_level (Low/Moderate/High/VeryHigh). Discord uses
// Moderate, which NSLevelModerate names.
type NSLevel int

const (
	NSLevelLow      NSLevel = 0 // WEBRTC_APM_NS_LOW
	NSLevelModerate NSLevel = 1 // WEBRTC_APM_NS_MODERATE (Discord)
	NSLevelHigh     NSLevel = 2 // WEBRTC_APM_NS_HIGH
	NSLevelVeryHigh NSLevel = 3 // WEBRTC_APM_NS_VERY_HIGH
)

// Config is the Discord-exact APM configuration applied at construction. Only the
// top-level toggles Discord actually sets are exposed; everything else is left at
// the WebRTC library defaults (the task's "accept library defaults — do not invent
// sub-values" rule), which is precisely how Discord configures the module.
//
// The Discord-matching values are:
//
//	HighPassFilterEnabled: true
//	NoiseSuppressionEnabled: true, NoiseSuppressionLevel: NSLevelModerate
//	GainControlEnabled: true   (AGC2 adaptive-digital + limiter, library defaults)
//	EchoCancellationEnabled: false  (server-side mix; no far-end render reference)
//	CaptureChannels: 1, RenderChannels: 1
//
// DiscordConfig() returns exactly this.
type Config struct {
	HighPassFilterEnabled   bool
	NoiseSuppressionEnabled bool
	NoiseSuppressionLevel   NSLevel
	GainControlEnabled      bool
	EchoCancellationEnabled bool
	CaptureChannels         int
	RenderChannels          int
}

// DiscordConfig returns the APM configuration that matches Discord's capture
// chain exactly: HPF on, noise suppression on at Moderate, gain control on, echo
// cancellation off, mono capture and render. The worker passes this to New; the NS
// and AGC toggles are then flipped per the user's UI settings before construction.
func DiscordConfig() Config {
	return Config{
		HighPassFilterEnabled:   true,
		NoiseSuppressionEnabled: true,
		NoiseSuppressionLevel:   NSLevelModerate,
		GainControlEnabled:      true,
		EchoCancellationEnabled: false,
		CaptureChannels:         1,
		RenderChannels:          1,
	}
}

// Processor runs the WebRTC capture chain on one mic frame at a time. It is a
// single-goroutine object: build one per mic stream and call ProcessCapture only
// from the DSP worker, never concurrently. ProcessCapture is allocation-free and
// lock-free after construction so it is safe to drive from the near-real-time
// worker hot loop (NOT from a malgo callback).
type Processor interface {
	// ProcessCapture runs the configured APM submodules (HPF, NS, AGC) over frame
	// (len must be FrameSize) IN PLACE, returning the APM error code (0 == success;
	// negative values mirror webrtc_apm_error). A no-op Processor leaves the frame
	// untouched and returns 0.
	ProcessCapture(frame []float32) int

	// Reconfigure re-applies cfg to the live APM instance, returning the APM error
	// code (0 == success). It is how the worker flips the NS / AGC toggles at runtime
	// (e.g. the user unchecks Noise suppression) without rebuilding the Processor.
	//
	// WebRTC's APM is thread-safe with one assumption: the config setters are never
	// called CONCURRENTLY with ProcessStream. The worker satisfies this by calling
	// Reconfigure from the same goroutine that calls ProcessCapture (between frames),
	// so it is safe. A no-op Processor returns 0. The capture/render channel counts in
	// cfg are ignored here (the stream config is fixed at construction); only the
	// submodule toggles are re-applied.
	Reconfigure(cfg Config) int

	// Close releases the native APM instance. Safe to call exactly once; further use
	// after Close is a no-op.
	Close()
}

// noopProcessor is the fallback Processor used when no real APM is available
// (non-Windows build, or the DLL failed to load). It leaves every frame untouched,
// so the worker degrades to clean passthrough rather than a broken chain — the
// same "never a broken pipeline" guarantee denoise.Passthrough gives.
type noopProcessor struct{}

func (noopProcessor) ProcessCapture(frame []float32) int { return 0 }
func (noopProcessor) Reconfigure(cfg Config) int         { return 0 }
func (noopProcessor) Close()                             {}
