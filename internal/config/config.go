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
// Defaults (filled by normalize when unset): MicMode "vad", GateSensitivity
// ~0.15, AGC off, Ducking off, ForceThrough off, EchoCancellation off, no PTT
// hotkey, MonitorSource "clips", AudioSubsystem "standard". The Discord-parity
// breathing fix sets three NON-off defaults on a FRESH config: NoiseSuppressionTier
// "high", AdvancedVoiceActivity ON, and AutoSensitivity ON, plus AttenuationAmount
// 0.5. On an UPGRADED config the tier is derived from the legacy NoiseSuppression
// bool so a prior explicit choice is respected.
type AudioProcessing struct {
	// NoiseSuppression is the RETAINED LEGACY noise-suppression toggle (pre-tier
	// configs stored a single bool). It is kept so old configs round-trip and so
	// normalize() can derive NoiseSuppressionTier from it on upgrade (true ->
	// "standard", false -> "none"). Once NoiseSuppressionTier is set, the TIER is
	// authoritative and this bool is vestigial. The engine binding (SetNoiseSuppression)
	// still maps to/from this bool for back-compat.
	NoiseSuppression bool `json:"noiseSuppression,omitempty"`
	// NoiseSuppressionTier selects the noise-suppression strength, the Discord
	// "Noise Suppression" control's parity field. One of "none" | "standard" |
	// "high" | "strong":
	//   - none     -> APM noise suppression off.
	//   - standard -> APM NS Moderate (Discord "Standard").
	//   - high     -> APM NS High/VeryHigh (default; aggressive enough to shave
	//                 breath riding under the voice).
	//   - strong   -> RNNoise denoiser, our Krisp analog; APM NS auto-disables so
	//                 the two are never stacked.
	// normalize() defaults an UNSET tier on a FRESH config to "high" (the
	// breathing-kill default); on an UPGRADED config (one that already had a
	// processing block) it derives the tier from the legacy NoiseSuppression bool.
	// An unrecognized value coerces to "high".
	NoiseSuppressionTier string `json:"noiseSuppressionTier,omitempty"`
	// EchoCancellation toggles the APM acoustic echo canceller (Discord "Echo
	// Cancellation"). Exposed for parity; it is inert without a far-end render
	// reference (the engine never feeds process_reverse_stream), so it is documented
	// as a parity/no-op control. Default off.
	EchoCancellation bool `json:"echoCancellation,omitempty"`
	// AGC enables the RMS-target automatic gain leveler on the mic.
	AGC bool `json:"agc,omitempty"`
	// Ducking is the ATTENUATION toggle (Discord "Attenuation"): it lowers
	// soundboard clips while the mic gate is open (and vice-versa) via an envelope
	// follower. The attenuation DEPTH is AttenuationAmount.
	Ducking bool `json:"ducking,omitempty"`
	// AttenuationAmount is the attenuation depth in [0,1] applied while ducking is
	// active (Discord's "Attenuation amount" slider); 0 = no duck, 1 = full duck.
	// normalize() defaults 0 to 0.5 and clamps to [0,1].
	AttenuationAmount float32 `json:"attenuationAmount,omitempty"`
	// MicMode selects the gate behavior: one of "vad", "ptt", "always", "mute".
	// normalize() defaults an empty/invalid value to "vad".
	MicMode string `json:"micMode,omitempty"`
	// AdvancedVoiceActivity selects the VAD-gate implementation in "vad" MicMode:
	// when true (the default), the gate is driven by the RNNoise speech-activity
	// probability (a trained discriminator that does NOT open for breathing); when
	// false, the legacy energy/RMS latch is used. This is the breathing fix, so it
	// defaults ON. A *bool so a deliberate OFF round-trips: nil (unset) means the
	// default (true); a non-nil false is the user's explicit choice. Use
	// AdvancedVAD() to read the effective value. normalize() seeds nil to true.
	AdvancedVoiceActivity *bool `json:"advancedVoiceActivity,omitempty"`
	// AutoSensitivity is Discord's "Automatically determine input sensitivity"
	// toggle: when true (the default) the gate threshold tracks a noise-floor
	// follower; when false the manual GateSensitivity is used. A *bool so a
	// deliberate OFF round-trips (nil = default true). Use AutoSens() to read the
	// effective value. normalize() seeds nil to true.
	AutoSensitivity *bool `json:"autoSensitivity,omitempty"`
	// GateSensitivity is the VAD/RMS gate threshold in [0,1]; higher = the gate
	// requires a louder voice to open. In "vad" mode with AdvancedVoiceActivity on
	// it biases the speech-probability open threshold; otherwise it is the energy
	// threshold. This is the Discord "Input Sensitivity" manual slider value.
	// normalize() defaults 0 to ~0.15 and clamps to [0,1].
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
	// AudioSubsystem mirrors Discord's "Audio Subsystem" selector: one of
	// "standard" | "legacy" | "experimental". COSMETIC for us — the engine has a
	// single malgo/WASAPI backend, so this is persisted for UI/parity only and has
	// no audio effect. normalize() coerces an empty/invalid value to "standard".
	AudioSubsystem string `json:"audioSubsystem,omitempty"`
}

// Noise-suppression tiers (Discord "Noise Suppression": None / Standard / Krisp,
// mapped to our engine). Stored lowercase so the config stays human-editable and
// forward-compatible. normalize() validates against this set.
const (
	// NSModeNone disables noise suppression (APM NS off).
	NSModeNone = "none"
	// NSModeStandard maps to APM NS Moderate (Discord "Standard").
	NSModeStandard = "standard"
	// NSModeHigh maps to APM NS High/VeryHigh (the breathing-kill default).
	NSModeHigh = "high"
	// NSModeStrong runs the RNNoise denoiser (our Krisp analog); APM NS is disabled
	// so the two are never stacked.
	NSModeStrong = "strong"
)

// validNSMode reports whether m is one of the four recognized NS tiers.
func validNSMode(m string) bool {
	switch m {
	case NSModeNone, NSModeStandard, NSModeHigh, NSModeStrong:
		return true
	}
	return false
}

// Audio subsystems (Discord "Audio Subsystem"). Cosmetic for us; persisted for
// parity only. normalize() validates against this set.
const (
	AudioSubsystemStandard     = "standard"
	AudioSubsystemLegacy       = "legacy"
	AudioSubsystemExperimental = "experimental"
)

// validAudioSubsystem reports whether s is one of the three recognized subsystems.
func validAudioSubsystem(s string) bool {
	switch s {
	case AudioSubsystemStandard, AudioSubsystemLegacy, AudioSubsystemExperimental:
		return true
	}
	return false
}

// defaultNoiseSuppressionTier is the tier seeded for a FRESH config. High is
// chosen deliberately to suppress breathing that survives during open speech;
// this is the core of the Discord breathing-parity fix.
const defaultNoiseSuppressionTier = NSModeHigh

// defaultAttenuationAmount is the ducking depth used when none is configured
// (≈ −6 dB). Mirrors the Discord default attenuation feel.
const defaultAttenuationAmount float32 = 0.5

// BoolPtr returns a pointer to a copy of v. Callers use it to set an EXPLICIT
// AdvancedVoiceActivity / AutoSensitivity value — including an explicit false —
// so the value round-trips through JSON instead of being treated as unset and
// coerced back to the default (true).
func BoolPtr(v bool) *bool { return &v }

// orTrue returns the pointed-to value, or true when p is nil (unset). Centralizes
// the "nil means default-on, explicit false means the user turned it off" rule for
// the AdvancedVoiceActivity and AutoSensitivity toggles.
func orTrue(p *bool) bool {
	if p == nil {
		return true
	}
	return *p
}

// AdvancedVAD reports the effective Advanced-Voice-Activity setting: the explicit
// value when set (including an explicit false), else the default (true). Callers
// (engine seeding, UI) use this so an unset config defaults ON without a nil deref.
func (p AudioProcessing) AdvancedVAD() bool { return orTrue(p.AdvancedVoiceActivity) }

// AutoSens reports the effective Automatic-input-sensitivity setting: the explicit
// value when set (including an explicit false), else the default (true).
func (p AudioProcessing) AutoSens() bool { return orTrue(p.AutoSensitivity) }

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
	// Detect whether this config already had a processing block BEFORE we coerce
	// the mic mode. Every config saved by a prior version wrote a non-empty MicMode
	// ("vad"/"ptt"/...), so a non-empty MicMode here means "upgraded config"; an
	// empty MicMode means a fresh/missing config. This disambiguates the
	// NoiseSuppressionTier default (fresh -> aggressive "high"; upgrade -> derive
	// from the user's prior NoiseSuppression bool so their choice is respected).
	upgraded := p.MicMode != ""

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

	// NoiseSuppressionTier: fresh config -> "high" (breathing-kill default);
	// upgraded config without a tier -> derive from the legacy bool; an invalid
	// stored tier -> "high".
	if p.NoiseSuppressionTier == "" {
		switch {
		case upgraded && p.NoiseSuppression:
			p.NoiseSuppressionTier = NSModeStandard
		case upgraded:
			p.NoiseSuppressionTier = NSModeNone
		default:
			p.NoiseSuppressionTier = defaultNoiseSuppressionTier
		}
	}
	if !validNSMode(p.NoiseSuppressionTier) {
		p.NoiseSuppressionTier = defaultNoiseSuppressionTier
	}

	// AttenuationAmount: 0 -> default, clamp to [0,1].
	if p.AttenuationAmount == 0 {
		p.AttenuationAmount = defaultAttenuationAmount
	}
	if p.AttenuationAmount < 0 {
		p.AttenuationAmount = 0
	}
	if p.AttenuationAmount > 1 {
		p.AttenuationAmount = 1
	}

	// AudioSubsystem: empty/invalid -> "standard" (cosmetic).
	if !validAudioSubsystem(p.AudioSubsystem) {
		p.AudioSubsystem = AudioSubsystemStandard
	}

	// Advanced VAD and Auto-sensitivity default ON; a nil pointer is unset, an
	// explicit false is the user's deliberate OFF and is preserved verbatim.
	if p.AdvancedVoiceActivity == nil {
		p.AdvancedVoiceActivity = BoolPtr(true)
	}
	if p.AutoSensitivity == nil {
		p.AutoSensitivity = BoolPtr(true)
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
