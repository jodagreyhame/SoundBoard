// Package audio is the real-time engine: a malgo DUPLEX device that mixes the
// live mic with active clip cursors into the VB-CABLE input, plus an optional
// independent playback-only monitor device.
//
// Canonical format everywhere: float32 PCM, 48000 Hz, 2 channels interleaved.
// The data callback must be allocation-free and lock-light.
//
// Concurrency model:
//   - Each RT data callback OWNS its cursor slice exclusively; no other
//     goroutine touches it, so mixing needs no lock.
//   - Trigger hands new clips to the callbacks over buffered channels (a
//     lock-free SPSC-style handoff). The callback drains its channel
//     non-blocking at the top of each buffer and appends to its own slice.
//   - ctrlMu serializes lifecycle operations (Configure/SetMonitor/Start/Stop)
//     and guards the device handles and C-pointer fields. The RT callbacks
//     never take ctrlMu.
package audio

import (
	"errors"
	"math"
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/gen2brain/malgo"

	"soundboard/internal/catalog"
	"soundboard/internal/devices"
)

const (
	// sampleRate / channels mirror the canonical catalog format.
	sampleRate = catalog.SampleRate
	channels   = catalog.Channels

	// periodFrames is a small period so triggered clips start with low latency.
	periodFrames = 192
	// periods is the number of periods miniaudio keeps queued.
	periods = 3

	// fadeFrames is the length of the linear fade-in (clip start) and fade-out
	// (clip end) ramp, ~4ms at 48kHz, to suppress clicks. Applied per frame so
	// both channels of an interleaved frame share the same gain.
	fadeFrames = 192

	// pendingCap bounds how many simultaneously-triggered clips can be queued
	// for a device between callback invocations. Generous so Trigger never
	// blocks in practice; if it somehow fills, Trigger drops the extra trigger
	// rather than block a non-RT goroutine.
	pendingCap = 256

	// maxGain is the upper clamp for the mic, master, monitor, and per-clip
	// gains. A little headroom above unity (1.5 ~= +3.5 dB) lets the user boost a
	// quiet mic or clip without permitting extreme values that would just slam
	// every sample into the [-1,1] clamp and clip badly.
	maxGain = 1.5
)

// clampGain constrains a linear gain to [0, maxGain]. Shared by every gain
// setter (mic, master, monitor, per-clip) so the RT callback never sees a NaN
// or an out-of-range value.
func clampGain(g float32) float32 {
	if g != g { // NaN
		return 0
	}
	if g < 0 {
		return 0
	}
	if g > maxGain {
		return maxGain
	}
	return g
}

// clipCursor tracks playback position of one active clip instance. Multiple
// cursors over the same clip allow overlap. pos is a sample index into the
// interleaved pcm (advances by `channels` per frame).
//
// gain is the linear amplitude applied to this cursor's samples on top of the
// fade ramp. It is captured at Trigger time as the PER-CLIP volume only — the
// soundboard "what others hear" (master) and "what you hear" (monitor) levels
// are NOT baked in here; they are applied per buffer in mixInto so the duplex
// and monitor paths can scale the SAME triggered clip independently. The ZERO
// value is treated as unity (1.0) by gainOf so cursors constructed without an
// explicit gain — including those in the existing mix tests — play at full
// level; a genuinely silent (per-clip zero) clip is never triggered.
type clipCursor struct {
	pcm  []float32
	pos  int
	gain float32
}

// done reports whether the cursor has played all of its samples.
func (c *clipCursor) done() bool { return c.pos >= len(c.pcm) }

// gainOf returns the effective per-cursor gain, mapping the zero value to unity
// so cursors created without an explicit gain are not silenced.
func gainOf(c *clipCursor) float32 {
	if c.gain == 0 {
		return 1
	}
	return c.gain
}

// pendingClip is the unit handed from Trigger to an RT callback: the decoded
// clip plus the per-instance PER-CLIP gain captured at trigger time. Carrying
// the gain on the handoff keeps the RT callback from reading any shared per-clip
// volume map. The master ("others hear") and monitor ("you hear") levels are
// applied per buffer in mixInto, not folded in here, so the two paths stay
// independent.
type pendingClip struct {
	clip *catalog.Clip
	gain float32
}

// float32bits / float32frombits store a float32 in an atomic.Uint32.
func float32bits(f float32) uint32     { return math.Float32bits(f) }
func float32frombits(u uint32) float32 { return math.Float32frombits(u) }

// Engine owns the duplex device, the optional monitor device, and the active
// cursor lists. Each device has its OWN cursor list (independent clocks).
type Engine struct {
	ctx *malgo.AllocatedContext
	lib *catalog.Library

	mic   devices.Device
	cable devices.Device

	// ctrlMu serializes lifecycle calls and guards the device handles plus the
	// C-pointer fields below. Never acquired by an RT callback.
	ctrlMu     sync.Mutex
	duplexDev  *malgo.Device
	monitorDev *malgo.Device

	// C-heap copies of the device IDs handed to miniaudio (malgo's
	// DeviceID.Pointer() calls C.CBytes and never frees it). We free them on
	// reconfigure / Stop so repeated Configure calls do not leak.
	micIDPtr   unsafe.Pointer
	cableIDPtr unsafe.Pointer
	monIDPtr   unsafe.Pointer

	started bool

	// monitorActive is read by Trigger (any goroutine) to decide whether to
	// also enqueue a cursor for the monitor device. Atomic so Trigger never
	// contends with the lifecycle lock.
	monitorActive atomic.Bool

	// micGainBits / masterGainBits / monitorGainBits hold three INDEPENDENT
	// levels as float32 bit patterns, written by the UI thread and read lock-free
	// by the RT callbacks so volume changes take effect without touching ctrlMu:
	//   - micGainBits     : live mic passthrough, applied in the DUPLEX callback.
	//   - masterGainBits   : the soundboard "what OTHERS hear in Discord" level,
	//                        applied to clips in the DUPLEX callback (-> cable).
	//   - monitorGainBits : the soundboard "what YOU hear" level, applied to clips
	//                        in the MONITOR callback (-> the user's headset).
	// master and monitor scale the SAME triggered clips but on different paths, so
	// they are applied per buffer in mixInto rather than baked in at trigger time.
	micGainBits     atomic.Uint32
	masterGainBits  atomic.Uint32
	monitorGainBits atomic.Uint32

	// pending / monPending hand clips from Trigger to the RT callbacks. Each
	// callback drains its own channel; the slices below are touched only by
	// their owning callback.
	pending    chan pendingClip
	monPending chan pendingClip

	// cursors / monCursors are owned exclusively by their RT callback.
	cursors    []*clipCursor // duplex (mic+SFX) device cursors
	monCursors []*clipCursor // monitor device cursors
}

// NewEngine creates an engine bound to a context and the decoded library.
// Mic, master, and monitor gains all default to unity (1.0) so a fresh engine
// passes the mic through unchanged and plays clips at full level on both the
// duplex (-> Discord) and monitor (-> the user's headset) paths.
func NewEngine(ctx *malgo.AllocatedContext, lib *catalog.Library) *Engine {
	e := &Engine{
		ctx:        ctx,
		lib:        lib,
		pending:    make(chan pendingClip, pendingCap),
		monPending: make(chan pendingClip, pendingCap),
	}
	e.micGainBits.Store(float32bits(1))
	e.masterGainBits.Store(float32bits(1))
	e.monitorGainBits.Store(float32bits(1))
	return e
}

// SetMicGain sets the live mic-passthrough gain (linear, 1.0 = unchanged).
// The value is clamped to [0, maxGain]. Safe to call from any goroutine; the RT
// callback picks the new value up on its next buffer.
func (e *Engine) SetMicGain(g float32) {
	e.micGainBits.Store(float32bits(clampGain(g)))
}

// SetMasterGain sets the soundboard "what OTHERS hear in Discord" gain (linear,
// 1.0 = unchanged) applied to every clip on the DUPLEX path (-> cable ->
// Discord) on top of its per-clip volume. The value is clamped to [0, maxGain].
// Safe to call from any goroutine. Independent of the monitor gain, so muting
// what Discord hears does not silence what the user hears locally.
func (e *Engine) SetMasterGain(g float32) {
	e.masterGainBits.Store(float32bits(clampGain(g)))
}

// SetMonitorGain sets the soundboard "what YOU hear" gain (linear, 1.0 =
// unchanged) applied to every clip on the MONITOR path (-> the user's headset)
// on top of its per-clip volume. The value is clamped to [0, maxGain]. Safe to
// call from any goroutine. Independent of the master gain, so the user can lower
// their own local monitor without changing what Discord transmits.
func (e *Engine) SetMonitorGain(g float32) {
	e.monitorGainBits.Store(float32bits(clampGain(g)))
}

// micGain / masterGain / monitorGain read the current gains lock-free.
func (e *Engine) micGain() float32     { return float32frombits(e.micGainBits.Load()) }
func (e *Engine) masterGain() float32  { return float32frombits(e.masterGainBits.Load()) }
func (e *Engine) monitorGain() float32 { return float32frombits(e.monitorGainBits.Load()) }

// Configure sets up the malgo DUPLEX device: Capture=mic, Playback=cable,
// FormatF32, 48k, 2ch, small period (128-256 frames). It does not start it.
// A zero-value cable (e.g. VB-CABLE absent) is rejected so we never hand
// miniaudio a non-nil all-zero device id, which WASAPI cannot match.
func (e *Engine) Configure(mic devices.Device, cable devices.Device) error {
	if e.ctx == nil {
		return errors.New("audio: nil context")
	}
	if cable.Name == "" {
		return errors.New("audio: no cable playback device")
	}

	e.ctrlMu.Lock()
	defer e.ctrlMu.Unlock()

	// Tear down any previously configured duplex device first.
	if e.duplexDev != nil {
		e.duplexDev.Uninit()
		e.duplexDev = nil
	}
	freeCPtr(&e.micIDPtr)
	freeCPtr(&e.cableIDPtr)
	// The Uninit above synchronously waits for the duplex callback to finish, so
	// no callback owns e.cursors here. Drop the leftover cursors and drain the
	// pending queue so a re-Configure starts from a clean cursor list instead of
	// resuming old cursors from their stale positions on the new device.
	e.cursors = nil
	drainPending(e.pending)

	e.mic = mic
	e.cable = cable

	cfg := malgo.DefaultDeviceConfig(malgo.Duplex)
	cfg.SampleRate = sampleRate
	cfg.PeriodSizeInFrames = periodFrames
	cfg.Periods = periods
	cfg.PerformanceProfile = malgo.LowLatency

	cfg.Capture.Format = malgo.FormatF32
	cfg.Capture.Channels = channels
	// A zero-value mic (no capture device) means "default mic": leave the
	// capture DeviceID nil rather than handing over an all-zero id.
	if mic.Name != "" {
		e.micIDPtr = mic.RawID.Pointer()
		cfg.Capture.DeviceID = e.micIDPtr
	}

	cfg.Playback.Format = malgo.FormatF32
	cfg.Playback.Channels = channels
	e.cableIDPtr = cable.RawID.Pointer()
	cfg.Playback.DeviceID = e.cableIDPtr

	dev, err := malgo.InitDevice(e.ctx.Context, cfg, malgo.DeviceCallbacks{
		Data: e.duplexCallback,
		Stop: func() {},
	})
	if err != nil {
		freeCPtr(&e.micIDPtr)
		freeCPtr(&e.cableIDPtr)
		return err
	}
	e.duplexDev = dev
	return nil
}

// SetMonitor enables or disables the local monitor output. nil disables; a
// non-nil dev opens a SECOND playback-only device with its own cursor list
// (independent clock) that plays clip PCM only (no mic).
func (e *Engine) SetMonitor(dev *devices.Device) error {
	e.ctrlMu.Lock()
	defer e.ctrlMu.Unlock()

	// Always tear the old monitor down first.
	e.monitorActive.Store(false)
	if e.monitorDev != nil {
		e.monitorDev.Uninit()
		e.monitorDev = nil
	}
	freeCPtr(&e.monIDPtr)
	// Drain any clips queued for the now-defunct monitor AND drop its cursor
	// slice so a re-enable starts clean. The Uninit above synchronously waits for
	// the monitor callback to finish, so no callback owns monCursors here; without
	// this, a re-enabled monitor would resume the old cursors from their stale
	// positions, replaying partial clips as an audible glitch.
	drainPending(e.monPending)
	e.monCursors = nil

	if dev == nil {
		return nil
	}
	if e.ctx == nil {
		return errors.New("audio: nil context")
	}
	if dev.Name == "" {
		return errors.New("audio: no monitor playback device")
	}

	cfg := malgo.DefaultDeviceConfig(malgo.Playback)
	cfg.SampleRate = sampleRate
	cfg.PeriodSizeInFrames = periodFrames
	cfg.Periods = periods
	cfg.PerformanceProfile = malgo.LowLatency
	cfg.Playback.Format = malgo.FormatF32
	cfg.Playback.Channels = channels
	e.monIDPtr = dev.RawID.Pointer()
	cfg.Playback.DeviceID = e.monIDPtr

	mdev, err := malgo.InitDevice(e.ctx.Context, cfg, malgo.DeviceCallbacks{
		Data: e.monitorCallback,
		Stop: func() {},
	})
	if err != nil {
		freeCPtr(&e.monIDPtr)
		return err
	}
	e.monitorDev = mdev

	// Match the duplex device's running state so toggling at runtime works.
	if e.started {
		if err := mdev.Start(); err != nil {
			mdev.Uninit()
			e.monitorDev = nil
			freeCPtr(&e.monIDPtr)
			return err
		}
	}
	e.monitorActive.Store(true)
	return nil
}

// Start activates all configured devices.
func (e *Engine) Start() error {
	e.ctrlMu.Lock()
	defer e.ctrlMu.Unlock()

	if e.duplexDev == nil {
		return errors.New("audio: not configured")
	}
	if err := e.duplexDev.Start(); err != nil {
		return err
	}
	if e.monitorDev != nil {
		if err := e.monitorDev.Start(); err != nil {
			_ = e.duplexDev.Stop()
			return err
		}
	}
	e.started = true
	return nil
}

// Stop uninitializes/stops all devices without leaking.
func (e *Engine) Stop() error {
	e.ctrlMu.Lock()
	defer e.ctrlMu.Unlock()

	e.started = false
	e.monitorActive.Store(false)
	if e.duplexDev != nil {
		e.duplexDev.Uninit()
		e.duplexDev = nil
	}
	if e.monitorDev != nil {
		e.monitorDev.Uninit()
		e.monitorDev = nil
	}
	freeCPtr(&e.micIDPtr)
	freeCPtr(&e.cableIDPtr)
	freeCPtr(&e.monIDPtr)

	// Devices are uninitialized, so no callback is running: it is safe to drop
	// the (callback-owned) cursor slices and drain the handoff channels here.
	e.cursors = nil
	e.monCursors = nil
	drainPending(e.pending)
	drainPending(e.monPending)
	return nil
}

// Trigger plays the clip at unit per-clip gain (scaled by the master/monitor
// gains per path). It is shorthand for TriggerGain(id, 1). Implements the
// ui.Player default path.
func (e *Engine) Trigger(id string) { e.TriggerGain(id, 1) }

// TriggerGain looks up the clip by ID and hands it to the RT callbacks over the
// pending channels with the PER-CLIP gain captured now, so later per-clip
// changes do not retroactively alter sounds already in flight. The soundboard
// master ("others hear") and monitor ("you hear") levels are NOT folded in here
// — each callback applies its own level per buffer in mixInto, so muting one
// path never silences the other for a clip already in flight. Both the duplex
// and (if active) monitor devices get their OWN cursor instance so their
// independent clocks never share state. The handoff is non-blocking: if a queue
// is somehow full, the extra trigger is dropped rather than blocking this
// (non-RT) goroutine. Implements ui.Player.
func (e *Engine) TriggerGain(id string, gain float32) {
	clip := e.lib.Get(id)
	if clip == nil {
		return
	}
	// Decode on first play (off the RT path, on this goroutine). EnsureDecoded
	// fully populates clip.PCM before we hand the clip to a callback over the
	// channel, so the callback's later read is safely published via the channel.
	if _, err := e.lib.EnsureDecoded(clip); err != nil || len(clip.PCM) == 0 {
		return
	}
	// Capture only the per-clip gain, clamped to the same [0, maxGain] range as
	// the mic/master/monitor gains. The master and monitor levels are applied per
	// buffer in mixInto, so they are deliberately NOT folded in here.
	g := clampGain(gain)
	// A zero PER-CLIP gain is pure silence on every path: drop the trigger instead
	// of enqueuing it. This both saves a mix slot and upholds gainOf's "zero ==
	// unity" convention — that sentinel is only safe because a genuinely-silent
	// clip is never handed to a callback. A muted master or monitor must NOT drop
	// the trigger here: the OTHER path may still be audible, and each path applies
	// its own (possibly zero) level in mixInto.
	if g == 0 {
		return
	}
	pc := pendingClip{clip: clip, gain: g}
	select {
	case e.pending <- pc:
	default:
	}
	if e.monitorActive.Load() {
		select {
		case e.monPending <- pc:
		default:
		}
	}
}

// duplexCallback is the real-time mixer for the mic->cable path (what Discord
// hears). It drains any newly-triggered clips into its own cursor slice, then
// sums the live mic input with every active clip cursor — scaling the clips by
// the soundboard MASTER ("others hear") gain — clamps, and writes the result to
// the playback buffer. Allocation-free in steady state and lock-free (no mutex).
// The master gain is read once per buffer via a single atomic load.
func (e *Engine) duplexCallback(pOutput, pInput []byte, frameCount uint32) {
	e.cursors = drainInto(e.pending, e.cursors)

	out := bytesAsF32(pOutput)
	mic := bytesAsF32(pInput)
	n := int(frameCount) * channels
	if n > len(out) {
		n = len(out)
	}
	// Apply the live mic-passthrough gain in place on the input view before
	// mixing. miniaudio hands us a fresh input buffer each call, so scaling it
	// here is safe and allocation-free. Skip the loop at unity to stay cheap.
	if g := e.micGain(); g != 1 {
		for i := range mic {
			mic[i] *= g
		}
	}
	e.cursors = mixInto(out[:n], mic, e.cursors, e.masterGain())
}

// monitorCallback is the real-time mixer for the local monitor device (what the
// USER hears). It plays ONLY clip audio (no mic passthrough), scaling the clips
// by the INDEPENDENT monitor ("you hear") gain, read once per buffer via a
// single atomic load. Allocation-free in steady state and lock-free.
func (e *Engine) monitorCallback(pOutput, pInput []byte, frameCount uint32) {
	_ = pInput
	e.monCursors = drainInto(e.monPending, e.monCursors)

	out := bytesAsF32(pOutput)
	n := int(frameCount) * channels
	if n > len(out) {
		n = len(out)
	}
	e.monCursors = mixInto(out[:n], nil, e.monCursors, e.monitorGain())
}

// drainInto pops every clip currently queued in ch and appends a fresh cursor
// for each onto cursors, returning the grown slice. It blocks on nothing (a
// non-blocking select loop). Any append growth happens on the callback's own
// slice, never under a lock shared with the producer.
func drainInto(ch <-chan pendingClip, cursors []*clipCursor) []*clipCursor {
	for {
		select {
		case pc := <-ch:
			cursors = append(cursors, &clipCursor{pcm: pc.clip.PCM, gain: pc.gain})
		default:
			return cursors
		}
	}
}

// drainPending empties a pending channel without consuming into cursors. Used
// when a device is being torn down or reconfigured.
func drainPending(ch chan pendingClip) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

// mixInto writes one callback buffer worth of interleaved float32 samples into
// out. mic (may be nil) is summed in as passthrough; each cursor contributes
// its remaining samples scaled by its per-clip gain, the per-buffer soundboard
// level (master for the duplex path, monitor for the monitor path), and a short
// linear fade-in at the start / fade-out at the end. The accumulator is clamped
// to [-1,1] per sample. Finished cursors are removed and the surviving slice is
// returned (reusing the caller's backing array so the hot path does not
// allocate). Cursors still advance (and retire) even when soundboard==0, so a
// muted path drains its queue rather than stalling it.
//
// soundboard is passed in (not read from the engine) so this function stays PURE
// (no device/context access) and unit-testable without real audio hardware: the
// caller decides which independent level applies to this buffer.
func mixInto(out, mic []float32, cursors []*clipCursor, soundboard float32) []*clipCursor {
	// Lay down the mic passthrough (or silence) first.
	for i := range out {
		if i < len(mic) {
			out[i] = mic[i]
		} else {
			out[i] = 0
		}
	}

	frames := len(out) / channels

	for _, c := range cursors {
		cg := gainOf(c) * soundboard
		for f := 0; f < frames; f++ {
			if c.done() {
				break
			}
			g := fadeGain(c.pos, len(c.pcm)) * cg
			base := f * channels
			for ch := 0; ch < channels; ch++ {
				if c.pos >= len(c.pcm) {
					break
				}
				out[base+ch] += c.pcm[c.pos] * g
				c.pos++
			}
		}
	}

	// Clamp the summed result.
	for i := range out {
		if out[i] > 1 {
			out[i] = 1
		} else if out[i] < -1 {
			out[i] = -1
		}
	}

	// Compact: drop finished cursors in place.
	kept := cursors[:0]
	for _, c := range cursors {
		if !c.done() {
			kept = append(kept, c)
		}
	}
	return kept
}

// fadeGain returns the linear fade gain in [0,1] for the sample at interleaved
// index pos within a clip of total length samples. It ramps up over the first
// fadeFrames frames and down over the final fadeFrames frames. For very short
// clips the two ramps overlap; the returned gain is the min so it still starts
// and ends near silence.
func fadeGain(pos, total int) float32 {
	frame := pos / channels
	totalFrames := total / channels
	g := float32(1)
	if frame < fadeFrames {
		in := float32(frame+1) / float32(fadeFrames)
		if in < g {
			g = in
		}
	}
	if rem := totalFrames - frame; rem <= fadeFrames {
		out := float32(rem) / float32(fadeFrames)
		if out < g {
			g = out
		}
	}
	if g < 0 {
		g = 0
	}
	return g
}

// bytesAsF32 reinterprets a miniaudio []byte sample buffer as []float32 without
// copying. The backing buffer is FormatF32 so its byte length is a multiple of 4.
func bytesAsF32(b []byte) []float32 {
	if len(b) < 4 {
		return nil
	}
	return unsafe.Slice((*float32)(unsafe.Pointer(&b[0])), len(b)/4)
}

// freeCPtr releases a C.CBytes allocation produced by DeviceID.Pointer() and
// nils the holder. Safe to call on a nil holder or nil pointer.
func freeCPtr(p *unsafe.Pointer) {
	if p == nil || *p == nil {
		return
	}
	cFree(*p)
	*p = nil
}
