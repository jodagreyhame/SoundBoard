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

	// tapRingCapPeriods sizes the confidence-monitor TAP ring in whole periods of
	// interleaved-stereo samples. The duplex and monitor devices both run at
	// periodFrames, so in steady state the ring oscillates around one period of
	// lead (set by the Start() prime); a handful of periods of slack absorbs the
	// scheduling jitter between the two independent device clocks without ever
	// forcing the producer to drop a period. 8 periods (~32 ms of stereo headroom)
	// matches the in/out rings' generosity.
	tapRingCapPeriods = 8

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

// monSource* are the RT-friendly integer encodings of the two monitor SOURCE
// modes — what the local monitor device (the user's headset) plays. The monitor
// callback reads monitorSourceBits (an atomic.Int32) once per buffer instead of
// comparing strings on the audio thread. SetMonitorSource maps a config string to
// one of these; an unknown string falls back to monSourceClips.
//
//   - monSourceClips      : the DEFAULT (and historical) behavior — the monitor
//     plays ONLY the triggered clips (no mic). The user hears clean clips plus
//     their own natural voice acoustically; the processed mic is NOT echoed back.
//   - monSourceTransmitted: the CONFIDENCE-MONITOR mode — the monitor plays the
//     EXACT signal written to the cable (processedMic + clips), so the user can
//     audit what Discord actually receives. The duplex callback taps its final
//     cable mix into a dedicated SPSC ring and the monitor callback drains it.
const (
	monSourceClips       int32 = 0 // monitor plays clips only (default, legacy)
	monSourceTransmitted int32 = 1 // monitor plays the exact cable-bound mix
)

// MonitorSourceClips / MonitorSourceTransmitted are the config/UI string values
// that map to the encodings above. Exported so config, the App binding, and the
// UI share one spelling and never drift.
const (
	MonitorSourceClips       = "clips"
	MonitorSourceTransmitted = "transmitted"
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
//
// idx is the clipRegistry index of the clip this cursor is playing (see
// nowplaying.go). It is captured at Trigger time so the RT callback can publish
// WHICH clips are active — and honour a per-clip stop — using only integer
// comparisons, never a string or a map lookup on the audio thread.
type clipCursor struct {
	pcm  []float32
	pos  int
	gain float32
	idx  int32
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
	idx  int32 // clipRegistry index, resolved off the RT path in TriggerGain
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

	// stopFlag / monStopFlag are one-shot "stop everything" signals raised by
	// StopAll (a non-RT goroutine) and consumed by the owning RT callback. Each
	// callback CompareAndSwaps its flag from true->false at the top of a buffer
	// and, on success, drops every active cursor AND discards anything still
	// queued in its pending channel, so playback halts within one buffer without
	// the callback ever taking a lock or allocating. Atomic so StopAll never
	// contends with the lifecycle lock or the RT path.
	stopFlag    atomic.Bool
	monStopFlag atomic.Bool

	// stopClipIdx / monStopClipIdx are the PER-CLIP analogue of the flags above,
	// raised by StopClip (the UI's per-chip ✕) and consumed by the owning RT
	// callback. The value is a clipRegistry index PLUS ONE so that zero means "no
	// request"; each callback Swaps its own field to 0 at the top of a buffer and,
	// on a non-zero value, drops every cursor whose idx matches — an integer
	// compare and an in-place compaction, so still allocation-free and lock-free.
	// Only one request is held at a time: if two arrive between buffers the later
	// wins and the earlier clip keeps playing, which the UI reports truthfully
	// because the now-playing set is derived from the cursors, not from the click.
	stopClipIdx    atomic.Int32
	monStopClipIdx atomic.Int32

	// playing is the RT-published set of currently-playing clips (nowplaying.go).
	// The duplex callback republishes it every buffer; the App's events loop polls
	// it at ~20 Hz and emits a "nowPlaying" event when the set changes. registry
	// interns clip IDs to the int32 indices stored in it, so the audio thread only
	// ever deals in integers.
	playing  playSet
	registry clipRegistry

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

	// monitorSourceBits selects WHAT the monitor device plays — the legacy
	// clips-only mix (monSourceClips, default) or the exact cable-bound transmit
	// mix (monSourceTransmitted, the confidence monitor). Written by the UI thread
	// via SetMonitorSource and read lock-free by BOTH RT callbacks once per buffer:
	// the duplex callback consults it to decide whether to TAP its final cable mix
	// into the tap ring, and the monitor callback consults it to decide whether to
	// DRAIN that ring instead of mixing clips. Atomic so a mode change never touches
	// ctrlMu or contends with the RT path.
	monitorSourceBits atomic.Int32

	// --- Mic-path processing-suite controls (see processing.go) ---
	//
	// All are atomic so any goroutine (UI, hotkeys) may set them and the RT
	// callback / DSP worker reads each once per buffer or frame via a single
	// atomic load — no lock, no allocation. They configure ONLY the live-mic
	// chain (gain -> mono -> HPF -> denoise -> AGC -> gate -> stereo); triggered
	// soundboard clips are mixed in AFTER the chain and are never affected.
	//
	// These fields are declared here but, in this foundation phase, are not yet
	// read by duplexCallback's DSP — the heavy worker + ring buffers land in
	// phase 2. The setters and GateLevel are live now so config, UI, and hotkeys
	// can be wired against stable signatures.
	micModeBits   atomic.Int32  // gate mode enum (see micMode* consts)
	gateSensBits  atomic.Uint32 // gate threshold float32 bits, [0,1]
	pttDown       atomic.Bool   // true while the PTT hotkey is held
	agcOn         atomic.Bool   // run the RMS-target leveler when true
	duckingOn     atomic.Bool   // duck clips under an open mic gate when true
	gateLevelBits atomic.Uint32 // published gate-open level float32 bits, [0,1]

	// --- Discord Voice & Video parity control surface (the breathing fix) ---
	//
	// Like the toggles above, these are atomic so any goroutine (UI, hotkeys) may
	// set them and the DSP worker reads each once per frame via a single atomic
	// load — no lock, no allocation on (or near) the audio thread.
	//
	// nsTierBits supersedes the old noiseSuppressOn bool: noise suppression is now a
	// TIER (none/standard/high/strong, see nsTier* consts). The worker maps it to the
	// APM noise-suppression submodule (and, for "strong", to RNNoise). The other four
	// mirror the new config.AudioProcessing parity fields.
	nsTierBits      atomic.Int32  // NS tier enum (see nsTier* consts)
	echoCancelOn    atomic.Bool   // APM echo cancellation (parity; inert w/o render ref)
	advancedVADOn   atomic.Bool   // VAD gate uses the RNNoise speech probability when true
	autoSensOn      atomic.Bool   // automatic input sensitivity (else the manual slider)
	attenAmountBits atomic.Uint32 // ducking depth float32 bits, [0,1]

	// pending / monPending hand clips from Trigger to the RT callbacks. Each
	// callback drains its own channel; the slices below are touched only by
	// their owning callback.
	pending    chan pendingClip
	monPending chan pendingClip

	// cursors / monCursors are owned exclusively by their RT callback.
	cursors    []*clipCursor // duplex (mic+SFX) device cursors
	monCursors []*clipCursor // monitor device cursors

	// --- Mic-path DSP plumbing (see worker.go / dsp.go) ---
	//
	// inRing carries DOWNMIXED mono mic samples from duplexCallback (producer) to
	// the worker (consumer); outRing carries PROCESSED mono back from the worker
	// (producer) to duplexCallback (consumer). Both are lock-free SPSC rings,
	// allocated once in Configure and reset on teardown so the RT path never
	// allocates. The worker is launched in Start and stopped in Stop.
	inRing  *ringSPSC
	outRing *ringSPSC
	worker  *micWorker

	// tapRing carries the EXACT interleaved-stereo cable mix from duplexCallback
	// (producer) to monitorCallback (consumer) when MonitorSource is "transmitted".
	// It is a lock-free SPSC ring of INTERLEAVED samples (NOT mono like in/outRing):
	// duplexCallback pushes its whole final `out` buffer each period, monitorCallback
	// pulls one period out. Allocated once in Configure, primed in Start (one period
	// of lead, like outRing, to absorb the duplex<->monitor clock drift), and reset
	// on teardown so the RT path never allocates. In "clips" mode it is left idle.
	tapRing *ringSPSC

	// monTapHold is the monitor callback's "hold-last" seam value per channel: on a
	// tap-ring underrun it ramps the last delivered frame down to silence rather than
	// splicing a hard gap, applying the buzz-fix lesson (no raw splice, click-free).
	// Callback-owned (monitorCallback only); sized to one interleaved frame.
	monTapHold []float32

	// duplexCallback-owned scratch, preallocated in Configure so the RT path is
	// allocation-free: monIn holds the downmixed mono mic for a buffer, monOut the
	// processed mono pulled back from the worker. Sized to one period of frames;
	// only ever touched by the duplex callback.
	monIn  []float32
	monOut []float32

	// duckEnv is the duplex callback's ducking envelope follower (clip<->mic
	// cross-gain), callback-owned (advanced once per buffer in duckedMaster).
	duckEnv float32
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
	// Monitor source defaults to clips-only so a fresh engine reproduces the exact
	// historical monitor behavior (clean clips on the headset, natural voice
	// acoustically); main overrides it from saved settings via SetMonitorSource.
	e.monitorSourceBits.Store(monSourceClips)
	// Mic-processing defaults mirror config.AudioProcessing's normalize: gate by
	// voice activity at the default sensitivity, every optional feature off. main
	// overrides these from saved settings at startup via the Set* methods.
	e.micModeBits.Store(micModeVAD)
	e.gateSensBits.Store(float32bits(defaultGateSensitivity))
	// Parity control-surface engine defaults. These are the BARE-engine baseline;
	// main overrides every one from saved settings via applyProcessingW at startup.
	// The tier starts at "none" (so a fresh engine reports noiseSuppression()==false,
	// the historical baseline) and advanced-VAD / auto-sensitivity start OFF so a bare
	// engine keeps using the energy gate until main seeds the config defaults. The
	// attenuation depth starts at the historical 0.65 so ducking behaves exactly as
	// the pre-parity build until main applies the saved amount.
	e.nsTierBits.Store(nsTierNone)
	e.attenAmountBits.Store(float32bits(defaultAttenAmount))
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

// SetMonitorSource selects WHAT the local monitor device plays from a config/UI
// string: MonitorSourceClips ("clips", the default) or MonitorSourceTransmitted
// ("transmitted", the confidence monitor). An empty or unrecognized value is
// treated as "clips". Safe to call from any goroutine; both RT callbacks pick the
// new mode up on their next buffer via a single atomic load. The string is mapped
// to an int encoding here so the audio thread never compares strings.
//
// "transmitted" makes the monitor play the EXACT signal sent to the cable
// (processedMic + clips) so the user can audit the transmitted quality. It applies
// even at unity monitor gain and is independent of the monitor on/off and monitor
// gain controls, which still take effect (the gain scales the drained tap mix).
func (e *Engine) SetMonitorSource(mode string) {
	var m int32
	switch mode {
	case MonitorSourceTransmitted:
		m = monSourceTransmitted
	default: // MonitorSourceClips and anything unknown
		m = monSourceClips
	}
	e.monitorSourceBits.Store(m)
}

// monitorSource reads the current monitor-source encoding lock-free (RT helper).
func (e *Engine) monitorSource() int32 { return e.monitorSourceBits.Load() }

// monitorTransmitting reports whether the monitor is in "transmitted" mode, read
// lock-free by both RT callbacks (duplex taps, monitor drains) once per buffer.
func (e *Engine) monitorTransmitting() bool { return e.monitorSource() == monSourceTransmitted }

// GetMonitorSource returns the current monitor source as its config/UI string
// ("clips" | "transmitted") so the App snapshot (GetState) can report it. Safe to
// call from any goroutine; a single atomic load.
func (e *Engine) GetMonitorSource() string {
	if e.monitorSource() == monSourceTransmitted {
		return MonitorSourceTransmitted
	}
	return MonitorSourceClips
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

	// Tear down any previously configured duplex device first. Uninit synchronously
	// waits for the duplex callback to finish, so afterwards NO callback is pushing
	// or pulling the rings.
	if e.duplexDev != nil {
		e.duplexDev.Uninit()
		e.duplexDev = nil
	}
	freeCPtr(&e.micIDPtr)
	freeCPtr(&e.cableIDPtr)

	// Reconfigure path: a worker may still be running from a previous Start() if
	// Configure is called without an intervening Stop(). It reads e.inRing/e.outRing
	// dynamically, so if we reallocate the rings below while it lives, the old worker
	// would start consuming the NEW ring AND the next Start() would launch a SECOND
	// worker on the same rings — two consumers on one SPSC ring, which corrupts the
	// read index and leaks the old goroutine. Stop it now (the device is already
	// uninitialized, so the worker is not racing a live callback) BEFORE reallocating.
	// 'started' is cleared so the device must be Start()ed again, which relaunches a
	// single fresh worker on the new rings.
	e.stopWorker()
	e.started = false

	// The Uninit above synchronously waits for the duplex callback to finish, so
	// no callback owns e.cursors here. Drop the leftover cursors and drain the
	// pending queue so a re-Configure starts from a clean cursor list instead of
	// resuming old cursors from their stale positions on the new device.
	e.cursors = nil
	drainPending(e.pending)

	e.mic = mic
	e.cable = cable

	// Allocate the mic-DSP plumbing once, here, off the audio thread. The rings
	// hold mono samples (one per stereo frame), so they are sized in mono samples;
	// the scratch buffers are one period of mono frames. Doing this in Configure
	// (not per callback) keeps duplexCallback allocation-free.
	e.inRing = newRing(ringCapFrames * dspFrame)
	e.outRing = newRing(ringCapFrames * dspFrame)
	e.monIn = make([]float32, periodFrames)
	e.monOut = make([]float32, periodFrames)
	e.duckEnv = 0

	// Allocate the confidence-monitor TAP ring. Unlike in/outRing (mono), it carries
	// INTERLEAVED stereo samples: duplexCallback pushes a whole period of `out`
	// (periodFrames*channels samples) and monitorCallback pulls one period. Size it
	// to several periods so a small duplex<->monitor scheduling jitter never forces a
	// producer drop in steady state. monTapHold is one interleaved frame of seam
	// scratch for the monitor's hold-last underrun ramp. Allocated here, off the
	// audio thread, so neither callback ever allocates.
	e.tapRing = newRing(tapRingCapPeriods * periodFrames * channels)
	e.monTapHold = make([]float32, channels)

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

	// Reset and re-prime the confidence-monitor tap ring on EVERY runtime enable,
	// exactly as Start does, so the documented anti-race invariant holds when the
	// monitor is toggled OFF then back ON (a path SetMonitor explicitly supports
	// below while e.started). Two things would otherwise break in "transmitted"
	// mode: (1) without the one-period prime the monitor's pull and the duplex's
	// push race frame-for-frame with zero lead, firing the underrun seam on any
	// scheduling jitter (audible micro-dropouts the prime exists to prevent); and
	// (2) the tap ring is NOT drained while the monitor is off (the duplex tap is
	// gated on monitorActive at miccallback.go), so any cable mix left from a prior
	// enabled period would be replayed as stale audio when the new monitor starts
	// draining. Reset clears that backlog; primeTapRing restores the one-period lead.
	//
	// This runs under ctrlMu with monitorActive still false (so the duplex tap is
	// gated off and is not pushing) and BEFORE the new monitor device is started (so
	// its callback is not yet draining) — the same "no callback concurrently touches
	// the ring" safety conditions Start's reset/prime rely on. In "clips" mode it is
	// harmless: the ring just holds idle silence the monitor never drains.
	e.resetAndPrimeTapRing()

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

// primeOutputRing preloads the output ring with exactly one DSP frame (480 mono
// samples) of silence so the worker starts one full frame AHEAD of the callback.
// Without this, the duplex callback consumes the output ring in 192-sample periods
// while the worker only ever refills it in whole 480-sample bursts, so the
// available count beats against LCM(192,480)=960: once every 960 samples it dips
// below 192 and outRing.pull returns a PARTIAL frame. That partial pull fires the
// underrun seam at 48000/960 = 50 Hz — the audible buzz on the cable. Holding 480
// zero samples of headroom means available never drops below 192 in steady state
// (480 - 192 = 288 > 0 across any single period), so the 50 Hz underrun-splice can
// no longer fire. The prefill is pure silence: it adds ~10 ms of latency and is
// inaudible. It uses the real ring push API and must be called off the audio thread
// (under ctrlMu, with no callback running) — exactly the conditions Start() holds.
//
// This is its own method so the framing-buzz regression suite can exercise the
// production prime path on a hardware-free engine: deleting or weakening this call
// turns those tests red.
func (e *Engine) primeOutputRing() {
	if e.outRing == nil {
		return
	}
	var prime [dspFrame]float32
	e.outRing.push(prime[:])
}

// primeTapRing preloads the confidence-monitor TAP ring with exactly ONE period of
// interleaved-stereo silence so the monitor callback starts one full period BEHIND
// the duplex callback's tap. Both devices run at periodFrames, but they are driven
// by INDEPENDENT clocks (separate malgo playback devices), so without a lead the
// monitor's pull and the duplex's push race frame-for-frame: any scheduling jitter
// that lets the monitor pull before the duplex pushes yields a partial/empty pull
// and a seam on every such beat. One period of preloaded silence gives the monitor
// a full period of slack to absorb that drift, so its pull stays full in steady
// state (mirroring outRing's prime, which kills the duplex's own framing seam). The
// prefill is pure silence: it adds ~4 ms of monitor latency and is inaudible. It
// uses the real push API and must run off the audio thread with no callback running
// — exactly the conditions Start() holds.
func (e *Engine) primeTapRing() {
	if e.tapRing == nil {
		return
	}
	var prime [periodFrames * channels]float32
	e.tapRing.push(prime[:])
}

// resetAndPrimeTapRing clears any buffered cable mix from the confidence-monitor tap
// ring and re-establishes the one-period silence lead. It is the exact sequence both
// Start (initial activation) and SetMonitor (runtime re-enable) need so the monitor's
// pull never races the duplex's push frame-for-frame and no stale backlog is replayed
// on enable. A nil tap ring (engine never Configured) is a safe no-op via the inner
// nil checks. It MUST be called only with no callback concurrently draining/pushing
// the ring (under ctrlMu, monitor not yet active) — the same conditions reset/prime
// already rely on. Factored out so both call sites share one definition and the
// re-enable invariant is directly exercised by the regression suite.
func (e *Engine) resetAndPrimeTapRing() {
	if e.tapRing == nil {
		return
	}
	e.tapRing.reset()
	e.primeTapRing()
}

// Start activates all configured devices.
func (e *Engine) Start() error {
	e.ctrlMu.Lock()
	defer e.ctrlMu.Unlock()

	if e.duplexDev == nil {
		return errors.New("audio: not configured")
	}
	// Idempotent: a second Start() without an intervening Stop() must NOT relaunch
	// the worker (that would put a second consumer on the SPSC rings and leak the
	// first goroutine). The device is already running; nothing to do.
	if e.started {
		return nil
	}
	// Launch the mic-DSP worker BEFORE the device starts so the output ring is
	// already being filled when the first callback fires (avoiding an initial burst
	// of passthrough). Configure allocated the rings; startWorker builds the
	// denoiser/filters and the goroutine. Reset the rings first so a restart begins
	// with empty buffers rather than stale samples.
	if e.inRing != nil {
		e.inRing.reset()
	}
	if e.outRing != nil {
		e.outRing.reset()
		// PRIME the output ring with one DSP frame of silence so the worker starts a
		// full frame AHEAD of the callback, killing the 50 Hz framing seam. See
		// primeOutputRing for the full LCM(192,480)=960 derivation. Reset first so a
		// restart begins from empty, then prime exactly one frame of headroom.
		e.primeOutputRing()
	}
	// Reset then prime the confidence-monitor tap ring with one period of silence so
	// the monitor device (independent clock) starts a full period behind the duplex
	// tap and never races it frame-for-frame. See resetAndPrimeTapRing / primeTapRing.
	// Harmless in "clips" mode: the ring just holds idle silence the monitor never
	// drains. SetMonitor's runtime re-enable shares this exact sequence.
	e.resetAndPrimeTapRing()
	e.startWorker()

	if err := e.duplexDev.Start(); err != nil {
		e.stopWorker()
		return err
	}
	if e.monitorDev != nil {
		if err := e.monitorDev.Start(); err != nil {
			_ = e.duplexDev.Stop()
			e.stopWorker()
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

	// The duplex device is uninitialized (Uninit synchronously drains its
	// callback), so no callback is pushing/pulling the rings: it is now safe to stop
	// the worker, free its native denoiser, and reset both rings. Order matters —
	// stop the worker only AFTER the device so the worker is never racing a live
	// callback on the rings.
	e.stopWorker()
	if e.inRing != nil {
		e.inRing.reset()
	}
	if e.outRing != nil {
		e.outRing.reset()
	}
	if e.tapRing != nil {
		e.tapRing.reset()
	}
	e.setGateLevel(0)

	// Devices are uninitialized, so no callback is running: it is safe to drop
	// the (callback-owned) cursor slices and drain the handoff channels here.
	e.cursors = nil
	e.monCursors = nil
	drainPending(e.pending)
	drainPending(e.monPending)

	// Nothing is playing any more and no callback will run to say so, so clear the
	// published now-playing set here (and any unconsumed per-clip stop request).
	// Without this the UI would keep showing the chips that were live at teardown.
	e.playing.reset()
	e.stopClipIdx.Store(0)
	e.monStopClipIdx.Store(0)
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
	// Intern the CANONICAL clip ID (clip.ID, not the possibly extension-suffixed
	// id the caller passed) to a stable int32 so the RT callback can publish which
	// clips are playing without ever touching a string or a map. This is a mutexed
	// map op, but we are on the caller's (non-RT) goroutine — the same one that
	// just decoded the clip — so it costs the audio thread nothing.
	pc := pendingClip{clip: clip, gain: g, idx: e.registry.intern(clip.ID)}
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

// StopAll immediately silences every clip currently playing on BOTH the duplex
// (-> Discord) and monitor (-> headset) paths. It is the UI "Stop" button's
// action. StopAll itself only raises two atomic flags; the actual cursor drop
// and pending-queue discard happen inside each RT callback (see clearOnStop), so
// no lock or allocation is taken on the audio thread and a clip is never
// corrupted mid-write — the callback owns its cursor slice exclusively and clears
// it at a buffer boundary. Safe to call from any goroutine. The mic passthrough
// is unaffected: only triggered clips are stopped, the user's live voice keeps
// flowing to Discord.
func (e *Engine) StopAll() {
	e.stopFlag.Store(true)
	e.monStopFlag.Store(true)
}

// StopClip silences every instance of ONE clip on both paths — the action behind
// the UI's per-chip ✕. Like StopAll it only raises atomics; the RT callbacks do
// the actual cursor drop at a buffer boundary (see clearOnStopClip), so no lock
// or allocation lands on the audio thread. A clip that has never been triggered
// has no registry index and therefore cannot be playing, so it is a no-op. Safe
// to call from any goroutine.
//
// The request is best-effort by design: only one pending per-clip stop is held
// per path, so two ✕ clicks landing inside the same ~4 ms buffer will keep the
// first clip playing. That is not a UI bug — the now-playing set is derived from
// the live cursors, so the surviving clip's chip correctly stays until it really
// stops.
func (e *Engine) StopClip(id string) {
	clip := e.lib.Get(id)
	if clip == nil {
		return
	}
	idx, ok := e.registry.lookup(clip.ID)
	if !ok {
		return
	}
	e.stopClipIdx.Store(idx + 1)
	e.monStopClipIdx.Store(idx + 1)
}

// PlayingClips returns the canonical IDs of the clips currently playing on the
// duplex (-> Discord) path, oldest trigger first and de-duplicated, plus whether
// the read was consistent. It is a lock-free seqlock read of the set the RT
// callback publishes each buffer (see nowplaying.go), so it is safe to poll from
// the UI goroutine and costs the audio thread nothing.
//
// A FALSE second return means the snapshot could not be taken cleanly; the
// caller must keep its previous view rather than treat the empty result as
// "nothing is playing".
func (e *Engine) PlayingClips() ([]string, bool) {
	idx, ok := e.playing.snapshot(nil)
	if !ok {
		return nil, false
	}
	return e.registry.names(idx), true
}

// The real-time mic->cable data callback (duplexCallback) and its helpers
// (processMicThroughWorker, duckedMaster) live in miccallback.go. The monitor
// callback stays here because it shares no mic-DSP state.

// monitorCallback is the real-time mixer for the local monitor device (what the
// USER hears). Its behavior depends on the MonitorSource mode, read once per buffer
// via a single atomic load:
//
//   - "clips" (default): plays ONLY clip audio (no mic passthrough), scaling the
//     clips by the INDEPENDENT monitor ("you hear") gain. This is the exact legacy
//     behavior — the user hears clean clips plus their own natural voice.
//   - "transmitted" (confidence monitor): plays the EXACT cable-bound mix
//     (processedMic + clips) that duplexCallback tapped into the tap ring, scaled by
//     the monitor gain, so the user auditions precisely what Discord receives. The
//     tapped mix ALREADY contains the clips, so the monitor cursors are NOT mixed in
//     here (that would double them); they are still drained so the handoff channel
//     and stop flag never back up.
//
// Allocation-free in steady state and lock-free in both modes.
func (e *Engine) monitorCallback(pOutput, pInput []byte, frameCount uint32) {
	_ = pInput

	out := bytesAsF32(pOutput)
	n := int(frameCount) * channels
	if n > len(out) {
		n = len(out)
	}

	if e.tapRing != nil && e.monitorTransmitting() {
		// CONFIDENCE-MONITOR PATH. Drain the cable-bound mix from the tap ring into
		// the monitor output. The clips ride along inside the tapped mix, so we do NOT
		// mix monCursors here — but we still drain monPending and consume the stop flag
		// so a triggered clip queued for the monitor (Trigger enqueues to both paths)
		// and a StopAll never pile up while in this mode. The drained cursors are then
		// discarded (truncated) without mixing.
		e.monCursors = drainInto(e.monPending, e.monCursors)
		e.monCursors = clearOnStop(&e.monStopFlag, e.monCursors, e.monPending)
		e.monCursors = clearOnStopClip(&e.monStopClipIdx, e.monCursors)
		e.monCursors = e.monCursors[:0]

		e.fillMonitorFromTap(out[:n], e.monitorGain())
		return
	}

	// CLIPS PATH (legacy, unchanged). Mix only the triggered clips, scaled by the
	// monitor gain.
	e.monCursors = drainInto(e.monPending, e.monCursors)
	e.monCursors = clearOnStop(&e.monStopFlag, e.monCursors, e.monPending)
	e.monCursors = clearOnStopClip(&e.monStopClipIdx, e.monCursors)
	e.monCursors = mixInto(out[:n], nil, e.monCursors, e.monitorGain())
}

// fillMonitorFromTap writes one monitor buffer from the confidence-monitor tap
// ring, applying the monitor gain. It is the "transmitted"-mode core and is
// allocation-free and lock-free.
//
// On a FULL pull (the steady state, kept so by the one-period tap-ring prime) it
// copies the tapped cable mix straight out at the monitor gain and remembers the
// last frame as the hold-last seam value. On a PARTIAL/empty pull (the monitor
// device momentarily outran the duplex tap) it does NOT splice raw or stale audio
// in — applying the buzz-fix lesson — it HOLDS the last delivered frame and linearly
// ramps it down to silence over spliceRampFrames, then stays silent for the rest of
// the buffer. That keeps the underrun seam click-free and leaks no stale signal,
// exactly like the duplex path's hold-and-ramp on its own underrun.
func (e *Engine) fillMonitorFromTap(out []float32, gain float32) {
	got := e.tapRing.pull(out)
	if got > len(out) {
		got = len(out)
	}

	// Apply the monitor gain to the delivered (tapped) samples in place. The tapped
	// mix is already at cable level; gain is the user's local "you hear" level.
	if gain != 1 {
		for i := 0; i < got; i++ {
			out[i] *= gain
		}
	}

	// Remember the last fully-delivered frame as the hold-last seam (gained), so a
	// later underrun ramps from the real signal rather than a hard gap.
	if got >= channels {
		base := got - channels
		for ch := 0; ch < channels; ch++ {
			e.monTapHold[ch] = out[base+ch]
		}
	}

	if got >= len(out) {
		return // full buffer delivered; nothing to fill.
	}

	// Underrun tail: fill from `got` to the end. HOLD the last seam frame and ramp it
	// to silence over spliceRampFrames frames, then stay silent. No raw/stale splice
	// (buzz-fix). The ramp is keyed per FRAME, so start at the first WHOLE frame at or
	// after `got`: any sub-frame straggler in [got, ceilFrame*channels) — which only
	// occurs if a pull ever returned a partial frame, never in steady state since
	// periods are pushed whole — is filled at full hold (step 0) so it is never left
	// uninitialized, and the delivered samples below `got` are never overwritten.
	ceilFrame := (got + channels - 1) / channels // first whole frame >= got
	for i := got; i < ceilFrame*channels && i < len(out); i++ {
		out[i] = e.monTapHold[i%channels]
	}
	frames := len(out) / channels
	for f := ceilFrame; f < frames; f++ {
		var ramp float32
		if step := f - ceilFrame; step < spliceRampFrames {
			ramp = 1 - float32(step+1)/float32(spliceRampFrames)
		}
		base := f * channels
		for ch := 0; ch < channels; ch++ {
			out[base+ch] = e.monTapHold[ch] * ramp
		}
	}
}

// drainInto pops every clip currently queued in ch and appends a fresh cursor
// for each onto cursors, returning the grown slice. It blocks on nothing (a
// non-blocking select loop). Any append growth happens on the callback's own
// slice, never under a lock shared with the producer.
func drainInto(ch <-chan pendingClip, cursors []*clipCursor) []*clipCursor {
	for {
		select {
		case pc := <-ch:
			cursors = append(cursors, &clipCursor{pcm: pc.clip.PCM, gain: pc.gain, idx: pc.idx})
		default:
			return cursors
		}
	}
}

// clearOnStop consumes a one-shot stop flag for an RT callback: if the flag was
// set (CompareAndSwap true->false), it truncates the callback's cursor slice to
// length zero — reusing the same backing array, so no allocation — AND drains any
// clip still sitting in the pending channel, so a clip that was queued but had
// not yet become a cursor is discarded too. The result is that within one buffer
// nothing is playing and nothing pending will start. When the flag is clear this
// is a single atomic load and the slice is returned untouched. Allocation-free
// and lock-free; only ever called by the callback that owns `cursors`.
func clearOnStop(flag *atomic.Bool, cursors []*clipCursor, pending chan pendingClip) []*clipCursor {
	if !flag.CompareAndSwap(true, false) {
		return cursors
	}
	drainPending(pending)
	return cursors[:0]
}

// clearOnStopClip consumes a one-shot PER-CLIP stop request for an RT callback.
// The flag holds a clipRegistry index plus one (0 == no request); Swap takes the
// request and clears it in a single atomic op. On a request it compacts every
// cursor playing that clip out of the callback's slice IN PLACE — reusing the
// same backing array, exactly like mixInto's compaction, so nothing is
// allocated. Cursors for other clips keep their position and keep playing.
//
// Clips still sitting in the pending channel are deliberately left alone: a clip
// triggered microseconds before the ✕ may not be a cursor yet, and it will start
// and then show a chip that is TRUE (it really is playing). Allocation-free and
// lock-free; only ever called by the callback that owns `cursors`.
func clearOnStopClip(flag *atomic.Int32, cursors []*clipCursor) []*clipCursor {
	v := flag.Swap(0)
	if v == 0 {
		return cursors
	}
	want := v - 1
	kept := cursors[:0]
	for _, c := range cursors {
		if c.idx != want {
			kept = append(kept, c)
		}
	}
	return kept
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
