// Package config persists user settings as JSON under
// os.UserConfigDir()/soundboard/config.json.
package config

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// appDir is the per-application subdirectory under the OS user config dir.
const appDir = "soundboard"

// configFile is the settings file name within appDir.
const configFile = "config.json"

// Settings is the persisted user configuration. Hotkeys maps a combo string
// (e.g. "ctrl+alt+1") to a clip ID.
type Settings struct {
	MicName     string            `json:"micName"`
	CableName   string            `json:"cableName"`
	MonitorName string            `json:"monitorName"`
	Monitor     bool              `json:"monitor"`
	Hotkeys     map[string]string `json:"hotkeys"`

	// Volumes holds the in-app mixer levels (all managed inside SoundBoard;
	// nothing in Discord is changed by the user). Omitted when empty so an
	// upgraded config without volumes round-trips to its prior shape.
	Volumes Volumes `json:"volumes,omitempty"`

	// Window holds the Fyne main-window placement/size preferences so the app
	// reopens where the user left it. Omitted when empty.
	Window WindowPrefs `json:"window,omitempty"`
}

// Volumes are the soundboard mixer levels, all linear amplitudes where 1.0
// means unchanged. Mic scales the live mic passthrough; Master scales every
// clip; PerClip maps a clip ID to its own multiplier (applied on top of
// Master). A missing or zero PerClip entry means unity.
type Volumes struct {
	Mic     float32            `json:"mic,omitempty"`
	Master  float32            `json:"master,omitempty"`
	PerClip map[string]float32 `json:"perClip,omitempty"`
}

// WindowPrefs persists the main window's last size and whether it was shown.
// Width/Height of 0 mean "use the default size".
type WindowPrefs struct {
	Width     float32 `json:"width,omitempty"`
	Height    float32 `json:"height,omitempty"`
	Maximized bool    `json:"maximized,omitempty"`
}

// normalize fills in safe defaults so callers never read a zero mixer level or
// nil PerClip map. A missing or zero Mic/Master means unity (1.0); a nil
// PerClip becomes an empty map. This is applied after Load so the in-app mixer
// starts at full volume on a fresh config and is never nil-dereferenced.
func (s *Settings) normalize() {
	if s.Hotkeys == nil {
		s.Hotkeys = map[string]string{}
	}
	if s.Volumes.Mic == 0 {
		s.Volumes.Mic = 1
	}
	if s.Volumes.Master == 0 {
		s.Volumes.Master = 1
	}
	if s.Volumes.PerClip == nil {
		s.Volumes.PerClip = map[string]float32{}
	}
}

// dir returns the absolute path to the application's config directory.
func dir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, appDir), nil
}

// path returns the absolute path to the config file.
func path() (string, error) {
	d, err := dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, configFile), nil
}

// logFile is the diagnostics log file name within appDir.
const logFile = "soundboard.log"

// LogPath returns the absolute path to the diagnostics log file and ensures the
// application config directory exists. Under the shipping GUI build
// (-H=windowsgui) stderr is detached, so user-facing diagnostics are written
// here instead of being silently lost.
func LogPath() (string, error) {
	d, err := dir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(d, 0o755); err != nil {
		return "", err
	}
	return filepath.Join(d, logFile), nil
}

// Load reads settings from disk. If the file is missing it returns a
// zero-value *Settings (with an initialized Hotkeys map) and a nil error.
func Load() (*Settings, error) {
	p, err := path()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			s := &Settings{}
			s.normalize()
			return s, nil
		}
		return nil, err
	}
	s := &Settings{}
	if err := json.Unmarshal(data, s); err != nil {
		return nil, err
	}
	s.normalize()
	return s, nil
}

// Save creates the config directory if needed and writes pretty-printed JSON
// atomically (write to a temp file in the same directory, then rename).
func (s *Settings) Save() error {
	d, err := dir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(d, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	tmp, err := os.CreateTemp(d, configFile+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	// Best-effort cleanup if anything below fails before a successful rename.
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	final := filepath.Join(d, configFile)
	// On Windows, os.Rename fails if the destination exists; remove it first.
	if err := os.Rename(tmpName, final); err != nil {
		if err := os.Remove(final); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if err := os.Rename(tmpName, final); err != nil {
			return err
		}
	}
	return nil
}
