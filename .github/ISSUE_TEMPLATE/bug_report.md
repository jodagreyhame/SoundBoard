---
name: Bug report
about: Report a bug with SoundBoard
title: '[Bug] '
labels: bug
assignees: ''
---

## What happened

<!-- Clear description of the bug, what you expected, and what actually happened. -->

## Reproduction

<!-- Minimal steps to reproduce. The shorter and more self-contained, the faster we can fix. -->

1. ...
2. ...
3. ...

## Expected behavior

<!-- What did you expect to happen? -->

## Actual behavior

<!-- What actually happened? Include error messages, logs, or screenshots. -->

```
<paste error output or logs here>
```

## Environment

- **SoundBoard version:** (output of `SoundBoard --version` or commit hash)
- **OS / platform:** (e.g. macOS 14.3, Ubuntu 22.04, Windows 11)
- **Runtime version:** (e.g. Go 1.25, Node 20.10, Python 3.12 — whichever applies)
- **Installation method:** (e.g. Homebrew, `go install`, pre-built binary, from source)

## Additional context

<!-- Any other context about the problem. Related issues, screenshots, config files, etc. -->

## Checklist

- [ ] I searched existing issues for duplicates
- [ ] I can reproduce this on the latest release
- [ ] I have included all information needed to reproduce

## Environment (SoundBoard)

Please fill these in — most reports are unreproducible without them.

- **Windows version:**
- **SoundBoard build:** (release tag, or `go build` commit)
- **VB-CABLE installed?** (version if known)
- **Discord Noise Suppression setting:** `None` / `Standard` / `Krisp`
  <!-- If this is not `None`, set it to `None` and retest first. Krisp strips
       non-voice audio and will mangle or silence soundboard clips. -->
- **Microphone device:**
- **Did the audio processing module load?** (check `%AppData%\soundboard\soundboard.log` for the APM line)

## Log

Attach or paste the relevant part of `%AppData%\soundboard\soundboard.log`.

