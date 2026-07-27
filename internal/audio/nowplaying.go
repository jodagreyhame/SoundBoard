package audio

// nowplaying.go publishes WHICH clips are currently playing so the UI can show a
// live "now playing" list that clears itself when a clip ends.
//
// The problem this solves: the only place that knows a clip finished is the
// real-time mix callback (mixInto compacts a cursor out of the slice the moment
// clipCursor.done() goes true). That callback may not emit an event, take a
// mutex, log, or allocate — so it cannot tell the UI anything directly.
//
// The mechanism, mirroring how gateLevel is published (a single atomic the RT
// side writes and the UI polls at ~20 Hz):
//
//   - clipRegistry interns each triggered clip's canonical ID to a small stable
//     int32 index. Interning happens in TriggerGain and lookup in the poller —
//     both NON-RT goroutines — so the audio thread only ever handles ints and
//     never touches a map or a string.
//   - playSet is a fixed-size table of those indices guarded by a SEQLOCK. The
//     duplex callback republishes its WHOLE current cursor set once per buffer;
//     the poller reads a consistent snapshot with a bounded retry.
//
// Publishing the whole set (rather than one-shot "clip ended" pulses) is what
// makes this self-healing: StopAll, a device teardown, a dropped trigger, and a
// missed poll all converge on the truth within one buffer, because the published
// state is DERIVED from the cursor slice every time rather than accumulated.
//
// RT cost per buffer: two atomic adds plus one store per active cursor, and an
// early-out to zero work while nothing is playing. No allocation, no lock, no
// syscall, bounded by maxPublishedPlays.

import (
	"sync"
	"sync/atomic"
)

// maxPublishedPlays bounds the published table. The UI shows a handful of
// entries and the mixer realistically carries a few simultaneous clips; a set
// larger than this is truncated (the extra clips still PLAY, they are just not
// listed). Fixed size is what keeps publish allocation-free.
const maxPublishedPlays = 32

// playSet is the RT-published set of currently-playing clip indices.
//
// Concurrency: exactly ONE writer — the duplex RT callback, via publish — and
// any number of readers via snapshot. seq is the seqlock: the writer bumps it to
// ODD before touching the table and to EVEN after, so a reader that sees the
// same EVEN value before and after its read knows the table did not move under
// it. reset is the one exception to the single-writer rule and may only be
// called while no callback is running (Engine.Stop, after the device is
// uninitialized).
type playSet struct {
	seq atomic.Uint64
	n   atomic.Int32
	idx [maxPublishedPlays]atomic.Int32
}

// publish replaces the published set with the callback's current cursors. Called
// once per buffer from the duplex RT callback, which owns `cursors` exclusively.
// Allocation-free, lock-free, and bounded.
func (p *playSet) publish(cursors []*clipCursor) {
	n := len(cursors)
	if n > maxPublishedPlays {
		n = maxPublishedPlays
	}
	// Idle fast path: nothing is playing and nothing was published last buffer,
	// so the table already reads empty. This is the overwhelmingly common case
	// (no clip triggered), and it costs a single atomic load. Only the writer
	// stores n, so loading it here is race-free.
	if n == 0 && p.n.Load() == 0 {
		return
	}
	p.seq.Add(1) // -> odd: table in flux
	for i := 0; i < n; i++ {
		p.idx[i].Store(cursors[i].idx)
	}
	p.n.Store(int32(n))
	p.seq.Add(1) // -> even: table consistent
}

// snapshot copies the published indices into dst (reusing its backing array) and
// reports whether the read was consistent. A false return means the writer was
// mid-publish for every attempt — vanishingly unlikely against a 50 ms poll, but
// the caller must then KEEP ITS PREVIOUS VIEW rather than treat the empty result
// as "nothing is playing", or the UI would flicker.
func (p *playSet) snapshot(dst []int32) ([]int32, bool) {
	for try := 0; try < 8; try++ {
		s1 := p.seq.Load()
		if s1&1 != 0 {
			continue // writer is inside the table
		}
		n := int(p.n.Load())
		if n < 0 {
			n = 0
		}
		if n > maxPublishedPlays {
			n = maxPublishedPlays
		}
		dst = dst[:0]
		for i := 0; i < n; i++ {
			dst = append(dst, p.idx[i].Load())
		}
		if p.seq.Load() == s1 {
			return dst, true
		}
	}
	return dst[:0], false
}

// reset clears the published set. Only safe to call when no RT callback is
// running (Engine.Stop, after the devices are uninitialized) — it writes the
// same fields publish does, without the seqlock discipline being useful.
func (p *playSet) reset() {
	p.seq.Add(1)
	p.n.Store(0)
	p.seq.Add(1)
}

// clipRegistry interns clip IDs to stable int32 indices so the RT path can
// identify a clip with an integer instead of a string. Entries are added on
// first trigger and never removed, so an index stays valid for the process
// lifetime — which is what lets the RT callback publish a bare int and the
// poller resolve it later.
//
// Every method is called from NON-RT goroutines only (TriggerGain, StopClip, and
// the events poller), so an ordinary mutex is the right tool here.
type clipRegistry struct {
	mu   sync.Mutex
	byID map[string]int32
	ids  []string
}

// intern returns the index for id, assigning a new one on first sight.
func (r *clipRegistry) intern(id string) int32 {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.byID == nil {
		r.byID = make(map[string]int32)
	}
	if i, ok := r.byID[id]; ok {
		return i
	}
	i := int32(len(r.ids))
	r.ids = append(r.ids, id)
	r.byID[id] = i
	return i
}

// lookup returns the index previously assigned to id. A clip that has never been
// triggered has no index — and therefore cannot be playing.
func (r *clipRegistry) lookup(id string) (int32, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	i, ok := r.byID[id]
	return i, ok
}

// names resolves indices back to clip IDs, dropping unknown indices and
// duplicates (the same clip triggered twice has two cursors but is ONE entry in
// the UI). Order is preserved: cursors are appended in trigger order, so the
// result is oldest-first.
func (r *clipRegistry) names(idx []int32) []string {
	if len(idx) == 0 {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(idx))
	for _, i := range idx {
		if i < 0 || int(i) >= len(r.ids) {
			continue
		}
		id := r.ids[i]
		dup := false
		for _, seen := range out {
			if seen == id {
				dup = true
				break
			}
		}
		if !dup {
			out = append(out, id)
		}
	}
	return out
}
