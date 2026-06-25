package audio

// This file is the PUBLIC control surface of the mic-path processing suite. The
// heavy DSP (HPF -> RNNoise -> AGC -> gate, run on a worker goroutine fed by
// lock-free ring buffers) lands in phase 2; here we define the atomic-backed
// settings the UI, hotkeys, and config wire against, plus the GateLevel readout
// the UI's mic-open meter polls. Every setter is safe to call from ANY goroutine;
// the RT callback / DSP worker reads each value once per buffer/frame via a single
// atomic load, so there is no lock and no allocation on the audio thread.
//
// CONTRACT: SetMicMode / SetGateSensitivity / SetPTTDown / SetNoiseSuppression /
// SetAGC / SetDucking / SetForceThrough / GateLevel. The existing gain/trigger/
// lifecycle methods in audio.go are unchanged.

// micMode* are the RT-friendly integer encodings of the four config MicMode
// strings. The callback reads micModeBits (an atomic.Int32) once per buffer
// instead of comparing strings on the audio thread. SetMicMode maps the config
// string to one of these; an unknown string falls back to micModeVAD.
const (
	micModeVAD    int32 = 0 // gate by voice activity / RMS (default)
	micModePTT    int32 = 1 // open only while the PTT hotkey is held
	micModeAlways int32 = 2 // gate forced open
	micModeMute   int32 = 3 // gate forced closed
)

// defaultGateSensitivity is the engine-side default gate threshold, mirroring
// config's default so a freshly constructed Engine (before main applies saved
// settings) gates a normal speaking voice in but rejects idle room noise.
const defaultGateSensitivity float32 = 0.15

// SetMicMode selects the gate behavior from a config MicMode string ("vad",
// "ptt", "always", "mute"). An empty or unrecognized value is treated as "vad".
// Safe to call from any goroutine; the RT path reads the new mode on its next
// buffer. The string is mapped to an int encoding here so the audio thread never
// compares strings.
func (e *Engine) SetMicMode(mode string) {
	var m int32
	switch mode {
	case "ptt":
		m = micModePTT
	case "always":
		m = micModeAlways
	case "mute":
		m = micModeMute
	default: // "vad" and anything unknown
		m = micModeVAD
	}
	e.micModeBits.Store(m)
}

// micMode reads the current gate-mode encoding lock-free (RT path helper).
func (e *Engine) micMode() int32 { return e.micModeBits.Load() }

// SetGateSensitivity sets the VAD/RMS gate threshold, clamped to [0,1]. Higher
// means the gate requires a louder voice to open. Safe from any goroutine; the
// gate reads it once per frame. Stored as a float32 bit pattern like the gains.
func (e *Engine) SetGateSensitivity(t float32) {
	if t != t { // NaN
		t = 0
	}
	if t < 0 {
		t = 0
	}
	if t > 1 {
		t = 1
	}
	e.gateSensBits.Store(float32bits(t))
}

// gateSensitivity reads the current gate threshold lock-free (RT path helper).
func (e *Engine) gateSensitivity() float32 { return float32frombits(e.gateSensBits.Load()) }

// SetPTTDown reports whether the push-to-talk key is currently held. The hotkey
// manager calls this on key DOWN (true) and key UP (false). In "ptt" MicMode the
// gate opens iff this is true; in other modes it is ignored. Safe from any
// goroutine (it is driven from the hotkey pump).
func (e *Engine) SetPTTDown(down bool) { e.pttDown.Store(down) }

// pttIsDown reads the PTT held-state lock-free (RT path helper).
func (e *Engine) pttIsDown() bool { return e.pttDown.Load() }

// SetNoiseSuppression toggles RNNoise on the mic path. A no-op effect when the
// build lacks cgo/RNNoise (the worker uses passthrough), so enabling it never
// breaks the chain. Safe from any goroutine.
func (e *Engine) SetNoiseSuppression(on bool) { e.noiseSuppressOn.Store(on) }

// noiseSuppression reads the toggle lock-free (worker helper).
func (e *Engine) noiseSuppression() bool { return e.noiseSuppressOn.Load() }

// SetAGC toggles the RMS-target automatic gain leveler on the mic path. Safe from
// any goroutine.
func (e *Engine) SetAGC(on bool) { e.agcOn.Store(on) }

// agc reads the AGC toggle lock-free (worker helper).
func (e *Engine) agc() bool { return e.agcOn.Load() }

// SetDucking toggles soundboard ducking under an open mic gate. Safe from any
// goroutine.
func (e *Engine) SetDucking(on bool) { e.duckingOn.Store(on) }

// ducking reads the ducking toggle lock-free (RT path helper).
func (e *Engine) ducking() bool { return e.duckingOn.Load() }

// SetForceThrough is retained as an INERT no-op for API/config/UI compatibility.
//
// It previously enabled a continuous voiced "carrier" bed on the CABLE path to hold
// Discord's voice-activity gate open. That carrier was a buzz BY CONSTRUCTION (a
// static ~130 Hz voiced tone) and has been removed from the engine entirely as part
// of the framing-buzz fix. The setter stays so the saved config field, Wails
// bindings, and the Fyne "Force through" toggle continue to compile and round-trip;
// the engine simply ignores the value and never emits a carrier. Safe from any
// goroutine.
func (e *Engine) SetForceThrough(bool) {}

// GateLevel returns the current mic-gate open level in [0,1] for the UI's
// mic-open meter: 0 = fully closed (silent), 1 = fully open. The DSP worker
// publishes this each frame (phase 2); until then it reads 0. Safe to poll from
// the UI goroutine; it is a single atomic load.
func (e *Engine) GateLevel() float32 { return float32frombits(e.gateLevelBits.Load()) }

// setGateLevel publishes the current gate-open level for GateLevel to read. Called
// by the DSP worker once per frame in phase 2; defined now so the publish path is
// in place. Unexported: only the engine's own audio thread writes it.
func (e *Engine) setGateLevel(v float32) {
	if v < 0 {
		v = 0
	} else if v > 1 {
		v = 1
	}
	e.gateLevelBits.Store(float32bits(v))
}
