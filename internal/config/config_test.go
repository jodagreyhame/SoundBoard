package config

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

// pointConfigDir points os.UserConfigDir() at a temporary directory by setting
// the platform-appropriate environment variable for the duration of the test.
func pointConfigDir(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	switch runtime.GOOS {
	case "windows":
		t.Setenv("APPDATA", tmp)
	case "darwin":
		t.Setenv("HOME", tmp)
	default:
		t.Setenv("XDG_CONFIG_HOME", tmp)
	}
	return tmp
}

func TestLoadMissingReturnsDefaults(t *testing.T) {
	pointConfigDir(t)

	s, err := Load()
	if err != nil {
		t.Fatalf("Load() of missing file returned error: %v", err)
	}
	if s == nil {
		t.Fatal("Load() returned nil Settings")
	}
	if s.Hotkeys == nil {
		t.Error("Load() defaults: Hotkeys map should be initialized, got nil")
	}
	if s.MicName != "" || s.CableName != "" || s.MonitorName != "" || s.Monitor {
		t.Errorf("Load() defaults: expected zero-value fields, got %+v", s)
	}
	// Volumes default to unity gain with a non-nil PerClip map so the in-app
	// mixer starts at full volume and is never nil-dereferenced. The levels are
	// pointers (nil = unset) seeded to unity by normalize; the effective-gain
	// accessors report 1.0 here.
	if s.Volumes.MicGain() != 1 {
		t.Errorf("Load() defaults: Volumes.Mic = %v, want 1", s.Volumes.MicGain())
	}
	if s.Volumes.MasterGain() != 1 {
		t.Errorf("Load() defaults: Volumes.Master = %v, want 1", s.Volumes.MasterGain())
	}
	if s.Volumes.MonitorGain() != 1 {
		t.Errorf("Load() defaults: Volumes.Monitor = %v, want 1", s.Volumes.MonitorGain())
	}
	if s.Volumes.PerClip == nil {
		t.Error("Load() defaults: Volumes.PerClip should be initialized, got nil")
	}
	// Favourites default to a non-nil empty slice so the UI never nil-panics.
	if s.Favorites == nil {
		t.Error("Load() defaults: Favorites should be initialized (empty, not nil)")
	}
	if len(s.Favorites) != 0 {
		t.Errorf("Load() defaults: Favorites = %v, want empty", s.Favorites)
	}
	// Mic-processing defaults: gate by voice activity at the default sensitivity,
	// every optional feature off. This is the "clean voice, no carrier" baseline.
	if s.Processing.MicMode != MicModeVAD {
		t.Errorf("Load() defaults: Processing.MicMode = %q, want %q", s.Processing.MicMode, MicModeVAD)
	}
	if s.Processing.GateSensitivity != defaultGateSensitivity {
		t.Errorf("Load() defaults: Processing.GateSensitivity = %v, want %v", s.Processing.GateSensitivity, defaultGateSensitivity)
	}
	if s.Processing.NoiseSuppression || s.Processing.AGC || s.Processing.Ducking || s.Processing.ForceThrough || s.Processing.EchoCancellation {
		t.Errorf("Load() defaults: all plain processing toggles should be off, got %+v", s.Processing)
	}
	if s.Processing.PTTHotkey != "" {
		t.Errorf("Load() defaults: Processing.PTTHotkey = %q, want empty", s.Processing.PTTHotkey)
	}
	// Discord-parity breathing-fix defaults on a FRESH config: NS tier "high",
	// Advanced Voice Activity ON, Auto-sensitivity ON, attenuation depth 0.5, and a
	// "standard" audio subsystem.
	if s.Processing.NoiseSuppressionTier != NSModeHigh {
		t.Errorf("Load() defaults: NoiseSuppressionTier = %q, want %q", s.Processing.NoiseSuppressionTier, NSModeHigh)
	}
	if !s.Processing.AdvancedVAD() {
		t.Error("Load() defaults: AdvancedVoiceActivity should default ON (the breathing fix)")
	}
	if !s.Processing.AutoSens() {
		t.Error("Load() defaults: AutoSensitivity should default ON")
	}
	if s.Processing.AttenuationAmount != defaultAttenuationAmount {
		t.Errorf("Load() defaults: AttenuationAmount = %v, want %v", s.Processing.AttenuationAmount, defaultAttenuationAmount)
	}
	if s.Processing.AudioSubsystem != AudioSubsystemStandard {
		t.Errorf("Load() defaults: AudioSubsystem = %q, want %q", s.Processing.AudioSubsystem, AudioSubsystemStandard)
	}
}

// TestLoadNormalizesProcessing pins that an upgraded config with a partial or
// invalid processing block loads back with a valid MicMode and a clamped, sane
// gate sensitivity, while preserving the toggles the user explicitly set.
func TestLoadNormalizesProcessing(t *testing.T) {
	pointConfigDir(t)

	// An invalid MicMode and an out-of-range sensitivity must be coerced; the AGC
	// toggle the user set must survive.
	stored := &Settings{
		Hotkeys: map[string]string{},
		Processing: AudioProcessing{
			MicMode:         "bogus",
			GateSensitivity: 9.0, // out of range, must clamp to 1
			AGC:             true,
		},
	}
	if err := stored.Save(); err != nil {
		t.Fatalf("Save() error: %v", err)
	}

	got, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if got.Processing.MicMode != MicModeVAD {
		t.Errorf("MicMode = %q, want %q (invalid coerced)", got.Processing.MicMode, MicModeVAD)
	}
	if got.Processing.GateSensitivity != 1 {
		t.Errorf("GateSensitivity = %v, want 1 (clamped)", got.Processing.GateSensitivity)
	}
	if !got.Processing.AGC {
		t.Error("AGC = false, want true (preserved)")
	}
}

// TestProcessingNormalizeEdges pins the remaining normalize() branches the other
// tests don't reach: a NEGATIVE gate sensitivity clamps up to 0 (not to the
// default), and every one of the four valid MicModes is preserved verbatim
// rather than being coerced to "vad". Driven directly through normalize() so the
// table stays cheap and doesn't touch the disk.
func TestProcessingNormalizeEdges(t *testing.T) {
	// A negative sensitivity is out of range low; the contract clamps it to 0
	// (max sensitivity), distinct from the 0->default coercion.
	neg := AudioProcessing{MicMode: MicModeAlways, GateSensitivity: -3}
	neg.normalize()
	if neg.GateSensitivity != 0 {
		t.Errorf("negative GateSensitivity = %v, want 0 (clamped)", neg.GateSensitivity)
	}
	if neg.MicMode != MicModeAlways {
		t.Errorf("MicMode = %q, want %q (valid mode preserved)", neg.MicMode, MicModeAlways)
	}

	// Each valid mode must survive normalize() unchanged; a non-zero sensitivity
	// in range must also be preserved (not re-defaulted).
	for _, mode := range []string{MicModeVAD, MicModePTT, MicModeAlways, MicModeMute} {
		p := AudioProcessing{MicMode: mode, GateSensitivity: 0.5}
		p.normalize()
		if p.MicMode != mode {
			t.Errorf("MicMode %q not preserved, got %q", mode, p.MicMode)
		}
		if p.GateSensitivity != 0.5 {
			t.Errorf("MicMode %q: GateSensitivity = %v, want 0.5 (preserved)", mode, p.GateSensitivity)
		}
	}

	// MonitorSource: an empty or unrecognized value coerces to "clips" (the safe
	// default = legacy behavior); each valid value survives normalize() verbatim.
	for _, in := range []string{"", "bogus", "TRANSMITTED"} {
		p := AudioProcessing{MonitorSource: in}
		p.normalize()
		if p.MonitorSource != MonitorSourceClips {
			t.Errorf("MonitorSource %q: normalize() = %q, want %q (default)", in, p.MonitorSource, MonitorSourceClips)
		}
	}
	for _, src := range []string{MonitorSourceClips, MonitorSourceTransmitted} {
		p := AudioProcessing{MonitorSource: src}
		p.normalize()
		if p.MonitorSource != src {
			t.Errorf("MonitorSource %q not preserved, got %q", src, p.MonitorSource)
		}
	}
}

// TestNoiseSuppressionTierBackCompat pins the upgrade path: a config saved by a
// prior version (it has a MicMode but NO noiseSuppressionTier) must derive its
// tier from the legacy NoiseSuppression bool so the user's prior choice is
// respected — true -> "standard", false -> "none" — rather than being force-reset
// to the aggressive fresh-install "high" default.
func TestNoiseSuppressionTierBackCompat(t *testing.T) {
	cases := []struct {
		name     string
		legacyNS bool
		wantTier string
	}{
		{"legacy NS on -> standard", true, NSModeStandard},
		{"legacy NS off -> none", false, NSModeNone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// MicMode set (no tier) marks this as an upgraded config.
			p := AudioProcessing{MicMode: MicModeVAD, NoiseSuppression: tc.legacyNS}
			p.normalize()
			if p.NoiseSuppressionTier != tc.wantTier {
				t.Errorf("upgraded tier = %q, want %q", p.NoiseSuppressionTier, tc.wantTier)
			}
		})
	}

	// A FRESH config (no MicMode, no tier) defaults to the aggressive breathing-kill
	// tier regardless of the legacy bool's zero value.
	fresh := AudioProcessing{}
	fresh.normalize()
	if fresh.NoiseSuppressionTier != NSModeHigh {
		t.Errorf("fresh tier = %q, want %q", fresh.NoiseSuppressionTier, NSModeHigh)
	}

	// An explicit tier always wins over the legacy bool (tier is authoritative once set).
	explicit := AudioProcessing{MicMode: MicModeVAD, NoiseSuppression: false, NoiseSuppressionTier: NSModeStrong}
	explicit.normalize()
	if explicit.NoiseSuppressionTier != NSModeStrong {
		t.Errorf("explicit tier = %q, want %q (tier authoritative)", explicit.NoiseSuppressionTier, NSModeStrong)
	}

	// An invalid stored tier coerces to the default.
	bad := AudioProcessing{MicMode: MicModeVAD, NoiseSuppressionTier: "bogus"}
	bad.normalize()
	if bad.NoiseSuppressionTier != NSModeHigh {
		t.Errorf("invalid tier coerced to %q, want %q", bad.NoiseSuppressionTier, NSModeHigh)
	}
}

// TestAdvancedToggleExplicitFalseRoundTrips is the regression for "a user who
// turns OFF Advanced Voice Activity / Auto-sensitivity finds it back ON after a
// restart". These default ON (the breathing fix), so a deliberate OFF must persist:
// the *bool stores an explicit false that survives Save/Load, while a genuinely
// unset (nil) value defaults to ON via normalize.
func TestAdvancedToggleExplicitFalseRoundTrips(t *testing.T) {
	pointConfigDir(t)

	stored := &Settings{
		Hotkeys: map[string]string{},
		Processing: AudioProcessing{
			MicMode:               MicModeVAD,
			AdvancedVoiceActivity: BoolPtr(false),
			AutoSensitivity:       BoolPtr(false),
		},
	}
	if err := stored.Save(); err != nil {
		t.Fatalf("Save() error: %v", err)
	}

	got, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if got.Processing.AdvancedVoiceActivity == nil || *got.Processing.AdvancedVoiceActivity {
		t.Errorf("AdvancedVoiceActivity = %v, want explicit false (deliberate OFF preserved)", got.Processing.AdvancedVoiceActivity)
	}
	if got.Processing.AdvancedVAD() {
		t.Error("AdvancedVAD() = true, want false (explicit OFF must NOT coerce back ON)")
	}
	if got.Processing.AutoSensitivity == nil || *got.Processing.AutoSensitivity {
		t.Errorf("AutoSensitivity = %v, want explicit false", got.Processing.AutoSensitivity)
	}
	if got.Processing.AutoSens() {
		t.Error("AutoSens() = true, want false (explicit OFF must NOT coerce back ON)")
	}

	// And a nil (unset) toggle must default ON through the accessor.
	none := AudioProcessing{}
	if !none.AdvancedVAD() || !none.AutoSens() {
		t.Error("unset toggles must report ON via accessors (nil -> default true)")
	}
}

// TestProcessingNormalizeNewEdges pins the remaining new-field normalize branches:
// AttenuationAmount 0->default, negative clamp, over-range clamp, in-range
// preserved; and AudioSubsystem empty/invalid->"standard", valid preserved.
func TestProcessingNormalizeNewEdges(t *testing.T) {
	// AttenuationAmount.
	atten := []struct {
		in, want float32
	}{
		{0, defaultAttenuationAmount}, // unset -> default
		{-2, 0},                       // below range -> 0
		{5, 1},                        // above range -> 1
		{0.3, 0.3},                    // in range -> preserved
	}
	for _, tc := range atten {
		p := AudioProcessing{MicMode: MicModeVAD, AttenuationAmount: tc.in}
		p.normalize()
		if p.AttenuationAmount != tc.want {
			t.Errorf("AttenuationAmount(%v) = %v, want %v", tc.in, p.AttenuationAmount, tc.want)
		}
	}

	// AudioSubsystem: empty/invalid coerce to "standard"; each valid value survives.
	for _, in := range []string{"", "bogus", "STANDARD"} {
		p := AudioProcessing{AudioSubsystem: in}
		p.normalize()
		if p.AudioSubsystem != AudioSubsystemStandard {
			t.Errorf("AudioSubsystem %q -> %q, want %q", in, p.AudioSubsystem, AudioSubsystemStandard)
		}
	}
	for _, sub := range []string{AudioSubsystemStandard, AudioSubsystemLegacy, AudioSubsystemExperimental} {
		p := AudioProcessing{AudioSubsystem: sub}
		p.normalize()
		if p.AudioSubsystem != sub {
			t.Errorf("AudioSubsystem %q not preserved, got %q", sub, p.AudioSubsystem)
		}
	}
}

// TestFavoritesRoundTrip pins that a non-empty, ORDERED favourites list survives
// a Save/Load cycle unchanged. Order matters: the UI pins favourites in this
// sequence, so a reordering bug would silently reshuffle the user's section.
func TestFavoritesRoundTrip(t *testing.T) {
	pointConfigDir(t)

	want := &Settings{
		Hotkeys:   map[string]string{},
		Favorites: []string{"memes/airhorn", "effects/laser", "games/level-up"},
	}
	if err := want.Save(); err != nil {
		t.Fatalf("Save() error: %v", err)
	}

	got, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if !reflect.DeepEqual(want.Favorites, got.Favorites) {
		t.Errorf("favourites round-trip mismatch:\n want %v\n got  %v", want.Favorites, got.Favorites)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	pointConfigDir(t)

	want := &Settings{
		MicName:     "Microphone (Realtek)",
		CableName:   "CABLE Input (VB-Audio Virtual Cable)",
		MonitorName: "Speakers (Realtek)",
		Monitor:     true,
		// Clip IDs are canonically extensionless ("<category>/<basename>");
		// see catalog.Library.Get, which also accepts an extension suffix.
		Hotkeys: map[string]string{
			"ctrl+alt+1": "memes/airhorn",
			"ctrl+alt+2": "games/level-up",
		},
		Volumes: Volumes{
			Mic:     FloatPtr(0.8),
			Master:  FloatPtr(0.5),
			Monitor: FloatPtr(0.6),
			PerClip: map[string]float32{
				"memes/airhorn": 1.25,
			},
		},
		// Favourites are an ordered list of clip IDs; order must round-trip.
		Favorites: []string{"memes/airhorn", "games/level-up"},
		Window:    WindowPrefs{Width: 800, Height: 600},
		// The mic-processing block must round-trip every field, including the
		// non-default MicMode, a custom gate sensitivity, and the non-default
		// MonitorSource (the confidence-monitor setting).
		Processing: AudioProcessing{
			NoiseSuppression:      true,
			NoiseSuppressionTier:  NSModeStrong,
			EchoCancellation:      true,
			AGC:                   true,
			Ducking:               true,
			AttenuationAmount:     0.75,
			MicMode:               MicModePTT,
			AdvancedVoiceActivity: BoolPtr(false), // explicit OFF must round-trip
			AutoSensitivity:       BoolPtr(false), // explicit OFF must round-trip
			GateSensitivity:       0.42,
			ForceThrough:          true,
			PTTHotkey:             "ctrl+grave",
			MonitorSource:         MonitorSourceTransmitted,
			AudioSubsystem:        AudioSubsystemExperimental,
		},
	}

	if err := want.Save(); err != nil {
		t.Fatalf("Save() error: %v", err)
	}

	got, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if !reflect.DeepEqual(want, got) {
		t.Errorf("round-trip mismatch:\n want %+v\n got  %+v", want, got)
	}
}

func TestLoadNormalizesPartialVolumes(t *testing.T) {
	pointConfigDir(t)

	// A saved config that set only Master (e.g. an older pre-Monitor write path)
	// must load back with Mic AND Monitor defaulted to unity, a non-nil PerClip
	// map, and the stored Master preserved. The Monitor default matters most: an
	// upgraded config without it must still let the user HEAR their clips.
	stored := &Settings{
		Hotkeys: map[string]string{},
		Volumes: Volumes{Master: FloatPtr(0.3)},
	}
	if err := stored.Save(); err != nil {
		t.Fatalf("Save() error: %v", err)
	}

	got, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if got.Volumes.MicGain() != 1 {
		t.Errorf("Volumes.Mic = %v, want 1 (defaulted)", got.Volumes.MicGain())
	}
	if got.Volumes.MonitorGain() != 1 {
		t.Errorf("Volumes.Monitor = %v, want 1 (defaulted for pre-Monitor config)", got.Volumes.MonitorGain())
	}
	if got.Volumes.MasterGain() != 0.3 {
		t.Errorf("Volumes.Master = %v, want 0.3 (preserved)", got.Volumes.MasterGain())
	}
	if got.Volumes.PerClip == nil {
		t.Error("Volumes.PerClip is nil, want initialized empty map")
	}
}

// TestExplicitMuteRoundTrips is the regression for the "deliberate mute silently
// un-mutes on next launch" bug. A user who drags Master (what Discord hears) to 0%
// — or Mic to 0% — must find it STILL muted after a restart, not auto-reset to
// 100%. An explicit 0 is a non-nil pointer, so it persists as `0` and reloads as a
// real mute; only a genuinely UNSET (nil) channel defaults to unity.
func TestExplicitMuteRoundTrips(t *testing.T) {
	pointConfigDir(t)

	stored := &Settings{
		Hotkeys: map[string]string{},
		Volumes: Volumes{
			Master: FloatPtr(0), // deliberately mute what Discord hears
			Mic:    FloatPtr(0), // deliberately mute the user's own voice
			// Monitor left nil (unset) -> must default to unity.
		},
	}
	if err := stored.Save(); err != nil {
		t.Fatalf("Save() error: %v", err)
	}

	got, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if got.Volumes.Master == nil || *got.Volumes.Master != 0 {
		t.Errorf("Master mute did not round-trip: got %v, want explicit 0", got.Volumes.Master)
	}
	if got.Volumes.MasterGain() != 0 {
		t.Errorf("MasterGain() = %v, want 0 (mute preserved, NOT coerced to unity)", got.Volumes.MasterGain())
	}
	if got.Volumes.Mic == nil || *got.Volumes.Mic != 0 {
		t.Errorf("Mic mute did not round-trip: got %v, want explicit 0", got.Volumes.Mic)
	}
	if got.Volumes.MicGain() != 0 {
		t.Errorf("MicGain() = %v, want 0 (mute preserved)", got.Volumes.MicGain())
	}
	// The unset Monitor must still default to unity so the user hears clips locally.
	if got.Volumes.MonitorGain() != 1 {
		t.Errorf("MonitorGain() = %v, want 1 (unset channel defaults to unity)", got.Volumes.MonitorGain())
	}
}

func TestSaveOverwriteAtomic(t *testing.T) {
	pointConfigDir(t)

	first := &Settings{MicName: "mic-a", Hotkeys: map[string]string{"ctrl+1": "a"}}
	if err := first.Save(); err != nil {
		t.Fatalf("first Save() error: %v", err)
	}

	second := &Settings{MicName: "mic-b", Hotkeys: map[string]string{"ctrl+2": "b"}}
	if err := second.Save(); err != nil {
		t.Fatalf("second Save() error: %v", err)
	}

	got, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	// Load normalizes Volumes to unity defaults; mirror that on the expected
	// value so the structural comparison reflects what a fresh Load produces.
	second.normalize()
	if !reflect.DeepEqual(second, got) {
		t.Errorf("overwrite mismatch:\n want %+v\n got  %+v", second, got)
	}

	// Ensure no stray temp files were left behind in the config dir.
	d, err := dir()
	if err != nil {
		t.Fatalf("dir() error: %v", err)
	}
	entries, err := os.ReadDir(d)
	if err != nil {
		t.Fatalf("ReadDir error: %v", err)
	}
	for _, e := range entries {
		if e.Name() != configFile {
			t.Errorf("unexpected leftover file in config dir: %s", e.Name())
		}
	}
}

// TestClipFolderAbsentFromLegacyConfig pins backward compatibility for configs
// written before the clip folder became a setting. An upgrading user's file has
// neither key; both must default to "use the default folder" and "the user has
// not been told yet", and re-saving must not inject the keys into a file that
// never had them.
func TestClipFolderAbsentFromLegacyConfig(t *testing.T) {
	tmp := pointConfigDir(t)

	cfgDir := filepath.Join(tmp, "soundboard")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	legacy := `{"micName":"Mic","cableName":"CABLE Input","monitor":true,"theme":"light"}`
	if err := os.WriteFile(filepath.Join(cfgDir, "config.json"), []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if s.ClipFolder != "" {
		t.Errorf("ClipFolder = %q, want empty so the default is resolved at runtime", s.ClipFolder)
	}
	if s.ClipFolderNoticeSeen {
		t.Error("ClipFolderNoticeSeen = true for a legacy config; an upgrading user has not been told where clips moved to")
	}

	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	out, err := os.ReadFile(filepath.Join(cfgDir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "clipFolder") {
		t.Errorf("re-saving a legacy config injected clipFolder keys: %s", out)
	}
}
