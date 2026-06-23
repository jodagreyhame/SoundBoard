package denoise

import (
	"math"
	"testing"
)

// TestPassthroughLeavesFrameUntouched confirms the no-op Denoiser does not alter
// samples and reports no VAD estimate. Clips bypass all processing by being mixed
// AFTER the mic chain, but the suite also relies on Passthrough being a true
// identity when NoiseSuppression is off.
func TestPassthroughLeavesFrameUntouched(t *testing.T) {
	var d Denoiser = Passthrough{}
	frame := make([]float32, FrameSize)
	for i := range frame {
		frame[i] = float32(math.Sin(float64(i)))
	}
	want := append([]float32(nil), frame...)

	if got := d.Process(frame); got != 0 {
		t.Errorf("Passthrough.Process returned VAD %v, want 0", got)
	}
	for i := range frame {
		if frame[i] != want[i] {
			t.Fatalf("Passthrough altered sample %d: got %v want %v", i, frame[i], want[i])
		}
	}
	d.Close() // must not panic
}

// TestNewWhenSuppressOff always yields Passthrough regardless of build, because
// the user asked for no suppression.
func TestNewWhenSuppressOff(t *testing.T) {
	d := New(false)
	if _, ok := d.(Passthrough); !ok {
		t.Fatalf("New(false) = %T, want Passthrough", d)
	}
	d.Close()
}

// TestNewWhenSuppressOn returns a working Denoiser. In a cgo build it is the real
// RNNoise (Available() true) and must reduce a pure tone's energy; without cgo it
// degrades to Passthrough and that is acceptable. Either way the chain stays
// intact — that is the contract the rest of the suite depends on.
func TestNewWhenSuppressOn(t *testing.T) {
	d := New(true)
	defer d.Close()

	frame := make([]float32, FrameSize)
	for i := range frame {
		frame[i] = float32(math.Sin(2*math.Pi*float64(i)/48.0) * 0.25)
	}
	var eIn float64
	for _, s := range frame {
		eIn += float64(s) * float64(s)
	}
	d.Process(frame)
	var eOut float64
	for _, s := range frame {
		eOut += float64(s) * float64(s)
	}

	if Available() {
		if _, isPass := d.(Passthrough); isPass {
			t.Fatal("Available() is true but New(true) returned Passthrough")
		}
		if eOut >= eIn {
			t.Fatalf("RNNoise did not reduce tone energy: in %.4f out %.4f", eIn, eOut)
		}
	} else {
		if eOut != eIn {
			t.Fatalf("Passthrough fallback altered energy: in %.4f out %.4f", eIn, eOut)
		}
		t.Log("cgo not available — NoiseSuppression degraded to Passthrough (expected)")
	}
}
