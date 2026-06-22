package setup

import (
	"archive/zip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"soundboard/internal/devices"
)

func dev(name string) devices.Device { return devices.Device{Name: name} }

// TestDetect covers the CanEngage matrix: both endpoints present, only one, or
// neither.
func TestDetect(t *testing.T) {
	cableIn := dev("CABLE Input (VB-Audio Virtual Cable)")
	cableOut := dev("CABLE Output (VB-Audio Virtual Cable)")
	mic := dev("Microphone (Realtek Audio)")
	speakers := dev("Speakers (Realtek Audio)")

	tests := []struct {
		name          string
		playback      []devices.Device
		capture       []devices.Device
		wantIn        bool
		wantOut       bool
		wantCanEngage bool
	}{
		{
			name:          "both present",
			playback:      []devices.Device{speakers, cableIn},
			capture:       []devices.Device{mic, cableOut},
			wantIn:        true,
			wantOut:       true,
			wantCanEngage: true,
		},
		{
			name:          "input only",
			playback:      []devices.Device{speakers, cableIn},
			capture:       []devices.Device{mic},
			wantIn:        true,
			wantOut:       false,
			wantCanEngage: false,
		},
		{
			name:          "output only",
			playback:      []devices.Device{speakers},
			capture:       []devices.Device{mic, cableOut},
			wantIn:        false,
			wantOut:       true,
			wantCanEngage: false,
		},
		{
			name:          "neither",
			playback:      []devices.Device{speakers},
			capture:       []devices.Device{mic},
			wantIn:        false,
			wantOut:       false,
			wantCanEngage: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Detect(tc.playback, tc.capture)
			if got.CableInputPresent != tc.wantIn {
				t.Errorf("CableInputPresent = %v, want %v", got.CableInputPresent, tc.wantIn)
			}
			if got.CableOutputPresent != tc.wantOut {
				t.Errorf("CableOutputPresent = %v, want %v", got.CableOutputPresent, tc.wantOut)
			}
			if got.CanEngage != tc.wantCanEngage {
				t.Errorf("CanEngage = %v, want %v", got.CanEngage, tc.wantCanEngage)
			}
		})
	}
}

// TestDetectCachesCaptureList verifies Detect records the capture list so
// resolvePrevMic can later match against it (no COM, no system change).
func TestDetectCachesCaptureList(t *testing.T) {
	mic := dev("Microphone (USB)")
	Detect(nil, []devices.Device{mic})

	resolvePrevMic("Microphone (USB)", nil) // force a miss with empty list
	if _, ok := PreviousDefaultMic(); ok {
		t.Fatal("expected no resolution against an empty list")
	}

	// Resolve against the cached list captured by Detect.
	state.mu.Lock()
	cached := state.captureList
	state.mu.Unlock()
	resolvePrevMic("Microphone (USB)", cached)
	got, ok := PreviousDefaultMic()
	if !ok || got.Name != "Microphone (USB)" {
		t.Fatalf("resolve from cached list failed: %q ok=%v", got.Name, ok)
	}
}

// TestResolvePrevMicEmptyName ensures an empty friendly name resolves to no
// device (ok=false) rather than the first device in the list.
func TestResolvePrevMicEmptyName(t *testing.T) {
	resolvePrevMic("", []devices.Device{dev("Anything")})
	if _, ok := PreviousDefaultMic(); ok {
		t.Fatal("empty name must not resolve to a device")
	}
}

// TestDownloadURL confirms the public URL is the official VB-Audio direct link.
func TestDownloadURL(t *testing.T) {
	got := DownloadURL()
	if !strings.HasPrefix(got, "https://download.vb-audio.com/Download_CABLE/VBCABLE_Driver_Pack") {
		t.Fatalf("unexpected download URL: %q", got)
	}
	if !strings.HasSuffix(got, ".zip") {
		t.Fatalf("download URL should be a zip: %q", got)
	}
}

// TestDownloadURLsAllOfficial guards against a mirror sneaking into the list:
// every candidate must be on VB-Audio's own host over HTTPS.
func TestDownloadURLsAllOfficial(t *testing.T) {
	if len(cableDownloadURLs) == 0 {
		t.Fatal("no download URLs configured")
	}
	for _, u := range cableDownloadURLs {
		if !strings.HasPrefix(u, "https://download.vb-audio.com/") {
			t.Errorf("non-official URL: %q", u)
		}
	}
}

// TestExtractSetupFindsExe builds an in-memory driver-pack-like zip and confirms
// extractSetup returns the path to VBCABLE_Setup_x64.exe with its bytes intact.
func TestExtractSetupFindsExe(t *testing.T) {
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "pack.zip")
	want := []byte("MZ fake installer payload")
	writeZip(t, zipPath, map[string][]byte{
		"readme.txt":            []byte("hello"),
		"VBCABLE_Setup_x64.exe": want,
		"VBCABLE_Setup.exe":     []byte("32-bit, ignored"),
	})

	dest := filepath.Join(dir, "out")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := extractSetup(zipPath, dest)
	if err != nil {
		t.Fatalf("extractSetup: %v", err)
	}
	if filepath.Base(got) != setupExeName {
		t.Fatalf("returned %q, want basename %q", got, setupExeName)
	}
	data, err := os.ReadFile(got)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != string(want) {
		t.Fatalf("extracted bytes mismatch")
	}
}

// TestExtractSetupMissingExe reports a clear error when the x64 setup is absent.
func TestExtractSetupMissingExe(t *testing.T) {
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "pack.zip")
	writeZip(t, zipPath, map[string][]byte{"readme.txt": []byte("no exe here")})

	if _, err := extractSetup(zipPath, filepath.Join(dir, "out")); err == nil {
		t.Fatal("expected error when setup exe is missing")
	}
}

// TestExtractSetupRejectsZipSlip ensures a malicious "../" entry is not written
// outside the destination directory.
func TestExtractSetupRejectsZipSlip(t *testing.T) {
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "evil.zip")
	writeZip(t, zipPath, map[string][]byte{
		"../escape.exe":         []byte("evil"),
		"VBCABLE_Setup_x64.exe": []byte("ok"),
	})

	dest := filepath.Join(dir, "out")
	if _, err := extractSetup(zipPath, dest); err != nil {
		t.Fatalf("extractSetup should skip the slip entry, not fail: %v", err)
	}
	// The traversal target must not exist outside dest.
	if _, err := os.Stat(filepath.Join(dir, "escape.exe")); err == nil {
		t.Fatal("zip-slip entry escaped the destination directory")
	}
}

// writeZip creates a zip at path with the given entries.
func writeZip(t *testing.T, path string, entries map[string][]byte) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	for name, data := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
}
