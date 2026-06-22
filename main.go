// Command soundboard is a Windows 11 system-tray soundboard that mixes
// sound clips over your live microphone via the VB-CABLE virtual audio cable,
// so anyone in Discord (or any voice app) hears them as if you spoke.
//
// Sounds are NOT embedded in the binary. The app is plug-and-play: at launch it
// reads the sounds/ folder that sits next to the executable, so you can drop new
// clips into sounds/<category>/ and relaunch — no rebuild required.
package main

import (
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"

	"github.com/gen2brain/malgo"

	"soundboard/internal/audio"
	"soundboard/internal/catalog"
	"soundboard/internal/config"
	"soundboard/internal/devices"
	"soundboard/internal/hotkeys"
	"soundboard/internal/tray"
	"soundboard/internal/winui"
	"soundboard/internal/wizard"
)

func main() {
	// Route diagnostics to a log file under the config dir. The shipping build
	// uses -H=windowsgui, which detaches stderr, so plain log.* output would be
	// invisible. Mirror to stderr too for console/dev builds.
	if logPath, err := config.LogPath(); err == nil {
		if f, ferr := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); ferr == nil {
			defer f.Close()
			log.SetOutput(io.MultiWriter(os.Stderr, f))
		}
	}

	// Settings.
	settings, err := config.Load()
	if err != nil {
		log.Fatalf("load settings: %v", err)
	}

	// Catalog: load clips from the sounds/ folder next to the executable at
	// runtime (plug-and-play — nothing is embedded in the binary).
	root, base := soundsRoot()
	soundsDir := filepath.Join(base, "sounds")
	lib, err := catalog.New(root)
	if err != nil {
		log.Fatalf("build library from %s: %v", soundsDir, err)
	}
	// Clips are decoded lazily on first play (catalog.EnsureDecoded), so startup
	// is instant and idle memory stays low regardless of how big the library is.
	var clipCount int
	for _, c := range lib.Categories {
		clipCount += len(c.Clips)
	}
	log.Printf("library: %d categories, %d clips indexed from %s (decoded on demand)", len(lib.Categories), clipCount, soundsDir)

	// Audio context (WASAPI first, DirectSound fallback).
	ctx, err := malgo.InitContext(
		[]malgo.Backend{malgo.BackendWasapi, malgo.BackendDsound},
		malgo.ContextConfig{},
		nil,
	)
	if err != nil {
		log.Fatalf("init audio context: %v", err)
	}
	defer func() {
		_ = ctx.Uninit()
		ctx.Free()
	}()

	// Enumerate devices and resolve the mic + cable endpoints.
	playback, capture, err := devices.Enumerate(ctx)
	if err != nil {
		log.Fatalf("enumerate devices: %v", err)
	}

	status := wizard.Check(playback, capture)
	if !status.CableInputPresent {
		log.Printf("VB-CABLE not detected. Download: %s", wizard.DownloadURL())
		log.Print(wizard.DiscordChecklist())
	}

	mic, micOK := resolveMic(capture, settings.MicName)
	cable, cableOK := resolveCable(playback, settings.CableName)
	if !micOK {
		// No usable mic: Configure leaves the capture device defaulted, so this
		// is only informational.
		log.Printf("no microphone resolved; using the system default capture device")
	}

	engine := audio.NewEngine(ctx, lib)

	// Without the VB-CABLE playback endpoint there is nowhere to route the mix
	// (a zero device id would target garbage, not the default speakers). Run in
	// degraded mode: keep the tray alive so the user can install VB-CABLE and
	// relaunch, but do not start the duplex engine.
	engineRunning := false
	if cableOK {
		if err := engine.Configure(mic, cable); err != nil {
			log.Printf("configure engine: %v (running without audio routing)", err)
		} else {
			// Optional local monitor: only when a monitor device is configured.
			if settings.Monitor && settings.MonitorName != "" {
				if mon, ok := devices.FindByName(playback, settings.MonitorName); ok {
					if err := engine.SetMonitor(&mon); err != nil {
						log.Printf("enable monitor: %v", err)
					}
				} else {
					log.Printf("monitor device %q not found; monitor disabled", settings.MonitorName)
				}
			}
			if err := engine.Start(); err != nil {
				log.Printf("start engine: %v (running without audio routing)", err)
			} else {
				engineRunning = true
				defer func() { _ = engine.Stop() }()
			}
		}
	} else {
		log.Printf("VB-CABLE absent: starting in setup mode (no audio routing). " +
			"Install VB-CABLE and relaunch.")
	}

	// Hotkeys.
	hk := hotkeys.New()
	hk.OnTrigger(func(clipID string) { engine.Trigger(clipID) })
	for combo, clipID := range settings.Hotkeys {
		if lib.Get(clipID) == nil {
			log.Printf("hotkey %q: clip %q not found in library", combo, clipID)
		}
		if err := hk.Register(combo, clipID); err != nil {
			log.Printf("hotkey %q: %v", combo, err)
		}
	}
	hk.Run()
	defer hk.Close()

	// Tray UI (blocks until quit).
	ui := tray.New(lib, engine)

	// Setup section: open the VB-CABLE download page, or show the Discord steps.
	ui.SetSetup(
		status.CableInputPresent,
		func() {
			if err := winui.OpenURL(wizard.DownloadURL()); err != nil {
				log.Printf("open download page: %v", err)
			}
		},
		func() { winui.Info("SoundBoard — Discord setup", wizard.DiscordChecklist()) },
	)

	// The checkbox reflects the engine's actual monitor state at launch.
	ui.SetMonitorInitialState(engineRunning && settings.Monitor && settings.MonitorName != "")
	ui.OnMonitorToggle(func(on bool) {
		if !on {
			_ = engine.SetMonitor(nil)
			settings.Monitor = false
			return
		}
		if settings.MonitorName == "" {
			log.Printf("monitor toggle ignored: no monitor device configured")
			return
		}
		if mon, ok := devices.FindByName(playback, settings.MonitorName); ok {
			if err := engine.SetMonitor(&mon); err != nil {
				log.Printf("enable monitor: %v", err)
				return
			}
			settings.Monitor = true
		} else {
			log.Printf("monitor device %q not found", settings.MonitorName)
		}
	})
	ui.OnQuit(func() {
		_ = engine.Stop()
		hk.Close()
		if err := settings.Save(); err != nil {
			log.Printf("save settings: %v", err)
		}
	})

	// On launch, if VB-CABLE is missing the app can't route audio yet. Pop a
	// visible setup dialog (non-blocking, so the tray still appears) offering to
	// open the download page. Runs once the tray is ready.
	ui.Run(func() {
		if status.CableInputPresent {
			return
		}
		go func() {
			msg := "SoundBoard plays sounds over your microphone using VB-CABLE, " +
				"but VB-CABLE isn't installed yet.\n\n" +
				"Click Yes to open the download page. Install it (needs admin + a reboot), " +
				"then restart SoundBoard.\n\n" +
				"After installing:\n" + wizard.DiscordChecklist()
			if winui.Confirm("SoundBoard — setup needed", msg) {
				if err := winui.OpenURL(wizard.DownloadURL()); err != nil {
					log.Printf("open download page: %v", err)
				}
			}
		}()
	})
}

// soundsRoot locates the directory that contains the sounds/ folder and returns
// an fs.FS rooted there plus the chosen base path. It prefers the directory of
// the running executable (so a shipped soundboard.exe + sounds/ folder work
// regardless of the working directory), then the current working directory. If
// no sounds/ exists yet, it creates an empty one next to the exe so first run is
// clean and the user can drop clips in.
func soundsRoot() (fs.FS, string) {
	var bases []string
	if exe, err := os.Executable(); err == nil {
		bases = append(bases, filepath.Dir(exe))
	}
	if wd, err := os.Getwd(); err == nil {
		bases = append(bases, wd)
	}
	for _, b := range bases {
		if st, err := os.Stat(filepath.Join(b, "sounds")); err == nil && st.IsDir() {
			return os.DirFS(b), b
		}
	}
	base := "."
	if len(bases) > 0 {
		base = bases[0]
	}
	_ = os.MkdirAll(filepath.Join(base, "sounds"), 0o755)
	return os.DirFS(base), base
}

func resolveMic(capture []devices.Device, name string) (devices.Device, bool) {
	if name != "" {
		if d, ok := devices.FindByName(capture, name); ok {
			return d, true
		}
	}
	return devices.DefaultMic(capture)
}

func resolveCable(playback []devices.Device, name string) (devices.Device, bool) {
	if name != "" {
		if d, ok := devices.FindByName(playback, name); ok {
			return d, true
		}
	}
	return devices.FindCableInput(playback)
}
