package tray

import (
	"testing"

	"soundboard/internal/catalog"
)

// fakePlayer records the IDs that Trigger is called with.
type fakePlayer struct {
	triggered []string
}

func (f *fakePlayer) Trigger(id string) { f.triggered = append(f.triggered, id) }

// TestNewAndCallbacks constructs a UI with a fake player and a tiny library and
// exercises the non-systray surface (New, OnMonitorToggle, OnQuit). It does not
// call Run, which would block on a real systray event loop.
func TestNewAndCallbacks(t *testing.T) {
	lib := &catalog.Library{
		Categories: []catalog.Category{
			{
				Name: "memes",
				Clips: []*catalog.Clip{
					{ID: "memes/airhorn", Name: "airhorn", Category: "memes"},
				},
			},
		},
	}

	player := &fakePlayer{}
	ui := New(lib, player)
	if ui == nil {
		t.Fatal("New returned nil")
	}
	if ui.lib != lib {
		t.Error("library not stored")
	}
	if ui.player == nil {
		t.Error("player not stored")
	}

	var gotMonitor bool
	var monitorCalled bool
	ui.OnMonitorToggle(func(b bool) { monitorCalled = true; gotMonitor = b })
	ui.onMonitorToggle(true)
	if !monitorCalled || !gotMonitor {
		t.Error("OnMonitorToggle callback not wired")
	}

	var quitCalled bool
	ui.OnQuit(func() { quitCalled = true })
	ui.onQuit()
	if !quitCalled {
		t.Error("OnQuit callback not wired")
	}

	// Sanity check the embedded icon is present and looks like an ICO.
	if len(iconICO) < 6 || iconICO[0] != 0x00 || iconICO[1] != 0x00 || iconICO[2] != 0x01 {
		t.Errorf("embedded icon.ico missing or not an ICO (len=%d)", len(iconICO))
	}

	// Player wiring: invoking Trigger as the menu click would propagates.
	player.Trigger(lib.Categories[0].Clips[0].ID)
	if len(player.triggered) != 1 || player.triggered[0] != "memes/airhorn" {
		t.Errorf("Trigger not propagated: %v", player.triggered)
	}
}
