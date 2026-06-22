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
)

// clipCursor tracks playback position of one active clip instance. Multiple
// cursors over the same clip allow overlap. pos is a sample index into the
// interleaved pcm (advances by `channels` per frame).
type clipCursor struct {
	pcm []float32
	pos int
}

// done reports whether the cursor has played all of its samples.
func (c *clipCursor) done() bool { return c.pos >= len(c.pcm) }

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

	// pending / monPending hand clips from Trigger to the RT callbacks. Each
	// callback drains its own channel; the slices below are touched only by
	// their owning callback.
	pending    chan *catalog.Clip
	monPending chan *catalog.Clip

	// cursors / monCursors are owned exclusively by their RT callback.
	cursors    []*clipCursor // duplex (mic+SFX) device cursors
	monCursors []*clipCursor // monitor device cursors
}

// NewEngine creates an engine bound to a context and the decoded library.
func NewEngine(ctx *malgo.AllocatedContext, lib *catalog.Library) *Engine {
	return &Engine{
		ctx:        ctx,
		lib:        lib,
		pending:    make(chan *catalog.Clip, pendingCap),
		monPending: make(chan *catalog.Clip, pendingCap),
	}
}

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
	// Drain any clips queued for the now-defunct monitor so a re-enable starts
	// clean. (The monitor callback is no longer running to drain them.)
	drainPending(e.monPending)

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

// Trigger looks up the clip by ID and hands it to the RT callbacks over the
// pending channels, allowing overlap. Both the duplex and (if active) monitor
// devices get their OWN cursor instance so their independent clocks never share
// state. The handoff is non-blocking: if a queue is somehow full, the extra
// trigger is dropped rather than blocking this (non-RT) goroutine.
// Implements tray.Player.
func (e *Engine) Trigger(id string) {
	clip := e.lib.Get(id)
	if clip == nil || len(clip.PCM) == 0 {
		return
	}
	select {
	case e.pending <- clip:
	default:
	}
	if e.monitorActive.Load() {
		select {
		case e.monPending <- clip:
		default:
		}
	}
}

// duplexCallback is the real-time mixer for the mic->cable path. It drains any
// newly-triggered clips into its own cursor slice, then sums the live mic input
// with every active clip cursor, clamps, and writes the result to the playback
// buffer. Allocation-free in steady state and lock-free (no mutex).
func (e *Engine) duplexCallback(pOutput, pInput []byte, frameCount uint32) {
	e.cursors = drainInto(e.pending, e.cursors)

	out := bytesAsF32(pOutput)
	mic := bytesAsF32(pInput)
	n := int(frameCount) * channels
	if n > len(out) {
		n = len(out)
	}
	e.cursors = mixInto(out[:n], mic, e.cursors)
}

// monitorCallback is the real-time mixer for the local monitor device. It plays
// ONLY clip audio (no mic passthrough).
func (e *Engine) monitorCallback(pOutput, pInput []byte, frameCount uint32) {
	_ = pInput
	e.monCursors = drainInto(e.monPending, e.monCursors)

	out := bytesAsF32(pOutput)
	n := int(frameCount) * channels
	if n > len(out) {
		n = len(out)
	}
	e.monCursors = mixInto(out[:n], nil, e.monCursors)
}

// drainInto pops every clip currently queued in ch and appends a fresh cursor
// for each onto cursors, returning the grown slice. It blocks on nothing (a
// non-blocking select loop). Any append growth happens on the callback's own
// slice, never under a lock shared with the producer.
func drainInto(ch <-chan *catalog.Clip, cursors []*clipCursor) []*clipCursor {
	for {
		select {
		case clip := <-ch:
			cursors = append(cursors, &clipCursor{pcm: clip.PCM})
		default:
			return cursors
		}
	}
}

// drainPending empties a pending channel without consuming into cursors. Used
// when a device is being torn down or reconfigured.
func drainPending(ch chan *catalog.Clip) {
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
// its remaining samples with a short linear fade-in at the start and fade-out
// at the end. The accumulator is clamped to [-1,1] per sample. Finished cursors
// are removed and the surviving slice is returned (reusing the caller's backing
// array so the hot path does not allocate).
//
// This function is pure (no device/context access) so it is unit-testable
// without real audio hardware.
func mixInto(out, mic []float32, cursors []*clipCursor) []*clipCursor {
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
		for f := 0; f < frames; f++ {
			if c.done() {
				break
			}
			g := fadeGain(c.pos, len(c.pcm))
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
