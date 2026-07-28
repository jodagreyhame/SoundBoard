package main

// Wails-bound methods for the clip library's location.
//
// Kept out of app.go, which is already well past the size where it is pleasant
// to navigate. Everything here follows the same contract as the rest of the
// bound surface: no Wails runtime calls unless ctx is present, all settings
// mutation under lcMu, and every failure returned to the front end rather than
// only logged — the bug this feature fixes was a silent one.

import (
	"errors"
	"fmt"
	"log"
	"os/exec"

	"github.com/wailsapp/wails/v2/pkg/runtime"
)

// appVersion is the single runtime source of the app's version, surfaced to the
// UI through GetState so the badge in the title bar cannot drift from the
// binary the way a hard-coded "v2" in index.html did.
//
// Keep it in step with productVersion in wails.json, which feeds the Windows
// version resource, and with the release tag.
const appVersion = "1.0.0"

// ClipFolderInfo is the front end's view of where clips live.
type ClipFolderInfo struct {
	Path       string   `json:"path"`
	IsDefault  bool     `json:"isDefault"`
	Error      string   `json:"error"`
	Categories int      `json:"categories"`
	Clips      int      `json:"clips"`
	Warnings   []string `json:"warnings"`
	NoticeSeen bool     `json:"noticeSeen"`
}

func (a *App) clipFolderInfo(state clipFolderState, warnings []string) ClipFolderInfo {
	if warnings == nil {
		warnings = []string{}
	}
	info := ClipFolderInfo{
		Path:       state.Path,
		IsDefault:  state.IsDefault,
		Error:      state.Error,
		Categories: state.Categories,
		Clips:      state.Clips,
		Warnings:   warnings,
	}
	if b := a.getBackend(); b != nil && b.settings != nil {
		a.lcMu.Lock()
		info.NoticeSeen = b.settings.ClipFolderNoticeSeen
		a.lcMu.Unlock()
	}
	return info
}

// ClipFolder reports the current clip library location and what is in it.
func (a *App) ClipFolder() ClipFolderInfo {
	b := a.getBackend()
	if b == nil {
		return ClipFolderInfo{Error: "backend unavailable", Warnings: []string{}}
	}
	return a.clipFolderInfo(b.clipFolderSnapshot(), nil)
}

// ReloadLibrary re-indexes the current clip folder without a restart, so newly
// added clips appear without relaunching the app.
func (a *App) ReloadLibrary() (ClipFolderInfo, error) {
	b := a.getBackend()
	if b == nil {
		return ClipFolderInfo{Warnings: []string{}}, errors.New("backend unavailable")
	}

	state, err := b.reloadClipFolder()
	info := a.clipFolderInfo(state, nil)
	if err != nil {
		return info, fmt.Errorf("reload clip library: %w", err)
	}
	a.emitLibraryChanged()
	return info, nil
}

// ChooseClipFolder opens a directory picker and points the library at the
// result.
//
// The new folder is scanned BEFORE the choice is saved or swapped in: saving
// first would leave the config naming a folder the running app is not using, so
// a failure here would boot the next launch into a broken library with no
// obvious cause. If anything fails, nothing changes at all.
func (a *App) ChooseClipFolder() (ClipFolderInfo, error) {
	b := a.getBackend()
	if b == nil {
		return ClipFolderInfo{Warnings: []string{}}, errors.New("backend unavailable")
	}
	current := a.clipFolderInfo(b.clipFolderSnapshot(), nil)

	if a.ctx == nil {
		// No Wails runtime means no dialog. Say so rather than appearing to do
		// nothing.
		return current, errors.New("folder picker unavailable")
	}

	// The window is frameless and can be hidden to the tray; show it first so
	// the dialog is never parented to something the user cannot see.
	runtime.WindowShow(a.ctx)

	picked, err := runtime.OpenDirectoryDialog(a.ctx, runtime.OpenDialogOptions{
		Title:                "Choose the folder holding your clips",
		DefaultDirectory:     current.Path,
		CanCreateDirectories: true,
	})
	if err != nil {
		return current, fmt.Errorf("open folder picker: %w", err)
	}
	if picked == "" {
		return current, nil // cancelled
	}

	state, warnings, err := b.changeClipFolder(picked)
	info := a.clipFolderInfo(state, warnings)
	if err != nil {
		return info, err
	}

	// Persisted only now that the scan succeeded, and flushed rather than
	// debounced: a folder change is not a slider drag, and losing it to a crash
	// in the next few hundred milliseconds would be baffling.
	a.lcMu.Lock()
	if b.settings != nil {
		b.settings.ClipFolder = state.Path
	}
	a.lcMu.Unlock()
	a.flushPersist()

	a.emitLibraryChanged()
	return info, nil
}

// OpenClipFolder reveals the clip folder in Explorer.
func (a *App) OpenClipFolder() error {
	b := a.getBackend()
	if b == nil {
		return errors.New("backend unavailable")
	}
	path, _, _ := b.clipFolderState()
	if path == "" {
		return errors.New("no clip folder is configured")
	}

	// Explorer returns a non-zero exit status even when it succeeds, so its
	// error is deliberately not treated as failure; only a failure to start it
	// at all is reported.
	cmd := exec.Command("explorer", path)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("open '%s': %w", path, err)
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

// DismissClipFolderNotice records that the user has seen where clips live, so
// the first-run notice does not reappear on every launch.
func (a *App) DismissClipFolderNotice() {
	b := a.getBackend()
	if b == nil || b.settings == nil {
		return
	}
	a.lcMu.Lock()
	b.settings.ClipFolderNoticeSeen = true
	a.lcMu.Unlock()
	a.flushPersist()
}

// emitLibraryChanged tells the front end to re-pull its snapshot. The grid is
// rendered from GetState, so a single signal is enough; there is no separate
// incremental path to keep in sync.
func (a *App) emitLibraryChanged() {
	if a.ctx == nil {
		return
	}
	runtime.EventsEmit(a.ctx, "libraryChanged", map[string]any{})
	log.Printf("library: reloaded; front end notified")
}
