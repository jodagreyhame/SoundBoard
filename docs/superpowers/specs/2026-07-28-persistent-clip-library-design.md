# Persistent, user-configurable clip library

**Date:** 2026-07-28
**Status:** Approved for implementation

## Problem

`soundsRootW()` (`backend.go:455`) resolves the clip library by looking for `<exe dir>/sounds`,
then `<cwd>/sounds`, and otherwise creating `sounds/` beside the executable — discarding the
`MkdirAll` error with `_ =`. Three consequences:

1. The library lives next to the `.exe`. Move, re-download, or reinstall the app and the library is
   orphaned.
2. Under `Program Files` the `MkdirAll` fails, the error is discarded, and the user gets an empty
   grid with no explanation.
3. The `cwd` fallback means the launch directory decides which library you get. Observed in
   practice: a Wails build logged `0 clips indexed from C:\Users\…\AppData\Local\Temp\sounds`.

## Decisions

| Decision | Choice |
|---|---|
| Default location | `<Documents>\SoundBoard\`, categories directly inside |
| Config + log | Unchanged, stay in `os.UserConfigDir()/soundboard/` |
| First run | Create silently, then a dismissable in-app notice. No new modal — VB-CABLE consent already blocks first run |
| Legacy `<exe dir>/sounds` | Ignored. Clean break, no migration |
| Applying a folder change | Live rescan, no restart, plus a general "Reload library" control |
| Empty-folder guidance | Seed `examples\README.txt` on creation so the required layout is self-evident |

Out of scope, deliberately: a filesystem watcher (`fsnotify`) for auto-reload, decode-cache
carry-over across reloads, and any migration of legacy clips.

## Architecture

### Single library owner

The library is owned by **one** `atomic.Pointer[catalog.Library]` on `Backend`. `Engine` reads
through an accessor onto that same atomic rather than holding its own field.

This is load-bearing. `Engine.lib` (`audio.go:167`) is not the only handle: `GetState` builds the
entire clip grid from `Backend.lib` (`app.go:310-312`). Swapping only the engine's copy leaves the
UI rendering the old folder forever; maintaining two atomics in step is a bug factory.

Safety of a live swap, verified against the source:

- The RT callbacks never read the library. The three read sites (`audio.go:799`, `:806`, `:867`) are
  all on caller goroutines.
- In-flight voices are unaffected. `drainInto` (`audio.go:1019`) copies the `[]float32` slice header
  into a `clipCursor` at cursor-creation time and never touches the `Clip` or `Library` again; the
  backing array is kept alive by the cursor. `Clip.PCM` is fully written before the channel send, so
  the RT-thread read is safely published.
- Cross-library `PCM` races are impossible: `catalog.New` allocates fresh `*Clip` values, so old and
  new libraries share no `Clip` objects.

### `internal/library` (new)

Owns path resolution and nothing else. Split by build tag (`library_windows.go` /
`library_other.go`) exactly as `internal/apm` already does.

```go
func DefaultDir() (string, error)
func Resolve(configured string) (path string, isDefault bool, err error)
func Ensure(path string, isDefault bool) error
func Validate(path string) error
```

`DefaultDir` must use the Windows Known Folder API, **not** `os.UserHomeDir() + "Documents"`. Under
OneDrive Known Folder Move — on by default on many OEM Windows 11 installs — the real Documents
folder is `%USERPROFILE%\OneDrive\Documents`, so the naive join silently reads and writes somewhere
Explorer never shows the user.

`golang.org/x/sys` is already a direct dependency (`go.mod:13`) and exposes what is needed:
`windows.KnownFolderPath(windows.FOLDERID_Documents, windows.KF_FLAG_DEFAULT)`. The syscall is
injected behind a function value so the fallback path is unit-testable; `os.UserHomeDir()` is used
only as a **logged** fallback when the syscall fails.

Localised folder names need no handling: since Vista the on-disk name is always `Documents`, only
the display name is localised.

`Ensure` creates the folder only when it is the default, and seeds `examples\README.txt` describing
the `<category>\clip.wav` layout. It never silently creates a user-chosen path. Its `MkdirAll` error
propagates to a **visible** UI error, not just a log line.

### `Validate` rejection matrix

| Case | Verdict | Reason |
|---|---|---|
| Relative path | Reject | `os.DirFS(rel)` is CWD-dependent — the bug being fixed |
| Missing / not a directory | Reject | Probe with `os.ReadDir`, not `os.Stat`: `Stat` succeeds on a directory you cannot list |
| No read permission | Reject | Only `os.ReadDir` detects it |
| Inside the exe directory | Reject | Recreates the moved-exe problem and fails under `Program Files` |
| The config dir | Reject | Would index alongside `config.json` and `soundboard.log` |
| Drive root, `%USERPROFILE%`, Documents itself | Warn, allow | Survivable with depth pruning; fatal without it |
| UNC `\\server\share` | Allow, probe with timeout | `os.DirFS` handles UNC; the real risk is a 30–60 s SMB stall blocking the UI. Reject *unreachable*, not UNC as such |
| Path > ~240 chars | Warn, allow | Go transparently `\\?\`-prefixes absolute paths, but `explorer.exe` chokes. Leave headroom for `<category>\<file>.<ext>` |

Reserved device names (`CON`, `NUL`, `COM1`…) cannot exist as directory names; no handling needed.

### `internal/catalog` changes

**Walk the FS root, not a hard-coded `"sounds"`.** `catalog.New` currently does
`fs.WalkDir(fsys, "sounds", …)`, which would require every user-chosen folder to literally be named
`sounds`. Callers pass `os.DirFS(clipFolder)` and categories sit directly inside.

**Fault-tolerant walk.** Today any per-entry error aborts the whole walk (`catalog.go:85-87`) and
`catalog.New` returns `nil, err`. Verified: a subdirectory that vanishes mid-walk produces zero
indexed clips, including from healthy sibling directories. Against a user-chosen folder this fires
for OneDrive cloud-only placeholders, ACL'd subfolders, antivirus locks, and transient network
blips. The callback will log the error and return `nil` for files / `fs.SkipDir` for directories;
only a failure to open the **root** fails `catalog.New`.

**Depth pruning.** With the walk anchored at an arbitrary folder there is no depth bound. Walking a
real `Documents` visits ~8,900 entries. Prune with `fs.SkipDir` at depth ≥ 2 and skip
dot-directories *before* descending (`catalog.go:109` currently skips them only after visiting).
Accept a `context.Context` so a scan against a stalled network path can be cancelled.

**Explicit `Empty()` constructor**, replacing the `emptySoundsFS` shim (see below).

**Diagnostics, per the fail-loudly rule.** Log when:
- two files in one category collide on a single ID (`airhorn.wav` + `airhorn.mp3` both yield
  `memes/airhorn`; `byID` keeps the last while `byCat` keeps both, so the grid shows two tiles and
  one fires the other's audio);
- a depth-1 entry is a reparse point (junction/symlink). `fs.WalkDir` does not follow these, so
  `mklink /J memes D:\audio\memes` is otherwise skipped in total silence;
- audio files are found at the folder root with no category directories.

**`Library.Load()` (`catalog.go:173`) writes `clip.PCM` with no lock**, racing `EnsureDecoded`
(which correctly holds `decMu`). Route `Load` through `EnsureDecoded` or take `decMu`; CI runs
`go test -race ./internal/catalog/...` and will eventually flag it.

### `backend.go` changes

Delete `soundsRootW()` and `emptySoundsFS`. The latter is not merely redundant, it is broken:
`emptyDir.ReadDir` returns `(nil, io.EOF)` (`backend.go:543`), but the `fs.ReadDirFile` contract
requires all entries and a **nil** error for `n <= 0`. So `fs.WalkDir` propagates `EOF`,
`catalog.New` returns `nil, err`, the error is discarded by `_`, and `backend.go:88` panics
dereferencing a nil `Library`. Latent today; routinely reachable once the folder is user-chosen.

Replaced by `library.Resolve` + `catalog.New`, with the degraded path using `catalog.Empty()` and
handling its error explicitly.

### `internal/config` changes

Two new prefs on the existing JSON config: `ClipFolder string` (empty means "use the default") and
`ClipFolderNoticeSeen bool`.

### `app_library.go` (new)

Four Wails-bound methods, in a new file because `app.go` is already 1,124 lines and `backend.go`
552 — both over the project's ~500-line guideline.

| Method | Behaviour |
|---|---|
| `ClipFolder()` | Current path, is-default, exists, category/clip counts, last error |
| `ChooseClipFolder()` | Picker → validate → scan → swap → persist → emit |
| `ReloadLibrary()` | Scan current path → swap → emit |
| `OpenClipFolder()` | Reveal in Explorer |

`ChooseClipFolder` must guard against invoking `runtime.OpenDirectoryDialog` with no visible owner
window; the window is frameless and can be hidden to tray (`app.go:856`).

## Data flow

```
Change… → OpenDirectoryDialog → Validate(path)
        → catalog.New(ctx, os.DirFS(path))        ← scan FIRST
        → [success] swap atomic → persist → flushPersist() → emit "libraryChanged"
        → [failure] no state changes; error string returned for display
```

Scanning before persisting matters: the reverse order leaves `config.json` pointing at a folder the
running app is not using, so the *next* launch boots into a broken library with no obvious cause.
`flushPersist()` bypasses the 400 ms persist debounce (`app.go:926`) — a folder change is not a
slider drag.

`ReloadLibrary` is the same path minus the picker and the persist.

**Startup:** `Resolve(cfg.ClipFolder)` → if default and missing, `Ensure` creates and seeds it → if
a *user-chosen* folder is missing (unmounted drive, deleted), fall back to the default **loudly**:
log it and surface a visible error state, never a silently empty grid.

**Frontend:** one new `EventsOn("libraryChanged")` handler doing
`call("GetState").then(ingest).then(render)` — `ingest` (`app.js:119`) already normalises a full
snapshot, so no new rendering path is required. Bindings are reached via
`window.go.main.App.<Method>`, so the gitignored `frontend/wailsjs` needs no regeneration. Plus the
first-run notice, the Reload control, and error surfacing for a failed scan.

## Error handling

Three silent failures are deleted outright and none are added:

- `_ = os.MkdirAll` (`backend.go:472`) → error propagated to a visible UI error.
- `lib, _ = catalog.New(emptySoundsFS{})` (`backend.go:84`) → explicit `catalog.Empty()`.
- A clip ID that misses in `TriggerGain` (`audio.go:800-802`) returns silently → now logged, so a
  user whose hotkey stopped working after a folder change has a diagnostic.

## Testing

- **`internal/library`** — table tests over `Resolve` and the `Validate` matrix. Pure path logic: no
  audio device, no filesystem beyond `t.TempDir()`. The Known Folder syscall is injected, so the
  fallback path is testable off-Windows.
- **`internal/catalog`** — a walk where one subdirectory errors, asserting siblings still index
  rather than the library vanishing; depth pruning; extension-collision detection; `Empty()`.
- **`internal/audio`** — `SetLibrary` under load: swap while `Play` is in flight, assert in-flight
  voices finish and no race fires. CI already runs `-race` over `./internal/audio/...
  ./internal/catalog/...`.
- The 20 `"sounds…"` literals across 5 test files (`catalog_test.go` ×10, `app_test.go` ×5,
  `audio/{gain,nowplaying,engine_concurrency}_test.go` ×5) updated for the new walk root.

`TestPlayingSetSnapshotUnderConcurrentPublish` is already flaky under `-race` — it failed on PR #2
and locally on 2026-07-28. Its retry budget should be fixed as part of this work, or every future
failure in this area will look like a regression from it.

## Known pre-existing issues this touches

- **`settings.Volumes.PerClip` data race.** The hotkey callback reads it via `clipGainW`
  (`backend.go:181`) with no lock while `App.SetClipVolume` (`app.go:474-477`) writes it under
  `a.lcMu`. Adding `ClipFolder` to the same `Settings` struct widens the same hole. Either guard
  `Settings` with its own mutex or snapshot what the hotkey path needs.
- **`clipRegistry.intern` never evicts** (`nowplaying.go`); `r.ids` grows monotonically across
  reloads. Harmless — same ID maps to the same index, so now-playing labels stay correct — but worth
  a comment so nobody "fixes" it into a correctness bug.
- **Reload discards decoded PCM.** Fresh `*Clip` objects mean every clip re-decodes on next play.
  Acceptable; do not describe Reload as free.
