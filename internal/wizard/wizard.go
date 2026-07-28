// Package wizard provides first-run help: detecting the VB-CABLE endpoints and
// surfacing the download link plus the exact Discord configuration checklist.
package wizard

import (
	"github.com/jodagreyhame/SoundBoard/internal/devices"
)

// Status reports which VB-CABLE endpoints are present.
type Status struct {
	CableInputPresent  bool
	CableOutputPresent bool
}

// Check inspects enumerated playback/capture devices for the CABLE Input
// (playback) and CABLE Output (capture) endpoints.
func Check(playback, capture []devices.Device) Status {
	_, inputPresent := devices.FindCableInput(playback)
	_, outputPresent := devices.FindCableOutput(capture)
	return Status{
		CableInputPresent:  inputPresent,
		CableOutputPresent: outputPresent,
	}
}

// DownloadURL returns the VB-CABLE download page.
func DownloadURL() string {
	return "https://vb-audio.com/Cable/"
}

// DiscordChecklist returns the exact steps the user must perform in Discord:
// set Input Device to CABLE Output; disable Noise Suppression (Krisp), Echo
// Cancellation, Automatic Gain Control, and auto input sensitivity; and keep
// the Windows default playback device set to the real speakers.
func DiscordChecklist() string {
	return `Discord setup checklist:

1. Discord -> Settings -> Voice & Video -> Input Device:
     set to "CABLE Output (VB-Audio Virtual Cable)".
2. In the same panel, TURN OFF:
     - Noise Suppression (Krisp)   (strips non-voice audio -> kills SFX)
     - Echo Cancellation
     - Automatic Gain Control
     - "Automatically determine input sensitivity"
       (then lower the manual threshold so short SFX are not gated out)
3. Keep Windows default playback device set to your real speakers/headphones,
   NOT "CABLE Input".`
}
