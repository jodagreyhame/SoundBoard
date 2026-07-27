# Contributing to SoundBoard

Thanks for your interest in SoundBoard. This guide covers the basics of contributing. If something is unclear, open an issue or discussion and we'll update this doc.

All participants are expected to follow the [Code of Conduct](./CODE_OF_CONDUCT.md).

## Before you start

- **Small fixes** (typos, broken links, doc clarity): just open a PR. No issue needed.
- **Bug fixes**: open an issue first with a reproduction if one doesn't already exist. Link the PR to the issue.
- **New features or significant changes**: open an issue or discussion first to talk through the approach. Maintainer time is finite — a 10-minute alignment chat can save hours of rework.
- **Security vulnerabilities**: do not open a public issue. Follow [SECURITY.md](./SECURITY.md) instead.

## Development setup

1. Fork the repo on GitHub
2. Clone your fork:
   ```bash
   git clone https://github.com/YOUR-USERNAME/SoundBoard.git
   cd SoundBoard
   ```
3. Add upstream remote so you can sync:
   ```bash
   git remote add upstream https://github.com/jodagreyhame/SoundBoard.git
   ```
4. Verify the baseline works before you change anything:
   ```bash
   CGO_ENABLED=1 go test -count=1 ./...
   ```

### Platform and toolchain requirements

SoundBoard is **Windows-only** and cannot be built or meaningfully tested elsewhere. The
routing is WASAPI + VB-CABLE + the undocumented `IPolicyConfig` COM interface, and the audio
processing module is a Windows DLL embedded at build time.

- **Windows 10/11** (developed and verified on Windows 11).
- **Go 1.25.5+**.
- **`CGO_ENABLED=1` with MinGW-w64 gcc on `PATH`** (`CC=gcc`, no clang/MSVC override needed).
  cgo is required by `malgo` (miniaudio) and by the vendored RNNoise C sources. A
  `CGO_ENABLED=0` build compiles but silently degrades the denoiser to a no-op.
- **Wails v2 CLI** — optional. `wails build` is the shipping path, but a plain
  `go build -ldflags "-H=windowsgui" -o soundboard.exe .` works without it.
- **Edge WebView2 runtime** — preinstalled on Windows 11.

### Please do not submit audio files

The `sounds/` library is **user-supplied and deliberately not bundled**. PRs adding audio
clips will be declined regardless of content — we cannot verify the licensing of third-party
audio, and shipping it would expose the project and its users. Contribute code and docs; bring
your own clips.

### Testing notes

Audio quality is subjective and hardware-dependent. Automated tests cover the DSP maths, ring
buffers, gate hysteresis, config round-trips and real-time race-safety — they cannot tell you
whether something *sounds* right. If your change affects the audio path, say so in the PR and
describe what you heard on a real call.

## Making a change

1. Create a topic branch from `main`:
   ```bash
   git checkout -b feat/short-description
   # or: fix/thing-that-broke / docs/what-is-unclear / refactor/area
   ```
2. Keep the change focused. One concern per PR is easier to review and easier to revert.
3. Write or update tests alongside code changes. A PR that adds behavior without tests will usually be asked to add them before merge.
4. Run the checks locally before pushing:
   ```bash
   go vet ./...
   CGO_ENABLED=1 go test -count=1 ./...
   ```
5. Keep commits reasonably clean. Squash noise, but don't squash away meaningful steps — commit history is documentation.

## Commit messages

Follow the [Conventional Commits](https://www.conventionalcommits.org/) style:

```
type(scope): short subject, imperative mood, no trailing period

Optional body explaining WHY (not what — the diff shows what).

Refs #123
```

Common types: `feat`, `fix`, `docs`, `refactor`, `test`, `chore`, `perf`, `ci`.

Subject under 72 characters. Body wrapped at ~80.

## Opening a pull request

- Push your branch to your fork
- Open a PR against `jodagreyhame/SoundBoard:main`
- Fill out the PR template (auto-populated from `.github/PULL_REQUEST_TEMPLATE.md`) — in particular the *how to test* section
- Link any related issues: `Closes #123` or `Refs #123`
- CI must be green before a maintainer reviews. If CI is broken, fix it first.

## Review & merge

- A maintainer will review, usually within a few days. Gentle nudge after a week if it's gone quiet — the notification might have been lost.
- Feedback is meant to improve the change, not to gatekeep. Disagreement is fine; we'll discuss trade-offs.
- Once approved, a maintainer merges. We generally use **squash-merge** to keep `main` linear; your commit message gets the chance to represent the whole PR.

## What we welcome

- Bug reports with reproduction steps
- Fixes for those bugs
- Documentation improvements (including fixing stale content)
- Performance improvements (with benchmarks)
- Better error messages
- Tests for uncovered paths
- Accessibility improvements
- Internationalization and localization work

## What tends to be declined

- Wholesale refactors or rewrites without prior discussion
- Changes that trade broad compatibility for narrow preference
- "Drive-by" code-style changes unrelated to the actual fix
- Dependencies added without a clear justification
- Features that expand scope beyond the project's stated goals

Not a rejection of your effort — just things worth agreeing on *before* writing the code.

## Getting help

- **Stuck on a PR?** Push what you have, mark the PR as Draft, and ask a question in the description.
- **Not sure how to approach something?** Open a discussion: `https://github.com/jodagreyhame/SoundBoard/discussions`
- **Found a gap in these docs?** Opening an issue or PR to fix it is itself a welcome contribution.

## Recognition

Contributors show up in the commit history, in release notes, and in GitHub's contributor graph automatically. Significant contributions over time may lead to a maintainer invite — see [GOVERNANCE.md](./GOVERNANCE.md) for the process.

Thank you for contributing.
