// Package audio is the real-time engine: a malgo DUPLEX device that mixes the
// live mic with active clip cursors into the VB-CABLE input, plus an optional
// independent playback-only monitor device.
//
// Canonical format everywhere: float32 PCM, 48000 Hz, 2 channels interleaved.
// The data callback must be allocation-free and lock-light.
package audio

import (
	"errors"
	"sync"
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

	duplexDev  *malgo.Device
	monitorDev *malgo.Device

	// C-heap copies of the device IDs handed to miniaudio (malgo's
	// DeviceID.Pointer() calls C.CBytes and never frees it). We free them on
	// reconfigure / Stop so repeated Configure calls do not leak.
	micIDPtr   unsafe.Pointer
	cableIDPtr unsafe.Pointer
	monIDPtr   unsafe.Pointer

	mu          sync.Mutex
	cursors     []*clipCursor // duplex (mic+SFX) device cursors
	monCursors  []*clipCursor // monitor device cursors
	monitorDest *devices.Device

	started bool
}

// NewEngine creates an engine bound to a context and the decoded library.
func NewEngine(ctx *malgo.AllocatedContext, lib *catalog.Library) *Engine {
	return &Engine{ctx: ctx, lib: lib}
}

// Configure sets up the malgo DUPLEX device: Capture=mic, Playback=cable,
// FormatF32, 48k, 2ch, small period (128-256 frames). It does not start it.
func (e *Engine) Configure(mic devices.Device, cable devices.Device) error {
	if e.ctx == nil {
		return errors.New("audio: nil context")
	}
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
	e.micIDPtr = mic.RawID.Pointer()
	cfg.Capture.DeviceID = e.micIDPtr

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
	// Always tear the old monitor down first.
	if e.monitorDev != nil {
		e.monitorDev.Uninit()
		e.monitorDev = nil
	}
	freeCPtr(&e.monIDPtr)
	e.mu.Lock()
	e.monCursors = nil
	e.monitorDest = nil
	e.mu.Unlock()

	if dev == nil {
		return nil
	}
	if e.ctx == nil {
		return errors.New("audio: nil context")
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
	d := *dev
	e.mu.Lock()
	e.monitorDest = &d
	e.mu.Unlock()

	// Match the duplex device's running state so toggling at runtime works.
	if e.started {
		if err := mdev.Start(); err != nil {
			mdev.Uninit()
			e.monitorDev = nil
			freeCPtr(&e.monIDPtr)
			e.mu.Lock()
			e.monitorDest = nil
			e.mu.Unlock()
			return err
		}
	}
	return nil
}

// Start activates all configured devices.
func (e *Engine) Start() error {
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
	e.started = false
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

	e.mu.Lock()
	e.cursors = nil
	e.monCursors = nil
	e.monitorDest = nil
	e.mu.Unlock()
	return nil
}

// Trigger looks up the clip by ID and appends a clipCursor to the active list,
// allowing overlap. Both the duplex and (if present) monitor devices get their
// OWN cursor instance so their independent clocks never share state.
// Implements tray.Player.
func (e *Engine) Trigger(id string) {
	clip := e.lib.Get(id)
	if clip == nil || len(clip.PCM) == 0 {
		return
	}
	e.mu.Lock()
	e.cursors = append(e.cursors, &clipCursor{pcm: clip.PCM})
	if e.monitorDest != nil {
		e.monCursors = append(e.monCursors, &clipCursor{pcm: clip.PCM})
	}
	e.mu.Unlock()
}

// duplexCallback is the real-time mixer for the mic->cable path. It sums the
// live mic input with every active clip cursor, clamps, and writes the result
// to the playback buffer. Allocation-free; the mutex is held only for the brief
// snapshot/compaction of the cursor slice.
func (e *Engine) duplexCallback(pOutput, pInput []byte, frameCount uint32) {
	out := bytesAsF32(pOutput)
	mic := bytesAsF32(pInput)
	n := int(frameCount) * channels
	if n > len(out) {
		n = len(out)
	}

	e.mu.Lock()
	e.cursors = mixInto(out[:n], mic, e.cursors)
	e.mu.Unlock()
}

// monitorCallback is the real-time mixer for the local monitor device. It plays
// ONLY clip audio (no mic passthrough).
func (e *Engine) monitorCallback(pOutput, pInput []byte, frameCount uint32) {
	_ = pInput
	out := bytesAsF32(pOutput)
	n := int(frameCount) * channels
	if n > len(out) {
		n = len(out)
	}

	e.mu.Lock()
	e.monCursors = mixInto(out[:n], nil, e.monCursors)
	e.mu.Unlock()
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
