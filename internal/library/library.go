// Package library resolves where the user's clip library lives on disk.
//
// It answers exactly one question — which directory holds the clips — and
// deliberately knows nothing about indexing them (internal/catalog) or playing
// them (internal/audio). The clip folder is a user-visible, user-editable
// location that must survive the executable being moved, so it is NOT derived
// from os.Executable(): it defaults to <Documents>/SoundBoard and can be
// pointed anywhere, with the choice persisted in the app config.
package library

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// AppFolderName is the directory created inside Documents by default.
const AppFolderName = "SoundBoard"

// configAppDir mirrors the directory internal/config uses under
// os.UserConfigDir(). Duplicated rather than imported to keep this package free
// of a dependency on config, which would otherwise be circular once config
// grows a clip-folder preference.
const configAppDir = "soundboard"

// exampleCategory is seeded into a freshly created default folder. The catalog
// only indexes <category>/<file>, so a user who drops clips directly into the
// clip folder gets nothing; an example directory makes the required shape
// self-evident the moment the folder is opened.
const exampleCategory = "examples"

const exampleReadme = `Put your sound clips in folders inside this directory.

Each folder becomes a category in SoundBoard:

    SoundBoard\memes\airhorn.wav
    SoundBoard\games\coin.mp3

Supported formats: .wav .mp3 .flac .ogg

Files placed directly in this directory, outside any folder, are NOT loaded.
After adding clips, press Reload in SoundBoard.
`

// longPathWarnThreshold leaves headroom under the legacy MAX_PATH of 260 for a
// <category>\<file>.<ext> tail. Go transparently \\?\-prefixes absolute paths so
// the app itself copes, but Explorer and most third-party tools do not.
const longPathWarnThreshold = 240

// documentsDir is the platform lookup for the user's Documents folder, held in
// a variable so tests can substitute it without a real Windows environment.
var documentsDir = platformDocumentsDir

// DefaultDir returns the default clip library path, <Documents>/SoundBoard.
func DefaultDir() (string, error) {
	docs, err := documentsDir()
	if err != nil {
		return "", fmt.Errorf("locate Documents folder: %w", err)
	}
	return filepath.Join(docs, AppFolderName), nil
}

// Resolve decides which directory the clip library should be read from.
//
// When configured is empty the default is returned. When configured is set but
// unusable, Resolve returns the DEFAULT path together with a non-nil error
// explaining the rejection: the caller gets a working app and an explanation it
// is expected to log and surface. Falling back without telling anyone is the
// failure mode this whole package exists to remove, so callers must not discard
// the error just because path is non-empty.
func Resolve(configured string) (path string, isDefault bool, err error) {
	def, defErr := DefaultDir()

	if strings.TrimSpace(configured) == "" {
		return def, true, defErr
	}

	if _, vErr := Validate(configured); vErr != nil {
		if defErr != nil {
			return "", true, fmt.Errorf("configured clip folder '%s' unusable (%w) and no default available: %v", configured, vErr, defErr)
		}
		return def, true, fmt.Errorf("configured clip folder '%s' unusable, falling back to '%s': %w", configured, def, vErr)
	}

	return filepath.Clean(configured), filepath.Clean(configured) == filepath.Clean(def), nil
}

// Ensure makes the clip folder usable.
//
// A missing DEFAULT folder is created and seeded with an example category. A
// missing user-chosen folder is an error: silently recreating a path whose
// drive was unplugged would hide the problem and index into the wrong place.
func Ensure(path string, isDefault bool) error {
	if path == "" {
		return errors.New("clip folder path is empty")
	}

	st, err := os.Stat(path)
	switch {
	case err == nil && st.IsDir():
		return nil
	case err == nil:
		return fmt.Errorf("clip folder '%s' is a file, not a directory", path)
	case !os.IsNotExist(err):
		return fmt.Errorf("stat clip folder '%s': %w", path, err)
	}

	if !isDefault {
		return fmt.Errorf("clip folder '%s' does not exist", path)
	}

	if err := os.MkdirAll(filepath.Join(path, exampleCategory), 0o755); err != nil {
		return fmt.Errorf("create clip folder '%s': %w", path, err)
	}

	readme := filepath.Join(path, exampleCategory, "README.txt")
	if err := os.WriteFile(readme, []byte(exampleReadme), 0o644); err != nil {
		// The folder itself exists, which is what matters; the README is a hint.
		return fmt.Errorf("seed '%s': %w", readme, err)
	}
	return nil
}

// Validate reports whether path is usable as a clip library root.
//
// Warnings describe conditions that are allowed but likely to disappoint (a
// very large tree, a path long enough to upset Explorer). A non-nil error means
// the path must be rejected outright.
func Validate(path string) (warnings []string, err error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("path is empty")
	}
	if !filepath.IsAbs(path) {
		// A relative path is resolved against the working directory, which is
		// exactly the launch-directory-dependent bug this change removes.
		return nil, fmt.Errorf("path '%s' is relative; an absolute path is required", path)
	}

	clean := filepath.Clean(path)

	// ReadDir rather than Stat: Stat succeeds on a directory the process cannot
	// list, which would then index as empty with no explanation.
	if _, err := os.ReadDir(clean); err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("path '%s' does not exist", clean)
		}
		return nil, fmt.Errorf("cannot read directory '%s': %w", clean, err)
	}

	if err := rejectReservedLocations(clean); err != nil {
		return nil, err
	}

	if len(clean) > longPathWarnThreshold {
		warnings = append(warnings, fmt.Sprintf("path is %d characters; Windows Explorer and some tools struggle beyond 260, leaving little room for category and file names", len(clean)))
	}
	if isBroadLocation(clean) {
		warnings = append(warnings, "this is a large, general-purpose folder; scanning it may be slow and will pick up unrelated audio")
	}
	return warnings, nil
}

// rejectReservedLocations refuses directories the app must not treat as a clip
// library: beside the executable (the bug being fixed) and the config directory
// (which holds config.json and the log).
func rejectReservedLocations(clean string) error {
	if exe, err := os.Executable(); err == nil {
		if same, err := sameDir(clean, filepath.Dir(exe)); err == nil && same {
			return errors.New("path is the folder containing SoundBoard.exe; the clip library must live outside it so it survives moving or reinstalling the app")
		}
	}
	if cfg, err := os.UserConfigDir(); err == nil {
		if same, err := sameDir(clean, filepath.Join(cfg, configAppDir)); err == nil && same {
			return errors.New("path is SoundBoard's configuration folder; choose a different directory")
		}
	}
	return nil
}

// sameDir compares two directories through their resolved paths so a symlink or
// junction cannot smuggle a rejected location past the check.
func sameDir(a, b string) (bool, error) {
	ra, err := filepath.EvalSymlinks(a)
	if err != nil {
		ra = a
	}
	rb, err := filepath.EvalSymlinks(b)
	if err != nil {
		rb = b
	}
	return strings.EqualFold(filepath.Clean(ra), filepath.Clean(rb)), nil
}

// isBroadLocation flags directories whose subtree is typically enormous. The
// catalog prunes below category depth, so these remain workable; the warning
// exists because the user probably meant a dedicated folder.
func isBroadLocation(clean string) bool {
	if vol := filepath.VolumeName(clean); vol != "" && filepath.Clean(vol+string(filepath.Separator)) == clean {
		return true
	}
	if home, err := os.UserHomeDir(); err == nil {
		if same, _ := sameDir(clean, home); same {
			return true
		}
	}
	if docs, err := documentsDir(); err == nil {
		if same, _ := sameDir(clean, docs); same {
			return true
		}
	}
	return false
}
