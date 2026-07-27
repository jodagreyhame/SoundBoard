<!--
Thanks for contributing to SoundBoard! Please fill out the sections below. Remove
sections that don't apply.
-->

## Summary

<!-- One or two sentences describing what this PR does and why. -->

## Type of change

- [ ] Bug fix (non-breaking change that fixes an issue)
- [ ] New feature (non-breaking change that adds functionality)
- [ ] Breaking change (fix or feature that would cause existing functionality to change)
- [ ] Documentation update
- [ ] Refactor / internal change (no user-visible effect)
- [ ] Other: ___

## Related issue

<!-- Link the issue this PR resolves, e.g. "Closes #123" or "Refs #456". -->

## How to test

<!-- Concrete steps a reviewer can follow to verify the change. Prefer reproducible
     commands over prose. -->

1. ...
2. ...

## Checklist

- [ ] Tests added or updated (or: "no new behavior, existing tests cover this")
- [ ] All tests pass locally (`CGO_ENABLED=1 go test -count=1 ./...`)
- [ ] Linter/formatter pass (`go vet ./...`)
- [ ] Docs updated if user-visible behavior changed
- [ ] CHANGELOG entry added (or: "not user-visible")
- [ ] PR title follows the conventions in [CONTRIBUTING.md](../CONTRIBUTING.md)

## Screenshots / recordings (UI changes only)

<!-- Before/after screenshots or a short screen recording. -->

## Notes for reviewers

<!-- Anything specific you want eyes on? Trade-offs considered, alternatives
     rejected, open questions? -->

## SoundBoard-specific checks

- [ ] **I have not added any audio files.** The `sounds/` library is user-supplied; audio PRs are declined regardless of content.
- [ ] Built on Windows with `CGO_ENABLED=1` and MinGW-w64 gcc on `PATH`.
- [ ] `CGO_ENABLED=1 go test -count=1 ./...` passes.
- [ ] If this touches the real-time audio path: `go test -race ./internal/audio/...` passes, and I have described what I heard on a real call.

