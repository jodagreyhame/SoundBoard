package audio

// parity_vad.go adds the task-named engine setters that are thin aliases over the
// canonical control surface in processing.go, so callers that speak in Discord's
// vocabulary (a noise-suppression LEVEL, an "Attenuation" toggle, an "Automatically
// determine input sensitivity" toggle) get a method by that exact name without a
// second source of truth. Each delegates to the authoritative setter; the engine
// stores every value in one atomic, read once per worker frame (RT-safe).

import (
	"soundboard/internal/apm"
	"soundboard/internal/config"
)

// SetNoiseSuppressionLevel selects the APM noise-suppression aggressiveness by its
// WebRTC level and maps it onto the engine's noise-suppression TIER (the engine's
// single source of truth). Low/Moderate -> the "standard" tier (APM NS Moderate),
// High/VeryHigh -> the "high" tier (APM NS High, the breathing-kill default). It
// never selects "none" or "strong" — those are reached via SetNoiseSuppressionTier,
// since they are denoiser-PATH choices rather than an APM level. Safe from any
// goroutine; the worker re-applies the APM config on its next frame.
func (e *Engine) SetNoiseSuppressionLevel(level apm.NSLevel) {
	switch level {
	case apm.NSLevelHigh, apm.NSLevelVeryHigh:
		e.SetNoiseSuppressionTier(config.NSModeHigh)
	default: // NSLevelLow, NSLevelModerate
		e.SetNoiseSuppressionTier(config.NSModeStandard)
	}
}

// SetAttenuation toggles attenuation — Discord's name for ducking the soundboard
// under an open mic gate. It is the parity-named alias of SetDucking; the depth is
// SetAttenuationAmount. Safe from any goroutine.
func (e *Engine) SetAttenuation(on bool) { e.SetDucking(on) }

// SetInputSensitivityAuto toggles "Automatically determine input sensitivity"
// (Discord's name). It is the parity-named alias of SetAutoSensitivity: when on the
// gate threshold tracks the worker's noise-floor follower; when off the manual
// GateSensitivity slider is used. Safe from any goroutine.
func (e *Engine) SetInputSensitivityAuto(on bool) { e.SetAutoSensitivity(on) }
