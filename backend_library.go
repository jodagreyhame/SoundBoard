package main

// Clip-folder bootstrap, rescanning, and relocation.
//
// The library lives in the engine (single owner). Everything here is about
// deciding WHICH directory to index and reporting honestly when that fails —
// the previous implementation created a sounds/ folder beside the executable
// and discarded the error, so a library that could not be written or read
// presented as an empty grid with no explanation.

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/jodagreyhame/SoundBoard/internal/catalog"
	"github.com/jodagreyhame/SoundBoard/internal/library"
)

// clipFolderState is an immutable snapshot of where the clips are and whether
// getting them worked, for the UI to render.
type clipFolderState struct {
	Path       string `json:"path"`
	IsDefault  bool   `json:"isDefault"`
	Error      string `json:"error"`
	Categories int    `json:"categories"`
	Clips      int    `json:"clips"`
}

// initClipFolder resolves the clip folder, creates it if it is the default, and
// indexes it. It always returns a usable library — an empty one on failure —
// because the window opening with an explained empty grid beats not opening.
func (b *Backend) initClipFolder() *catalog.Library {
	configured := ""
	if b.settings != nil {
		configured = b.settings.ClipFolder
	}

	path, isDefault, err := library.Resolve(configured)
	if err != nil {
		// Resolve still returns a usable default when it rejects a configured
		// path, so this is a warning the user must see rather than a dead end.
		log.Printf("clip folder: %v", err)
		b.setClipFolder(path, isDefault, err.Error())
	} else {
		b.setClipFolder(path, isDefault, "")
	}

	if path == "" {
		log.Printf("clip folder: no usable location found; starting with an empty library")
		return catalog.Empty()
	}

	if err := library.Ensure(path, isDefault); err != nil {
		log.Printf("clip folder: %v", err)
		b.setClipFolder(path, isDefault, err.Error())
		return catalog.Empty()
	}

	lib, err := b.indexClipFolder(path)
	if err != nil {
		log.Printf("clip folder: %v", err)
		b.setClipFolder(path, isDefault, err.Error())
		return catalog.Empty()
	}
	return lib
}

// indexClipFolder walks path and logs what it found. The count matters in the
// log: "0 clips" alongside the resolved path is usually the whole diagnosis
// when a user reports an empty grid.
func (b *Backend) indexClipFolder(path string) (*catalog.Library, error) {
	lib, err := catalog.New(os.DirFS(path))
	if err != nil {
		return nil, fmt.Errorf("index '%s': %w", path, err)
	}
	cats, clips := libraryCounts(lib)
	log.Printf("library: %d categories, %d clips indexed from %s (decoded on demand)", cats, clips, path)
	return lib, nil
}

// reloadClipFolder re-indexes the current folder and swaps the result in. Clips
// already playing finish against the old library; new triggers resolve against
// the new one.
func (b *Backend) reloadClipFolder() (clipFolderState, error) {
	path, isDefault, _ := b.clipFolderState()
	if path == "" {
		return b.clipFolderSnapshot(), fmt.Errorf("no clip folder is configured")
	}

	if _, err := library.Validate(path); err != nil {
		b.setClipFolder(path, isDefault, err.Error())
		return b.clipFolderSnapshot(), err
	}

	lib, err := b.indexClipFolder(path)
	if err != nil {
		b.setClipFolder(path, isDefault, err.Error())
		return b.clipFolderSnapshot(), err
	}

	b.swapLibrary(lib)
	b.setClipFolder(path, isDefault, "")
	return b.clipFolderSnapshot(), nil
}

// changeClipFolder points the app at a new directory.
//
// The scan happens BEFORE anything is persisted or swapped: persisting first
// would leave config.json naming a folder the running app is not using, so a
// failure here would boot the NEXT launch into a broken library with no obvious
// cause. On any failure nothing changes at all.
func (b *Backend) changeClipFolder(path string) (clipFolderState, []string, error) {
	warnings, err := library.Validate(path)
	if err != nil {
		return b.clipFolderSnapshot(), nil, err
	}

	lib, err := b.indexClipFolder(path)
	if err != nil {
		return b.clipFolderSnapshot(), warnings, err
	}

	defaultPath, defErr := library.DefaultDir()
	isDefault := defErr == nil && samePath(path, defaultPath)

	b.swapLibrary(lib)
	b.setClipFolder(path, isDefault, "")

	for _, w := range warnings {
		log.Printf("clip folder: %s", w)
	}
	log.Printf("clip folder: now using %s", path)
	return b.clipFolderSnapshot(), warnings, nil
}

// swapLibrary installs lib as the live library. Safe while clips are playing.
func (b *Backend) swapLibrary(lib *catalog.Library) {
	if b.engine != nil {
		b.engine.SetLibrary(lib)
	}
}

// currentLibrary returns the live library, never nil, so callers can range over
// it without a guard.
func (b *Backend) currentLibrary() *catalog.Library {
	if b.engine != nil {
		if lib := b.engine.Library(); lib != nil {
			return lib
		}
	}
	return catalog.Empty()
}

func (b *Backend) setClipFolder(path string, isDefault bool, errMsg string) {
	b.libMu.Lock()
	defer b.libMu.Unlock()
	b.clipFolder = path
	b.clipFolderIsDefault = isDefault
	b.clipFolderErr = errMsg
}

func (b *Backend) clipFolderState() (path string, isDefault bool, errMsg string) {
	b.libMu.RLock()
	defer b.libMu.RUnlock()
	return b.clipFolder, b.clipFolderIsDefault, b.clipFolderErr
}

// clipFolderSnapshot pairs the folder metadata with live counts from the
// library actually loaded, so the UI cannot show a path and a clip count that
// disagree.
func (b *Backend) clipFolderSnapshot() clipFolderState {
	path, isDefault, errMsg := b.clipFolderState()
	cats, clips := libraryCounts(b.currentLibrary())
	return clipFolderState{
		Path:       path,
		IsDefault:  isDefault,
		Error:      errMsg,
		Categories: cats,
		Clips:      clips,
	}
}

// samePath compares two directories the way Windows does — case-insensitively,
// through any symlink or junction — so "the default folder" is recognised even
// when the user picks it via the browse dialog with different casing.
func samePath(a, b string) bool {
	ra, err := filepath.EvalSymlinks(a)
	if err != nil {
		ra = a
	}
	rb, err := filepath.EvalSymlinks(b)
	if err != nil {
		rb = b
	}
	return strings.EqualFold(filepath.Clean(ra), filepath.Clean(rb))
}

func libraryCounts(lib *catalog.Library) (categories, clips int) {
	if lib == nil {
		return 0, 0
	}
	for _, c := range lib.Categories {
		clips += len(c.Clips)
	}
	return len(lib.Categories), clips
}
