package audio

// ring.go is the lock-free single-producer / single-consumer (SPSC) handoff that
// decouples the malgo real-time callback from the heavy DSP worker goroutine.
//
// Why a ring and not a channel: the mic chain needs RNNoise, which only accepts
// exactly 480-sample (10ms) MONO frames, while our duplex period is 192 frames
// of STEREO. So the RT callback can never run RNNoise itself (wrong frame size,
// and the network is far too heavy for the audio thread). Instead the callback
// does only cheap bounded work and shovels mono samples across two of these rings
// to/from a worker:
//
//	duplexCallback  --push-->  input ring  --pull-->  worker (HPF/denoise/AGC/gate)
//	duplexCallback  <--pull--  output ring <--push--  worker
//
// SPSC contract (STRICT): for one ring, exactly ONE goroutine calls push and
// exactly ONE other goroutine calls pull. Under that contract no lock is needed:
// the producer owns w (write index) and only loads r; the consumer owns r and
// only loads w. Both indices are monotonically increasing uint64 counters (never
// wrapped), masked to an index into a power-of-two backing array. Using
// free-running counters instead of wrapped indices removes the classic
// "full vs empty look identical" ambiguity: size == w-r is always exact, and a
// 64-bit counter cannot realistically overflow at audio rates.
//
// The RT callback (audio thread) must NEVER block, so its push/pull are
// non-blocking and bounded: they move as many samples as fit / are available and
// return the count. The worker may spin briefly but is given a non-blocking API
// too; it sleeps between empty polls rather than busy-waiting hard (see worker.go).

import "sync/atomic"

// ringSPSC is a lock-free SPSC float32 ring buffer. The zero value is unusable;
// construct with newRing. All storage is preallocated, so push/pull never
// allocate and are safe on the real-time audio thread.
type ringSPSC struct {
	buf  []float32
	mask uint64 // len(buf)-1; len(buf) is a power of two

	// w is the total number of samples ever pushed; r the total ever pulled.
	// They only grow. The producer writes w (after filling the slot); the
	// consumer writes r (after reading the slot). Each side loads the other's
	// index with Acquire/Release ordering provided by atomic.Uint64 so the sample
	// writes are correctly published across goroutines.
	w atomic.Uint64
	r atomic.Uint64
}

// nextPow2 rounds n up to the next power of two (minimum 2). Capacities are kept
// power-of-two so the index wrap is a single mask instead of a modulo.
func nextPow2(n int) int {
	if n < 2 {
		return 2
	}
	p := 1
	for p < n {
		p <<= 1
	}
	return p
}

// newRing allocates a ring that can hold at least capSamples float32 samples. The
// real capacity is rounded up to a power of two. Allocation happens once here, off
// the audio thread.
func newRing(capSamples int) *ringSPSC {
	size := nextPow2(capSamples)
	return &ringSPSC{
		buf:  make([]float32, size),
		mask: uint64(size - 1),
	}
}

// cap returns the maximum number of samples the ring can hold.
func (rg *ringSPSC) capacity() int { return len(rg.buf) }

// length returns the number of samples currently available to pull. Safe to call
// from either side (it is only a hint to the other side; the owning side's own
// index is exact).
func (rg *ringSPSC) length() int {
	return int(rg.w.Load() - rg.r.Load())
}

// push copies as many samples from src into the ring as currently fit, WITHOUT
// blocking or overwriting unread data, and returns the count written (which may be
// less than len(src), or 0 if the ring is full). Called only by the producer
// goroutine for this ring. Allocation-free.
//
// The producer owns w: it loads r once (the consumer's progress), computes the
// free space, copies into the slots [w, w+n), then publishes the new w with a
// single atomic store. The store releases the sample writes to the consumer.
func (rg *ringSPSC) push(src []float32) int {
	w := rg.w.Load()
	r := rg.r.Load()
	free := uint64(len(rg.buf)) - (w - r)
	n := uint64(len(src))
	if n > free {
		n = free
	}
	for i := uint64(0); i < n; i++ {
		rg.buf[(w+i)&rg.mask] = src[i]
	}
	rg.w.Store(w + n)
	return int(n)
}

// pull copies up to len(dst) available samples from the ring into dst WITHOUT
// blocking, and returns the count read (0 if the ring is empty). Called only by
// the consumer goroutine for this ring. Allocation-free.
//
// The consumer owns r: it loads w once (the producer's progress), computes how
// many samples are available, copies out of the slots [r, r+n), then publishes the
// new r with a single atomic store so the producer sees the freed space.
func (rg *ringSPSC) pull(dst []float32) int {
	r := rg.r.Load()
	w := rg.w.Load()
	avail := w - r
	n := uint64(len(dst))
	if n > avail {
		n = avail
	}
	for i := uint64(0); i < n; i++ {
		dst[i] = rg.buf[(r+i)&rg.mask]
	}
	rg.r.Store(r + n)
	return int(n)
}

// reset discards all buffered samples by snapping the read index up to the write
// index. It must be called only when NEITHER side is concurrently pushing or
// pulling (e.g. during Stop/Configure with the device uninitialized), so it is not
// part of the lock-free hot path. It does not allocate.
func (rg *ringSPSC) reset() {
	rg.r.Store(rg.w.Load())
}
