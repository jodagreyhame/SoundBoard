package audio

import "testing"

// TestProcessingDefaults confirms a freshly constructed Engine starts in VAD mode
// at the default gate sensitivity with every optional feature off and the gate
// meter reading 0 — the "clean voice, no carrier" baseline that mirrors config.
func TestProcessingDefaults(t *testing.T) {
	e := NewEngine(nil, nil)
	if got := e.micMode(); got != micModeVAD {
		t.Errorf("default micMode = %d, want %d (vad)", got, micModeVAD)
	}
	if got := e.gateSensitivity(); got != defaultGateSensitivity {
		t.Errorf("default gateSensitivity = %v, want %v", got, defaultGateSensitivity)
	}
	if e.noiseSuppression() || e.agc() || e.ducking() || e.forceThrough() || e.pttIsDown() {
		t.Error("default processing toggles should all be off")
	}
	if got := e.GateLevel(); got != 0 {
		t.Errorf("default GateLevel = %v, want 0", got)
	}
}

// TestSetMicModeMapping pins the config-string -> RT-int encoding, including the
// "unknown -> vad" fallback so a malformed setting never leaves the gate in an
// undefined mode.
func TestSetMicModeMapping(t *testing.T) {
	e := NewEngine(nil, nil)
	cases := map[string]int32{
		"vad":     micModeVAD,
		"ptt":     micModePTT,
		"always":  micModeAlways,
		"mute":    micModeMute,
		"":        micModeVAD, // empty -> default
		"garbage": micModeVAD, // unknown -> default
	}
	for in, want := range cases {
		e.SetMicMode(in)
		if got := e.micMode(); got != want {
			t.Errorf("SetMicMode(%q): micMode = %d, want %d", in, got, want)
		}
	}
}

// TestSetGateSensitivityClamps confirms the threshold is clamped to [0,1] and
// NaN maps to 0, so the gate never sees an out-of-range value.
func TestSetGateSensitivityClamps(t *testing.T) {
	e := NewEngine(nil, nil)
	nan := float32(0)
	nan = nan / nan // produce a NaN without importing math
	cases := []struct {
		in   float32
		want float32
	}{
		{0.3, 0.3},
		{-1, 0},
		{2, 1},
		{nan, 0},
	}
	for _, c := range cases {
		e.SetGateSensitivity(c.in)
		if got := e.gateSensitivity(); got != c.want {
			t.Errorf("SetGateSensitivity(%v): got %v, want %v", c.in, got, c.want)
		}
	}
}

// TestProcessingSettersRoundTrip exercises each bool setter and the PTT/gate-level
// publish path, proving the atomic readers reflect the latest written value.
func TestProcessingSettersRoundTrip(t *testing.T) {
	e := NewEngine(nil, nil)

	e.SetNoiseSuppression(true)
	if !e.noiseSuppression() {
		t.Error("SetNoiseSuppression(true) not reflected")
	}
	e.SetAGC(true)
	if !e.agc() {
		t.Error("SetAGC(true) not reflected")
	}
	e.SetDucking(true)
	if !e.ducking() {
		t.Error("SetDucking(true) not reflected")
	}
	e.SetForceThrough(true)
	if !e.forceThrough() {
		t.Error("SetForceThrough(true) not reflected")
	}
	e.SetPTTDown(true)
	if !e.pttIsDown() {
		t.Error("SetPTTDown(true) not reflected")
	}

	// The worker publishes gate level; GateLevel reads it back, clamped to [0,1].
	e.setGateLevel(0.7)
	if got := e.GateLevel(); got != 0.7 {
		t.Errorf("GateLevel after publish = %v, want 0.7", got)
	}
	e.setGateLevel(5)
	if got := e.GateLevel(); got != 1 {
		t.Errorf("GateLevel clamp = %v, want 1", got)
	}
}
