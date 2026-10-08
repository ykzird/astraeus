# Contributing

Thanks for looking. This is a small project with a strong idea of how changes
are made, and the rules below are the ones that have actually caught bugs.

## Before you start

- **Requirements:** Go (see `go.mod`), `ffmpeg` and `ffprobe`. `tesseract` is
  optional — it turns PGS image subtitles into text, and the tests that need it
  skip when it is absent.
- **No front-end build step.** `web/` is served as written: plain scripts, no
  bundler, no `npm install`.
- **Three Go modules total.** Adding a fourth needs a reason that survives
  review; the Prometheus exposition and the OTLP exporter are hand-rolled to
  keep the list short.

## Running the checks

```sh
go build ./...
go vet ./...
go test -race -count=1 ./...              # unit
go test -tags=integration -race ./...     # also runs real ffmpeg/ffprobe
node --test web/*.test.js                          # the front end's pure core
node --check web/app.js
```

The integration tests generate their own media, run real `ffmpeg`, and assert
what comes out. They skip themselves when a dependency is missing rather than
failing a machine that does not have it.

## The rules that matter

- **Test the artefact, not the command line.** When colour, format, pixels or
  text is the point, assert the produced segment or the served file. An
  assertion about the arguments passed to `ffmpeg` would have passed for a
  mechanism that does not work: `-color_primaries` is accepted and ignored.
- **A regression test must fail before the fix.** Run it against the unfixed
  build and see it fail; if it passes, it is not testing the bug.
- **A failing test after a deliberate behaviour change usually asserts the old
  bug.** Check which of the two is wrong before "fixing" the code.
- **Docs are part of the change.** `README.md`, `SPECIFICATION.md`, `TODO.md`,
  `CONTEXT.md` and `docs/handoff.md` are kept in sync in the same commit, and
  `TODO.md` records gaps honestly rather than aspirationally.
- **Never claim a capability you have not verified.** If something cannot be
  done, say so in a reason or a doc rather than quietly disabling it.
- **The front end never injects HTML.** There is no `innerHTML` in `web/`; the
  content security policy is strict because the UI earned it.

## Pull requests

- One increment per pull request, and a message that explains *why*, not just
  what — the existing history is the model.
- Say how you verified it: the command you ran and what it printed. "It works"
  is not evidence; a captured `sha256sum -c`, a probe of a produced segment, or
  a passing harness is.
- `main` requires a pull request and a green CI run. Merge is squash or rebase;
  there are no merge commits.

## Secrets

Do not commit keys, tokens or credentials; secrets like `TMDB_API_KEY` and
`ASTRAEUS_AUTH_TOKEN` belong in the environment. CI runs
[trufflehog](https://github.com/trufflesecurity/trufflehog) over every push, and
GitHub's secret scanning and push protection are enabled, so an accidental key is
blocked rather than merely regretted. To check before you push:

```sh
docker run --rm -v "$PWD":/repo trufflesecurity/trufflehog:latest \
  git file:///repo --results=verified,unknown --no-update
```

## Reporting a security problem

Please do not open a public issue — see [SECURITY.md](SECURITY.md).
