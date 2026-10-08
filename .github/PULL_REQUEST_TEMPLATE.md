## What this changes

<!-- One increment per pull request. Explain why, not just what. -->

## How it was verified

<!--
The command you ran and what it printed. Evidence, not assertion: a captured
checksum, a probe of a produced segment, a harness summary.

If this fixes a bug, say which test you watched fail before the fix.
-->

```
paste the command and its output here
```

## Checklist

- [ ] `go vet ./...`, `go test -race ./...` and `node --test web/*.test.js` pass
- [ ] Integration tests run if the change touches ffmpeg, streaming or subtitles
      (`go test -tags=integration -race ./...`)
- [ ] A regression test fails before the fix (if this is a bug fix)
- [ ] The artefact is asserted, not the command line (if colour, format or
      pixels are the point)
- [ ] `README.md`, `SPECIFICATION.md`, `TODO.md`, `CONTEXT.md` and
      `docs/handoff.md` are updated in this same pull request
- [ ] No secrets, keys or tokens are included

Closes #
