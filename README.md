# Astraeus Media

A media-first "spatial" media environment: a Go server that scans a library of
movies and shows, attaches metadata, and negotiates with each client how to
deliver a file — direct play when the client can handle it, repackaging or
re-encoding when it cannot.

The project brief is in [`SPECIFICATION.md`](SPECIFICATION.md) and the domain
vocabulary in [`CONTEXT.md`](CONTEXT.md).

## Status

The **Library Manager**, the **Media Engine**, artwork, subtitles and the KPI
registry all work today and are covered by tests. The **Spatial Web UI** is
served by the binary and plays both direct and segmented streams. Concretely:

| Area | State |
| --- | --- |
| Filesystem scanning | Done. Movies and shows; idempotent re-scans; **periodic background scanning** with a manual override; files removed from disk are pruned, unless the scan could not see the whole library |
| Series / Season / Episode hierarchy | Done, derived from the directory layout |
| Persistence | Done. SQLite via `sqlx`, versioned in-code migrations |
| Metadata providers | TMDB (real HTTP) and a synthetic fallback; chainable |
| Background enrichment | Done. Entities stay `Incomplete` until enriched |
| Artwork | Done. Posters and backdrops proxied from TMDB and cached locally |
| REST API | Done. Libraries, entities, scanning, enrichment, playback, artwork, subtitles |
| Capability negotiation | Done. Direct play / remux / transcode, with reasons |
| Segmented streaming | Done. HLS via ffmpeg, passthrough or transcode |
| Hardware acceleration | Verified at startup (QuickSync / VAAPI); software fallback |
| Subtitles | Done. Text tracks extracted to WebVTT, cached and served |
| Observability | Done. The KPI registry is exposed in Prometheus format at `/metrics` |
| Web UI | Three-column spatial layout, served by the binary; HLS via a vendored hls.js; player controls overlaid on the video (transport, seek, subtitles, volume, quality, fullscreen) |
| Authentication | Optional gate: trusted-proxy identity (Tailscale / Cloudflare Access) or a bearer token |

### Playback compatibility

`direct_play` serves the original file with range support, which any browser can
play when the codecs are ones it supports (typically mp4/H.264/AAC).

`remux` and `transcode` produce **HLS**. Safari demuxes HLS natively; Chromium
and Firefox do not, so the UI loads the vendored **hls.js** (MSE) for those
browsers. See [`web/vendor/README.md`](web/vendor/README.md) for the pinned
version, licence and provenance, and
[`THIRD_PARTY_NOTICES.md`](THIRD_PARTY_NOTICES.md) for the licence notices that
must travel with any redistribution. No shipped page depends on a third-party origin
at runtime.

Seek behaviour on a segmented stream is not limited to what has been produced:
the player's seek bar spans the whole film, and a seek beyond the produced
window re-negotiates the session at that offset rather than clamping. The
landing point is keyframe-aligned, so a resumed stream can begin a second or two
earlier than requested.

## Requirements

- Go 1.26 or newer (the module declares `go 1.26.3`)
- `ffmpeg` and `ffprobe` for playback negotiation and segmented streaming.
  Without them the library still works; playback endpoints report the feature
  as unavailable rather than failing obscurely.
- No database server: SQLite is embedded.
- No network access is needed at runtime. hls.js is vendored under
  `web/vendor/`, and artwork is fetched from the metadata provider only when a
  `MetadataSet` references it.

## Quick start

Nothing to play with yet? `scripts/make-demo-media.sh` generates a small
library - a movie, a three-episode show, and an episode carrying a real
subtitle track - registers it in `demo.db` and scans it:

```sh
./scripts/make-demo-media.sh
./astraeus-server serve --db demo.db --web-dir web
```

Otherwise, build and point it at your own media:

```sh
# Build
go build -o astraeus-server ./cmd/astraeus-server

# Register and scan a library (the library is created if it is new)
./astraeus-server scan --db astraeus.db --path /media/movies --kind movies
./astraeus-server scan --db astraeus.db --path /media/shows  --kind shows

# Attach metadata. Without an API key this writes clearly-marked synthetic
# metadata so the rest of the pipeline can be exercised.
./astraeus-server enrich --db astraeus.db

# Serve (defaults to :8642)
./astraeus-server serve --db astraeus.db
```

The UI is served from the same origin as the API, from the `web` directory
(`--web-dir`, set it to an empty string to serve the API only).

Set `TMDB_API_KEY` (or pass `--tmdb-key`) to use real metadata. With a key, a
title TMDB cannot find stays `Incomplete` so it shows up as work for an
administrator, rather than being quietly filled in with a placeholder.

The database is SQLite, opened with `busy_timeout(5000)`, `journal_mode(WAL)`,
`synchronous(NORMAL)` and `foreign_keys(1)`. WAL means recent writes live in
`<db>-wal` (with a `<db>-shm` index) until SQLite checkpoints them into the main
file, so moving or backing up a library must copy all three files together — or
stop the server first, which checkpoints on a clean shutdown. Copying `<db>`
alone loses recent writes. Foreign keys are genuinely enforced, so an insert
that would dangle is refused rather than stored silently.

`--stream-root` controls where HLS session output is written (default:
`$TMPDIR/astraeus-streams`) and `--max-sessions` (default `8`) caps how many
segmented streams may run at once — each is an ffmpeg process, so the cap is
what stops a loop of playback requests from forking the host to death.
Requesting one over the limit answers `429 too_many_sessions`. Session
directories carry a `.astraeus-session` marker, and the reaper removes only
marked directories, so a shared or mistyped `--stream-root` cannot lose data.
`--device-dir` (default `/dev/dri`) is where hardware transcoding devices are
looked for.

### Hardware acceleration

A hardware encoder is used only after it has **proved it can encode**: at
startup each candidate is run against a fraction of a second of test video, and
one that cannot open a session is dropped from the reported capabilities. This
matters because neither signal alone is trustworthy. `ffmpeg -encoders` lists
what was *compiled in*, and a populated `/dev/dri` says nothing about the GPU
vendor — an AMD machine exposes a device directory exactly as an Intel one does,
so QuickSync can look available on a host where it can never work.

If a hardware encoder is chosen and still fails at runtime, the session is
retried once in software and `astraeus_transcode_fallbacks_total` counts it,
rather than failing the request when a working software path exists.

`GET /api/system/capabilities` reports what survived that check, including the
`video_encoders` and `audio_encoders` this host can actually offer.

### Scanning

A scan is idempotent: re-running it reuses entities rather than duplicating
them, and it reports distinct entities rather than lookups. It also **prunes**:
a file that is gone stops appearing, and any entity left holding neither an
object nor a child is removed with it, working upwards so an emptied season
takes its series with it.

Pruning deletes data, so it refuses to run whenever the scan's view of the disk
might be incomplete — if any path was unreadable, or if the scan found zero
files while the library still holds entities, which is the shape of a drive
that is not mounted. Silently emptying a library is far worse than leaving a
ghost entry behind. The returned `ScanResult` reports what was removed as
`objects_pruned` and `entities_pruned`. `--scan-interval` (default `6h`,
`0` disables) re-scans every library for new files; `--enrich-interval` does the
same for metadata. `--image-cache` and `--subtitle-cache` place the artwork and
WebVTT caches.

## CLI

```
astraeus-server serve [flags]                  start the HTTP API
astraeus-server scan --path DIR --kind KIND    scan a directory
astraeus-server scan --library ID              re-scan a registered library
astraeus-server library add --name N --path D --kind KIND
astraeus-server library list [--json]
astraeus-server library rm ID
astraeus-server enrich                         run one metadata pass
astraeus-server version
```

Run any command with `-h` for its flags. Shared flags: `--db`, `--tmdb-key`,
`--log-level`, `--log-format`. Flags are per-command: there is no global `--db`,
so it has to follow the subcommand. `astraeus-server version` prints the build
identifier (currently `0.2.0`).

## HTTP API

| Method | Path | Purpose |
| --- | --- | --- |
| GET | `/api/health` | Liveness |
| GET | `/api/system/capabilities` | ffmpeg/ffprobe presence, video and audio encoders, hardware path |
| GET | `/api/libraries` | List libraries |
| POST | `/api/libraries` | Register `{name, path, kind}` |
| GET | `/api/libraries/{id}` | One library |
| DELETE | `/api/libraries/{id}` | Remove a library and its entities |
| POST | `/api/libraries/{id}/scan` | Scan, returns a `ScanResult` |
| POST | `/api/scan` | Re-scan every library, returns a `ScanOutcome` each |
| DELETE | `/api/streams/{id}` | Stop a streaming session now, `204` |
| GET | `/api/libraries/{id}/entities` | Entities, optionally `?status=Incomplete` |
| GET | `/api/entities` | All entities, optionally `?status=` |
| GET | `/api/entities/{id}` | Entity with its objects, children and parent |
| POST | `/api/metadata/enrich` | Run one enrichment pass |
| POST | `/api/entities/{id}/playback` | Negotiate playback, returns a URL and subtitle tracks |
| GET | `/api/objects/{id}/file` | The original file (range requests supported) |
| GET | `/api/objects/{id}/subtitles/{track}.vtt` | One subtitle track as WebVTT |
| GET | `/api/images/{size}/{file}` | Poster/backdrop artwork, proxied and cached |
| GET | `/metrics` | Prometheus metrics |
| GET | `/hls/{session}/{file}` | Playlist and segments of a live session |

Entity payloads also carry `poster_url` and `backdrop_url` pointing at the local
image proxy, so a client never has to know the metadata provider's URL scheme.

`size` must be one of the allow-listed TMDB renditions (`w45`, `w92`, `w154`,
`w185`, `w300`, `w342`, `w500`, `w780`, `w1280`, `h632`, `original`), and
`file` must be a bare image basename; anything else is rejected before it can
reach either the cache or the upstream origin.

### Metrics

`/metrics` exposes the specification's KPI registry in Prometheus text format.
The whole registry is declared at startup, so an idle server still reports every
metric and absence-based alerting works.

| Metric | Meaning |
| --- | --- |
| `astraeus_first_segment_seconds` | `fttt_latency`: stream preparation to the first segment delivered |
| `astraeus_transcode_startup_seconds` | `transcode_startup_time`: ffmpeg start to playlist available |
| `astraeus_metadata_lookup_seconds` | `metadata_latency`, labelled by provider and outcome |
| `astraeus_stream_errors_total` ÷ `astraeus_stream_sessions_total` | `stream_error_rate` |
| `astraeus_playback_decisions_total` | Negotiations by chosen mode |
| `astraeus_scan_seconds`, `astraeus_scan_files_total`, `astraeus_scan_runs_total` | Scanning |
| `astraeus_http_requests_total`, `astraeus_http_request_seconds` | HTTP |
| `astraeus_stream_sessions_active`, `astraeus_probe_errors_total` | Operational |
| `astraeus_transcode_fallbacks_total` | Hardware transcodes retried in software |
| `astraeus_auth_granted_total`, `astraeus_auth_denied_total{reason}` | Access gate grants and denials |

Errors are `{"code": "...", "message": "..."}` with a matching status code.

### Negotiation

`POST /api/entities/{id}/playback` takes an optional `ClientCapability` body:

```json
{
  "containers": ["mp4", "webm", "hls"],
  "video_codecs": ["h264", "vp9"],
  "audio_codecs": ["aac", "opus"],
  "max_width": 1920,
  "max_height": 1080,
  "max_bitrate_kbps": 120000,
  "max_bit_depth": 8,
  "max_audio_channels": 2,
  "supports_hls": true,
  "subtitles": true
}
```

The body may also carry `start_seconds`, the offset into the source at which the
session should begin; the response echoes it back as `start_seconds` so a client
can keep a continuous timeline across a seek or a quality change. A negative
value, or one at or past the end of the media, is rejected with `400`.

Omit the body to use the built-in browser profile, which caps at **1920x1080,
8-bit, stereo**. Those three limits are not arbitrary: they are the ones a
browser cannot be relied on to exceed. A 4K source would otherwise be
re-encoded at 4K (about four times the CPU) for a client that usually cannot
decode it; 10-bit H.264 ("High 10") and 5.1 AAC are refused outright by
Chromium's media pipeline, and a refused audio append tears the whole
MediaSource down, so the player attaches, fetches every segment, and never
shows a frame. A client that genuinely wants more can ask for it in its
manifest. The response carries the
decision and the reason for every choice:

```json
{
  "mode": "remux",
  "url": "/hls/5d9866ee-.../playlist.m3u8",
  "session_id": "5d9866ee-...",
  "decision": {
    "mode": "remux",
    "deliverable": true,
    "container": "hls",
    "video_action": "copy",
    "audio_action": "copy",
    "reasons": [
      "client cannot open container \"matroska\"",
      "remux: the streams are compatible but the container is not, so they are copied into HLS unchanged"
    ]
  }
}
```

Modes:

- **`direct_play`** — the client handles the source container and codecs, so the
  original file is served as-is and the client does the buffering.
- **`remux`** — streams are compatible but the container is not; they are copied
  into HLS unchanged, so quality is bit-identical.
- **`transcode`** — at least one stream is incompatible, or the source is larger
  than the client accepts; only the offending streams are re-encoded.

A `409` means the client's declaration makes delivery impossible (for example it
needs re-encoding but cannot play HLS); the reasons explain why.

The decision also carries the concrete targets it chose — `target_height` for a
downscale and `target_audio_channels` for a downmix — so a client can see not
just that it will be re-encoded but what it will get.

A manifest naming a codec that does not exist is refused with `400` before it
can reach ffmpeg. A codec that exists but that this host cannot encode is a
negotiation outcome, not a bad request: the server picks another codec the
client accepts if there is one, and otherwise answers `409` saying so.

An omitted limit means **unrestricted**, so a client that says nothing about
channels gets the source's own channel count. That is the right default for a
native player, which may well want the 5.1 mix, but it is a trap for a browser:
Chromium refuses a 5.1 AAC `SourceBuffer`, and because the audio append fails
first it tears down the whole `MediaSource`, so the player attaches, fetches
every segment and never shows a frame. A browser client should declare
`max_audio_channels: 2` and `max_bit_depth: 8` — the built-in profile already
does.

## Access gate

Per the specification, authentication is delegated to an identity-aware proxy
rather than a built-in user database. `--auth-mode` selects the policy:

| Mode | Behaviour |
| --- | --- |
| `none` (default) | No gate. Correct for a trusted LAN; **never** expose this to the internet |
| `proxy` | Believe an identity header — but only when the request arrives from a configured trusted address |
| `token` | Require `Authorization: Bearer <token>`, compared in constant time |

```sh
# Behind Tailscale (tailscale serve sets Tailscale-User-Login) or Cloudflare Access
./astraeus-server serve --auth-mode proxy --trusted-proxy 100.64.0.0/10,127.0.0.1/32

# For API clients and scripts
ASTRAEUS_AUTH_TOKEN=$(openssl rand -hex 32) ./astraeus-server serve --auth-mode token
```

The security property that matters: **an identity header is only believed from a
trusted address.** Headers are trivially forgeable by anyone who can reach the
port, so trusting them without that check would be worse than no gate at all.
`X-Forwarded-For` is deliberately ignored for the same reason — the address the
connection actually came from is the only trustworthy one. Configuration fails
closed: `proxy` mode without `--trusted-proxy`, or `token` mode without a token,
refuses to start.

`--auth-exempt` (default `/api/health`) lists exact paths that bypass the gate,
so liveness probes keep working. `/metrics` is **not** exempt: point Prometheus
at it with a token or let it through the proxy.

Identity is recorded on every request log line, so the gate is auditable, and
`astraeus_auth_granted_total` / `astraeus_auth_denied_total{reason}` show what
the gate is doing.

A browser cannot attach a bearer token to a plain navigation, so browser access
belongs behind `proxy` mode; `token` mode suits clients and automation.

## Testing

```sh
go test ./...                            # unit tests
go test -race ./...                      # with the race detector
go test -tags=integration ./...          # also runs real ffmpeg/ffprobe
```

The integration tests generate their own clips, run ffprobe against them, and
drive a real HLS session end to end. They skip themselves when ffmpeg is absent.

Playback and subtitle behaviour in a browser is covered separately by the CDP
harnesses in [`scripts/ui-verify/`](scripts/ui-verify/), which assert what the
`<video>` element actually does rather than what the server intended. Both the
repackaged (`remux`) and re-encoded (`transcode`) HLS paths have been observed
playing in Chromium through those harnesses.

## Layout

```
cmd/astraeus-server/    CLI entry point and wiring
internal/library/       domain model, repository port, scanner
internal/library/naming/  pure filename and path rules (no dependencies)
internal/library/sqlite/  the SQLite adapter for that port
internal/metadata/      provider interface, TMDB client, mock, enrichment worker
internal/streaming/     capability negotiation, probing, HLS session manager
internal/subtitles/     WebVTT extraction and caching
internal/images/        artwork proxy and cache
internal/observability/ KPI registry and Prometheus exposition
internal/access/        the access gate
internal/api/           HTTP layer
web/                    the Spatial Web UI, including vendored hls.js
                        and the generated BoxIcons registry (web/icons.js)
scripts/ui-verify/      browser harnesses for playback and subtitles
scripts/make-demo-media.sh  generates a throwaway demo library
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

## Licence

Astraeus Media is released under the [MIT Licence](LICENSE). Vendored third-party
components and their notices are listed in
[`THIRD_PARTY_NOTICES.md`](THIRD_PARTY_NOTICES.md).

An adversarial review of the codebase, its known gaps and the competitive
landscape lives in [`docs/adversarial-review.md`](docs/adversarial-review.md),
with the two detailed source reports alongside it in `docs/review/`.
