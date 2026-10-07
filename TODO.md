# Astraeus Media MVP Roadmap

Status as of the current build. Evidence for each claim is the test suite
(`go test ./...`, plus `-tags=integration`) and the commands in `README.md`.

## Phase 1: Library Manager — complete

- [x] Scanner with persistence (SQLite via `sqlx`)
- [x] Idempotent scans: re-scanning an unchanged tree creates no new rows, and a
      changed file size updates the existing object
- [x] Metadata integration (TMDB over HTTP, with a synthetic provider fallback)
- [x] Chainable providers (`ChainProvider`) so TVDB/IMDB can be added behind TMDB
- [x] Entity hierarchy: Series / Season / Episode derived from the directory
      layout, scoped so two series can each own a "Season 1"
- [x] Entity status lifecycle: `Incomplete` until a `MetadataSet` is attached
- [x] Background enrichment worker with an interval and a manual trigger
- [x] Periodic library scanning on an interval (`--scan-interval`) with the CLI
      and `POST /api/scan` as the manual override
- [x] Artwork: TMDB posters and backdrops proxied through the server and cached
- [x] Migration path for databases written by the earlier prototype
- [x] Movie and show libraries (`--kind movies|shows`)

## Phase 2: Media Engine — complete for MVP scope

- [x] Client capability model and validation
- [x] ffprobe-based media inspection, cached per file
- [x] Proactive negotiation: direct play / remux / transcode, with per-decision
      reasons
- [x] Resolution-aware downscale negotiation
- [x] HLS segmented delivery via ffmpeg (passthrough or re-encode)
- [x] Session lifecycle: TTL reaping, graceful shutdown, cleanup
- [x] Path-traversal-safe segment serving
- [x] Direct play with HTTP range support (seeking)
- [x] Server capability detection: encoder list and QuickSync/VAAPI availability
- [x] Subtitle tracks probed (codec, language, title, default/forced, text vs
      bitmap), text tracks extracted to WebVTT on demand and cached
- [ ] Subtitle *rendering* in the player (track selection UI)
- [ ] Multi-audio-track selection
- [ ] Serving image-based subtitles (PGS/VobSub) — needs OCR or bitmap overlay
- [ ] Bitrate-aware ABR ladder (currently a single target rendition)

## Phase 3: API & Security — API complete, security outstanding

- [x] REST API over the library, scanner, metadata, playback, artwork and subtitles
- [x] Structured request logging (`slog`), graceful shutdown
- [x] Prometheus metrics endpoint with the specification's KPI registry
      declared up front: `fttt_latency` (first segment), `transcode_startup_time`,
      `metadata_latency` and `stream_error_rate`
- [x] Access gate: `proxy` mode (identity header believed only from a trusted
      address, for Tailscale / Cloudflare Access) and `token` mode (constant-time
      bearer token), with per-request identity logging and grant/deny metrics.
      Fails closed on misconfiguration.
- [ ] Rate limiting
- [ ] OpenTelemetry tracing

## Phase 4: Spatial Web UI — complete for MVP scope

- [x] Base three-column layout (navigation | canvas | context)
- [x] Media canvas with the selected entity as the visual anchor
- [x] Contextual sidebar (metadata, playback transport, reasons, queue)
- [x] Navigation sidebar (libraries, scan, enrich, incomplete filter)
- [x] Glassmorphism panels + neobrutalist interaction elements
- [x] Served by the binary from `--web-dir`, same origin as the API
- [x] Direct-play playback in the browser: real `<video>`, working seek, and
      honest reporting of the negotiation decision
- [x] Segmented playback in Chromium and Firefox via a locally vendored hls.js
      (1.7.3, lazy-loaded), verified in a real browser
- [x] Subtitle tracks rendered and selectable in the player, defaulting to Off
      unless the server marks a default; image-based tracks shown but disabled
- [ ] Subtitle appearance controls (size, colour, background)
- [ ] Poster/backdrop artwork: TMDB artwork is proxied and cached server-side;
      the UI prefers `poster_url`/`backdrop_url` and falls back to generated art

## Known gaps

- Transcoding runs one rendition per request; there is no adaptive bitrate
  ladder, and seeking on a segmented stream is bounded by how far the transcoder
  has produced (Plex and Jellyfin restart ffmpeg at an offset; we do not yet).
- Image-based subtitles (PGS, VobSub) are detected and reported but not
  delivered — that needs OCR or bitmap overlay support.
- Multi-audio-track selection is not implemented; the first audio stream wins.
- Browser clients are capped at 1080p, 8-bit and stereo by default. There is no
  surround passthrough and no per-client override beyond sending a capability
  manifest, and the cap is a constant rather than a flag.
- `--auth-mode` defaults to `none`. That is right for a trusted LAN and wrong
  for anything reachable from the internet; use `proxy` or `token` before
  exposing it.
- `--stream-root`, `--image-cache` and `--subtitle-cache` default to temporary
  directories, so caches do not survive a reboot. Stale stream directories from
  a previous run are swept at startup.
- Probe results are cached per path for the process lifetime; a file replaced
  underneath the server keeps its old technical metadata until restart.
- The hls.js fatal-error recovery path and the native-HLS (Safari) branch are
  implemented but have not been observed firing — no Safari was available, and a
  stream failure could not be forced on a live server.
- OpenTelemetry tracing is not implemented; the KPI registry beyond the four
  specified metrics is limited to HTTP, scanning and auth counters.
