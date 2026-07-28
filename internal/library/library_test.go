package library

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withDocuments swaps the platform Documents lookup for a fixed path so the
// path logic is testable without a real Windows known-folder environment.
func withDocuments(t *testing.T, dir string) {
	t.Helper()
	prev := documentsDir
	documentsDir = func() (string, error) { return dir, nil }
	t.Cleanup(func() { documentsDir = prev })
}

func TestDefaultDirIsDocumentsSoundBoard(t *testing.T) {
	docs := t.TempDir()
	withDocuments(t, docs)

	got, err := DefaultDir()
	if err != nil {
		t.Fatalf("DefaultDir: %v", err)
	}
	if want := filepath.Join(docs, AppFolderName); got != want {
		t.Fatalf("DefaultDir = %q, want %q", got, want)
	}
}

// TestResolveEmptyUsesDefault pins the first-run path: no stored preference
// means the default, flagged as such so Ensure is allowed to create it.
func TestResolveEmptyUsesDefault(t *testing.T) {
	docs := t.TempDir()
	withDocuments(t, docs)

	path, isDefault, err := Resolve("")
	if err != nil {
		t.Fatalf("Resolve(\"\"): %v", err)
	}
	if !isDefault {
		t.Error("isDefault = false, want true for an empty configuration")
	}
	if want := filepath.Join(docs, AppFolderName); path != want {
		t.Fatalf("path = %q, want %q", path, want)
	}
}

func TestResolveHonoursValidConfiguredPath(t *testing.T) {
	withDocuments(t, t.TempDir())
	chosen := t.TempDir()

	path, isDefault, err := Resolve(chosen)
	if err != nil {
		t.Fatalf("Resolve(%q): %v", chosen, err)
	}
	if isDefault {
		t.Error("isDefault = true, want false for an explicit path")
	}
	if path != filepath.Clean(chosen) {
		t.Fatalf("path = %q, want %q", path, chosen)
	}
}

// TestResolveFallsBackLoudly is the core anti-silent-fallback guarantee: an
// unusable configured folder (unplugged drive, deleted directory) must still
// yield a working default AND a non-nil error the caller is obliged to surface.
func TestResolveFallsBackLoudly(t *testing.T) {
	docs := t.TempDir()
	withDocuments(t, docs)
	missing := filepath.Join(t.TempDir(), "gone")

	path, isDefault, err := Resolve(missing)
	if err == nil {
		t.Fatal("err = nil; a rejected configured path must report why, not fall back silently")
	}
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("error %q does not name the rejected path %q", err, missing)
	}
	if !isDefault {
		t.Error("isDefault = false, want true after falling back")
	}
	if want := filepath.Join(docs, AppFolderName); path != want {
		t.Fatalf("path = %q, want the default %q so the app still works", path, want)
	}
}

// TestEnsureCreatesAndSeedsDefault covers first run: the folder appears, and it
// contains an example category, because the catalog ignores files that are not
// inside a category directory.
func TestEnsureCreatesAndSeedsDefault(t *testing.T) {
	target := filepath.Join(t.TempDir(), AppFolderName)

	if err := Ensure(target, true); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if st, err := os.Stat(target); err != nil || !st.IsDir() {
		t.Fatalf("clip folder not created: err=%v", err)
	}

	readme := filepath.Join(target, exampleCategory, "README.txt")
	body, err := os.ReadFile(readme)
	if err != nil {
		t.Fatalf("example README not seeded: %v", err)
	}
	if !strings.Contains(string(body), "category") {
		t.Error("seeded README does not explain the category layout")
	}
}

func TestEnsureIsIdempotent(t *testing.T) {
	target := filepath.Join(t.TempDir(), AppFolderName)
	if err := Ensure(target, true); err != nil {
		t.Fatalf("first Ensure: %v", err)
	}
	if err := Ensure(target, true); err != nil {
		t.Fatalf("second Ensure: %v", err)
	}
}

// TestEnsureRefusesToCreateChosenPath pins the deliberate asymmetry: recreating
// a user's missing folder would hide an unplugged drive and index nothing.
func TestEnsureRefusesToCreateChosenPath(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "not-there")

	err := Ensure(missing, false)
	if err == nil {
		t.Fatal("Ensure created a user-chosen path that did not exist; it must report the absence instead")
	}
	if _, statErr := os.Stat(missing); !os.IsNotExist(statErr) {
		t.Error("user-chosen path was created despite the error")
	}
}

func TestEnsureRejectsFile(t *testing.T) {
	f := filepath.Join(t.TempDir(), "clips")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Ensure(f, true); err == nil {
		t.Fatal("Ensure accepted a regular file as the clip folder")
	}
}

func TestValidateRejects(t *testing.T) {
	dir := t.TempDir()

	file := filepath.Join(dir, "a-file")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		path string
	}{
		{"empty", ""},
		{"whitespace", "   "},
		{"relative", filepath.Join("some", "relative")},
		{"missing", filepath.Join(dir, "absent")},
		{"regular file", file},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Validate(tc.path); err == nil {
				t.Fatalf("Validate(%q) = nil, want an error", tc.path)
			}
		})
	}
}

func TestValidateAcceptsOrdinaryDirectory(t *testing.T) {
	withDocuments(t, t.TempDir())

	warnings, err := Validate(t.TempDir())
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("unexpected warnings for an ordinary directory: %v", warnings)
	}
}

// TestValidateRejectsExecutableDirectory guards the regression this whole
// package exists to prevent: a library beside the .exe dies when the app moves
// and cannot be written under Program Files.
func TestValidateRejectsExecutableDirectory(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Skipf("os.Executable unavailable: %v", err)
	}
	if _, err := Validate(filepath.Dir(exe)); err == nil {
		t.Fatal("Validate accepted the executable's own directory")
	}
}

func TestValidateRejectsConfigDirectory(t *testing.T) {
	cfg, err := os.UserConfigDir()
	if err != nil {
		t.Skipf("os.UserConfigDir unavailable: %v", err)
	}
	target := filepath.Join(cfg, configAppDir)
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Skipf("cannot create config dir for test: %v", err)
	}
	if _, err := Validate(target); err == nil {
		t.Fatal("Validate accepted SoundBoard's own config directory")
	}
}

// TestValidateWarnsOnBroadLocation checks the allow-but-warn half of the
// matrix: picking Documents itself works, but the user probably meant a
// dedicated folder.
func TestValidateWarnsOnBroadLocation(t *testing.T) {
	docs := t.TempDir()
	withDocuments(t, docs)

	warnings, err := Validate(docs)
	if err != nil {
		t.Fatalf("Validate(%q) rejected a broad location; it should warn, not reject: %v", docs, err)
	}
	if len(warnings) == 0 {
		t.Error("no warning for selecting the Documents folder itself")
	}
}
