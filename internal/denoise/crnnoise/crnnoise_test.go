package crnnoise

import (
	"math"
	"testing"
)

// TestProcessOneFrame is the de-risking spike: prove a vendored RNNoise state can
// be created and process exactly one 480-sample MONO frame on this MinGW build,
// with the [-1,1] <-> +/-32768 scaling handled inside Process. A pure tone is
// treated as "not speech" by the network and heavily attenuated, so we assert
// the output energy dropped relative to the input — which can only happen if the
// C call actually ran (the scaling trap was respected). A no-op/passthrough or a
// mis-scaled call would leave the energy unchanged or zero it differently.
func TestProcessOneFrame(t *testing.T) {
	st := New()
	if st == nil {
		t.Fatal("New() returned nil — RNNoise state allocation failed")
	}
	defer st.Destroy()

	// A ~1kHz tone at amplitude 0.25 in normalized [-1,1]. Process scales it to
	// +/-32768 internally; we must NOT pre-scale here.
	frame := make([]float32, FrameSize)
	for i := range frame {
		frame[i] = float32(math.Sin(2*math.Pi*float64(i)/48.0) * 0.25)
	}

	var eIn float64
	for _, s := range frame {
		eIn += float64(s) * float64(s)
	}

	vad := st.Process(frame)
	if vad < 0 || vad > 1 {
		t.Errorf("Process returned VAD probability %v outside [0,1]", vad)
	}

	var eOut float64
	for _, s := range frame {
		eOut += float64(s) * float64(s)
	}
	if eOut >= eIn {
		t.Fatalf("output energy %.4f >= input energy %.4f: RNNoise did not process the frame (scaling trap or no-op)", eOut, eIn)
	}
	t.Logf("RNNoise OK: vad=%.4f rmsIn=%.5f rmsOut=%.5f", vad,
		math.Sqrt(eIn/float64(len(frame))), math.Sqrt(eOut/float64(len(frame))))
}

// TestProcessWrongLengthPanics guards the fixed-frame contract: feeding anything
// other than 480 samples is a programming error and must panic loudly rather
// than read/write out of bounds in C.
func TestProcessWrongLengthPanics(t *testing.T) {
	st := New()
	if st == nil {
		t.Fatal("New() returned nil")
	}
	defer st.Destroy()

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("Process with a non-480 frame did not panic")
		}
	}()
	st.Process(make([]float32, 256))
}

// TestDestroyIdempotent confirms Destroy is safe to call and that a destroyed
// State no longer processes (returns 0 without touching C).
func TestDestroyIdempotent(t *testing.T) {
	st := New()
	if st == nil {
		t.Fatal("New() returned nil")
	}
	st.Destroy()
	st.Destroy() // second call must be a harmless no-op
	if got := st.Process(make([]float32, FrameSize)); got != 0 {
		t.Fatalf("Process on destroyed State returned %v, want 0", got)
	}
}
