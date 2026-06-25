package ui

import (
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
)

// Mic-mode display labels and the captions for the Audio panel. They are
// exported as constants so the wording is asserted by a test (label drift then
// fails the build rather than silently confusing the user). The labels map
// one-to-one to the config MicMode strings via micModeFromLabel / labelForMode.
const (
	modeVADLabel    = "Voice-activated"
	modePTTLabel    = "Push-to-talk"
	modeAlwaysLabel = "Always-on"
	modeMuteLabel   = "Mute"

	noiseLabel   = "Noise suppression (RNNoise)"
	agcLabel     = "Automatic gain control"
	duckingLabel = "Duck soundboard while talking"
	forceLabel   = "Force sounds through Discord voice-activity gate"

	// audioCaption explains the headline promise of the suite.
	audioCaption = "Clean, gated, leveled voice — applied to your microphone only, " +
		"before it reaches Discord. Soundboard clips are never processed. " +
		"Leave Discord's own noise suppression OFF."

	// forceCaption explains that the former carrier toggle is now inert. The voiced
	// "carrier" tone it controlled was removed from the engine (it was a buzz by
	// construction); the toggle is retained only so existing layouts/settings stay
	// valid. Toggling it has no audio effect.
	forceCaption = "Inactive. The voiced-carrier tone this toggled has been removed; " +
		"this control no longer affects audio."

	// gateMeterCaption labels the live indicator.
	gateMeterCaption = "Mic open"
)

// gateTickInterval is how often the live mic-open meter polls GateLevel().
// ~20 Hz is smooth to the eye and trivially cheap (one atomic load per tick).
const gateTickInterval = 50 * time.Millisecond

// micModes lists the selector options in display order, paired with their
// config MicMode strings.
var micModes = []struct {
	label string
	mode  string
}{
	{modeVADLabel, "vad"},
	{modePTTLabel, "ptt"},
	{modeAlwaysLabel, "always"},
	{modeMuteLabel, "mute"},
}

// labelForMode maps a config MicMode string to its display label, defaulting to
// the VAD label for any unknown/empty value (matching config.normalize()).
func labelForMode(mode string) string {
	for _, m := range micModes {
		if m.mode == mode {
			return m.label
		}
	}
	return modeVADLabel
}

// modeFromLabel maps a display label back to its config MicMode string,
// defaulting to "vad" for an unknown label.
func modeFromLabel(label string) string {
	for _, m := range micModes {
		if m.label == label {
			return m.mode
		}
	}
	return "vad"
}

// modeLabels returns just the display labels for the selector, in order.
func modeLabels() []string {
	out := make([]string, len(micModes))
	for i, m := range micModes {
		out[i] = m.label
	}
	return out
}

// buildAudioPanel builds the "Audio" settings card mirroring Discord's voice
// controls, wired to the AudioController. Every control applies instantly via
// the controller (which also persists it) and is seeded from the controller's
// getters. Returns nil when no AudioController is attached, so the caller can
// omit the panel entirely.
//
// The mic-open meter is a thin colored bar driven by a ticker that polls
// GateLevel(); the ticker is owned by the App and stopped on quit/close so it
// never outlives the window. The panel never imports internal/audio or any cgo.
func (a *App) buildAudioPanel() fyne.CanvasObject {
	if a.audio == nil {
		return nil
	}
	c := a.audio

	// Mic mode selector — a radio group reads cleanest for four exclusive modes
	// and seeds directly from the current MicMode.
	mode := widget.NewRadioGroup(modeLabels(), func(label string) {
		c.SetMicMode(modeFromLabel(label))
	})
	mode.SetSelected(labelForMode(c.MicMode()))

	// Gate sensitivity slider [0,1], shown as a percent, with the live mic-open
	// meter beside it so the user can tune the threshold against real speech.
	sens := widget.NewSlider(0, 1)
	sens.Step = 0.01
	sens.SetValue(clamp01(float64(c.GateSensitivity())))
	sensPct := widget.NewLabel(pct(sens.Value))
	sens.OnChanged = func(v float64) {
		sensPct.SetText(pct(v))
		c.SetGateSensitivity(float32(v))
	}

	meter := a.buildGateMeter()

	// Toggles. Each pushes to the controller immediately.
	noise := widget.NewCheck(noiseLabel, c.SetNoiseSuppression)
	noise.SetChecked(c.NoiseSuppression())
	agc := widget.NewCheck(agcLabel, c.SetAGC)
	agc.SetChecked(c.AGC())
	ducking := widget.NewCheck(duckingLabel, c.SetDucking)
	ducking.SetChecked(c.Ducking())
	force := widget.NewCheck(forceLabel, c.SetForceThrough)
	force.SetChecked(c.ForceThrough())

	caption := wrappedCaption(audioCaption)
	forceCap := wrappedCaption(forceCaption)

	body := container.NewVBox(
		caption,
		widget.NewLabelWithStyle("Input mode", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		mode,
		widget.NewSeparator(),
		widget.NewLabelWithStyle("Gate sensitivity", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		container.NewBorder(nil, nil, nil, sensPct, sens),
		container.NewBorder(nil, nil, widget.NewLabel(gateMeterCaption), nil, meter),
		widget.NewSeparator(),
		noise,
		agc,
		ducking,
		widget.NewSeparator(),
		force,
		forceCap,
	)
	card := widget.NewCard("Audio", "Mic processing — applies instantly and is saved on exit.", body)
	return container.NewPadded(card)
}

// buildGateMeter builds the live mic-open indicator: a colored "light" that
// glows green as the gate opens (toward 1.0) and dims toward the primary colour
// as it closes, beside a horizontal bar showing the same level. It stores the
// refresh closure on the App as gateUpdate; the polling ticker (started in Run
// for the real GUI) calls it via fyne.Do, and tests call it directly. Building
// the widgets does NOT start a goroutine, so headless test builds stay
// single-threaded and race-clean.
func (a *App) buildGateMeter() fyne.CanvasObject {
	const lightSize = float32(16)

	light := canvas.NewCircle(theme.Color(theme.ColorNamePrimary))
	lightCell := container.NewGridWrap(fyne.NewSize(lightSize, lightSize), light)

	bar := widget.NewProgressBar()
	bar.Min, bar.Max = 0, 1
	bar.TextFormatter = func() string { return "" } // a bar, not a percent readout

	a.gateUpdate = func() {
		if a.audio == nil {
			return
		}
		level := clamp01f(a.audio.GateLevel())
		bar.SetValue(float64(level))
		if level > 0.5 {
			light.FillColor = theme.Color(theme.ColorNameSuccess)
		} else {
			light.FillColor = theme.Color(theme.ColorNamePrimary)
		}
		canvas.Refresh(light)
	}

	return container.NewBorder(nil, nil, lightCell, nil, bar)
}

// startGateTicker launches the polling goroutine that drives the live mic-open
// meter: every gateTickInterval it calls a.gateUpdate on the Fyne thread (via
// fyne.Do) until the App's gateStop channel is closed (quit/close/rebuild). It
// is a no-op when no meter (gateUpdate) is wired, and stops any previous ticker
// first so a window rebuild does not leak goroutines. Called from Run for the
// real GUI; headless tests drive gateUpdate directly instead, so they never
// spawn this goroutine (keeping -race clean under the inline test driver).
func (a *App) startGateTicker() {
	a.stopGateTicker()
	if a.gateUpdate == nil {
		return
	}
	update := a.gateUpdate
	stop := make(chan struct{})
	a.gateStop = stop
	go func() {
		t := time.NewTicker(gateTickInterval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				select {
				case <-stop:
					return
				default:
				}
				fyne.Do(update)
			}
		}
	}()
}

// stopGateTicker stops the live mic-open meter's polling goroutine if running.
// Idempotent and safe with no ticker active. Called on rebuild and on quit.
func (a *App) stopGateTicker() {
	if a.gateStop != nil {
		close(a.gateStop)
		a.gateStop = nil
	}
}

// wrappedCaption builds an italic, word-wrapped caption label.
func wrappedCaption(text string) *widget.Label {
	l := widget.NewLabelWithStyle(text, fyne.TextAlignLeading, fyne.TextStyle{Italic: true})
	l.Wrapping = fyne.TextWrapWord
	return l
}

// clamp01 keeps a seeded slider value within [0,1].
func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// clamp01f clamps a float32 into [0,1] for the meter fill.
func clamp01f(v float32) float32 {
	if v < 0 || v != v { // also coerces NaN to 0
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}
