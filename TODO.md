# Astraeus Media MVP Roadmap

Status as of the current build. Evidence for each claim is the test suite
(`go test ./...`, plus `-tags=integration`) and the commands in
[`docs/development.md`](docs/development.md).

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
- [x] Image-based subtitles: `burn_subtitle_index` names the stream, the decision
      forces a single-rendition transcode, and the composite is applied after the
      colour chain so a tone map cannot wash the subtitle out. Verified at the
      pixel level by a Go integration test (a generated PGS fixture, burned versus
      not) and in a browser (10/10 harness checks)
- [x] **OCR for PGS image subtitles**: the PGS stream is decoded by
      `internal/subtitles` and read by `tesseract` into WebVTT, so the track is
      served like any text track and can be toggled, restyled and searched. The
      engine is an optional runtime dependency — without it the server keeps the
      old `415` refusal and the burn-in path, rather than failing — and the reader
      is PGS-only, so VobSub keeps its refusal and its burn. The generated fixture
      is now a real caption (a 5x7 bitmap font, `scripts/pgsgen -text`), and the
      integration test reads the words back through real ffmpeg and a real
      tesseract; the browser harness sees the caption on screen (10/10). The
      server must not be *worse* without the dependency: a unit test pins the
      refusal and the integration test skips when tesseract is absent
- [x] `max_bitrate_kbps` acted on rather than echoed: the audio's share is
      reserved and the video held to the remainder as a VBV ceiling, uniformly
      across encoder families. Verified end to end — the same 9 Mbps source came
      out at 3.4 Mbps unlimited and 632 kbps under a 500 kbps limit
- [x] Bitrate-aware ABR ladder: up to three rungs with per-rung ceilings in one
      ffmpeg process, a master playlist the client is pointed at, and per-stream
      specifiers so each rung really is its own size. A client that pins a height
      still gets a single rendition
- [x] **A quality choice that caps the ladder instead of pinning it**:
      `preferred_height` asks for a ladder topped there, so the player can step
      down under pressure, while `max_height` alone keeps meaning exactly one
      rendition (the deterministic, single-encode request). `max_height` beside a
      preference is the hard ceiling, so a manifest can say "my screen is 1080"
      and "I chose 720" at once. The quality menu sends the preference and labels
      its choices "Up to Np". Verified at the rung level: the integration test
      builds a 480-topped ladder and asserts each produced segment's *measured*
      height, and a unit test pins that a 4K source with a 720 preference is not
      direct-played untouched
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
- [x] OpenTelemetry tracing: opt-in OTLP/HTTP export (`--otel-endpoint`), a span
      per HTTP request that continues an incoming W3C `traceparent`, child spans
      for the playback negotiation and the streaming session it starts, and
      `trace_id`/`span_id` on the request log line. Hand-rolled encoder and
      batcher, so no SDK dependency; verified against a real collector (Jaeger
      all-in-one in a container) end to end, including the session span arriving
      as a child of its request

## Phase 4: Spatial Web UI — complete for MVP scope

- [x] Base three-column layout (navigation | canvas | context)
- [x] Media canvas with the selected entity as the visual anchor
- [x] Contextual sidebar (metadata, decision reasons, produced window, queue)
- [x] Navigation sidebar (libraries, scan, enrich, incomplete filter)
- [x] Glassmorphism panels + neobrutalist interaction elements
- [x] The front end's pure timeline maths extracted to `web/core.js` and unit
      tested with Node's own runner (`node --test web/*.test.js`), so the source↔media
      time conversion, the produced window and the clock have a seam that a
      browser is not needed to test
- [x] Served by the binary from `--web-dir`, same origin as the API
- [x] Direct-play playback in the browser: real `<video>`, working seek, and
      honest reporting of the negotiation decision
- [x] Segmented playback in Chromium and Firefox via a locally vendored hls.js
      (1.7.3, lazy-loaded), verified in a real browser
- [x] Subtitle tracks rendered and selectable in the player, defaulting to Off
      unless the server marks a default; an image track the server can read is
      offered as an ordinary track (a `<track>`, no re-encode), and one only a
      burn can show is offered as "burned in" (a real re-negotiation, labelled
      with its cost); the choice survives a quality change or a seek
- [x] Transport overlaid on the video rather than in the sidebar, with fullscreen on
      the player container, volume and mute (remembered), and a quality menu that
      re-negotiates at the current position
- [x] Controls that fade while playing and return on interaction, never hiding while
      paused, while focused, or while the pointer rests on them
- [ ] An opt-in 4K path: a client whose system reports 4K support is asked
      whether to switch, and a declined or dismissed prompt plays the 1080p
      stream instead. The negotiation already expresses this — it is a default
      and a consent step in the player, not a new delivery mode — and the
      round-14 measurement is the argument for it: a 4K transcode costs roughly
      2.6 CPU cores per realtime viewer against 0.5 for 1080p, so it should
      never be what a viewer silently lands on
- [ ] Subtitle appearance controls (size, colour, background)
- [x] Poster/backdrop artwork: the UI renders `poster_url`/`backdrop_url`, which
      are this server's own proxy, and never the metadata provider's absolute URL
      — `img-src 'self'` makes that structural. With no proxy configured the
      generated gradient stands in, which leaks nothing
- [x] Resume and continue watching in the player: Play resumes at the stored
      position, the position is reported while watching and cleared when the
      viewer starts over or finishes, and the navigation lists what is worth
      continuing

## Phase 5: Packaging — released (v0.17.0, v0.18.0)

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
- [x] The unit started for real on a clean Debian 12 VM and made to run ffmpeg:
      `/api/health` answers, the real `systemd-analyze security` score is 1.6 (OK),
      and the ffmpeg child carries the filter itself — `Seccomp: 2`, empty
      capabilities, no `EPERM` — while producing a correct downscaled segment
- [x] Release automation run for real: `v0.17.0` and `v0.18.0` tagged, a GitHub
      Release published with both archives and `checksums.txt`, and a multi-arch
      image on GHCR with provenance and SBOM attestations. The first run found and
      fixed the defects only a real run could: the archive was missing `deploy/`
      and the documents the runbook installs, and the publish job had no
      repository context for `gh`
- [x] A release archive that carries documentation a user needs and nothing else:
      the five reference pages, not `docs/handoff.md` or the removed reviews, with
      every relative link inside the archive resolving (checked in CI)
- [x] TLS and the reverse proxy as a runbook (`deploy/tls/`): a Caddy
      configuration beside the unit, a client certificate per tester as the
      viewer identity, the `--trusted-proxy` interaction the gate requires, and
      the `Strict-Transport-Security` the server deliberately does not send.
      Verified against a real Caddy rather than reasoned about: mutual TLS
      enforced, per-viewer progress isolated through the proxy, a client-forged
      identity header overwritten, LAN and Tailscale sources refused `403`, and
      the rate limiter keyed on the identity the proxy asserted
- [ ] Package the VAAPI userspace drivers into the image, so GPU transcoding
      works in a container without extra packages

## Known gaps

- Capacity on the development host is measured rather than assumed (round 14,
  `scripts/load-verify/`): the JSON API serves ~4,900 requests/second; 1080p
  H.264 direct play costs ~0.1 CPU cores per stream and is limited by bandwidth
  rather than by the server; a 1080p HEVC source transcodes at ~20× realtime in
  total, so roughly 20 simultaneous realtime viewers; and a 17 GB 4K HDR film
  transcodes at ~4.5×, so roughly four. Total output is set by the host and not
  by the request, so more concurrent streams divide the same capacity and each
  waits longer to start. Hardware encoding is the lever that would move this
  ceiling, and no GPU is reachable here.
- A ladder multiplies the work: each rung is a separate encode of the same source,
  so three rungs cost roughly three times one. There is no per-host cap on how many
  rungs to offer, and no use of the client's bandwidth estimate to skip rungs it
  could not afford anyway. That cost is why `max_height` alone still means one
  rendition: a client that wants determinism, or a host that cannot afford three
  encodes, can ask for a pin and get one.
- A `preferred_height` is honoured as a top rung, not as a floor. If the player's
  own adaptive logic settles below it, that is the player's choice and the server
  does not second-guess it; nothing reports which rung a client actually watched.
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
- OCR covers **PGS** and only PGS. VobSub (`dvd_subtitle`) and DVB subtitles are
  bitmaps in different containers with different palettes; they keep the `415`
  refusal and are offered as a burn rather than advertised and then failing inside
  the extractor. A second decoder is the work that would change that.
- OCR is verified against **handwritten fixtures**, not real disc subtitles. The
  fixture font is sized so the recogniser reads it exactly; a real Blu-ray track
  brings anti-aliased edges, a black outline and a palette, none of which has been
  tried here. Recognition can misread a word, and a bitmap that is not text can be
  read as some (a solid rectangle comes back as a mark), so the recognised text
  should be treated as approximate. No real PGS or VobSub sample exists on this
  host; a real disc would strengthen this more than anything else.
- OCR reads the composition and the palette but ignores window definitions, and
  treats a display set as a clear unless it is a palette update. A stream that
  reuses an object across an epoch in a way the fixture does not would be decoded
  less faithfully than ffmpeg would, though the words would still be cropped
  correctly. Cue timing from a hand-written `.sup` is shifted when ffmpeg remuxes
  it into Matroska, which is why the tests assert words rather than exact times.
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
  (see [`docs/configuration.md`](docs/configuration.md)). The failure surfaces as a stalled player rather than a
  clear error, so a client that guesses wrong has nothing to go on.
- `--auth-mode` defaults to `none`. That is right for a trusted LAN and wrong
  for anything reachable from the internet; use `proxy` or `token` before
  exposing it.
- **The gate treats an identity header as an assertion and does not verify it.**
  In `proxy` mode it believes `Tailscale-User-Login` or
  `Cf-Access-Authenticated-User-Email` from a trusted address, and neither header
  is signed. That is sound where the header can only arrive through the proxy you
  configured — a client-certificate handshake, or Tailscale Serve, which strips
  its own headers from incoming requests — and unsound wherever the port is
  reachable another way, which is why the backend belongs on loopback.
  Cloudflare Access signs `Cf-Access-Jwt-Assertion` for exactly this reason;
  validating it at the origin is the missing second control if an Access path is
  ever added. [`deploy/tls/README.md`](deploy/tls/README.md) has the arrangement
  that does not rest on an assertion at all.
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
- Packaging is released, and both shapes have now been run: the container by
  running it, and the systemd unit on a clean Debian 12 VM, where it started,
  answered `/api/health` and transcoded under its own syscall filter. What remains
  unobserved there is hardware — the guest had no render node, so VAAPI, NVENC, AMF
  and VideoToolbox are still only unit-tested and probe-validated. The
  `SystemCallFilter` in that unit is the one setting that could stop ffmpeg on a
  host where an encoder needs a call outside the list; a `libx264` encode is now
  known to pass it, and the runbook says how to diagnose and relax it when another
  encoder does not.
- The container image ships ffmpeg's VAAPI support but not the vendor userspace
  drivers, so GPU transcoding in a container needs extra packages. NVENC needs
  the NVIDIA container runtime.
- The web UI's unit tests cover `web/core.js` only — the pure timeline maths and
  clock formatting. The DOM-building and player-control code in `app.js` is still
  covered only by the CDP harnesses in `scripts/ui-verify/`, which need a browser
  and a running server and are run by hand; the seam that made `core.js` testable
  has not been extended further into the render path.
- The content security policy has no `report-uri`: there is no collector to send
  reports to, and a policy that reports nowhere is theatre. A deployment that
  wants violation reporting has to add both ends.
- `Strict-Transport-Security` is not sent by the server. It speaks HTTP, so the
  header would mean nothing there; `deploy/tls/` is the proxy configuration that
  sends it, and the runbook that says why.
- Cross-origin isolation (`COOP`/`COEP`) is not configured. Nothing here needs
  it, and enabling it would break resources that are not `CORP`-tagged.
- Tracing is traces only, over OTLP/HTTP, and opt-in. There is no OTLP over
  gRPC, no sampling beyond honouring a parent's sampled flag, no baggage, and no
  propagation to the outbound calls the server makes (the metadata provider and
  the artwork proxy). Only the request, the playback negotiation and the session
  start are spanned: scans, metadata lookups and individual segments are not.
  Beyond the four specified metrics, the registry holds counters and histograms
  for HTTP, scanning, streaming sessions, playback decisions, probe errors,
  transcoder fallbacks, the access gate, the rate limiter and dropped spans.
  The transcode KPIs carry their own bucket bounds (`TranscodeBuckets`) rather
  than the defaults, because the default list put every transcode startup inside
  one bin and made its percentiles interpolations rather than measurements
  (round 14).
