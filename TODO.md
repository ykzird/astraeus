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
- [x] Dynamic range: classified from the source's transfer function (PQ / HLG),
      reported with the Dolby Vision profile, tone mapped to SDR for clients that
      cannot show HDR, and passed through at 10 bits for those that can
- [x] 10-bit HDR encoder support verified by a second startup probe per encoder,
      separate from the 8-bit one because an encoder that works at 8 may refuse 10
- [x] Multi-audio-track selection: every track probed and listed, chosen by
      stream index, the file's `default` track delivered when the client does not
      choose, and every audio decision (codec, channels, bitrate share) made about
      the chosen track. A chosen track repackages rather than direct-plays
- [x] Image-based subtitles (PGS, VobSub) delivered by burning the bitmap into
      the picture: `burn_subtitle_index` names the stream, the decision forces a
      single-rendition transcode, and the composite is applied after the colour
      chain so a tone map cannot wash the subtitle out. Verified at the pixel
      level by a Go integration test (a generated PGS fixture, burned versus not)
      and in a browser (10/10 harness checks). OCR — text instead of a picture —
      remains open; see "Not covered yet"
- [x] `max_bitrate_kbps` acted on rather than echoed: the audio's share is
      reserved and the video held to the remainder as a VBV ceiling, uniformly
      across encoder families. Verified end to end — the same 9 Mbps source came
      out at 3.4 Mbps unlimited and 632 kbps under a 500 kbps limit
- [x] Bitrate-aware ABR ladder: up to three rungs with per-rung ceilings in one
      ffmpeg process, a master playlist the client is pointed at, and per-stream
      specifiers so each rung really is its own size. A client that pins a height
      still gets a single rendition
- [ ] Dolby Vision profile 5 *correct* conversion (IPTPQc2 needs a Dolby Vision
      tone mapper; the software chain produces approximate colour and says so)

## Phase 3: API & Security — API and hardening complete; rate limiting and tracing are not

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
- [x] Resumable playback over the API: `PUT`/`DELETE /api/entities/{id}/progress`,
      the stored position on the entity detail, and `GET /api/progress` for what is
      worth continuing
- [x] Progress is per viewer: keyed on the identity the access gate attaches to
      the request, so two viewers of one film keep separate places, listings are
      scoped to the caller, and a pre-existing database keeps its rows under the
      named local viewer
- [x] Security headers and a content security policy with no `unsafe-inline` or
      `unsafe-eval`, driven by what the UI actually needs; verified in a browser
      (third-party image and inline script refused, 32/32 harness checks pass)
- [x] Artwork is loaded only through the server's own proxy; the metadata
      provider's absolute URLs are no longer fetched by the browser, and
      `img-src 'self'` makes that structural rather than a convention
- [x] Rate limiting: a token bucket per client in front of `/api/` only, keyed by
      the gate's identity when there is one and the peer address otherwise, off by
      default (`--rate-limit`), answering `429` with a `Retry-After` and counted in
      `astraeus_rate_limited_total`. Per process, so several servers behind one
      proxy limit as a sum
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
      unless the server marks a default; image-based tracks offered as "burned in"
      (a real re-negotiation, labelled with its cost); the choice survives a
      quality change or a seek
- [x] Transport overlaid on the video rather than in the sidebar, with fullscreen on
      the player container, volume and mute (remembered), and a quality menu that
      re-negotiates at the current position
- [x] Controls that fade while playing and return on interaction, never hiding while
      paused, while focused, or while the pointer rests on them
- [ ] Subtitle appearance controls (size, colour, background)
- [x] Poster/backdrop artwork: the UI renders `poster_url`/`backdrop_url`, which
      are this server's own proxy, and never the metadata provider's absolute URL
      — `img-src 'self'` makes that structural. With no proxy configured the
      generated gradient stands in, which leaks nothing
- [x] Resume and continue watching in the player: Play resumes at the stored
      position, the position is reported while watching and cleared when the
      viewer starts over or finishes, and the navigation lists what is worth
      continuing

## Phase 5: Packaging — the pieces exist, nothing is released

- [x] Multi-stage `Dockerfile`: static binary, ffmpeg and ffprobe in the runtime
      image, fixed non-root uid, one writable volume, health check
- [x] Verified by running it, not by building it: the image serves `/api/health`,
      scans a mounted library, and delivered all three paths — direct play, HDR
      tone mapped to 8-bit `bt709`, HDR remuxed and re-encoded at 10-bit
      `bt2020`/`smpte2084` — using the image's own ffmpeg 5.1.9
- [x] Hardened systemd unit (`deploy/astraeus.service`): loopback, token auth
      required, `ProtectSystem=strict` with two writable trees, empty capability
      set, syscall filter; `systemd-analyze verify` clean and
      `systemd-analyze security` scoring 1.6 (OK)
- [x] Deployment runbook (`deploy/README.md`) covering both shapes, backups,
      upgrades, GPU passthrough and what is deliberately not hardened
- [x] CI workflow (`.github/workflows/ci.yml`): format, vet, build, unit tests,
      integration tests, `node --check`, image build and a health wait
- [ ] Release automation: nothing is tagged, versioned or published anywhere
- [ ] A TLS reverse-proxy example (Caddy or nginx) beside the systemd unit
- [ ] Package the VAAPI userspace drivers into the image, so GPU transcoding
      works in a container without extra packages

## Known gaps

- A ladder is built only when the client omits `max_height`. A player that pins a
  height — a viewer choosing a setting from the quality menu — gets exactly one
  rendition, so the menu caps quality rather than expressing a preference within a
  ladder. Letting a client pin a *top* rung while still adapting below it would be
  the more capable design.
- A ladder multiplies the work: each rung is a separate encode of the same source,
  so three rungs cost roughly three times one. There is no per-host cap on how many
  rungs to offer, and no use of the client's bandwidth estimate to skip rungs it
  could not afford anyway.
- A VBV ceiling bounds the average rather than every instant, so over a segment
  shorter than the buffer the delivered rate can exceed the client's limit. That
  is standard rate-control behaviour, not a bug, but it is a limit on how literal
  the guarantee is.
- Seeking is no longer bounded by how far the transcoder has got: the client
  re-negotiates with `start_seconds` and ffmpeg seeks the input, so any point
  in the film is reachable in a couple of seconds. The landing point is
  keyframe-aligned, so it can be a second or two early.
- Playback position is stored per viewer, resumed on play, and listed as a
  **Continue watching** section. The viewer is whatever identity the access gate
  attached to the request, so the guarantee is only as strong as the gate: in
  `token` mode every API client is reported as `token` and shares one place, and
  with the gate disabled there is a single `local` viewer. Rows written before
  the key was widened are kept under `local`, because who wrote them was never
  recorded.
- HDR is handled, with caveats that are all reported in the negotiation reasons
  rather than hidden:
  - **Dolby Vision profile 5** stores IPTPQc2, not PQ. Tone mapping it with the
    software chain gives approximate colour — better than refusing to play the
    file, but not correct. Doing it properly needs libplacebo (which requires
    Vulkan) or the Dolby Vision tooling. No profile 5 sample exists on this host,
    so even the approximation is unverified; it is reported and left at that.
  - **Re-encoding loses Dolby Vision dynamic metadata.** Profile 8's HDR10 base
    layer survives, and the reasons say the dynamic metadata does not. A
    direct play or remux keeps everything.
  - **Mastering-display and content-light (MaxCLL/MaxFALL) metadata are not
    carried through a re-encode.** They are neither read from the source nor
    written to the output.
  - **HLG is implemented but unverified.** It is classified and tagged
    (`arib-std-b67`), but no HLG sample exists here to tone map or pass through.
  - **The tone-map chain needs a genuinely HDR-tagged source.** `zscale` reports
    "no path between colorspaces" on an SDR input, so a file that claims PQ but
    is not would fail the session rather than playing badly. That is a deliberate
    choice - a failed session is more honest than a wrong picture - but the
    failure surfaces as `500 stream_start_failed`, not as a clear explanation of
    the mis-tagging.
  - **Hardware HDR encoding is unverified**, in the same way the hardware
    encoders are: the 10-bit probe runs on whatever host starts the server, but
    no NVIDIA, Intel or AMD GPU is reachable here. On this machine the probe
    verified five software encoders at 10-bit.
- Image-based subtitles are burned into the picture rather than converted to
  text: the subtitle is exact, but it needs a re-encode, cannot be toggled
  without one, and cannot be searched or restyled. OCR (tesseract) would deliver
  text that survives all three, at the cost of a runtime dependency and OCR
  errors — neither is implemented.
- The burn-in path is verified for **PGS** and only for software encoders. VobSub
  shares the track classification and the same overlay path, but no VobSub sample
  exists on this host, so it is reasoned about rather than observed; a hardware
  encoder's upload filter has never been combined with the burn graph.
- Browser clients are capped at 1080p, 8-bit and stereo by default, and do not
  declare HDR support, so a PQ film is tone mapped for them by design. There is
  no surround passthrough and no per-client override beyond sending a capability
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
- Packaging exists but nothing has been released, and two pieces of it are
  asserted rather than observed: the CI workflow has never run (the repository
  has no remote), and the systemd unit has been checked with `systemd-analyze`
  but never started, because the development host has no reachable systemd
  manager. The `SystemCallFilter` in that unit is the one setting that could stop
  ffmpeg on a host where an encoder needs a call outside the list; the runbook
  says how to diagnose and relax it.
- The container image ships ffmpeg's VAAPI support but not the vendor userspace
  drivers, so GPU transcoding in a container needs extra packages. NVENC needs
  the NVIDIA container runtime.
- The web UI has no unit tests. It is one large file with no module seam, so its
  logic is only covered by the CDP harnesses in `scripts/ui-verify/`, which need a
  browser and a running server.
- The content security policy has no `report-uri`: there is no collector to send
  reports to, and a policy that reports nowhere is theatre. A deployment that
  wants violation reporting has to add both ends.
- `Strict-Transport-Security` is not sent by the server. It speaks HTTP, so the
  header would mean nothing there; a TLS-terminating proxy should set it.
- Cross-origin isolation (`COOP`/`COEP`) is not configured. Nothing here needs
  it, and enabling it would break resources that are not `CORP`-tagged.
- OpenTelemetry tracing is not implemented; beyond the four specified metrics,
  the registry holds counters and histograms for HTTP, scanning, streaming
  sessions, playback decisions, probe errors, transcoder fallbacks and the
  access gate.
