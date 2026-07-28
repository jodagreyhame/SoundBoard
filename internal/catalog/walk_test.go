package catalog

import (
	"bytes"
	"context"
	"io/fs"
	"log"
	"os"
	"strings"
	"testing"
	"testing/fstest"
)

// hostileFS wraps a MapFS and fails one directory, standing in for the things a
// user-chosen folder does that an app-owned one never did: OneDrive cloud-only
// placeholders, an ACL'd subfolder, an antivirus lock, a directory deleted
// mid-scan.
type hostileFS struct {
	fstest.MapFS
	failDir string
}

func (h hostileFS) ReadDir(name string) ([]fs.DirEntry, error) {
	if name == h.failDir {
		return nil, fs.ErrPermission
	}
	return h.MapFS.ReadDir(name)
}

func (h hostileFS) Open(name string) (fs.File, error) {
	if name == h.failDir {
		return nil, fs.ErrPermission
	}
	return h.MapFS.Open(name)
}

// captureLog redirects the standard logger for the duration of a test so the
// diagnostics the walk is supposed to emit can be asserted rather than assumed.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	})
	return &buf
}

// TestNewSurvivesUnreadableSubdirectory is the regression that matters most for
// pointing the app at a user folder: before, ANY per-entry error aborted the
// whole walk and returned zero clips, so one locked subfolder erased the entire
// library.
func TestNewSurvivesUnreadableSubdirectory(t *testing.T) {
	logs := captureLog(t)

	fsys := hostileFS{
		MapFS: fstest.MapFS{
			"good/keeper.wav":   {Data: []byte("x")},
			"locked/hidden.wav": {Data: []byte("x")},
		},
		failDir: "locked",
	}

	lib, err := New(fsys)
	if err != nil {
		t.Fatalf("New returned an error for one unreadable subdirectory; it should skip and continue: %v", err)
	}
	if lib.Get("good/keeper") == nil {
		t.Error("healthy sibling category was lost because another directory failed")
	}
	if !strings.Contains(logs.String(), "locked") {
		t.Errorf("unreadable directory was skipped silently; logs = %q", logs.String())
	}
}

// TestNewFailsOnUnreadableRoot is the other half: skipping entries is right,
// but a clip folder that cannot be opened at all is a real error the caller
// must surface, not an empty grid.
func TestNewFailsOnUnreadableRoot(t *testing.T) {
	fsys := hostileFS{MapFS: fstest.MapFS{"a/b.wav": {Data: []byte("x")}}, failDir: "."}

	if _, err := New(fsys); err == nil {
		t.Fatal("New succeeded on a clip folder whose root could not be read")
	}
}

// TestNewPrunesBelowCategoryDepth guards against walking an arbitrary user
// folder to its full depth. Only <category>/<file> is indexable, so anything
// deeper must not even be descended.
func TestNewPrunesBelowCategoryDepth(t *testing.T) {
	lib, err := New(fstest.MapFS{
		"memes/airhorn.wav":             {Data: []byte("x")},
		"memes/nested/deeper.wav":       {Data: []byte("x")},
		"memes/nested/more/deepest.wav": {Data: []byte("x")},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if lib.Get("memes/airhorn") == nil {
		t.Error("category-level clip missing")
	}
	if got := len(lib.Categories); got != 1 {
		t.Errorf("categories = %d, want 1; nested directories must not become categories", got)
	}
	if n := len(lib.Categories[0].Clips); n != 1 {
		t.Errorf("clips in memes = %d, want 1; nested clips must not be indexed", n)
	}
}

func TestNewSkipsDotDirectories(t *testing.T) {
	lib, err := New(fstest.MapFS{
		"memes/ok.wav":     {Data: []byte("x")},
		".hidden/skip.wav": {Data: []byte("x")},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if len(lib.Categories) != 1 || lib.Categories[0].Name != "memes" {
		t.Fatalf("dot-directory became a category: %+v", lib.Categories)
	}
}

// TestNewReportsIDCollision pins the extension-stripping collision. Two files
// that differ only by extension produce one ID: the grid shows both tiles while
// only one can ever play. That is confusing enough to deserve a log line.
func TestNewReportsIDCollision(t *testing.T) {
	logs := captureLog(t)

	lib, err := New(fstest.MapFS{
		"memes/airhorn.wav": {Data: []byte("x")},
		"memes/airhorn.mp3": {Data: []byte("x")},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if lib.Get("memes/airhorn") == nil {
		t.Fatal("colliding id resolves to nothing")
	}
	if !strings.Contains(logs.String(), "share the id") {
		t.Errorf("id collision was not reported; logs = %q", logs.String())
	}
}

// TestNewReportsLooseRootFiles covers the likeliest first-run mistake: dropping
// clips straight into the clip folder instead of into a category.
func TestNewReportsLooseRootFiles(t *testing.T) {
	logs := captureLog(t)

	lib, err := New(fstest.MapFS{"airhorn.wav": {Data: []byte("x")}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if len(lib.Categories) != 0 {
		t.Fatalf("loose root file was indexed: %+v", lib.Categories)
	}
	if !strings.Contains(logs.String(), "category folders") {
		t.Errorf("loose root files were ignored silently; logs = %q", logs.String())
	}
}

func TestEmptyIsUsable(t *testing.T) {
	lib := Empty()
	if lib == nil {
		t.Fatal("Empty returned nil")
	}
	if len(lib.Categories) != 0 {
		t.Errorf("Empty has %d categories", len(lib.Categories))
	}
	if lib.Get("anything") != nil {
		t.Error("Empty resolved a clip id")
	}
}

func TestNewContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := NewContext(ctx, fstest.MapFS{"memes/a.wav": {Data: []byte("x")}}); err == nil {
		t.Fatal("NewContext ignored a cancelled context")
	}
}

// TestNewRealDirFSRootIsClipFolder pins the contract callers depend on: the FS
// root IS the clip folder, so categories are its immediate subdirectories.
func TestNewRealDirFSRootIsClipFolder(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(root+"/beeps", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(root+"/beeps/tone.wav", []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	lib, err := New(os.DirFS(root))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if lib.Get("beeps/tone") == nil {
		t.Fatalf("clip not indexed from the clip-folder root: %+v", lib.Categories)
	}
}

// TestNewReportsUnsupportedFormats covers the case that otherwise looks like a
// bug in the app: a category full of .m4a files indexes to nothing, with no
// hint that the format is the problem.
func TestNewReportsUnsupportedFormats(t *testing.T) {
	logs := captureLog(t)

	lib, err := New(fstest.MapFS{
		"voices/greeting.m4a": {Data: []byte("x")},
		"voices/notes.txt":    {Data: []byte("x")},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if len(lib.Categories) != 0 {
		t.Fatalf("unsupported file was indexed: %+v", lib.Categories)
	}
	out := logs.String()
	if !strings.Contains(out, "unsupported format") {
		t.Errorf("unsupported audio was skipped silently; logs = %q", out)
	}
	if strings.Contains(out, "notes.txt") {
		t.Errorf("a non-audio file was reported as unsupported audio; logs = %q", out)
	}
}

// TestClipOrderIsDeterministicUnderIDCollision pins stable ordering. IDs are not
// unique once the extension is stripped, so an unstable sort keyed on ID alone
// shuffles tiles between runs.
func TestClipOrderIsDeterministicUnderIDCollision(t *testing.T) {
	build := func() []string {
		lib, err := New(fstest.MapFS{
			"memes/airhorn.wav": {Data: []byte("x")},
			"memes/airhorn.mp3": {Data: []byte("x")},
			"memes/zzz.wav":     {Data: []byte("x")},
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		var paths []string
		for _, c := range lib.Categories[0].Clips {
			paths = append(paths, c.Path)
		}
		return paths
	}

	first := build()
	for i := 0; i < 8; i++ {
		if got := build(); !slicesEqual(got, first) {
			t.Fatalf("clip order varies between runs: %v vs %v", got, first)
		}
	}
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
