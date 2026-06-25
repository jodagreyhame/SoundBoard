package apm

import (
	"math"
	"math/rand"
	"testing"
)

// TestDiscordConfigValues pins the Discord-exact configuration so a stray edit to
// DiscordConfig is caught: HPF on, NS on at Moderate, AGC on, AEC off, mono in/out.
func TestDiscordConfigValues(t *testing.T) {
	c := DiscordConfig()
	if !c.HighPassFilterEnabled {
		t.Error("HighPassFilter must be enabled (Discord)")
	}
	if !c.NoiseSuppressionEnabled || c.NoiseSuppressionLevel != NSLevelModerate {
		t.Errorf("NoiseSuppression must be on at Moderate, got on=%v level=%d", c.NoiseSuppressionEnabled, c.NoiseSuppressionLevel)
	}
	if !c.GainControlEnabled {
		t.Error("GainControl must be enabled (Discord)")
	}
	if c.EchoCancellationEnabled {
		t.Error("EchoCancellation must be OFF (server-side, no render reference)")
	}
	if c.CaptureChannels != 1 || c.RenderChannels != 1 {
		t.Errorf("capture/render channels must be mono, got %d/%d", c.CaptureChannels, c.RenderChannels)
	}
}

// TestProcessorRoundTrip builds a real (or no-op) Processor at the Discord config
// and processes a 480-sample mono frame. On a build where the APM is available
// (Windows + DLL), it asserts processing succeeds (rc==0) and leaves a finite,
// in-range signal; otherwise it asserts the no-op leaves the frame untouched. This
// is the production analogue of the apmspike smoke test.
func TestProcessorRoundTrip(t *testing.T) {
	p, err := New(DiscordConfig())
	if err != nil && Available() {
		t.Fatalf("APM is available but New failed: %v", err)
	}
	defer p.Close()

	// A 480-sample triangle, exactly like the spike's input.
	frame := make([]float32, FrameSize)
	var inEnergy float64
	for i := range frame {
		v := float32(0.2) * float32((i%16)-8) / 8.0
		frame[i] = v
		inEnergy += float64(v) * float64(v)
	}
	orig := make([]float32, FrameSize)
	copy(orig, frame)

	rc := p.ProcessCapture(frame)

	if !Available() {
		// No-op path: the frame must be untouched and rc must be 0.
		if rc != 0 {
			t.Fatalf("no-op ProcessCapture rc=%d, want 0", rc)
		}
		for i := range frame {
			if frame[i] != orig[i] {
				t.Fatalf("no-op processor must not modify frame at %d: got %v want %v", i, frame[i], orig[i])
			}
		}
		t.Skip("APM unavailable in this build; verified no-op passthrough")
	}

	// Real APM path.
	if rc != 0 {
		t.Fatalf("ProcessCapture rc=%d, want 0", rc)
	}
	var outEnergy float64
	for i, v := range frame {
		if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
			t.Fatalf("APM produced non-finite sample at %d: %v", i, v)
		}
		if v < -1.01 || v > 1.01 {
			t.Fatalf("APM produced out-of-range sample at %d: %v", i, v)
		}
		outEnergy += float64(v) * float64(v)
	}
	// The APM is not a mute: a voiced-energy input yields some output energy.
	if outEnergy <= 0 {
		t.Fatalf("APM zeroed a non-silent frame (inEnergy=%.4f)", inEnergy)
	}
	t.Logf("APM round-trip ok: inEnergy=%.4f outEnergy=%.4f", inEnergy, outEnergy)
}

// TestGainControlBoostsQuietInput is the behavioral guard for the AGC toggle. The
// production "AGC" maps to GainControlEnabled; with the working GainController1
// adaptive-digital path (the fix), driving a QUIET voiced signal with gain control
// ON must raise the output RMS well above the same signal processed with gain
// control OFF. The previous GC2-only wiring was a DEAD toggle (on == off, ~1.00x),
// so this test would have failed against it — it can never silently regress again.
// Skipped when the APM is unavailable (non-Windows / DLL missing).
func TestGainControlBoostsQuietInput(t *testing.T) {
	if !Available() {
		t.Skip("APM unavailable in this build")
	}
	// Run a quiet (~-26 dBFS) voiced sine through the APM with NS off (isolate gain)
	// and report the steady-state output RMS after the adaptive gain has settled.
	run := func(agcOn bool) float32 {
		c := DiscordConfig()
		c.NoiseSuppressionEnabled = false // isolate gain control from NS
		c.GainControlEnabled = agcOn
		p, err := New(c)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		defer p.Close()
		const amp = 0.05 // ~-26 dBFS, a quiet talker the AGC should lift
		const freq = 220.0
		phase := 0.0
		const dphase = 2 * math.Pi * freq / float64(SampleRate)
		var last float32
		for i := 0; i < 400; i++ { // ~4s so the adaptive-digital gain fully ramps
			f := make([]float32, FrameSize)
			for j := range f {
				f[j] = float32(amp * math.Sin(phase))
				phase += dphase
			}
			p.ProcessCapture(f)
			var s float64
			for _, v := range f {
				s += float64(v) * float64(v)
			}
			last = float32(math.Sqrt(s / float64(len(f))))
		}
		return last
	}
	off := run(false)
	on := run(true)
	// The adaptive-digital gain must lift the quiet input meaningfully (require at
	// least 1.5x; the spike measures ~3.2x). A dead toggle (on ~= off) fails here.
	if on <= off*1.5 {
		t.Fatalf("AGC on should boost a quiet input >=1.5x over AGC off: on=%v off=%v (ratio %.2fx)", on, off, on/off)
	}
	t.Logf("AGC quiet-input boost ok: off=%v on=%v (%.2fx)", off, on, on/off)
}

// TestNoiseSuppressionAttenuatesNoise confirms the APM's noise suppression actually
// reduces broadband noise when enabled: feeding white noise with NS on yields a far
// lower output RMS than with NS off. This is the behavioral proof that the Discord NS
// submodule is wired and the UI's "Noise suppression" toggle has real effect. Gain
// control is isolated OFF here so the NS effect is measured alone; the AGC boost is
// proven separately by TestGainControlBoostsQuietInput. Skipped when the APM is
// unavailable.
func TestNoiseSuppressionAttenuatesNoise(t *testing.T) {
	if !Available() {
		t.Skip("APM unavailable in this build")
	}
	run := func(nsOn bool) float32 {
		c := DiscordConfig()
		c.NoiseSuppressionEnabled = nsOn
		c.GainControlEnabled = false // isolate NS from any gain effect
		p, err := New(c)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		defer p.Close()
		rng := rand.New(rand.NewSource(1))
		var last float32
		for i := 0; i < 200; i++ { // ~2s so NS fully adapts
			f := make([]float32, FrameSize)
			for j := range f {
				f[j] = float32((rng.Float64()*2 - 1) * 0.1) // white noise
			}
			p.ProcessCapture(f)
			var s float64
			for _, v := range f {
				s += float64(v) * float64(v)
			}
			last = float32(math.Sqrt(s / float64(len(f))))
		}
		return last
	}
	off := run(false)
	on := run(true)
	if on >= off {
		t.Fatalf("NS on should attenuate noise below NS off: on=%v off=%v", on, off)
	}
}

// TestReconfigureFlipsNS confirms Reconfigure changes the live processing: a
// Processor built with NS off, then reconfigured NS on, suppresses noise after the
// flip. This is the runtime-toggle path the worker uses. Skipped when unavailable.
func TestReconfigureFlipsNS(t *testing.T) {
	if !Available() {
		t.Skip("APM unavailable in this build")
	}
	c := DiscordConfig()
	c.NoiseSuppressionEnabled = false
	c.GainControlEnabled = false
	p, err := New(c)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer p.Close()

	noiseRMS := func() float32 {
		rng := rand.New(rand.NewSource(7))
		var last float32
		for i := 0; i < 200; i++ {
			f := make([]float32, FrameSize)
			for j := range f {
				f[j] = float32((rng.Float64()*2 - 1) * 0.1)
			}
			p.ProcessCapture(f)
			var s float64
			for _, v := range f {
				s += float64(v) * float64(v)
			}
			last = float32(math.Sqrt(s / float64(len(f))))
		}
		return last
	}

	withNSoff := noiseRMS()
	c.NoiseSuppressionEnabled = true
	if rc := p.Reconfigure(c); rc != 0 {
		t.Fatalf("Reconfigure rc=%d", rc)
	}
	withNSon := noiseRMS()
	if withNSon >= withNSoff {
		t.Fatalf("Reconfigure NS on should suppress noise: on=%v off=%v", withNSon, withNSoff)
	}
}

// TestProcessorWrongFrameLength confirms a non-FrameSize frame is rejected as a
// bad-length no-op rather than crashing the native side.
func TestProcessorWrongFrameLength(t *testing.T) {
	p, _ := New(DiscordConfig())
	defer p.Close()

	short := make([]float32, FrameSize-1)
	rc := p.ProcessCapture(short)
	if Available() && rc != -8 {
		t.Fatalf("wrong-length frame rc=%d, want -8 (bad data length)", rc)
	}
}

// TestProcessorCloseIdempotent confirms Close can be called twice without panic and
// that ProcessCapture after Close is a no-op.
func TestProcessorCloseIdempotent(t *testing.T) {
	p, _ := New(DiscordConfig())
	p.Close()
	p.Close() // must not panic
	frame := make([]float32, FrameSize)
	if rc := p.ProcessCapture(frame); rc != 0 {
		t.Fatalf("ProcessCapture after Close rc=%d, want 0", rc)
	}
}
