package ui

import (
	"testing"
	"testing/fstest"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/test"

	"soundboard/internal/catalog"
)

// emptyContainer returns a fresh empty VBox for renderSections to append into.
func emptyContainer() *fyne.Container { return container.NewVBox() }

// fakePlayer records every TriggerGain call.
type fakePlayer struct {
	lastID   string
	lastGain float32
	calls    int
}

func (f *fakePlayer) TriggerGain(id string, gain float32) {
	f.lastID, f.lastGain = id, gain
	f.calls++
}

// fakeVol is an in-memory VolumeController.
type fakeVol struct {
	mic, master, monitor float32
	clip                 map[string]float32
}

func newFakeVol() *fakeVol {
	return &fakeVol{mic: 1, master: 1, monitor: 1, clip: map[string]float32{}}
}

func (v *fakeVol) SetMic(g float32)             { v.mic = g }
func (v *fakeVol) SetMaster(g float32)          { v.master = g }
func (v *fakeVol) SetMonitor(g float32)         { v.monitor = g }
func (v *fakeVol) SetClip(id string, g float32) { v.clip[id] = g }
func (v *fakeVol) Mic() float32                 { return v.mic }
func (v *fakeVol) Master() float32              { return v.master }
func (v *fakeVol) Monitor() float32             { return v.monitor }
func (v *fakeVol) Clip(id string) float32 {
	if g, ok := v.clip[id]; ok {
		return g
	}
	return 1
}

// fakeSetup is a controllable SetupController.
type fakeSetup struct {
	ready        bool
	canEngage    bool
	detail       string
	installErr   error
	engageErr    error
	installCalls int
	engageCalls  int
}

func (s *fakeSetup) Status() (bool, string) { return s.ready, s.detail }
func (s *fakeSetup) CanEngage() bool        { return s.canEngage }
func (s *fakeSetup) Install() error         { s.installCalls++; return s.installErr }
func (s *fakeSetup) Engage() error          { s.engageCalls++; return s.engageErr }

// testLibrary builds a two-category library from an in-memory FS. catalog.New
// only indexes paths (no decode), so empty files are fine.
func testLibrary(t *testing.T) *catalog.Library {
	t.Helper()
	fsys := fstest.MapFS{
		"sounds/memes/airhorn.mp3":  {Data: []byte("x")},
		"sounds/memes/wow.wav":      {Data: []byte("x")},
		"sounds/effects/laser.flac": {Data: []byte("x")},
	}
	lib, err := catalog.New(fsys)
	if err != nil {
		t.Fatalf("catalog.New: %v", err)
	}
	return lib
}

// buildTestApp constructs an App and builds its window/tray under a headless
// Fyne test app — never calling Run (which would block on the GUI loop).
func buildTestApp(t *testing.T, setup SetupController) (*App, *fakePlayer, *fakeVol) {
	t.Helper()
	player := &fakePlayer{}
	vol := newFakeVol()
	a := New(testLibrary(t), player, vol, setup)
	a.build(test.NewApp())
	return a, player, vol
}

// fakeWindowStore is an in-memory WindowStore.
type fakeWindowStore struct {
	w, h float32
	ok   bool
	set  int
}

func (s *fakeWindowStore) WindowSize() (float32, float32, bool) { return s.w, s.h, s.ok }
func (s *fakeWindowStore) SetWindowSize(w, h float32)           { s.w, s.h, s.set = w, h, s.set+1 }

// TestWindowStoreRestoresSize verifies a saved window size is applied on build
// (not the hard-coded default). With no store the default size is used.
func TestWindowStoreRestoresSize(t *testing.T) {
	store := &fakeWindowStore{w: 900, h: 700, ok: true}
	a := New(testLibrary(t), &fakePlayer{}, newFakeVol(), &fakeSetup{}).WithWindowStore(store)
	a.build(test.NewApp())

	got := a.win.Canvas().Size()
	if got.Width != 900 || got.Height != 700 {
		t.Fatalf("restored window size = %vx%v, want 900x700", got.Width, got.Height)
	}
}

// TestRecordWindowSizePersists verifies the close/quit path writes the current
// window size back into the store so the next Save persists it.
func TestRecordWindowSizePersists(t *testing.T) {
	store := &fakeWindowStore{}
	a := New(testLibrary(t), &fakePlayer{}, newFakeVol(), &fakeSetup{}).WithWindowStore(store)
	a.build(test.NewApp())

	a.recordWindowSize()
	if store.set == 0 {
		t.Fatal("recordWindowSize did not write to the store")
	}
	if store.w <= 0 || store.h <= 0 {
		t.Fatalf("recorded a non-positive size: %vx%v", store.w, store.h)
	}
}

// TestNilWindowStoreUsesDefault verifies build is safe with no store attached
// and falls back to the default size.
func TestNilWindowStoreUsesDefault(t *testing.T) {
	a := New(testLibrary(t), &fakePlayer{}, newFakeVol(), &fakeSetup{})
	a.build(test.NewApp())
	// recordWindowSize must be a safe no-op without a store.
	a.recordWindowSize()
	got := a.win.Canvas().Size()
	if got.Width != 760 || got.Height != 600 {
		t.Fatalf("default window size = %vx%v, want 760x600", got.Width, got.Height)
	}
}

func TestNewDoesNotBuildFyneObjects(t *testing.T) {
	a := New(testLibrary(t), &fakePlayer{}, newFakeVol(), &fakeSetup{})
	if a.win != nil || a.fyneApp != nil {
		t.Fatal("New must not build Fyne objects before Run/build")
	}
	if a.lib == nil || a.player == nil || a.vol == nil || a.setup == nil {
		t.Fatal("New must retain all dependencies")
	}
}

func TestBuildWiresWindowAndClosures(t *testing.T) {
	a, _, _ := buildTestApp(t, &fakeSetup{ready: true, detail: "routing ready"})
	if a.win == nil {
		t.Fatal("build must create the main window")
	}
	if a.rebuildBrowser == nil {
		t.Fatal("clip browser rebuild closure not wired")
	}
	if a.selectClip == nil {
		t.Fatal("per-clip select closure not wired")
	}
	// ShowWindow must be safe and not panic with a real window.
	a.ShowWindow()
}

func TestPlayTriggersAndSelects(t *testing.T) {
	a, player, vol := buildTestApp(t, &fakeSetup{})
	vol.SetClip("memes/airhorn", 0.5)

	clip := a.lib.Get("memes/airhorn")
	if clip == nil {
		t.Fatal("expected memes/airhorn clip in test library")
	}
	a.play(clip)

	if player.calls != 1 || player.lastID != "memes/airhorn" {
		t.Fatalf("play did not trigger correct clip: %+v", player)
	}
	if player.lastGain != 0.5 {
		t.Fatalf("play should use saved per-clip gain 0.5, got %v", player.lastGain)
	}
	if a.selected != "memes/airhorn" {
		t.Fatalf("play should select the clip, selected=%q", a.selected)
	}
}

func TestSearchFilterRebuilds(t *testing.T) {
	a, _, _ := buildTestApp(t, &fakeSetup{})

	// Sanity: with no filter, both categories' clips match.
	a.search = ""
	if got := a.renderSections(emptyContainer()); got != 3 {
		t.Fatalf("expected 3 clips shown unfiltered, got %d", got)
	}
	// Filter by name.
	a.search = "airhorn"
	if got := a.renderSections(emptyContainer()); got != 1 {
		t.Fatalf("expected 1 clip for 'airhorn', got %d", got)
	}
	// Filter by category.
	a.search = "effects"
	if got := a.renderSections(emptyContainer()); got != 1 {
		t.Fatalf("expected 1 clip for category 'effects', got %d", got)
	}
	// No match.
	a.search = "zzz-nope"
	if got := a.renderSections(emptyContainer()); got != 0 {
		t.Fatalf("expected 0 clips for non-matching filter, got %d", got)
	}
	// rebuildBrowser must not panic.
	a.search = "wow"
	a.rebuildBrowser()
}

// TestOnFixRoutingEngagesWhenCablePresent verifies the Engage path is reachable:
// when the cable is present (CanEngage) but routing is NOT yet ready, the fix
// action engages routing rather than (re)installing. This is the regression for
// the "Engage branch is dead code" finding.
func TestOnFixRoutingEngagesWhenCablePresent(t *testing.T) {
	setup := &fakeSetup{ready: false, canEngage: true, detail: "cable present"}
	a, _, _ := buildTestApp(t, setup)

	a.onFixRouting()
	// onFixRouting runs the op in a goroutine; wait briefly for it to record.
	waitFor(t, func() bool { return setup.engageCalls > 0 || setup.installCalls > 0 })

	if setup.engageCalls != 1 {
		t.Fatalf("expected exactly 1 Engage call, got %d (install=%d)", setup.engageCalls, setup.installCalls)
	}
	if setup.installCalls != 0 {
		t.Fatalf("Engage path must not call Install, got %d install calls", setup.installCalls)
	}
}

// TestOnFixRoutingInstallsWhenCableAbsent verifies the install path: with the
// cable absent (!CanEngage) the fix action installs VB-CABLE, never engages.
func TestOnFixRoutingInstallsWhenCableAbsent(t *testing.T) {
	setup := &fakeSetup{ready: false, canEngage: false, detail: "cable absent"}
	a, _, _ := buildTestApp(t, setup)

	a.onFixRouting()
	waitFor(t, func() bool { return setup.engageCalls > 0 || setup.installCalls > 0 })

	if setup.installCalls != 1 {
		t.Fatalf("expected exactly 1 Install call, got %d (engage=%d)", setup.installCalls, setup.engageCalls)
	}
	if setup.engageCalls != 0 {
		t.Fatalf("Install path must not call Engage, got %d engage calls", setup.engageCalls)
	}
}

// waitFor polls cond up to ~2s so the async onFixRouting goroutine can complete
// without a fixed sleep.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met within timeout")
}

func TestBannerHandlesNilAndStatuses(t *testing.T) {
	// Ready status renders the success banner.
	a, _, _ := buildTestApp(t, &fakeSetup{ready: true, detail: "cable found"})
	if a.buildSetupBanner() == nil {
		t.Fatal("ready banner should render")
	}
	// Not-ready status renders the warning banner + action.
	b, _, _ := buildTestApp(t, &fakeSetup{ready: false, detail: "cable missing"})
	if b.buildSetupBanner() == nil {
		t.Fatal("warning banner should render")
	}
	// A nil setup controller must not crash the banner.
	c := New(testLibrary(t), &fakePlayer{}, newFakeVol(), nil)
	c.build(test.NewApp())
	if c.buildSetupBanner() == nil {
		t.Fatal("banner should render with a nil setup controller")
	}
}
