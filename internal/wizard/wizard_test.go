package wizard

import (
	"strings"
	"testing"

	"github.com/jodagreyhame/SoundBoard/internal/devices"
)

// dev is a tiny helper to build a Device with just a name (RawID/IsDefault are
// irrelevant to the name-based VB-CABLE matching the wizard relies on).
func dev(name string) devices.Device { return devices.Device{Name: name} }

func TestCheckBothPresent(t *testing.T) {
	playback := []devices.Device{
		dev("Speakers (Realtek)"),
		dev("CABLE Input (VB-Audio Virtual Cable)"),
	}
	capture := []devices.Device{
		dev("Microphone (USB)"),
		dev("CABLE Output (VB-Audio Virtual Cable)"),
	}
	st := Check(playback, capture)
	if !st.CableInputPresent {
		t.Errorf("CableInputPresent = false, want true")
	}
	if !st.CableOutputPresent {
		t.Errorf("CableOutputPresent = false, want true")
	}
}

func TestCheckNeitherPresent(t *testing.T) {
	playback := []devices.Device{dev("Speakers (Realtek)")}
	capture := []devices.Device{dev("Microphone (USB)")}
	st := Check(playback, capture)
	if st.CableInputPresent {
		t.Errorf("CableInputPresent = true, want false")
	}
	if st.CableOutputPresent {
		t.Errorf("CableOutputPresent = true, want false")
	}
}

func TestCheckInputOnly(t *testing.T) {
	playback := []devices.Device{dev("CABLE Input (VB-Audio Virtual Cable)")}
	capture := []devices.Device{dev("Microphone (USB)")}
	st := Check(playback, capture)
	if !st.CableInputPresent {
		t.Errorf("CableInputPresent = false, want true")
	}
	if st.CableOutputPresent {
		t.Errorf("CableOutputPresent = true, want false")
	}
}

func TestCheckOutputOnly(t *testing.T) {
	playback := []devices.Device{dev("Speakers (Realtek)")}
	capture := []devices.Device{dev("CABLE Output (VB-Audio Virtual Cable)")}
	st := Check(playback, capture)
	if st.CableInputPresent {
		t.Errorf("CableInputPresent = true, want false")
	}
	if !st.CableOutputPresent {
		t.Errorf("CableOutputPresent = false, want true")
	}
}

func TestCheckEmpty(t *testing.T) {
	st := Check(nil, nil)
	if st.CableInputPresent || st.CableOutputPresent {
		t.Errorf("empty lists: got %+v, want both false", st)
	}
}

func TestDownloadURL(t *testing.T) {
	if got := DownloadURL(); got != "https://vb-audio.com/Cable/" {
		t.Errorf("DownloadURL() = %q, want https://vb-audio.com/Cable/", got)
	}
}

func TestDiscordChecklistContent(t *testing.T) {
	c := DiscordChecklist()
	for _, want := range []string{"CABLE Output", "Noise Suppression"} {
		if !strings.Contains(c, want) {
			t.Errorf("DiscordChecklist() missing %q", want)
		}
	}
}
