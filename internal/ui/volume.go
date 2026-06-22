package ui

import (
	"fmt"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"soundboard/internal/catalog"
)

// buildVolumeArea builds the mic / soundboard-master / per-clip volume panel
// shown at the bottom of the window. The per-clip slider acts on whichever clip
// was last clicked (selectClip), so a user plays a sound then nudges its level.
func (a *App) buildVolumeArea() fyne.CanvasObject {
	mic := newGainSlider()
	master := newGainSlider()
	perClip := newGainSlider()
	perClip.Disable() // enabled once a clip is selected

	micPct := widget.NewLabel("")
	masterPct := widget.NewLabel("")
	clipPct := widget.NewLabel("")
	clipName := widget.NewLabel("No clip selected")
	clipName.TextStyle = fyne.TextStyle{Italic: true}

	// Seed slider positions from the controller (defaulting to unity).
	if a.vol != nil {
		mic.SetValue(clampGain(float64(a.vol.Mic())))
		master.SetValue(clampGain(float64(a.vol.Master())))
	} else {
		mic.SetValue(1)
		master.SetValue(1)
	}
	perClip.SetValue(1)
	micPct.SetText(pct(mic.Value))
	masterPct.SetText(pct(master.Value))
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

	grid := container.NewGridWithColumns(1,
		gainRow(theme.VolumeUpIcon(), "Mic", mic, micPct),
		gainRow(theme.MediaPlayIcon(), "Soundboard", master, masterPct),
		clipName,
		gainRow(theme.SettingsIcon(), "This clip", perClip, clipPct),
	)
	card := widget.NewCard("Volume", "Levels apply instantly and are saved on exit.", grid)
	return container.NewPadded(card)
}

// gainRow lays out an icon + fixed-width label + slider + percent readout.
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
