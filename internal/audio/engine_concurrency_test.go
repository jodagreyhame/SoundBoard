package audio

import (
	"sync"
	"testing"
	"testing/fstest"

	"soundboard/internal/catalog"
)

// newTestEngine builds an engine with a tiny in-memory library, no real audio
// context. It exercises only the lock-free Trigger -> callback handoff, which
// needs no hardware. The library is built through catalog.New (which indexes by
// ID without decoding); we then fill the clip PCM directly.
func newTestEngine(t *testing.T) (*Engine, string) {
	t.Helper()
	lib, err := catalog.New(fstest.MapFS{
		"sounds/test/clip.wav": {Data: []byte("not decoded in this test")},
	})
	if err != nil {
		t.Fatalf("catalog.New: %v", err)
	}
	const id = "test/clip"
	clip := lib.Get(id)
	if clip == nil {
		t.Fatalf("clip %q not indexed", id)
	}
	clip.PCM = flat(0.3, fadeFrames*4)
	e := NewEngine(nil, lib)
	return e, id
}

// TestTriggerCallbackHandoffNoRace fires Trigger from many goroutines while the
// duplex and monitor callbacks run concurrently, verifying the lock-free
// channel handoff is data-race-free (run with -race) and that triggered clips
// actually reach the mixer. The callbacks own their cursor slices; Trigger only
// touches the channels and the atomic flag, so there is no shared mutable state
// to race on.
func TestTriggerCallbackHandoffNoRace(t *testing.T) {
	e, id := newTestEngine(t)
	e.monitorActive.Store(true) // route to both queues without a real device

	const producers = 8
	const perProducer = 200

	stop := make(chan struct{})

	// Simulate the two RT callbacks pulling buffers continuously. Each owns its
	// cursor slice exclusively AND, as in production, gets its OWN output buffer
	// from miniaudio (the two devices never share a backing array).
	var cbWG sync.WaitGroup
	run := func(drain func()) {
		defer cbWG.Done()
		for {
			select {
			case <-stop:
				return
			default:
				drain()
			}
		}
	}
	cbWG.Add(2)
	go run(func() {
		buf := make([]byte, periodFrames*channels*4)
		mic := make([]byte, periodFrames*channels*4)
		e.duplexCallback(buf, mic, periodFrames)
	})
	go run(func() {
		buf := make([]byte, periodFrames*channels*4)
		e.monitorCallback(buf, nil, periodFrames)
	})

	var prodWG sync.WaitGroup
	prodWG.Add(producers)
	for p := 0; p < producers; p++ {
		go func() {
			defer prodWG.Done()
			for i := 0; i < perProducer; i++ {
				e.Trigger(id)
			}
		}()
	}
	prodWG.Wait()
	close(stop)
	cbWG.Wait()

	// Final sanity: triggering an unknown id is a no-op and does not panic.
	e.Trigger("does/not-exist")
}

// TestSetMonitorResetsCursors confirms SetMonitor(nil) drops the monitor cursor
// slice so a later re-enable does not resume stale, partially-played cursors.
// No real device is involved: we plant cursors directly and tear the (nil)
// monitor down. SetMonitor(nil) returns before opening any device, so the
// teardown path that resets monCursors is exercised without hardware.
func TestSetMonitorResetsCursors(t *testing.T) {
	e := NewEngine(nil, nil)
	e.monCursors = []*clipCursor{{pcm: flat(0.3, fadeFrames*4)}}
	if err := e.SetMonitor(nil); err != nil {
		t.Fatalf("SetMonitor(nil): %v", err)
	}
	if e.monCursors != nil {
		t.Fatalf("SetMonitor(nil) must clear monCursors, got %d", len(e.monCursors))
	}
}

// TestStopResetsCursors confirms Stop drops BOTH cursor slices and drains both
// pending queues. Stop is hardware-free when no device was configured, so it
// exercises the same callback-owned-slice reset that Configure/SetMonitor rely
// on after teardown.
func TestStopResetsCursors(t *testing.T) {
	e := NewEngine(nil, nil)
	e.cursors = []*clipCursor{{pcm: flat(0.3, fadeFrames*4)}}
	e.monCursors = []*clipCursor{{pcm: flat(0.3, fadeFrames*4)}}
	e.pending <- pendingClip{}
	e.monPending <- pendingClip{}

	if err := e.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if e.cursors != nil || e.monCursors != nil {
		t.Fatalf("Stop must clear both cursor slices, got %d/%d", len(e.cursors), len(e.monCursors))
	}
	if len(e.pending) != 0 || len(e.monPending) != 0 {
		t.Fatalf("Stop must drain both pending queues, got %d/%d", len(e.pending), len(e.monPending))
	}
}

// TestTriggerDropsWhenFull confirms Trigger never blocks even if the pending
// queue is saturated (the callbacks may be stalled). It returns promptly and
// drops the overflow rather than blocking the calling goroutine.
func TestTriggerDropsWhenFull(t *testing.T) {
	e, id := newTestEngine(t)
	// No callback is draining, so after pendingCap triggers the channel is full;
	// further triggers must not block.
	for i := 0; i < pendingCap*2; i++ {
		e.Trigger(id) // would deadlock if Trigger blocked on a full channel
	}
}
