package ui

import (
	"testing"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/test"
	"fyne.io/fyne/v2/widget"
)

// TestVolumeLabelsAreSelfExplanatory pins the exact who-hears-what wording of the
// redesigned panel so a future edit cannot silently make the labels ambiguous
// again. These strings are the whole point of the volume-clarity redesign.
func TestVolumeLabelsAreSelfExplanatory(t *testing.T) {
	cases := []struct {
		got, want string
	}{
		{micLabel, "Microphone — your voice"},
		{masterLabel, "Soundboard — what others hear in Discord"},
		{monitorLabel, "Soundboard — what you hear"},
		{clipLabel, "This clip"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("label = %q, want %q", c.got, c.want)
		}
	}
	if volumeCaption == "" {
		t.Error("volume caption must explain the local-vs-Discord split, got empty")
	}
}

// collectSliders walks a Fyne object tree and returns every *widget.Slider, in
// build order. The volume panel lays out mic, master, monitor, then per-clip, so
// the returned order matches that sequence.
func collectSliders(o fyne.CanvasObject) []*widget.Slider {
	var out []*widget.Slider
	var walk func(fyne.CanvasObject)
	walk = func(obj fyne.CanvasObject) {
		switch v := obj.(type) {
		case *widget.Slider:
			out = append(out, v)
		case *fyne.Container:
			for _, c := range v.Objects {
				walk(c)
			}
		case *widget.Card:
			if v.Content != nil {
				walk(v.Content)
			}
		}
	}
	walk(o)
	return out
}

// TestVolumePanelSeedsAndAppliesAllThreeLevels verifies the redesigned panel
// seeds each of the three independent sliders from the controller getters and
// pushes changes back through the matching setter — in particular the NEW
// monitor ("what you hear") level, which must be wired to SetMonitor.
func TestVolumePanelSeedsAndAppliesAllThreeLevels(t *testing.T) {
	a := New(testLibrary(t), &fakePlayer{}, newFakeVol(), &fakeSetup{})
	a.build(test.NewApp())
	vol := a.vol.(*fakeVol)

	// Distinct seed values so a mis-wired slider is obvious.
	vol.mic, vol.master, vol.monitor = 0.4, 0.6, 0.8

	panel := a.buildVolumeArea()
	sliders := collectSliders(panel)
	if len(sliders) < 4 {
		t.Fatalf("expected at least 4 sliders (mic, master, monitor, per-clip), got %d", len(sliders))
	}
	micS, masterS, monitorS := sliders[0], sliders[1], sliders[2]

	if !approxF(micS.Value, 0.4) {
		t.Errorf("mic slider seeded at %v, want 0.4", micS.Value)
	}
	if !approxF(masterS.Value, 0.6) {
		t.Errorf("master slider seeded at %v, want 0.6", masterS.Value)
	}
	if !approxF(monitorS.Value, 0.8) {
		t.Errorf("monitor slider seeded at %v, want 0.8 (the new 'what you hear' level)", monitorS.Value)
	}

	// Dragging each slider must apply instantly via the matching setter and NOT
	// cross-talk into another level.
	micS.SetValue(0.1)
	masterS.SetValue(0.2)
	monitorS.SetValue(0.3)

	if !approxF32(vol.mic, 0.1) {
		t.Errorf("SetMic not applied: mic=%v want 0.1", vol.mic)
	}
	if !approxF32(vol.master, 0.2) {
		t.Errorf("SetMaster not applied: master=%v want 0.2", vol.master)
	}
	if !approxF32(vol.monitor, 0.3) {
		t.Errorf("SetMonitor not applied: monitor=%v want 0.3 (the 'what you hear' level)", vol.monitor)
	}
}

func approxF(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d <= 1e-4
}

func approxF32(a, b float32) bool { return approxF(float64(a), float64(b)) }
