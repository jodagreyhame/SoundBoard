package audio

import (
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jodagreyhame/SoundBoard/internal/catalog"
)

// bufBytes returns a zeroed byte buffer sized for one period of interleaved
// stereo float32 — what miniaudio hands a callback.
func bufBytes() []byte { return make([]byte, periodFrames*channels*4) }

// pumpDuplex runs the duplex callback n times with fresh buffers, exactly as the
// device would. No hardware is involved: duplexCallback degrades to plain
// passthrough when the DSP rings are not wired (Configure never called).
func pumpDuplex(e *Engine, n int) {
	for i := 0; i < n; i++ {
		e.duplexCallback(bufBytes(), bufBytes(), periodFrames)
	}
}

// newTwoClipEngine builds an engine over a two-clip in-memory library (clip "a"
// and clip "b" under category "test"), both with synthetic PCM already filled in
// so no decoder or hardware is involved. Clip a is longer than b so a test can
// let one finish while the other keeps playing.
func newTwoClipEngine(t *testing.T) (e *Engine, idA, idB string) {
	t.Helper()
	lib, err := catalog.New(fstest.MapFS{
		"sounds/test/a.wav": {Data: []byte("not decoded in this test")},
		"sounds/test/b.wav": {Data: []byte("not decoded in this test")},
	})
	if err != nil {
		t.Fatalf("catalog.New: %v", err)
	}
	idA, idB = "test/a", "test/b"
	for _, id := range []string{idA, idB} {
		clip := lib.Get(id)
		if clip == nil {
			t.Fatalf("clip %q not indexed", id)
		}
		clip.PCM = flat(0.3, fadeFrames*4)
	}
	return NewEngine(nil, lib), idA, idB
}

// TestPlayingSetClearsWhenClipEnds is the regression test for the bug this file
// exists to fix: the NOW PLAYING chips never went away because nothing told the
// UI a clip had finished. It drives the real RT callback with a short clip and
// asserts the published playing set fills and then EMPTIES ON ITS OWN once the
// cursor is done — no stop, no timer.
func TestPlayingSetClearsWhenClipEnds(t *testing.T) {
	e, id := newTestEngine(t)

	// Nothing triggered: the set is empty.
	if got, ok := e.PlayingClips(); !ok || len(got) != 0 {
		t.Fatalf("before trigger: got %v ok=%v, want empty", got, ok)
	}

	e.TriggerGain(id, 1)

	// One buffer is enough for the callback to drain the pending clip into a
	// cursor and publish it.
	pumpDuplex(e, 1)
	got, ok := e.PlayingClips()
	if !ok {
		t.Fatal("snapshot after trigger was inconsistent")
	}
	if len(got) != 1 || got[0] != id {
		t.Fatalf("while playing: got %v, want [%s]", got, id)
	}

	// The clip is fadeFrames*4 frames long; run well past it. The cursor retires
	// inside mixInto and the very next publish must report an empty set.
	clipFrames := len(e.lib.Get(id).PCM) / channels
	pumpDuplex(e, clipFrames/periodFrames+2)

	got, ok = e.PlayingClips()
	if !ok {
		t.Fatal("snapshot after clip end was inconsistent")
	}
	if len(got) != 0 {
		t.Fatalf("after the clip ended the playing set should be empty, got %v", got)
	}
}

// TestPlayingSetDedupesAndOrders checks the shape of the published set the UI
// reconciles against: one entry per clip even when an overlapping instance is
// triggered, and trigger order preserved across clips.
func TestPlayingSetDedupesAndOrders(t *testing.T) {
	e, idA, idB := newTwoClipEngine(t)

	e.TriggerGain(idA, 1)
	e.TriggerGain(idA, 1) // overlapping second instance of the SAME clip
	e.TriggerGain(idB, 1)
	pumpDuplex(e, 1)

	got, ok := e.PlayingClips()
	if !ok {
		t.Fatal("inconsistent snapshot")
	}
	if len(got) != 2 || got[0] != idA || got[1] != idB {
		t.Fatalf("got %v, want [%s %s] (deduped, trigger order)", got, idA, idB)
	}
}

// TestStopAllClearsPlayingSet: the Stop button must empty the published set
// within one buffer, so the chips and the "Stop · N" counter clear together.
func TestStopAllClearsPlayingSet(t *testing.T) {
	e, id := newTestEngine(t)
	e.TriggerGain(id, 1)
	pumpDuplex(e, 1)
	if got, _ := e.PlayingClips(); len(got) != 1 {
		t.Fatalf("setup: expected 1 playing, got %v", got)
	}

	e.StopAll()
	pumpDuplex(e, 1)
	if got, _ := e.PlayingClips(); len(got) != 0 {
		t.Fatalf("after StopAll: got %v, want empty", got)
	}
}

// TestStopClipRemovesOnlyThatClip backs the per-chip ✕: stopping one clip must
// drop every cursor for it and leave the others playing.
func TestStopClipRemovesOnlyThatClip(t *testing.T) {
	e, idA, idB := newTwoClipEngine(t)

	e.TriggerGain(idA, 1)
	e.TriggerGain(idA, 1) // two instances of A
	e.TriggerGain(idB, 1)
	pumpDuplex(e, 1)

	e.StopClip(idA)
	pumpDuplex(e, 1)

	got, ok := e.PlayingClips()
	if !ok {
		t.Fatal("inconsistent snapshot")
	}
	if len(got) != 1 || got[0] != idB {
		t.Fatalf("got %v, want [%s] only", got, idB)
	}
	if len(e.cursors) != 1 {
		t.Fatalf("both instances of %s should be gone; %d cursors remain", idA, len(e.cursors))
	}
}

// TestStopUnknownClipIsNoop: a clip that was never triggered has no registry
// index, and stopping it must not disturb what IS playing.
func TestStopUnknownClipIsNoop(t *testing.T) {
	e, id := newTestEngine(t)
	e.TriggerGain(id, 1)
	pumpDuplex(e, 1)

	e.StopClip("no/such-clip")
	pumpDuplex(e, 1)

	if got, _ := e.PlayingClips(); len(got) != 1 || got[0] != id {
		t.Fatalf("got %v, want [%s] untouched", got, id)
	}
}

// TestPublishIsAllocationFree pins the constraint that makes this safe on the
// real-time audio thread: the publish path must not allocate. It measures the
// callback with clips active (the publishing path) after the cursor slice has
// grown, so any per-buffer allocation would show up.
func TestPublishIsAllocationFree(t *testing.T) {
	const runs = 200

	e, id := newTestEngine(t)
	// A clip long enough to stay active for every measured buffer, so the
	// publishing path (not the idle fast path) is what gets measured.
	e.lib.Get(id).PCM = flat(0.3, periodFrames*(runs+8))
	e.TriggerGain(id, 1)

	out, mic := bufBytes(), bufBytes()
	e.duplexCallback(out, mic, periodFrames) // warm up: drain the pending clip, grow the slice
	if len(e.cursors) != 1 {
		t.Fatalf("setup: expected 1 active cursor, got %d", len(e.cursors))
	}

	allocs := testing.AllocsPerRun(runs, func() {
		e.duplexCallback(out, mic, periodFrames)
	})
	if len(e.cursors) != 1 {
		t.Fatalf("clip retired mid-measurement; the idle path was measured instead")
	}
	if allocs != 0 {
		t.Fatalf("RT callback allocated %v times per buffer while publishing; it must be allocation-free", allocs)
	}
}

// TestPlayingSetSnapshotUnderConcurrentPublish runs the poller against a live
// callback the way app.go's events loop does, verifying the seqlock reader never
// reports a torn set (run with -race for the data-race half).
func TestPlayingSetSnapshotUnderConcurrentPublish(t *testing.T) {
	e, id := newTestEngine(t)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		out, mic := bufBytes(), bufBytes()
		for {
			select {
			case <-stop:
				return
			default:
				e.TriggerGain(id, 1)
				e.duplexCallback(out, mic, periodFrames)
			}
		}
	}()

	deadline := time.Now().Add(200 * time.Millisecond)
	reads, torn := 0, 0
	for time.Now().Before(deadline) {
		got, ok := e.PlayingClips()
		reads++
		if !ok {
			torn++
			continue
		}
		for _, gotID := range got {
			if gotID != id {
				t.Fatalf("snapshot contained an unknown clip %q", gotID)
			}
		}
	}
	close(stop)
	wg.Wait()

	if reads == 0 {
		t.Fatal("no snapshots taken")
	}
	// A torn read is legal (the caller keeps its previous view) but should be
	// rare; a high rate would mean the seqlock retry budget is wrong.
	if torn*10 > reads {
		t.Fatalf("%d of %d snapshots were torn; retry budget looks too small", torn, reads)
	}
}

// TestStopResetsPlayingSet: tearing the engine down must clear the published set,
// otherwise the UI would keep showing chips for clips that can no longer play.
func TestStopResetsPlayingSet(t *testing.T) {
	e, id := newTestEngine(t)
	e.TriggerGain(id, 1)
	pumpDuplex(e, 1)
	if got, _ := e.PlayingClips(); len(got) != 1 {
		t.Fatalf("setup: expected 1 playing, got %v", got)
	}

	if err := e.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if got, _ := e.PlayingClips(); len(got) != 0 {
		t.Fatalf("after Stop: got %v, want empty", got)
	}
}
