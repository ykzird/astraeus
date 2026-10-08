# Handoff

**As of the round-6 work of 2026-10-08 — OpenTelemetry trace export. Version
0.13.0. 105 tracked files.** (`git log` names the commits; the previous handoff
was `9458674`, which added API rate limiting. Round 4 delivered image subtitles
by burn-in, round 3 per-viewer progress, and round 2 HDR/Dolby Vision, packaging,
the bitrate ceiling, the adaptive ladder, audio track selection, resumable
playback, the continue-watching list and response hardening.)

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
`/home/jok/jellyfin/media/movies/` (a 17 GB 4K HEVC HDR film and a second film).
`real.db` points at it. **Never modify anything under `/home/jok/jellyfin`**, and
remember `*.db` is gitignored, so the library database is local state.

Traps that have cost time:

| Trap | What to do |
| --- | --- |
| `/tmp` is a per-invocation tmpfs | `export GOCACHE=/home/jok/Work/deepseek-harness/.gocache TMPDIR=/home/jok/Work/deepseek-harness/.tmp` before any Go command |
| Go needs `mise exec --` | `mise exec -- go test ./...` |
| `pgrep -f` / `pkill -f` match the wrapper's **own** command line | never `pkill -f` a pattern that appears in the command you are running; it kills your own shell (this happened twice) |
| `go build ./...` does **not** refresh `./astraeus-server` | `mise exec -- go build -o ./astraeus-server ./cmd/astraeus-server` |
| Port 8080 belongs to another service | use 86xx–92xx for tests; the server defaults to 8642 |
| CLI flags are per-command, and the top-level `--help` lists only the commands | run `./astraeus-server <cmd> -h`; `--db` exists on `scan`, `library add` and `enrich`, but not at the top level |
| `scan` has no `--all` flag | use `POST /api/scan` for every library, or `scan --library <id>` for one |
| Stream and cache dirs default to temp | pass `--stream-root` / `--subtitle-cache` / `--image-cache` inside the workspace when testing |
| `web/vendor/hls.min.js` is 620 KB on one line | exclude `web/vendor` (or `*.min.js`) from any search across `web/`, or one grep flushes the result |
| The session transcript echoes each provider response in a `stream` field | a char count over it says ~3x the truth. Take the prompt size from the `usage` block of the last `assistant/message`: `inputTokens + cacheReadTokens` |

---

## 3. How to verify

```sh
# Everything, including the tests that shell out to real ffmpeg.
mise exec -- go test -tags=integration -race -count=1 ./...

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

`player-chrome-verify.mjs` is the broadest single check: 28 assertions covering
the overlay, icons, fullscreen, audio, quality switching, subtitle persistence
and auto-hide, against real 4K content. Last run: **28/28**, and it ran against
the 17 GB film, not the demo clips.

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
# rung's produced segment really is the height it was advertised as.
mise exec -- go test -tags=integration -run TestManager_Ladder -v ./internal/streaming/

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

# Image subtitles in a real browser: the menu offers the image track, choosing it
# re-negotiates with burn_subtitle_index, and the decision comes back with
# burned_subtitle_index. Needs an entity with a PGS track; scripts/pgsgen and the
# harness README build one. What is composited into the picture is the Go
# integration test's job, not this harness's.
CDP_PORT=9413 timeout 200 node scripts/ui-verify/burn-verify.mjs http://127.0.0.1:8924 <imageSubtitleEntityId>

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

---

## 4. Architecture

```
cmd/astraeus-server     flags, wiring, the composition root
internal/library        domain: entities, the Repository port, the scanner
internal/library/naming pure filename/path rules — imports nothing
internal/library/sqlite the SQLite adapter for that port, schema and migrations
internal/metadata       provider interface, TMDB, mock, enrichment worker
internal/streaming      capability negotiation, encoder selection, HLS sessions
internal/subtitles      WebVTT extraction and caching
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
  attribute keys. A CSP does not exist yet, so this discipline is currently the
  only XSS defence — keep it.
- **Docs are part of the change.** `README`, `SPECIFICATION`, `TODO` and the
  package READMEs have all been kept in sync; `TODO.md` marks gaps honestly
  rather than aspirationally.

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
# Measures the real prompt size from the provider's own usage block.
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
print(f"{used:,} of {WINDOW:,} tokens ({100*used/WINDOW:.1f}% used), last measured at {at}")'
EOF
bash .tmp/ctx-check.sh
```

The window is **1,000,000 tokens** for `deepseek-flash`, so the two thresholds
below leave room to finish properly. They are deliberately far below the
harness's own compaction point, because the point is to end by choice:

| Used | Do |
| --- | --- |
| **70%** (700k) | Wrap up: finish the increment in hand, no new work |
| **80%** (800k) | Stop picking up work entirely. Sync `README`, `SPECIFICATION`, `TODO` and this handoff with the state you reached, commit it, and hand over a short summary a fresh session can start from |

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

Verified in a real browser against real 4K content: 28/28 chrome, 13/13 player,
10/10 subtitles. Full Go suite green with race and integration.

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

The **adaptive bitrate ladder** landed as of 0.5.0. The contract is one sentence:
a manifest that pins `max_height` is asking for one rendition, and one that omits
it is asking to adapt. A ladder is up to three rungs (the target height, two
thirds, half) encoded by one ffmpeg process, each with its own VBV ceiling from a
conventional table scaled by the client's limit, and the client is handed
`master.m3u8`. Per-rung settings need stream specifiers (`-c:v:0`, `-filter:v:0`,
`-pix_fmt:0`, `-force_key_frames:0`); without them every option lands on the first
rung and the "ladder" is two copies of one stream — which is exactly what the
first experiment produced, and why the integration test asserts each rung's
*measured* height rather than the argument list. Verified on the real 17 GB film:
a 3-rung ladder at 1920x820 / 1278x546 / 960x410, delivered through the allowlist,
with the browser harness still at **28/28** — including the quality menu, which
pins a height and so exercises the single-rendition path beside it.

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
filter chain). The unit passes `systemd-analyze verify` and scores 1.6 (OK) on
`systemd-analyze security`. See §8 for what that does *not* cover.

---

## 7. Open work

Priority order, with the reasoning. Take it top-down.

1. **UI unit tests.** The front end is one 133 KB file with no seam, and that is
   now the largest untested surface in the project: everything else has unit or
   integration coverage. `web/core.js` for the pure timeline maths is the
   cheapest first cut, and the CDP harnesses already pin the behaviour a unit
   test would protect. CSP, security headers and the artwork leak are done as of
   0.9.0; per-user progress 0.10.0; image subtitles by burn-in 0.11.0; rate
   limiting 0.12.0; trace export 0.13.0.
2. **OCR for image subtitles, if the burn-in cost is unwanted.** A burn is exact
   but needs a re-encode, cannot be toggled without one, and cannot be searched or
   restyled. OCR (tesseract is already installed on the development host) would
   deliver text that survives all three, at the cost of a runtime dependency and
   OCR errors. `internal/testfixtures/pgs` and `scripts/pgsgen` now make the
   fixture side of that work cheap; a PGS parser does not exist yet.
3. **A ladder a client can pin the top of.** Today pinning a height means one
   rendition, so the quality menu caps quality rather than expressing a preference
   within a ladder — the negotiation model is thinner than it looks there. A burn
   deliberately pins one rendition, so the two interact.
4. **Release automation and a TLS example.** Packaging landed (see §6), but
   nothing is tagged or published, the CI workflow has never run, and there is no
   reverse-proxy configuration beside the unit.
5. **Per-user *access*, if it is ever wanted.** Progress is per viewer now, but
   the gate remains instance-wide: it admits a request, it does not decide what
   the request may see, so every admitted viewer sees the whole library.
6. **Dolby Vision profile 5 done properly** (libplacebo with a Vulkan device, or
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
- **The content security policy is verified against the current UI, not against
  a future one.** A new inline `<style>` or a script fetched from elsewhere would
  be refused at runtime and show up as a console error in the harness — which is
  the point of the policy, but it means a front-end change that adds one will
  fail verification rather than merely being flagged.
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
  quarter of that CPU would feel it, and there is no rung cap by host.
- **No browser has been asked to play an HDR stream.** The HDR path is verified
  down to the produced segment (10-bit, `bt2020nc`/`smpte2084`/`bt2020`), not to
  a compositor showing it correctly — which is why the client has to declare
  `supports_hdr` rather than being assumed capable.
- **Packaging is verified unevenly, and the gaps are known.** The container was
  built, started and exercised. The systemd unit was checked with
  `systemd-analyze verify` and `security`, but never *started*: the development
  host has no reachable systemd manager, so the hardening directives
  (`ProtectSystem=strict`, `ReadWritePaths`, the syscall filter) are reasoned
  about rather than observed. The CI workflow has never executed anywhere — the
  repository has no remote — though every step in it was run by hand here.
  `deploy/README.md` states all of this in place, so a reader of the deployment
  docs does not have to find this handoff to learn it.
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
