# Handoff

**As of the round-10 work of 2026-10-08 — release automation, and the repository
going public. 126 tracked files; the in-tree version is `dev` and a release takes
its number from its tag (the next tag would be v0.17.0).** (`git log` names the
commits; the previous handoff was `e2a434e`, which made a quality choice cap a
ladder. Round 9 was that ladder cap, round 8 OCR for PGS image subtitles, round 7
front-end unit tests, round 6 trace export, round 5 API rate limiting, round 4
image subtitles by burn-in, round 3 per-viewer progress, and round 2 HDR/Dolby
Vision, packaging, the bitrate ceiling, the adaptive ladder, audio track
selection, resumable playback, the continue-watching list and response
hardening.)

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
cgo, no build step for the front end. `README.md` is the reference for what it
does; `SPECIFICATION.md` is the design; `TODO.md` holds the honest state of what
is missing.

An adversarial review of the codebase, the competitive landscape and the front
end lives in `docs/adversarial-review.md`, with the two raw reports in
`docs/review/`. Those are point-in-time and say so.

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

# OCR for PGS. The parser is unit-tested against a generated fixture, pixel for
# pixel; the OCR path is integration-tagged and asserts the *words*, through real
# ffmpeg and a real tesseract - it skips when tesseract is absent, and a separate
# always-run unit test pins the refusal that an install without it keeps. The API
# routing (image track -> ConvertImage, VobSub stays burn-only) is unit-tested.
mise exec -- go test -count=1 -run 'ParsePGS|OCR|ConvertImage' ./internal/subtitles/
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

- every metric declared in `internal/observability/kpi.go` is named in the README,
  and nothing named there is undeclared;
- every route mounted in `internal/api/server.go` appears in the README's table.

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
mise exec -- scripts/build-release.sh 0.17.0
(cd dist && sha256sum -c checksums.txt)
tar -xzf dist/astraeus-server_0.17.0_linux_amd64.tar.gz -C /tmp
/tmp/astraeus-server_0.17.0_linux_amd64/astraeus-server version   # 0.17.0

# The image carries the version and the OCI provenance labels; both are read
# back from the built artefact rather than from the Dockerfile.
docker build --build-arg VERSION=0.17.0 --build-arg REVISION="$(git rev-parse --short HEAD)" \
  -t astraeus-media:0.17.0 .
docker inspect astraeus-media:0.17.0 --format '{{json .Config.Labels}}' | python3 -m json.tool
docker run --rm astraeus-media:0.17.0 version

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
- **Docs are part of the change.** `README`, `SPECIFICATION`, `TODO`, and the two
  READMEs beside the code they describe (`web/README.md`, `deploy/README.md`)
  have all been kept in sync; `TODO.md` marks gaps honestly rather than
  aspirationally.

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
templates, a code of conduct and CODEOWNERS — and `main` is protected: a pull
request, green CI, linear history.

**Release automation** landed as of round 10 (untagged; the next tag is
v0.17.0). Pushing a `v*` tag runs `.github/workflows/release.yml`, which gates on
the unit tests, builds one archive per platform with
`scripts/build-release.sh`, publishes a GitHub Release with a checksums file and
generated notes, and pushes a multi-arch image — `linux/amd64` and `linux/arm64`
in one manifest — to GHCR with build provenance and an SBOM attested alongside
it. The workflow is thin on purpose: all of the work is in the script, which runs
by hand, and that is how it was verified. CI has since run on GitHub — and found
the ladder defect described above — but the release workflow still has not,
because no tag has been pushed.

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
platforms. `actionlint` accepts both workflows. What has *not* happened is GitHub
running any of it.

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
the fixture's caption. The unit passes `systemd-analyze verify` and scores 1.6
(OK) on `systemd-analyze security`. See §8 for what that does *not* cover.

---

## 7. Open work

Priority order, with the reasoning. Take it top-down.

1. **A TLS example.** Release automation landed in round 10 (see §6), so a tag
   now builds and publishes the archives and the image; what is still missing is
   the reverse-proxy configuration beside the unit, and the runbook for the
   access-gate interaction a proxy creates.
2. **A second image-subtitle reader, for VobSub.** OCR now covers PGS only; a
   VobSub (or DVB) track keeps its refusal and its burn because it lives in a
   different container with a different palette, and no such sample exists here.
   This is the natural continuation of round 8 and is smaller than it was: the
   pipeline, the routing and the fixture font all exist, so the work is one more
   decoder plus a fixture. See the OCR bullet in §8 for what is unverified.
3. **Per-user *access*, if it is ever wanted.** Progress is per viewer now, but
   the gate remains instance-wide: it admits a request, it does not decide what
   the request may see, so every admitted viewer sees the whole library.
4. **Dolby Vision profile 5 done properly** (libplacebo with a Vulkan device, or
   the Dolby Vision tooling) and **carrying mastering-display / content-light
   metadata through a re-encode**. Both are refinements of work that is otherwise
   complete, and both need hardware or samples that do not exist on this host.

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
- **Rate limiting is verified by unit tests and one manual run, not by a real
  proxy or a load test.** The identity-keyed path is exercised through the gate's
  context in a test, not by Tailscale or Cloudflare Access forwarding headers on a
  real network; `token` mode puts every API client in one bucket; and the limiter
  is per process, so several replicas behind one proxy limit as a sum. The idle
  sweep that bounds bucket memory is unit-tested with a two-key threshold, but has
  not been observed under a flood of distinct addresses.
- **One transport assertion is timing-sensitive on a 4K transcode.** A harness
  run against the 17 GB film reported 31/32 once, with the failing check outside
  the captured tail, and two immediate re-runs passed 32/32 on the same code.
  Treat a single failure there as worth re-running before chasing it — but do
  capture the whole output, because `tail` is what lost the name of the check.
- **Image subtitles are verified for PGS, for software encoders, at one
  rendition.** The fixture is hand-written by `internal/testfixtures/pgs` because
  no real PGS or VobSub sample exists on this host, so what was exercised is
  ffmpeg's PGS decoder on a synthetic rectangle — not a real Blu-ray subtitle with
  its palette, cropping and partial object updates. VobSub shares the track
  classification and the same overlay path but has never been decoded here at
  all. `scale2ref`/`overlay` has only been run with libx264: a hardware encoder's
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
- **Recognition can be wrong, and the fixture was tuned until it was not.** The
  integration test asserts the exact caption because the fixture's glyph size was
  chosen so tesseract reads it; at a different size the same font read
  "ASTRAEUS" as "ASTRAELS", so OCR accuracy is a property of the picture, not a
  guarantee. A bitmap that is not text can be read as some — a solid rectangle
  came back as a mark — which means a PGS track of a shape may yield a spurious
  cue rather than none. The words OCR produces should be treated as approximate.
- **OCR covers PGS only**, and the extraction step is PGS-specific: it copies the
  stream with `-f sup`, which the `sup` muxer accepts only for
  `hdmv_pgs_subtitle`. VobSub (`dvd_subtitle`) and DVB subtitles therefore keep
  the `415` refusal and the burn, even with an engine installed, and no VobSub
  sample exists on this host to change that. `--ocr-language` is passed through
  to tesseract but only `eng` is installed here, and no non-English caption has
  been recognised.
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
- **Packaging is verified unevenly, and the gaps are known.** The container was
  built, started and exercised. The systemd unit was checked with
  `systemd-analyze verify` and `security`, but never *started*: the development
  host has no reachable systemd manager, so the hardening directives
  (`ProtectSystem=strict`, `ReadWritePaths`, the syscall filter) are reasoned
  about rather than observed. **CI runs on GitHub; the release workflow does
  not yet.** Every step of both was run by hand here before the repository was
  public, and CI has since proved itself by failing on the runner's ffmpeg where
  this host passed (see the ladder defect above). No tag has been pushed, so the
  GitHub Release and the GHCR push remain untested. The release build was run end
  to end (archives, checksums, the injected
  version, the labels, a real multi-arch build). The arm64 image needed QEMU for
  the runtime layer's `apt-get`; `tonistiigi/binfmt --install arm64` registered it
  on this host, which is a kernel-level change this repository did not make and
  does not document, because CI's `docker/setup-qemu-action` does the same thing
  from scratch. `deploy/README.md` states all of this in place, so a reader of the
  deployment docs does not have to find this handoff to learn it.
- **The unit's `SystemCallFilter=@system-service` is the one hardening line that
  could break playback** on a host where an encoder needs a call outside the
  list. It is the standard set for a service of this kind, but no encoder here
  exercises every path through it.
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
- **`docs/review/*` name types that no longer exist** (`library.SQLiteRepository`,
  `MetadataProvider`). They are point-in-time records with headers saying so.
