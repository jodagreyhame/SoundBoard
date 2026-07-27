# SoundBoard

A Windows 11 **soundboard app** that plays your own sound clips **over your live
microphone**, so anyone in Discord (or any voice app) hears them as if you said them.

SoundBoard is a **windowed application**. The GUI is a **[Wails v2](https://wails.io)
WebView2 frontend**: a frameless window with a gradient titlebar, a category sidebar with
jump-to-section and a routing pill, a setup banner, a category-grouped clip grid, a bottom
mixer dock, and a Mic & Audio view with a live ring meter. It has a **searchable,
categorized grid** of clips and **in-app volume sliders**, plus a **system-tray icon** that
reopens the window. Closing the window hides it to the tray; the soundboard and global
hotkeys keep running in the background.

There is **no Discord bot, no token, no `discordgo`**. The mechanism is a virtual audio
cable (VB-CABLE): the app opens a single duplex audio device, mixes your real mic input
with the triggered clip(s) sample-by-sample, and pushes the result into **CABLE Input**.
Discord listens to **CABLE Output**. Because it is an in-app mixer, you can **talk and fire
clips simultaneously** (like Soundpad / Resanance), not just replace your mic while a clip
plays.

SoundBoard also includes a full **in-app mic-processing suite** (the "Mic & Audio" view):
WebRTC **noise suppression** at selectable levels, **automatic gain control**, a **voice
gate** with four input modes (voice-activated / push-to-talk / always-on / mute), and
soundboard **ducking** under your voice. Because this cleans your mic *before* it reaches
the cable, **you turn Discord's own noise suppression (Krisp) OFF** and let SoundBoard do
the work — see [The in-app audio suite](#the-in-app-audio-suite) below.

**Zero Discord configuration.** When VB-CABLE is present, SoundBoard auto-engages routing
on launch: it points the **Windows default recording device** at CABLE Output (so Discord —
which uses the default mic — hears the soundboard with nothing changed inside Discord), and
**restores your real default mic when you quit**. The audio engine still captures your real
microphone (the previous default), never the cable.

Written in Go. Windows 11 only (the routing is WASAPI + VB-CABLE specific).

---

## How it works (one line)

One `malgo` **duplex** WASAPI device: `Capture = your real mic`, `Playback = CABLE Input`.
In the real-time data callback: `out[i] = clamp(mic[i]*micGain + Σ (clip[i] * clipGain * masterGain), -1, +1)`
in float32. Mic, soundboard-master, and per-clip gains are applied lock-free in the callback.
Clips are decoded and resampled to 48 kHz / 2ch / float32 **lazily on first play**, never in
the callback.

```
  mic  ─┐
        ├─►  duplex device  ──►  CABLE Input ──(cable)──► CABLE Output ──► Discord mic
 clips ─┘    (mix + clamp)            ▲                         ▲
                                      │                         │
                          SoundBoard plays here    Windows default mic (auto-set
                                                      on launch, restored on quit)
```

---

## The app window

The window is a **frameless Wails WebView2 shell**: a gradient titlebar (drag region + theme
toggle + window controls), a **left sidebar**, and a main content area that switches between
two views — **Soundboard** and **Mic & Audio** — selected from the sidebar.

- **Sidebar** — the **Soundboard / Mic & Audio** view switcher; a **Jump to category** list
  (each category with its colour chip and live clip count) that scrolls the grid to that
  section; a **routing pill** (green "Routing active" / amber "Routing needs setup") mirroring
  the banner state; and **Tray** (hide-to-tray) / **Quit** footer buttons.
- **Search box** — live-filters every clip by name or category as you type; a clear (✕)
  affordance appears when there is text.
- **Categorized grid** — clips grouped under one labelled section per category (each shows a
  count and a category colour chip), laid out as a reflowing grid of clip tiles. Click a tile
  to fire the clip; it also becomes the target of the per-clip mixer slider. Each tile has a
  play affordance, the clip name, and a favourite star.
- **★ Favourites** — every tile has a star toggle; favourited clips are pinned in a
  "★ Favourites" section at the top of the grid, in the order you starred them. Saved on exit.
- **Now-playing chips** — clicking a clip adds an animated chip (with a per-chip ✕) to a
  "NOW PLAYING" row above the grid. This is a client-side recent-trigger affordance, not a
  backend playback signal.
- **Stop button** — a "stop all sounds" button in the search row turns danger-red while clips
  are playing and silences every clip on both paths at once; your live mic is unaffected.
- **Mixer dock** (bottom of the Soundboard view) — four sliders, all 0–150%, applied
  **instantly** and saved on exit. Three are independent "who hears what" levels:
  - **Mic — your voice** — scales your live mic passthrough (how loud *your voice* is to Discord).
  - **Others hear** — master gain on every clip to the cable (what others hear in Discord).
  - **You hear** — the local *monitor* level on your own headset, independent of what Discord
    hears. By default the monitor plays **clips only**; it can also be switched to a
    **confidence monitor** that plays the *exact transmitted signal* (see below).
  - **This clip** — per-clip gain for whichever clip you last clicked (disabled until a clip is
    selected; saved per clip).
- **Mic & Audio view** — the mic-processing suite (documented below): input-mode buttons, an
  input-sensitivity slider, a **live ring meter** that fills green when the gate opens, the
  processing controls, and a read-only **Discord settings checklist**.
- **Setup banner** (top of the Soundboard view) — shows the VB-CABLE / routing status. When
  routing isn't active it offers a one-click action: **Install / Fix audio routing** (downloads
  + silently installs VB-CABLE, elevated) when the cable is missing, or **Engage routing** when
  the cable is present but not yet hijacking the default mic. When routing is active the banner
  is green. Install/engage progress and outcomes show in a modal dialog.
- **System tray** — the tray icon reopens the window (and a menu offers *Open SoundBoard* /
  *Quit*). Closing the window **hides to the tray** instead of quitting, so the soundboard and
  hotkeys keep running.

The window opens at 1160×760 with a 900×620 minimum.

---

## The in-app audio suite

The **Mic & Audio** view is a small voice-processing chain applied to your **live microphone
only**, *before* it is mixed into the cable. Triggered soundboard clips are summed in **after**
the chain and are **never** denoised, gated, or leveled. Every control applies instantly (it is
read lock-free by the real-time audio thread, one atomic load per buffer/frame) and is saved
to `config.json` on exit.

The processing order on the mic is:

```
mic → gain → downmix to mono → WebRTC APM (HPF + noise suppression + AGC)
    → RNNoise (Strong tier and/or speech-probability VAD) → hard gate
    → back to stereo → mix with clips → cable
```

The mic chain runs the **real WebRTC AudioProcessingModule (APM)** — the same DSP library
Discord itself uses — at Discord's capture config: high-pass filter on, noise suppression on,
automatic gain control on (GainController1 adaptive-digital + limiter), echo cancellation off,
mono. A single `ProcessCapture` call per 10 ms (480-sample) frame does the HPF + NS + AGC.
A **hard gate** is kept *after* the APM (the APM has no hard mute) to enforce Mute /
Push-to-talk / Always-on / voice-activated modes; the UI "Mic open" meter is derived from the
**post-APM** RMS — the level Discord actually receives.

**RNNoise** ([Xiph](https://gitlab.xiph.org/xiph/rnnoise), vendored and cgo-compiled) serves
two roles: it is the **Strong** noise-suppression tier (a Krisp analogue, with the APM's own
suppressor switched off), and it is the **speech-probability source** for the advanced voice
activity gate. On a build without cgo, or if its native state fails to allocate, the Strong
tier degrades to a clean passthrough and the gate falls back to an energy latch.

The heavy DSP (APM + RNNoise + gate) runs on a **worker goroutine** fed by lock-free SPSC ring
buffers, never inside the audio callback — so the real-time path stays allocation-free and the
mic adds only ~one period + one frame of latency. If the worker ever falls behind, the callback
emits mic passthrough for that span rather than stalling.

The APM ships as a self-contained WebRTC DLL (`webrtc-apm.dll`, BSD-3-Clause) that is
**embedded into the binary and loaded at runtime via `LoadLibrary`** — so the C++ APM is
never linked at cgo compile time, the whole module keeps building with the default MinGW
gcc toolchain, and the produced `.exe` is self-contained (no loose DLL to ship). See
[Build](#build).

### Confidence monitor — hear exactly what Discord hears

The local monitor ("You hear") has two sources:

- **Clips** (default) — your own headset plays soundboard clips only. Your live voice is
  **not** monitored, so there is no local echo of yourself.
- **Transmitted** — the monitor plays back the **exact signal sent to the cable**: your
  **processed** voice (post-APM: HPF + noise suppression + AGC + gate) **plus** the clips, scaled
  only by your local "You hear" level. This is what the other end actually receives — so you can
  hear, on your own headphones, precisely how your noise suppression, gain control, and gate sound
  to everyone else, and confirm a clip is reaching the call.

It is implemented without touching the real-time safety of the audio callbacks: the duplex callback
taps a copy of its final cable mix into a lock-free single-producer/single-consumer **tap ring**
(only while transmitted mode is selected *and* the monitor is open), and the monitor callback drains
that ring. The ring is **primed one period** at start so the two independent device clocks don't
race, is **reset and re-primed** when you toggle the monitor, and **holds-last-then-ramps-to-silence**
on an underrun (the same anti-buzz rule used elsewhere — never a raw or stale splice). No
allocation, lock, or I/O is added to either callback.

### Controls

| Control | What it does |
|---|---|
| **Input mode** | **Voice-activated** (gate opens on speech — the default), **Push-to-talk** (open only while the PTT hotkey is held), **Always-on** (gate forced open), **Mute** (gate forced closed; the mic never reaches Discord). Mute / PTT-up are enforced on the **real-time thread** so the mic is authoritatively silent even if the DSP worker underruns. |
| **Input sensitivity** | The gate's open threshold, 0–100%. Higher = a louder voice is required to open the gate. The live **"Mic open"** ring meter beside it shows the current gate-open level (green when open) so you can tune it against your real speaking voice. |
| **Automatically determine input sensitivity** | When on, the energy gate's threshold tracks a slow noise-floor follower instead of the manual sensitivity slider (Discord parity). |
| **Advanced voice activity** | When on (the default), the voice-activated gate opens on RNNoise's **trained speech probability** rather than raw energy — so breathing, which has energy but is not speech, no longer trips the gate. When off, or when RNNoise is unavailable, the legacy energy latch is used. |
| **Noise suppression** | Four levels: **None**, **Standard** (WebRTC APM at *Moderate*), **High** (APM at *High* — the default, chosen to suppress breathing), and **Strong** (RNNoise instead of the APM suppressor; the Krisp analogue). Strips keyboard, fan, and room noise while preserving voice. |
| **Echo cancellation** | Toggles the APM echo canceller (off by default; Discord parity). |
| **Automatic gain control** | Toggles the **WebRTC APM gain controller** (GainController1 in adaptive-digital mode + limiter), which brings a quiet talker up and tames a loud one. The adaptive-digital controller applies a real, level-dependent digital gain to the capture signal, so a soft voice reaches Discord at a usable level. |
| **Attenuation (duck soundboard while talking)** | Lowers soundboard clips under an open mic gate via an envelope follower, so clips sit under your speech and pop back up when you stop. The depth is adjustable, 0–100% (default ≈ −9 dB). |
| **Push-to-talk hotkey** | A combo (e.g. `ctrl+grave`) that opens the mic in **Push-to-talk** mode. It is re-bound live from the UI and persisted; a combo another application already owns is rejected, logged, and leaves the previous binding in place. |
| **Audio subsystem** | A cosmetic selector (Standard / Legacy / Experimental) kept for Discord parity. It is persisted but has **no engine effect** — there is a single malgo/WASAPI backend. |

`config.json` also retains an inert `forceThrough` field. It once drove a continuous voiced
"carrier" tone on the cable to hold Discord's voice-activity gate open; that carrier was a
buzz by construction and has been removed from the engine. The setter is a no-op and the
value only round-trips so older settings files still load. To keep short clips from being
gated, set Discord's input sensitivity manually (next section) instead.

### Why Discord's own Noise Suppression must be set to **None**

SoundBoard cleans, gates, and levels your mic itself, then mixes the *unprocessed*
soundboard clips on top, and sends the whole mix to **CABLE Output**, which is what Discord
hears. If you *also* leave **Discord → Voice & Video → Noise Suppression** on (Krisp or the
standard suppressor), Discord runs a **second** noise/voice filter over that mix — and Krisp is
trained to keep **human voice** and discard **everything else**, so it will **mangle or
silence your soundboard clips** (sound effects, music, game lines are exactly the "non-voice"
it strips). It also fights SoundBoard's own gate and AGC. So:

- **Set Discord's Noise Suppression to `None`.** SoundBoard already runs the **same WebRTC
  noise suppressor Discord would**, upstream of the cable — running it twice just fights itself
  and mangles the soundboard clips.
- Turn **Echo Cancellation** and **Automatic Gain Control** **off** in Discord too (SoundBoard's
  APM gain control is the only leveler you want in the chain).
- Turn off **"Automatically determine input sensitivity"** in Discord and lower the manual
  threshold so short clips aren't gated out.

This is the single most important setting: with Krisp on, the soundboard appears to "not
work" or to drop the first part of every clip. **Krisp off → clips come through clean.**

### The 50 Hz framing buzz — root cause + fix

Earlier builds emitted a steady ~50 Hz buzz on the cable whenever the mic chain was running.
There were **two** independent contributors, both now fixed.

**1. Output-ring framing beat (the main buzz).** The duplex audio callback consumes the
processed-mic output ring in the hardware's **192-sample** period, while the DSP worker only
ever refills it in whole **480-sample** (10 ms) frames. With the ring starting empty, the
*available* sample count beats against `LCM(192, 480) = 960`: once every 960 samples the
available count dips below a period, and the ring returns a **partial** frame. That partial
pull fired an underrun seam at `48000 / 960 = 50 Hz` — exactly the audible buzz.

> **Fix — prime the ring + hold-last.** On `Start()` the engine **primes the output ring
> with one 480-sample frame of silence** (`primeOutputRing`), so the worker runs a full frame
> ahead of the callback and the available count never drops below a period in steady state
> (`480 − 192 = 288 > 0`) — the 50 Hz partial pull can no longer fire. It adds ~10 ms of
> latency and is inaudible. As defence-in-depth, the partial-underrun path **holds the last
> processed sample and zero-ramps it to silence** over ~16 frames instead of splicing the
> full-level raw mic in (the old behaviour collided processed-vs-raw levels on that same
> 960-sample beat).

**2. The static "force-through" carrier.** An old **Force sounds through…** toggle added a
continuous voiced tone to the cable to hold Discord's voice-activity gate open. A constant tone
is **a buzz by construction**. It has been **removed from the engine**: `SetForceThrough` is an
inert no-op, and the `forceThrough` config field is retained only so existing settings
round-trip.

The fix is covered by a hardware-free regression suite in `internal/audio/buzzfix_test.go`
(pure-arithmetic ring proof, the production prime path, and a real worker + duplex-callback
end-to-end test that asserts no 50 Hz seam).

---

## Build

### Requirements

- **Go 1.25+** (developed and verified on go1.25.5 windows/amd64).
- **CGO enabled** with the **default MinGW-w64 gcc** on `PATH` (verified on gcc 15.2.0) —
  `CC=gcc CXX=g++`, no clang/MSVC override needed. `malgo` (miniaudio) and RNNoise require
  cgo on Windows. The WebRTC APM is a **prebuilt DLL loaded at runtime** (not linked at cgo
  compile time), so it adds **no** toolchain requirement: the entire module builds with plain gcc.
- **Wails v2 CLI** (verified on v2.11.0): `go install github.com/wailsapp/wails/v2/cmd/wails@latest`.
  Only needed for the recommended `wails build` path (the plain `go build` path below does not
  need the CLI).
- **Microsoft Edge WebView2 runtime** — the Wails GUI renders inside WebView2. It is
  **preinstalled on Windows 11** (and on current Windows 10), so the target platform needs no
  extra install. Verified against WebView2 runtime 149.x. (If ever absent, Wails' bootstrapper
  can fetch it; the build does not bundle the WebView2 binaries.)

### Build the shipping binary (recommended: the Wails CLI)

```bash
CGO_ENABLED=1 wails build -trimpath
#   -> build/bin/soundboard.exe
```

`wails build` is the shipping path: it builds a **`-H=windowsgui`** binary (no console
window), generates the JS↔Go bindings, embeds the `frontend/dist` assets, and packages the
Windows icon + manifest from `build/windows/`. The console is detached, so diagnostics go to
a log file (see [Run](#run)).

### Build the shipping binary (plain `go build`, no Wails CLI)

```bash
CGO_ENABLED=1 go build -trimpath -tags desktop,production -ldflags "-H=windowsgui -s -w" -o build/bin/soundboard.exe .
```

> **`-tags desktop,production` is mandatory, not an optimisation.** Wails guards its real
> `CreateApp` behind `//go:build !dev && !production && !bindings`. Build without one of those
> tags and you still get a 15 MiB executable that links, runs, and exits 0 from `go build` —
> but at launch it shows only a message box reading *"Wails applications will not build without
> the correct build tags"*, because the stub `CreateApp` was compiled in instead of the real
> one. There is no compile-time error and no warning; the failure appears only when a user runs
> it. `wails build` passes these tags for you, which is why the CLI path cannot hit this.
>
> To check a binary you already have:
> ```bash
> strings -a build/bin/soundboard.exe | grep -c "will not build without the correct build tags"
> ```
> `0` is a real build. `1` means you shipped the stub.

Both paths write to the same place — **`build/bin/soundboard.exe` is the only build output
this project produces.** Keeping one location matters more than it looks: a stale binary left
somewhere else is indistinguishable from a fresh one, and running the wrong copy is a
confusing failure to diagnose.

Functionally equivalent to `wails build` (same entrypoint, same embedded frontend); it just
skips the Wails CLI's icon/manifest packaging. `-s -w` strips the symbol table and DWARF, which
is what `wails build` does in production mode — omit them and the binary is ~20 MiB instead of
~16 MiB.

> **The WebRTC APM DLL is embedded — nothing to ship alongside the exe.** `webrtc-apm.dll`
> (~4.3 MiB, BSD-3-Clause) lives in `internal/apm/` and is pulled into the binary with
> `//go:embed`. At first use the APM is extracted to a per-process temp file and loaded via
> `LoadLibrary`, so the C++ APM is **never linked at cgo compile time** — both `wails build` and
> `go build` produce a **self-contained** `soundboard.exe` that needs no loose DLL. (The built
> exe's import table does not list `webrtc-apm.dll`; it is loaded at runtime.) On a
> non-Windows build the APM is unavailable and the mic chain degrades to a clean passthrough +
> the hard gate — the module still compiles everywhere.

> **Always build with `-trimpath` if you intend to distribute the binary.** Go compiles
> absolute source paths into the executable for panic tracebacks, so a build without it embeds
> your own machine's layout — your `GOPATH`, your module cache under your home directory, and
> the full path to your checkout. On a real build here that was ~1,100 strings containing the
> builder's username. `-trimpath` reduces them to module-relative paths and costs nothing but
> slightly less useful stack traces. It is not a Wails or cgo concern; it applies to every Go
> binary you hand to someone else.

> **Binary size.** Both paths produce roughly **16 MiB** when built as documented above. Drop
> `-s -w` from the plain `go build` and it grows to roughly **20 MiB** (Wails strips debug
> symbols in production mode, a bare `go build`
> does not). Sounds are **not** embedded — they load from the `sounds/` folder at runtime; the
> size is the cgo miniaudio + RNNoise runtime, the **embedded ~4.3 MiB WebRTC APM DLL**, and the
> (small) embedded HTML/CSS/JS frontend, and is independent of how many clips you use.

### Build / test everything

```bash
go vet ./...
CGO_ENABLED=1 go test -count=1 ./...        # all packages, incl. the WebRTC APM (internal/apm)
go test -race ./internal/audio/...          # RT mixer + worker + ring handoff, race-clean
```

> **CGO is required (for malgo and RNNoise), but NOT for the APM.** `malgo` (miniaudio) needs
> cgo on Windows, so the build is always `CGO_ENABLED=1` — but it builds with the **default
> gcc** (`CC=gcc CXX=g++`, no override). The WebRTC APM that does noise suppression and gain
> control is loaded from the **embedded DLL at runtime**, so no C++ is linked at cgo compile
> time and no special toolchain is required. A `CGO_ENABLED=0` build still compiles (malgo,
> RNNoise, and the APM all degrade), but the shipping build is `CGO_ENABLED=1`.

### Headless device-enumeration smoke check

`cmd/devcheck` initializes the real malgo WASAPI context and lists the audio devices the
engine would see at startup — **without** opening any device or needing the GUI. Use it to
confirm cgo/malgo links and runs, and to see whether VB-CABLE is detected:

```bash
go run ./cmd/devcheck
```

It exits 0 on a successful context init + enumeration (whether or not VB-CABLE is present),
non-zero on a real backend/link failure.

---

## Run

Double-click `soundboard.exe` (or run it). The window opens; a tray icon also appears.

- Browse/search clips in the window and click to play them.
- Adjust mic / soundboard / per-clip volumes with the sliders.
- Close the window to hide to the tray (keeps running); use the tray **Quit** to exit.

Diagnostics (library load, VB-CABLE status, routing engage/restore, device resolution,
hotkey errors) are appended to:

```
%AppData%\soundboard\soundboard.log
```

Settings (volumes, window size, mic/cable/monitor names, hotkeys) persist to
`%AppData%\soundboard\config.json`.

---

## First-time setup

### The easy path: let the app do it

If VB-CABLE is **not** installed, the setup banner shows **Install / Fix audio routing**.
Clicking it downloads the official VB-CABLE driver pack (from VB-Audio's own CDN, HTTPS-pinned)
and runs its **silent installer elevated** (you approve one Windows UAC prompt). **VB-CABLE
requires a full Windows reboot** before its endpoints appear — the app tells you so. After the
reboot, relaunch SoundBoard and it **auto-engages routing**: Discord hears the soundboard
with **zero changes inside Discord**, and your real mic is restored when you quit.

### What auto-route does (and undoes)

- **On launch (cable present):** saves your current Windows default recording device, then
  sets the default recording device (console + communications roles) to
  **CABLE Output (VB-Audio Virtual Cable)** via the undocumented `IPolicyConfig` interface.
- **On quit:** restores your saved default mic. The hijack only lasts while SoundBoard runs.
- The audio **engine captures your real mic** (the previous default that was saved *before* the
  hijack), never the cable — so you are not feeding the cable back into itself.

### Manual Discord setup (only if you skip auto-route)

If you'd rather not let the app set your default mic, you can point Discord at the cable
yourself. In **Discord → Settings → Voice & Video**:

1. **Input Device → `CABLE Output (VB-Audio Virtual Cable)`**.
2. **Set Noise Suppression to `None`**, and **turn off all of these** (each one will mangle or
   silence injected SFX *and* fight SoundBoard's own mic processing):
   - **Noise Suppression (Krisp) → `None`** — Krisp keeps human voice and discards everything
     else, so it strips your sound effects. **Biggest offender.** SoundBoard's own WebRTC /
     RNNoise suppression replaces it.
   - **Echo Cancellation → off**
   - **Automatic Gain Control → off** (SoundBoard's AGC is the leveler you want).
   - **"Automatically determine input sensitivity" → off** — then lower the manual threshold so
     short clips aren't gated out.
3. Keep **Windows default playback = your real speakers/headphones**, NOT CABLE Input — or you
   will hear nothing locally. (VB-CABLE's installer sometimes switches your default *playback*
   to CABLE Input; put it back in Windows Sound settings → Output.)

This checklist is also available as text in `internal/wizard`.

> **Note on the Krisp/AGC settings:** auto-route sets Discord's *input device* for you, but it
> **cannot** toggle Discord's in-app Noise Suppression / gain settings — those live inside
> Discord and there is no API to change them. You must set Discord's Noise Suppression to
> `None` yourself. If injected clips sound choppy, lose their first syllable, or go silent,
> Krisp is almost always the cause.

SoundBoard is an independent project and is not affiliated with or endorsed by Discord Inc.
or VB-Audio.

---

## Sounds: bring your own clips

**No audio ships with this project.** SoundBoard reads clips from a `sounds/` folder that
you populate; the folder is created next to the executable on first launch if it is missing.
Nothing is embedded in the binary, and you are responsible for the licensing of whatever you
put in it.

The layout is:

```
sounds/<category>/<clip>.<ext>
```

At launch the app finds `sounds/` beside `soundboard.exe` (falling back to the current
working directory), so **keep the exe and the `sounds/` folder together**.

- Each top-level directory under `sounds/` becomes a **category section** in the window.
- Each audio file in it becomes a **clip button** (display name = filename with `_`/`-`
  turned into spaces, extension stripped). Clip ID is `"<category>/<basename>"`.
- `.keep` files and dotfiles are ignored; categories are sorted alphabetically, clips by ID.
- Supported formats: **`.wav`, `.mp3`, `.flac`, `.ogg`**. Each clip is decoded and resampled
  to 48 kHz / 2ch / float32 **on first play** (lazy), so startup is instant and idle RAM stays
  low.

### Adding sounds (no rebuild — plug-and-play)

1. Drop audio files into `sounds/<category>/` (create a new directory for a new category).
2. **Relaunch the app.** The new clips appear in the grid — no rebuild required.

---

## Status

The audio chain is unit-tested (DSP math, gate hysteresis and ramps, the output-ring priming
and hold-last buzz fix, mix/gain/fade logic, config round-trip, device name-matching,
hotkey/PTT parsing, COM refcount classification, the VB-CABLE installer's host pinning and
zip-slip defences) and the real-time mixer, worker, and ring handoff are race-clean under
`go test -race`. Audio *quality* is subjective: whether the noise-suppression level suits your
mic and room, whether the gate opens on your voice without chopping words, whether the AGC
level and ducking depth feel right, and whether the end-to-end Discord path sounds clean, are
all best judged on a real call with real listeners.

### Known limitations

- **No hot-reload of the sounds folder.** New clips appear after a relaunch.
- **No per-clip playback signal in the backend.** The "NOW PLAYING" chips are a client-side
  recent-trigger affordance, not a live per-tile playing state.
- **The Audio subsystem selector is cosmetic.** It is persisted for Discord parity but has no
  engine effect.
- **The `forceThrough` config field is inert.** It is retained only so older settings files
  round-trip.
- **Web fonts load from Google Fonts over the network.** With no network the window falls back
  to `system-ui`: the layout is intact, the type is slightly different. Embedding the `.woff2`
  files into `frontend/dist` would make it fully offline; that is not done yet.
- **Window chrome is the app's own frameless titlebar**, not native Windows chrome. Both the
  restore and close glyphs **hide to tray**, matching the tray-app lifecycle.

---

## Limitations & Terms of Service

- **You supply the audio.** No sound clips ship with this project. The app reads whatever you
  place in `sounds/<category>/<clip>.<ext>`, a folder created next to the executable on first
  launch. **You are responsible for the licensing of every clip you put there** — many popular
  soundboard clips are copyrighted, and playing them into a voice call is a use you must have
  the right to make.
- **Do not bundle the VB-CABLE installer.** It is donationware; redistribution requires a paid,
  negotiated license from VB-Audio. The app *downloads* the official installer from VB-Audio's
  CDN at the user's request and runs it — it never ships the installer bytes itself.
- **Mic-mixing latency.** The duplex device adds a small amount of latency to your live mic
  (the callback runs at ~192-frame periods, low-latency profile). It is tuned to be small but
  is not zero; this is inherent to routing your mic through an in-app mixer.
- **Discord ToS / etiquette.** Soundboards that inject audio into voice channels can be
  disruptive or against a server's rules. Use responsibly and only where it's welcome.
- **Windows 11 only.** The mechanism is WASAPI + VB-CABLE + `IPolicyConfig` specific. No
  macOS/Linux support.

---

## Licence

SoundBoard is released under the **MIT Licence** — see [LICENSE](LICENSE).

The binary includes third-party components under their own terms:

- **WebRTC AudioProcessingModule** (`internal/apm/webrtc-apm.dll`) — BSD-3-Clause, embedded in
  the executable and loaded at runtime.
- **RNNoise** (`internal/denoise/crnnoise/`) — BSD-3-Clause, vendored C sources compiled via cgo.

Full attribution and licence texts are in [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).

---

## Repository layout

```
soundboard/
├── main.go                  # entrypoint: wails.Run — frameless WebView2 window, embeds
│                            #   frontend/dist, companion tray, close-to-tray, OnShutdown cleanup
├── app.go                   # Wails-bound App: methods the frontend calls (window.go.main.App.*) +
│                            #   GetState snapshot + live events (gateLevel/routingStatus/installProgress)
├── backend.go               # adapts the real engine/catalog/setup/config/hotkeys to the bound App
├── systray.go               # companion getlantern/systray (icon + Open/Quit) on its own goroutine
├── app_test.go              # tests App.GetState against a hardware-free fake backend
├── wails.json               # Wails project config (vanilla frontend, no npm step)
├── frontend/dist/           # the GUI: index.html + styles.css + app.js (vanilla state/render/event
│                            #   code — no JS framework, no build step)
├── frontend/wailsjs/        # Wails-generated JS bindings + runtime (reference; the app embeds dist/)
├── docs/GUI_DESIGN_SPEC.md  # GUI specification
├── assets/                  # tray icon (ico + png)
├── build/windows/           # icon.ico / icon.png / manifest (packaged by `wails build`)
├── cmd/devcheck/            # headless device-enumeration smoke check
└── internal/
    ├── apm/                 # WebRTC APM: embedded webrtc-apm.dll + runtime LoadLibrary binding
    ├── audio/               # real-time duplex mixer (mic + clips -> cable) + gains + optional monitor
    │                        #   + the mic-processing suite: gate/VAD (dsp.go, parity_vad.go), the
    │                        #   worker goroutine + SPSC rings (worker.go/ring.go), RT mic path
    │                        #   (miccallback.go), and the atomic control surface (processing.go)
    ├── denoise/             # Denoiser interface + Passthrough; crnnoise/ is the vendored Xiph
    │                        #   RNNoise C library (cgo, model compiled in) selected when cgo is on
    ├── catalog/             # walk sounds/ folder -> lazy decode/resample to f32/48k/2ch
    ├── config/              # JSON settings (volumes, favourites, window, hotkeys, processing) + log path
    ├── devices/             # malgo enumeration + VB-CABLE / mic name matching
    ├── hotkeys/             # global hotkeys + PTT (golang.design/x/hotkey) + combo parser
    ├── setup/               # VB-CABLE detect + one-click install + auto-route engage/restore
    ├── winaudio/            # COM MMDevice + IPolicyConfig (set/restore default recording endpoint)
    └── wizard/              # VB-CABLE detection + Discord setup checklist text
```
