package devices

import "testing"

func dev(name string, def bool) Device { return Device{Name: name, IsDefault: def} }

func TestFindCableInput(t *testing.T) {
	tests := []struct {
		name   string
		list   []Device
		want   string
		wantOK bool
	}{
		{
			name:   "empty list",
			list:   nil,
			wantOK: false,
		},
		{
			name: "exact match preferred over contains",
			list: []Device{
				dev("Realtek Speakers", false),
				dev("CABLE Input (something else)", false),
				dev("CABLE Input (VB-Audio Virtual Cable)", false),
			},
			want:   "CABLE Input (VB-Audio Virtual Cable)",
			wantOK: true,
		},
		{
			name: "contains fallback when no exact",
			list: []Device{
				dev("Realtek Speakers", false),
				dev("VB-CABLE Input Port", false),
			},
			want:   "VB-CABLE Input Port",
			wantOK: true,
		},
		{
			name: "first contains match wins among multiple",
			list: []Device{
				dev("CABLE Input Alpha", false),
				dev("CABLE Input Beta", false),
			},
			want:   "CABLE Input Alpha",
			wantOK: true,
		},
		{
			name: "no cable present",
			list: []Device{
				dev("Realtek Speakers", false),
				dev("Headphones", false),
			},
			wantOK: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := FindCableInput(tc.list)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && got.Name != tc.want {
				t.Fatalf("got %q, want %q", got.Name, tc.want)
			}
		})
	}
}

func TestFindCableOutput(t *testing.T) {
	list := []Device{
		dev("Microphone (Realtek)", true),
		dev("CABLE Output (some clone)", false),
		dev("CABLE Output (VB-Audio Virtual Cable)", false),
	}
	got, ok := FindCableOutput(list)
	if !ok {
		t.Fatal("expected a match")
	}
	if got.Name != "CABLE Output (VB-Audio Virtual Cable)" {
		t.Fatalf("exact should win, got %q", got.Name)
	}

	// Contains fallback.
	got, ok = FindCableOutput([]Device{dev("My CABLE Output Thing", false)})
	if !ok || got.Name != "My CABLE Output Thing" {
		t.Fatalf("contains fallback failed: %q ok=%v", got.Name, ok)
	}

	// Absent.
	if _, ok := FindCableOutput([]Device{dev("Mic", false)}); ok {
		t.Fatal("expected no match")
	}
}

func TestFindByName(t *testing.T) {
	list := []Device{
		dev("Microphone (USB Audio)", false),
		dev("Microphone (USB Audio) #2", false),
	}

	// Exact wins even though both contain the substring.
	got, ok := FindByName(list, "Microphone (USB Audio)")
	if !ok || got.Name != "Microphone (USB Audio)" {
		t.Fatalf("exact match failed: %q ok=%v", got.Name, ok)
	}

	// Contains fallback when no exact match.
	got, ok = FindByName(list, "USB Audio")
	if !ok || got.Name != "Microphone (USB Audio)" {
		t.Fatalf("contains fallback failed: %q ok=%v", got.Name, ok)
	}

	// Not found.
	if _, ok := FindByName(list, "Nonexistent"); ok {
		t.Fatal("expected no match")
	}

	// Empty list.
	if _, ok := FindByName(nil, "anything"); ok {
		t.Fatal("expected no match on empty list")
	}
}

func TestDefaultMic(t *testing.T) {
	// IsDefault wins over order.
	list := []Device{
		dev("First", false),
		dev("Second Default", true),
		dev("Third", false),
	}
	got, ok := DefaultMic(list)
	if !ok || got.Name != "Second Default" {
		t.Fatalf("default mic failed: %q ok=%v", got.Name, ok)
	}

	// Falls back to first when none default.
	got, ok = DefaultMic([]Device{dev("Only", false), dev("Other", false)})
	if !ok || got.Name != "Only" {
		t.Fatalf("first fallback failed: %q ok=%v", got.Name, ok)
	}

	// Empty list -> ok false.
	if _, ok := DefaultMic(nil); ok {
		t.Fatal("expected ok=false on empty list")
	}
}

// TestFindCableCaseInsensitive covers the case-insensitive matching that brings
// devices.go into agreement with winaudio.FindCaptureEndpointID, which has
// always lowercased both sides. While the two disagreed, a friendly name could
// satisfy the engage path and not the detect path.
func TestFindCableCaseInsensitive(t *testing.T) {
	in, ok := FindCableInput([]Device{dev("cable input (vb-audio virtual cable)", false)})
	if !ok || in.Name != "cable input (vb-audio virtual cable)" {
		t.Fatalf("lowercase exact name not matched: %q ok=%v", in.Name, ok)
	}
	out, ok := FindCableOutput([]Device{dev("CaBlE OuTpUt (Whatever)", false)})
	if !ok || out.Name != "CaBlE OuTpUt (Whatever)" {
		t.Fatalf("mixed-case contains not matched: %q ok=%v", out.Name, ok)
	}
}

// TestFindCableRenamedEndpoint covers the last-resort adapter match. Windows lets
// a user rename an endpoint, which rewrites only the leading half of the friendly
// name and persists across reboots AND driver reinstalls — so without this a
// renamed cable is indistinguishable from "not installed", and the app sends the
// user into a reinstall loop that can never fix it.
func TestFindCableRenamedEndpoint(t *testing.T) {
	tests := []struct {
		name   string
		list   []Device
		want   string
		wantOK bool
	}{
		{
			name:   "renamed input found via adapter name",
			list:   []Device{dev("Speakers (Realtek)", false), dev("Discord Feed (VB-Audio Virtual Cable)", false)},
			want:   "Discord Feed (VB-Audio Virtual Cable)",
			wantOK: true,
		},
		{
			name: "plain endpoint preferred over the 16ch variant",
			list: []Device{
				dev("CABLE In 16ch (VB-Audio Virtual Cable)", false),
				dev("Renamed Board (VB-Audio Virtual Cable)", false),
			},
			want:   "Renamed Board (VB-Audio Virtual Cable)",
			wantOK: true,
		},
		{
			name:   "16ch accepted when it is the only VB-Audio device",
			list:   []Device{dev("CABLE In 16ch (VB-Audio Virtual Cable)", false)},
			want:   "CABLE In 16ch (VB-Audio Virtual Cable)",
			wantOK: true,
		},
		{
			name:   "named match still wins over adapter fallback",
			list:   []Device{dev("Renamed (VB-Audio Virtual Cable)", false), dev("CABLE Input (VB-Audio Virtual Cable)", false)},
			want:   "CABLE Input (VB-Audio Virtual Cable)",
			wantOK: true,
		},
		{
			name:   "unrelated devices never match",
			list:   []Device{dev("Speakers (Realtek)", false), dev("Microphone (Logitech)", false)},
			wantOK: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := FindCableInput(tc.list)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && got.Name != tc.want {
				t.Fatalf("name = %q, want %q", got.Name, tc.want)
			}
		})
	}

	// The same fallback must work for the capture side.
	out, ok := FindCableOutput([]Device{dev("Board Return (VB-Audio Virtual Cable)", false)})
	if !ok || out.Name != "Board Return (VB-Audio Virtual Cable)" {
		t.Fatalf("renamed output not matched: %q ok=%v", out.Name, ok)
	}
}

// TestFindCableReporterMachine reproduces the first real bug report byte for
// byte. The reporter's VB-CABLE playback endpoint is named "Speakers (VB-Audio
// Virtual Cable)" — not "CABLE Input" — and their driver names the 16-channel
// variant "CABLE In 16 Ch" (spaced), while newer packs ship "CABLE In 16ch".
// The old named-needle matcher saw no cable at all, reported "absent", and sent
// the reporter into an install loop that a reinstall could never fix (a
// reinstall does not rename endpoints). Device names and order are exactly as
// enumerated from their Windows 11 Sound settings screenshots.
func TestFindCableReporterMachine(t *testing.T) {
	playback := []Device{
		dev("U32J59x (NVIDIA High Definition Audio)", false),
		dev("Speakers (VB-Audio Virtual Cable)", false),
		dev("Headphones (Arctis Nova 7)", true),
		dev("CABLE In 16 Ch (VB-Audio Virtual Cable)", false),
		dev("U32J59x (NVIDIA High Definition Audio)", false),
		dev("Speakers (Realtek(R) Audio)", false),
		dev("U32J59x (NVIDIA High Definition Audio)", false),
	}
	capture := []Device{
		dev("CABLE Output (VB-Audio Virtual Cable)", false),
		dev("Microphone (HD Pro Webcam C920)", false),
		dev("Microphone (Arctis Nova 7)", true),
	}

	in, ok := FindCableInput(playback)
	if !ok {
		t.Fatal("renamed cable input not found — this is the reporter's install loop")
	}
	// The renamed plain endpoint must win over the 16-channel variant even
	// though the spaced "16 Ch" form does not literally contain "16ch".
	if in.Name != "Speakers (VB-Audio Virtual Cable)" {
		t.Fatalf("picked %q, want the renamed plain endpoint", in.Name)
	}

	out, ok := FindCableOutput(capture)
	if !ok || out.Name != "CABLE Output (VB-Audio Virtual Cable)" {
		t.Fatalf("cable output: %q ok=%v", out.Name, ok)
	}

	// Enumeration order is not guaranteed: when the spaced "CABLE In 16 Ch"
	// comes FIRST, the renamed plain endpoint must still win — this is the case
	// the space-stripping in isMultiChannelName exists for (a literal "16ch"
	// needle does not match "16 Ch", which would promote the variant).
	reordered, ok := FindCableInput([]Device{
		dev("CABLE In 16 Ch (VB-Audio Virtual Cable)", false),
		dev("Speakers (VB-Audio Virtual Cable)", false),
	})
	if !ok || reordered.Name != "Speakers (VB-Audio Virtual Cable)" {
		t.Fatalf("16 Ch enumerated first: picked %q, want the renamed plain endpoint", reordered.Name)
	}

	// The 16 Ch variant must still be accepted when it is the only cable
	// playback endpoint (e.g. the plain one is disabled).
	only16, ok := FindCableInput([]Device{
		dev("Headphones (Arctis Nova 7)", true),
		dev("CABLE In 16 Ch (VB-Audio Virtual Cable)", false),
	})
	if !ok || only16.Name != "CABLE In 16 Ch (VB-Audio Virtual Cable)" {
		t.Fatalf("16 Ch-only fallback: %q ok=%v", only16.Name, ok)
	}
}
