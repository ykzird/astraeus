# Master Technical Specification: The Spatial Media Environment (Project "Jellyfin-Killer")

## 1. Executive Summary

### 1.1 Vision
The goal is to build a "media-first" spatial environment that moves away from the conventional "database rendered as rectangles" approach used by modern streaming services. Instead of a dashboard containing media, this application provides a cinematic canvas where media is the primary inhabitant, and the interface appears only when and where it is useful.

### 1.2 Core Principles
* **Media-First:** The media (artwork, video, imagery) is the visual anchor. The UI provides structure and context without obscuring the content.
* **Spatial Awareness:** Using a structured three-column layout to provide clear, predictable navigation and contextual depth.
* **Resilient Streaming:** Prioritizing "Direct Play" with a fallback to highly resilient, segmented adaptive bitrate streaming (HLS/DASH).
* **Performance through Observability:** Designing with built-in instrumentation to enable scientific, data-driven optimization.
* **Minimalist Complexity:** Providing high-performance, professional-grade features without the bloat of traditional media managers.

---

## 2. System Architecture

### 2.1 Technology Stack
* **Backend:** **Go (Golang)**. Chosen for its high concurrency primitives, performance, and ability to produce a single, high-performance native binary.
* **Database:** **PostgreSQL** for production; **SQLite** for the MVP. PostgreSQL is chosen for its reliability, scalability, and robust support for relational data and complex queries. The MVP ships on embedded SQLite behind a `Repository` interface, so the storage implementation can be swapped without touching domain code — see §9 for what is actually implemented today.
* **Media Processing:** **FFmpeg**. The industry standard for decoding, encoding, and segmenting media.
* **Hardware Acceleration:** **NVENC, QuickSync, VideoToolbox, VAAPI and AMF**, used in that order of preference. Each is proved by running it before it is offered, so a machine whose driver cannot open a session falls back rather than failing.

### 2.2 Deployment Model
* **Primary:** **Native Binary**. The application will be distributed as a statically linked or minimally dependent binary for Linux, allowing for easy integration with systemd and direct hardware access.
* **Secondary:** **Containerized (Docker/Podman)**. A containerized version will be provided, with specific configurations to map hardware acceleration devices (`/dev/dri`) into the runtime.

Both shapes exist as of 0.3.0 (`Dockerfile`, `deploy/astraeus.service`, and
`deploy/README.md` as the runbook). The binary is pure Go with no cgo, so the
image is a two-stage build: one stage compiles a static binary, the runtime stage
adds the one hard dependency, ffmpeg. The container runs as a fixed non-root uid
with every writable path inside a single volume, and the unit binds loopback with
the access gate on, so neither shape publishes an unauthenticated library by
default. Deployment is where the server's own honesty matters most: the startup
probe reports which encoders this host can actually use, so a container that
cannot see a GPU says so instead of transcoding badly.

### 2.3 High-Level Architecture
The system follows a modular service-oriented architecture:
1.  **Library Manager:** Handles filesystem scanning, `MediaEntity` lifecycle, and `MetadataSet` management.
2.  **Streaming Engine:** Manages `StreamSession` negotiation, segment delivery (HLS/DASH), and `TranscodeJob` orchestration.
3.  **API Layer:** A high-performance REST or gRPC interface for client communication and administrative control.
4.  **Observability Agent:** A background service collecting metrics, traces, and logs.

---

## 3. Domain Model & Data Schema

### 3.1 The Ubiquitous Language
* **MediaEntity**: A logical representation of content.
    * **ContainerEntity**: A `MediaEntity` that holds other `MediaEntity` objects (e.g., `Series`, `Season`).
    * **LeafEntity**: A `MediaEntity` representing the final content node (e.g., `Episode`, `Movie`).
* **MediaObject**: The physical file(s) representing a `LeafEntity`.
* **Library**: A logical grouping of `MediaEntity` objects.
* **MetadataSet**: Descriptive attributes (title, art, etc.) associated with a `MediaEntity`.
* **StreamSession**: An active, stateful connection between a client and the server.
* **ClientCapability**: A description of a client's codec, resolution, and protocol support.
* **TranscodeJob**: A task to convert a `MediaObject` to satisfy a `StreamSession`.
* **AccessPolicy**: Rules governing user access to the application and its content.

### 3.2 Data Schema Overview (PostgreSQL)
* **`libraries`**: `id`, `name`, `path`, `type`.
* **`media_entities`**: `id`, `library_id`, `parent_id`, `type` (Series/Season/Episode/Movie), `status` (Complete/Incomplete).
* **`media_objects`**: `id`, `media_entity_id`, `file_path`, `size`, `mime_type`.
* **`metadata_sets`**: `id`, `media_entity_id`, `provider_id`, `data` (JSONB).
* **`stream_sessions`**: `id`, `media_entity_id`, `client_id`, `start_time`, `status`.
* **`transcode_jobs`**: `id`, `session_id`, `media_object_id`, `target_format`, `status`.
* **`users`**: `id`, `username`, `access_level`.

---

## 4. Media Engine & Streaming Pipeline

### 4.1 The Negotiation Flow
To ensure maximum efficiency and minimal latency, the server implements a **Proactive Negotiation** strategy:
1.  **Discovery:** The client connects and immediately sends its `ClientCapability` manifest.
2.  **Evaluation:** The server compares `ClientCapability` against the available `MediaObject` properties and hardware capabilities.
3.  **Decision:**
    *   **Direct Play (Segmented):** If the client supports the source codec and protocol, the server serves the original bits via segmented passthrough (HLS/DASH).
    *   **Transcoded Streaming:** If a mismatch is detected, the server initiates a `TranscodeJob` to convert the source into a compatible, segmented stream.
    *   **Tone Mapping:** If the source is high dynamic range and the client has not declared that it can render HDR, the job converts the transfer function to SDR as well as any codec mismatch, since PQ or HLG code values shown as SDR describe different light rather than merely less of it. A client that can render HDR keeps it, provided an encoder on this host has proved it can produce 10-bit output.

### 4.2 Streaming Protocols
* **Primary:** **HLS (HTTP Live Streaming)** or **DASH (Dynamic Adaptive Streaming over HTTP)**. 
* **Rationale:** These protocols provide the necessary resilience for "over the ether" playback through adaptive bitrate switching and small-chunk buffering.
* **Adaptive delivery (0.5.0):** a client that does not pin a height is served a **ladder** - up to three rungs, one ffmpeg process, a master playlist, and per-rung ceilings scaled by the client's own limit - so the protocol's adaptive switching is actually used rather than merely available. A ladder re-encodes the audio once per rung rather than copying it, because ffmpeg's HLS muxer refuses to place one copied elementary stream in two variants; on the ffmpeg versions Debian and Ubuntu ship that refusal silently drops rungs from the master playlist, so a client never sees them.
* **A quality choice as a ceiling (0.16.0):** `preferred_height` asks for a ladder topped at that height, so a viewer's choice bounds quality without forbidding the player to step down when the network cannot sustain the top rung. `max_height` alone keeps its 0.5.0 meaning of pinning exactly one rendition - the deterministic and cheaper request - and alongside a preference it is the hard ceiling the ladder stays under. Omitting both adapts to the client's own box. The distinction is what lets a manifest say "my screen is 1080" and "I chose 720" at once.

---

## 5. User Interface & Experience

### 5.1 Spatial Layout (The Three-Column Model)
The UI is organized into three distinct functional zones:
*   **Left Sidebar (Navigation):** High-level navigation, library selection, collections, and application settings.
*   **Main Content Canvas (The Environment):** A spacious, cinematic area where the selected `MediaEntity` becomes the visual anchor. This area uses large-scale imagery and video to create an immersive atmosphere.
*   **Right Sidebar (Contextual Intelligence):** A translucent panel providing metadata and the reasons behind a delivery decision, related content, and queue management for the currently selected media. It is informational: it does not carry the transport.
*   **Player (overlaid on the canvas):** Once playback starts, the transport is drawn over the video itself — play/pause, restart, skip, stop, the seek bar with a time readout, subtitle selection, volume and mute, quality selection, and fullscreen. The bar fades out while playing and returns on interaction or focus, and is `inert` while hidden so a keyboard user cannot land in an invisible trap.

### 5.2 Visual Language
*   **Atmosphere (Glassmorphism):** Contextual panels, metadata overlays, and player controls utilize translucent, blurred surfaces to allow the background media to bleed through, maintaining immersion.
*   **Interaction (Neo-brutalism):** Functional elements—buttons, toggles, selected states, and borders—utilize high-contrast, bold, and tactile design to provide unmistakable affordance and feedback.

---

## 6. Observability & Performance

### 6.1 KPI Registry
The system will track the following key metrics to ensure performance excellence:
*   **`fttt_latency`**: Time from request to first segment delivery.
*   **`transcode_startup_time`**: Latency of the transcoding engine initialization.
*   **`metadata_latency`**: Time taken to retrieve and process metadata.
*   **`stream_error_rate`**: Frequency of playback interruptions or failures.

### 6.2 Instrumentation Strategy
*   **Structured Logging:** All major system events will be logged using `slog` with rich context.
*   **Distributed Tracing:** Using OpenTelemetry to provide end-to-end visibility into the lifecycle of a request, from API call to media segment delivery.
*   **Metrics Collection:** Prometheus-compatible metrics endpoint for real-time monitoring.

---

## 7. Security & Access Control

### 7.0 Response hardening

Every response carries `X-Content-Type-Options`, `Referrer-Policy`,
`X-Frame-Options`, `Cross-Origin-Resource-Policy` and `Permissions-Policy`;
documents also carry a content security policy with no `unsafe-inline` and no
`unsafe-eval`, and `img-src 'self'` so artwork can only come from this server's
own proxy. That policy is affordable because the front end has no inline script,
no inline style and no HTML-injection sink — the discipline the UI already
followed is now an enforced boundary rather than a convention. JSON and metrics
responses are exempt from the policy, which they could not act on.
`Strict-Transport-Security` is left to a TLS-terminating proxy, because this
server speaks plain HTTP and browsers ignore the header there.

### 7.1 Authentication & Authorization
*   **Access Model:** A single gate that admits a request, plus an optional
    per-viewer policy (`--access-policy`) deciding which libraries each viewer may
    see and who may change the library. Visibility defaults to everything when no
    policy is configured, so the single-gate model is what an install has until an
    operator chooses otherwise.
*   **Implementation:** Integration with **Tailscale** or **Cloudflare Access** for secure, identity-aware remote connectivity. No built-in user registration; access is managed by the administrator.

---

## 8. Implementation Roadmap

> This is the plan as originally written. Phases 1-4 are substantially built;
> §9 records what actually exists and where it differs. It is kept as written
> because it shows the intended shape of the project rather than the order it
> happened in.

### Phase 1: Foundation (Current)
*   Finalize Domain Model and Technical Specification.
*   Set up Go project structure and PostgreSQL schema.

### Phase 2: Core Engine (MVP)
*   Implement Filesystem Scanner and TMDB Metadata Worker.
*   Develop the basic HTTP API and Segmented Streaming (HLS) capability.
*   Build the basic Three-Column Web UI.

### Phase 3: Hardware & Optimization
*   Integrate Intel QuickSync for hardware-accelerated transcoding.
*   Implement the full Negotiation Logic.
*   Enable full Observability/Telemetry suite.

### Phase 4: Refinement & Scale
*   Implement advanced UI animations and transitions.
*   Expand metadata providers and library types.
*   Conduct large-scale performance benchmarking and optimization.

---

## 9. Implementation Status

This section records where the running system diverges from, or has advanced
beyond, the plan above. It is the authoritative description of what exists; the
code lives in `astraeus-media/`; `README.md` is the way in and `docs/` is the
reference.

### 9.1 Storage

The MVP uses **SQLite** (`modernc.org/sqlite`, pure Go, no cgo) through `sqlx`,
behind a `library.Repository` interface. PostgreSQL remains the production
target; a `PostgresRepository` implementing the same interface is the only
change required. Schema creation is versioned and idempotent in Go rather than
delegated to an external migration tool: the project has no migration
dependency, and the migrations also repair data written by the earlier
prototype (backfilling `name`, resetting entities that were marked `Complete`
without a `MetadataSet`).

`sqlite.DSN` (`internal/library/sqlite`) opens every pooled connection with `busy_timeout(5000)`,
`journal_mode(WAL)`, `synchronous(NORMAL)` and `foreign_keys(1)`. WAL is what
lets a reader proceed during a write and makes bulk scans fast, at the cost of
keeping recent writes in `<db>-wal` and `<db>-shm` sidecars until a checkpoint:
a backup must copy all three files, or stop the server first. Foreign keys are
now genuinely enforced, which is why the prune deletes objects and child
entities before their parents.

The realised schema is narrower than §3.2:

*   `libraries` — `id`, `name`, `path`, `kind`, `created_at`.
*   `media_entities` — `id`, `library_id`, `parent_id`, `type`, `name`,
    `status`, `created_at`, `updated_at`, `metadata` (JSON).
*   `media_objects` — `id`, `media_entity_id`, `file_path`, `size`, `mime_type`,
    `created_at`.

`metadata_sets`, `stream_sessions`, `transcode_jobs` and `users` do not exist as
tables. `MetadataSet` is embedded as JSON on the entity; streaming sessions and
transcode jobs are in-memory process state, which is correct for transient
deliveries but means they do not survive a restart.

### 9.2 Identity and hierarchy

A `MediaEntity` is identified by
`(library_id, COALESCE(parent_id, ''), type, name)`, enforced by a unique index.
This is what prevents a "Season 1" of one series from colliding with a "Season 1"
of another, and what makes re-scans idempotent.

Hierarchy is derived from the directory layout:

*   Movies — `<root>/Title (Year)/file.ext` or `<root>/file.ext`.
*   Shows — `<root>/Series/Season NN/file.ext`, with `SxxExx` parsed from the
    file name. The `Season NN` directory wins over the number in the file name;
    the file name supplies the episode number.

An entity is `Incomplete` until a `MetadataSet` is attached, and stays that way
if the provider fails, so it surfaces as work for an administrator rather than
being silently filled in.

### 9.3 Media engine

Negotiation is a pure function of `MediaInfo` (from `ffprobe`) and
`ClientCapability`, returning one of `direct_play`, `remux` or `transcode` plus
the reason for every choice. Delivery is HLS generated by `ffmpeg`; direct play
serves the original file with HTTP range support.

Encoder selection walks a preference list of hardware families - NVENC,
QuickSync, VideoToolbox, VAAPI, AMF - and falls back to software. A hardware
encoder is offered only after a startup probe has encoded with **the same options
a real session uses**: an encoder can be compiled in, be listed, and still reject
its own flags, and a probe that tested something else would pass while playback
failed. An encoder that fails is reported with ffmpeg's complaint, because a
machine with an unusable GPU must not look like one with no GPU. Even a software
encoder must appear in the host's reported list: being compiled into ffmpeg is
not proof that this build has it.

Only the list of verified encoders decides selection, so a family cannot be
chosen that the probe rejected. VAAPI additionally needs a DRM render node, which
is discovered once and passed to both the probe and every session, ahead of the
input, since its upload filter has no device to upload to without it.

Negotiation considers five independent axes, each with a reason attached:
container, codec, resolution (as a bounding box, so a 2.35:1 source fits a
16:9 limit), video bit depth, and audio channel count. Bit depth and channel
count matter because a codec name the client accepts is not proof it can decode
the stream: Chromium refuses 10-bit H.264 and 5.1 AAC SourceBuffers, and a
refused audio append tears down the video with it.

Dynamic range is a sixth axis. It is classified from the source's **transfer
function** — PQ (SMPTE ST 2084) and HLG (ARIB STD-B67) are HDR, everything else
is SDR — and deliberately not from bit depth or primaries, because a 10-bit
BT.2020 SDR master is stored like any other SDR material. A client that has not
declared `supports_hdr` receives a tone-mapped SDR stream: an HDR source shown as
SDR is not merely dimmer, its code values describe different light. A client that
has declared it keeps HDR through a copy, a remux or a 10-bit re-encode, and the
HDR path is gated on an encoder the startup probe drove with a 10-bit pixel
format, because an encoder that works at 8 bits may still refuse 10. Where no
such encoder exists the stream is tone mapped to SDR with the reason attached
rather than failed. Dolby Vision is reported alongside the range (profile and
whether the base layer is HDR10); its dynamic metadata cannot survive a
re-encode and profile 5's IPTPQc2 base layer cannot be converted correctly
without a Dolby Vision tone mapper, both of which appear in the reasons.

Bandwidth is the seventh. A client's `max_bitrate_kbps` is a limit on the whole
stream, so it is honoured by reserving the audio's share — the source's own rate
when the audio is copied, this server's encode target otherwise — and holding the
video to the remainder as a VBV ceiling (`-maxrate` with a buffer twice that
size, applied uniformly to every encoder family, so quality-driven encodes stay
quality-driven). A source the client can decode but cannot afford is therefore
re-encoded rather than copied, and a limit below what the audio alone needs is
refused as undeliverable instead of being met with an unwatchable picture. Two
honest consequences: an unknown source bitrate is not assumed to exceed the limit,
because guessing would transcode files that fit, and a VBV ceiling is an average
rather than an instantaneous bound — over a segment shorter than the buffer the
measured rate can exceed it while the encoder spends what it has saved.

The output's colour is set on the frames, not through `-color_primaries` and
friends: those options were measured not to reach the output, because ffmpeg
writes the encoder's VUI from the frame properties. The tone-map chain sets
BT.709 through `zscale`; an HDR pass-through states BT.2020 and its transfer
through `setparams`.

The pure function knows nothing about the host, so the API runs it through
`NegotiateForServer`: if the chosen target codec has no encoder here it is
retargeted to another codec the client accepts, and if there is none the
decision is marked undeliverable and the request answers `409`. A manifest
naming a codec outside the known vocabulary is refused with `400` before ffmpeg
is invoked. A session can also begin at an offset (`start_seconds`), which is
what makes a seek into unproduced content reachable.

Sessions are bounded and explicit: `--max-sessions` (default 8) caps concurrent
segmented streams — each is an ffmpeg process — and a refusal is a `429
too_many_sessions`, while `DELETE /api/streams/{id}` stops one immediately
rather than waiting for the idle reaper.

The resumable positions are also a query - one viewer's, most recently watched
first - so the stored state is discoverable rather than something a viewer has to
remember their way back to.

Playback position is persisted per **viewer** and entity and resumable: a report
replaces that viewer's stored position, a position in the closing 5% clears it
because that viewer watched the entity through, and the row cascades when the
entity is pruned, so a pruned film cannot leave a bookmark behind. The viewer is
the identity the access gate attaches to the request; with no gate there is one
viewer, named by the domain, and the gate's `token` mode names every API client
the same, so per-viewer state is a `proxy`-mode property. No handler reads an
identity header itself: the only source of a viewer is the context the verified
gate fills, which is what stops one viewer from naming themselves another.

Audio tracks are probed as a list and selected by stream index. Every audio
decision follows the chosen track rather than the first one, and a file's own
`default` disposition decides what a client that does not choose receives. A
chosen track forces at least a remux, because direct play hands the player the
whole file and the player would pick its own track; the picture is copied, so the
cost is repackaging rather than re-encoding.

Image-based subtitles (PGS, VobSub) carry pictures rather than text, and a
browser has no way to render a timed bitmap. Two delivery paths exist, and the
server offers the better one it can actually perform.

**OCR, for PGS and VobSub, when an engine is installed.** `internal/subtitles`
decodes an image stream into bitmaps, renders each cue as dark glyphs on a white
page, and hands it to `tesseract`, whose output becomes the WebVTT body. The
resulting track is delivered, cached and served exactly like a text track, so it
can be toggled, restyled and searched and costs a fetch rather than a re-encode.
The OCR engine is an **optional runtime dependency**: when it is absent the
server keeps the previous behaviour - no URL is advertised for an image track and
the endpoint answers `415 subtitle_format_unsupported` - rather than failing.
Two decoders exist. The PGS one reads the HDMV segments (PCS/ODS/PDS,
run-length-encoded objects, the YCbCr palette) from a raw `.sup`. The VobSub one
reads a `dvd_subtitle` track's control sequence, two interleaved run-length
fields and its container's palette: because that palette lives outside the
picture stream - in Matroska's codec private, or a `.idx` sidecar - the
extraction keeps a container that carries one, and a bare MPEG-PS source with no
palette is refused rather than rendered blank. DVB subtitles have no decoder and
stay burn-only; the refusal names the format rather than failing inside the
extractor. `subtitle_ocr_enabled` in `/api/system/capabilities` reports the
host's answer.

**Burn-in, as the fallback.** A client asks for it with `burn_subtitle_index` on
the playback request; the decision then reports `burned_subtitle_index`, forces a
transcode, and pins one rendition, because the bitmap is composited once in one
filter graph rather than per ladder rung. A text track named for burning is not
burned - it is delivered as a selectable track, which is better in every way -
and the reasons say so. Burning is irreversible for the session by design, so
turning the subtitles off re-negotiates a session without the composite.

### 9.4 Interface

The MVP exposes a REST API (documented in [`docs/api.md`](docs/api.md)) rather than
the gRPC option mentioned in §2.3.

Observability is implemented: the KPI registry of §6.1 is instrumented and
exposed at `/metrics` in the Prometheus text exposition format. The registry is
declared at startup so an idle server still reports every metric. The metrics
are produced by a small in-process implementation rather than the
`prometheus/client_golang` dependency; replacing it is a contained change if
summaries, exemplars or a push gateway are ever needed.

Distributed tracing is implemented for traces only, over OTLP/HTTP, and is
opt-in (`--otel-endpoint`; empty disables it). It follows §6.2's intent — end-to-end
visibility from an API call — while keeping the dependency list short: the OTLP
JSON encoder and the batcher are hand-rolled in `internal/tracing`, the same
trade the Prometheus exposition makes. A request span reads an incoming W3C
`traceparent` so a trace started upstream continues here, and inside a request a
**playback negotiation** and the **streaming session it starts** are child spans,
so forking ffmpeg and waiting for the first segment — the slow part of a request —
is visible as the part that took the time. The request log line carries
`trace_id` and `span_id`, which is what connects a log entry to the trace it
belongs to. Spans are exported in batches; a full export queue drops spans and
counts them in `astraeus_spans_dropped_total` rather than adding latency to a
request that has already finished, and a collector that is down is logged rather
than retried into a growing queue. Metrics and logs are *not* sent over OTLP:
metrics stay on `/metrics` and logs stay on stderr. Scans and metadata lookups
are not spanned yet.

The front end's timeline arithmetic has a tested seam. `web/core.js` holds the
pure functions — source↔media time, the produced window, the clock — with no DOM,
network or module state, and is unit-tested with Node's own runner (`node --test
web/`), which needs no npm install. `app.js` reads it as `window.AstraeusCore` and
keeps everything that touches the DOM; that remainder is covered by the CDP
harnesses rather than by unit tests, because a render function's contract is what
a browser shows.

Authentication is implemented as an access gate in front of the whole server
(§7.1's model: identity is established at the edge, not by a built-in user
database). `--auth-mode proxy` believes an identity header — `Tailscale-User-Login`
or `Cf-Access-Authenticated-User-Email` — **but only when the request arrives
from a configured trusted address**; the address check is what makes the header
believable, since headers are otherwise trivially forgeable. `--auth-mode token`
requires a constant-time compared bearer token for API clients. Configuration
fails closed: `proxy` without `--trusted-proxy`, or `token` without a token,
refuses to start. `X-Forwarded-For` is deliberately not used to establish trust.
The authenticated identity is recorded on every request log line, and grants and
denials are counted. `--auth-mode none` remains the default for a trusted LAN.

Rate limiting is implemented as a token bucket per client in front of the API
(`--rate-limit`, off by default; `--rate-limit-burst`). Only `/api/` paths are
bounded: the UI, its assets and the HLS segments are delivery rather than work,
sessions are capped separately by `--max-sessions`, and `/api/health` stays open
for liveness probes. A client is the gate's identity when there is one — behind a
proxy every request arrives from the proxy's address, so an address-keyed limit
would be one global bucket for everyone it serves — and otherwise the peer
address, with `X-Forwarded-For` ignored for the same reason the gate ignores it.
A refusal is a `429` in the API's error shape with a `Retry-After`. The limiter
is per process: several servers behind one proxy each hold their own buckets, so
the effective limit is their sum.

### 9.5 Artwork, subtitles and scanning

Three capabilities were added after the phases above were written:

*   **Artwork.** TMDB poster and backdrop references are turned into
    server-local URLs (`poster_url`, `backdrop_url` on every entity payload) and
    served through a caching proxy at `/api/images/{size}/{file}`. The browser
    therefore never contacts a third-party origin, and the server caches what it
    fetches. The size segment and the file name are both allow-listed before
    they reach the cache or the upstream URL, which is what keeps a
    proxy of this kind from becoming an SSRF or path-traversal hole.
*   **Subtitles.** `ffprobe` reports each subtitle track with its codec,
    language, title, default/forced flags and whether it is text or image based.
    Text tracks (`subrip`, `ass`, `mov_text`, …) are converted to WebVTT with
    `ffmpeg` on demand, cached against the source file's size and modification
    time, and served at `/api/objects/{id}/subtitles/{track}.vtt`. The playback
    response lists every track and only advertises a URL for the ones that can
    actually be delivered. Text tracks always can; an image-based PGS or VobSub
    track can when an OCR engine is installed, in which case it is decoded by the
    matching reader in `internal/subtitles` and read by `tesseract` into WebVTT.
    An image track with no text path - no engine, or a codec no reader decodes
    (DVB) - is delivered by **burn-in** instead (§9.3): the picture is
    re-encoded with the bitmap composited into it. The subtitle is decoded from a
    second opening of the input, scaled to the picture with `scale2ref` so a
    downscaled re-encode places it correctly, and overlaid after the plan's own
    filters - which is what makes it right for a tone map, since a subtitle
    bitmap is SDR white and must not be tone mapped with the picture.
*   **Periodic scanning.** The scanner previously ran only when invoked. It now
    runs on an interval (`--scan-interval`, default 6h) across every library,
    with `POST /api/scan` and the CLI as the manual override. A failure for one
    library is recorded against it and does not stop the others. A scan also
    **prunes**: an object whose file is gone is removed, and any entity left
    holding neither an object nor a child is deleted upwards, reported as
    `objects_pruned` and `entities_pruned`. Pruning refuses to act when the scan
    could not read every path, or when it saw zero files against a library that
    still holds entities — the unmounted-drive case — because silently emptying
    a library is worse than leaving a ghost entry behind.


