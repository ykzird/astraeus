# Astraeus Media — Competitive Gap Analysis

> **Point in time.** This is the analysis as delivered, against commit `aad2cb1`. Its
> feature inventory has been overtaken in places by work since: `LICENSE` and the
> third-party notices exist, playback now resumes at an offset, `max_bitrates_kbps`
> is still unused, and the package layout has been reorganised. The body below is
> deliberately unedited, because a comparison is only useful if it records what was
> true when it was made.


**Question:** what is missing before anyone could call this a "Jellyfin killer"?

**Method.** I read `README.md`, `SPECIFICATION.md`, `CONTEXT.md`, `TODO.md`, `go.mod`, the git
history and every non-test Go file under `internal/` and `cmd/`, plus the shipped web client
(`web/app.js`, `web/index.html`). External claims were checked against 2025–2026 web sources and
are cited inline. Where a source is a secondary aggregator I say so. The repository was not
modified.

---

## 1. Where this project already stands

### 1.1 Scale and maturity (the unflattering context first)

- ~13,400 lines of Go (46 files, 191 test functions) plus ~3,300 lines of vanilla JS. Three direct
  dependencies (`google/uuid`, `jmoiron/sqlx`, `modernc.org/sqlite`) — no cgo, no DB server.
- **Git history is 7 commits, all dated 2026-10-07/08.** This is roughly a day or two of
  development. The "Library Manager", "Media Engine", artwork, subtitles and KPI registry are
  genuinely implemented and tested — but the whole thing is younger than most bug reports against
  Jellyfin. Any comparison must be read with that in mind.
- `LICENSE` is absent. No Dockerfile, no systemd unit, no Makefile, no CI. The `deploy/`,
  `configs/`, `docs/`, `pkg/` and `api/` directories exist but are **empty placeholders**.

### 1.2 Feature inventory

Verdicts: **done** (works, as far as code and tests show) · **partial** (narrow or incomplete) ·
**absent**.

| Area | What is actually there | Verdict |
| --- | --- | --- |
| Architecture | Go single binary, clear package seams (`library`, `streaming`, `subtitles`, `images`, `access`, `observability`, `api`), `Repository` interface so Postgres can replace SQLite | done |
| Packaging / distribution | Empty `deploy/`/`configs/`; no image, unit file, sample config, release pipeline or licence | **absent** |
| Filesystem scanning | Movies and shows; idempotent via unique index on `(library_id, COALESCE(parent_id,''), type, name)`; periodic scan (`--scan-interval`, default 6h) with manual override; per-library failure isolation | done |
| Library kinds | `movies` and `shows` only. No music, photos, home video, books, or mixed libraries | **absent** |
| Hierarchy | Series → Season → Episode derived from directory layout; `SxxExx` from filename with the `Season NN` directory winning | done |
| Persistence | SQLite via `sqlx`, versioned in-code migrations (including repair of an earlier prototype's data). No `users`, `metadata_sets`, `stream_sessions` or `transcode_jobs` tables; metadata is a JSON blob on the entity, sessions are process memory | partial |
| Metadata providers | TMDB for movies and series, plus a synthetic fallback and a `ChainProvider`. Episode/season-level lookups are **deliberately skipped**. `MetadataSet` is thin: title, description, poster path, backdrop path, provider, `extra map[string]string`. No cast, genres, ratings, runtime, studios, or first-class external IDs. No NFO/local provider, no TVDB/AniDB | partial |
| Artwork | TMDB poster/backdrop proxied through `/api/images/{size}/{file}` with allow-listed sizes and bare basenames (defends SSRF and traversal) and a local disk cache. Cache defaults to a temp dir, so it does not survive reboot | done (narrow) |
| Enrichment | Background worker with interval and manual trigger; entities stay `Incomplete` on provider failure rather than being filled with a placeholder | done |
| Capability negotiation | 10-field `ClientCapability`, normalised vocabulary; a **pure function** returning `direct_play`/`remux`/`transcode` with a reason per axis: container, codec, resolution (as a bounding box), video bit depth, audio channels. `409` when the declaration makes delivery impossible | done (narrow) |
| Bitrate cap | `max_bitrate_kbps` is parsed, validated and echoed in the payload — **never used** in `Negotiate` or in the ffmpeg arguments. A client asking for 3 Mbps still gets the source bitrate | **cosmetic stub** |
| HLS delivery | ffmpeg, `copy` or re-encode, single variant, MPEG-TS segments, `event` playlist, TTL reaping, graceful shutdown, traversal-safe segment serving | done (single-rendition) |
| ABR ladder | None. One rendition per request. Confirmed in `SPECIFICATION.md` §9.3, `TODO.md` and `BuildFFmpegArgsAt` | **absent** |
| Seek on segmented streams | The **API** accepts `start_seconds` and restarts ffmpeg at an offset (`-ss` before `-i`); the **shipped web UI never sends it**. `TODO.md`'s claim that "Plex and Jellyfin restart ffmpeg at an offset; we do not yet" is stale | partial |
| Direct play | Original file over HTTP with range support | done |
| Multi-audio tracks | First audio stream wins: `-map 0:v:0 -map 0:a:0?`. No selection, no API field, no UI | **absent** |
| Subtitles | Tracks probed (codec, language, title, default, forced, text vs bitmap); text tracks converted to WebVTT on demand, cached against size+mtime, served; UI selector with default-off behaviour; image tracks reported and refused with `415` | partial |
| Subtitle burn-in / styling | None. ASS becomes plain WebVTT, so styling, positioning and embedded fonts are lost; PGS/VobSub cannot be delivered at all (needs OCR or bitmap overlay) | **absent** |
| HDR / Dolby Vision | Only bit depth is probed. Output pixel format is pinned to 8-bit `yuv420p`/`nv12`; there is no `color_transfer`/`color_primaries`/`side_data` parsing and no tone-mapping filter. A 10-bit HDR (BT.2020/PQ) source is therefore converted to 8-bit SDR by the naive default — which produces the classic washed-out/dark result. Dolby Vision is not detected or handled at all | **absent** |
| Audio passthrough | No passthrough concept. Codec vocabulary includes `truehd`/`dts`, but HLS/MPEG-TS delivery and the browser profile's stereo downmix mean lossless formats do not survive a transcode | partial |
| Users / accounts | None. No user table, no login, no session tokens, no profiles, no per-user anything | **absent** |
| Access gate | `none` (default) / `proxy` (identity header believed **only** from a trusted CIDR — Tailscale or Cloudflare Access) / `token` (constant-time bearer). Fails closed, identity on every log line, grant/deny metrics, `/metrics` not exempt | done for what it is |
| Watch state / resume | None. There is nowhere to store it — no users, no per-item user data | **absent** |
| Playlists / collections / favourites | None. The UI's "Queue" panel is a list of a leaf's files or a container's children, not a playback queue | **absent** |
| Search / sort / filter | No search. The only filter is "Incomplete only" | **absent** |
| Observability | Custom in-process registry (not `prometheus/client_golang`), ~15 metrics including the four specified KPIs, Prometheus text at `/metrics`, full registry declared at startup so absence-based alerting works. No OpenTelemetry tracing, no dashboards, no per-stream/per-user playback reporting | done (narrow) |
| Web UI | Vanilla-JS SPA, three-column spatial layout, vendored hls.js 1.7.3, subtitle selector, decision reasons and codec chips surfaced to the user, honest "Growing · seekable to X" indicator. No search, settings, PiP, Cast/AirPlay or Media Session | partial |
| Hardware acceleration | QSV and VAAPI only. Every hardware encoder is **proved by a real 0.2 s encode** at startup, with a counted software fallback at runtime. No NVENC, AMF, VideoToolbox or RKMPP | partial |
| Clients | Web browser only. No Android/iOS, no TV, no desktop, no Kodi | **absent** |
| Plugin system | None. Providers are an internal Go interface | **absent** |
| Live TV / DVR | None | **absent** |
| Rate limiting / TLS | None (TLS is assumed to terminate elsewhere) | **absent** |

### 1.3 Honest summary

This is a **well-engineered kernel** of a media server with an unusually good instinct for
operational honesty (verified encoders, negotiation reasons, refusal to fake metadata). It is not
a product. Compared with the incumbents it is roughly: *library scanning and negotiation at parity
for one narrow input shape; everything a user touches daily — accounts, resume, search, music,
subtitles that actually render as authored, HDR that looks right — either partial or missing; and
the entire client and plugin ecosystem missing.*

---

## 2. The gap list

### 2.1 Table stakes — without these it is a demo, not a product

| # | Gap | Why it is table stakes |
| --- | --- | --- |
| 1 | **User accounts and per-user state** (resume position, played, favourites, ratings) | Jellyfin stores this server-side per user via `UserData`/`PlaybackPositionTicks` ([PR #10573](https://github.com/jellyfin/jellyfin/pull/10573)); Plex and Emby treat it as core. Without it there is no "continue watching" — the single most obvious media-server behaviour |
| 2 | **Client apps** — Android, iOS, Android TV/Fire TV, tvOS, Roku, webOS/Tizen, Kodi, desktop | Jellyfin ships official apps for essentially all of these ([client list](https://jellyfin.org/downloads/clients/), [docs](https://github.com/jellyfin/jellyfin-docs/blob/master/general/clients/index.md)). A browser-only server is not usable on a TV, which is where most people watch |
| 3 | **A stable, documented API and SDKs** | Jellyfin has Swagger/OpenAPI, a Kotlin SDK ([kotlin-sdk.jellyfin.org](https://kotlin-sdk.jellyfin.org/)) and a TypeScript SDK ([typescript-sdk.jellyfin.org](https://typescript-sdk.jellyfin.org/)), which is *why* third-party clients exist. Astraeus has REST endpoints and no spec, no versioning and no SDK |
| 4 | **Multi-audio-track selection** | `0:a:0` only. Any dual-language or commentary release is unplayable as intended |
| 5 | **Subtitle delivery that works for real discs** — burn-in for ASS/PGS/VobSub, font extraction | Jellyfin documents burn-in as the most CPU-intensive path but *does* it ([codec support](https://jellyfin.org/docs/general/clients/codec-support/)); REFUSING PGS/VobSub outright excludes most Blu-ray remuxes |
| 6 | **Correct HDR → SDR conversion** | Pinning 8-bit without tone mapping is worse than transcoding slowly: it produces wrong colours. Jellyfin has HW tone mapping for HDR10/HLG and DV P5/P8 ([hardware acceleration](https://jellyfin.org/docs/general/post-install/transcoding/hardware-acceleration/)) |
| 7 | **Music** | Jellyfin supports music libraries with MusicBrainz-style scraping ([docs](https://jellyfin.org/docs/general/server/media/music/)); Plex has Plexamp; Emby has a music story. A "media server" without music is a video server |
| 8 | **Search, sorting, filtering, collections, playlists** | Jellyfin improved search substantially in 10.11 ([release notes](https://jellyfin.org/posts/jellyfin-release-10.11.0/)). Astraeus has a single "incomplete" toggle |
| 9 | **Metadata depth** — episode-level metadata, cast/crew, genres, ratings, runtime, external IDs; NFO/local metadata; TVDB/AniDB for anime | TMDB series-level lookup only. Jellyfin ships TMDB, OMDb, NFO, and TVDB/fanart.tv/AniDB via plugins ([metadata docs](https://jellyfin.org/docs/general/server/metadata/)) |
| 10 | **Remaining library kinds** — photos, home video, books | Jellyfin has all three (books via the Bookshelf plugin, [Codeberg fork](https://codeberg.org/bfordham/jellyfin-plugin-bookshelf)) |
| 11 | **Trickplay thumbnails and chapters** | Scrubbing without previews feels broken in 2025. Jellyfin shipped server-side trickplay in ~10.9 ([PR #9554](https://github.com/jellyfin/jellyfin/pull/9554)) |
| 12 | **Packaging and operations** — Docker/Compose, systemd, sample config, backups, a licence | "docker compose up" is the 2025–2026 expectation; the empty `deploy/` directory is the deliverable's biggest non-code hole |
| 13 | **Rate limiting, TLS guidance, hardening** | Self-acknowledged gap in `SPECIFICATION.md` §9.4 |

### 2.2 The features users would switch for

These are the reasons people move between servers, or stay put and complain. Each is a real,
documented incumbent capability.

| Feature | State of the art | Astraeus |
| --- | --- | --- |
| **Skip intro / skip credits** | Plex: audio-fingerprint intro detection per season and credit detection, Plex Pass on both server and player, with cloud-shared markers ([skip content](https://support.plex.tv/articles/skip-content/), [credits detection](https://support.plex.tv/articles/credits-detection/)). Jellyfin: "media segments" since 10.10 with provider plugins ([docs](https://jellyfin.org/docs/general/server/metadata/media-segments/)) | absent |
| **Real adaptive bitrate / auto quality** | Plex auto-adjusts quality between 192 kbps and 20 Mbps based on measured bandwidth ([auto quality](https://support.plex.tv/articles/115007570148-automatically-adjust-quality-when-streaming/)) and adds proactive quality suggestions ([quality suggestions](https://support.plex.tv/articles/quality-suggestions/)). Jellyfin still has **no** bitrate ladder — it is an open feature request ([features.jellyfin.org](https://features.jellyfin.org/posts/3680/bitrate-ladder-support)) and Android's "Auto" quality is reported broken ([jellyfin-android#1214](https://github.com/jellyfin/jellyfin-android/issues/1214)) | absent — and `max_bitrate_kbps` is ignored |
| **Dolby Vision Profile 7 with client-side HDR fallback** | Plex direct-plays the original MKV and lets ExoPlayer fall back to HDR10, preserving TrueHD; Jellyfin regressed in 10.11 and forces remux/transcode, breaking lossless audio ([jellyfin-androidtv#5303](https://github.com/jellyfin/jellyfin-androidtv/issues/5303), [jellyfin#15692](https://github.com/jellyfin/jellyfin/issues/15692), [jellyfin-web#7231](https://github.com/jellyfin/jellyfin-web/issues/7231)) | not detected at all |
| **Offline downloads / sync** | Plex Pass "Downloads"; Jellyfin's official mobile apps still lack offline playback, which is why Infuse and third-party clients exist ([Firecore](https://support.firecore.com/hc/en-us/articles/360006462093-Streaming-from-Plex-Emby-and-Jellyfin)) | absent |
| **Sharing with friends / remote access for non-technical people** | Plex: plex.tv account, Relay, automatic remote access; since 2025-04-29 remote playback needs Plex Pass or Remote Watch Pass ([Plex 2025 update](https://www.plex.tv/blog/important-2025-plex-updates/), [requirements](https://support.plex.tv/articles/requirements-for-remote-playback-of-personal-media/)). Jellyfin has no relay and expects a reverse proxy or VPN ([networking](https://jellyfin.org/docs/general/post-install/networking/), [reverse proxy](https://jellyfin.org/docs/general/post-install/networking/reverse-proxy/)) | absent — though the gate's Tailscale/Cloudflare trust model is a credible *technical* answer for a technical user |
| **Parental controls / per-user libraries** | Jellyfin: per-library access, max parental rating, tag allow/block, access schedules ([managing users](https://jellyfin.org/docs/general/server/users/adding-managing-users/)). Emby free tier includes parental controls ([Premiere matrix](https://emby.media/support/articles/Premiere-Feature-Matrix.html)) | impossible without users |
| **Live TV & DVR** | Jellyfin supports HDHomeRun, M3U and XMLTV with recordings ([setup guide](https://jellyfin.org/docs/general/server/live-tv/setup-guide/)); Plex and Emby require their paid tiers | absent |
| **Hardware acceleration breadth and quality** | Jellyfin supports QSV, NVENC, AMF, VAAPI, VideoToolbox and RKMPP, with HW tone mapping and 3D-LUT tone mapping on Intel/Rockchip ([HWA docs](https://jellyfin.org/docs/general/post-install/transcoding/hardware-acceleration/)). Plex does NVENC/QSV/AMD | QSV and VAAPI only |
| **Plugin ecosystem** | Jellyfin's plugin catalogue is a real differentiator (OpenSubtitles, lyrics, books, metadata providers). Its plugin API is also churning — DB access is "HIGHLY experimental" as of 10.11, targeting stability in 10.12 ([release notes](https://jellyfin.org/posts/jellyfin-release-10.11.0/), [catalog fix](https://github.com/jellyfin/jellyfin/pull/16724)) | none |
| **SyncPlay (watch together)** | Jellyfin: server maintains group state, clients sync against server time ([implementation](https://github.com/jellyfin/jellyfin/blob/9239b121/Emby.Server.Implementations/SyncPlay/Group.cs), [SDK API](https://kotlin-sdk.jellyfin.org/dokka/jellyfin-api/org.jellyfin.sdk.api.operations/-sync-play-api/index.html)) | absent |
| **Music depth: Plexamp, lyrics, sonic analysis** | Plexamp's sonic analysis, radio and lyrics are Plex Pass features ([Plexamp](https://www.plex.tv/plexamp/)); Jellyfin gets lyrics via plugins ([lyrics plugin](https://github.com/Felitendo/jellyfin-plugin-lyrics)) and has no equivalent of Plexamp ([comparison](https://selfhosting.sh/compare/jellyfin-vs-plex-music/)) | absent |
| **Subtitle acquisition** | Jellyfin official OpenSubtitles plugin ([docs](https://jellyfin.org/docs/general/server/plugins/open-subtitles/)) | absent |
| **Acquisition-ecosystem interop** | The self-hosting stack expects Sonarr/Radarr → hardlinked library → server, with Seerr/Overseerr for requests ([Seerr](https://github.com/seerr-team/seerr), [arr stack setup](https://github.com/Pharkie/ultimate-arr-stack/blob/main/docs/SETUP.md)). Interop is mostly about naming conventions and being scanned reliably | partially compatible already (movies/shows layout); no request integration, no naming-configurability story |

### 2.3 Differentiators worth doubling down on

These are the things Astraeus does *better* than an incumbent, or could own outright:

1. **Negotiation transparency.** Every decision carries the reason and the concrete target
   (`target_height`, `target_audio_channels`). Jellyfin's equivalent debates are long GitHub
   threads about *why* something remuxed ([example](https://github.com/jellyfin/jellyfin-web/issues/7231)).
   "Tell the user exactly why" is a real, defensible product principle.
2. **Startup-verified hardware encoders.** The refusal to trust `ffmpeg -encoders` or a populated
   `/dev/dri` addresses a genuine, recurring class of failure — Jellyfin maintains a whole page of
   [known HWA issues](https://jellyfin.org/docs/general/post-install/transcoding/hardware-acceleration/known-issues/)
   and Plex users chase QSV artefacts for generations ([Plex forum](https://forums.plex.tv/t/intel-quick-sync-transcode-quality-issues-on-intel-12th-13th-gen-cpus/832304)).
   This is a marketing-grade differentiator hiding in a startup probe.
3. **Honesty as UX.** The "Growing · seekable to X" indicator and the refusal to mark an entity
   complete without metadata are small things that build trust. Most media servers lie to their UI.
4. **Operator-grade observability.** A KPI registry with first-segment latency, transcode startup
   and stream error rate, in Prometheus form, in a media server. Nobody in this space ships this;
   it is the natural wedge into the homelab/operator audience.
5. **Identity-aware access without a user database.** `proxy` mode's trusted-CIDR check is
   *correct* (Jellyfin requires "Known Proxies" for the same reason, [reverse proxy docs](https://jellyfin.org/docs/general/post-install/networking/reverse-proxy/)),
   and it maps directly onto the Tailscale/Cloudflare pattern the self-hosting community actually
   uses. Combined with no telemetry, no account and no paywall, it matches the 2025–2026 mood:
   Plex's April 2025 pricing and remote-play changes triggered visible churn
   ([Plex](https://www.plex.tv/blog/important-2025-plex-updates/), [forum reaction](https://forums.plex.tv/t/anybody-else-switching-to-jellyfin/931059)).
6. **A single pure-Go binary that needs no DB server and no cgo.** Genuinely pleasant to deploy.
7. **The media engine as a library.** `streaming` is a clean, testable package. Almost nobody ships
   "capability negotiation + HLS session management" as a reusable Go component.

### 2.4 What the incumbents are actually bad at (the openings)

The user complaints are real and documented, and several are exactly where Astraeus is already
strong — but note that most complaints are about *clients*, not the server.

- **Jellyfin library scans regressed badly in 10.11.** The project's own issue tracker has users
  reporting scans going from 2.5 minutes to 30 minutes, or 17 hours on a 2M-file library, with
  SQLite "database table is locked" errors and thread-pool starvation
  ([jellyfin#15070](https://github.com/jellyfin/jellyfin/issues/15070)); forum reports of 2–3 day
  scans ([forum](https://forum.jellyfin.org/showthread.php?mode=linear&pid=66259&tid=14392)).
  The project's answer is "expected, the scanning method changed". A scanner that is *fast and
  predictable* is a legitimate wedge.
- **Transcoding/colour quality.** Bad tone-mapping colours are a live class of bug even upstream
  ([jellyfin-ffmpeg#467](https://github.com/jellyfin/jellyfin-ffmpeg/issues/467)), and DV
  regressions broke direct play for Shield/Tizen users in 10.11
  ([jellyfin#15692](https://github.com/jellyfin/jellyfin/issues/15692),
  [jellyfin-web#7231](https://github.com/jellyfin/jellyfin-web/issues/7231)).
- **Subtitle rendering.** ASS styling, embedded fonts and subtitle timing have been complained
  about for years ([jellyfin#1589](https://github.com/jellyfin/jellyfin/issues/1589)).
- **Client stability.** Third-party clients exist *because* official ones have gaps; Findroid
  crashes on unclassified seasons ([findroid#1107](https://github.com/jarnedemeulemeester/findroid/issues/1107)),
  Streamyfin crashes on large season downloads ([streamyfin#1234](https://github.com/streamyfin/streamyfin/issues/1234)),
  and offline playback is the most-cited missing feature across reviews
  ([Android Authority](https://www.androidauthority.com/jellyfin-vs-plex-home-server-3360937/),
  [selfhosting.sh](https://selfhosting.sh/compare/plex-vs-jellyfin/)).
- **Metadata matching** is a chronic annoyance on both sides: Jellyfin users hand-curate names and
  IDs ([naming docs](https://jellyfin.org/docs/general/server/media/shows)), and Plex's aggregated
  agent needs days to ingest new titles ([Plex forum](https://forums.plex.tv/t/plex-movie-agent-wont-match-item-thats-in-imdb-tvdb-and-tmdb/932682)).
  Neither is easy to beat, but both are beatable on *diagnosability* (say why a match failed).
- **Remote access / sharing with non-technical friends** is Jellyfin's most-cited structural
  weakness and Plex's most-cited strength ([Android Authority](https://www.androidauthority.com/jellyfin-vs-plex-home-server-3360937/)).

**Caveats.** I could not find a rigorous, primary measure of installed base. Aggregators report
Jellyfin at 51.2% of *self-hosters* in a 2024 r/selfhosted survey (n≈2,181) versus Plex at ~37%,
and Plex at ~25M monthly active users overall ([commandlinux.com](https://commandlinux.com/statistics/media-server-os-statistics-plex-jellyfin-deployment-on-linux/),
[jellywatch.app](https://jellywatch.app/blog/jellyfin-surpasses-plex-self-hosting-market-share-2026)).
Both pages are SEO-flavoured secondary sources citing a Reddit survey: treat the numbers as
directionally indicative, not authoritative. The claim that Plex deprecated and effectively killed
its plugin system also comes from a secondary comparison
([selfhosting.sh](https://selfhosting.sh/compare/plex-vs-jellyfin/)) and I did not verify it
against Plex's own documentation.

---

## 3. "Jellyfin killer" verdict

**It is not one, it is not close, and the server is the wrong place to fight.**

Three blunt reasons:

1. **Jellyfin's value is its ecosystem, not its server.** Official apps for Android, Android TV,
   iOS, tvOS, Roku, webOS, Tizen, Kodi and desktop, plus a Kotlin SDK, a TypeScript SDK and an
   OpenAPI spec that lets *other* people build Findroid, Streamyfin and Infuse against it
   ([clients](https://jellyfin.org/downloads/clients/), [SDKs](https://typescript-sdk.jellyfin.org/)).
   Astraeus has a web page. That difference is not a feature gap; it is six to eight separate
   codebases, each needing platform-specific player integration (ExoPlayer/AVPlayer/media3),
   passthrough handling and store presence. For one developer that is years, and it is the *bulk*
   of what makes Jellyfin valuable. A perfect server with no TV client loses to a mediocre server
   with one.
2. **Astraeus is a day old.** Seven commits. The incumbents have a decade of accumulated
   container, codec, anamorphic, interlacing and HDR-profile edge cases — the exact long tail that
   makes playback "just work". `TODO.md` is admirably honest about this, but it also means the
   comparison is not like-for-like yet.
3. **Jellyfin is free and, among self-hosters, popular.** The complaint-driven opening is real,
   but "better than Jellyfin" is a hard pitch to people who are already satisfied and paying
   nothing. The wedge is not general superiority; it is a specific, painful, recurring failure the
   incumbents have normalised.

### What "killer" would actually require

- **A client strategy**, which is the dominant cost. Minimum credible set: Android TV/Fire TV,
  Apple TV/iOS, Roku, and Android/iOS mobile. Anything less and the family cannot watch on the TV.
- **Accounts and per-user state**, because "continue watching" and profiles are what make a server
  feel like a product.
- **Metadata depth and library breadth** (episodes, cast, music, photos, books, NFO, TVDB/AniDB).
- **Codec correctness**: HDR/DV, tone mapping, multi-audio, subtitle burn-in/image subs,
  passthrough. This is where enthusiast trust is won or lost.
- **A plugin/extension surface** so the community multiplies the work — which Jellyfin has and is
  itself still churning ([10.11 plugin notes](https://jellyfin.org/posts/jellyfin-release-10.11.0/)).
- **Packaging and remote access** that a non-technical family member can survive.

### The narrow niches this project could plausibly win

Be realistic: none of these is "a Jellyfin killer". All of them are honest positions.

- **A) The Jellyfin-API-compatible server (the only credible route to "killer").** Implement
  Jellyfin's client-facing API subset — auth, `/Users`, `/Items`, `/Videos/{id}/stream`,
  `PlaybackInfo`, session reporting — well enough that Jellyfin's own clients and the third-party
  ecosystem (Findroid, Streamyfin, Infuse, Swiftfin, Jellyfin Media Player, Jellyfin for Kodi)
  connect unchanged. Then compete purely on server quality: scan speed, tone-mapped transcodes,
  verified hardware encoders, observability. This is still a large project, but it is *bounded*,
  it avoids writing eight clients, and it turns the project's existing strengths (honest
  negotiation, fast scanning, metrics) into the actual product. It is the single highest-leverage
  idea in this report. The risk is real too: you inherit an API that upstream changes
  ([PlaybackPositionTicks moved twice in two PRs](https://github.com/jellyfin/jellyfin/pull/17327)),
  and you are permanently a second implementation of someone else's spec.
- **B) The operator-grade media engine.** Position on measurement and determinism, not features:
  Prometheus KPIs, verified hardware encoders, per-decision reasons, fast idempotent scans,
  single pure-Go binary, Tailscale-first identity. Audience: homelab operators who already run
  Grafana and who write the recommendations everyone else follows. Small, but *reachable* and
  genuinely differentiated today.
- **C) An embeddable Go media engine (library, not product).** Package `streaming` as reusable
  capability negotiation + HLS session management for other Go projects (NAS vendors, home-grown
  apps, IoT/media gateways). Honest, low-glamour, and it plays to the code that is already clean
  and tested.
- **D) Browser-first media server.** Zero install, works on any device with a browser, with the
  browser treated as a first-class client (HLS, WebVTT, tone-mapped SDR, honest growing seek
  range). Defensible for the "watch on the laptop/phone browser" case and for environments where
  installing apps is impossible. But be honest that the browser is a *second-class* A/V client:
  no TrueHD/Atmos passthrough, no Dolby Vision, stereo at best — and Jellyfin already ships a web
  client. This niche is a foothold, not a victory.

**Recommended position:** ship B+C now as the honest identity ("the observable, honest media
engine"), and treat A as the deliberate, multi-year bet that is the only realistic path to
displacing Jellyfin. Do not position against Jellyfin's feature list; you will lose on clients and
metadata breadth for years.

---

## 4. Five concrete, high-leverage next features

Ordered by leverage, not by ease. Effort is in single-developer weeks: **S** ≈ 1, **M** ≈ 2–4,
**L** ≈ 6+.

### 1. Per-user watch state and resume — **Server-side** (tiny client change) — **S–M**

- **Benefit.** Turns a viewer into a returning viewer: "Continue watching", resume across devices,
  played/unplayed state, and the foundation for favourites and parental controls. This is the most
  visible single difference between a demo and a media server.
- **What it needs.** A minimal identity concept (even one implicit user, later N), a `user_data`
  table keyed `(user, entity)` storing position in ticks, played flag, last-played timestamp and
  play count; a report endpoint the player calls on pause/stop; and the player reading it back.
- **Why it is cheap here.** The playback API *already* accepts `start_seconds` and already restarts
  ffmpeg at an offset (`BuildFFmpegArgsAt`). Only the persistence and the client wiring are
  missing — the hard part is done.

### 2. Full audio-track and subtitle-rendition handling — **Server-side + client-side** — **M**

- **Benefit.** Dual-language, commentary tracks and Blu-ray subtitles start working. Today the
  first audio stream always wins and PGS/VobSub is refused with `415`, which excludes most remuxes.
- **What it needs.** Probe already returns every audio and subtitle track — surface audio tracks in
  the playback response the way subtitle tracks already are; add a track parameter; pass
  `-map 0:a:<n>`; add burn-in (`-filter_complex subtitles=...`) or an HLS subtitle rendition for
  image/styled subs, plus a UI selector (the subtitle selector pattern already exists to copy).
- **Effort driver.** Burn-in needs a re-encode and interacts with the ABR work; image subs need a
  bitmap overlay or OCR. A track-selection-only first cut is much smaller.

### 3. HDR/Dolby Vision-aware transcode path with tone mapping — **Server-side** — **M–L**

- **Benefit.** Fixes visibly wrong output. Right now a 10-bit BT.2020/PQ source is transcoded to
  8-bit SDR by naive conversion, producing the washed-out or darkened picture that is the single
  most common HDR complaint against every server in this space.
- **What it needs.** Parse `color_transfer`, `color_primaries`, `color_space`, `side_data_list`
  (mastering display, content light level) and Dolby Vision fields in `ffprobe`; detect HDR10/HLG/
  DV P5/P7/P8; when transcoding HDR → SDR insert a tone-mapping filter chain (software
  `zscale`+`tonemap`, or `tonemap_opencl`/QSV VPP when the verified hardware path allows); set
  output colour metadata explicitly; and record DV/HDR in `MediaInfo` so negotiation can prefer
  direct play with client-side fallback — the behaviour Plex gets right and Jellyfin regressed on
  ([jellyfin-androidtv#5303](https://github.com/jellyfin/jellyfin-androidtv/issues/5303)).
- **Caveat.** Getting tone mapping *right* is harder than adding a filter; this is where the
  incumbents still have open bugs ([jellyfin-ffmpeg#467](https://github.com/jellyfin/jellyfin-ffmpeg/issues/467)).
  Ship it behind the existing per-decision reason so failures are visible.

### 4. A real ABR ladder, and honour `max_bitrate_kbps` — **Server-side** (small client change) — **M–L**

- **Benefit.** "Auto quality" that actually adapts, plus the existing (currently cosmetic) bitrate
  cap finally doing something. This is a headline Plex feature and a long-standing Jellyfin gap
  ([feature request](https://features.jellyfin.org/posts/3680/bitrate-ladder-support),
  [Plex auto quality](https://support.plex.tv/articles/115007570148-automatically-adjust-quality-when-streaming/)).
- **What it needs.** Emit a master playlist with 2–3 variants (e.g. source-height, 720p, 480p) at
  distinct bitrates; segment each variant with aligned keyframes (the code already forces key
  frames on the segment boundary); make the decision pick a ladder from `max_bitrate_kbps` and the
  client's declared limits instead of ignoring them; expose a quality selector in the UI. hls.js is
  already vendored and switches between variants natively.
- **Effort driver.** Multiple ffmpeg outputs in one process, bandwidth/CPU cost control, and
  deciding when *not* to build a ladder (direct play, remux).

### 5. A Jellyfin-compatible API surface — **Server-side** — **L**

- **Benefit.** Instantly usable by Jellyfin's official clients and the whole third-party ecosystem
  (Findroid, Streamyfin, Infuse, Swiftfin, Kodi, Jellyfin Media Player) with no client work. This
  is the difference between "another web-only server" and "a server people can actually watch on
  their TV tonight" — and it is the only route to the word "killer" that does not require writing
  six clients.
- **What it needs.** An auth/session layer, user and item endpoints in Jellyfin's shapes, a
  `PlaybackInfo` response that maps Jellyfin's device-profile model onto the existing
  `ClientCapability` negotiator, and session/playback reporting (`/Sessions/Playing*`) so clients
  show state correctly. Middleware can translate between the internal API and the compatible
  surface, keeping the domain clean.
- **Risks.** You inherit upstream API churn (Jellyfin is actively reshaping user-data and
  audio-ABI surfaces, e.g. [plugin DB access is experimental](https://jellyfin.org/posts/jellyfin-release-10.11.0/)),
  and the compatible surface will always lag. Scope it to the client-facing subset, not the admin
  API.
- **Prerequisite, not a feature:** before or alongside this, add a `Dockerfile` +
  `docker-compose.yml`, a systemd unit, a sample config, a `LICENSE`, and tagged releases —
  otherwise none of the above reaches anyone.

---

## Appendix — sources

Jellyfin: [10.11 release](https://jellyfin.org/posts/jellyfin-release-10.11.0/) ·
[10.11.0 tag notes](https://github.com/jellyfin/jellyfin/releases/tag/v10.11.0) ·
[clients](https://jellyfin.org/downloads/clients/) ·
[client docs](https://github.com/jellyfin/jellyfin-docs/blob/master/general/clients/index.md) ·
[codec support](https://jellyfin.org/docs/general/clients/codec-support/) ·
[hardware acceleration](https://jellyfin.org/docs/general/post-install/transcoding/hardware-acceleration/) ·
[known HWA issues](https://jellyfin.org/docs/general/post-install/transcoding/hardware-acceleration/known-issues/) ·
[metadata](https://jellyfin.org/docs/general/server/metadata/) ·
[media segments](https://jellyfin.org/docs/general/server/metadata/media-segments/) ·
[users](https://jellyfin.org/docs/general/server/users/) ·
[managing users](https://jellyfin.org/docs/general/server/users/adding-managing-users/) ·
[music](https://jellyfin.org/docs/general/server/media/music/) ·
[shows naming](https://jellyfin.org/docs/general/server/media/shows) ·
[live TV](https://jellyfin.org/docs/general/server/live-tv/setup-guide/) ·
[networking](https://jellyfin.org/docs/general/post-install/networking/) ·
[reverse proxy](https://jellyfin.org/docs/general/post-install/networking/reverse-proxy/) ·
[OpenSubtitles plugin](https://jellyfin.org/docs/general/server/plugins/open-subtitles/) ·
[lyrics plugin](https://github.com/Felitendo/jellyfin-plugin-lyrics) ·
[Bookshelf plugin](https://codeberg.org/bfordham/jellyfin-plugin-bookshelf) ·
[trickplay PR](https://github.com/jellyfin/jellyfin/pull/9554) ·
[scan regression #15070](https://github.com/jellyfin/jellyfin/issues/15070) ·
[scan forum thread](https://forum.jellyfin.org/showthread.php?mode=linear&pid=66259&tid=14392) ·
[bitrate ladder request](https://features.jellyfin.org/posts/3680/bitrate-ladder-support) ·
[auto quality bug](https://github.com/jellyfin/jellyfin-android/issues/1214) ·
[DV #15692](https://github.com/jellyfin/jellyfin/issues/15692) ·
[DV web #7231](https://github.com/jellyfin/jellyfin-web/issues/7231) ·
[DV AndroidTV #5303](https://github.com/jellyfin/jellyfin-androidtv/issues/5303) ·
[tone-map colours](https://github.com/jellyfin/jellyfin-ffmpeg/issues/467) ·
[subtitle fonts #1589](https://github.com/jellyfin/jellyfin/issues/1589) ·
[SyncPlay](https://github.com/jellyfin/jellyfin/blob/9239b121/Emby.Server.Implementations/SyncPlay/Group.cs) ·
[user data PR](https://github.com/jellyfin/jellyfin/pull/10573) ·
[API overview](https://jellyfin-jellyfin.mintlify.app/development/api-overview) ·
[Kotlin SDK](https://kotlin-sdk.jellyfin.org/) ·
[TypeScript SDK](https://typescript-sdk.jellyfin.org/) ·
[plugin catalog PR](https://github.com/jellyfin/jellyfin/pull/16724)

Plex: [2025 update](https://www.plex.tv/blog/important-2025-plex-updates/) ·
[remote playback requirements](https://support.plex.tv/articles/requirements-for-remote-playback-of-personal-media/) ·
[Plex Pass overview](https://support.plex.tv/articles/201751006-plex-pass-feature-overview/) ·
[skip content](https://support.plex.tv/articles/skip-content/) ·
[credits detection](https://support.plex.tv/articles/credits-detection/) ·
[auto quality](https://support.plex.tv/articles/115007570148-automatically-adjust-quality-when-streaming/) ·
[quality suggestions](https://support.plex.tv/articles/quality-suggestions/) ·
[metadata agents](https://support.plex.tv/articles/200241558-agents/) ·
[Plexamp](https://www.plex.tv/plexamp/) ·
[remote access](https://support.plex.tv/articles/200931138-troubleshooting-remote-access/) ·
[QSV quality thread](https://forums.plex.tv/t/intel-quick-sync-transcode-quality-issues-on-intel-12th-13th-gen-cpus/832304) ·
[agent ingest delay](https://forums.plex.tv/t/plex-movie-agent-wont-match-item-thats-in-imdb-tvdb-and-tmdb/932682) ·
[user churn thread](https://forums.plex.tv/t/anybody-else-switching-to-jellyfin/931059)

Adjacent: [Emby Premiere matrix](https://emby.media/support/articles/Premiere-Feature-Matrix.html) ·
[Infuse](https://firecore.com/infuse) ·
[Infuse server integration](https://support.firecore.com/hc/en-us/articles/360006462093-Streaming-from-Plex-Emby-and-Jellyfin) ·
[Seerr](https://github.com/seerr-team/seerr) · [Seerr docs](https://docs.seerr.dev/) ·
[arr stack setup](https://github.com/Pharkie/ultimate-arr-stack/blob/main/docs/SETUP.md) ·
[findroid #1107](https://github.com/jarnedemeulemeester/findroid/issues/1107) ·
[streamyfin #1234](https://github.com/streamyfin/streamyfin/issues/1234)

Comparisons and market (secondary sources, lower confidence):
[Android Authority](https://www.androidauthority.com/jellyfin-vs-plex-home-server-3360937/) ·
[selfhosting.sh Plex vs Jellyfin](https://selfhosting.sh/compare/plex-vs-jellyfin/) ·
[selfhosting.sh music](https://selfhosting.sh/compare/jellyfin-vs-plex-music/) ·
[homelabsec](https://homelabsec.com/posts/jellyfin-vs-plex-vs-emby/) ·
[market stats aggregator](https://commandlinux.com/statistics/media-server-os-statistics-plex-jellyfin-deployment-on-linux/) ·
[jellywatch](https://jellywatch.app/blog/jellyfin-surpasses-plex-self-hosting-market-share-2026)
