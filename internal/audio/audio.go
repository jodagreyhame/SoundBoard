// Package audio is the real-time engine: a malgo DUPLEX device that mixes the
// live mic with active clip cursors into the VB-CABLE input, plus an optional
// independent playback-only monitor device.
//
// Canonical format everywhere: float32 PCM, 48000 Hz, 2 channels interleaved.
// The data callback must be allocation-free and lock-light.
package audio

import (
	"sync"

	"github.com/gen2brain/malgo"

	"soundboard/internal/catalog"
	"soundboard/internal/devices"
)

// clipCursor tracks playback position of one active clip instance. Multiple
// cursors over the same clip allow overlap.
type clipCursor struct {
	pcm []float32
	pos int
}

// Engine owns the duplex device, the optional monitor device, and the active
// cursor lists. Each device has its OWN cursor list (independent clocks).
type Engine struct {
	ctx *malgo.AllocatedContext
	lib *catalog.Library

	mic   devices.Device
	cable devices.Device

	duplexDev  *malgo.Device
	monitorDev *malgo.Device

	mu          sync.Mutex
	cursors     []*clipCursor // duplex (mic+SFX) device cursors
	monCursors  []*clipCursor // monitor device cursors
	monitorDest *devices.Device
}

// NewEngine creates an engine bound to a context and the decoded library.
func NewEngine(ctx *malgo.AllocatedContext, lib *catalog.Library) *Engine {
	panic("todo")
}

// Configure sets up the malgo DUPLEX device: Capture=mic, Playback=cable,
// FormatF32, 48k, 2ch, small period (128-256 frames). It does not start it.
func (e *Engine) Configure(mic devices.Device, cable devices.Device) error {
	panic("todo")
}

// SetMonitor enables or disables the local monitor output. nil disables; a
// non-nil dev opens a SECOND playback-only device with its own cursor list
// (independent clock) that plays clip PCM only (no mic).
func (e *Engine) SetMonitor(dev *devices.Device) error {
	panic("todo")
}

// Start activates all configured devices.
func (e *Engine) Start() error {
	panic("todo")
}

// Stop uninitializes/stops all devices without leaking.
func (e *Engine) Stop() error {
	panic("todo")
}

// Trigger looks up the clip by ID and appends a clipCursor to the active list,
// allowing overlap. The real-time callback sums mic + active cursors, clamps to
// [-1,1], and applies a short fade in/out. Implements tray.Player.
func (e *Engine) Trigger(id string) {
	panic("todo")
}
