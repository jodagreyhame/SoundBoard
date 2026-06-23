package audio

import (
	"sync"
	"testing"
)

// TestRingPushPullBasic checks the simplest contract: pushed samples come back out
// in FIFO order, push reports the count written, and pull reports the count read.
func TestRingPushPullBasic(t *testing.T) {
	r := newRing(8) // rounds to 8
	if r.capacity() != 8 {
		t.Fatalf("capacity = %d, want 8", r.capacity())
	}
	in := []float32{1, 2, 3, 4, 5}
	if n := r.push(in); n != 5 {
		t.Fatalf("push wrote %d, want 5", n)
	}
	if r.length() != 5 {
		t.Fatalf("length = %d, want 5", r.length())
	}
	dst := make([]float32, 5)
	if n := r.pull(dst); n != 5 {
		t.Fatalf("pull read %d, want 5", n)
	}
	for i, v := range in {
		if dst[i] != v {
			t.Errorf("sample %d: got %v want %v", i, dst[i], v)
		}
	}
	if r.length() != 0 {
		t.Errorf("ring should be empty, length = %d", r.length())
	}
}

// TestRingFullDropsAndNeverOverwrites confirms push never overwrites unread data:
// once the ring is full it writes 0 more samples, and the buffered data is intact.
func TestRingFullDropsAndNeverOverwrites(t *testing.T) {
	r := newRing(4)
	first := []float32{1, 2, 3, 4}
	if n := r.push(first); n != 4 {
		t.Fatalf("push wrote %d, want 4 (full)", n)
	}
	// Ring is now full; a further push must write nothing.
	if n := r.push([]float32{9, 9}); n != 0 {
		t.Fatalf("push to full ring wrote %d, want 0", n)
	}
	// A partial push when there is only room for some: drain 2, then push 4 -> only
	// 2 fit.
	got := make([]float32, 2)
	r.pull(got)
	if got[0] != 1 || got[1] != 2 {
		t.Fatalf("pull got %v, want [1 2]", got)
	}
	if n := r.push([]float32{5, 6, 7, 8}); n != 2 {
		t.Fatalf("partial push wrote %d, want 2", n)
	}
	// Remaining order must be 3,4,5,6.
	rest := make([]float32, 4)
	if n := r.pull(rest); n != 4 {
		t.Fatalf("pull read %d, want 4", n)
	}
	want := []float32{3, 4, 5, 6}
	for i := range want {
		if rest[i] != want[i] {
			t.Errorf("rest[%d] = %v, want %v", i, rest[i], want[i])
		}
	}
}

// TestRingWrapAround drives many push/pull cycles so the masked indices wrap the
// backing array repeatedly, verifying FIFO order is preserved across the wrap.
func TestRingWrapAround(t *testing.T) {
	r := newRing(4)
	next := float32(0)
	expect := float32(0)
	chunk := make([]float32, 3)
	out := make([]float32, 3)
	for iter := 0; iter < 1000; iter++ {
		for i := range chunk {
			chunk[i] = next
			next++
		}
		n := r.push(chunk)
		got := r.pull(out[:n])
		if got != n {
			t.Fatalf("iter %d: pushed %d but pulled %d", iter, n, got)
		}
		for i := 0; i < got; i++ {
			if out[i] != expect {
				t.Fatalf("iter %d sample %d: got %v want %v", iter, i, out[i], expect)
			}
			expect++
		}
	}
}

// TestRingSPSCConcurrentNoLoss is the core correctness test: one producer pushes a
// long monotonically-increasing sequence while one consumer pulls concurrently;
// every value must appear exactly once, in order, with no loss or duplication.
// Run under -race to also prove the atomic indices are correctly synchronized.
func TestRingSPSCConcurrentNoLoss(t *testing.T) {
	const total = 1 << 16 // 65536 samples through a small ring
	r := newRing(64)

	var wg sync.WaitGroup
	wg.Add(2)

	// Producer: push 0,1,2,...,total-1, retrying on a full ring (never overwrite).
	go func() {
		defer wg.Done()
		var sent int
		buf := make([]float32, 1)
		for sent < total {
			buf[0] = float32(sent)
			if r.push(buf) == 1 {
				sent++
			}
		}
	}()

	// Consumer: pull until it has received total samples; assert strict order.
	var firstErr string
	go func() {
		defer wg.Done()
		var recv int
		var want float32
		buf := make([]float32, 7) // odd size to exercise partial pulls
		for recv < total {
			n := r.pull(buf)
			for i := 0; i < n; i++ {
				if buf[i] != want && firstErr == "" {
					firstErr = "out-of-order or lost sample"
				}
				want++
				recv++
			}
		}
	}()

	wg.Wait()
	if firstErr != "" {
		t.Fatal(firstErr)
	}
}

// TestRingReset confirms reset discards buffered samples (length back to 0) without
// corrupting subsequent use.
func TestRingReset(t *testing.T) {
	r := newRing(8)
	r.push([]float32{1, 2, 3})
	r.reset()
	if r.length() != 0 {
		t.Fatalf("after reset length = %d, want 0", r.length())
	}
	// Still usable after reset.
	if n := r.push([]float32{7, 8}); n != 2 {
		t.Fatalf("push after reset wrote %d, want 2", n)
	}
	out := make([]float32, 2)
	r.pull(out)
	if out[0] != 7 || out[1] != 8 {
		t.Errorf("after reset got %v, want [7 8]", out)
	}
}

// TestNextPow2 pins the capacity rounding.
func TestNextPow2(t *testing.T) {
	cases := map[int]int{0: 2, 1: 2, 2: 2, 3: 4, 5: 8, 8: 8, 9: 16, 480: 512}
	for in, want := range cases {
		if got := nextPow2(in); got != want {
			t.Errorf("nextPow2(%d) = %d, want %d", in, got, want)
		}
	}
}
