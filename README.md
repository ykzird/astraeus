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
| Resume / watch state | Done. A viewer's position is stored per viewer and entity, reported while watching, and resumed when Play is pressed again; watched-through films forget theirs |
| Audio tracks | Done. Every track is probed and listed; a client picks one by stream index, and the file's own default is delivered when it does not |
| Adaptive bitrate | Done. A manifest that asks for a `preferred_height` gets a master playlist with up to three rungs topped there; `max_height` alone pins one rendition; omitting both adapts to the client's own ceiling |
| HDR and Dolby Vision | Detected from the source's colour tags; **tone mapped to SDR** for clients that cannot show it, and passed through at 10 bits for those that can. Dolby Vision profile 8 keeps its HDR10 base layer; profile 5 is flagged as approximate |
| Hardware acceleration | NVENC, QuickSync, VideoToolbox, VAAPI and AMF, each **verified by running it with the real options** at startup; rejected encoders report why; software fallback |
| Subtitles | Done. Text tracks extracted to WebVTT, cached and served; **PGS image tracks read into text by OCR** when tesseract is installed (toggleable and searchable), and burned into the picture otherwise; VobSub stays burn-only |
| Observability | Done. The KPI registry is exposed in Prometheus format at `/metrics`, and traces can be exported over OTLP/HTTP (`--otel-endpoint`, opt-in) with `trace_id` on the request log line |
| Web UI | Three-column spatial layout, served by the binary; HLS via a vendored hls.js; player controls overlaid on the video (transport, seek, subtitles, volume, quality, fullscreen) |
| Authentication | Optional gate: trusted-proxy identity (Tailscale / Cloudflare Access) or a bearer token |
| Rate limiting | Optional token bucket per client in front of `/api/`, off by default (`--rate-limit`); keyed by the gate's identity when there is one, otherwise the peer address |
| Security headers | A content security policy with no `unsafe-inline` and no `unsafe-eval`, plus nosniff, a referrer policy, frame denial and a permissions policy. Verified in a browser: a third-party image and an inline script are both refused |
| Packaging | A multi-stage `Dockerfile` (ffmpeg included, non-root, health check) and a hardened systemd unit, both verified as far as this host allows — see [`deploy/README.md`](deploy/README.md) |

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

A `go build` from a working tree reports its version as `dev`, because that is
what it is. Tagged releases build with the version baked in:

```sh
# Published archives: binary, the web UI it serves, and the licence notices.
tar -xzf astraeus-server_0.17.0_linux_amd64.tar.gz
cd astraeus-server_0.17.0_linux_amd64
sha256sum -c ../checksums.txt     # from the same release
./astraeus-server version         # astraeus-server 0.17.0
./astraeus-server serve --db astraeus.db

# Or the image, which includes ffmpeg and tesseract.
docker pull ghcr.io/ykzird/astraeus:0.17.0
```

A release is cut by pushing a tag; `scripts/build-release.sh` is the whole build
and can be run by hand, and [`deploy/README.md`](deploy/README.md) is the
runbook.

The UI is served from the same origin as the API, from the `web` directory
(`--web-dir`, set it to an empty string to serve the API only).

Set `TMDB_API_KEY` (or pass `--tmdb-key`) to use real metadata. With a key, a
title TMDB cannot find stays `Incomplete` so it shows up as work for an
administrator, rather than being quietly filled in with a placeholder.

To run it as a service rather than from a shell, see
[`deploy/README.md`](deploy/README.md): a container image that includes ffmpeg,
and a hardened systemd unit. Both set every writable path explicitly and turn the
access gate on before publishing a port, because `--auth-mode` defaults to
`none`.

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
`--device-dir` (default `/dev/dri`) is where a VAAPI render node is looked for.
It is the only family that needs one: NVENC, QuickSync, VideoToolbox and AMF find
their own devices, and each is verified by running it regardless (see below).

### Hardware acceleration

Five families are supported, preferred in this order: **NVENC** (NVIDIA),
**QuickSync** (Intel), **VideoToolbox** (Apple), **VAAPI** (AMD and Intel on
Linux) and **AMF** (AMD). A machine with more than one — an NVIDIA card beside an
Intel iGPU — reports all of them and uses the first that works.

A hardware encoder is used only after it has **proved it can encode**: at startup
each candidate is run against a fraction of a second of test video, with *the
same options a real session would use*, and one that cannot open a session is
dropped from the reported capabilities. Verifying with the real flags is the part
that matters. An encoder can be compiled in, be listed, and still reject the
options it is given, and a probe that tested something else would pass while
playback failed.

Neither signal on its own is trustworthy. `ffmpeg -encoders` lists what was
*compiled in* — this project's own development machine lists NVENC, AMF,
QuickSync and VAAPI and can use none of them. A populated `/dev/dri` says nothing
about the GPU vendor either, since an AMD machine exposes a device directory
exactly as an Intel one does.

**When hardware transcoding does not happen**, the reason is reported rather than
hidden. Startup logs a line per rejected encoder with ffmpeg's own complaint, and
`GET /api/system/capabilities` carries the same thing:

```console
$ ./astraeus-server serve --db astraeus.db
WARN hardware encoder rejected encoder=h264_nvenc reason="exit status 255: Cannot load libcuda.so.1"
WARN hardware encoder rejected encoder=h264_qsv reason="exit status 171: Error creating a MFX session: -9."
WARN no hardware encoder is usable on this host; transcoding will use the CPU rejected=12
```

That is the difference between a fixable driver problem and a mystery. A machine
with an NVIDIA card and no driver does not look like a machine with no GPU.

VAAPI additionally needs a DRM render node; one is located at startup and passed
to both the probe and every session, before the input, because `hwupload` has no
device to upload to without it.

If a hardware encoder is chosen and still fails at runtime, the session is
retried once in software and `astraeus_transcode_fallbacks_total` counts it,
rather than failing the request when a working software path exists.

`GET /api/system/capabilities` reports what survived that check, including the
`video_encoders` and `audio_encoders` this host can actually offer, and
`hdr_video_encoders`, which lists the encoders that also produced a **10-bit**
stream — each with the pixel format it accepted. `subtitle_ocr_enabled` reports
whether image subtitles can be read into text on this host (see
[Image subtitles](#image-subtitles)).

### HDR and dynamic range

A high dynamic range source is not merely a brighter one. PQ (SMPTE ST 2084) and
HLG code values describe light through a different transfer function, so SDR
output has to convert them; ffmpeg's default behaviour is to reinterpret the
values and tag the result as the source was tagged. The picture then reaches the
player looking washed out — milky blacks, flat contrast — **and the stream
claims to be HDR while containing 8-bit data**, which is what makes it wrong
rather than merely different.

Dynamic range is decided from the source's **transfer function**, not its bit
depth or its primaries. A 10-bit BT.2020 SDR master is stored exactly like an SDR
one and is never tone mapped; guessing HDR from a wide-gamut tag would reduce the
contrast of a correct picture, which is worse than leaving a mis-tagged file
alone.

| Source | Client | Result |
| --- | --- | --- |
| SDR | any | Unchanged. No tone mapping, whatever the bit depth |
| HDR | declares `supports_hdr` | **Kept**: direct play, remux, or a 10-bit re-encode, tagged BT.2020 with the source's PQ or HLG transfer. Dolby Vision's dynamic metadata is not carried through a re-encode and the reasons say so |
| HDR | does not | **Tone mapped to SDR**: 8-bit BT.709, tagged `bt709`/`bt709`/`bt709` |

Tone mapping is the documented software chain — `zscale` to linear light, the
`hable` tone curve, `zscale` back to BT.709 — because it needs no GPU. The
libplacebo filter tone maps better and understands BT.2390 and Dolby Vision, but
it requires a working Vulkan device and fails the whole transcode where one is
missing, so it is deliberately not used.

Keeping HDR is a *separately proved* capability. A video encoder that works at
8 bits can still refuse 10, so at startup each encoder is probed a second time
with a 10-bit pixel format and a real session's options; `hdr_video_encoders` is
that list. If a client asks for HDR and no encoder here proved 10-bit — or the
only one that did has since failed — the stream is tone mapped to SDR with the
reason attached, because failing a request that a working software encoder could
have served is the wrong answer.

Two caveats are reported rather than hidden. **Dolby Vision profile 5** stores
IPTPQc2, not PQ, so tone mapping it without a Dolby Vision converter produces
approximate colour, and the reasons say exactly that. Re-encoding a **profile 8**
stream keeps its HDR10 base layer but not the dynamic metadata. Mastering-display
and content-light metadata are not carried through a re-encode either.

Detection reads ffprobe's `color_transfer`, `color_primaries`, `color_space` and
the stream's `DOVI configuration record`; all of it appears in the playback
response's `media_info` as `dynamic_range`, `dolby_vision_profile` and
`dolby_vision_base_layer_hdr10`.

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
identifier: a released binary prints its tag, and one built from a working tree
prints `dev`.

`serve` flags that are easy to miss because they are named in the sections below
rather than here:

| Flag | Default | Purpose |
| --- | --- | --- |
| `--addr` | `127.0.0.1:8642` | Listen address |
| `--web-dir` | `web` | Static UI directory |
| `--ffmpeg`, `--ffprobe` | `ffmpeg`, `ffprobe` | Binaries to run (both are hard dependencies) |
| `--stream-root` | temp | HLS session directories |
| `--segment-seconds` | 6 | HLS target segment duration |
| `--max-sessions` | 8 | Concurrent segmented streams |
| `--image-cache`, `--subtitle-cache` | temp | Artwork and WebVTT caches |
| `--tesseract-bin`, `--ocr-language` | `tesseract`, tesseract's own | OCR of image subtitles; a missing binary leaves them burn-only |
| `--tmdb-image-base` | TMDB's own root | Upstream artwork root |
| `--enrich-interval`, `--scan-interval` | — | Background passes; `0` disables |
| `--auth-mode`, `--auth-header`, `--trusted-proxy`, `--auth-token`, `--auth-exempt` | see [Access gate](#access-gate) | Gate configuration |
| `--rate-limit`, `--rate-limit-burst` | `0` (off) | API limit |
| `--otel-endpoint`, `--otel-service-name` | off | Trace export |

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
| GET | `/api/progress` | This viewer's resumable positions, most recently watched first |
| PUT | `/api/entities/{id}/progress` | Record where the viewer got to (resumable playback) |
| DELETE | `/api/entities/{id}/progress` | Forget the caller's position, which is what starting over means |
| GET | `/api/objects/{id}/file` | The original file (range requests supported) |
| GET | `/api/objects/{id}/subtitles/{track}.vtt` | One subtitle track as WebVTT |
| GET | `/api/images/{size}/{file}` | Poster/backdrop artwork, proxied and cached |
| GET | `/metrics` | Prometheus metrics |
| GET | `/hls/{session}/{file}` | Playlist and segments of a live session |

### Resume and watch state

Where a viewer got to is stored per viewer and entity and reported by `PUT
/api/entities/{id}/progress` with `{"position_seconds": 754.5,
"duration_seconds": 7025}`; `GET /api/entities/{id}` then carries a `progress`
object with the same numbers plus `percent` and `finished`. `DELETE` forgets the
caller's position.

Three rules keep it honest rather than merely present:

- A position in the last 5% of the media is **finished**, and finishing *clears*
  the position instead of storing it. Resuming three seconds from the end is
  worse than starting the next thing.
- A position beyond the end of the media is a `400` with the arithmetic in the
  message. A player reporting its final frame a moment after the end is fine;
  numbers that are simply wrong are not stored for the next resume to trust.
- Progress belongs to the **viewer the request authenticated as**, so two people
  watching the same film each keep their own place. The row cascades when the
  entity is pruned, so a deleted film cannot leave a bookmark behind.

The viewer is the identity the access gate verified and put on the request. With
the gate disabled there is a single viewer, named `local`; a client cannot claim
someone else's timeline because no handler ever reads an identity header, only
the context the gate fills. In `token` mode the gate reports every bearer-token
client as `token`, so API clients share one place — per-viewer state needs
`proxy` mode. A database written before this behaviour keeps its rows under
`local`, because the identity that wrote them was never recorded.

The player resumes on Play and reports while watching. It reports a position
under five seconds as a *clear* rather than a store, so a viewer who sampled ten
seconds of something does not get offered a resume at the beginning.

`GET /api/progress` is what makes a stored position findable: it lists one
viewer's positions worth resuming, most recently watched first, with each entity
attached so a client can render a row without a second request. Watched-through
entries are excluded by the query rather than filtered afterwards — a finished
row consuming one of the limit's slots is how a listing of one comes back empty.
The player's sidebar renders it as a **Continue watching** section, hidden
entirely when there is nothing to continue, and refreshes when progress changes.

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
| `astraeus_rate_limited_total` | API requests refused by the rate limiter |
| `astraeus_spans_dropped_total` | Trace spans dropped because the export queue was full |

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
  "preferred_height": 0,
  "max_bitrate_kbps": 120000,
  "max_bit_depth": 8,
  "max_audio_channels": 2,
  "supports_hdr": false,
  "audio_track_index": 0,
  "burn_subtitle_index": 0,
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
  than the client accepts, or its bitrate is above what the client will take; only
  the offending streams are re-encoded. This is also where an HDR source is tone
  mapped for a client that cannot show HDR.

A `409` means the client's declaration makes delivery impossible (for example it
needs re-encoding but cannot play HLS, or its bitrate limit leaves nothing for
video after the audio); the reasons explain why.

The decision also carries the concrete targets it chose — `target_height` for a
downscale, `target_audio_channels` for a downmix, `target_dynamic_range` for
the delivered video's dynamic range, `target_bitrate_kbps` for a bitrate ceiling,
`target_audio_stream_index` for the audio track that was actually mapped, and
`renditions` for a ladder — so a client can see not just that it will be
re-encoded but what it will get. A `tone_map` flag says the picture was converted from HDR to SDR, which is a
visible change rather than a quality trade-off.

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

`supports_hdr` is the one field that is **not** inferred from anything else, and
it is off in the built-in profile. Nothing about an arbitrary browser proves it
can render HDR, so assuming it would hand a PQ stream to a compositor that shows
it washed out. A client that really can — a television app, a browser on an HDR
display reporting through its own manifest — says so. Declaring it alongside
`max_bit_depth: 8` is a contradiction and is refused with `400`: HDR is stored at
ten bits or more.

`max_bitrate_kbps` is a limit on the **whole** stream, and it is acted on rather
than echoed. The audio's share is reserved first — the source's own rate when the
audio is being copied and the container states it, otherwise the 192 kbps this
server encodes audio at — and the video is held to the remainder with a VBV
ceiling, which every encoder family honours while keeping its own quality
settings. So a source the client can decode but cannot afford is re-encoded
rather than copied, a source that already fits is left alone, and an unknown
source bitrate is not assumed to exceed the limit (the reasons say the limit could
not be checked). A limit under 100 kbps is refused as malformed, and one that
leaves nothing for video after the audio is a `409` with the arithmetic in the
reason.

### Audio tracks

A media file can carry several audio tracks, and the server lists all of them in
`media_info.audio_tracks` — `index` (the ffmpeg stream index, which is what a
client sends back), `codec`, `channels`, `bitrate_kbps`, `language`, `title` and
`default`. `audio_track_index` in the request chooses one; **omitting it or
sending 0 means the server's choice**, which is the track the file marks
`default`, falling back to the first. That distinction matters: a file whose
second track is the one marked default is common, and "the first stream wins"
delivers the wrong language on a file that says which one it means.

The choice is honoured in what arrives, not just in what is reported. Every
audio decision — codec compatibility, channel count, the bitrate the audio
reserves — is made about the *chosen* track, and a session maps it by its stream
index. **A chosen track cannot come from direct play**, because direct play serves
the original file and the player then picks a track itself; so choosing one
repackages into HLS with the picture copied and no re-encoding, which is cheap.
When the chosen track is one the client cannot decode, it is re-encoded and
downmixed as usual.

An `audio_track_index` naming a track the file does not have is a `400` listing
the indices that do exist, because the client already had that list. A player can
therefore offer the choice without probing anything itself.

### Image subtitles

Text tracks are served to the browser as WebVTT. Image-based tracks (PGS, VobSub)
carry pictures rather than text, so no browser can render one as a subtitle
track. There are two ways to show one, and the server chooses the better one it
can actually do.

**OCR reads PGS into text.** When `tesseract` is installed (that is, when
`subtitle_ocr_enabled` is true), an image track is advertised with a URL like a
text track and served as WebVTT: the subtitle stream is demuxed with ffmpeg, the
PGS bitmaps are decoded by `internal/subtitles`, each cue's picture is turned
into dark glyphs on a white page, and tesseract returns the words. The result is
an ordinary `<track>` — the viewer can toggle it, restyle it and search it, and
switching it on costs nothing but a fetch. That is the whole point: a burn can do
none of those things.

The runtime dependency is treated as optional, not assumed. Without tesseract the
server does not fail: it logs that image subtitles will be burned in, withholds
the URL, and answers `GET /api/objects/{id}/subtitles/{track}.vtt` with the same
`415 subtitle_format_unsupported` it always did. `--tesseract-bin` names the
executable and `--ocr-language` passes a language (unset means tesseract's own
default, which is `eng` when only the base package is installed).

**A burn is the fallback.** When there is no OCR engine, or the track is a codec
the reader does not decode, `burn_subtitle_index` in the playback request names
the ffmpeg stream index to composite into the video (`media_info.subtitles`
reports each track, with `text: false` marking the image ones). The decision then
carries `burned_subtitle_index`, forces a transcode and pins **one** rendition:
the bitmap is composited once in a single filter graph, so a ladder would mean
burning only one rung. The composite scales the subtitle to the picture with
`scale2ref`, so a downscaled re-encode places and sizes it correctly, and it is
applied *after* the plan's own filters — which is what makes it right for a tone
map, since a subtitle bitmap is SDR white and must be laid over the finished SDR
picture rather than converted with it. The subtitle is decoded from a second
opening of the input, because asking one input for both the video and the
subtitle stream in the same graph does not deliver subtitle frames.

What OCR covers, honestly:

- **PGS only.** The reader is an HDMV PGS decoder. VobSub (`dvd_subtitle`) and
  DVB subtitles are different containers with different palettes; they keep the
  `415` refusal and are offered as a burn, rather than being advertised and then
  failing inside the extractor. The message names the format.
- **It is OCR, so it can be wrong.** Recognition of a small or unusual font can
  misread a word, and a picture that is not text can be read as some. The
  fixtures are sized so the recogniser reads them exactly; a real disc's subtitles
  have not been tried here.
- **A burn is irreversible for the session.** Turning the subtitles off asks the
  server for a session without the burn, which is another re-encode, not a
  toggle. The player does this at the current position. An OCR'd track turns off
  instantly.
- **A text track named for burning is not burned.** It is delivered as a
  selectable track instead — better in every way — and the reasons say so.
- **A stream index the file does not have is a `400`** listing the tracks it
  does, and a text track named for burning is a `400` too, because that is a
  category error rather than an unsupported one.

`internal/testfixtures/pgs` writes the PGS fixtures the tests use, because no
image-subtitle sample ships with the project and ffmpeg cannot encode a bitmap
subtitle from text; `scripts/pgsgen` exposes it for browser fixtures, and
`-text` makes it draw real letters so the OCR path has something to read.

### Adaptive bitrate

**A quality choice is a ceiling, and a pin is a separate request.** Three height
fields, three meanings:

- `preferred_height` — "adapt, but do not go above this." The client gets a
  **ladder topped at that height** (clipped by its own box and the source), and
  its player is free to step down when the network cannot sustain the top rung.
  This is what the quality menu sends when a viewer picks a setting.
- `max_height` **alone** — "give me exactly this." One rendition, no ladder. It
  is the deterministic request, and on a small host it is also the cheap one: a
  ladder is up to three encodes, a pin is one.
- **neither** — "adapt as far as my box allows." A ladder topped at the client's
  own ceiling or the source.

`max_height` alongside `preferred_height` is the hard ceiling that ladder stays
under, so a manifest can say "my screen is 1080" and "I chose 720" at once. A
`burn_subtitle_index` still pins one rendition whatever the heights say, because
the bitmap is composited once; the preference then chooses that single encode's
height.

A ladder has up to three rungs — the target height, two thirds of it, and half —
each with its own encoder settings, its own VBV ceiling from a conventional
bitrate table scaled by the client's own limit, and its own forced segment
boundaries. One ffmpeg process produces all of them, and the client is handed
`master.m3u8`; a player that understands HLS then switches rungs on its own. The
decision reports the rungs as `renditions`, and `target_height` and
`target_bitrate_kbps` describe the top one, so a client that reads only those
still sees a coherent answer. Every decision explains itself in `reasons`,
including which height field chose the top rung.

Rungs that would be too small to encode or too close to the rung above to be a
real choice are dropped, and a source with no room below it gets no ladder at
all rather than two renditions nobody can tell apart. Each rung is a separate
encode of the same source, so a ladder costs roughly what its rungs add up to —
on a small host, pinning a height is the cheaper way to watch.

One honest caveat: a VBV ceiling bounds the average, not every instant. A buffer
twice the ceiling lets the encoder spend what it has saved, so over a segment
shorter than the buffer the measured rate can exceed the ceiling — on a
six-second test segment a 500 kbps limit measured 632 kbps muxed, while the same
source unlimited measured 3.3 Mbps. Over a real stream the average converges.

## Security headers

Every response carries `X-Content-Type-Options: nosniff`, `Referrer-Policy:
no-referrer`, `X-Frame-Options: DENY`, `Cross-Origin-Resource-Policy:
same-origin` and a `Permissions-Policy` that refuses the features this app has no
use for. Documents additionally carry a content security policy:

```
default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self';
media-src 'self' blob:; worker-src 'self' blob:; connect-src 'self';
font-src 'self'; object-src 'none'; base-uri 'none'; form-action 'none';
frame-ancestors 'none'
```

The strictness is possible because of how the front end is written, not in spite
of it: there is no inline script, no inline style, and no HTML-injection sink
anywhere in `web/`, so `'unsafe-inline'` is not needed, and the vendored hls.js
contains no `eval` either — that was checked, not assumed. `blob:` appears for
`media-src` and `worker-src` because Media Source Extensions play a blob URL and
hls.js runs its demuxer in a worker built from one. JSON responses get the
transport-level headers but **not** the policy: a policy on a JSON body is
something a client can never act on.

`img-src 'self'` is also the enforcement behind a privacy fix: artwork is only
ever loaded from this server's `/api/images/...` proxy, which fetches the
provider's image server-side. The UI no longer uses the metadata's own absolute
URL even when it is present, because fetching `image.tmdb.org` from the browser
tells that third party who is watching and from where. Verified in headless
Chromium: a third-party image is refused with an `img-src` violation, an inline
script is refused, a same-origin image is not, and the whole 32-check player
harness passes with no console errors under the policy.

`Strict-Transport-Security` is deliberately absent: this server speaks plain
HTTP, where browsers ignore it. A reverse proxy that terminates TLS is where it
belongs — see [`deploy/README.md`](deploy/README.md).

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

## Rate limiting

`--rate-limit` (requests per second per client, default `0` = off) bounds how
often the API may be called, and `--rate-limit-burst` sets how many requests a
client may make at once (default: the rate rounded up, which is the smallest
bucket a normal page load still fits in).

```sh
./astraeus-server serve --rate-limit 20 --rate-limit-burst 40
```

The limit is a token bucket, so a client that behaves gets its allowance back
rather than being cut off for the rest of a window. Only `/api/` is limited:
the UI, its assets and the HLS segments are *delivery*, and a page load pulls
several files at once while one playback fetches a segment every few seconds.
Sessions are capped separately by `--max-sessions`, and `/api/health` stays open
for liveness probes. A refusal is a `429` in the API's error shape with a
`Retry-After: 1`, and is counted in `astraeus_rate_limited_total`.

**What a client is depends on the gate.** With an identity from `proxy` mode the
bucket is per person, which is the point: behind a proxy every request arrives
from the proxy's own address, so an address-keyed limit would be one global
bucket for everyone it serves. With no gate, the peer address is used — the same
address the access gate trusts, with `X-Forwarded-For` ignored for the same
reason. In `token` mode every API client shares the `token` identity and so
shares one bucket. A request the server cannot attribute at all is allowed
rather than charged to a bucket it shares with everyone else.

The limiter is per process. Several servers behind one proxy each hold their own
buckets, so the effective limit is the sum; a shared limit would need a shared
store.

## Tracing

`--otel-endpoint` (or `OTEL_EXPORTER_OTLP_ENDPOINT`) turns on trace export over
**OTLP/HTTP**; empty disables it, which is the default. `--otel-service-name`
sets the resource's `service.name` (default `astraeus-media`).

```sh
./astraeus-server serve --otel-endpoint http://127.0.0.1:4318
```

Every HTTP request becomes a **server span** carrying the method, path, peer
address and response status, and an incoming W3C `traceparent` is continued, so a
trace started by a proxy or another service keeps its id here. Inside a request,
a playback negotiation and the streaming session it starts are **child spans**
(the second carries the mode, the video action and the session id), so the slow
part — forking ffmpeg and waiting for the first segment — is visible as the part
that took the time. The request log line carries `trace_id` and `span_id`, which
is what connects a log entry to the trace it belongs to. Spans are batched and
posted to `<endpoint>/v1/traces`.

The encoder and the batcher are hand-rolled rather than the OpenTelemetry SDK —
the same trade the Prometheus exposition makes — so the dependency list stays at
three modules. Three consequences are worth knowing:

- A full export queue **drops spans and counts them** in
  `astraeus_spans_dropped_total` instead of adding latency to a request that has
  already finished, and a collector that is down is logged rather than retried
  into a growing queue.
- What is sent is traces only. Metrics stay on `/metrics`; logs stay on stderr.
- There is no sampling beyond honouring a parent's sampled flag, no baggage, and
  no propagation to the outbound calls the server itself makes.

Verified end to end against a real collector: Jaeger all-in-one in a container
listed `astraeus-media` as a service, received a span per request, and showed a
span carrying an incoming `traceparent` as its child.

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
node --test web/                         # the front end's pure core
node --check web/app.js                  # the rest of the UI parses
```

The front end has no build step, so its timeline arithmetic lives in
[`web/core.js`](web/core.js) — plain functions, no DOM — and is unit-tested with
Node's own runner. No npm install and no lockfile are involved. The DOM-heavy
remainder of `web/app.js` is covered by the browser harnesses below.

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
internal/subtitles/     WebVTT extraction and caching, a PGS decoder and OCR
internal/testfixtures/pgs/  a PGS (.sup) writer for image-subtitle fixtures
internal/images/        artwork proxy and cache
internal/observability/ KPI registry and Prometheus exposition
internal/access/        the access gate
internal/api/           HTTP layer
web/                    the Spatial Web UI, including vendored hls.js
                        and the generated BoxIcons registry (web/icons.js);
                        web/core.js is the unit-tested pure timeline maths
scripts/ui-verify/      browser harnesses for playback and subtitles
scripts/pgsgen/         writes a PGS (.sup) fixture for those harnesses
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

## Licence

Astraeus Media is released under the [MIT Licence](LICENSE). Vendored third-party
components and their notices are listed in
[`THIRD_PARTY_NOTICES.md`](THIRD_PARTY_NOTICES.md).

An adversarial review of the codebase, its known gaps and the competitive
landscape lives in [`docs/adversarial-review.md`](docs/adversarial-review.md),
with the two detailed source reports alongside it in `docs/review/`.

If you are picking this project up — architecture, the conventions that matter,
how to verify, what is unverified, and the traps in this environment — start with
[`docs/handoff.md`](docs/handoff.md).
