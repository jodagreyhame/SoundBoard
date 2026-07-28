package main

import (
	"strings"
	"testing"
	"testing/fstest"

	"github.com/jodagreyhame/SoundBoard/internal/config"
)

// TestGetStateCarriesVersion pins the fix for a title-bar badge that was
// hard-coded to "v2" in index.html and stayed there through the v1.0.0 release.
// The version now comes from the Go side, so the badge cannot drift from the
// binary again.
func TestGetStateCarriesVersion(t *testing.T) {
	app := newAppWithBackend(fakeBackend(t, fstest.MapFS{"memes/a.wav": {Data: []byte("x")}}, &config.Settings{}))

	if got := app.GetState().Version; got != appVersion {
		t.Fatalf("State.Version = %q, want %q", got, appVersion)
	}
}

// TestGetStateVersionWithoutBackend covers the bare-App path the frontend also
// renders from, so the badge is populated even in the degraded snapshot.
func TestGetStateVersionWithoutBackend(t *testing.T) {
	if got := NewApp().GetState().Version; got != appVersion {
		t.Fatalf("nil-backend State.Version = %q, want %q", got, appVersion)
	}
}

// TestAppVersionLooksLikeARelease guards against a placeholder or a stray "v"
// prefix; the frontend adds the "v" itself when rendering.
func TestAppVersionLooksLikeARelease(t *testing.T) {
	if appVersion == "" {
		t.Fatal("appVersion is empty")
	}
	if strings.HasPrefix(appVersion, "v") {
		t.Errorf("appVersion = %q; store the bare version, the UI adds the 'v'", appVersion)
	}
	if strings.Count(appVersion, ".") != 2 {
		t.Errorf("appVersion = %q, want major.minor.patch", appVersion)
	}
}

// TestClipFolderReportsWithoutBackend pins the degraded shape the front end
// renders: never a nil Warnings slice, and an explanation rather than silence.
func TestClipFolderReportsWithoutBackend(t *testing.T) {
	info := NewApp().ClipFolder()

	if info.Error == "" {
		t.Error("ClipFolder on a bare App reported no error; it must say why it has nothing")
	}
	if info.Warnings == nil {
		t.Error("Warnings is nil; the front end iterates it without a guard")
	}
}

// TestReloadLibraryWithoutBackendFails keeps the bound method honest: it must
// return an error rather than silently appearing to succeed.
func TestReloadLibraryWithoutBackendFails(t *testing.T) {
	if _, err := NewApp().ReloadLibrary(); err == nil {
		t.Fatal("ReloadLibrary succeeded with no backend")
	}
}

// TestClipFolderSnapshotCountsMatchLibrary ensures the path and the counts
// shown beside it come from the same library, so the UI cannot display a folder
// and a clip total that disagree.
func TestClipFolderSnapshotCountsMatchLibrary(t *testing.T) {
	b := fakeBackend(t, fstest.MapFS{
		"memes/a.wav": {Data: []byte("x")},
		"memes/b.wav": {Data: []byte("x")},
		"games/c.wav": {Data: []byte("x")},
	}, &config.Settings{})

	snap := b.clipFolderSnapshot()
	if snap.Categories != 2 || snap.Clips != 3 {
		t.Fatalf("snapshot = %d categories / %d clips, want 2/3", snap.Categories, snap.Clips)
	}
}
