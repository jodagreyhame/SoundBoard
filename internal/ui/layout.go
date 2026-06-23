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

// buildContent assembles the full window layout: the setup banner always sits
// on top. The body is the Soundboard view (clip browser + volume panel). When an
// AudioController is wired, the body becomes a two-tab view — "Soundboard" and
// "Audio" — so the mic-processing controls get a clean home without cluttering
// the clip browser. Without an AudioController the layout is unchanged.
func (a *App) buildContent() fyne.CanvasObject {
	// A content rebuild (e.g. refreshBanner) replaces every widget, so stop any
	// live mic-open meter ticker from the previous build before wiring new ones.
	a.stopGateTicker()

	soundboard := container.NewBorder(
		nil,                 // top (banner is added once, above the tabs)
		a.buildVolumeArea(), // bottom
		nil, nil,
		a.buildClipBrowser(), // center
	)

	var body fyne.CanvasObject = soundboard
	if audio := a.buildAudioPanel(); audio != nil {
		tabs := container.NewAppTabs(
			container.NewTabItemWithIcon("Soundboard", theme.MediaMusicIcon(), soundboard),
			container.NewTabItemWithIcon("Audio", theme.SettingsIcon(), audio),
		)
		tabs.SetTabLocation(container.TabLocationTop)
		body = tabs
	}

	return container.NewBorder(
		a.buildSetupBanner(), // top
		nil, nil, nil,
		body, // center
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
				a.refreshBanner()
				return
			}
			// The installer also rescanned devices and bounced the Audio Endpoint
			// Builder, so on most machines the cable is already present in this
			// Windows session — NO reboot. We just need a fresh process to pick it
			// up (a clean audio context that enumerates the new cable and
			// auto-engages routing), which is an APP restart, not a Windows reboot.
			// If the cable still isn't there after the restart, that machine
			// genuinely needs a Windows reboot — the relaunched app will show that.
			dialog.ShowConfirm("VB-CABLE installed",
				"VB-CABLE is installed and Windows audio was refreshed — on most PCs this "+
					"needs NO Windows reboot.\n\nSoundBoard just needs to restart (the app, "+
					"not Windows) to pick up the cable and route automatically. Restart now?",
				func(ok bool) {
					if ok {
						a.restart()
						return
					}
					a.refreshBanner()
				}, a.win)
		})
		// Completion hook (nil in production). Fires AFTER the fyne.Do UI update so a
		// test can join this goroutine — including its widget builds — before
		// asserting, instead of polling a counter while the goroutine is still
		// touching Fyne widgets. See App.onFixDone.
		if a.onFixDone != nil {
			a.onFixDone()
		}
	}()
}

// refreshBanner rebuilds the whole window content so the banner reflects the
// latest SetupController.Status (e.g. after a successful install). Cheap enough
// for a one-off user action.
func (a *App) refreshBanner() {
	if a.win != nil {
		a.win.SetContent(a.buildContent())
		// buildContent stopped the old meter ticker and rebuilt the panel (with a
		// fresh gateUpdate closure bound to the new widgets); restart it so the
		// live meter keeps running after the rebuild. No-op without an Audio panel.
		a.startGateTicker()
	}
}

// buildClipBrowser builds the search box and the scrollable, category-grouped
// sections of clip buttons. A pinned "★ Favourites" section sits above the
// categories. Live filtering re-renders every section (including favourites) on
// each keystroke; it stays usable for 200+ clips because the sections live inside
// a single vertical scroll and empty categories are dropped. A prominent "Stop"
// button sits in the search row to silence all playing clips at once.
func (a *App) buildClipBrowser() fyne.CanvasObject {
	search := widget.NewEntry()
	search.SetPlaceHolder("Search clips by name or category…")
	search.SetText(a.search)

	// favSection holds the pinned favourites; it is hidden when empty/filtered out.
	favSection := container.NewVBox()
	sections := container.NewVBox()
	empty := widget.NewLabel("No clips match your search.")
	empty.Alignment = fyne.TextAlignCenter
	empty.Hide()

	a.rebuildBrowser = func() {
		favSection.RemoveAll()
		favShown := a.renderFavorites(favSection)
		if favShown == 0 {
			favSection.Hide()
		} else {
			favSection.Show()
		}
		favSection.Refresh()

		sections.RemoveAll()
		shown := a.renderSections(sections)
		if shown == 0 && favShown == 0 {
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

	// Stop button: a prominent, danger-coloured "stop all sounds" action.
	stop := widget.NewButtonWithIcon("Stop", theme.MediaStopIcon(), func() {
		if a.player != nil {
			a.player.StopAll()
		}
	})
	stop.Importance = widget.DangerImportance

	body := container.NewVScroll(container.NewVBox(favSection, sections, empty))
	// search box fills the row; the search icon hugs the left and the Stop button
	// the right so it is always reachable above the scrolling clip list.
	header := container.NewBorder(nil, nil, widget.NewIcon(theme.SearchIcon()), stop, search)
	return container.NewBorder(container.NewPadded(header), nil, nil, nil, body)
}

// renderFavorites appends the pinned "★ Favourites" section listing the
// favourited clips (in Favorites() order) that pass the current filter, and
// returns how many were shown. Returns 0 (and adds nothing) when there is no
// FavoritesController or no matching favourites, so the caller can hide the
// section. Each entry is the same play-button-plus-star cell as the categories.
func (a *App) renderFavorites(into *fyne.Container) int {
	if a.favs == nil || a.lib == nil {
		return 0
	}
	var cells []fyne.CanvasObject
	for _, id := range a.favs.Favorites() {
		clip := a.lib.Get(id)
		if clip == nil {
			continue // a favourited clip whose file was removed: skip silently
		}
		if !match(clip, a.search) {
			continue
		}
		cells = append(cells, a.clipCell(clip))
	}
	if len(cells) == 0 {
		return 0
	}
	title := widget.NewLabelWithStyle(
		fmt.Sprintf("★ Favourites  (%d)", len(cells)),
		fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
	grid := container.NewGridWrap(clipButtonSize, cells...)
	into.Add(container.NewVBox(title, grid, widget.NewSeparator()))
	return len(cells)
}

// renderSections appends one labelled category section per non-empty,
// filter-matching category and returns how many clip cells were shown total.
func (a *App) renderSections(into *fyne.Container) int {
	if a.lib == nil {
		return 0
	}
	total := 0
	for i := range a.lib.Categories {
		cat := &a.lib.Categories[i]
		cells := a.categoryButtons(cat)
		if len(cells) == 0 {
			continue
		}
		total += len(cells)

		title := widget.NewLabelWithStyle(
			fmt.Sprintf("%s  (%d)", prettyCategory(cat.Name), len(cells)),
			fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
		grid := container.NewGridWrap(clipButtonSize, cells...)
		into.Add(container.NewVBox(title, grid, widget.NewSeparator()))
	}
	return total
}

// categoryButtons builds the clip cells for the clips in cat that pass the
// current filter. Each cell is a play button plus a compact star toggle.
func (a *App) categoryButtons(cat *catalog.Category) []fyne.CanvasObject {
	var cells []fyne.CanvasObject
	for _, clip := range cat.Clips {
		if !match(clip, a.search) {
			continue
		}
		cells = append(cells, a.clipCell(clip))
	}
	return cells
}

// clipCell builds one grid cell: a leading-aligned play button (taps to play at
// the clip's saved per-clip gain and selects it into the per-clip slider) plus a
// compact star toggle on the right. The star shows ★ when favourited and ☆ when
// not; tapping it toggles the favourite and re-renders the browser so both the
// star state and the pinned Favourites section update live. When no
// FavoritesController is attached the cell is just the play button.
func (a *App) clipCell(clip *catalog.Clip) fyne.CanvasObject {
	c := clip // capture per-iteration
	play := widget.NewButtonWithIcon(c.Name, theme.MediaPlayIcon(), func() {
		a.play(c)
	})
	play.Alignment = widget.ButtonAlignLeading

	if a.favs == nil {
		return play
	}

	label := "☆"
	if a.favs.IsFavorite(c.ID) {
		label = "★"
	}
	star := widget.NewButton(label, func() {
		a.favs.ToggleFavorite(c.ID)
		if a.rebuildBrowser != nil {
			a.rebuildBrowser()
		}
	})
	star.Importance = widget.LowImportance
	// Star on the right; the play button takes the remaining width.
	return container.NewBorder(nil, nil, nil, star, play)
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
