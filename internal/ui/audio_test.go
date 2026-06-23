package ui

import (
	"testing"

	"fyne.io/fyne/v2/test"
)

// fakeAudio is an in-memory AudioController mirroring config.AudioProcessing. It
// lets the UI tests drive the Audio panel wiring without importing internal/audio
// or any cgo, exactly as the real adapter is wired in main.
type fakeAudio struct {
	noise, agc, ducking, force bool
	mode                       string
	sens                       float32
	ptt                        string
	gateLevel                  float32
}

func newFakeAudio() *fakeAudio {
	return &fakeAudio{mode: "vad", sens: 0.15}
}

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
func (a *fakeAudio) GateLevel() float32           { return a.gateLevel }

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
}
