package ui

import (
	"fmt"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"soundboard/internal/catalog"
)

// Slider labels make it unmistakable WHO hears each level. They are exported as
// constants so the redesign's wording is asserted by a test (a label drift then
// fails the build rather than silently confusing the user).
const (
	micLabel     = "Microphone — your voice"
	masterLabel  = "Soundboard — what others hear in Discord"
	monitorLabel = "Soundboard — what you hear"
	clipLabel    = "This clip"

	// volumeCaption spells out the local/Discord split so the panel is
	// self-explanatory at a glance.
	volumeCaption = "Your mic and \"what you hear\" stay local to you. " +
		"\"What others hear\" is the only level Discord transmits."
)

// buildVolumeArea builds the self-explanatory volume panel shown at the bottom
// of the window. It groups three INDEPENDENT levels — the mic (your voice to
// Discord), the soundboard level OTHERS hear in Discord, and the soundboard
// level YOU hear locally — plus a per-clip control. Each is labelled with who
// hears it, and a caption clarifies the local-vs-Discord split. The per-clip
// slider acts on whichever clip was last clicked (selectClip), so a user plays a
// sound then nudges its level.
func (a *App) buildVolumeArea() fyne.CanvasObject {
	mic := newGainSlider()
	master := newGainSlider()
	monitor := newGainSlider()
	perClip := newGainSlider()
	perClip.Disable() // enabled once a clip is selected

	micPct := widget.NewLabel("")
	masterPct := widget.NewLabel("")
	monitorPct := widget.NewLabel("")
	clipPct := widget.NewLabel("")
	clipName := widget.NewLabel("No clip selected")
	clipName.TextStyle = fyne.TextStyle{Italic: true}

	// Seed slider positions from the controller getters (defaulting to unity).
	if a.vol != nil {
		mic.SetValue(clampGain(float64(a.vol.Mic())))
		master.SetValue(clampGain(float64(a.vol.Master())))
		monitor.SetValue(clampGain(float64(a.vol.Monitor())))
	} else {
		mic.SetValue(1)
		master.SetValue(1)
		monitor.SetValue(1)
	}
	perClip.SetValue(1)
	micPct.SetText(pct(mic.Value))
	masterPct.SetText(pct(master.Value))
	monitorPct.SetText(pct(monitor.Value))
	clipPct.SetText(pct(perClip.Value))

	mic.OnChanged = func(v float64) {
		micPct.SetText(pct(v))
		if a.vol != nil {
			a.vol.SetMic(float32(v))
		}
	}
	master.OnChanged = func(v float64) {
		masterPct.SetText(pct(v))
		if a.vol != nil {
			a.vol.SetMaster(float32(v))
		}
	}
	monitor.OnChanged = func(v float64) {
		monitorPct.SetText(pct(v))
		if a.vol != nil {
			a.vol.SetMonitor(float32(v))
		}
	}
	perClip.OnChanged = func(v float64) {
		clipPct.SetText(pct(v))
		if a.vol != nil && a.selected != "" {
			a.vol.SetClip(a.selected, float32(v))
		}
	}

	// selectClip binds a clip to the per-clip slider: enable it, show the name,
	// and move it to the clip's saved gain without firing OnChanged's setter for
	// the previously-selected clip.
	a.selectClip = func(clip *catalog.Clip) {
		a.selected = clip.ID
		clipName.SetText("Selected: " + clip.Name)
		g := 1.0
		if a.vol != nil {
			g = clampGain(float64(a.vol.Clip(clip.ID)))
			if g == 0 {
				g = 1
			}
		}
		perClip.SetValue(g)
		clipPct.SetText(pct(g))
		perClip.Enable()
	}

	caption := widget.NewLabelWithStyle(volumeCaption, fyne.TextAlignLeading, fyne.TextStyle{Italic: true})
	caption.Wrapping = fyne.TextWrapWord

	grid := container.NewVBox(
		caption,
		gainRow(theme.VolumeUpIcon(), micLabel, mic, micPct),
		gainRow(theme.MediaPlayIcon(), masterLabel, master, masterPct),
		gainRow(theme.VolumeDownIcon(), monitorLabel, monitor, monitorPct),
		widget.NewSeparator(),
		clipName,
		gainRow(theme.SettingsIcon(), clipLabel, perClip, clipPct),
	)
	card := widget.NewCard("Volume", "Levels apply instantly and are saved on exit.", grid)
	return container.NewPadded(card)
}

// gainRow lays out an icon + label + slider + percent readout. The label takes a
// fixed leading width so the who-hears-what text is always fully visible and the
// sliders line up.
func gainRow(icon fyne.Resource, label string, slider *widget.Slider, readout *widget.Label) fyne.CanvasObject {
	name := widget.NewLabelWithStyle(label, fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
	left := container.NewHBox(widget.NewIcon(icon), name)
	return container.NewBorder(nil, nil, left, readout, slider)
}

// newGainSlider returns a 0..150% slider stepped at 1%.
func newGainSlider() *widget.Slider {
	s := widget.NewSlider(gainMin, gainMax)
	s.Step = gainStep
	return s
}

// pct formats a linear gain (1.0) as a percent string ("100%").
func pct(v float64) string {
	return fmt.Sprintf("%3.0f%%", v*100)
}

// clampGain keeps a seeded value within the slider's [min,max] so an
// out-of-range saved value never throws the thumb off the track.
func clampGain(v float64) float64 {
	if v < gainMin {
		return gainMin
	}
	if v > gainMax {
		return gainMax
	}
	return v
}
