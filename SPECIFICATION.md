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
* **Hardware Acceleration:** **Intel QuickSync (VAAPI/QSV)**. Leveraged via `/dev/dri` for high-efficiency, low-latency hardware-accelerated transcoding.

### 2.2 Deployment Model
* **Primary:** **Native Binary**. The application will be distributed as a statically linked or minimally dependent binary for Linux, allowing for easy integration with systemd and direct hardware access.
* **Secondary:** **Containerized (Docker/Podman)**. A containerized version will be provided, with specific configurations to map hardware acceleration devices (`/dev/dri`) into the runtime.

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

### 4.2 Streaming Protocols
* **Primary:** **HLS (HTTP Live Streaming)** or **DASH (Dynamic Adaptive Streaming over HTTP)**. 
* **Rationale:** These protocols provide the necessary resilience for "over the ether" playback through adaptive bitrate switching and small-chunk buffering.

---

## 5. User Interface & Experience

### 5.1 Spatial Layout (The Three-Column Model)
The UI is organized into three distinct functional zones:
*   **Left Sidebar (Navigation):** High-level navigation, library selection, collections, and application settings.
*   **Main Content Canvas (The Environment):** A spacious, cinematic area where the selected `MediaEntity` becomes the visual anchor. This area uses large-scale imagery and video to create an immersive atmosphere.
*   **Right Sidebar (Contextual Intelligence):** A translucent panel providing metadata, playback controls, related content, and queue management for the currently selected media.

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

### 7.1 Authentication & Authorization
*   **Access Model:** A single-gate, instance-wide access model.
*   **Implementation:** Integration with **Tailscale** or **Cloudflare Access** for secure, identity-aware remote connectivity. No built-in user registration; access is managed by the administrator.

---

## 8. Implementation Roadmap

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
code lives in `astraeus-media/` and its `README.md` documents usage.

### 9.1 Storage

The MVP uses **SQLite** (`modernc.org/sqlite`, pure Go, no cgo) through `sqlx`,
behind a `library.Repository` interface. PostgreSQL remains the production
target; a `PostgresRepository` implementing the same interface is the only
change required. Schema creation is versioned and idempotent in Go rather than
delegated to an external migration tool: the project has no migration
dependency, and the migrations also repair data written by the earlier
prototype (backfilling `name`, resetting entities that were marked `Complete`
without a `MetadataSet`).

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
serves the original file with HTTP range support. Encoder selection prefers
QuickSync, then VAAPI, then software, but hardware is only used when the device
is actually present — an encoder being compiled into ffmpeg is not treated as
proof that it works.

Not yet implemented: multiple audio track selection, and an adaptive bitrate
ladder. Subtitle tracks are probed, and text-based tracks are extracted to
WebVTT on demand (§9.5).

### 9.4 Interface

The MVP exposes a REST API (§HTTP API in `astraeus-media/README.md`) rather than
the gRPC option mentioned in §2.3.

Observability is implemented: the KPI registry of §6.1 is instrumented and
exposed at `/metrics` in the Prometheus text exposition format. The registry is
declared at startup so an idle server still reports every metric. The metrics
are produced by a small in-process implementation rather than the
`prometheus/client_golang` dependency; replacing it is a contained change if
summaries, exemplars or a push gateway are ever needed. Distributed tracing
(OpenTelemetry) is still outstanding.

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

Rate limiting is not implemented.

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
    actually be delivered; image-based tracks (PGS, VobSub) are reported as
    `text: false` and refused with `415`, since converting them would require
    OCR.
*   **Periodic scanning.** The scanner previously ran only when invoked. It now
    runs on an interval (`--scan-interval`, default 6h) across every library,
    with `POST /api/scan` and the CLI as the manual override. A failure for one
    library is recorded against it and does not stop the others.


