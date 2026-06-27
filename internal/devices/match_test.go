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
