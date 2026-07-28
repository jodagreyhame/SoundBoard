package audio

import (
	"sync"
	"testing"
	"testing/fstest"

	"github.com/jodagreyhame/SoundBoard/internal/catalog"
)

func libFrom(t *testing.T, files map[string]string) *catalog.Library {
	t.Helper()
	m := fstest.MapFS{}
	for name, body := range files {
		m[name] = &fstest.MapFile{Data: []byte(body)}
	}
	lib, err := catalog.New(m)
	if err != nil {
		t.Fatalf("catalog.New: %v", err)
	}
	return lib
}

func TestSetLibrarySwapsWhatTriggersResolveAgainst(t *testing.T) {
	first := libFrom(t, map[string]string{"old/a.wav": "x"})
	second := libFrom(t, map[string]string{"new/b.wav": "x"})

	e := NewEngine(nil, first)
	if e.Library().Get("old/a") == nil {
		t.Fatal("initial library not in use")
	}

	e.SetLibrary(second)

	if e.Library().Get("new/b") == nil {
		t.Error("new library not in use after SetLibrary")
	}
	if e.Library().Get("old/a") != nil {
		t.Error("old library still resolving after SetLibrary")
	}
}

// TestSetLibraryUnderConcurrentTriggers is the guarantee that makes a live clip
// folder change safe: reloading while the user is firing clips (or a hotkey is)
// must not race or panic. Run under -race, which CI does for this package.
func TestSetLibraryUnderConcurrentTriggers(t *testing.T) {
	libs := []*catalog.Library{
		libFrom(t, map[string]string{"a/one.wav": "x"}),
		libFrom(t, map[string]string{"b/two.wav": "x"}),
		libFrom(t, map[string]string{"c/three.wav": "x"}),
	}

	e := NewEngine(nil, libs[0])

	const iterations = 400
	var wg sync.WaitGroup

	// Triggers and stops run on the control path, exactly as the UI and hotkey
	// goroutines do.
	for _, id := range []string{"a/one", "b/two", "c/three", "gone/missing"} {
		wg.Add(1)
		go func(clipID string) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				e.TriggerGain(clipID, 1)
				e.StopClip(clipID)
			}
		}(id)
	}

	// Meanwhile the library is swapped out from under them.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			e.SetLibrary(libs[i%len(libs)])
		}
	}()

	wg.Wait()

	if e.Library() == nil {
		t.Fatal("library is nil after concurrent swaps")
	}
}

// TestEngineWithoutLibraryIsInert pins the bare-engine case: dozens of tests
// construct NewEngine(nil, nil), and triggering against no library must be a
// no-op rather than a nil dereference.
func TestEngineWithoutLibraryIsInert(t *testing.T) {
	e := NewEngine(nil, nil)

	if e.Library() != nil {
		t.Fatal("bare engine reports a library")
	}
	e.TriggerGain("anything", 1) // must not panic
	e.StopClip("anything")       // must not panic
}
