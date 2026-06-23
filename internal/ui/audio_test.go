package ui

import (
	"math"
	"sync/atomic"
	"testing"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/test"
	"fyne.io/fyne/v2/widget"
)

// fakeAudio is an in-memory AudioController mirroring config.AudioProcessing. It
// lets the UI tests drive the Audio panel wiring without importing internal/audio
// or any cgo, exactly as the real adapter is wired in main.
type fakeAudio struct {
	noise, agc, ducking, force bool
	mode                       string
	sens                       float32
	ptt                        string
	// gateLevel is read by the live-meter ticker goroutine and written by tests,
	// so it is atomic float-bits to stay -race clean.
	gateLevel atomic.Uint32
}

func newFakeAudio() *fakeAudio {
	return &fakeAudio{mode: "vad", sens: 0.15}
}

// setGate stores a [0,1] gate level for the live-meter ticker to read.
func (a *fakeAudio) setGate(v float32) { a.gateLevel.Store(math.Float32bits(v)) }

func (a *fakeAudio) NoiseSuppression() bool       { return a.noise }
func (a *fakeAudio) SetNoiseSuppression(on bool)  { a.noise = on }
func (a *fakeAudio) AGC() bool                    { return a.agc }
func (a *fakeAudio) SetAGC(on bool)               { a.agc = on }
func (a *fakeAudio) Ducking() bool                { return a.ducking }
func (a *fakeAudio) SetDucking(on bool)           { a.ducking = on }
func (a *fakeAudio) MicMode() string              { return a.mode }
func (a *fakeAudio) SetMicMode(mode string)       { a.mode = mode }
func (a *fakeAudio) GateSensitivity() float32     { return a.sens }
func (a *fakeAudio) SetGateSensitivity(t float32) { a.sens = t }
func (a *fakeAudio) ForceThrough() bool           { return a.force }
func (a *fakeAudio) SetForceThrough(on bool)      { a.force = on }
func (a *fakeAudio) PTTHotkey() string            { return a.ptt }
func (a *fakeAudio) SetPTTHotkey(combo string)    { a.ptt = combo }
func (a *fakeAudio) GateLevel() float32           { return math.Float32frombits(a.gateLevel.Load()) }

// compile-time check that the fake satisfies the interface.
var _ AudioController = (*fakeAudio)(nil)

// TestWithAudioStoresController confirms WithAudio attaches the controller and is
// chainable, and that building the window with an Audio controller attached does
// not panic (foundation: the panel widgets land later, so we only assert the
// wiring is in place and the build stays healthy).
func TestWithAudioStoresController(t *testing.T) {
	audio := newFakeAudio()
	a := New(testLibrary(t), &fakePlayer{}, newFakeVol(), &fakeSetup{}).WithAudio(audio)
	if a.audio == nil {
		t.Fatal("WithAudio did not store the controller")
	}
	// Building with an audio controller attached must remain safe.
	a.build(test.NewApp())
}

// TestNilAudioControllerSafe confirms the UI builds without an AudioController
// (the Audio panel is simply omitted), so existing call sites that do not wire
// audio keep working unchanged.
func TestNilAudioControllerSafe(t *testing.T) {
	a := New(testLibrary(t), &fakePlayer{}, newFakeVol(), &fakeSetup{})
	if a.audio != nil {
		t.Fatal("expected no audio controller by default")
	}
	a.build(test.NewApp())
	// buildAudioPanel must return nil so the layout omits the Audio tab.
	if a.buildAudioPanel() != nil {
		t.Fatal("buildAudioPanel should be nil with no AudioController")
	}
}

// newAudioApp builds an App with the given fake AudioController under a headless
// test app and returns the App plus the constructed Audio panel. The caller
// drives the panel's widgets to simulate user interaction. Building does not
// spawn the meter ticker goroutine (that starts only in Run for the real GUI);
// tests drive the meter via a.gateUpdate, so headless builds stay single-
// threaded and -race clean.
func newAudioApp(t *testing.T, audio AudioController) (*App, fyne.CanvasObject) {
	t.Helper()
	a := New(testLibrary(t), &fakePlayer{}, newFakeVol(), &fakeSetup{}).WithAudio(audio)
	a.build(test.NewApp())
	panel := a.buildAudioPanel()
	if panel == nil {
		t.Fatal("buildAudioPanel returned nil with an AudioController attached")
	}
	return a, panel
}

// TestAudioPanelSeedsFromController verifies every control is seeded from the
// controller's getters at build time (not a hard-coded default).
func TestAudioPanelSeedsFromController(t *testing.T) {
	audio := newFakeAudio()
	audio.noise, audio.agc, audio.ducking, audio.force = true, true, true, true
	audio.mode, audio.sens = "ptt", 0.42

	_, panel := newAudioApp(t, audio)

	if rg := findRadioGroup(panel); rg == nil {
		t.Fatal("Audio panel should contain a mic-mode radio group")
	} else if rg.Selected != modePTTLabel {
		t.Fatalf("mode radio seeded = %q, want %q", rg.Selected, modePTTLabel)
	}
	if s := findSlider(panel); s == nil {
		t.Fatal("Audio panel should contain a gate-sensitivity slider")
	} else if !near(s.Value, 0.42) {
		// float32->float64 widening means the seed is ~0.42, not exactly 0.42.
		t.Fatalf("sensitivity slider seeded = %v, want ~0.42", s.Value)
	}
	for _, tc := range []struct {
		label string
		want  bool
	}{
		{noiseLabel, true}, {agcLabel, true}, {duckingLabel, true}, {forceLabel, true},
	} {
		c := findCheck(panel, tc.label)
		if c == nil {
			t.Fatalf("Audio panel missing %q toggle", tc.label)
		}
		if c.Checked != tc.want {
			t.Fatalf("%q seeded checked=%v, want %v", tc.label, c.Checked, tc.want)
		}
	}
}

// TestAudioPanelTogglesApply verifies toggling each control drives the
// controller (and therefore persists, since the real adapter persists on set).
func TestAudioPanelTogglesApply(t *testing.T) {
	audio := newFakeAudio() // all false, mode "vad", sens 0.15
	_, panel := newAudioApp(t, audio)

	// Mode: select Push-to-talk -> controller mode "ptt".
	rg := findRadioGroup(panel)
	rg.SetSelected(modePTTLabel)
	if audio.mode != "ptt" {
		t.Fatalf("selecting %q set mode=%q, want ptt", modePTTLabel, audio.mode)
	}
	rg.SetSelected(modeMuteLabel)
	if audio.mode != "mute" {
		t.Fatalf("selecting %q set mode=%q, want mute", modeMuteLabel, audio.mode)
	}

	// Sensitivity slider drives SetGateSensitivity.
	s := findSlider(panel)
	s.SetValue(0.8)
	if !near(float64(audio.sens), 0.8) {
		t.Fatalf("sensitivity slider set sens=%v, want ~0.8", audio.sens)
	}

	// Each toggle flips the matching controller field.
	for _, tc := range []struct {
		label string
		get   func() bool
	}{
		{noiseLabel, func() bool { return audio.noise }},
		{agcLabel, func() bool { return audio.agc }},
		{duckingLabel, func() bool { return audio.ducking }},
		{forceLabel, func() bool { return audio.force }},
	} {
		c := findCheck(panel, tc.label)
		c.SetChecked(true)
		if !tc.get() {
			t.Fatalf("checking %q did not enable it on the controller", tc.label)
		}
		c.SetChecked(false)
		if tc.get() {
			t.Fatalf("unchecking %q did not disable it on the controller", tc.label)
		}
	}
}

// TestGateMeterTracksLevel verifies the live mic-open meter reads GateLevel()
// and pushes the value into the progress bar. It drives the App's gateUpdate
// closure directly — the same closure the production ticker calls each tick —
// so the test is deterministic and single-threaded (no goroutine, no sleeps).
func TestGateMeterTracksLevel(t *testing.T) {
	audio := newFakeAudio()
	audio.setGate(0.75)

	a, panel := newAudioApp(t, audio)
	bar := findProgressBar(panel)
	if bar == nil {
		t.Fatal("Audio panel should contain a gate-meter progress bar")
	}
	if a.gateUpdate == nil {
		t.Fatal("buildGateMeter did not wire the meter update closure")
	}

	a.gateUpdate()
	if !near(bar.Value, 0.75) {
		t.Fatalf("meter bar = %v after gate 0.75, want ~0.75", bar.Value)
	}

	// A new level propagates too — the meter is live, not a one-shot seed.
	audio.setGate(0.25)
	a.gateUpdate()
	if !near(bar.Value, 0.25) {
		t.Fatalf("meter bar = %v after gate 0.25, want ~0.25", bar.Value)
	}
}

// near reports whether two floats are within a small tolerance, absorbing the
// float32<->float64 widening the meter and sliders do.
func near(a, b float64) bool {
	d := a - b
	return d < 1e-5 && d > -1e-5
}

// TestStopGateTickerIdempotent verifies starting/stopping the live-meter ticker
// is safe to call repeatedly and with no meter wired (quit/rebuild paths rely on
// it). startGateTicker is a no-op without a gateUpdate closure.
func TestStopGateTickerIdempotent(t *testing.T) {
	a := New(testLibrary(t), &fakePlayer{}, newFakeVol(), &fakeSetup{})
	a.stopGateTicker()  // no ticker yet — safe no-op.
	a.startGateTicker() // no meter wired — safe no-op.
	if a.gateStop != nil {
		t.Fatal("startGateTicker should not start a ticker without a meter")
	}
	a.stopGateTicker()
	a.stopGateTicker() // second stop must not panic
}

// labelForModeRoundTrips guards the mode-label mapping the panel relies on.
func TestModeLabelRoundTrip(t *testing.T) {
	for _, m := range []string{"vad", "ptt", "always", "mute"} {
		if got := modeFromLabel(labelForMode(m)); got != m {
			t.Fatalf("mode %q round-trip = %q", m, got)
		}
	}
	// Unknown inputs coerce to vad, matching config.normalize().
	if labelForMode("bogus") != modeVADLabel {
		t.Fatal("unknown mode should map to the VAD label")
	}
	if modeFromLabel("bogus") != "vad" {
		t.Fatal("unknown label should map to vad")
	}
}

// findRadioGroup / findSlider / findCheck / findProgressBar walk an object tree
// and return the first matching widget, or nil.
func findRadioGroup(o fyne.CanvasObject) *widget.RadioGroup {
	var found *widget.RadioGroup
	walkObjects(o, func(obj fyne.CanvasObject) bool {
		if v, ok := obj.(*widget.RadioGroup); ok {
			found = v
			return true
		}
		return false
	})
	return found
}

func findSlider(o fyne.CanvasObject) *widget.Slider {
	var found *widget.Slider
	walkObjects(o, func(obj fyne.CanvasObject) bool {
		if v, ok := obj.(*widget.Slider); ok {
			found = v
			return true
		}
		return false
	})
	return found
}

func findCheck(o fyne.CanvasObject, label string) *widget.Check {
	var found *widget.Check
	walkObjects(o, func(obj fyne.CanvasObject) bool {
		if v, ok := obj.(*widget.Check); ok && v.Text == label {
			found = v
			return true
		}
		return false
	})
	return found
}

func findProgressBar(o fyne.CanvasObject) *widget.ProgressBar {
	var found *widget.ProgressBar
	walkObjects(o, func(obj fyne.CanvasObject) bool {
		if v, ok := obj.(*widget.ProgressBar); ok {
			found = v
			return true
		}
		return false
	})
	return found
}

// walkObjects depth-first walks the container tree, calling visit on each object
// and stopping at the first one for which visit returns true.
func walkObjects(o fyne.CanvasObject, visit func(fyne.CanvasObject) bool) {
	done := false
	var walk func(fyne.CanvasObject)
	walk = func(obj fyne.CanvasObject) {
		if done || obj == nil {
			return
		}
		if visit(obj) {
			done = true
			return
		}
		switch c := obj.(type) {
		case *fyne.Container:
			for _, child := range c.Objects {
				walk(child)
			}
		case *widget.Card:
			walk(c.Content)
		}
	}
	walk(o)
}
