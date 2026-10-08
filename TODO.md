# Astraeus Media MVP Roadmap

Status as of the current build. Evidence for each claim is the test suite
(`go test ./...`, plus `-tags=integration`) and the commands in `README.md`.

## Phase 1: Library Manager — complete

- [x] Scanner with persistence (SQLite via `sqlx`)
- [x] Idempotent scans: re-scanning an unchanged tree creates no new rows, and a
      changed file size updates the existing object
- [x] Metadata integration (TMDB over HTTP, with a synthetic provider fallback)
- [x] Chainable providers (`metadata.Chain`) so TVDB/IMDB can be added behind TMDB
- [x] Entity hierarchy: Series / Season / Episode derived from the directory
      layout, scoped so two series can each own a "Season 1"
- [x] Entity status lifecycle: `Incomplete` until a `MetadataSet` is attached
- [x] Background enrichment worker with an interval and a manual trigger
- [x] Periodic library scanning on an interval (`--scan-interval`) with the CLI
      and `POST /api/scan` as the manual override
- [x] Artwork: TMDB posters and backdrops proxied through the server and cached
- [x] Pruning: a file removed from disk stops being listed and entities left with
      nothing are removed upwards, refused outright when a scan could not read
      every path or saw no files at all
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
- [x] Server capability detection: encoders **verified by running one**, not inferred
      from the compiled-in list, plus the audio encoder list
- [x] Seeking past what the transcoder has produced re-negotiates at an offset
      (`start_seconds`), so any point in a film is reachable without restarting from zero
- [x] Concurrent stream cap (`--max-sessions`), answered with `429`
- [x] Subtitle tracks probed (codec, language, title, default/forced, text vs
      bitmap), text tracks extracted to WebVTT on demand and cached
- [x] Subtitle *rendering* in the player (track selection UI)
- [ ] Multi-audio-track selection
- [ ] Serving image-based subtitles (PGS/VobSub) — needs OCR or bitmap overlay
- [ ] Bitrate-aware ABR ladder (currently a single target rendition)

## Phase 3: API & Security — API complete, security outstanding

- [x] Capability manifests validated: an unknown codec name is refused with `400`
      before it can reach ffmpeg, and a codec this host cannot encode is a `409` with
      a reason rather than a `500` later
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
- [x] Contextual sidebar (metadata, decision reasons, produced window, queue)
- [x] Navigation sidebar (libraries, scan, enrich, incomplete filter)
- [x] Glassmorphism panels + neobrutalist interaction elements
- [x] Served by the binary from `--web-dir`, same origin as the API
- [x] Direct-play playback in the browser: real `<video>`, working seek, and
      honest reporting of the negotiation decision
- [x] Segmented playback in Chromium and Firefox via a locally vendored hls.js
      (1.7.3, lazy-loaded), verified in a real browser
- [x] Subtitle tracks rendered and selectable in the player, defaulting to Off
      unless the server marks a default; image-based tracks shown but disabled; the
      choice survives a quality change or a seek
- [x] Transport overlaid on the video rather than in the sidebar, with fullscreen on
      the player container, volume and mute (remembered), and a quality menu that
      re-negotiates at the current position
- [x] Controls that fade while playing and return on interaction, never hiding while
      paused, while focused, or while the pointer rests on them
- [ ] Subtitle appearance controls (size, colour, background)
- [ ] Poster/backdrop artwork: TMDB artwork is proxied and cached server-side
      and every entity payload carries `poster_url`/`backdrop_url`, but the UI
      does not consume them yet — it only renders an absolute
      `backdrop_path`/`poster_path` and otherwise falls back to generated art

## Known gaps

- Transcoding runs one rendition per request; there is no adaptive bitrate
  ladder, and `max_bitrate_kbps` is accepted but not yet acted on.
- Seeking is no longer bounded by how far the transcoder has got: the client
  re-negotiates with `start_seconds` and ffmpeg seeks the input, so any point
  in the film is reachable in a couple of seconds. The landing point is
  keyframe-aligned, so it can be a second or two early.
- Playback position is not stored, so there is no resume across sessions or
  devices; the offset machinery it needs now exists.
- HDR and Dolby Vision are not handled. Only bit depth is probed, and output is
  pinned to 8-bit, so a 10-bit BT.2020/PQ source is converted to SDR naively and
  looks washed out or dark. There is no tone mapping and no colour metadata, and
  a client that could handle HDR is not told the source has it.
- Image-based subtitles (PGS, VobSub) are detected and reported but not
  delivered — that needs OCR or bitmap overlay support.
- Multi-audio-track selection is not implemented; the first audio stream wins.
- Browser clients are capped at 1080p, 8-bit and stereo by default. There is no
  surround passthrough and no per-client override beyond sending a capability
  manifest, and the cap is a constant rather than a flag.
- A capability manifest that omits `max_audio_channels` or `max_bit_depth` is
  treated as unrestricted, which can hand a browser a stream it cannot decode
  (see the README). The failure surfaces as a stalled player rather than a
  clear error, so a client that guesses wrong has nothing to go on.
- `--auth-mode` defaults to `none`. That is right for a trusted LAN and wrong
  for anything reachable from the internet; use `proxy` or `token` before
  exposing it.
- `--stream-root`, `--image-cache` and `--subtitle-cache` default to temporary
  directories, so caches do not survive a reboot. Stale stream directories from
  a previous run are swept at startup.
- Probe results are cached per path for the process lifetime; a file replaced
  underneath the server keeps its old technical metadata until restart.
- NVENC, AMF and VideoToolbox are implemented but have never been run against
  real hardware: the development host is an AMD machine with none of the three
  reachable. What is on this host is covered by tests that simulate the
  detection path with a stub ffmpeg, and the startup probe validates the options
  on whatever machine actually runs it. VAAPI is in the same position - its
  `/dev/dri` is not visible here - which is how it came to be built without the
  `-vaapi_device` its upload filter needs.
- Hardware encoders are verified only at startup, and only at one resolution: a
  driver that works for 320x180 could still fail at 4K, which is what the runtime
  software fallback is for.
- The hls.js fatal-error recovery path and the native-HLS (Safari) branch are
  implemented but have not been observed firing — no Safari was available, and a
  stream failure could not be forced on a live server.
- Nothing is packaged: no `Dockerfile`, no systemd unit, no CI. There is a
  `LICENSE` (MIT) and a third-party notices file, but running this anywhere means
  building it yourself.
- The web UI has no unit tests. It is one large file with no module seam, so its
  logic is only covered by the CDP harnesses in `scripts/ui-verify/`, which need a
  browser and a running server.
- No `Content-Security-Policy` or other security headers are sent. The absence of
  any HTML-injection sink in the UI is currently the only defence, and it is a
  discipline rather than an enforced boundary.
- Artwork from a metadata provider is fetched by the browser directly when the
  entity carries an absolute URL, which tells that third party the viewer's IP.
  The server-side image proxy exists but is not used for those URLs.
- OpenTelemetry tracing is not implemented; beyond the four specified metrics,
  the registry holds counters and histograms for HTTP, scanning, streaming
  sessions, playback decisions, probe errors, transcoder fallbacks and the
  access gate.
