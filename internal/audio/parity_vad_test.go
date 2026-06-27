package audio

import (
	"testing"

	"soundboard/internal/apm"
)

// TestSetNoiseSuppressionTierMapping pins the config-string -> tier encoding,
// including the "unknown -> high" fallback (the breathing-kill default) so a
// malformed setting still suppresses aggressively rather than leaving NS off.
func TestSetNoiseSuppressionTierMapping(t *testing.T) {
	e := NewEngine(nil, nil)
	cases := map[string]int32{
		"none":     nsTierNone,
		"standard": nsTierStandard,
		"high":     nsTierHigh,
		"strong":   nsTierStrong,
		"":         nsTierHigh, // unknown -> default high
		"garbage":  nsTierHigh,
	}
	for in, want := range cases {
		e.SetNoiseSuppressionTier(in)
		if got := e.nsTier(); got != want {
			t.Errorf("SetNoiseSuppressionTier(%q): tier = %d, want %d", in, got, want)
		}
	}
}

// TestSetNoiseSuppressionLevelMapping confirms the task-named level setter maps the
// APM aggressiveness onto the engine tier: Low/Moderate -> standard, High/VeryHigh ->
// high. (None and Strong are denoiser-path choices, reached via the tier setter.)
func TestSetNoiseSuppressionLevelMapping(t *testing.T) {
	e := NewEngine(nil, nil)
	cases := []struct {
		level apm.NSLevel
		want  int32
	}{
		{apm.NSLevelLow, nsTierStandard},
		{apm.NSLevelModerate, nsTierStandard},
		{apm.NSLevelHigh, nsTierHigh},
		{apm.NSLevelVeryHigh, nsTierHigh},
	}
	for _, c := range cases {
		e.SetNoiseSuppressionLevel(c.level)
		if got := e.nsTier(); got != c.want {
			t.Errorf("SetNoiseSuppressionLevel(%d): tier = %d, want %d", c.level, got, c.want)
		}
	}
}

// TestNSConfigForTierNoStack is the "strong tier never stacks" guard: the Strong tier
// must produce an APM config with noise suppression OFF (RNNoise denoises instead),
// while Standard/High map to the WebRTC Moderate/High levels and None disables it.
func TestNSConfigForTierNoStack(t *testing.T) {
	cases := []struct {
		tier        int32
		wantEnabled bool
		wantLevel   apm.NSLevel
	}{
		{nsTierNone, false, apm.NSLevelModerate},
		{nsTierStandard, true, apm.NSLevelModerate},
		{nsTierHigh, true, apm.NSLevelHigh},
		{nsTierStrong, false, apm.NSLevelModerate}, // APM NS OFF: never stacked with RNNoise
	}
	for _, c := range cases {
		enabled, level := nsConfigForTier(c.tier)
		if enabled != c.wantEnabled {
			t.Errorf("nsConfigForTier(%d) enabled = %v, want %v", c.tier, enabled, c.wantEnabled)
		}
		if enabled && level != c.wantLevel {
			t.Errorf("nsConfigForTier(%d) level = %d, want %d", c.tier, level, c.wantLevel)
		}
	}

	// And via the engine: selecting the Strong tier yields an APM config with NS off
	// while keeping HPF/AGC available (the non-NS APM stages still run).
	e := NewEngine(nil, nil)
	e.SetNoiseSuppressionTier("strong")
	if cfg := e.buildAPMConfig(); cfg.NoiseSuppressionEnabled {
		t.Fatal("Strong tier engine config must have NoiseSuppressionEnabled=false (no stacking)")
	}
}

// TestBuildAPMConfigReflectsToggles confirms the worker's APM config builder folds in
// the live AGC and echo-cancellation toggles and the tier's NS level.
func TestBuildAPMConfigReflectsToggles(t *testing.T) {
	e := NewEngine(nil, nil)
	e.SetNoiseSuppressionTier("high")
	e.SetAGC(true)
	e.SetEchoCancellation(true)
	cfg := e.buildAPMConfig()
	if !cfg.NoiseSuppressionEnabled || cfg.NoiseSuppressionLevel != apm.NSLevelHigh {
		t.Fatalf("high tier should enable NS at High, got enabled=%v level=%d", cfg.NoiseSuppressionEnabled, cfg.NoiseSuppressionLevel)
	}
	if !cfg.GainControlEnabled {
		t.Fatal("AGC toggle should flow into the APM config")
	}
	if !cfg.EchoCancellationEnabled {
		t.Fatal("EchoCancellation toggle should flow into the APM config")
	}
	if !cfg.HighPassFilterEnabled {
		t.Fatal("HPF should stay on (Discord-exact base config)")
	}
}

// TestParityAliasSetters confirms the Discord-named alias setters drive the same
// engine state as their canonical counterparts.
func TestParityAliasSetters(t *testing.T) {
	e := NewEngine(nil, nil)

	e.SetAttenuation(true)
	if !e.ducking() {
		t.Error("SetAttenuation(true) should enable ducking")
	}
	e.SetAttenuation(false)
	if e.ducking() {
		t.Error("SetAttenuation(false) should disable ducking")
	}

	e.SetInputSensitivityAuto(true)
	if !e.autoSensitivity() {
		t.Error("SetInputSensitivityAuto(true) should enable auto-sensitivity")
	}
	e.SetInputSensitivityAuto(false)
	if e.autoSensitivity() {
		t.Error("SetInputSensitivityAuto(false) should disable auto-sensitivity")
	}

	e.SetEchoCancellation(true)
	if !e.echoCancellation() {
		t.Error("SetEchoCancellation(true) not reflected")
	}
	e.SetAdvancedVoiceActivity(true)
	if !e.advancedVAD() {
		t.Error("SetAdvancedVoiceActivity(true) not reflected")
	}
}

// TestAttenuationAmountDefaultClampAndDuck confirms the attenuation amount defaults to
// the historical 0.65 depth, clamps to [0,1] (NaN -> 0), and actually drives the
// ducking depth: a smaller amount ducks the soundboard LESS than a larger one.
func TestAttenuationAmountDefaultClampAndDuck(t *testing.T) {
	e := NewEngine(nil, nil)
	if got := e.attenuationAmount(); got != defaultAttenAmount {
		t.Fatalf("default attenuationAmount = %v, want %v", got, defaultAttenAmount)
	}

	nan := float32(0)
	nan = nan / nan
	for _, c := range []struct{ in, want float32 }{
		{0.5, 0.5}, {-1, 0}, {2, 1}, {nan, 0},
	} {
		e.SetAttenuationAmount(c.in)
		if got := e.attenuationAmount(); got != c.want {
			t.Errorf("SetAttenuationAmount(%v) = %v, want %v", c.in, got, c.want)
		}
	}

	// The amount drives the duck depth. Open mic gate + ducking on; converge the
	// envelope, then compare the ducked master at two amounts.
	duckedAt := func(amount float32) float32 {
		e := NewEngine(nil, nil)
		e.SetMasterGain(1)
		e.setGateLevel(1)
		e.SetDucking(true)
		e.SetAttenuationAmount(amount)
		var last float32
		for i := 0; i < 200; i++ {
			last = e.duckedMaster()
		}
		return last
	}
	shallow := duckedAt(0.3) // ducks little -> stays HIGHER
	deep := duckedAt(0.9)    // ducks hard -> drops LOWER
	if !(shallow > deep) {
		t.Fatalf("a larger attenuation amount must duck the soundboard lower: shallow(0.3)=%v deep(0.9)=%v", shallow, deep)
	}
}
