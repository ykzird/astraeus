# Handoff

**As of commit `428341c`, 2026-10-08. Version 0.2.0. 83 tracked files.**

Written for whoever picks this up next — a person or an agent. The durable parts
(architecture, conventions, environment, how to verify) should stay true for a
long time; the state and open-work sections are the ones to refresh.

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

Two mechanical checks worth re-running after any change to the API surface or the
metrics registry — both are scripted in the review's spirit and catch documentation
drift that eyeballing does not:

- every metric declared in `internal/observability/kpi.go` is named in the README,
  and nothing named there is undeclared;
- every route mounted in `internal/api/server.go` appears in the README's table.

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
  bug.** This has happened four times now — `EncoderFor`'s "nothing available"
  case expected a software encoder that was never declared, the sweep fixture
  predated the marker file, `compiledButUnusable` put a rejected encoder in the
  verified list, and the pixel-format test expected VAAPI to take `-pix_fmt`.
  Check which is wrong before "fixing" the code.
- **Regression tests must fail before the fix.** The auto-hide check was run
  against the unfixed build to prove it caught the reported bug.

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

Verified in a real browser against real 4K content: 28/28 chrome, 13/13 player,
10/10 subtitles. Full Go suite green with race and integration.

---

## 7. Open work

Priority order, with the reasoning:

1. **HDR and Dolby Vision.** Deliberately deferred — the owner has no HDR monitor
   and would have to test on a TV. Only bit depth is probed, output is pinned to
   8-bit, so a 10-bit BT.2020/PQ source is converted to SDR naively and looks
   washed out or dark. This is *visibly wrong output*, not a missing feature, which
   is why it leads the list. Both Jellyfin and Plex still have open bugs here.
2. **Packaging.** No `Dockerfile`, systemd unit or CI. `LICENSE` and the notices
   exist. Nothing reaches anyone without this.
3. **A real ABR ladder**, and making `max_bitrate_kbps` do something. The field is
   currently accepted, validated and echoed but never acted on, which is worse
   than not having it.
4. **Multi-audio-track selection** (the first stream wins today) and **image
   subtitles** (PGS/VobSub are detected, reported, and refused).
5. **Resume / watch state.** The hard part already works: a session can start at
   an offset, so this is mostly persistence plus a report endpoint.
6. **CSP and security headers**, **UI unit tests** (the front end is one 133 KB
   file with no seam — `web/core.js` for the pure timeline maths is the cheapest
   first cut), **artwork IP leak** (metadata-supplied absolute URLs are fetched by
   the browser directly), rate limiting, OpenTelemetry.

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
- **HDR**, as above.
- **Firefox is not installed**, so the hls.js path has only been verified in
  Chromium, and the native-HLS (Safari) branch has never been observed firing.
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
- **`docs/review/*` name types that no longer exist** (`library.SQLiteRepository`,
  `MetadataProvider`). They are point-in-time records with headers saying so.
