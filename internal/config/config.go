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

	// Theme is the persisted UI theme for the Wails front end, one of "dark" |
	// "light". Empty means "unset" — the UI defaults an empty value to dark itself,
	// so normalize() deliberately does NOT coerce it (keeping a fresh config's
	// round-trip byte-identical and the existing config tests passing). Omitted
	// from JSON when empty.
	Theme string `json:"theme,omitempty"`

	// Volumes holds the in-app mixer levels (all managed inside SoundBoard;
	// nothing in Discord is changed by the user). Omitted when empty so an
	// upgraded config without volumes round-trips to its prior shape.
	Volumes Volumes `json:"volumes,omitempty"`

	// Favorites is the ordered list of favourited clip IDs (canonically
	// extensionless "<category>/<basename>"). The UI pins these at the top of the
	// browser in this order. Omitted when empty so an upgraded config without
	// favourites round-trips to its prior shape. normalize() guarantees this is a
	// non-nil (possibly empty) slice so callers never nil-panic.
	Favorites []string `json:"favorites,omitempty"`

	// Window holds the Fyne main-window placement/size preferences so the app
	// reopens where the user left it. Omitted when empty.
	Window WindowPrefs `json:"window,omitempty"`

	// Processing holds the mic-path audio-processing suite settings (noise
	// suppression, AGC, gate/VAD mode, ducking, and the "force through" carrier).
	// These apply ONLY to the live mic before it is mixed into the cable;
	// soundboard clips bypass all of it. Omitted when empty so an upgraded config
	// without a processing block round-trips to its prior shape; normalize() fills
	// in sane defaults (e.g. MicMode "vad") on load.
	Processing AudioProcessing `json:"processing,omitempty"`
}

// MicMode names the four mic-gate behaviors. Stored as a lowercase string in the
// config so it is human-editable and forward-compatible. normalize() coerces an
// empty or unrecognized value back to MicModeVAD.
const (
	// MicModeVAD opens the gate by voice activity (RMS/VAD), the default.
	MicModeVAD = "vad"
	// MicModePTT opens the mic only while the configured PTTHotkey is held.
	MicModePTT = "ptt"
	// MicModeAlways keeps the gate open (no gating; processing still applies).
	MicModeAlways = "always"
	// MicModeMute keeps the gate closed (mic silenced to Discord).
	MicModeMute = "mute"
)

// AudioProcessing is the persisted configuration of the mic-path processing
// suite. Every field is independent and applies ONLY to the live microphone
// (input gain -> mono -> HPF -> denoise -> AGC -> gate -> stereo) before it is
// mixed into the cable; triggered soundboard clips are summed in AFTER this chain
// and are never denoised, gated, or leveled.
//
// Defaults (filled by normalize when unset): NoiseSuppression off, AGC off,
// Ducking off, MicMode "vad", GateSensitivity ~0.15, ForceThrough off, no PTT
// hotkey. With everything at its default the suite gates the mic by voice
// activity and otherwise passes clean voice through, so the user can run Discord
// with all of Discord's own processing OFF.
type AudioProcessing struct {
	// NoiseSuppression runs RNNoise on the mic frames when true. A no-op when the
	// build lacks cgo/RNNoise (the engine falls back to passthrough), so enabling
	// it is always safe.
	NoiseSuppression bool `json:"noiseSuppression,omitempty"`
	// AGC enables the RMS-target automatic gain leveler on the mic.
	AGC bool `json:"agc,omitempty"`
	// Ducking lowers soundboard clips slightly while the mic gate is open (and
	// vice-versa) via an envelope follower.
	Ducking bool `json:"ducking,omitempty"`
	// MicMode selects the gate behavior: one of "vad", "ptt", "always", "mute".
	// normalize() defaults an empty/invalid value to "vad".
	MicMode string `json:"micMode,omitempty"`
	// GateSensitivity is the VAD/RMS gate threshold in [0,1]; higher = the gate
	// requires a louder voice to open. normalize() defaults 0 to ~0.15 and clamps
	// to [0,1].
	GateSensitivity float32 `json:"gateSensitivity,omitempty"`
	// ForceThrough is a RETAINED-BUT-INERT setting. It formerly enabled a continuous
	// voiced "carrier" bed on the CABLE path to hold Discord's voice-activity gate
	// open; that carrier was a static tone (a buzz by construction) and has been
	// removed from the audio engine. The field is kept so existing saved settings
	// round-trip without error, but the engine ignores it. Default off.
	ForceThrough bool `json:"forceThrough,omitempty"`
	// PTTHotkey is the combo (e.g. "ctrl+grave") that opens the mic in "ptt" mode.
	// Empty means no PTT binding is registered. Parsed by internal/hotkeys.
	PTTHotkey string `json:"pttHotkey,omitempty"`
	// MonitorSource selects what the local monitor (the user's headset) plays: one
	// of "clips" (the default — clean clips only, the user hears their own voice
	// acoustically) or "transmitted" (the confidence monitor — the EXACT cable-bound
	// mix of processedMic + clips, so the user can audit what Discord receives).
	// normalize() defaults an empty/invalid value to "clips". This is a monitor
	// auditing aid only; it never changes what is transmitted to Discord.
	MonitorSource string `json:"monitorSource,omitempty"`
}

// MonitorSource names the two monitor-source modes. Stored as a lowercase string
// in the config so it is human-editable and forward-compatible. normalize()
// coerces an empty or unrecognized value back to MonitorSourceClips.
const (
	// MonitorSourceClips plays only the triggered clips on the monitor (default).
	MonitorSourceClips = "clips"
	// MonitorSourceTransmitted plays the exact cable-bound mix (processedMic + clips)
	// on the monitor — the confidence monitor.
	MonitorSourceTransmitted = "transmitted"
)

// validMonitorSource reports whether m is one of the two recognized monitor sources.
func validMonitorSource(m string) bool {
	switch m {
	case MonitorSourceClips, MonitorSourceTransmitted:
		return true
	}
	return false
}

// defaultGateSensitivity is the gate threshold used when none is configured. Low
// enough that a normal speaking voice opens the gate, high enough to reject idle
// room/keyboard noise. Mirrors the contract default (~0.15).
const defaultGateSensitivity float32 = 0.15

// validMicMode reports whether m is one of the four recognized gate modes.
func validMicMode(m string) bool {
	switch m {
	case MicModeVAD, MicModePTT, MicModeAlways, MicModeMute:
		return true
	}
	return false
}

// Volumes are the soundboard mixer levels, all linear amplitudes where 1.0
// means unchanged. The three top-level levels are INDEPENDENT:
//   - Mic     scales the live mic passthrough (how loud YOUR VOICE is to Discord).
//   - Master  scales every clip on the path to Discord (what OTHERS hear).
//   - Monitor scales every clip on the local path (what YOU hear in your headset).
//
// PerClip maps a clip ID to its own multiplier, applied on top of both Master
// and Monitor. A missing or zero PerClip entry means unity.
//
// Mic/Master/Monitor are POINTERS so the config can tell "unset" (nil — an
// upgraded/fresh config that never wrote a level, which must default to unity so
// the user still hears clips) apart from an EXPLICIT 0 (a deliberate mute the user
// dragged, which must round-trip rather than silently un-mute on the next launch).
// A non-nil pointer to 0 persists as `"master": 0` and reloads as a real mute; a
// nil pointer is omitted from JSON and seeded to unity by normalize(). Use the
// MicGain/MasterGain/MonitorGain accessors to read an effective value (nil ->
// unity) without dereferencing.
type Volumes struct {
	Mic     *float32           `json:"mic,omitempty"`
	Master  *float32           `json:"master,omitempty"`
	Monitor *float32           `json:"monitor,omitempty"`
	PerClip map[string]float32 `json:"perClip,omitempty"`
}

// FloatPtr returns a pointer to a copy of v. Callers use it to set an EXPLICIT
// Volumes level — including an explicit 0 mute — so the value round-trips through
// JSON instead of being treated as unset and coerced back to unity.
func FloatPtr(v float32) *float32 { return &v }

// orUnity returns the pointed-to value, or 1.0 (unity) when p is nil (unset).
// Centralizes the "nil means unity, explicit 0 means muted" rule so an explicit 0
// is honoured everywhere instead of being coerced back to full volume.
func orUnity(p *float32) float32 {
	if p == nil {
		return 1
	}
	return *p
}

// MicGain/MasterGain/MonitorGain return the effective level for each independent
// channel: the explicitly-configured value (including an explicit 0 mute) when set,
// else unity. Callers (the engine seeding, the UI sliders) use these so a saved
// mute is respected and an unset channel still defaults to full volume.
func (v Volumes) MicGain() float32     { return orUnity(v.Mic) }
func (v Volumes) MasterGain() float32  { return orUnity(v.Master) }
func (v Volumes) MonitorGain() float32 { return orUnity(v.Monitor) }

// WindowPrefs persists the main window's last content size so the app reopens at
// the size the user left it. Width/Height of 0 mean "use the default size". The
// ui layer reads these on build and writes the latest size back on window close
// or quit, which main persists via Settings.Save.
type WindowPrefs struct {
	Width  float32 `json:"width,omitempty"`
	Height float32 `json:"height,omitempty"`
}

// normalize fills in safe defaults so callers never read a nil mixer level or nil
// PerClip map. An UNSET (nil) Mic/Master/Monitor is seeded to unity (1.0) so a
// fresh or pre-Monitor config still passes the mic through and HEARS clips; an
// EXPLICIT level — including an explicit 0 the user dragged to mute — is preserved
// verbatim so the mute round-trips instead of silently un-muting on the next
// launch. A nil PerClip becomes an empty map. Applied after Load.
func (s *Settings) normalize() {
	if s.Hotkeys == nil {
		s.Hotkeys = map[string]string{}
	}
	if s.Volumes.Mic == nil {
		s.Volumes.Mic = FloatPtr(1)
	}
	if s.Volumes.Master == nil {
		s.Volumes.Master = FloatPtr(1)
	}
	if s.Volumes.Monitor == nil {
		s.Volumes.Monitor = FloatPtr(1)
	}
	if s.Volumes.PerClip == nil {
		s.Volumes.PerClip = map[string]float32{}
	}
	if s.Favorites == nil {
		s.Favorites = []string{}
	}
	s.Processing.normalize()
}

// normalize fills in the mic-processing defaults so a fresh or upgraded config
// has a usable gate mode and a sane gate sensitivity. An empty or unrecognized
// MicMode becomes "vad"; a zero GateSensitivity becomes the default, and any
// value is clamped to [0,1] so the engine never sees an out-of-range threshold.
// The bool toggles default to false (off), matching the contract.
func (p *AudioProcessing) normalize() {
	if !validMicMode(p.MicMode) {
		p.MicMode = MicModeVAD
	}
	if p.GateSensitivity == 0 {
		p.GateSensitivity = defaultGateSensitivity
	}
	if p.GateSensitivity < 0 {
		p.GateSensitivity = 0
	}
	if p.GateSensitivity > 1 {
		p.GateSensitivity = 1
	}
	if !validMonitorSource(p.MonitorSource) {
		p.MonitorSource = MonitorSourceClips
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
