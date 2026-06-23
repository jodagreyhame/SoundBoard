# SoundBoard — GUI Design Specification

**For:** External GUI / product designer
**Purpose:** Design a new look for SoundBoard that "slots right in" to the existing Go + Fyne v2 backend with **zero new backend work** (or with every new dependency explicitly flagged).
**Status:** Ground-truth audit of the shipping app. Every control, label, and state below already exists in code.
**Last updated:** 2026-06-23

---

## How to read this document

This is a **slot-in redesign brief**, not a greenfield product spec. The app already works: it has a window, a tray icon, a clip grid, a mixer, a mic-processing suite, and a setup banner. Your job is to give all of that a new, cohesive, beautiful look — **without inventing features the backend can't perform.**

Two rules govern everything:

1. **Cover every screen, component, and state listed here.** If it's in this doc, it must appear in your deliverables (including each state/variant).
2. **Bind only to the capabilities listed in Section 5.** Every button, slider, toggle, and label maps to a named backend method (the "slot-in contract"). If you want something the contract doesn't offer, that's fine — but you must **flag it explicitly as "needs new backend work"** so we can scope it. Three such gaps are already known and called out (hotkey editing UI, PTT-combo recorder, per-tile "now playing" state).

Throughout, **exact current label text appears in `code font`.** Some of these strings are asserted by automated tests, so changing the wording is a code change — propose new wording deliberately, don't drift it.

---

## 1. Overview & purpose

**SoundBoard** is a Windows 11 **system-tray soundboard**. It lets a user play short audio clips (memes, game sounds, movie quotes, reactions) so that **other people in a Discord voice call hear them**, mixed in alongside the user's real microphone. It also includes an **in-app microphone-processing suite** that cleans up the user's actual voice (noise suppression, leveling, gating) before it reaches Discord.

It does this without any changes inside Discord's own settings being strictly required at runtime, by routing audio through a virtual audio device (VB-CABLE). The app's setup banner manages getting that routing in place.

### Who uses it

- **Gamers and Discord community members** who want a fast, always-available soundboard during voice calls.
- People who also want their **voice to sound clean** on calls (the mic-processing suite) without buying separate software.
- Power users with **200+ clips** who need to find the right sound in a second or two, mid-conversation.

### Core jobs-to-be-done

1. **"Get me set up once"** — install/route audio so Discord can hear the soundboard (the setup banner).
2. **"Play the right clip, instantly"** — browse/search 200+ clips, hit one, it plays into the call. Favourite the ones I use most.
3. **"Make me sound good"** — tune mic processing (gate, noise suppression, gain) and see my mic open/close live.
4. **"Get the levels right"** — independently balance my voice, what others hear, what I hear, and per-clip loudness.
5. **"Kill it now"** — one button silences all playing clips instantly (e.g. a clip is too loud / wrong one).
6. **"Stay out of my way"** — live in the tray, reopen when needed, keep running in the background.

---

## 2. Design goal & the "slot-in" rule

### The goal

A fresh, modern, cohesive visual identity for SoundBoard that feels like a polished consumer app — while mapping **1:1 onto the screens, components, and states that already exist** and remaining **fully implementable in Fyne v2** (see Section 3 for what that allows).

### The slot-in rule (non-negotiable)

- **Same screens.** Two tabs — **Soundboard** and **Audio** — plus a **persistent setup banner** pinned above them, plus the **tray menu**. No new top-level screens unless flagged as new work.
- **Same controls.** Every control in Section 5 must have a home in your design. You may restyle, relabel (deliberately), regroup, and re-lay-out — but you may not remove a capability or add one that has no backend.
- **Stay a tray app with a single resizable window.** Closing the window **hides to tray** (the app keeps running); only the tray **Quit** ends it. This is the lifecycle the design must assume — design for a window that is frequently hidden and reshown, not a document window.
- **Bind to existing capabilities.** Use the controller interfaces in Section 5 as your capability list. If you reach for something not on that list, mark it clearly as **[NEEDS NEW BACKEND]**.

### Already-known capability gaps (flagged for you)

These are the only things a designer is likely to want that the current backend does **not** wire to a widget. Treat them as optional, clearly-labelled "stretch" ideas — not baseline:

| Idea | Backend reality | Verdict |
|---|---|---|
| **Edit clip-trigger hotkeys in the UI** (assign `ctrl+alt+1` to a clip) | Hotkeys are loaded from the config file at startup only. No UI interface exposes register/edit/remove. | **[NEEDS NEW BACKEND]** — net-new wiring. Don't design as baseline. |
| **PTT-combo recorder field** (capture a key combo for push-to-talk) | `AudioController.PTTHotkey()` / `SetPTTHotkey()` exist and persist the combo, **but the binding only takes effect on the next launch** (it is not live-rebound). | **Partial** — you may design a read/edit field that binds these methods, but you must label it "applies after restart." Live re-binding is **[NEEDS NEW BACKEND]**. |
| **Per-tile "now playing" indicator** | Clips are fire-and-forget; nothing reports per-clip playback state. The only "something is playing" affordance is the global **Stop** button. | **[NEEDS NEW BACKEND]** if you want a live per-tile state. A purely-decorative press-flash is fine (custom widget). |
| **Grey-out controls in degraded "setup mode"** | The app does not currently disable clips/sliders when audio routing is absent. | You may **design** a disabled/locked treatment, but note that wiring it to live state is **[NEEDS NEW BACKEND]** unless we only key it off the banner state (which is available). |

---

## 3. Platform & technical constraints

### Environment

- **OS:** Windows 11 (primary and only target).
- **Stack:** Go + **Fyne v2 (v2.7.4)**, single embedded binary, shipped `-H=windowsgui` → **no console window**. The GUI is the only feedback channel.
- **Lifecycle:** system-tray app. Window default size **760×600**, `CenterOnScreen`, **resizable**, and the **chosen size is persisted and restored** across launches. Close-to-tray; tray menu = **Open / Quit**; only Quit exits.
- **Theme:** **dark** is the design target (the app follows the OS dark/light setting; design dark-first, supply light values too — see Section 9).
- **DPI:** Fyne auto-scales to the OS DPI (`FYNE_SCALE` can override). **Design in logical pixels and never hardcode absolute pixel positions.**

### What Fyne can render for free (use these as-is, styled by theme)

- **Structural widgets:** Button (with an *Importance* enum — Low / Medium / High / **Danger** / **Warning** / **Success** — plus alignment + icon), Entry (search), Slider, Check, RadioGroup, Card (header + body panel), ProgressBar, Icon, Label (Bold / Italic / wrapping / alignment).
- **Containers:** Border, VBox, HBox, **GridWrap** (reflowing tile grid), VScroll, Padded, **AppTabs** (the two tabs), plus Grid-with-columns, Max/stack, Center, Split.
- **Dialogs:** Information, Error, Confirm, **ProgressInfinite** (all already used for setup).
- **System tray:** menu + icon + window association.
- **Also available but unused** (you may propose): Select (dropdown), List/Table/Tree (**virtualized** — better than GridWrap at 1000+ items), Accordion, Toolbar, RichText.

### What Fyne **cannot** do (design within these rails)

- **No CSS. No per-widget styling.** You cannot give *one* button a different background or *one* card rounded corners while leaving others square. A stock widget's only knobs are: its **Importance**, its **TextStyle**, its **icon**, and whatever the **global theme** returns.
- **Styling is global, via one theme object.** A custom `fyne.Theme` controls **all ~30 color names**, the **fonts** (per text style), **all icons**, and a set of **size tokens** (padding, text/heading/subheading/caption sizes, separator thickness, and a few **radius** tokens). One theme file restyles the entire app at once. **This is your single biggest, lowest-risk lever — the app ships no custom theme today, so everything is stock Fyne.**
- **Rounded corners are partial.** Theme radius tokens round **only** entries, selection highlights, and scrollbars — **not buttons, cards, or panels.** "Rounded everything" is **not** a global setting. Rounded buttons/cards/tiles are achievable **only** by hand-building them as custom-canvas widgets.
- **No native gradients, shadows-as-style, or animation on stock widgets.** These exist in the canvas layer but only appear if you build a custom widget that draws them.
- **Layout is flow-based.** You place widgets in rows/columns/grids; you do **not** set arbitrary x/y or per-element margins. The **only** spacing knob is the global **padding** token plus your choice of container. Design in flow terms.

### Where custom-canvas widgets are acceptable (spend the cost here only)

Custom widgets are real Go code (a renderer arranging canvas primitives — rectangle with corner-radius/stroke, text, circle, arc, line, gradient, plus animations). They are worth the effort **only at two high-impact spots**:

1. **The clip cell** — the visual heart of the board. A custom rounded tile (corner radius + stroke + optional category color chip + hover/press flash) that stock Button can't deliver.
2. **The mic-open meter** — a custom gradient bar or **arc/ring gauge** that animates green as the gate opens, replacing the stock progress-bar + dot.

**Everything else — banner, tabs, volume sliders, audio toggles, radios, dialogs — must stay stock and be carried by the theme.** If a design element needs a non-standard look, it must become either (a) a global theme token change affecting all widgets of that type, or (b) one of the two custom widgets above. Tag anything else "custom widget" so we can cost it.

### "Design within these rails" checklist

1. Deliver a **token sheet**, not a freeform mock: exact hex for all ~30 color names (dark **and** light), and values for any size tokens you change.
2. Pick **one accent** → `Primary` (recommend `#5865F2`, see Section 9), and define `ForegroundOnPrimary`.
3. Choose **one font family** (regular + bold, optionally italic) as embeddable `.ttf` files.
4. Express any "different-looking" element as **either** a global token change **or** an explicit "this is a custom canvas widget" call-out. Only the clip cell + mic meter qualify as custom.
5. Assume **rectangular** cards/buttons everywhere except the custom clip cell. Do **not** design globally rounded panels.
6. **No** gradients/animation in baseline mocks unless tagged "custom widget."
7. Lay out in **rows/columns/grids + one padding value**, never absolute coordinates.
8. The design must survive the window being **~600 px wide** and being **hidden/reshown** from the tray.
9. Provide **one app/tray PNG** that reads at **16 px** (the tray size).
10. Keep stock widgets for banner, sliders, checks, radio, tabs, dialogs — they carry the theme for free.

---

## 4. Information architecture

The whole app is one window plus a tray icon. The window is a vertical stack: a **setup banner** pinned to the top, and below it a **two-tab** content area.

```
SYSTEM TRAY (always present, the app's true anchor)
├── Tray icon  ── click ──▶ reopens / focuses the window
└── Tray menu "SoundBoard"
    ├── "Open SoundBoard"   → show + focus window
    ├── ──────────────────
    └── "Quit"                → the ONLY way to exit the process

MAIN WINDOW  (title: "SoundBoard", default 760×600, resizable, size persisted)
│
├── ░░ SETUP BANNER ░░  (pinned top — always visible, every tab)
│      states: Engaged(green) / Cable-present-not-engaged(warning) / Cable-absent(warning)
│      + transient modal dialogs: progress, error, success-info, install-confirm
│
└── TABS  (top-located: "Soundboard" | "Audio")
    │
    ├── TAB 1 — SOUNDBOARD
    │   ├── Search row:  [search icon] [ search entry .......... ] [ Stop ]
    │   ├── Scrollable clip browser (one vertical scroll):
    │   │     ├── ★ Favourites (N)         ← pinned section, hidden when empty
    │   │     │     └─ GridWrap of clip cells (play + star)
    │   │     ├── Category A (N) ─ GridWrap of clip cells ─ separator
    │   │     ├── Category B (N) ─ GridWrap of clip cells ─ separator
    │   │     └── … (empty/filtered-out categories are dropped)
    │   │     └── "No clips match your search."   ← shown only when nothing matches
    │   └── Volume card (bottom): 4 slider rows (Mic / Master / Monitor / This clip)
    │
    └── TAB 2 — AUDIO  (mic-processing suite)
        └── Single card "Audio":
              ├── "Input mode" — radio: Voice-activated / Push-to-talk / Always-on / Mute
              ├── "Gate sensitivity" — slider + % readout
              ├── Live mic meter:  "Mic open"  [====bar====] (•)   ← animates in real time
              └── Toggles: Noise suppression / Automatic gain control /
                           Duck soundboard while talking /
                           Force sounds through Discord voice-activity gate
```

> **Note:** When the app is launched without the audio suite wired, only the Soundboard view shows (no tabs). In the shipping build the suite is **always** wired, so **design for the two-tab layout** as the canonical case.

---

## 5. Screen-by-screen component spec (with the slot-in contract)

For each component: **purpose · slot-in binding (the exact backend method) · states to design · current label text.**
The backend surface is six small Go interfaces. Treat these as the **complete capability list** — if it's not here, it doesn't exist without new work.

> **Capability list (the controller interfaces):**
> - **Player** — `TriggerGain(id, gain)`, `StopAll()`
> - **VolumeController** — `SetMic/SetMaster/SetMonitor/SetClip(id, gain)`, `Mic()/Master()/Monitor()/Clip(id)`
> - **FavoritesController** — `IsFavorite(id)`, `ToggleFavorite(id)`, `Favorites()`
> - **AudioController** — `NoiseSuppression()/SetNoiseSuppression`, `AGC()/SetAGC`, `Ducking()/SetDucking`, `MicMode()/SetMicMode`, `GateSensitivity()/SetGateSensitivity`, `ForceThrough()/SetForceThrough`, `PTTHotkey()/SetPTTHotkey`, `GateLevel()`
> - **SetupController** — `Status() → (ready, detail)`, `CanEngage()`, `Install()`, `Engage()`
> - **WindowStore** — `WindowSize()`, `SetWindowSize()` (no visible widget)

---

### 5.1 Setup banner

- **Purpose:** Tell the user whether Discord can actually hear the soundboard, and give a one-click fix. Pinned above both tabs, always visible.
- **Slot-in binding:** reads `SetupController.Status() → (ready bool, detail string)` and `CanEngage() bool`; the action button calls **either** `Install()` **or** `Engage()`.
- **States to design (3 resting states):**
  1. **Engaged / ready** — success/green treatment, a confirm/check icon, **no action button**. Detail is prefixed `"Audio routing active — "` (e.g. `Audio routing active — Discord hears the soundboard — no Discord changes needed`).
  2. **Cable present, not engaged** — warning treatment, action button labelled **`Engage routing`** (calls `Engage()`). Detail e.g. `VB-CABLE detected — click Engage routing`.
  3. **Cable absent** — warning treatment, action button labelled **`Install / Fix audio routing`** (calls `Install()`). Detail e.g. `VB-CABLE NOT detected — click Install / Fix routing` (a variant: `VB-CABLE Input found, but CABLE Output is missing`).
- **Action button style:** Warning importance + a download icon. Design the warning vs success colour treatments and a banner that can **show or hide** its action button.
- **Transient modal states (also design these):**
  - **In-progress:** a modal infinite-progress dialog, title `Installing VB-CABLE` or `Engaging routing`, waiting copy (install copy mentions approving the Windows UAC elevation prompt).
  - **Error:** error dialog, e.g. `<action> failed: …`.
  - **Engage success:** info dialog, e.g. `Routing engaged`.
  - **Install success:** **confirm** dialog titled `VB-CABLE installed` offering an **app restart** (not a Windows reboot).
- **Label note:** banner strings are **inline literals** (not test-asserted constants) — relatively safe to reword, but keep the three-state logic intact.

---

### 5.2 Search box

- **Purpose:** Live filter across the whole library — the primary way to find one of 200+ clips fast.
- **Slot-in binding:** **no backend method** — it filters the in-memory catalog client-side (case-insensitive substring on clip **Name** OR **Category**; empty = show all). Re-renders the browser on every keystroke.
- **States to design:** empty (placeholder shown), focused, has-text. Lives in a row: **search icon (left) · entry (fills) · Stop button (right).**
- **Current label text:** placeholder `Search clips by name or category…`

---

### 5.3 Stop-all button

- **Purpose:** Instantly silence everything playing. The only "playback is happening" affordance in the app.
- **Slot-in binding:** `Player.StopAll()` — silences every clip on both the →Discord and →headset paths at once; the user's live mic passthrough is unaffected.
- **States to design:** default (always enabled/visible in the search row), pressed/hover. Use **Danger** importance (red) + a stop icon. Always visible above the scrolling list.
- **Current label text:** `Stop`

---

### 5.4 Category-grouped clip grid

- **Purpose:** Browse the full library, grouped by category, in one vertical scroll.
- **Slot-in binding:** renders the read-only `catalog.Library` (categories → clips). **No mutation** — pure display + the per-cell actions below.
- **States to design:**
  - **Section header** per non-empty, filter-matching category: bold `Pretty Category Name  (N)` where **N is the count of currently-visible (filtered) cells**, not the total. (e.g. `films  (12)`.) A separator follows each section.
  - **Dense section** (39 tiles, "games") and **sparse section** (2 tiles, "game-clips") must both look right — the grid reflows responsively on resize.
  - **No-results:** when nothing matches, a centered label `No clips match your search.` replaces all sections (favourites hidden too).
- **Tile sizing today:** cells are a fixed ~168×40 logical px in a reflowing GridWrap. You may propose a different cell size/shape (custom widget) — note it.

---

### 5.5 Clip cell (play button + favourite star)

- **Purpose:** One tile = one clip. Tap to play; star to favourite.
- **Slot-in binding (play):** `Player.TriggerGain(id, gain)` where `gain` comes from `VolumeController.Clip(id)` (default 1). Playing also **selects** the clip into the per-clip volume slider (§5.8).
- **Slot-in binding (star):** present **only** when FavoritesController is wired. Reads `FavoritesController.IsFavorite(id)`; tap calls `ToggleFavorite(id)` then re-renders (both the star and the pinned Favourites section update live).
- **States to design (per cell):**
  - **Idle / not favourited** — play affordance + hollow star `☆`.
  - **Favourited** — filled star `★` (also mirrored in the pinned section).
  - **Hover / pressed** (decorative; a press-flash is fine as a custom widget).
  - **No-favourites build** — cell is just the play button, no star.
  - **[NEEDS NEW BACKEND] "now playing"** — there is no per-tile playback state today; if you design one, flag it.
- **Display:** the clip's **Name** (filename without extension, prettified) on the tile face, leading-aligned, with a play icon. ID/Category are internal keys (not shown).
- **Current label text:** the star glyphs are `★` / `☆` today (Low importance). You may replace these with custom icons.

---

### 5.6 Pinned Favourites section

- **Purpose:** Fast access to the user's most-used clips, above all categories.
- **Slot-in binding:** `FavoritesController.Favorites()` → clip IDs **in pinned (saved) order**, filtered by the current search. The same play+star cells as the grid.
- **States to design:**
  - **Visible** — bold header `★ Favourites  (N)` over a GridWrap of cells (pinned order, not alphabetical).
  - **Hidden** — when there are 0 matching favourites, or no FavoritesController. (Removed clips whose file is gone are skipped silently.)
- **Current label text:** header `★ Favourites  (N)`.

---

### 5.7 Volume card — three mixer sliders

- **Purpose:** Independently balance the three "who hears what" levels.
- **Slot-in binding:** `VolumeController` — setters push to the engine **and persist immediately**; getters seed at startup. The three levels are **independent**:
  - **Mic** → `SetMic` / `Mic` — how loud the user's voice is to Discord.
  - **Master** → `SetMaster` / `Master` — the soundboard level **others** hear in Discord.
  - **Monitor** → `SetMonitor` / `Monitor` — the soundboard level **the user** hears locally.
- **States to design:** each row = icon + bold label + slider + percent readout. Sliders are **0…150 % linear gain** (step 1 %), readout like `100%`. Design the slider at 0 %, 100 %, and 150 %.
- **Card chrome:** a Card titled **`Volume`** with subtitle **`Levels apply instantly and are saved on exit.`**, an italic wrapped caption (below), then the four rows.
- **Current label text (these are TEST-ASSERTED constants — reword deliberately):**
  - `Microphone — your voice`
  - `Soundboard — what others hear in Discord`
  - `Soundboard — what you hear`
  - Caption: `Your mic and "what you hear" stay local to you. "What others hear" is the only level Discord transmits.`

---

### 5.8 Volume card — per-clip slider + selection

- **Purpose:** Trim the loudness of one specific clip (the last one played/clicked).
- **Slot-in binding:** `VolumeController.SetClip(id, gain)` / `Clip(id)`. Selection is set by playing a clip (§5.5). **Note:** SetClip persists per-clip gain but does **not** push live to the engine — it's read at the clip's next trigger.
- **States to design (two distinct states for ONE control):**
  1. **Empty / disabled** — slider greyed/disabled, name label reads `No clip selected` (italic).
  2. **Selected / enabled** — slider enabled and seeded from the clip's saved gain; name label reads `Selected: <Name>`.
- **Current label text:** row label `This clip`; name label `No clip selected` → `Selected: <name>`.

---

### 5.9 Audio tab — input-mode radios

- **Purpose:** Choose how the mic gate behaves.
- **Slot-in binding:** `AudioController.MicMode()` / `SetMicMode(mode)` — labels map 1:1 to config strings `vad | ptt | always | mute`.
- **States to design:** a 4-option exclusive radio group, with a bold header above. Design selected/unselected.
- **Current label text (TEST-ASSERTED constants):** header `Input mode`; options in order: `Voice-activated`, `Push-to-talk`, `Always-on`, `Mute`.

---

### 5.10 Audio tab — gate sensitivity slider + live mic-open meter

- **Purpose:** Set the gate threshold and **see the mic open/close in real time.**
- **Slot-in binding:** slider → `GateSensitivity()` / `SetGateSensitivity()` (0…1, shown as %). Meter → polls `GateLevel()` (0…1) at ~20 Hz (every 50 ms).
- **States to design:**
  - **Slider** — bold header `Gate sensitivity`, a 0…1 slider with a trailing percent readout.
  - **Meter — gate CLOSED:** dim/primary-coloured "light," bar near 0.
  - **Meter — gate OPEN:** the light glows **success-green** once level > 0.5, bar high. **This animates live** — design both endpoints and imply the motion.
  - This is the **#1 candidate for a custom-canvas widget** (gradient bar or arc/ring gauge). The stock version is a horizontal progress bar + a 16 px circle that switches colour.
- **Current label text:** header `Gate sensitivity`; meter caption `Mic open`.

---

### 5.11 Audio tab — processing toggles + captions

- **Purpose:** Toggle the mic-cleanup chain. Each applies instantly and persists.
- **Slot-in binding (four independent checkboxes):**
  - `NoiseSuppression()` / `SetNoiseSuppression` — *offered always; silently a no-op if the build lacks RNNoise (the UI does not surface that today).* 
  - `AGC()` / `SetAGC`
  - `Ducking()` / `SetDucking`
  - `ForceThrough()` / `SetForceThrough` (separated below the first three)
- **States to design:** each checkbox on/off; the card with two italic wrapped captions.
- **Card chrome:** Card titled **`Audio`**, subtitle **`Mic processing — applies instantly and is saved on exit.`**
- **Current label text (TEST-ASSERTED constants):**
  - `Noise suppression (RNNoise)`
  - `Automatic gain control`
  - `Duck soundboard while talking`
  - `Force sounds through Discord voice-activity gate`
  - Top caption: `Clean, gated, leveled voice — applied to your microphone only, before it reaches Discord. Soundboard clips are never processed. Leave Discord's own noise suppression OFF.`
  - Force caption (under the force toggle): `Adds an inaudible voiced tone so Discord's voice-activity gate stays open and clip onsets are not clipped. Does not defeat Krisp.`

---

### 5.12 Audio tab — PTT hotkey **[PARTIAL / FLAG]**

- **Purpose:** Show/edit the push-to-talk key combo.
- **Slot-in binding:** `AudioController.PTTHotkey()` / `SetPTTHotkey(combo)` exist and persist, but **no widget binds them today**, and a changed combo **takes effect only on the next launch.**
- **Verdict:** You may design a read-only display or an edit field bound to these methods — but **label it "applies after restart."** A live key-recorder that re-binds without restart is **[NEEDS NEW BACKEND]**. Do not treat as baseline.

---

### 5.13 System-tray menu + icon

- **Purpose:** The app's persistent anchor; reopen or quit.
- **Slot-in binding:** menu items call show-window / quit; clicking the tray icon reopens the window. Quit is the only exit path.
- **States to design:** the tray menu (two items + separator) and the tray **icon** (must read at 16 px). The window and tray share one icon.
- **Current label text:** menu title `SoundBoard`; items `Open SoundBoard`, `Quit`.

---

### 5.14 Window shell, tabs & close-to-tray (structure, no widget)

- **Tabs:** top-located AppTabs — `Soundboard` (music icon) and `Audio` (settings icon). Pure layout; restyle the tab bar via theme + your icon set.
- **Close-to-tray:** closing the window records its size and **hides** it (app keeps running). No widget; defines the lifecycle your design must assume.
- **Window size persistence (`WindowStore`):** size is saved/restored; default 760×600. No visible widget — relevant only if you change default dimensions or propose a "reset size" affordance (the latter would be **[NEEDS NEW BACKEND]**).

---

## 6. Data model & scale

**The atom — a Clip:** has an **ID** (`<category>/<basename>`, internal), a display **Name** (filename without extension, with underscores/dashes turned into spaces and trimmed — e.g. `airhorn_loud-v2.mp3` → `airhorn loud v2`), and a **Category** (its folder name). Supported formats: mp3, wav, flac, ogg. There is **no rename/tag/metadata editing** in the UI — Name is display, ID/Category are keys.

**Real content scale (design to these concrete numbers):** **the clip library across 12 categories.** Per-category counts:

| Category | Display label | Clips |
|---|---|---|
| games | Games | **39** |
| movies | Movies | 36 |
| films | films | 35 |
| scifi | Scifi | 28 |
| reactions | Reactions | 14 |
| wow | Wow | 13 |
| memes | Memes | 12 |
| tv | Tv | 12 |
| game-clips | game-clips | 9 |
| game-clips | a game | 6 |
| game-clips | a game | 6 |
| game-clips | game-clips | **2** |

Note the **extreme spread**: one section has 39 tiles, another has 2. Your grid must look intentional at both ends. All ~200 tiles live in **one vertical scroll**.

**Category ordering & labels:** categories are **alphabetical** (game-clips, games, memes, movies, game-clips, reactions, game-clips, scifi, game-clips, films, tv, wow). The label is the category with dashes/underscores → spaces and the first letter capitalised (so `films` → `films`, `game-clips` → `game-clips`). Within a category, clips are sorted by ID. Each header's count is the **visible/filtered** count, live-updating as the user types.

**Favourites:** an **ordered, pinned** list of clip IDs (saved order, not alphabetical), persisted on exit. Pinned section above all categories; star toggle on every cell; toggling appends.

**Volume model:** four numbers — three independent levels (Mic, Master, Monitor) plus a per-clip multiplier (applied on top of Master and Monitor). All linear, 0…150 %, 1.0 = unchanged, shown as a percent.

**The 200+ browsability requirement:** the design must keep 212+ clips findable in ~1–2 seconds. Today's mitigations are **live search (name+category)**, **pinned favourites**, **per-section visible counts**, and **global Stop**. The library is **not** virtualized and re-renders every section per keystroke (fine at 212; flag any push toward 500+). **Design opportunities (optional, label structural ones):** category jump/anchor nav, collapsible sections, recently-played, an alphabetical index. Migrating the grid to a virtualized `List` is a structural backend-friendly change worth flagging if you lean denser.

---

## 7. State inventory (the full matrix to design)

Design **every** cell below. Where a state is a known gap, it's flagged.

**A. Setup / routing (banner + app):**
- A1 Cable **absent** (warning, `Install / Fix audio routing` button).
- A2 Cable **present, not engaged** (warning, `Engage routing` button).
- A3 **Engaged / ready** (green, no button, `Audio routing active — …`).
- A4 Action **in-progress** (modal infinite progress).
- A5 Action **error** (error dialog).
- A6 Engage **success** (info dialog).
- A7 Install **success** (confirm dialog offering app restart).
- A8 **Degraded "setup mode"** — full window runs but **no audio at all** (engine never started). Clips/sliders/mic controls are present but **inert**; they are **not greyed out today**. Decide how (or whether) to signal "sound disabled until routing is fixed" — the only current signal is the warning banner. (Live disabling = **[NEEDS NEW BACKEND]**, unless keyed purely off banner state.)

**B. Clip cell:**
- B1 Idle, not favourited (`☆`).
- B2 Favourited (`★`, mirrored in pinned section).
- B3 Hover / pressed (decorative).
- B4 **[NEEDS NEW BACKEND]** "now playing" (no backend signal today).

**C. Mic (Audio tab):**
- C1 Mode = Voice-activated; C2 = Push-to-talk (adds a held-key/PTT-down nuance); C3 = Always-on; C4 = Mute.
- C5 Gate **closed** (light dim/primary, bar ~0).
- C6 Gate **open** (light green, bar high) — animated live.

**D. Processing toggles:** each of the four on/off (and the "RNNoise no-op" edge — currently invisible to the user; you may propose surfacing it, flag if it needs backend).

**E. Volume:**
- E1 Each slider at 0 % / 100 % / 150 %.
- E2 Per-clip slider **empty/disabled** (`No clip selected`).
- E3 Per-clip slider **selected/enabled** (`Selected: <name>`).

**F. Search / browser:**
- F1 Empty filter (all sections + favourites).
- F2 Typing (sections re-render, empty categories dropped, counts update).
- F3 No-results (`No clips match your search.`, all sections hidden).

**G. First-run / fresh config:**
- No favourites (no pinned section); all three mixer levels at 100 %; per-clip disabled; mode = Voice-activated; gate sensitivity ~15 %; all toggles OFF; window 760×600 centered.
- If VB-CABLE present → routing **auto-engages silently** at startup (banner already green).
- If absent → lands directly in warning-banner setup mode (A1/A8).
- There is also a first-run **Discord-settings checklist** concept (set Discord input to CABLE Output; turn OFF Krisp / Echo / AGC / auto-sensitivity) you **may** surface as an onboarding panel — flag it as a new surface if you design it.

---

## 8. Key user flows

1. **First-run setup (install / engage VB-CABLE).** App opens → banner shows cable status. If present-not-engaged → `Engage routing` → progress → `Routing engaged` → banner turns green. If absent → `Install / Fix audio routing` → UAC elevation → progress → `VB-CABLE installed` confirm → app restart → green. (If present at launch, it auto-engages silently.)
2. **Play a sound.** Soundboard tab → optionally search → tap a clip cell → it plays into Discord and is **selected** into the per-clip slider.
3. **Favourite.** Tap a cell's star (`☆`→`★`) → it appears in the pinned `★ Favourites` section instantly.
4. **Set volumes.** Volume card → drag Mic / Master / Monitor (independent) → readouts update, levels apply instantly + persist. Play a clip, then nudge `This clip`.
5. **Configure mic processing.** Audio tab → pick an Input mode → set Gate sensitivity while watching the live `Mic open` meter → toggle Noise suppression / AGC / Ducking / Force-through.
6. **Push-to-talk.** Pick `Push-to-talk` mode. (The PTT key combo is config-only today and rebinds on next launch — see §5.12; design a combo field only as a flagged, "applies after restart" extra.)
7. **Stop-all.** Hit the red `Stop` button → every playing clip silences instantly; mic passthrough unaffected.
8. **Hide / reopen / quit.** Close the window → hides to tray (keeps running). Click tray icon or `Open SoundBoard` → reopens. `Quit` → exits.

---

## 9. Visual & brand direction

### Current real palette (stock Fyne dark — what ships today)

| Token | Current dark value | Notes |
|---|---|---|
| Background | `#171718` | near-black |
| HeaderBackground | `#1B1B1B` |  |
| Button / Menu | `#28292E` |  |
| InputBackground | `#202023` |  |
| Foreground | `#F3F3F3` |  |
| Placeholder | `#B2B2B2` |  |
| Disabled | `#39393A` |  |
| **Primary (accent)** | `#296FF6` | **Fyne default blue — NOT the brand blurple** |
| Success | `#43F436` |  |
| Warning | `#FF9800` |  |
| Error | `#F44336` |  |
| Separator | `#000000` |  |
| Hover / Pressed | white @6% / white @40% |  |

> **Important:** the **`#5865F2` "blurple" accent** the brand implies lives **only** in the embedded app icon (a flat blurple square). The live UI never shows it because no custom theme overrides `Primary`. **Recommended first move:** ship a custom theme that sets `Primary` = `#5865F2` (with `ForegroundOnPrimary` = white) so the app finally matches its own icon. This single change is the highest-impact, lowest-risk win.

### Typography

Stock Fyne sans today (Noto-ish), with Bold/Italic applied per label. You may swap in **one** cohesive family (e.g. an Inter / Whitney-like geometric humanist sans) supplied as embeddable `.ttf` (regular + bold, optionally italic). Define a clear type scale across the available size tokens (caption / body / subheading / heading).

### Iconography

Define a **cohesive icon set** mapped to the theme's icon names (search, play, stop, media-music for the Soundboard tab, settings/gear for the Audio tab, volume-up/down, download for the banner action, confirm/check for the engaged banner) plus the **favourite star** glyphs (replace `★`/`☆` with custom icon resources if you wish). Plus the **app/tray icon** (Section 10).

### Creative freedom vs. must-respect rails

- **Free:** full palette (all ~30 color names, dark + light), fonts, all icons, spacing (via the one padding token), the accent, the two custom widgets (clip cell + mic meter), tab-bar styling, importance choices per button.
- **Must respect:** rectangular cards/buttons (no global rounding); flow layout (no absolute positioning); stock widgets for banner/sliders/checks/radio/tabs/dialogs; dark-first; a tray icon legible at 16 px; the test-asserted label strings unless deliberately rewording.

### Accessibility

- **Contrast:** meet **WCAG AA** (4.5:1 body text, 3:1 large text / UI components) in **both** dark and light. Watch the green `Success` and warning colours against dark backgrounds.
- **Hit targets:** clip cells and controls comfortably tappable; today's cells are ~40 px tall — don't go below ~36 px effective height. Star toggle must be reliably hittable next to the play area.
- **Keyboard:** the app relies on Fyne's built-in focus/keyboard handling — design visible **focus states** (the `Focus` color token) for entry, sliders, checks, radio, buttons. Don't introduce interactions that require a mouse-only gesture.
- **Colour is not the only signal:** the gate meter pairs colour (green) with a bar level; keep that redundancy (don't rely on hue alone for the banner state either — pair with icon + text).

---

## 10. Deliverables expected from the designer

1. **High-fidelity mockups for every screen AND every state** in Sections 5 and 7 — including: banner ×3 resting states + 4 dialog states; Soundboard with favourites + dense/sparse sections; search empty/typing/no-results; Volume card with per-clip empty vs selected and sliders at 0/100/150 %; Audio tab with each mode, gate closed vs open, each toggle; first-run; degraded setup-mode treatment; tray menu. (Dark first; provide light equivalents.)
2. **Component / style guide:** the **token sheet** — exact hex for all ~30 color names (dark + light), the size-token values you change (padding, text/heading/subheading/caption, separator, any radius), the font files + type scale, the spacing scale (expressed against the single padding token), the full component inventory with states, and the **icon set**.
3. **Redline / spacing specs** expressed in flow terms (which container, what padding value, alignment) — **not** absolute coordinates.
4. **App / tray icon set:** one master PNG that reads as a real glyph (not a flat square) and remains legible at **16 px** (tray) up through window/taskbar sizes.
5. **A Fyne-implementability note per non-standard element:** for anything that isn't a plain themed stock widget, state explicitly whether it is (a) a global theme token change, (b) one of the two sanctioned custom widgets (clip cell / mic meter), or (c) **[NEEDS NEW BACKEND]**. Anything in category (c) must be separable so we can ship the rest without it.

---

## 11. Acceptance criteria

The design "slots in" when **all** of the following hold:

1. **Coverage:** every screen, component, and state in Sections 4, 5, and 7 has a mockup (including each variant/dialog and the dense/sparse grid extremes).
2. **Capability-bound:** every interactive element maps to a method in the Section 5 capability list, **or** is explicitly tagged **[NEEDS NEW BACKEND]** and is removable without breaking the rest.
3. **Fyne-buildable:** the design is achievable as **one custom theme** (palette + fonts + icons + size tokens) over stock widgets, plus **at most** the two sanctioned custom widgets (clip cell + mic meter). No requirement implies CSS, per-widget styling, globally rounded cards/buttons, or stock-widget gradients/animation.
4. **Token deliverable present:** a complete token sheet (all color names dark+light, changed size tokens, font files) exists and is internally consistent.
5. **Lifecycle-safe:** the layout reflows gracefully from ~600 px wide to large, and reads correctly when the window is hidden/reshown from the tray (no state that depends on the window having "just opened").
6. **Labels accounted for:** any change to a test-asserted label (Sections 5.7, 5.9, 5.11) is intentional and listed, so the corresponding code constants can be updated.
7. **Accessibility:** AA contrast in both themes, ≥~36 px effective hit targets, visible focus states, and no colour-only signals.
8. **Tray icon:** a single PNG legible at 16 px is delivered.

---

## 12. Out of scope / non-goals

- **New backend features.** No clip-trigger-hotkey manager, no live PTT re-binding, no per-tile live playback state, no rename/tag/metadata editing, no audio-format work — unless separately scoped from a **[NEEDS NEW BACKEND]** flag.
- **Non-Windows platforms.** Windows 11 only; no macOS/Linux/mobile layouts.
- **Multi-window / docked / MDI designs.** One resizable window + a tray icon. No secondary windows beyond the existing modal dialogs.
- **Web / Figma-only interactions.** Anything that assumes CSS, arbitrary margins, absolute positioning, per-widget styling, globally rounded surfaces, or stock-widget gradients/animation is out of scope by construction (it isn't buildable in Fyne).
- **Discord-side UI.** We do not design anything inside Discord; at most we may surface a read-only checklist of Discord settings the user should set (flagged as a new surface if designed).
- **Localization / RTL.** English only; current strings are the baseline.
- **Sound/clip content curation.** The library is whatever the user drops in the `sounds/` folder; we design the chrome, not the content.

---

*End of specification. Everything above is grounded in the shipping code: `internal/ui/ui.go` (interfaces, tray, close-to-tray), `internal/ui/layout.go` (banner, search, grid, Stop), `internal/ui/volume.go` (volume card), `internal/ui/audio.go` (Audio tab, gate meter), and `main.go` (adapters wiring the controllers to the real engine).*
