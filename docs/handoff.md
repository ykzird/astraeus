# Handoff

**As of the round-19 work of 2026-10-09 — a DVB fixture, a manual end-to-end
walk-through, and the last subtitle blocker gone. 158 tracked files; `v0.17.0` and `v0.18.0` are released, so the
in-tree version is `dev` and the next tag would be `v0.18.1`.** (`git log` names
the commits. Round 17 disproved the note that had been blocking the VobSub
reader for two rounds: ffmpeg cannot *mux* VobSub, but it can *encode* it, so
the project's PGS fixture re-encodes into a real sample that ffmpeg decodes and
tesseract reads. Round 18 wrote that decoder and the routing, so a
`dvd_subtitle` track is offered as text instead of burn-only. Round 19 did the
same trick one format further: ffmpeg's `dvbsub` encoder takes no text, but its
PGS decoder can feed it, so a `dvb_subtitle` sample now exists too (§6) — the
decoder for it is the next increment (§7) — a first attempt was written and
withdrawn, and §8 says why. The same round added a manual end-to-end
walk-through of the whole application, built to be run by hand and converted to
Playwright, with a check in CI that keeps it in step with its data twin (§3). Round 16 added
`--access-policy`, which turns the gate from "may this request in" into "what may
it see" (§6). Round 15 added `deploy/tls/` and ran it: Caddy terminating
TLS, a client certificate as the viewer's identity, and the trust boundary the
access gate depends on (§6). Round 14 added `scripts/load-verify/` and measured
the API and the 1080p and 4K transcode paths
on this host (§6), which turned up one real defect — transcode percentiles were
interpolations rather than measurements, now fixed with per-metric bucket
bounds. Round 12 ran the release workflow
for real and started the systemd unit on a clean VM, finding two defects in the
packaging and one in the release job, all since fixed (§6). Round 13 removed the
point-in-time reviews from the
tree, narrowed the release archive to the pages a user actually needs, and made
every relative link inside that archive resolve — `docs/development.md` and
`web/vendor/icons.md` now point at `scripts/` on GitHub, because the archive
ships the server and the pages but none of the development tooling. Round 11 was
the documentation restructure, which
added §10 — a QEMU VM for the systemd unit and the release run. Round 10 was
release automation and going public, which found a ladder defect the development
host's ffmpeg had been hiding; round 9 made a quality choice cap a ladder; round
8 was OCR for PGS image subtitles; round 7 front-end unit tests; round 6 trace
export; round 5 API rate limiting; round 4 image subtitles by burn-in; round 3
per-viewer progress; earlier rounds are in `git log`.)

Written for whoever picks this up next — a person or an agent. The durable parts
(architecture, conventions, environment, how to verify) should stay true for a
long time; the state and open-work sections are the ones to refresh.

If you are an agent picking this up, read §5 first: there is a session-budget
rule there that decides when to stop and hand over rather than run long.

---

## 1. What this is

A from-scratch, single-binary media server in Go with an embedded vanilla-JS
"spatial" web UI: it scans a library, negotiates how to deliver each file to the
client that asked for it, and transcodes with ffmpeg when it has to. SQLite, no
cgo, no build step for the front end. `README.md` is the user-facing way in and
[`docs/index.md`](index.md) is the map of everything else: `docs/playback.md` and
`docs/configuration.md` for how it behaves, `docs/api.md` for the endpoints,
`docs/development.md` for the tests and harnesses, `SPECIFICATION.md` for the
design, and `TODO.md` for the honest state of what is missing.

An adversarial review of the codebase, the competitive landscape and the front
end — `docs/adversarial-review.md` and the two reports in `docs/review/` — is no
longer in the tree. They were point-in-time records that named types the code no
longer has, so they belong in history rather than in the map a newcomer reads;
`git log -- docs/adversarial-review.md` still finds them.

---

## 2. Environment: read this before running anything

The development host is an **AMD Ryzen 7 9700X, no Intel or NVIDIA GPU, and
`/dev/dri` is not visible from the sandbox** — so no hardware encoding path can
be exercised here. Real media for testing lives at
`$HOME/jellyfin/media/movies/` (a 17 GB 4K HEVC HDR film and a second film).
`real.db` points at it. **Never modify anything under `$HOME/jellyfin`**, and
remember `*.db` is gitignored, so the library database is local state.

Traps that have cost time:

| Trap | What to do |
| --- | --- |
| `/tmp` is a per-invocation tmpfs | `export GOCACHE=$HOME/Work/deepseek-harness/.gocache TMPDIR=$HOME/Work/deepseek-harness/.tmp` before any Go command |
| Go needs `mise exec --` | `mise exec -- go test ./...` |
| `pgrep -f` / `pkill -f` match the wrapper's **own** command line | never `pkill -f` a pattern that appears in the command you are running; it kills your own shell (this happened twice) |
| `go build ./...` does **not** refresh `./astraeus-server` | `mise exec -- go build -o ./astraeus-server ./cmd/astraeus-server` |
| Port 8080 belongs to another service | use 86xx–92xx for tests; the server defaults to 8642 |
| CLI flags are per-command, and the top-level `--help` lists only the commands | run `./astraeus-server <cmd> -h`; `--db` exists on `scan`, `library add` and `enrich`, but not at the top level |
| `scan` has no `--all` flag | use `POST /api/scan` for every library, or `scan --library <id>` for one |
| Stream and cache dirs default to temp | pass `--stream-root` / `--subtitle-cache` / `--image-cache` inside the workspace when testing |
| `web/vendor/hls.min.js` is 620 KB on one line | exclude `web/vendor` (or `*.min.js`) from any search across `web/`, or one grep flushes the result |
| The session transcript echoes each provider response in a `stream` field | a char count over it says ~3x the truth. Take the prompt size from the `usage` block of the last `assistant/message`: `inputTokens + cacheReadTokens` |
| The Go module cache (`$(go env GOMODCACHE)`) is **read-only** under the sandbox | builds and tests work, but adding or bumping a module fails with `read-only file system`. The network is reachable, so the fix is to point `GOMODCACHE` (and `GOPATH` if needed) inside the workspace; the alternative is to avoid the new dependency, which is why the Prometheus exposition and the OTLP exporter are hand-rolled |
| `docker` wants its state inside the workspace | export `DOCKER_CONFIG=$PWD/../.tmp/docker-config BUILDX_CONFIG=$PWD/../.tmp/buildx`. The registry is reachable (`docker pull jaegertracing/all-in-one` was used to verify trace export) |

---

## 3. How to verify

```sh
# Everything, including the tests that shell out to real ffmpeg.
mise exec -- go test -tags=integration -race -count=1 ./...

# The front end's pure timeline core. Node's own runner; no npm install.
node --test web/*.test.js

# A demo library (a film plus a three-episode show, one with a real subtitle track).
# It needs ./astraeus-server built first. Defaults: media outside the repo, db at ./demo.db.
mise exec -- go build -o ./astraeus-server ./cmd/astraeus-server
./scripts/make-demo-media.sh
# or: ./scripts/make-demo-media.sh <media-dir> <db-path>

# The manual end-to-end walk-through, and the check that keeps it in step with
# its machine-readable twin. The walk-through is one ordered pass over the whole
# application, written to be run by hand and converted to Playwright afterwards;
# every step says whether a machine can assert it or only a person can. The
# check is cheap and CI runs it, so the two descriptions cannot drift.
node --test scripts/ui-verify/check-e2e-flows.test.mjs
node scripts/ui-verify/check-e2e-flows.mjs

# Browser harnesses: start a server, then a headless Chromium with a CDP port.
./astraeus-server serve --db demo.db --web-dir web --addr 127.0.0.1:8910 \
  --enrich-interval 0 --scan-interval 0 --stream-root $PWD/.streams &
chromium --headless=new --no-sandbox --disable-gpu --mute-audio \
  --remote-debugging-port=9410 --remote-allow-origins='*' \
  --user-data-dir=$(mktemp -d) about:blank &
cd scripts/ui-verify
CDP_PORT=9410 node player-chrome-verify.mjs http://127.0.0.1:8910 <entityId> 180
CDP_PORT=9410 node player-verify.mjs        http://127.0.0.1:8910 <episodeId> <movieId>
CDP_PORT=9410 node subtitle-verify.mjs      http://127.0.0.1:8910 <captionedEpisodeId> <movieId>
CDP_PORT=9410 node real-media-verify.mjs    http://127.0.0.1:8910 <realFilmId> 180
```

`player-chrome-verify.mjs` is the broadest single check: up to 32 assertions
covering the overlay, icons, fullscreen, audio, quality switching, subtitle
persistence and auto-hide. **Its score depends on the content**, so compare a run
against the same fixture: it was **28/28** against the 17 GB film in the round-2
verification, and it is **20/22** on the bundled three-second demo clips, where
the two auto-hide checks fail because playback ends during the wait. 20/22 is also
what the pre-change build scores there, so it is that fixture's baseline rather
than a regression (§8).

Dynamic range has its own checks, and they are worth re-running after any change
to negotiation or to the ffmpeg argument builder:

```sh
# Synthesises a PQ/BT.2020 fixture and runs both delivery paths through real
# ffmpeg, asserting the *produced segment*: 8-bit bt709 after a tone map, 10-bit
# smpte2084/bt2020 when HDR is kept.
mise exec -- go test -tags=integration -run 'ToneMapsHDR|KeepsHDR' -v ./internal/streaming/

# Encodes the same expensive source twice - unlimited and under a 500 kbps
# ceiling - and compares the measured bitrates, because a flag in an argument
# list is not evidence that the rate was bounded.
mise exec -- go test -tags=integration -run BitrateCeiling -v ./internal/streaming/

# Builds a ladder and asserts the master playlist names every rung and that each
# rung's produced segment really is the height it was advertised as. The second
# test is the preferred-height case: a `preferred_height` request must produce a
# ladder topped there, measured from the segments, not from the argument list.
mise exec -- go test -tags=integration -run TestManager_Ladder -v ./internal/streaming/

# The same ladder test against the ffmpeg the container ships, which is not the
# one on this desk: ffmpeg 9 accepts a ladder that shares one copied audio stream
# between variants, 5.1 does not, and the difference cost rungs from the master
# playlist. Run this after any change to the ladder's ffmpeg arguments.
docker run --rm -v "$PWD":/src -v "$(go env GOMODCACHE)":/gomod -w /src \
  -e GOMODCACHE=/gomod -e GOCACHE=/tmp/gocache golang:1.26-bookworm bash -c \
  'apt-get update -qq && apt-get install -y -qq ffmpeg >/dev/null 2>&1 && \
   go test -tags=integration -count=1 -run TestManager_Ladder -v ./internal/streaming/'

# The negotiation contract for a capped quality choice, and the trap it would
# otherwise fall into: a preference has to limit the output, not merely advise
# the ladder, or a 4K source with a 720 preference is direct-played whole.
mise exec -- go test -run 'PreferredHeight|BurnKeepsOneRendition|BuildsALadderOnly' ./internal/streaming/
mise exec -- go test -run 'PreferredHeight|SubtitleEndpoint|AdvertisesImageTrack' ./internal/api/

# Serves a file with two audio tracks and asserts the *delivered segment* carries
# the chosen one, which is the check a command-line assertion would miss.
mise exec -- go test -tags=integration -run TestManager_DeliversTheChosenAudioTrack -v ./internal/streaming/

# Resume state end to end, over HTTP against a copy of a real database: report a
# position, read it back, overrun it (400), finish it (cleared), force it.
mise exec -- go test -run 'TestPlaybackProgress|TestListProgress' ./internal/api/ ./internal/library/sqlite/

# Per-viewer progress: two identities through the real access gate keep separate
# positions, separate continue-watching listings, and finishing or clearing one
# leaves the other alone. The sqlite half also covers the rebuild that widens a
# pre-viewer table and keeps its rows under the local viewer.
mise exec -- go test -v -run 'PerViewer|LocalOne|WidensProgress|UngatedServer|FinishingIsOnly' \
  ./internal/api/ ./internal/library/sqlite/

# Image subtitles. The integration test writes a PGS fixture (nothing else can -
# ffmpeg will not encode a bitmap subtitle from text), burns it in with the real
# ffmpeg, and compares the produced segment's pixels against the same source
# without the burn. The unit tests pin the negotiation and the command line.
mise exec -- go test -tags=integration -run BurnIn -v ./internal/streaming/
mise exec -- go test -run 'BurnsAnImageSubtitle|DoesNotBurnATextSubtitle|BurnIndexThatDoesNotExist' ./internal/streaming/ ./internal/api/

# OCR for PGS *and* VobSub. Both parsers are unit-tested against committed
# fixtures; the VobSub fixture is decoded from a Matroska container, so that test
# needs no ffmpeg at all. The DVB fixture (scripts/make-dvb-fixture.sh) is
# committed and its framing is unit-tested, but no decoder reads it yet, so its
# tests are named separately rather than pretending to be OCR. The OCR path is integration-tagged and asserts the
# *words*, through real ffmpeg and a real tesseract - it skips when tesseract is
# absent, and separate always-run unit tests pin the refusal that an install
# without it keeps and the routing (image track -> ConvertImage, DVB -> 415).
# The VobSub fixture is regenerated with scripts/make-vobsub-fixture.sh; its
# decoder was checked against ffmpeg's own decode, pixel for pixel.
mise exec -- go test -count=1 -run 'ParsePGS|ParseVobSub|DVBFixture|OCR|ConvertImage' ./internal/subtitles/
mise exec -- go test -tags=integration -run OCR -v ./internal/subtitles/
mise exec -- go test -run 'SubtitleEndpoint|AdvertisesImageTrack' ./internal/api/
# And the whole path against a running server: an image track is advertised with
# a URL and fetching it returns the recognised words.
curl -s -X POST localhost:8921/api/entities/<id>/playback -H 'Content-Type: application/json' \
  -d '{"subtitles":true,"containers":["matroska"],"video_codecs":["h264"],"audio_codecs":["aac"],"max_height":1080,"max_bit_depth":8}' \
  | python3 -c 'import json,sys; print(json.load(sys.stdin)["subtitles"])'

# Rate limiting: the bucket's behaviour is unit-tested with an injected clock,
# and the wiring is checked against the running binary - a burst is allowed, the
# next call is a 429 with Retry-After, /api/health and the UI are untouched, and
# the refusal is counted.
mise exec -- go test -race -run 'Limiter|RateLimit' ./internal/ratelimit/ ./internal/api/
./astraeus-server serve --db .tmp/demo-verify.db --web-dir web --addr 127.0.0.1:8927 \
  --enrich-interval 0 --scan-interval 0 --stream-root "$PWD/.tmp/rl-streams" \
  --rate-limit 1 --rate-limit-burst 2 &
for i in 1 2 3; do curl -s -o /dev/null -w '%{http_code}\n' localhost:8927/api/entities; done  # 200 200 429
curl -s localhost:8927/metrics | grep '^astraeus_rate_limited_total'                # 1

# Tracing. The encoder and batcher are unit-tested against a stub OTLP receiver
# (the payload is decoded with the OTLP field names, so a wrong shape fails the
# test), and the whole path was checked against a real collector: run Jaeger,
# point the server at it, and query the collector's own API.
mise exec -- go test -race -run 'Trace|Tracer|Exporter|Middleware|Queue' ./internal/tracing/
docker run -d --name astraeus-jaeger -p 127.0.0.1:4318:4318 -p 127.0.0.1:16686:16686 jaegertracing/all-in-one:latest
./astraeus-server serve --db .tmp/demo-verify.db --web-dir web --addr 127.0.0.1:8929 \
  --enrich-interval 0 --scan-interval 0 --stream-root "$PWD/.tmp/otel-streams" \
  --otel-endpoint http://127.0.0.1:4318 &
curl -s -o /dev/null -H 'traceparent: 00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01' \
  localhost:8929/api/libraries
curl -s 'http://127.0.0.1:16686/api/traces?service=astraeus-media&limit=10'   # trace 4bf9... continued
docker rm -f astraeus-jaeger

# Both resume paths in a real browser - direct play seeks the element itself,
# segmented delivery is started at the offset by the server - plus the
# continue-watching list gaining and dropping the entry. Use a fixture longer
# than the resume position plus a minute; the harness README has a recipe for a
# three-minute clip whose mp4 and mkv remux exercise the two paths. The 4K films
# do not: their HDR ladder starts slower than the harness waits.
CDP_PORT=9413 timeout 200 node scripts/ui-verify/resume-verify.mjs http://127.0.0.1:8923 <directPlayEntityId> 30
CDP_PORT=9413 timeout 200 node scripts/ui-verify/resume-verify.mjs http://127.0.0.1:8923 <segmentedEntityId> 30

# Image subtitles in a real browser, burn path: the menu offers the image track,
# choosing it re-negotiates with burn_subtitle_index, and the decision comes back
# with burned_subtitle_index. Needs an entity with a PGS track; scripts/pgsgen and
# the harness README build one. This now needs a server with NO OCR engine,
# because a track the server can read is offered as text instead: start it with
# --tesseract-bin /nonexistent/tesseract. What is composited into the picture is
# the Go integration test's job, not this harness's.
CDP_PORT=9413 timeout 200 node scripts/ui-verify/burn-verify.mjs http://127.0.0.1:8924 <imageSubtitleEntityId>

# Image subtitles in a real browser, OCR path: the menu offers the PGS track as an
# ordinary track (no "(burned in)"), choosing it attaches a <track> with no
# re-negotiation at all, and the caption on screen is the text the fixture drew.
# Needs a server with tesseract and an entity whose PGS track carries letters
# (scripts/pgsgen -text; the harness README has the recipe).
CDP_PORT=9413 timeout 200 node scripts/ui-verify/ocr-verify.mjs http://127.0.0.1:8925 <imageSubtitleEntityId> "ASTRAEUS MEDIA"

# The real thing, against the 17 GB film: a browser profile gets bt709/bt709,
# an HDR manifest gets 10-bit bt2020/PQ. Copy real.db first; *.db is local state.
cp real.db /tmp/verify.db   # or anywhere outside the repo
./astraeus-server serve --db /tmp/verify.db --web-dir web --addr 127.0.0.1:8912 \
  --enrich-interval 0 --scan-interval 0 --stream-root /tmp/verify-streams &
curl -s localhost:8912/api/system/capabilities | python3 -m json.tool   # hdr_video_encoders
# then POST a capability manifest to /api/entities/{id}/playback and ffprobe a
# fetched segment for pix_fmt and colour tags (see §3's commands for the shape)
```

Two mechanical checks worth re-running after any change to the API surface or the
metrics registry — both are scripted in the review's spirit and catch documentation
drift that eyeballing does not:

- every metric declared in `internal/observability/kpi.go` is named in
  `docs/api.md`, and nothing named there is undeclared;
- every route mounted in `internal/api/server.go` appears in that page's table.

Deployment has its own checks. The unit is checked statically and the image is
checked by running it:

```sh
# Static checks: syntax and directives, then the hardening score.
systemd-analyze verify deploy/astraeus.service
systemd-analyze security --offline=yes deploy/astraeus.service   # 1.6 (OK)

# Build and start the image, then wait for the health check to go green.
docker build -t astraeus-media:dev .
docker run -d --name astraeus -p 127.0.0.1:8642:8642 \
  -v "$PWD/../demo-media:/media:ro" -v astraeus-data:/data astraeus-media:dev
curl -s localhost:8642/api/health
docker inspect --format '{{.State.Health.Status}}' astraeus      # healthy
docker exec astraeus astraeus-server scan --db /data/astraeus.db \
  --path /media/movies --kind movies --name Movies
```

Note that `docker build` needs its state inside the workspace on this host, or
the sandbox refuses it: set `DOCKER_CONFIG=$PWD/../.tmp/docker-config` and
`BUILDX_CONFIG=$PWD/../.tmp/buildx` before building.

Releases are checked by building one. The workflow calls the same script, and
the version it bakes in is the only difference between this and a plain build:

```sh
# Archives for the default platforms (linux/amd64, linux/arm64) in ./dist.
mise exec -- scripts/build-release.sh 0.18.0
(cd dist && sha256sum -c checksums.txt)
tar -xzf dist/astraeus-server_0.18.0_linux_amd64.tar.gz -C /tmp
/tmp/astraeus-server_0.18.0_linux_amd64/astraeus-server version   # 0.18.0

# The image carries the version and the OCI provenance labels; both are read
# back from the built artefact rather than from the Dockerfile.
docker build --build-arg VERSION=0.18.0 --build-arg REVISION="$(git rev-parse --short HEAD)" \
  -t astraeus-media:0.18.0 .
docker inspect astraeus-media:0.18.0 --format '{{json .Config.Labels}}' | python3 -m json.tool
docker run --rm astraeus-media:0.18.0 version

# Both workflows lint clean; actionlint runs from a container, so nothing is
# installed on the host.
docker run --rm --user "$(id -u):$(id -g)" -v "$PWD":/repo -w /repo rhysd/actionlint:latest
```

The repository is public, so secrets are scanned in two places: CI runs trufflehog
on every push and pull request, and GitHub's own secret scanning and push
protection are enabled. The same scan is worth running before pushing something
you suspect:

```sh
# Verified and unknown-result credentials, over the full history.
docker run --rm -v "$PWD":/repo trufflesecurity/trufflehog:latest \
  git file:///repo --results=verified,unknown --no-update
# A second opinion from a different rule set.
docker run --rm --user "$(id -u):$(id -g)" -v "$PWD":/repo zricethezav/gitleaks:latest \
  detect --source=/repo --redact -v
```

Two claims could not be checked on this host at all — the systemd unit, because
there is no reachable systemd manager, and the release workflow, because nothing
had been tagged. Both have now been checked on the QEMU VM in **§10**, which is a
recipe that has been run rather than a plan: it installs the *published* `v0.17.0`
release archive and then follows `deploy/README.md` verbatim.

A published release is checked against its artefacts, not its YAML:

```sh
gh release view v0.18.0                       # the assets, the notes, not a draft
gh release download v0.18.0 -D /tmp/rel
(cd /tmp/rel && sha256sum -c checksums.txt)   # both archives, against the checksums
docker pull ghcr.io/ykzird/astraeus:0.18.0
# Both platform manifests, and an attestation manifest per platform.
docker buildx imagetools inspect ghcr.io/ykzird/astraeus:0.18.0
# The version is baked in at build time, so this reads it back from the image.
docker run --rm --entrypoint astraeus-server ghcr.io/ykzird/astraeus:0.18.0 version
```

Performance is measured rather than argued about. The harness in
`scripts/load-verify/` drives the API and concurrent HLS streams and reads
`/metrics` before and after every phase, so the client's timings and the KPI
registry can be held against each other:

```sh
cp demo.db .tmp/load-verify/load.db          # a COPY; never point this at real.db
node scripts/load-verify/load-verify.mjs all --spawn \
  --db .tmp/load-verify/load.db --entity <entityId> --streams 1,2,4 --hold 20

# A live session someone drives by hand, recorded as a timeline, then read back.
node scripts/load-verify/metrics-watch.mjs --out live.jsonl --interval 2
node scripts/load-verify/metrics-watch.mjs --report live.jsonl

# Or diff two scrapes taken around a session that was driven by hand.
node scripts/load-verify/analyse.mjs before.prom after.prom
```

`--spawn` is what makes the CPU numbers possible: on Linux a process is visible
in `/proc` only inside the PID namespace it was started in, so a sampler in a
different shell than the server sees nothing at all. The same namespace split
means `pkill`/`pgrep` cannot reach a server another shell started.

---

## 4. Architecture

```
cmd/astraeus-server     flags, wiring, the composition root
internal/library        domain: entities, the Repository port, the scanner
internal/library/naming pure filename/path rules — imports nothing
internal/library/sqlite the SQLite adapter for that port, schema and migrations
internal/metadata       provider interface, TMDB, mock, enrichment worker
internal/streaming      capability negotiation, encoder selection, HLS sessions
internal/subtitles      WebVTT extraction and caching, a PGS decoder and OCR
internal/images         artwork proxy and cache
internal/access         the access gate
internal/api            HTTP layer
internal/observability  hand-rolled Prometheus text exposition
web/                    vanilla-JS UI, no build step, vendored hls.js
```

Dependencies point one way. `library` knows nothing of SQL; `metadata` depends on
`library` and nothing depends on `metadata`; `naming` depends on nothing. The
composition root in `cmd` is the only place that names a concrete adapter, and
the only place where a port and its adapter are checked against each other at
compile time.

Conventions that matter more than they look:

- **Never claim a capability you have not verified.** Hardware encoders are
  proved by encoding; metadata is only `Complete` when a provider succeeded;
  negotiation explains every choice with a reason. When something cannot be
  done, the reason is reported rather than the feature quietly disabled.
- **Comment the why, not the what.** Most of the load-bearing comments in this
  codebase explain a decision that would otherwise look arbitrary.
- **The front end never injects HTML.** There is no `innerHTML` anywhere in
  `web/`; DOM is built with `createElement`/`createElementNS` and literal
  attribute keys. Since 0.9.0 the content security policy (`script-src 'self'`,
  no `unsafe-inline`, no `unsafe-eval`) is the enforced boundary, but this
  discipline is what keeps the policy strict: an inline script or an
  HTML-injection sink would mean weakening the policy or handing a filename on
  disk a way into the DOM, not just a code smell.
- **Docs are part of the change.** `README` (the user's way in), the pages under
  `docs/`, `SPECIFICATION`, `TODO`, and the READMEs beside the code they describe
  (`web/README.md`, `deploy/README.md`) have all been kept in sync; `TODO.md`
  marks gaps honestly rather than aspirationally. `docs/index.md` is the map, and
  every page carries what it is for.

---

## 5. Working agreements that have worked well

- **UI work is delegated**, with the instruction to implement, run
  `node --check`, and report — *not* to run browsers or servers. Verification
  happens here, against the CDP harnesses. The one time an agent was left to
  iterate on verification it burned a lot of time for little.
- **Verify claims mechanically.** Test counts before and after a refactor (37
  before, 37 after the library split); metrics and routes against the docs.
- **A failing test after a deliberate behaviour change usually asserts the old
  bug.** This has happened five times now — `EncoderFor`'s "nothing available"
  case expected a software encoder that was never declared, the sweep fixture
  predated the marker file, `compiledButUnusable` put a rejected encoder in the
  verified list, the pixel-format test expected VAAPI to take `-pix_fmt`, and the
  encoder probe test assumed one probe per encoder where there are now two.
  Check which is wrong before "fixing" the code.
- **Regression tests must fail before the fix.** The auto-hide check was run
  against the unfixed build to prove it caught the reported bug; so was the HDR
  work, where reproducing the old command line showed an 8-bit stream still
  tagged `smpte2084`/`bt2020`.
- **Test the artefact, not the command line, when colour or format is the
  point.** Asserting the ffmpeg arguments would have passed for a mechanism that
  does not work: `-color_primaries` is accepted and ignored. The integration
  tests probe the produced segment instead.

### Session budget: stop before the harness compacts you

Long agent sessions get compacted, and a compaction loses the thread of a
multi-step increment. Rather than discover that halfway through a change, watch
the budget and hand over deliberately.

```sh
# Measures the real prompt size from the provider's own usage block, and prints
# the wrap line and the point at which the harness compacts.
mkdir -p .tmp && cat > .tmp/ctx-check.sh <<'EOF'
FILE=$(find "${DSH_HOME:-$HOME/.dsh}/sessions" -type d -name "${DSH_SESSION_ID:?}" | head -1)/session.v4.jsonl.zstd
zstd -dc "$FILE" | python3 -c '
import json,sys
WINDOW, used, at = 1_000_000, 0, (0,0)
for line in sys.stdin:
    e = json.loads(line) if line.strip() else {}
    if e.get("type") == "request/context":
        WINDOW = e["data"].get("contextWindow", WINDOW)
    if e.get("type") == "assistant/message" and e["data"].get("usage"):
        u = e["data"]["usage"]
        used, at = u.get("inputTokens",0)+u.get("cacheReadTokens",0), (e["data"].get("turn"), e["data"].get("step"))
print(f"{used:,} of {WINDOW:,} tokens ({100*used/WINDOW:.1f}% used), last measured at {at}")
print(f"wrap at {int(WINDOW*0.70):,} | compacts at {int(WINDOW*0.80):,}")'
EOF
bash .tmp/ctx-check.sh
```

The window is **1,000,000 tokens** for `deepseek-flash`. The harness's own
compaction triggers at **80%** — `DEFAULT_THRESHOLD_RATIO = .8` in
`dsh-compaction-basic`, with no override in the `web` profile — and a compaction
retains only about **16%** (`DEFAULT_RETAIN_RATIO`) and summarises the rest, so a
compaction is a lossy restart and not a pause. The thresholds below therefore do
*not* sit far below compaction: 80% **is** the compaction point. Treat 70% as the
working limit and 80% as the wall.

| Used | Do |
| --- | --- |
| **70%** (700k) | Wrap up: finish the increment in hand, no new work |
| **80%** (800k) | The harness compacts here. Do not be working on anything at this point: sync `README`, `SPECIFICATION`, `TODO` and this handoff with the state you reached, commit it, and hand over a short summary a fresh session can start from |

Budget an increment before starting it, from the session's own usage deltas.
Measured over rounds 3–7 on this project, with the provider's `usage` block as the
source of truth, an increment of this size costs roughly:

| Increment | Cost |
| --- | --- |
| A small, self-contained change plus docs (UI unit tests in `web/core.js`) | ~3 points |
| A new middleware/package with tests (rate limiting, trace export) | ~7 points |
| A domain change touching storage, API and tests (per-viewer progress) | ~13 points |
| A feature with new fixture infrastructure and pipeline work (image subtitles) | ~19 points |

So with 18 points of headroom under 70%, the 19-point class of increment does not
fit: it lands on the wrap line with no margin for a fixture or filter that needs a
second attempt. When the headroom is smaller than the work, hand over instead —
that is the whole point of this rule.

The handover must say what is done and verified, what is half-done, the exact
command that reproduces each claim, and the next priority from §7 — §7 is
written to be taken top-down for exactly this reason. A one-line reference to
this document plus the state is enough to start the next session; it should not
need this session's transcript.

---

## 6. State: done and verified

Scanning (idempotent, periodic, with pruning guarded against a blind scan);
Series/Season/Episode from the directory layout; TMDB metadata with an honest
`Incomplete` lifecycle; artwork proxying and caching; capability negotiation
across five axes with reasons; HLS via ffmpeg with software and five hardware
encoder families; seeking anywhere in a film by re-negotiating at an offset;
direct play with range requests; WebVTT subtitles; the access gate; Prometheus
KPIs; and a full player (overlay transport, fullscreen, subtitles, volume, quality
selection, auto-hide).

Dynamic range is a sixth axis as of `f489fa4`. HDR is classified from the
source's transfer function (PQ and HLG, never from bit depth or primaries),
Dolby Vision is reported with its profile and base-layer compatibility, a client
that cannot show HDR gets a tone-mapped SDR stream tagged BT.709, and one that
declares `supports_hdr` keeps 10-bit HDR through a copy, remux or re-encode. The
re-encode path is gated on a second startup probe per encoder, because an encoder
that works at 8 bits may refuse 10. Verified against the 17 GB film: 1920x820
H.264 tagged `bt709`/`bt709`/`bt709` for the browser profile, 2528x1080 10-bit
HEVC tagged `bt2020nc`/`smpte2084`/`bt2020` for a manifest declaring HDR.

Verified in a real browser against real 4K content in the round-2 work: 28/28
chrome, 13/13 player, 10/10 subtitles. Full Go suite green with race and
integration. Those numbers belong to that content: the bundled demo clips score
lower on the auto-hide checks for fixture reasons (§8).

**The first real CI run found a ladder defect the dev host could not see** (round
10). The integration test failed on the runner's ffmpeg 6.1 with a master
playlist naming two rungs of three — and it reproduced exactly against the
container's own ffmpeg 5.1.9, so the packaged server had been serving an
incomplete ladder. The cause was in our arguments, not ffmpeg's: a ladder mapped
the audio once per rung with `-c:a copy`, so all three variants carried the
*same* elementary stream. ffmpeg 9 accepts that; 5.1 and 6.1 answer "Same
elementary stream found more than once in two different variant definitions" and
quietly leave variants out of the master playlist. A ladder now re-encodes the
audio per rung, and the decision says why. The rule is pinned by a unit test
(a ladder transcodes the audio; a pinned single rendition still copies) and by
the integration test run inside a bookworm container, which is now part of §3.
This is the clearest argument for the CI and the container being real: the host's
ffmpeg is not the shipped one, and only running the test on the shipped version
told the truth.

**The documentation was restructured** in round 11, which changes how the project
explains itself rather than what it does. `README.md` had grown to 900 lines and
had become the reference manual: accurate, but not a front door. It is now the
front door — what this is, what it does, how to run it, where to read more — and
the detail moved, unchanged, into `docs/`: [playback.md](playback.md) (delivery,
negotiation, HDR, hardware encoders, audio, subtitles, the ladder),
[configuration.md](configuration.md) (scanning, every flag, the gate, rate
limiting, tracing), [api.md](api.md) (endpoints, resume state, metrics) and
[development.md](development.md) (tests, harnesses, layout), with
[index.md](index.md) as the map. Nothing was rewritten in the move: a script
sliced the old README by heading and normalised the heading levels, so the prose
and its claims are the ones that were already verified, and a link check confirms
every relative link still resolves. Two choices were deliberate. **The wiki was
rejected**: it is a separate git repository, so it cannot be reviewed in the pull
request that changes the code it describes, and it is not in the checkout an agent
or a contributor reads. **The code of conduct was removed**: nobody asked for one,
and a document nobody reads is worse than no document, because it implies a
process that does not exist. `CONTRIBUTING.md` and the pull-request template now
name `docs/` in the "docs are part of the change" rule, and the handoff's two
mechanical drift checks point at `docs/api.md` for the metric and route tables
they compare against.

**The repository went public** in round 10, which changes the project's surface
rather than its behaviour. The module path was `github.com/jok/astraeus-media`
while the remote is `github.com/ykzird/astraeus`, so `go install` and pkg.go.dev
would both have failed on a mismatch the compiler never sees — nothing in the
tree resolves the module by its public path. Every import, the `go.mod` module
line, the Dockerfile's `SOURCE` default and the README's registry example now say
`github.com/ykzird/astraeus`. Personal absolute paths came out of the handoff, the
licence holder matches the public identity, and `.gitignore` gained the files an
accident reaches for: `.env*`, editor noise, and what `gh auth login` writes when
it runs inside the tree.

Before the first push, all 35 commits were scanned: gitleaks found nothing across
2.26 MB, trufflehog found no verified or unknown credentials across 1183 chunks,
no credential-shaped file, database or media file had ever been committed, and the
largest blob in history was 606 KB (`web/vendor/hls.min.js`). That scan is now
continuous rather than a moment — CI runs trufflehog on every push and pull
request (pinned to a commit, because a tag can move under a job that reads the
whole repository), GitHub's secret scanning and push protection are on, and
Dependabot watches the Go modules, the workflow actions including those commit
pins, and the two container base images. The files a public repository is expected
to have came with it — CONTRIBUTING (the verification rules this project actually
follows), SECURITY, a pull-request template that asks for evidence, issue
templates and CODEOWNERS — and `main` is protected: a pull request, green CI,
linear history. There is deliberately no code of conduct: nobody asked for one and
it would be a document nobody reads.

**Release automation** landed as of round 10, and `v0.17.0` is the first release
it produced (round 12; §6 has what the run found). Pushing a `v*` tag runs
`.github/workflows/release.yml`, which gates on the unit tests, builds one archive
per platform with `scripts/build-release.sh`, publishes a GitHub Release with a
checksums file and generated notes, and pushes a multi-arch image — `linux/amd64`
and `linux/arm64` in one manifest — to GHCR with build provenance and an SBOM
attested alongside it. The workflow is thin on purpose: all of the work is in the
script, which runs by hand, and which is how the artifacts were verified before
the first tag existed.

Two decisions shaped it. **The version had to stop being a constant.** It is now
a variable set from the tag with `-ldflags "-X main.version=..."`, so `go build`
reports `dev` and a release reports its tag; a binary that cannot name the
revision it came from should say so rather than print a number it cannot back up.
**The build is hand-rolled rather than GoReleaser.** The project already avoids a
dependency it can replace with a page it controls, and here the whole release
fits in one script whose every step can be run and checked on the development
host — which is the property that made it verifiable at all without a remote.

Verified by running it, not by reading it. `scripts/build-release.sh 0.17.0`
produced `astraeus-server_0.17.0_linux_{amd64,arm64}.tar.gz` (binary, the `web`
directory it serves, `LICENSE`, the notices) and a checksums file that
`sha256sum -c` accepts; the amd64 binary printed `astraeus-server 0.17.0` and the
arm64 one is an AArch64 ELF. The image was built with the version and the OCI
labels, `docker inspect` read all eight labels back with the right revision, and
the container served `/api/health`. The multi-arch build was run for real
(emulated arm64 only for the runtime layer's `apt-get`, because the builder stage
runs on `$BUILDPLATFORM` and cross-compiles) and its OCI index carries both
platforms. `actionlint` accepts both workflows.

**The release workflow has now run, and the first tagged release found two
defects — which is exactly what running it was for.** Round 12.

The dry run (`gh workflow run release.yml -f dry_run=true`) was green on all four
jobs and published nothing: `Publish the GitHub Release` came back `skipped`, the
version resolved to `0.0.0-dryrun` rather than to a release number, and both
uploaded archives passed `sha256sum -c` against the uploaded checksums file.

The first real tag then failed, in the one job the dry run cannot exercise. The
publish job had no `actions/checkout`, so `gh` had no git remote to infer the
repository from:

```
failed to run git: fatal: not a git repository (or any of the parent directories): .git
```

Everything else in that run succeeded — the gate, the archives, and the
multi-arch image (`linux/amd64`, `linux/arm64` and one attestation manifest per
platform) reached GHCR. The fix is a checkout in that job; PR #9.

A second defect was found before the tag was spent, and it was older. The archive
held only the binary, `web/`, `LICENSE` and `THIRD_PARTY_NOTICES.md`, while
`deploy/README.md`'s systemd runbook copies `deploy/` and the project documents
into `/usr/local/share/doc/astraeus` and installs `deploy/astraeus.service` from
there. Eight of the ten paths that runbook names did not exist in a release, so
anyone with nothing but the archive could not install the unit at all — observed
on the clean VM as `cp: cannot stat 'README.md'` through `cp: cannot stat
'deploy'`. `scripts/build-release.sh` now stages them and fails the build if a
path the runbook names is missing, and CI asserts the same layout on every pull
request; PR #8.

**The archive was then narrowed and the reviews removed from the tree** (round 12,
after PR #8). Shipping `docs/` whole was too blunt: it put `handoff.md` and two
internal audits into an artifact that gets extracted onto a user's host. The
archive now carries only the pages a reader of the installed README needs —
`docs/index.md`, `docs/playback.md`, `docs/configuration.md`, `docs/api.md` and
`docs/development.md` — and `build-release.sh` fails the build if one of the
working documents is ever staged again. `docs/adversarial-review.md` and
`docs/review/` were point-in-time records that named types the code no longer
has, so they left the tree altogether and now live only in history; §1 and
`docs/index.md` no longer link to them. Narrowing the archive broke four relative
links, which a checker over every `.md` in the extracted tree found — the handoff
pair, plus `docs/development.md` and `web/vendor/icons.md` reaching into `scripts/`,
which does not ship. All four now point at the repository by URL, so the archive
has no dangling relative link and a reader of an installed tree can still follow
them.

`v0.17.0` was then re-cut onto the fixed commit and the whole workflow is green.
The GitHub Release exists with both archives and `checksums.txt` and generated
notes; a fresh download passes `sha256sum -c`; `docker pull
ghcr.io/ykzird/astraeus:0.17.0` succeeds anonymously and
`docker buildx imagetools inspect` shows the two platform manifests plus their
attestation manifests. The OCI labels carry `version=0.17.0` and
`revision=eead1e8`.

**Re-cutting the tag was a deliberate one-off**, not a habit: the release page
never existed, so nothing outside GHCR had been published under that number. A
tag runs the workflow *as it stood at that tag's commit*, so a fix to the
workflow cannot reach an already-pushed tag — the choice was to move it or to
release the number again as `v0.17.1`.

**`v0.18.0` is the second release** (round 13), cut from the documentation-sweep
commit. Its run was green on the first attempt, including the publish job that
failed on the first release: that fix held, and the version examples in the docs
now name it. The release carries an archive whose `docs/` is exactly the five
reference pages — no `handoff.md`, no reviews — and `scripts/check-doc-links.py`
proved from CI that every relative link inside it resolves. The image is a
multi-arch index over `linux/amd64` and `linux/arm64` with an SBOM and a
provenance attestation per platform, and its labels carry `version=0.18.0` and
`revision=4603d07`.

**The server was measured, not just tested** (round 14). A harness in
`scripts/load-verify/` drives the JSON API and concurrent HLS streams and reads
the server's own `/metrics` before and after each phase, so the KPI registry and
the client can be held against one another. On this host — a Ryzen 7 9700X, 8
cores and 16 threads, no GPU — three workloads were measured.

*The JSON API* served **4,895 requests/second** across 8 concurrent clients at
1.6 ms mean and 3.6 ms p99, using 1.5 cores, with no failed request. The API is
not a constraint at any concurrency this host can reach.

*1080p H.264, direct play* — what a 1080p library mostly does, because a browser
decodes it as delivered — costs about **0.1 cores per stream** and never starts
ffmpeg: 1 stream took 0.15 cores and 8 took 0.78, with negotiation at 2 ms once
the probe cache was warm (28 ms for the first, cold, request). Aggregate
throughput flattened at about **2 GB/s**, which is this loopback and HTTP path
rather than the server, so the ceiling for direct-play viewers is bandwidth and
not CPU. A note for the beta: the first request against a file pays for a probe
and the rest do not.

*A 1080p HEVC source* — an x265 library, which a browser cannot decode and so
must be transcoded — ran at about **20× realtime in total**, and as with 4K that
total barely moved with concurrency: 19.4×, 21.6×, 20.9× and 18.4× for 1, 2, 4
and 8 streams. Each stream still had 2.3× realtime in hand at eight, so the host
serves roughly **20 simultaneous realtime 1080p transcodes**. CPU was 10.0 cores
for the first stream and 14.6 from four onward — 0.52 cores per realtime stream,
which is what `-preset veryfast -crf 21` costs rather than a misconfiguration.

*A 17 GB 4K DV/HDR10+ film* tone-mapped to 1080p ran at about **4.5× realtime in
total**, again nearly invariant with concurrency (0.73, 0.77 and 0.75 segments
per second at 6 s for 1, 2 and 4 streams), so roughly **four realtime viewers**
of that workload, with about 12% of headroom at four. Negotiation scaled with
concurrency — 1.74 s, 3.13 s, 6.10 s — because one stream already uses 11.5 of
16 threads, so each extra stream waits rather than finding idle capacity. The
host reached **99.5% busy with 15.9 cores working**, and every phase of every
run reported zero stream errors and zero probe errors.

The shape is the same in all three cases, and it is the useful result: **total
output is set by the host, not by the request.** More concurrent streams do not
produce more video; they divide the same capacity and each waits longer to
start. Hardware encoding is what would move that ceiling, which is why the
Intel box in §8 is worth measuring on.

The server's own numbers agreed with the client's throughout (1.71 against
1.74 s, 3.14 against 3.13 s, 6.11 against 6.10 s, 2.86 against 2.87 s), which is
the cross-check the harness exists to make — with one disagreement that turned
out to be real. Every transcode startup was landing inside a single
`DefaultBuckets` bin, so `astraeus_transcode_startup_seconds` reported
interpolated percentiles rather than measured ones: a p50 of **7.50 s** where
the client measured **6.10 s** (4 streams, 4K), and **3.75 s** against **2.87 s**
(8 streams, 1080p). The mean was right both times, because it comes from `_sum`
and `_count`.

That is fixed. The registry can now declare per-metric bounds, and both
transcode KPIs carry `TranscodeBuckets`, which is fine through the seconds a
transcode actually runs for; the test that pins it failed first at exactly
7.50 s. Re-running the eight-stream 1080p case moved the reported p50 from
3.75 s to **2.67 s**, against the client's 2.95 s. The residual difference is
real and explainable rather than arithmetic: the server measures from ffmpeg
starting, the client measures the whole round trip.

**Per-viewer library access** landed as of round 16, as `--access-policy`: a file
mapping each identity to the libraries it may see, plus an admin list. Until now
the gate decided whether a request was admitted and nothing decided what it could
read, so every admitted viewer saw the whole library. A policy in use denies by
default; a hidden library answers `404` rather than `403`, so the API is not a way
to enumerate what exists; and no policy at all leaves an install exactly as it
was. Visibility and administration are separate grants — an admin may scan,
enrich and change libraries without being able to see them — which is why an
operator who wants both says both.

It is enforced through one seam: a per-request scoped view of the repository that
every viewer-facing read goes through, so an entity behind a library the viewer
may not see is indistinguishable from one that does not exist, and a new handler
that reads through the scoped view cannot forget the check. The playlist and
segment route re-checks the library its session belongs to, because a session URL
is a capability that can be passed on or outlive a grant. `DELETE
/api/streams/{id}` deliberately does not, because stopping is cleanup and
requiring visibility would leave a transcode running after a revocation. A
position outliving its grant is filtered out of Continue watching, so revoking a
library does not leave its titles in the list.

Verified against a running server, not only in tests. With a policy granting one
of two libraries by name, that viewer listed one library and three entities, was
refused the other library's series with `404`, and was refused `403` on
registering a library and on `POST /api/scan`, while the operator identity saw
both libraries, all eight entities and registered one. A policy file that does not
parse stops the server with the offending line number rather than starting open.

**Artwork is not scoped.** `/api/images` is a shared cache keyed by the upstream
path, so a poster can be fetched by anyone who knows its file name, whatever
library it belongs to. The exposure is limited to artwork of media the requester
cannot play, and discovery requires guessing a name that only appears in a
listing they cannot read — but it is a real gap and is recorded rather than
implied away.

**TLS and the reverse proxy** are documented and verified as of round 15, in
`deploy/tls/`: a Caddy configuration beside the unit that terminates TLS, sends
the `Strict-Transport-Security` the server deliberately does not, and
establishes the identity the gate believes — one client certificate per tester,
whose subject becomes the viewer. The server has to be told both halves, so the
runbook's unit change is `--auth-mode proxy --auth-header X-Astraeus-User
--trusted-proxy 127.0.0.1/32,::1/128`.

It was run rather than read. Against Caddy 2 with `client_auth`: a connection
with no client certificate fails the handshake before any HTTP request; a valid
one returns `200` with HSTS and no `Server` header; two testers appear as
`user="CN=tester1"` and `user="CN=tester2"` on their request log lines; a
position reported by one is invisible to the other; a client-forged
`X-Astraeus-User` is overwritten by the proxy and appears nowhere in the log;
direct play returns a working `206` range response and a transcode returns a
playlist and a segment. Requests from the LAN address and from the Tailscale
address, each carrying a well-formed identity header, are refused
`403 untrusted_source` — the header is worthless without the address.
`--rate-limit 1 --rate-limit-burst 2` then answered `200, 200, 429` with
`Retry-After: 1` for one identity and `200` for a second, so the limiter really
is keyed on the person and not on the connection.

**One trap is worth carrying forward.** `--trusted-proxy 127.0.0.1/32` does not
cover `::1`, and a proxy that resolves `localhost` may connect over IPv6. The
symptom is a `403` for every request through a proxy that is plainly running,
which reads like a fault in the proxy. The runbook lists both loopback families
and says why.

**A quality choice that caps a ladder** landed as of 0.16.0, which fixes a
negotiation model that was thinner than its field name. Until now
`max_height` did two jobs: it clipped the target height *and* it switched the
ladder off, so a viewer picking 720p on the quality menu got exactly one encode
and their player had nowhere to step down to when the network could not sustain
it. `preferred_height` is the new, additive request: a ladder topped at that
height, which the client's player is free to adapt below. `max_height` on its own
keeps its meaning — one rendition, which is the deterministic request and the
cheap one, since a ladder is up to three encodes — and alongside a preference it
is the hard ceiling the ladder stays under. That last rule is what lets a
manifest say "my screen is 1080" and "I chose 720" at once, two facts the player
had been conflating into one field. A burn still pins one rendition whatever the
heights say, because the bitmap is composited once; the preference then chooses
that single encode's height, and a reason says so.

The design question was whether to repurpose `max_height` as the ceiling (which
is what its name suggests, and what every other `max_*` field in the manifest
is) or to add a field. Repurposing would have been a breaking change and would
have deleted the only way to bound a session to one encode; adding
`preferred_height` keeps the pin available and is the reason the change is
additive. Two details were load-bearing. A preference has to be **a limit, not
advice**: a check for the trap (a 4K source the client could otherwise
direct-play, with a 720 preference) is a unit test, because treating the
preference as advice would deliver the 4K original whole. And a field added to
`ClientCapability` has to be carried by `Normalise()`, which rebuilds the struct
field by field — the tests caught it missing, and the symptom was every
preference silently ignored.

Verified where it matters. The negotiation contract is unit-tested: a preference
builds a ladder topped there, `max_height` beside it is the ceiling, `max_height`
alone still pins, a burn keeps one rendition, and a negative height is refused.
The artefact test is the one that counts: `TestManager_LadderTopsAtThePreferredHeight`
asks for a 480 cap on a 720 source and asserts each **produced segment's measured
height** — 480/320/240 — because an argument-list assertion would pass for a
session that built three copies of one rung. The browser was checked with the new
`quality-verify.mjs` at **8/8**: the menu reads "Up to 480p", choosing it sends
`preferred_height: 480` and no `max_height`, the decision comes back with three
rungs topped at 480, and playback continues. `player-chrome-verify.mjs` was run
against a *transcoding* entity (the demo's HEVC film) rather than a direct-play
one, so its quality checks actually execute instead of being skipped; it scored
**22/28**, and all six failures are the checks after its 600-second seek, which a
four-second asset cannot satisfy (§8). The quality-menu check itself passes there
with the new labels.

**OCR for PGS image subtitles** landed as of 0.15.0, which turns the last
picture-only subtitle format into text a browser can toggle, restyle and search.
`internal/subtitles` gained a hand-written PGS decoder (`pgs.go`: PCS/ODS/PDS
segments, run-length-encoded objects, the BT.709 YCbCr palette, fragments
reassembled, palette-update display sets not mistaken for clears) and an OCR
pipeline (`ocr.go`). An image track is demuxed to a raw `.sup` with ffmpeg, each
cue's bitmap is composited over black and inverted — which keeps the light glyphs
and drops the dark outline — and `tesseract --psm 6` reads it. The result is
cached and served as WebVTT exactly like a text track.

Three decisions carry the work. **The OCR engine is an optional runtime
dependency, and its absence is not an error**: `OCRReady()` gates it, the track
keeps its missing URL, the endpoint keeps the `415 subtitle_format_unsupported`
it always returned, and startup logs "image_subtitles=burned in" with the reason.
A unit test pins that refusal and the integration test skips when tesseract is
absent. **Only PGS is read**: VobSub and DVB keep the refusal and the burn,
because the `sup` muxer the extractor uses takes PGS only and advertising a track
that then fails inside the extractor would be worse than not offering it. **The
front end decides from the URL, not from `text`**: `subtitleDeliverable` and
`subtitleNeedsBurn` are now pure functions in `web/core.js` with Node tests, so
an image track the server has read is offered as an ordinary `<track>` and one it
cannot read is still offered as "(burned in)".

Verified at the artefact level, not the command line. The parser test decodes a
generated fixture and compares the bitmap against the fixture's own index plane
pixel for pixel. The integration test muxes a fixture carrying real letters into
Matroska, runs the whole pipeline through real ffmpeg and a real tesseract, and
asserts the WebVTT contains **"ASTRAEUS MEDIA"** — the words the fixture drew. A
further unit test drives the "OCR found nothing" path with stub tools and pins
`ErrNoCues`. The browser was checked both ways: `ocr-verify.mjs` is **10/10** on a
server with tesseract (the image track is offered without "(burned in)", choosing
it attaches a `<track>` with **zero** playback requests, and the active cue text
on screen is the caption), and `burn-verify.mjs` is **10/10** on the same entity
against a server started with `--tesseract-bin /nonexistent/tesseract`, so the
burn fallback still works. `subtitle-verify.mjs` is **10/10** (text tracks
unregressed) and `player-chrome-verify.mjs` is 20/21 on the demo clips, its
documented auto-hide baseline for three-second fixtures.

The fixture side had to grow: `internal/testfixtures/pgs` gained a 5x7 bitmap
font and a `-text` mode in `scripts/pgsgen`, and its run-length encoder had a
real bug — runs of one or two coloured pixels were written as a bare byte, which
is not a valid run — found because the new test drove run lengths the rectangle
fixture never reached. The glyph size is chosen for the recogniser rather than the
eye (capitals about 28 pixels tall); at twice that the blocky font read "ASTRAEUS"
as "ASTRAELS".

**The second bitmap-subtitle reader, for VobSub, landed in round 18**, which
finishes what round 17 unblocked. `internal/subtitles` gained `vobsub.go`: a
decoder that walks a packet's control sequence (palette selection, display
rectangle, the two bitmap offsets), expands the run-length data as the two
interleaved fields the format uses — the first field's runs carry the even lines
and the second's the odd ones — and applies the container's palette into an
`*image.RGBA`, which is the same shape the PGS decoder produces and therefore
feeds the same OCR path. `matroska.go` is a small EBML reader, enough to reach a
track's codec private and its blocks, so the committed fixture can be proved
without ffmpeg.

Three things carried the work, and the first is the one worth remembering.
**The decoder was checked against ffmpeg's own decode, not against reasoning.**
A tiny C program linked against `libavcodec` decoded the same packet, and the
Go decoder's output was compared against ffmpeg's rendered frame pixel for
pixel (3440 ink pixels, 332x28); the RLE nibble order and the rectangle's
packed coordinates were both got wrong first and corrected from that comparison.
**The palette is in the container, not the picture stream**, so the extraction
keeps a container that has one: a Matroska source is copied into a standalone
Matroska file and its codec private travels with it, while any other source is
demuxed into raw SPU packets with the codec private read out beside them. A bare
MPEG-PS sample carries no palette and is refused rather than rendered blank.
**A single never-cleared cue needs a duration.** A VobSub track clears the screen
with a separate erase packet, and a one-cue sample has none, so the open cue
ended at its own start — and an empty cue is dropped as though it had never been
drawn. It now gets a placeholder duration, which a real rip's erase packet never
exercises.

Verified at the artefact level. The unit test decodes the committed fixture and
asserts the geometry and the ink count, and a second pins the field interleave by
decoding each field on its own and checking the interleaved result row by row.
An integration test re-encodes the PGS fixture with ffmpeg's `dvdsub` encoder and
runs the whole pipeline — the extraction, the decoder, the render and a real
tesseract — asserting the exact caption `"ASTRAEUS MEDIA"`, which the fixture's
glyph size at its native 640x360 makes reliable. The API routing is pinned both
ways: a VobSub track is advertised and served with an engine, and keeps its
`415` refusal and its burn without one; DVB, which still has no decoder, keeps
the refusal even with an engine.

**The DVB fixture was solved in round 19**, removing the last obstacle to a
third bitmap reader. The VobSub route did not transfer: ffmpeg's `dvbsub`
*encoder* accepts only bitmap subtitle input and refuses even its own `dvdsub`
output. Its PGS *decoder* can feed it, though, so
`scripts/make-dvb-fixture.sh` decodes this project's own PGS fixture and
re-encodes it as a real `dvb_subtitle` track. One non-obvious detail decides
whether the result is usable: the encoder authors against a 720x576 canvas and
rescales whatever it is given to fit, so the project's 640x360 fixture came out
with warped glyphs and the recogniser read `"RSTRAELS MEDIA"`. Drawing the
source at 720x576 — the canvas the encoder actually uses — makes the encode a
straight copy and tesseract reads `"ASTRAEUS MEDIA"` exactly. The script
validates itself the same way: it renders the fixture with ffmpeg and refuses to
leave one behind unless tesseract reads the caption out of it, so the committed
`.mkv` and `.ts` are vouched for by that check rather than by inspection. An
always-run unit test pins the framing (sync byte, then type, *segment id*, then
length — the id first, the reverse of the VobSub framing and the one field easy
to read backwards) and that the track carries no codec private, which is the
difference that matters: a DVB colour table lives in the stream, not the
container, so there is no extraction step beyond demuxing. The decoder itself is
the next increment (§7).

**Front-end unit tests** landed as of 0.14.0, which closes the largest remaining
untested surface. The player's timeline arithmetic — the source↔media time
conversion (`source = media + sessionStart` and its inverse), the produced window,
and the clock — moved out of `app.js` into `web/core.js`, a dependency-free UMD
file with no DOM, network or module state. `app.js` aliases those functions at the
top of its IIFE, so every call site still reads as it did, and reads the module as
`window.AstraeusCore` (loaded by `index.html` before `app.js`). `web/core.test.js`
tests it with **Node's own runner and asserts** (`node --test web/*.test.js`), so there is
no npm install, no lockfile and no framework — the same stance the CDP harnesses
take. CI gained the step. Seven tests cover the clock at every scale and its
refusal to print `NaN`, the offset arithmetic including a missing element clock,
the round trip, and the produced window's null case. Verified in a browser that
the extraction regressed nothing: `player-chrome-verify.mjs` is **20/22**, exactly
its pre-change baseline on the same three-second demo clips (the two auto-hide
checks fail there both before and after), with no console errors — which is what
a missing `window.AstraeusCore` would have produced — and `subtitle-verify.mjs`
is 10/10.

**OpenTelemetry trace export** landed as of 0.13.0: `internal/tracing` is a
hand-rolled OTLP/HTTP JSON encoder and batcher, opt-in with `--otel-endpoint`
(or `OTEL_EXPORTER_OTLP_ENDPOINT`). The choice mirrors the Prometheus
exposition: the wire format is the standard, so any OTLP backend receives the
spans, and the dependency list stays at three modules instead of taking the SDK
with its protobuf and gRPC trees. Every request is a server span carrying method,
path, peer and status; an incoming W3C `traceparent` is continued, so a trace
started by a proxy keeps its id here; a **playback negotiation** and the
**streaming session it starts** are child spans, so forking ffmpeg and waiting for
the first segment shows up as the part of the request that took the time; and the
request log line carries `trace_id` and `span_id`. The exporter batches, and three
behaviours are deliberate: a full queue **drops spans and counts them** in
`astraeus_spans_dropped_total` rather than adding latency to a finished request, a
collector that is down is logged rather than retried into a growing queue, and
shutdown drains before exiting.
Verified against a **real collector**: Jaeger all-in-one in a container listed
`astraeus-media`, received a span per request, and showed a request carrying an
incoming `traceparent` as a `CHILD_OF` span of it. A remux playback then produced
`playback.negotiate` (`playback.mode=remux`) and `stream.session.start`
(`stream.mode=remux`, `stream.session_id=...`) as a child of the request, in the
trace the request's own `traceparent` named. The request log correlation was read
back from the running server (`trace_id=4bf92f...` on the matching line), and so
was the export-failure warning when the collector was stopped.

**API rate limiting** landed as of 0.12.0: `internal/ratelimit` is a token bucket
per client in front of `/api/` only, off by default, with `--rate-limit` and
`--rate-limit-burst`. The two decisions that matter are both about what a client
*is* and what is worth bounding: the bucket is keyed by the access gate's
identity when there is one (behind a proxy every request arrives from the proxy's
address, so an address-keyed limit would be one global bucket for everyone the
proxy serves) and by the peer address otherwise, with `X-Forwarded-For` ignored
for the same reason the gate ignores it; and the UI, its assets and the HLS
segments are deliberately **not** limited, because they are delivery rather than
work — a page load pulls several files at once and one playback fetches a segment
every few seconds — while sessions stay bounded by `--max-sessions`.
`/api/health` is exempt so a limit can never make a busy server look unhealthy.
A refusal is a `429` in the API's error shape with `Retry-After: 1`, and is
counted in `astraeus_rate_limited_total`. The limiter sits *inside* the security
headers and the request log, so a refusal carries the same headers and appears in
the same log as any other response. The unit tests inject the clock (burst,
refill, independent keys, exempt, unattributable, the idle sweep, and a
misconfiguration that would throttle everyone as one) and a wiring test pins the
wrapper order; the running binary was checked by hand for the burst → `429` →
metric sequence. The sweep test caught a real bug: the bucket being created was
stamped after the sweep ran, so a sweep evicted the live bucket and handed a
heavy client a fresh allowance.

**Response hardening** landed as of 0.9.0: a content security policy with no
`unsafe-inline` and no `unsafe-eval`, `img-src 'self'`, plus nosniff, referrer,
frame, cross-origin and permissions headers. The policy is strict because the
front end earned it — no inline script, no inline style, no HTML-injection sink,
and a vendored hls.js with no `eval` (checked, not assumed); `blob:` is allowed
for `media-src` and `worker-src` because MSE plays a blob URL and hls.js demuxes
in a worker built from one. The artwork leak is closed on both ends: the UI loads
only the server's own proxy, and `img-src 'self'` makes a provider fetch
impossible rather than merely discouraged. Verified in a browser with a
throwaway CDP probe (third-party image refused with an `img-src` violation,
inline script refused, same-origin image fine) and then with the real harnesses:
32/32 transport checks and 9/9 resume checks, no console errors under the policy.

**Continue watching** landed as of 0.8.0: `GET /api/progress` lists the stored
positions most recently watched first, with each entity attached so a row needs
no second request, and the sidebar renders it as a section that is hidden when
there is nothing to continue. The finished-position rule lives in that query as
well as in the report path — applied after the `LIMIT`, a finished row consumes
one of the slots and a listing of one comes back empty, which is the bug the
adapter test caught.

**Image subtitles** landed as of 0.11.0, closing the last subtitle gap. PGS and
VobSub carry pictures, so no browser can render one as a track; the server
composites the bitmap into the picture instead. A client sends
`burn_subtitle_index` (the ffmpeg stream index, which `media_info.subtitles`
reports with `text: false` for image tracks); the decision comes back with
`burned_subtitle_index`, forces a transcode, and pins **one** rendition, because
the bitmap is composited once in a single filter graph. Two details are
load-bearing and were found by experiment rather than reasoning: the subtitle is
decoded from a **second opening of the input** (asking one input for `[0:v:0]`
and `[0:s:N]` in the same graph delivered no subtitle frames), and the composite
goes **after** the plan's own filters so a tone map cannot wash the subtitle out
(a bitmap is SDR white). The subtitle is scaled to the picture with `scale2ref`,
so a downscaled re-encode places it correctly. A text track named for burning is
not burned — it is delivered as a track, and the reasons say so. Burning is
irreversible for the session, so turning subtitles off re-negotiates.

Verified at the **pixel level**: a Go integration test writes a PGS fixture with
`internal/testfixtures/pgs` (no image-subtitle sample exists on this host and
ffmpeg will not encode a bitmap subtitle from text), burns it in with the real
ffmpeg, and asserts the produced segment carries the 400x60 rectangle while the
same source without the burn has none. The API and negotiation are unit-tested,
and the player was verified in a browser with `burn-verify.mjs` at **10/10** —
the menu offers the track as "(burned in)", the captured request carries the
index, the captured decision reports it, the control reflects it, playback
continues, and Off re-negotiates without a burn. `player-chrome-verify.mjs`
scores 20/22 on the demo clips both before and after the front-end change, so
the two auto-hide checks it fails there are a property of three-second fixtures,
not a regression; `subtitle-verify.mjs` is 10/10.

**Per-viewer progress** landed as of 0.10.0, which closes the largest
simplification the product had left. `PlaybackProgress` gained a `ViewerID`, the
`playback_progress` table's primary key widened from `(entity_id)` to
`(viewer_id, entity_id)`, and every read, write, clear and listing is scoped to
one viewer. The viewer is the identity the access gate already put on the
request; nothing reads an identity header in a handler, because with no gate a
header is a string the client made up, and believing it would let one viewer
name themselves another. With no gate there is a single viewer, `local`, so an
ungated install behaves exactly as before. Two honest consequences: `token` mode
reports every API client as `token`, so they share a place (per-viewer state is a
`proxy`-mode property), and a database written before the key widened keeps its
rows under `local`, because the identity that wrote them was never recorded.
SQLite cannot widen a primary key in place, so that upgrade rebuilds the table
inside one transaction; the migration is idempotent and a test drives it from a
hand-built pre-viewer schema. Verified mechanically with three API tests through
the real gate middleware (two viewers, finishing, ungated) and three adapter
tests (two viewers, the empty-to-`local` naming rule, the rebuild and re-run),
and in a real browser with `resume-verify.mjs` at **9/9 on each resume path** —
a synthetic three-minute H.264 clip for direct play and the same stream remuxed
to Matroska for the segmented path — including the continue-watching list
gaining the entity and dropping it when the position is cleared. (The 4K films
are a poor fixture for that harness: their three-rung HDR ladder takes longer to
start than the harness waits, so it reports no frame. The harness README now
carries the fixture recipe.)

**Resumable playback** landed as of 0.7.0: a `playback_progress` table keyed by
viewer and entity with a cascading delete, `PUT`/`DELETE
/api/entities/{id}/progress`, and a `progress` object on the entity detail. Two
rules carry the honesty: a position in the closing 5% clears the row because that
viewer is watched through, and a position past the end is a `400` rather than
something the next resume would trust.

**Audio track selection** landed as of 0.6.0. Every audio stream is probed into
`media_info.audio_tracks` (index, codec, channels, bitrate, language, title,
default), and `audio_track_index` chooses one; omitting it, or sending 0, means
the track the file marks `default` — the "first stream wins" behaviour was wrong
on any file whose second track is the one it means. Every audio decision follows
the chosen track, and a session maps it by *global* stream index (`-map 0:3?`),
the same way subtitle extraction already did. A chosen track forces at least a
remux, because direct play serves the whole file and the player would then pick
its own track. An index the file does not have is a `400` listing what exists.
The player got a menu beside the quality control; the fixture for it is the
dual-audio movie `scripts/make-demo-media.sh` now generates, which is HEVC so the
clip also transcodes and both menus are present at once.

The **adaptive bitrate ladder** landed as of 0.5.0. Its contract then was one
sentence: a manifest that pins `max_height` is asking for one rendition, and one
that omits it is asking to adapt. (Round 9, 0.16.0, added `preferred_height` so a
client can ask for a ladder it will not exceed; see the 0.16.0 paragraph above.
`max_height` alone still means one rendition.) A ladder is up to three rungs (the
target height, two thirds, half) encoded by one ffmpeg process, each with its own
VBV ceiling from a conventional table scaled by the client's limit, and the client
is handed `master.m3u8`. Per-rung settings need stream specifiers (`-c:v:0`,
`-filter:v:0`, `-pix_fmt:0`, `-force_key_frames:0`); without them every option
lands on the first rung and the "ladder" is two copies of one stream — which is
exactly what the first experiment produced, and why the integration test asserts
each rung's *measured* height rather than the argument list. Verified on the real
17 GB film: a 3-rung ladder at 1920x820 / 1278x546 / 960x410, delivered through
the allowlist, with the browser harness still at **28/28** — including the quality
menu, which at the time pinned a height and so exercised the single-rendition path
beside it.

The bitrate axis landed too, as of 0.4.0: a client's `max_bitrate_kbps` reserves
the audio's share and holds the video to the remainder as a VBV ceiling, applied
uniformly to every encoder family so quality-driven encodes stay quality-driven.
Verified end to end by encoding the same 9 Mbps source twice — 3.4 Mbps
unlimited, 632 kbps under a 500 kbps limit. What remains of that item is the
*ladder*: one rendition per request, no master playlist.

Packaging landed after that, in the commits following `f489fa4`: a multi-stage
`Dockerfile`, `deploy/astraeus.service`, `deploy/README.md` as the runbook, and
`.github/workflows/ci.yml`. The image was **run**, not just built: it serves
`/api/health`, scans a mounted library read-only, and delivered direct play, an
HDR tone map and an HDR remux *and* an HDR re-encode using the image's own
**ffmpeg 5.1.9** (the host has 9.0, so this was a real second data point for the
filter chain). Round 8 added **tesseract** to the image and verified OCR there
with its own **tesseract 5.3.0**: a mounted PGS fixture served WebVTT carrying
the fixture's caption.

**The systemd unit has now been started and exercised, on a clean Debian 12 VM**
(QEMU under TCG, §10), installing the `v0.17.0` release archive and following
`deploy/README.md` verbatim. `systemd-analyze verify` was clean, the service came
up and stayed up, and `/api/health` answered
`{"service":"astraeus","status":"ok"}`. `systemd-analyze security astraeus`, run
*inside* the guest rather than with `--offline=yes`, scores **1.6 (OK)** — the
number `deploy/README.md` quotes, now observed rather than predicted. The unit
declares no `StateDirectory` or `CacheDirectory`: the runbook's `install -d`
creates the two writable trees, and the service created
`/var/lib/astraeus/streams` (0700) and `astraeus.db` (0600) itself under
`UMask=0077`.

The hardening was tested the way this list asked for it — by making the service
run ffmpeg, not by watching it start. Negotiating `max_height: 360` against a
1280x720 source returned `"mode":"transcode"`, and the server's own ffmpeg child
was caught alive and read back its own sandbox:

```
cgroup:       0::/system.slice/astraeus.service
Name:         ffmpeg
Seccomp:      2          (28 filters)
CapEff:       0000000000000000
CapBnd:       0000000000000000
NoNewPrivs:   1
```

The syscall filter is therefore installed on ffmpeg itself, not merely on the
server that forks it, and the encode finished with no `EPERM` in the journal: the
produced segment ffprobes as H.264 `854x480` from a 1920x1080 source, which is
something a copied stream could not be. §8 records what this still does not
cover.

---

## 7. Open work

Priority order, with the reasoning. Take it top-down.

The claims that used to head this list — the release run, the systemd unit and
the TLS example — are done, and observed rather than reasoned about. §6 records
what running them found, including the defects that only a real run could
surface. What is left:

1. **A DVB image-subtitle decoder.** The fixture half is done, which was the
   blocker: round 19 found that ffmpeg's `dvbsub` encoder takes only bitmap
   subtitle input (so the VobSub trick does not transfer) but that ffmpeg's PGS
   *decoder* can feed it, and `scripts/make-dvb-fixture.sh` now decodes this
   project's own PGS fixture into a real `dvb_subtitle` track that ffmpeg
   re-decodes and tesseract reads. A decoder was then written and **withdrawn**:
   it produced a correctly sized crop and a plausible colour table but drew only
   696 of ffmpeg's 3440 ink pixels, so it was not working and shipping it would
   have been worse than not having it. `TODO.md` now carries the verified
   segment layouts, the pixel-string grammars and the exact point the attempt
   stopped, so the next one starts from evidence. What is left is that decoder
   and the routing, which is the same shape the VobSub reader uses; a DVB track
   keeps its colour table in the stream rather than the container, so there is
   no extraction step beyond demuxing. **Budget note: this is a bigger increment
   than the VobSub reader was, and the fixture's framing is the only part that
   is already pinned.**
2. **Dolby Vision profile 5 done properly** (libplacebo with a Vulkan device, or
   the Dolby Vision tooling) and **carrying mastering-display / content-light
   metadata through a re-encode**. Both are refinements of work that is otherwise
   complete, and both need hardware or samples that do not exist on this host.
3. **Validating an identity-aware proxy's assertion, if one is ever put in
   front.** In `proxy` mode the gate believes `Tailscale-User-Login` or
   `Cf-Access-Authenticated-User-Email` from a trusted address, and neither is
   signed — so the whole control is that the proxy is the only path to the port
   (which is why the backend stays on loopback). Cloudflare Access signs
   `Cf-Access-Jwt-Assertion` (RS256 against the team's JWKS, with `iss` and
   `aud`) precisely so an origin can verify the claim instead of trusting it, and
   Go's `crypto/rsa` and `crypto/x509` would let that be done here without a
   dependency. Not needed for the Caddy + client-certificate arrangement, whose
   verification is the TLS handshake — but it is the missing control the moment
   an Access path is added. Round 15 evaluated consolidating the whole ingress on
   `tailscale serve` instead and did not take it: Serve's identity headers are an
   assertion too, its HTTPS mode under Headscale needs `dns.https_certs` plus an
   authoritative DNS server for the ACME challenge (PR #3300), Headscale does not
   ship Funnel, and a header is not populated for traffic from tagged devices.

`TODO.md` carries the complete list with detail.

---

## 8. Known-unverified — treat with suspicion

- **NVENC, AMF and VideoToolbox have never run on real hardware.** They are
  implemented, unit-tested for their arguments, and covered by a test that drives
  detection through a stub ffmpeg standing in for an NVIDIA host. The startup
  probe validates the actual options on the machine that runs it.
- **VAAPI is in the same position**, and its `-vaapi_device` was missing from
  real sessions until recently — the probe passed while playback would have
  failed. Fixed, and asserted by a test, but still unexercised on hardware.
- **HDR on hardware is unverified.** The 10-bit probe runs per encoder on
  whatever host starts the server, so the mechanism is exercised; the hardware
  families themselves are not. On this machine the probe verified five *software*
  encoders at 10-bit, and the VAAPI tone-map path (`tonemap_vaapi` is not used;
  tone mapping happens in software before the upload) has never run.
- **HLG is implemented but never seen.** It is classified, and tagged
  `arib-std-b67`, but no HLG sample exists here to tone map or pass through.
- **Dolby Vision profile 5 tone mapping is approximate and untested.** No profile
  5 sample exists here either; the reasons say the colour will be approximate,
  which is the honest position, but the approximation itself has not been looked
  at.
- **Firefox is not installed**, so the hls.js path has only been verified in
  Chromium, and the native-HLS (Safari) branch has never been observed firing.
- **The ladder has only been exercised with software encoders.** Every rung of
  every ladder tested here was libx264 or libx265. A hardware ladder means one
  `hwupload` per rung on one device, which is plausible but unverified; the
  software fallback after a hardware failure rebuilds the decision for software
  encoders, so a failed ladder should still land somewhere playable.
- **The development host's ffmpeg is not the shipped one, and that hid a defect
  for a whole round.** The host runs ffmpeg 9.0; the container ships Debian
  bookworm's 5.1.9, and CI runs Ubuntu's 6.1. A ladder used to map the audio once
  per rung with `-c:a copy`, which makes every variant carry the *same* elementary
  stream. ffmpeg 9 accepts that; 5.1 and 6.1 drop variants from the master
  playlist without failing the session, so the container's ladder had been serving
  two rungs of three. The fix re-encodes the audio per rung, and it is verified
  against 5.1.9 by running the integration test inside a bookworm container —
  which is now the cheap way to check any ffmpeg-version behaviour this host
  cannot see. **The lesson worth keeping: when a claim depends on ffmpeg's
  behaviour, run the test against the version the container ships, not the one on
  the desk.**
- **The content security policy is verified against the current UI, not against
  a future one.** A new inline `<style>` or a script fetched from elsewhere would
  be refused at runtime and show up as a console error in the harness — which is
  the point of the policy, but it means a front-end change that adds one will
  fail verification rather than merely being flagged.
- **Only `web/core.js` has unit tests; the rest of the UI is still covered by
  hand-run browser harnesses.** The testable seam grew in round 8 — it now also
  holds the subtitle-track classification (`subtitleDeliverable`,
  `subtitleNeedsBurn`), which is what decides whether an image track is offered
  as a `<track>` or as a burn — but the render functions, the player controls and
  the menu nodes themselves have no seam a Node test can reach, and the CDP
  harnesses that do cover them need Chromium and a running server: they are run
  by hand on this host, are Chromium-only (Firefox is not installed), and CI runs
  `node --test web/*.test.js` but not them.
- **Tracing is verified for traces over OTLP/HTTP against one backend.** The
  encoder was accepted by Jaeger all-in-one (pulled and run here), but no other
  OTLP backend has been tried, and only the attribute types this code emits were
  exercised. OTLP over gRPC, sampling policies, baggage and propagation to the
  server's own outbound calls (metadata provider, artwork proxy) are not
  implemented, and scans, metadata lookups and individual segments are not
  spanned — so the specification's "from API call to media segment delivery" is
  covered up to the session starting, not to each segment.
- **Rate limiting is verified by unit tests, one manual run and now one real
  proxy, but not by a load test.** The identity-keyed path was exercised for real
  in round 15 through Caddy with client certificates: a burst of two at one per
  second answers `200, 200, 429` with `Retry-After: 1` for one identity, and a
  second identity gets its own bucket. What that still does not cover: Tailscale
  and Cloudflare Access set their *own* header names, which the gate believes by
  default but which no run has actually seen arrive; `token` mode puts every API
  client in one bucket; and the limiter is per process, so several replicas
  behind one proxy limit as a sum. The idle sweep that bounds bucket memory is
  unit-tested with a two-key threshold, but has not been observed under a flood
  of distinct addresses.
- **One transport assertion is timing-sensitive on a 4K transcode.** A harness
  run against the 17 GB film reported 31/32 once, with the failing check outside
  the captured tail, and two immediate re-runs passed 32/32 on the same code.
  Treat a single failure there as worth re-running before chasing it — but do
  capture the whole output, because `tail` is what lost the name of the check.
- **Image subtitles are verified for PGS, for software encoders, at one
  rendition.** The fixture is hand-written by `internal/testfixtures/pgs` because
  no real Blu-ray or DVD sample exists on this host, so what was exercised is
  ffmpeg's PGS decoder on a synthetic rectangle — not a real Blu-ray subtitle with
  its palette, cropping and partial object updates. VobSub shares the track
  classification and the same overlay path but has never been decoded here at
  all; round 17 showed a VobSub sample can at least be synthesised (§7), which is
  a fixture and not yet a decoder. `scale2ref`/`overlay` has only been run with
  libx264: a hardware encoder's
  upload filter has never been combined with the burn graph, and a ladder is
  refused for a burn rather than composited per rung. A real PGS sample would be
  the cheapest way to strengthen all of this.
- **The burn fixture's cue timing is approximate.** Hand-written display sets are
  shifted by up to a second when ffmpeg muxes them from `.sup` into Matroska,
  which is why the integration test uses a five-second cue and asserts on a
  segment well inside it. The burn path does not depend on that, but a test that
  asserted exact cue boundaries would fail for a reason that is the fixture's, not
  the server's.
- **OCR is verified against handwritten fixtures, not real disc subtitles.** The
  PGS reader was written from the format's own field layout, and the only streams
  it has decoded are the ones `internal/testfixtures/pgs` writes. The fixture
  carries one object per cue, one palette, no cropping, no partial-object updates
  and no epoch reuse; a real Blu-ray subtitle brings all of those, plus
  anti-aliased edges and a black outline. ffmpeg's decoder agreeing with the
  fixture (checked by overlaying the fixture on black and reading it with
  tesseract) is evidence the fixture is well-formed, not evidence the reader
  handles a real disc. The reader ignores window definitions and treats any
  display set with no composition objects as a clear unless it is a palette
  update; that is right for the fixture and probably right for a disc, but it is
  reasoning rather than observation.
- **A DVB decoder was attempted and withdrawn.** The fixture is committed
  (`internal/subtitles/testdata/dvb-caption.{mkv,ts}`) and its framing is
  unit-tested, but the first decoder drew a correctly sized crop and a plausible
  colour table and only 696 of ffmpeg's 3440 ink pixels, so it was removed
  rather than left in the tree looking finished. `TODO.md` carries the verified
  segment layouts, the pixel-string grammars and the exact point the attempt
  stopped. Nothing in the server reads a DVB track: it still keeps the `415`
  refusal and the burn.
- **A DVB track's colour is BT.601, not the BT.709 the PGS reader uses.** The
  two are close but not identical, so a DVB decoder built by copying the PGS
  colour conversion would be subtly wrong rather than obviously broken.
- **Recognition can be wrong, and the fixture was tuned until it was not.** The
  integration test asserts the exact caption because the fixture's glyph size was
  chosen so tesseract reads it; at a different size the same font read
  "ASTRAEUS" as "ASTRAELS", so OCR accuracy is a property of the picture, not a
  guarantee. A bitmap that is not text can be read as some — a solid rectangle
  came back as a mark — which means a PGS track of a shape may yield a spurious
  cue rather than none. The words OCR produces should be treated as approximate.
- **OCR covers PGS and VobSub; DVB has no decoder.** The PGS extraction copies the
  stream with `-f sup`, which the `sup` muxer accepts only for
  `hdmv_pgs_subtitle`; the VobSub extraction keeps a container that carries the
  palette, so a bare MPEG-PS track with no palette is refused rather than
  rendered blank. DVB (`dvb_subtitle`) therefore still keeps the `415` refusal
  and the burn even with an engine installed. `--ocr-language` is passed through
  to tesseract but only `eng` is installed here, and no non-English caption has
  been recognised.
- **The VobSub reader is verified against a synthetic sample.** Its input is the
  codec private a container carries, and a real disc rip's palette arrives in an
  `.idx` sidecar, which the same parser reads -- but nothing here has tried a real
  one. The reader decodes the two-field run-length form ffmpeg's encoder emits
  and the eight-bit form the format allows; the eight-bit branch has no sample.
- **The OCR path has not been exercised with a hardware encoder or a ladder**,
  for the same reason the burn path has not: the burn and the OCR path both apply
  to the single-rendition case, and only software encoders exist on this host.
- **Progress is per viewer, but the viewer is only as real as the gate.** In
  `proxy` mode it is a forwarded identity and two people keep separate places; in
  `token` mode the gate names every API client `token`, so those clients share
  one; with the gate off there is a single `local` viewer. The API tests drive
  two identities through the real gate middleware, but that is a test double for
  a proxy, not Tailscale or Cloudflare Access on a real network, and no browser
  has made requests as two identities: the CDP harnesses run against an ungated
  server, so they exercise the `local` path only.
- **The pre-viewer migration attributes old rows to `local`.** A database from
  0.9.0 or earlier is rebuilt (SQLite cannot widen a primary key in place) and
  its rows are claimed by the single viewer, because the identity that wrote them
  was never recorded. On an install that already ran an identity-aware gate, a
  viewer will not find their older positions under their own identity; they are
  under `local` and are not lost. The rebuild is covered by a test that starts
  from a hand-built pre-viewer schema, but it has not been run against a copy of
  `real.db` or of a production database.
- **A progress report is only as good as the player's clock.** The stored
  position comes from the client; the server validates its range and clears a
  finished one, but it cannot tell a real position from a plausible wrong one.
  The consequence is bounded: the worst case is a resume in the wrong place,
  which the transport's seek control fixes.
- **The audio menu's browser behaviour is verified against the demo library, not
  the real films.** The 4K films in the real library each carry one audio track,
  so the menu only appears for the generated dual-audio fixture. The mapping
  itself is verified at the byte level by the Go integration test.
- **Ladder cost on a weak host is unmeasured.** The development machine is a
  9700X and transcoded three rungs of 4K-to-820p without complaint. A host with a
  quarter of that CPU would feel it, and there is no rung cap by host. This is why
  `max_height` alone still asks for one encode: a client that wants determinism,
  or a host that cannot afford three, can pin a rendition instead of capping a
  ladder.
- **A `preferred_height` is a top rung, not a floor, and nothing reports where
  the player actually settled.** The server builds the ladder and hands over the
  master playlist; whether the player ever leaves the top rung is its own
  decision, and no KPI or reason says which rung was watched. The integration test
  verifies the rungs that are *produced*, not the rung a viewer saw. The browser
  check (`quality-verify.mjs`) runs on the demo's four-second HEVC film, so it
  proves the request and the decision, not sustained adaptation over minutes.
- **`player-chrome-verify.mjs` cannot pass on the bundled demo library.** It
  seeks to 600s, which no three-second clip can satisfy, so every check after the
  seek fails for fixture reasons; run against the 17 GB film instead (see §3), or
  expect a score in the low twenties on the demo assets. The quality menu check
  itself passes there, with the labels this round introduced.
- **No browser has been asked to play an HDR stream.** The HDR path is verified
  down to the produced segment (10-bit, `bt2020nc`/`smpte2084`/`bt2020`), not to
  a compositor showing it correctly — which is why the client has to declare
  `supports_hdr` rather than being assumed capable.
- **A tag push does not run the full test suite.** `ci.yml`'s `push` trigger is
  filtered to `main`, so GitHub runs the integration tests when a commit lands on
  `main` and not again when a tag is pushed (a filtered event runs only for the
  refs it names). The release workflow's gate re-runs `vet` and the unit tests, so
  a tag cut anywhere but `main` would publish with less coverage than a merge.
  Tag from `main`. Both `v0.17.0` tags were cut that way, and the second one only
  after its commit had passed CI on `main`.
- **The VM recipe in §10 has been run once on this host, and its corrections are
  recorded there.** What it still does not cover: KVM (there is none here, so
  every timing in §10 is TCG and worth nothing as a benchmark) and any hardware
  encoder. The guest was Debian 12.15 with the distribution's **ffmpeg 5.1.9** —
  a third data point beside the host's 9.0 and the container's 5.1.9.
- **What packaging still does not cover is hardware, not the packaging.** The
  container and now the systemd unit have both been run for real — the unit on a
  clean VM (§6, §10) — and the release workflow has run end to end, so the GitHub
  Release and the GHCR push are observed rather than assumed. What is still
  reasoned about is VAAPI, NVENC, AMF and VideoToolbox inside either shape. The
  guest has no GPU and no render node, so it rejected all six hardware encoders at
  startup and transcoded on the CPU: that is a statement about the guest, not
  about the unit. The arm64 image needed QEMU for the runtime layer's `apt-get`;
  `tonistiigi/binfmt --install arm64` registered it on this host, which is a
  kernel-level change this repository did not make and does not document, because
  CI's `docker/setup-qemu-action` does the same thing from scratch.
  `deploy/README.md` states all of this in place, so a reader of the deployment
  docs does not have to find this handoff to learn it.
- **The unit's `SystemCallFilter=@system-service` is exercised, but not
  exhaustively.** A `libx264` transcode ran to completion under it inside the
  guest, with the filter attached to the ffmpeg process itself (§6) — that is the
  encoder the bundled fixtures and any CPU-only install will use. The set also
  admits `@resources` (`sched_setaffinity`, `mbind`, `set_mempolicy`), so x264's
  thread and NUMA probing is not the risk it might look like from the unit's
  comments. What remains unexercised is every *hardware* encoder path, for the
  reason above: no render node on this host or in the guest.
- The visual design of a narrow player, and Firefox/Safari rendering generally,
  have not been looked at by eye.

### When the Intel and NVIDIA boxes are available

This is the highest-value test remaining, and it should be cheap:

```sh
./astraeus-server serve --db astraeus-test.db --web-dir web --addr 127.0.0.1:8910
# read the startup lines, then:
curl -s localhost:8910/api/system/capabilities | python3 -m json.tool
```

Read `hardware_acceleration` (which families were accepted) and
`rejected_encoders` (which were not, and ffmpeg's own complaint). A reason like
`Cannot load libcuda.so.1` or `DLL libamfrt64.so.1 failed to open` means a driver
or runtime is missing; `Error creating a MFX session` on an Intel box usually
means the media driver stack; a device error in a container means `/dev/dri` or
`/dev/nvidia*` was not mapped in. If a family is accepted, play the 4K film and
confirm the CPU stays idle — the transcoding is then on the GPU.

If something is rejected and the reason looks like *our* flags rather than the
driver, that is a real bug worth reporting; the flags were written from vendor
documentation without hardware to check them against.

---

## 9. Things that look wrong but are not

- **Two Play buttons on a page** — the hero's, and the player's inside the
  overlay. They are different affordances, and the hero is `inert` while the
  player is up.
- **The hero Play button sits exactly where the control bar renders.** This is
  why the controls once never hid: the bar appeared under the stationary cursor.
  Any future change to the hero or the bar layout should keep this in mind.
- **`internal/library` still holds models, the port and the scanner** — 2,000
  lines doing one job, not a missed extraction. The seams that were worth cutting
  have been.
- **No `-color_primaries` appears anywhere in a session command line.** That is
  the fix, not an omission: ffmpeg writes the encoder's VUI from the *frame*
  properties and ignores those options — verified by re-encoding a PQ source and
  finding `unknown` primaries and transfer, and by watching them break tags that
  `-x265-params` alone got right. Colour is set where it is read: `zscale` stages
  in the tone-map chain, `setparams` on an HDR pass-through.
- **Every encoder is probed twice at startup**, once at 8 bits and once at 10.
  That is deliberate: an encoder can work at 8 and refuse 10, and offering HDR on
  the strength of the 8-bit result would fail at the first frame. The 10-bit list
  is reported separately as `hdr_video_encoders`, each with the pixel format it
  accepted.
- **A remux of an HDR source keeps HDR without re-encoding**, so an HDR film over
  an incompatible container is copied rather than converted. Only a client that
  declared `supports_hdr` ever reaches that path, because an SDR client's
  decision is a tone map.
- **A bitrate limit is honoured as a ceiling, not as a target.** The video keeps
  its own quality settings and is bounded with `-maxrate`/`-bufsize`; converting
  the encode to a fixed `-b:v` would make quality-driven encodes worse at the same
  size. The ceiling is an average, so a segment shorter than the buffer can
  measure above it — 632 kbps measured against a 500 kbps limit on a six-second
  fixture, converging over a real stream.
- **A tone-map session fails rather than degrades on a mis-tagged source.**
  `zscale` reports "no path between colorspaces" if a file claims PQ but is not
  HDR. Failing is deliberate — a wrong picture delivered silently is worse — but
  the error arrives as `500 stream_start_failed`, which does not name the
  mis-tagging. Worth improving if it is ever seen in the wild.

---

## 10. A QEMU VM: the systemd unit and the release run

**This has been run once (round 12), and the corrections below are what that run
taught.** The two claims §7 used to head with — a released artifact to install,
and a systemd manager to install it under — need an environment this development
host does not have. A small cloud-image VM gives both, and the tooling is
already here: `qemu-system-x86_64`, `qemu-img`, `cloud-localds`.

The host has **no `/dev/kvm`**, so the guest runs under TCG (software emulation).
That is fine for "did the service start, answer, and survive an ffmpeg run" and
useless as a benchmark. It is also less painful than it sounds: this guest booted
to ssh in **19 seconds** and installed ffmpeg under cloud-init while doing so. Do
keep fixtures small — but note the tension: a 5-second 720p clip transcodes in
about a second, which is *too fast to catch the ffmpeg child alive*. Use a 1080p
clip of 30–40 seconds when the point is to observe the process rather than the
artefact.

### Corrections to the original sketch

- **Do not name the cloud-init user `astraeus`.** The runbook's step 1 is
  `useradd --system ... astraeus`, so a cloud-init user of that name makes the
  runbook's *first* command fail with "user already exists". The run below uses
  `ops`, which leaves the runbook runnable verbatim — which is the point of
  following it rather than paraphrasing it.
- **`-nographic` writes the console to the job's stdout and gives no way back
  in.** `-display none -serial file:console.log` plus the `hostfwd` below is
  easier to watch from a background job.
- **The `curl /api/health` in runbook step 4 can fail even though the service
  started.** The server binds its port only after the startup capability probe,
  which forks ffmpeg once per encoder family; on this guest that took ~9 seconds
  and the next line of the runbook raced it. Wait for the port, or retry —
  `deploy/README.md` now says so.
- **This host's ssh refuses its own system config** (`Bad owner or permissions
  on /etc/ssh/ssh_config.d/20-omarchy-keepalive.conf`), so every ssh and scp
  below needs `-F /dev/null`. That is a quirk of this desk, not of the guest.

```sh
# A Debian cloud image, and a copy-on-write disk so the base image stays clean.
curl -LO https://cloud.debian.org/images/cloud/bookworm/latest/debian-12-genericcloud-amd64.qcow2
qemu-img create -f qcow2 -F qcow2 -b debian-12-genericcloud-amd64.qcow2 test.qcow2 20G

# cloud-init: one user (NOT named astraeus), a key, and ffmpeg. Anything else is
# installed later so that what the runbook says is what actually happens.
ssh-keygen -t ed25519 -N '' -f id_vm
cat > user-data <<YAML
#cloud-config
users:
  - name: ops
    sudo: ALL=(ALL) NOPASSWD:ALL
    lock_passwd: true
    ssh_authorized_keys: [ "$(cat id_vm.pub)" ]
packages: [ffmpeg, curl, sqlite3]
package_update: true
YAML
printf 'instance-id: astraeus-test\nlocal-hostname: astraeus-test\n' > meta-data
cloud-localds seed.iso user-data meta-data

qemu-system-x86_64 -m 4096 -smp 4 -accel tcg,thread=multi -cpu max \
  -display none -serial file:console.log \
  -drive file=test.qcow2,if=virtio -drive file=seed.iso,if=virtio,format=raw \
  -netdev user,id=n0,hostfwd=tcp:127.0.0.1:2222-:22 -device virtio-net-pci,netdev=n0

ssh -F /dev/null -i id_vm -o StrictHostKeyChecking=no -p 2222 ops@127.0.0.1
```

### Inside the guest

Follow `deploy/README.md`'s systemd runbook and nothing else — that is the second
half of the point, because the runbook is what a user follows. Install the
**release archive**, not a local build:

- it comes from
  `https://github.com/ykzird/astraeus/releases/download/v0.17.0/`; the guest was
  Debian 12.15 with the distribution's **ffmpeg 5.1.9**, a third data point for
  the filter chain beside the host's 9.0 and the image's 5.1.9;
- `sha256sum -c checksums.txt` before extracting — and the archive must carry
  `deploy/` and the five user-facing pages of `docs/`, or the runbook cannot be
  followed at all (that was a real defect; see §6);
- `systemd-analyze verify` (clean), then start it;
- `curl -s localhost:8642/api/health`, **after** the port opens;
- `systemd-analyze security astraeus` for the real score — **1.6 (OK)**, matching
  the number `deploy/README.md` quotes from `--offline=yes`;
- the unit declares no `StateDirectory`/`CacheDirectory`. The runbook's
  `install -d` creates the two writable trees, and the service creates
  `streams/` (0700) and the database (0600) itself under `UMask=0077`;
- generate a clip, register the library through the API using the token in
  `/etc/astraeus/astraeus.env` (the unit runs `--auth-mode token`, so every call
  except `/api/health` needs `Authorization: Bearer`), scan it, and negotiate a
  `max_height` **below the source height** — that is what produces
  `"mode":"transcode"` and therefore what makes the server fork ffmpeg inside the
  sandbox. A direct play proves nothing about the filter.

To see the sandbox rather than infer it from the output, catch the child while it
runs and read its own account of itself:

```sh
PID=$(pgrep -f '[f]fmpeg' | head -1)
cat /proc/$PID/cgroup                     # 0::/system.slice/astraeus.service
grep -E '^(Name|Seccomp|Seccomp_filters|NoNewPrivs|CapEff|CapBnd):' /proc/$PID/status
```

`Seccomp: 2` with a non-zero filter count, `CapEff`/`CapBnd` at zero and
`NoNewPrivs: 1` is the hardening applied to ffmpeg *itself*, not merely to the
server that forked it. A segment that ffprobes at the height that was asked for,
rather than the source's, is the artefact proving the encode really ran. An
`EPERM` in the journal would name the filter (or `ProtectSystem`/`ReadWritePaths`)
as the cause; the round-12 run produced none.
