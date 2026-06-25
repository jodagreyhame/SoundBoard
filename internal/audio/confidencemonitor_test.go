package audio

// confidencemonitor_test.go is the regression suite for the CONFIDENCE MONITOR:
// the "transmitted" monitor source that lets the user hear, on their own
// headphones, the EXACT signal sent to the cable (processedMic + clips) so the
// transmitted quality is auditable.
//
// The design is RT-safe: duplexCallback taps its FINAL cable mix into a
// preallocated SPSC tap ring (only when in "transmitted" mode AND a monitor is
// active), and monitorCallback DRAINS that ring into the monitor output instead of
// mixing the monitor clip cursors. On a tap-ring underrun the monitor holds-last
// and ramps to silence (the buzz-fix lesson — never a raw/stale splice). The tap
// ring is primed one period in Start so the independent duplex/monitor clocks do
// not race frame-for-frame.
//
// All tests here are hardware-free: they drive the real duplexCallback /
// monitorCallback over the real rings on a configuredEngine() (no malgo device).

import (
	"sync"
	"testing"

	"soundboard/internal/catalog"
)

// TestMonitorSourceDefaultAndSetGet pins the source enum surface: a fresh engine
// defaults to clips-only (the legacy behavior), the setter maps the two known
// strings (and coerces unknown values to clips), and GetMonitorSource round-trips.
func TestMonitorSourceDefaultAndSetGet(t *testing.T) {
	e := NewEngine(nil, nil)

	// Default: clips-only, so a fresh engine reproduces the historical monitor.
	if got := e.GetMonitorSource(); got != MonitorSourceClips {
		t.Fatalf("default monitor source = %q, want %q", got, MonitorSourceClips)
	}
	if e.monitorTransmitting() {
		t.Fatalf("a fresh engine must not be in transmitted mode")
	}

	e.SetMonitorSource(MonitorSourceTransmitted)
	if got := e.GetMonitorSource(); got != MonitorSourceTransmitted {
		t.Fatalf("after Set(transmitted): GetMonitorSource = %q, want %q", got, MonitorSourceTransmitted)
	}
	if !e.monitorTransmitting() {
		t.Fatalf("after Set(transmitted): monitorTransmitting must be true")
	}

	// Unknown / empty values coerce back to clips (never an undefined state).
	for _, in := range []string{"", "bogus", "CLIPS", "Transmitted"} {
		e.SetMonitorSource(in)
		if got := e.GetMonitorSource(); got != MonitorSourceClips {
			t.Errorf("Set(%q): GetMonitorSource = %q, want %q (default)", in, got, MonitorSourceClips)
		}
	}

	// Round-trip the valid clips value explicitly.
	e.SetMonitorSource(MonitorSourceClips)
	if got := e.GetMonitorSource(); got != MonitorSourceClips {
		t.Fatalf("Set(clips): GetMonitorSource = %q, want %q", got, MonitorSourceClips)
	}
}

// TestPrimeTapRingAppliesOnePeriod pins the production tap-ring prime: Start()
// calls primeTapRing, which must leave exactly one period of interleaved-stereo
// silence so the monitor device starts a period behind the duplex tap and never
// races it. Deleting the prime push turns this red.
func TestPrimeTapRingAppliesOnePeriod(t *testing.T) {
	e := configuredEngine()

	if avail := e.tapRing.length(); avail != 0 {
		t.Fatalf("precondition: freshly configured tap ring should be empty, got %d", avail)
	}

	e.primeTapRing()

	wantLen := periodFrames * channels
	if avail := e.tapRing.length(); avail != wantLen {
		t.Fatalf("primeTapRing must leave one period of lead: got %d, want %d", avail, wantLen)
	}

	// And the primed period must be silence (a prefill, not signal).
	out := make([]float32, wantLen)
	if n := e.tapRing.pull(out); n != wantLen {
		t.Fatalf("expected to pull a full primed period, got %d samples", n)
	}
	for i, s := range out {
		if s != 0 {
			t.Fatalf("primed period must be silence: sample %d = %v", i, s)
		}
	}
}

// TestPrimeTapRingNilSafe confirms primeTapRing is a no-op (no panic) when the tap
// ring was never allocated, mirroring primeOutputRing's self-safety.
func TestPrimeTapRingNilSafe(t *testing.T) {
	e := NewEngine(nil, nil) // no rings allocated
	if e.tapRing != nil {
		t.Fatalf("precondition: expected nil tap ring on a bare engine")
	}
	e.primeTapRing() // must not panic
}

// TestMonitorReEnableResetsAndPrimesTapRing is the regression guard for the runtime
// monitor toggle (SetMonitor while started, in "transmitted" mode). When the monitor
// is turned OFF the duplex tap is gated off (monitorActive false) so the tap ring is
// no longer drained; a re-enable must therefore (1) CLEAR any stale cable mix left in
// the ring from the prior enabled period, and (2) RESTORE the one-period silence lead
// so the monitor's pull does not race the duplex's push frame-for-frame. SetMonitor
// runs e.resetAndPrimeTapRing() before re-activating; this drives that exact sequence
// hardware-free (no malgo device) after deliberately leaving the ring in the bad
// post-toggle state (stale data, zero lead) and asserts it ends primed and clean.
//
// Against the pre-fix SetMonitor (which neither reset nor primed) the ring would
// still hold the stale partial backlog and have NO lead — exactly the two failures
// the finding called out — so this test would be red.
func TestMonitorReEnableResetsAndPrimesTapRing(t *testing.T) {
	e := configuredEngine()
	e.SetMonitorSource(MonitorSourceTransmitted)

	// Simulate the state left behind after the monitor ran, then was toggled OFF: a
	// stale, sub-period chunk of cable mix sits undrained in the tap ring (the duplex
	// tap is gated on monitorActive, which is now false, so nothing clears it).
	const staleFrames = 5 // a partial period, like a real mid-period backlog
	stale := make([]float32, staleFrames*channels)
	for i := range stale {
		stale[i] = 0.7 // non-silent so a replay would be audibly stale
	}
	if n := e.tapRing.push(stale); n != len(stale) {
		t.Fatalf("precondition: failed to seed stale backlog, pushed %d of %d", n, len(stale))
	}
	if avail := e.tapRing.length(); avail != len(stale) {
		t.Fatalf("precondition: tap ring should hold the stale backlog, got %d", avail)
	}

	// Re-enable path: SetMonitor calls exactly this before flipping monitorActive on.
	e.resetAndPrimeTapRing()

	// The ring must now hold EXACTLY one period of lead — the stale backlog gone, the
	// prime restored (not stale+prime appended).
	wantLen := periodFrames * channels
	if avail := e.tapRing.length(); avail != wantLen {
		t.Fatalf("after re-enable the tap ring must hold exactly one primed period: got %d, want %d", avail, wantLen)
	}

	// And every sample of that lead must be silence — no stale 0.7s survived the reset.
	out := make([]float32, wantLen)
	if n := e.tapRing.pull(out); n != wantLen {
		t.Fatalf("expected to pull one full primed period, got %d", n)
	}
	for i, s := range out {
		if s != 0 {
			t.Fatalf("re-enable lead must be pure silence (stale backlog cleared): sample %d = %v", i, s)
		}
	}
}

// TestClipsModeDoesNotTap confirms that in the DEFAULT clips mode the duplex
// callback does NOT push anything into the tap ring — the confidence-monitor path
// is fully idle, so clips mode keeps its exact historical behavior and the ring
// never accumulates.
func TestClipsModeDoesNotTap(t *testing.T) {
	e := configuredEngine()
	e.SetMonitorSource(MonitorSourceClips)
	e.monitorActive.Store(true) // monitor is open, but we are in clips mode

	micF := make([]float32, periodFrames*channels)
	for i := range micF {
		micF[i] = 0.2
	}
	outB := make([]byte, periodFrames*channels*4)
	e.duplexCallback(outB, f32bytes(micF), periodFrames)

	if avail := e.tapRing.length(); avail != 0 {
		t.Fatalf("clips mode tapped %d samples into the tap ring; want 0 (idle)", avail)
	}
}

// TestTransmittedModeRequiresActiveMonitor confirms the duplex tap is gated on an
// ACTIVE monitor: even in transmitted mode, with no monitor open to consume, the
// duplex callback does NOT push to the tap ring. That keeps the ring empty (no
// unbounded backlog) until a monitor is actually present to drain it.
func TestTransmittedModeRequiresActiveMonitor(t *testing.T) {
	e := configuredEngine()
	e.SetMonitorSource(MonitorSourceTransmitted)
	e.monitorActive.Store(false) // no monitor open

	micF := make([]float32, periodFrames*channels)
	for i := range micF {
		micF[i] = 0.2
	}
	outB := make([]byte, periodFrames*channels*4)
	e.duplexCallback(outB, f32bytes(micF), periodFrames)

	if avail := e.tapRing.length(); avail != 0 {
		t.Fatalf("transmitted mode with no active monitor tapped %d samples; want 0", avail)
	}
}

// TestTransmittedMonitorPlaysExactCableMix is the core confidence-monitor check:
// the monitor in "transmitted" mode plays back the EXACT bytes the duplex callback
// wrote to the cable (scaled by the monitor gain), and the monitor's own clip
// cursors are NOT mixed in on top (no doubling). It drives one duplexCallback (which
// taps the cable mix) then one monitorCallback (which drains it) and asserts a
// sample-for-sample match at unity monitor gain.
func TestTransmittedMonitorPlaysExactCableMix(t *testing.T) {
	e := configuredEngine()
	e.SetMonitorSource(MonitorSourceTransmitted)
	e.monitorActive.Store(true)
	// Force the mic gate open so the (worker-less) duplex path passes the mic through
	// to the cable as a steady, non-silent signal we can match exactly.
	e.SetMicMode("always")

	// A distinctive ramp mic signal so an accidental silence/zeroing would be obvious.
	micF := make([]float32, periodFrames*channels)
	for f := 0; f < periodFrames; f++ {
		v := float32(f) / float32(periodFrames) // 0..~1 ramp
		micF[f*channels] = v
		micF[f*channels+1] = -v
	}
	cableB := make([]byte, periodFrames*channels*4)

	// No worker is running, so the duplex path underruns to gained mic passthrough —
	// a deterministic, steady cable signal. Run the duplex callback: it computes the
	// cable mix into cableB AND taps a copy into the tap ring.
	e.duplexCallback(cableB, f32bytes(micF), periodFrames)
	cable := append([]float32(nil), bytesAsF32(cableB)...) // snapshot the cable bytes

	// The tap ring must now hold exactly this one period (it started empty in this
	// test — configuredEngine does not prime — so length == one period).
	if avail := e.tapRing.length(); avail != periodFrames*channels {
		t.Fatalf("after duplex tap, tap ring holds %d samples; want %d", avail, periodFrames*channels)
	}

	// Now the monitor callback drains the tap into the monitor output at unity gain.
	monB := make([]byte, periodFrames*channels*4)
	e.SetMonitorGain(1)
	e.monitorCallback(monB, nil, periodFrames)
	mon := bytesAsF32(monB)

	// Sample-for-sample: the monitor output equals the cable signal (what Discord
	// hears). This is the whole point — the monitor is an honest copy of the transmit.
	if len(mon) != len(cable) {
		t.Fatalf("monitor/cable length mismatch: %d vs %d", len(mon), len(cable))
	}
	for i := range cable {
		if !approx(mon[i], cable[i]) {
			t.Fatalf("monitor sample %d = %v, want %v (exact cable mix)", i, mon[i], cable[i])
		}
	}

	// And the cable signal must be non-silent (the test would be vacuous otherwise).
	if rms(cable) < 0.01 {
		t.Fatalf("cable signal is ~silent (RMS %v); the exact-mix check is vacuous", rms(cable))
	}
}

// TestTransmittedMonitorAppliesMonitorGain confirms the monitor ("you hear") gain
// scales the drained transmit mix, so the user can lower their local monitor level
// without changing what Discord receives. It taps a known cable mix then drains it
// at gain 0.5 and asserts every sample is halved.
func TestTransmittedMonitorAppliesMonitorGain(t *testing.T) {
	e := configuredEngine()
	e.SetMonitorSource(MonitorSourceTransmitted)
	e.monitorActive.Store(true)
	e.SetMicMode("always")

	const amp = 0.4
	micF := make([]float32, periodFrames*channels)
	for i := range micF {
		micF[i] = amp
	}
	cableB := make([]byte, periodFrames*channels*4)
	e.duplexCallback(cableB, f32bytes(micF), periodFrames)
	cable := append([]float32(nil), bytesAsF32(cableB)...)

	const g = 0.5
	e.SetMonitorGain(g)
	monB := make([]byte, periodFrames*channels*4)
	e.monitorCallback(monB, nil, periodFrames)
	mon := bytesAsF32(monB)

	for i := range cable {
		if !approx(mon[i], cable[i]*g) {
			t.Fatalf("monitor sample %d = %v, want %v (cable * %v)", i, mon[i], cable[i]*g, g)
		}
	}
}

// TestTransmittedMonitorUnderrunSilentFromCold confirms the buzz-fix lesson on the
// monitor side: with an EMPTY tap ring (the duplex never tapped this period), the
// monitor callback must NOT block, must NOT splice stale/raw audio, and must output
// a bounded hold-last-then-ramp-to-silence seam. With no prior delivered frame the
// hold value is zero, so the entire underrun buffer is pure silence — never noise.
func TestTransmittedMonitorUnderrunSilentFromCold(t *testing.T) {
	e := configuredEngine()
	e.SetMonitorSource(MonitorSourceTransmitted)
	e.monitorActive.Store(true)
	// Tap ring is empty (no duplex tap happened) and monTapHold is zero (cold start).

	monB := make([]byte, periodFrames*channels*4)
	e.monitorCallback(monB, nil, periodFrames)

	for i, s := range bytesAsF32(monB) {
		if s != 0 {
			t.Fatalf("cold underrun must be pure silence: monitor sample %d = %v", i, s)
		}
	}
}

// TestTransmittedMonitorUnderrunRampsFromLastFrame confirms that when the tap
// underruns AFTER some signal was delivered, the seam ramps from the last delivered
// frame down to silence over spliceRampFrames and then stays silent — bounded,
// monotonic-magnitude decay, never a hard cliff and never a raw splice. It pushes a
// SHORT partial buffer into the tap ring (fewer than a full period), then drains a
// full period so the tail [got, period) takes the hold-ramp path.
func TestTransmittedMonitorUnderrunRampsFromLastFrame(t *testing.T) {
	e := configuredEngine()
	e.SetMonitorSource(MonitorSourceTransmitted)
	e.monitorActive.Store(true)
	e.SetMonitorGain(1)

	// Push a partial buffer: only `gotFrames` whole interleaved frames of a constant
	// seam value, far fewer than a period, so the pull returns a partial and the rest
	// of the monitor buffer must be hold-ramp + silence.
	const gotFrames = 4
	const seam = 0.5
	partial := make([]float32, gotFrames*channels)
	for i := range partial {
		partial[i] = seam
	}
	if n := e.tapRing.push(partial); n != len(partial) {
		t.Fatalf("failed to push partial seam: pushed %d of %d", n, len(partial))
	}

	monB := make([]byte, periodFrames*channels*4)
	e.monitorCallback(monB, nil, periodFrames)
	mon := bytesAsF32(monB)

	// Frames [0, gotFrames): the delivered seam value, exactly.
	for f := 0; f < gotFrames; f++ {
		for ch := 0; ch < channels; ch++ {
			if got := mon[f*channels+ch]; !approx(got, seam) {
				t.Fatalf("delivered frame %d ch %d = %v, want %v", f, ch, got, float32(seam))
			}
		}
	}

	// Frames [gotFrames, gotFrames+spliceRampFrames): a strictly DECREASING ramp from
	// near-seam down toward 0 (magnitude), starting below the held value.
	prev := float32(seam)
	for step := 0; step < spliceRampFrames; step++ {
		f := gotFrames + step
		v := mon[f*channels] // channel 0
		if v < 0 || v > prev+fEps {
			t.Fatalf("ramp frame %d = %v not in a monotonic non-increasing [0, %v] decay", f, v, prev)
		}
		prev = v
	}

	// After the ramp completes, the rest of the buffer must be pure silence.
	for f := gotFrames + spliceRampFrames; f < periodFrames; f++ {
		for ch := 0; ch < channels; ch++ {
			if got := mon[f*channels+ch]; got != 0 {
				t.Fatalf("post-ramp frame %d ch %d = %v, want 0 (silence)", f, ch, got)
			}
		}
	}
}

// TestClipsModeMonitorPlaysClipsOnly confirms the DEFAULT (clips) monitor path is
// untouched: with a queued clip and NO tap data, the monitor plays the clip scaled
// by the monitor gain (exactly the legacy behavior), independent of the tap ring.
func TestClipsModeMonitorPlaysClipsOnly(t *testing.T) {
	e := configuredEngine()
	e.SetMonitorSource(MonitorSourceClips)
	e.monitorActive.Store(true)
	e.SetMonitorGain(1)

	// Queue one steady clip directly onto the monitor cursor list via the pending
	// channel (as Trigger would), long enough to fill a whole period past the fade-in.
	clip := &catalog.Clip{PCM: flat(0.5, fadeFrames*4)}
	cur := &clipCursor{pcm: clip.PCM, pos: fadeFrames * channels} // past fade-in -> full gain
	e.monCursors = []*clipCursor{cur}

	monB := make([]byte, periodFrames*channels*4)
	e.monitorCallback(monB, nil, periodFrames)
	mon := bytesAsF32(monB)

	// The monitor must carry the clip (non-silent), proving clips mode still mixes
	// cursors and does not depend on the (empty) tap ring.
	if rms(mon) < 0.1 {
		t.Fatalf("clips-mode monitor is ~silent (RMS %v); the clip was not mixed", rms(mon))
	}
	// And the tap ring stays empty in clips mode (the monitor never drains it).
	if avail := e.tapRing.length(); avail != 0 {
		t.Fatalf("clips mode touched the tap ring (%d samples); want 0", avail)
	}
}

// TestTransmittedTapRingConcurrentNoRace drives the duplex callback (the tap-ring
// PRODUCER) and the monitor callback (the tap-ring CONSUMER) from two goroutines
// concurrently in transmitted mode, while a third goroutine flips the monitor
// source and triggers clips. Run with -race it proves the new tap-ring SPSC handoff
// and the monitorSourceBits atomic are data-race-free: exactly one goroutine pushes
// (duplex) and exactly one pulls (monitor), so no lock is needed, and the source
// flip is a lock-free atomic both callbacks read once per buffer. Each callback owns
// its own output buffer (the two devices never share a backing array in production).
func TestTransmittedTapRingConcurrentNoRace(t *testing.T) {
	e, id := newTestEngine(t)
	e.monitorActive.Store(true)
	e.SetMonitorSource(MonitorSourceTransmitted)
	e.SetMicMode("always") // keep the cable signal non-trivial through the tap

	stop := make(chan struct{})
	var cbWG sync.WaitGroup
	cbWG.Add(2)

	// Producer: the duplex callback taps its cable mix into the tap ring each buffer.
	go func() {
		defer cbWG.Done()
		buf := make([]byte, periodFrames*channels*4)
		mic := make([]byte, periodFrames*channels*4)
		for {
			select {
			case <-stop:
				return
			default:
				e.duplexCallback(buf, mic, periodFrames)
			}
		}
	}()
	// Consumer: the monitor callback drains the tap ring each buffer.
	go func() {
		defer cbWG.Done()
		buf := make([]byte, periodFrames*channels*4)
		for {
			select {
			case <-stop:
				return
			default:
				e.monitorCallback(buf, nil, periodFrames)
			}
		}
	}()

	// A third goroutine flips the monitor source and fires clips concurrently, so the
	// source atomic is written while both callbacks read it.
	var mutWG sync.WaitGroup
	mutWG.Add(1)
	go func() {
		defer mutWG.Done()
		for i := 0; i < 1000; i++ {
			if i%2 == 0 {
				e.SetMonitorSource(MonitorSourceTransmitted)
			} else {
				e.SetMonitorSource(MonitorSourceClips)
			}
			e.Trigger(id)
		}
	}()
	mutWG.Wait()
	close(stop)
	cbWG.Wait()
}
