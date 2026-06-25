//go:build windows

package apm

// apm_windows.go is the real WebRTC APM binding on Windows. It embeds
// webrtc-apm.dll, extracts it to a per-process temp file once, loads it at runtime
// via windows.LoadDLL, and drives the C ABI (webrtc_apm_*). Because the DLL is
// loaded at runtime, the C++ APM is never linked at cgo compile time — the module
// keeps building with the default gcc toolchain and the produced binary is
// self-contained (no loose DLL to ship).
//
// The call sequence mirrors the validated apmspike.SmokeTest: create APM + config,
// set HPF / NS / GC1 / AEC, apply, initialize, create a mono 48k stream config,
// then process_stream one deinterleaved mono frame per call. All native handles
// live for the Processor's lifetime; ProcessCapture allocates nothing.

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// dllBytes is the embedded WebRTC APM DLL. Embedding keeps the build
// toolchain-clean (runtime LoadDLL, no cgo C++ link) AND ships a single
// self-contained binary: the bytes are written to a temp file the first time New
// runs and that file is what LoadDLL opens.
//
//go:embed webrtc-apm.dll
var dllBytes []byte

// loadOnce guards the one-time DLL extraction + load. The resulting handle and
// resolved procs are process-global (the DLL is stateless across APM instances;
// each webrtc_apm_create returns its own instance), so every Processor shares one
// loaded module.
var (
	loadOnce  sync.Once
	loadedDLL *dllProcs
	loadErr   error
)

// dllProcs holds the loaded module and every C ABI entry point the Processor
// needs, resolved once at load time so ProcessCapture never does a FindProc.
type dllProcs struct {
	dll *windows.DLL

	create    *windows.Proc
	destroy   *windows.Proc
	cfgCreate *windows.Proc
	cfgDestroy,
	cfgHPF,
	cfgNS,
	cfgGC1,
	cfgGC2,
	cfgAEC,
	applyCfg,
	initApm,
	scCreate,
	scDestroy,
	procStream,
	frameSize *windows.Proc
}

// loadDLL extracts the embedded DLL to a temp file and loads it, resolving all
// procs. It runs exactly once via loadOnce. The temp file is left in place for the
// process lifetime (LoadDLL keeps it mapped); the OS reclaims the temp dir.
func loadDLL() {
	dir, err := os.MkdirTemp("", "soundboard-apm-")
	if err != nil {
		loadErr = fmt.Errorf("apm: temp dir: %w", err)
		return
	}
	path := filepath.Join(dir, "webrtc-apm.dll")
	if err := os.WriteFile(path, dllBytes, 0o600); err != nil {
		loadErr = fmt.Errorf("apm: write dll: %w", err)
		return
	}

	dll, err := windows.LoadDLL(path)
	if err != nil {
		loadErr = fmt.Errorf("apm: LoadDLL %s: %w", path, err)
		return
	}

	mustProc := func(name string) *windows.Proc {
		if loadErr != nil {
			return nil
		}
		p, e := dll.FindProc(name)
		if e != nil {
			loadErr = fmt.Errorf("apm: FindProc %s: %w", name, e)
			return nil
		}
		return p
	}

	p := &dllProcs{
		dll:        dll,
		create:     mustProc("webrtc_apm_create"),
		destroy:    mustProc("webrtc_apm_destroy"),
		cfgCreate:  mustProc("webrtc_apm_config_create"),
		cfgDestroy: mustProc("webrtc_apm_config_destroy"),
		cfgHPF:     mustProc("webrtc_apm_config_set_high_pass_filter"),
		cfgNS:      mustProc("webrtc_apm_config_set_noise_suppression"),
		cfgGC1:     mustProc("webrtc_apm_config_set_gain_controller1"),
		cfgGC2:     mustProc("webrtc_apm_config_set_gain_controller2"),
		cfgAEC:     mustProc("webrtc_apm_config_set_echo_canceller"),
		applyCfg:   mustProc("webrtc_apm_apply_config"),
		initApm:    mustProc("webrtc_apm_initialize"),
		scCreate:   mustProc("webrtc_apm_stream_config_create"),
		scDestroy:  mustProc("webrtc_apm_stream_config_destroy"),
		procStream: mustProc("webrtc_apm_process_stream"),
		frameSize:  mustProc("webrtc_apm_get_frame_size"),
	}
	if loadErr != nil {
		_ = dll.Release()
		return
	}

	// Sanity-check the DLL agrees the 48k frame size is 480 samples — the contract
	// the whole worker is built on. A mismatch means the wrong DLL was loaded.
	fs, _, _ := p.frameSize.Call(uintptr(SampleRate))
	if int(fs) != FrameSize {
		loadErr = fmt.Errorf("apm: frame size mismatch: dll says %d, want %d", int(fs), FrameSize)
		_ = dll.Release()
		return
	}
	loadedDLL = p
}

// gcAdaptiveDigital is webrtc_apm_gc_mode WEBRTC_APM_GC_ADAPTIVE_DIGITAL: the
// GainController1 mode that applies an adaptive digital gain (level a quiet talker
// up, tame a loud one) WITHOUT needing analog-HAL coupling. This is the gain path
// Discord's voice config uses and the one the validated apmspike SmokeTest drives.
const gcAdaptiveDigital = 1

// applyConfig builds a fresh native config struct from cfg, sets every submodule
// toggle, and applies it to apmPtr, returning the apply rc (0 == success). It is
// shared by New (first apply) and Reconfigure (runtime NS/AGC flips) so the exact
// Discord mapping lives in one place.
//
// Gain control is the DISCORD shape: GainController1 in ADAPTIVE-DIGITAL mode. The
// C ABI's set_gain_controller2 takes only the top-level `enabled` bit — it does NOT
// (and cannot) enable gain_controller2.adaptive_digital or its input-volume
// controller, both of which default to false in the WebRTC config — so driving GC2
// alone applies NO adaptive gain (a dead toggle: a quiet talker stays quiet). GC1's
// setter exposes the full (enabled, mode, target_dbfs, comp_gain_db, limiter) shape,
// so GainControlEnabled drives gain_controller1 in adaptive-digital mode (the path
// Discord uses and apmspike.SmokeTest proves boosts a quiet signal) and
// gain_controller2 is forced OFF so the two never stack. The sub-values mirror the
// validated spike (target 0 dBFS, 9 dB compression, limiter on) — library-default
// shaped, not invented.
func applyConfig(p *dllProcs, apmPtr uintptr, cfg Config) int32 {
	cfgPtr, _, _ := p.cfgCreate.Call()
	if cfgPtr == 0 {
		return -2 // WEBRTC_APM_CREATION_FAILED
	}
	defer p.cfgDestroy.Call(cfgPtr)

	p.cfgHPF.Call(cfgPtr, b2u(cfg.HighPassFilterEnabled))
	p.cfgNS.Call(cfgPtr, b2u(cfg.NoiseSuppressionEnabled), uintptr(cfg.NoiseSuppressionLevel))
	// GainController1, adaptive-digital — the ACTUAL working AGC. Signature:
	// (cfg, enabled, mode, target_dbfs, comp_gain_db, limiter). This is exactly what
	// the apmspike.SmokeTest applies and is verified to level a quiet talker up.
	p.cfgGC1.Call(cfgPtr, b2u(cfg.GainControlEnabled), uintptr(gcAdaptiveDigital), 0, 9, 1)
	// gain_controller2 explicitly OFF so AGC1 and AGC2 never stack (and because GC2's
	// C ABI cannot turn on its adaptive-digital sub-controller anyway).
	p.cfgGC2.Call(cfgPtr, 0)
	p.cfgAEC.Call(cfgPtr, b2u(cfg.EchoCancellationEnabled), 0)

	rc, _, _ := p.applyCfg.Call(apmPtr, cfgPtr)
	return int32(rc)
}

// webrtcProcessor is the real APM-backed Processor. It owns one native APM
// instance and one mono stream config for its lifetime; the per-call channel
// pointer arrays are preallocated so ProcessCapture allocates nothing.
type webrtcProcessor struct {
	procs *dllProcs
	apm   uintptr // webrtc_apm*
	sc    uintptr // webrtc_stream_config* (mono, 48k)

	// srcChans/dstChans are the 1-element channel-pointer arrays process_stream
	// takes (const float* const* src, float* const* dest). Reused every call and
	// repointed at the caller's frame, so no per-frame allocation.
	srcChans [1]*float32
	dstChans [1]*float32

	closed bool
}

// Available reports whether a real APM Processor can be constructed: the embedded
// DLL loaded and every entry point resolved. False means New returns the no-op
// Processor and the worker should treat NS/AGC as unavailable (clean passthrough).
func Available() bool {
	loadOnce.Do(loadDLL)
	return loadErr == nil && loadedDLL != nil
}

// LoadError returns the error (if any) from the one-time DLL load, so callers can
// log an honest "APM unavailable: ..." line. nil when the APM loaded fine.
func LoadError() error {
	loadOnce.Do(loadDLL)
	return loadErr
}

// New builds a Processor configured per cfg (Discord-exact when cfg ==
// DiscordConfig()). It loads the embedded DLL on first use. If the DLL is
// unavailable or any native step fails, it returns the no-op Processor and a
// non-nil error — the caller is expected to fall back to passthrough, never to
// abort the audio engine.
func New(cfg Config) (Processor, error) {
	loadOnce.Do(loadDLL)
	if loadErr != nil || loadedDLL == nil {
		if loadErr != nil {
			return noopProcessor{}, loadErr
		}
		return noopProcessor{}, fmt.Errorf("apm: DLL not loaded")
	}
	p := loadedDLL

	apmPtr, _, _ := p.create.Call()
	if apmPtr == 0 {
		return noopProcessor{}, fmt.Errorf("apm: webrtc_apm_create returned NULL")
	}

	if rc := applyConfig(p, apmPtr, cfg); rc != 0 {
		p.destroy.Call(apmPtr)
		return noopProcessor{}, fmt.Errorf("apm: apply_config rc=%d", rc)
	}
	if rc, _, _ := p.initApm.Call(apmPtr); int32(rc) != 0 {
		p.destroy.Call(apmPtr)
		return noopProcessor{}, fmt.Errorf("apm: initialize rc=%d", int32(rc))
	}

	ch := cfg.CaptureChannels
	if ch <= 0 {
		ch = 1
	}
	scPtr, _, _ := p.scCreate.Call(uintptr(SampleRate), uintptr(ch))
	if scPtr == 0 {
		p.destroy.Call(apmPtr)
		return noopProcessor{}, fmt.Errorf("apm: stream_config_create returned NULL")
	}

	return &webrtcProcessor{procs: p, apm: apmPtr, sc: scPtr}, nil
}

// ProcessCapture runs the APM capture chain over frame in place. frame must be
// FrameSize mono samples; a wrong length is a no-op (returns a bad-data-length
// code) so the worker can never crash the native side with a short frame.
// Allocation-free and lock-free: src==dest (process_stream permits in-place) and
// the channel-pointer arrays are reused.
func (w *webrtcProcessor) ProcessCapture(frame []float32) int {
	if w.closed {
		return 0
	}
	if len(frame) != FrameSize {
		return -8 // WEBRTC_APM_BAD_DATA_LENGTH
	}
	w.srcChans[0] = &frame[0]
	w.dstChans[0] = &frame[0] // in place: src and dest may share memory
	rc, _, _ := w.procs.procStream.Call(
		w.apm,
		uintptr(unsafe.Pointer(&w.srcChans[0])),
		w.sc,
		w.sc,
		uintptr(unsafe.Pointer(&w.dstChans[0])),
	)
	return int(int32(rc))
}

// Reconfigure re-applies cfg to the live APM instance (the runtime NS/AGC toggle
// path). Must be called from the same goroutine as ProcessCapture, never
// concurrently with it — the WebRTC config-setter thread-safety assumption. The
// stream config is fixed at construction, so cfg's channel counts are ignored here;
// only the submodule toggles take effect.
func (w *webrtcProcessor) Reconfigure(cfg Config) int {
	if w.closed {
		return 0
	}
	return int(applyConfig(w.procs, w.apm, cfg))
}

// Close destroys the native stream config and APM instance. Idempotent. The shared
// DLL handle is intentionally NOT released here: it is process-global and reused by
// any later Processor; the OS unmaps it at exit.
func (w *webrtcProcessor) Close() {
	if w.closed {
		return
	}
	w.closed = true
	if w.sc != 0 {
		w.procs.scDestroy.Call(w.sc)
		w.sc = 0
	}
	if w.apm != 0 {
		w.procs.destroy.Call(w.apm)
		w.apm = 0
	}
}

// b2u maps a Go bool to the 0/1 uintptr the C ABI's int flags expect.
func b2u(b bool) uintptr {
	if b {
		return 1
	}
	return 0
}
