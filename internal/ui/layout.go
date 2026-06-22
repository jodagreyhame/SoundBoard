package ui

import (
	"fmt"
	"strings"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"soundboard/internal/catalog"
)

// Layout knobs. Buttons are sized for ~150 clips per category to stay readable
// while keeping the grid responsive (GridWrap reflows on resize).
var (
	clipButtonSize = fyne.NewSize(168, 40)
)

// gain bounds for the sliders: 0..150% expressed as a linear amplitude.
const (
	gainMin  = 0.0
	gainMax  = 1.5
	gainStep = 0.01
)

// buildContent assembles the full window layout: setup banner on top, volume
// area on the bottom, and the search box + category clip sections filling the
// center.
func (a *App) buildContent() fyne.CanvasObject {
	return container.NewBorder(
		a.buildSetupBanner(), // top
		a.buildVolumeArea(),  // bottom
		nil, nil,
		a.buildClipBrowser(), // center
	)
}

// buildSetupBanner renders the VB-CABLE status line plus an install/fix action.
// When routing is ready the line is success-green and the action button is
// hidden; otherwise it is a warning row with a prominent "Install / Fix audio
// routing" button.
func (a *App) buildSetupBanner() fyne.CanvasObject {
	ready, detail := false, "VB-CABLE status unknown"
	if a.setup != nil {
		ready, detail = a.setup.Status()
	}

	col := theme.Color(theme.ColorNameWarning)
	icon := theme.NewWarningThemedResource(theme.WarningIcon())
	if ready {
		col = theme.Color(theme.ColorNameSuccess)
		icon = theme.NewSuccessThemedResource(theme.ConfirmIcon())
		detail = "Audio routing active — " + detail
	}

	text := canvas.NewText(detail, col)
	text.TextStyle = fyne.TextStyle{Bold: true}
	left := container.NewHBox(widget.NewIcon(icon), text)

	// When routing is not yet ready, offer the appropriate next action: if the
	// cable is present we can Engage routing directly; otherwise we must Install
	// VB-CABLE first. The label reflects which one onFixRouting will run.
	var right fyne.CanvasObject
	if !ready {
		label := "Install / Fix audio routing"
		if a.setup != nil && a.setup.CanEngage() {
			label = "Engage routing"
		}
		btn := widget.NewButtonWithIcon(label, theme.DownloadIcon(), a.onFixRouting)
		btn.Importance = widget.WarningImportance
		right = btn
	}

	row := container.NewBorder(nil, nil, left, right, nil)
	return container.NewVBox(container.NewPadded(row), widget.NewSeparator())
}

// onFixRouting runs the next setup action in a goroutine with an infinite-
// progress dialog, then reports the result. If the cable is already present it
// ENGAGES routing (points Discord's default mic at CABLE Output); otherwise it
// INSTALLS VB-CABLE. The choice keys on CanEngage, NOT on Status's ready flag —
// ready means "engaged", which is false in both the install-needed and
// engage-needed states, so keying on ready would make the Engage path
// unreachable.
func (a *App) onFixRouting() {
	if a.setup == nil || a.win == nil {
		return
	}
	engaging := a.setup.CanEngage()

	title := "Installing VB-CABLE"
	msg := "Downloading and installing the virtual audio cable. Approve the Windows elevation prompt if it appears…"
	op := a.setup.Install
	if engaging {
		title = "Engaging routing"
		msg = "Pointing Discord's microphone at CABLE Output…"
		op = a.setup.Engage
	}

	prog := dialog.NewProgressInfinite(title, msg, a.win)
	prog.Show()
	go func() {
		err := op()
		fyne.Do(func() {
			prog.Hide()
			if err != nil {
				dialog.ShowError(fmt.Errorf("%s failed: %w", strings.ToLower(title), err), a.win)
				return
			}
			if engaging {
				// Routing is now live; the banner flips to the success state.
				dialog.ShowInformation("Routing engaged",
					"Discord now hears the soundboard automatically — no changes needed inside Discord. "+
						"Your real microphone is restored when you quit SoundBoard.", a.win)
			} else {
				// VB-CABLE almost always needs a full Windows reboot before its
				// endpoints enumerate; an app-only restart is not enough.
				dialog.ShowInformation("VB-CABLE installed — reboot required",
					"VB-CABLE was installed. Reboot Windows, then relaunch SoundBoard. "+
						"The virtual cable will NOT appear until you restart your PC.", a.win)
			}
			a.refreshBanner()
		})
	}()
}

// refreshBanner rebuilds the whole window content so the banner reflects the
// latest SetupController.Status (e.g. after a successful install). Cheap enough
// for a one-off user action.
func (a *App) refreshBanner() {
	if a.win != nil {
		a.win.SetContent(a.buildContent())
	}
}

// buildClipBrowser builds the search box and the scrollable, category-grouped
// sections of clip buttons. Live filtering re-renders the sections on every
// keystroke; it stays usable for 200+ clips because the sections live inside a
// single vertical scroll and empty categories are dropped.
func (a *App) buildClipBrowser() fyne.CanvasObject {
	search := widget.NewEntry()
	search.SetPlaceHolder("Search clips by name or category…")
	search.SetText(a.search)

	sections := container.NewVBox()
	empty := widget.NewLabel("No clips match your search.")
	empty.Alignment = fyne.TextAlignCenter
	empty.Hide()

	a.rebuildBrowser = func() {
		sections.RemoveAll()
		shown := a.renderSections(sections)
		if shown == 0 {
			empty.Show()
		} else {
			empty.Hide()
		}
		sections.Refresh()
	}

	search.OnChanged = func(s string) {
		a.search = s
		a.rebuildBrowser()
	}
	a.rebuildBrowser()

	body := container.NewVScroll(container.NewVBox(sections, empty))
	header := container.NewBorder(nil, nil, widget.NewIcon(theme.SearchIcon()), nil, search)
	return container.NewBorder(container.NewPadded(header), nil, nil, nil, body)
}

// renderSections appends one labelled category section per non-empty,
// filter-matching category and returns how many clip buttons were shown total.
func (a *App) renderSections(into *fyne.Container) int {
	if a.lib == nil {
		return 0
	}
	total := 0
	for i := range a.lib.Categories {
		cat := &a.lib.Categories[i]
		buttons := a.categoryButtons(cat)
		if len(buttons) == 0 {
			continue
		}
		total += len(buttons)

		title := widget.NewLabelWithStyle(
			fmt.Sprintf("%s  (%d)", prettyCategory(cat.Name), len(buttons)),
			fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
		grid := container.NewGridWrap(clipButtonSize, buttons...)
		into.Add(container.NewVBox(title, grid, widget.NewSeparator()))
	}
	return total
}

// categoryButtons builds the play buttons for the clips in cat that pass the
// current filter. Each button taps to play at the clip's saved per-clip gain and
// selects the clip into the per-clip volume slider.
func (a *App) categoryButtons(cat *catalog.Category) []fyne.CanvasObject {
	var buttons []fyne.CanvasObject
	for _, clip := range cat.Clips {
		if !match(clip, a.search) {
			continue
		}
		c := clip // capture per-iteration
		btn := widget.NewButtonWithIcon(c.Name, theme.MediaPlayIcon(), func() {
			a.play(c)
		})
		btn.Alignment = widget.ButtonAlignLeading
		buttons = append(buttons, btn)
	}
	return buttons
}

// play triggers a clip at its saved per-clip gain and selects it for the
// per-clip volume control.
func (a *App) play(clip *catalog.Clip) {
	gain := float32(1)
	if a.vol != nil {
		gain = a.vol.Clip(clip.ID)
		if gain == 0 {
			gain = 1
		}
	}
	if a.player != nil {
		a.player.TriggerGain(clip.ID, gain)
	}
	if a.selectClip != nil {
		a.selectClip(clip)
	}
}

// match reports whether a clip passes the current filter (case-insensitive
// substring on name or category; empty filter matches everything).
func match(clip *catalog.Clip, filter string) bool {
	if filter == "" {
		return true
	}
	f := strings.ToLower(strings.TrimSpace(filter))
	if f == "" {
		return true
	}
	return strings.Contains(strings.ToLower(clip.Name), f) ||
		strings.Contains(strings.ToLower(clip.Category), f)
}

// prettyCategory turns a category directory name into a display label.
func prettyCategory(name string) string {
	r := strings.NewReplacer("_", " ", "-", " ")
	s := strings.TrimSpace(r.Replace(name))
	if s == "" {
		return name
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
