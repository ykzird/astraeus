# Development

Building, testing and verifying a change. [CONTRIBUTING.md](../CONTRIBUTING.md)
has the rules; this page has the commands and what they actually assert.

## Testing

```sh
go test ./...                            # unit tests
go test -race ./...                      # with the race detector
go test -tags=integration ./...          # also runs real ffmpeg/ffprobe
```

The integration tests generate their own clips, run ffprobe against them, and
drive a real HLS session end to end. They skip themselves when ffmpeg is absent.
The OCR path is one of them, and it skips when `tesseract` is absent rather than
failing a host that does not have it:

```sh
# Decodes a handwritten PGS fixture and asserts the words it yields, through
# real ffmpeg and a real tesseract.
go test -tags=integration -run OCR ./internal/subtitles/
```

```sh
node --test web/*.test.js                         # the front end's pure core
node --check web/app.js                  # the rest of the UI parses
```

The front end has no build step, so its timeline arithmetic lives in
[`web/core.js`](../web/core.js) — plain functions, no DOM — and is unit-tested with
Node's own runner. No npm install and no lockfile are involved. The DOM-heavy
remainder of `web/app.js` is covered by the browser harnesses below.

Playback and subtitle behaviour in a browser is covered separately by the CDP
harnesses in
[`scripts/ui-verify/`](https://github.com/ykzird/astraeus/tree/main/scripts/ui-verify),
which assert what the `<video>` element actually does rather than what the server
intended. They live in the repository and not in a release archive, which ships
the server and the pages you are reading but none of the development tooling.
Both the repackaged (`remux`) and re-encoded (`transcode`) HLS paths have been
observed playing in Chromium through those harnesses.

What the server *costs* rather than whether it works is measured by
[`scripts/load-verify/`](https://github.com/ykzird/astraeus/tree/main/scripts/load-verify),
which drives the JSON API and concurrent HLS streams and reads the server's own
`/metrics` before and after every phase — so the client's timings and the KPI
registry can be held against each other, and a metric that disagrees with the
client is visible as a disagreement rather than a number nobody checks. The same
directory holds `metrics-watch.mjs`, which records a live session as a timeline,
and `analyse.mjs`, which diffs two scrapes. Read its README before quoting a
number from it: the histogram buckets are coarse exactly where transcodes land,
and a 4K transcode saturates the development host, so those runs measure
contention as much as they measure the server.

## Layout

```
cmd/astraeus-server/    CLI entry point and wiring
internal/library/       domain model, repository port, scanner
internal/library/naming/  pure filename and path rules (no dependencies)
internal/library/sqlite/  the SQLite adapter for that port
internal/metadata/      provider interface, TMDB client, mock, enrichment worker
internal/streaming/     capability negotiation, probing, HLS session manager
internal/subtitles/     WebVTT extraction and caching, PGS and VobSub decoders, OCR
internal/testfixtures/pgs/  a PGS (.sup) writer for image-subtitle fixtures
internal/images/        artwork proxy and cache
internal/observability/ KPI registry and Prometheus exposition
internal/access/        the access gate
internal/api/           HTTP layer
web/                    the Spatial Web UI, including vendored hls.js
                        and the generated BoxIcons registry (web/icons.js);
                        web/core.js is the unit-tested pure timeline maths
scripts/ui-verify/      browser harnesses for playback and subtitles
scripts/load-verify/    load, latency and CPU measurement harnesses
scripts/pgsgen/         writes a PGS (.sup) fixture for those harnesses
scripts/make-vobsub-fixture.sh  re-encodes a PGS fixture into a VobSub one
scripts/make-dvb-fixture.sh  re-encodes a PGS fixture into a DVB subtitle one
scripts/make-demo-media.sh  generates a throwaway demo library
deploy/                 the systemd unit and the deployment runbook
Dockerfile              the container image (multi-stage, ffmpeg included)
.github/workflows/      CI: format, vet, test, integration test, image
```

`library` is the domain: the entities, the `Repository` port and the scanner.
It contains no SQL and imports no database driver, which is what makes "SQLite
today, PostgreSQL later" a real statement rather than an aspiration — the
storage engine is a detail of `internal/library/sqlite`, which implements the
port, owns the schema and its migrations, and is the only package that knows the
tables exist.

The rules that turn a file name into a title, season and episode are in
`internal/library/naming`, which imports nothing at all. They are the part of
the domain most worth isolating: they are pure, they are fiddly, and the scanner
and the schema migration both depend on them agreeing with each other — an
entity renamed by a migration has to end up named the way a scan would name it.

The composition root in `cmd` is the one place that names the concrete adapter,
which is also where the port and the adapter are checked against each other at
compile time.

`metadata` is a service over that domain rather than part of it: it depends on
`library`, and nothing in `library` depends on it, so a library can be scanned
and served with no metadata provider at all. The enrichment worker asks for only
two methods — list the entities, write one back — through its own `Store` port,
so its tests run against an in-memory store instead of a database and the
service cannot quietly grow a dependency on the whole repository.
