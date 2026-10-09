# Manual end-to-end walk-through

One ordered pass over the whole application, written to be **run by hand first**
and **automated afterwards**. The existing harnesses in this directory each
check one feature deeply; this one checks that the product works as a product,
in the order a person meets it, and it deliberately says for every step whether
a machine can check it and how.

Read it with a browser open and a terminal beside it. Every step names what to
do, what you should see, and — where the answer is not obvious from the screen —
the request or file that proves it. Tick the boxes as you go.

## How to use this document

- **Do this** is the action. **Expect** is what proves it worked. **Prove it**
  is the mechanical check when the screen alone is ambiguous.
- A step marked **[auto]** can be asserted by Playwright later with no human
  judgement; the note says what the assertion is.
- A step marked **[human]** needs a judgement a machine cannot make (does this
  *look* right, does audio actually play). Keep these; do not automate them.
- A step marked **[needs real media]** cannot be exercised on the bundled demo
  library for the reason stated, which is usually that a 3-second clip is too
  short or that a browser cannot decode the codec.
- Steps are grouped into **phases**. A phase is a coherent thing that works or
  does not; if a phase fails, the later phases that depend on it will too, so
  fix forward rather than starting at the end.

## The machine-readable twin

`flows.json` beside this file is the same walk-through as data: one entry per
phase, each with its steps, their kind (`auto`/`human`), and the selector or
request the step needs. It exists so the Playwright conversion is a mechanical
translation rather than a re-reading of prose. **Keep the two in step**: when a
step changes here, change it there, and a test should fail if they disagree on
the number of phases or steps.

## Fixtures

The demo library covers most of it. Run:

```sh
mise exec -- go build -o ./astraeus-server ./cmd/astraeus-server
./scripts/make-demo-media.sh                      # media outside the repo, db at ./demo.db
```

That gives two movies and three episodes:

| Fixture | What it is for |
| --- | --- |
| `Blade Runner 2049 (2017).mp4` | A plain H.264 clip: direct play, the baseline. |
| `Arrival (2016).mp4` | HEVC with **two audio tracks** (English stereo, German mono, the latter marked default). No browser decodes HEVC, so this one **transcodes** — which is what makes the quality menu and the audio menu appear together. |
| `Astraeus Show S01E03 - Captions.mkv` | Carries a **subrip** track, for the subtitle flows. |

Two things the demo library does **not** cover, and what to do instead:

- **A clip long enough to seek.** The demo clips are three seconds. Most seek
  and resume steps need longer than the position being sought, so for those
  phases make a longer one (below) and register it.
- **An image subtitle track that the server can read.** `internal/subtitles`
  ships a VobSub and a DVB fixture, but both are subtitle-only files. To
  exercise the OCR and burn flows, mux one onto a video track (below).

```sh
# A three-minute clip, for the seek and resume phases.
ffmpeg -hide_banner -loglevel error -y \
  -f lavfi -i "testsrc2=size=640x360:rate=15:duration=180" \
  -f lavfi -i "sine=frequency=440:duration=180" \
  -c:v libx264 -preset ultrafast -pix_fmt yuv420p -c:a aac -shortest \
  /tmp/astraeus-e2e/long.mp4

# A captioned clip carrying the committed VobSub fixture, for the OCR phase.
ffmpeg -hide_banner -loglevel error -y \
  -f lavfi -i "color=c=0x202020:s=640x360:r=15" -t 6 \
  -i internal/subtitles/testdata/vobsub-caption.mkv \
  -map 0:v -map 1:s -c:v libx264 -preset ultrafast -pix_fmt yuv420p -c:s copy \
  /tmp/astraeus-e2e/ocr.mkv
```

Register whatever you add with `./astraeus-server scan`, then reload the UI.

---

## Phase 0 — the server comes up

**Start it**, in the workspace, with the caches inside the workspace:

```sh
./astraeus-server serve --db demo.db --web-dir web --addr 127.0.0.1:8910 \
  --enrich-interval 0 --scan-interval 0 --stream-root "$PWD/.tmp/e2e-streams"
```

- [ ] **Do this:** watch the first lines of the log.
      **Expect:** it names the ffmpeg it found, the encoder families it accepted
      and rejected *with reasons*, whether image subtitles are "read as text" or
      "burned in", and finally `listening addr=127.0.0.1:8910`.
      **[auto]** the log lines are stable enough to assert on; the harness should
      fail if `listening` never appears rather than hanging on a request.

- [ ] **Do this:** open `http://127.0.0.1:8910/` in the browser.
      **Expect:** the shell renders, and the health pill in the header settles on
      a connected state within a second or two.
      **Prove it:** `curl -s localhost:8910/api/health` → `{"service":"astraeus","status":"ok"}`.
      **[auto]** assert the health pill is not in its "Connecting…" state and
      carries no error styling.

- [ ] **Do this:** open the browser's console.
      **Expect:** no errors. A console error is a failure of this phase.
      **[auto]** the existing harnesses already fail on console errors; do the
      same here, and capture the text.

- [ ] **Do this:** `curl -s localhost:8910/metrics | head`.
      **Expect:** the Prometheus exposition is present.
      **[auto]** assert a known metric name is present.

## Phase 1 — the library lists and navigates

- [ ] **Do this:** with no selection, look at the sidebar.
      **Expect:** **Libraries** lists the demo libraries, and **Continue
      watching** is hidden — nothing has been watched yet. If it is showing, the
      database is not fresh; that is worth knowing before the resume phase.
      **[auto]** assert the library list is non-empty and the continue section is
      hidden on a fresh database.

- [ ] **Do this:** click a library.
      **Expect:** it becomes the selected breadcrumb, the entity list appears in
      the canvas, and the summary line counts what is in it.
      **[auto]** assert the breadcrumb text matches the library that was clicked
      and that the canvas is no longer empty.

- [ ] **Do this:** click the **incomplete** filter, and note the statuses first.
      **Expect:** the badge's count equals the number of entities whose status is
      `Incomplete`, and the visible list matches. A freshly scanned library is
      `Incomplete`; if `metadata enrich` has run (the demo script enriches when a
      TMDB key is configured, and `demo.db` is local state that may already have
      been enriched) every entity is `Complete` and the filter correctly shows
      none. **Check the badge against the statuses rather than assuming either**
      — that is the assertion, not a particular count.
      **[auto]** read each entity's `status` from `/api/entities`, count the
      `Incomplete` ones, and assert the badge and the visible list agree.

- [ ] **Do this:** toggle the filter off, then click an entity.
      **Expect:** the canvas shows the entity's detail — title, poster area,
      and a Play affordance — and the **Context** panel populates.
      **[auto]** assert the heading is the entity's title.

- [ ] **Do this:** use the browser's Back button.
      **Expect:** you return to the list rather than leaving the app, and the
      selection is restored.
      **[auto]** assert the URL changed and the canvas is a list again. This is
      the step most likely to be quietly broken by a routing change.

## Phase 2 — playback, and the decision behind it

This phase is where "it plays" stops being enough: the server explains *how* it
intends to deliver, and the request is what proves the UI asked for the right
thing.

- [ ] **Do this:** with the browser's network tab open, press Play on
      **Blade Runner 2049**.
      **Expect:** a `POST /api/entities/{id}/playback`, a `200`, and playback
      starts. On a direct-playable H.264 file the mode should be `direct_play`.
      **Prove it:** the response's `mode` and its `decision.reasons` say why.
      Note where they live: `mode` is top-level but `reasons`, the chosen audio
      stream and the target codecs are inside `decision`, and `media_info` is a
      third thing beside them. Asserting on the wrong level is an easy way to
      "pass" a check that read `null`.
      **[auto]** capture the playback request and response and assert `mode`,
      exactly as `burn-verify.mjs` already captures its own.

- [ ] **Do this:** press Play on **Arrival** (the HEVC one).
      **Expect:** the quality menu appears, because no browser here decodes HEVC
      and the server is transcoding. The mode should be `transcode`.
      **Prove it:** by hand, against a client that can play HLS — the manifest
      needs `"supports_hls": true` or the server answers `not_deliverable`
      rather than transcoding, which is correct and worth seeing once:

      ```sh
      curl -s -X POST localhost:8910/api/entities/<id>/playback \
        -H 'Content-Type: application/json' \
        -d '{"video_codecs":["h264"],"audio_codecs":["aac"],"supports_hls":true}' \
        | python3 -c 'import json,sys; d=json.load(sys.stdin); print(d["mode"]); print(d["decision"]["reasons"])'
      ```
      **[auto]** assert the mode badge/decision says transcode and that a
      quality control exists. On a direct-play entity, assert the quality control
      is *absent* — a missing control must not pass by accident.

- [ ] **Do this:** open the quality menu and choose a lower rung.
      **Expect:** the label reads "Up to Np", choosing it re-negotiates, and
      playback continues near the same place.
      **Prove it:** the new request carries `preferred_height` and no
      `max_height`, and the decision reports a ladder topped there.
      **[auto]** assert the request fields and that the new decision's rungs are
      topped at the choice. This is exactly `quality-verify.mjs`; fold it in
      rather than duplicating it.

- [ ] **Do this:** on **Arrival**, open the audio menu.
      **Expect:** both tracks are offered with sensible labels, and the one the
      file marks *default* (German mono) is the current choice.
      **[auto]** assert the menu has as many entries as the server reports audio
      tracks — ask the server, do not assume two — and that switching one
      re-negotiates with the matching `audio_track_index`.

- [ ] **Do this:** play a segmented file (an `.mkv` episode) and watch the
      network tab.
      **Expect:** a master playlist and then segments, with `hls.js` loaded
      lazily. Direct play must not have loaded it.
      **[auto]** assert `window.Hls` is undefined before the first segmented
      play and defined after, as `player-verify.mjs` does.

## Phase 3 — the player's own controls

`player-chrome-verify.mjs` covers this phase in depth. The steps here are the
ones a person would actually perform; if the harness is present, run it and
treat a failure as a failure of this phase rather than re-walking it by hand.

- [ ] **Do this:** click the video, then move the mouse away and wait.
      **Expect:** the control bar auto-hides while playing, and moving the
      pointer brings it back.
      **[human] [needs real media]** — this is a judgement about feel as much as
      state, and a three-second clip ends during the wait, so use the long clip.

- [ ] **Do this:** press Space, then click play/pause on the bar.
      **Expect:** both toggle playback and the control's glyph follows the state.
      **[auto]** assert `paused` flips and the control's label or icon changes.

- [ ] **Do this:** drag the seek bar to roughly the middle.
      **Expect:** the picture jumps there and playback continues. On a segmented
      stream this re-negotiates at the offset rather than buffering from zero.
      **Prove it:** the new request carries the seek offset.
      **[auto] [needs real media]** capture the re-negotiation and assert its
      offset is near where the seek landed, as `player-chrome-verify.mjs` does.

- [ ] **Do this:** mute, change volume, then reload the page.
      **Expect:** the volume choice is remembered across the reload.
      **[auto]** assert the stored value survives a reload.

- [ ] **Do this:** enter fullscreen from the player, then leave it.
      **Expect:** it is the **player container** that goes fullscreen, not the
      bare `<video>`, so the overlay and controls stay on screen; leaving
      updates the control rather than leaving it stuck.
      **[auto]** assert `document.fullscreenElement` is the player container and
      that the control's state follows. **[human]** whether it *looks* right.

## Phase 4 — subtitles

- [ ] **Do this:** play the captioned episode, open the subtitle menu.
      **Expect:** Off plus every deliverable track. Choosing one attaches a
      `<track>` and the cue text appears on screen.
      **[auto]** assert a `<track>` is attached, its `TextTrack` produces cues
      from the served WebVTT, and Off disables them — `subtitle-verify.mjs`.

- [ ] **Do this:** watch a cue boundary.
      **Expect:** the caption changes when the second cue starts, and the
      ampersands and quotes in the fixture text are shown as text, not markup.
      **[human]** — a person can see that `&` did not become `&amp;` on screen.

- [ ] **Do this:** play the VobSub-muxed clip (**needs the `ocr.mkv` fixture
      above**), and open the subtitle menu.
      **Expect:** with tesseract installed, the image track is offered as an
      ordinary track — **no "(burned in)"** — and choosing it attaches a
      `<track>` with **no re-negotiation at all**, whose text is the caption the
      fixture drew.
      **[auto]** assert no playback request is made when the track is chosen,
      and that the cue text matches the fixture — `ocr-verify.mjs`.

- [ ] **Do this:** restart the server with `--tesseract-bin /nonexistent/tesseract`
      and repeat the previous step.
      **Expect:** the same track is now offered as **"(burned in)"**, choosing it
      re-negotiates with `burn_subtitle_index`, and the decision comes back with
      `burned_subtitle_index`.
      **[auto]** assert all three — `burn-verify.mjs`. Note that the *picture* is
      not checked here: what is composited into a frame is the Go integration
      test's job, because a browser cannot be asked what is baked into a frame.

## Phase 5 — resume and continue watching

**[needs real media]** — every step here needs a clip longer than the position
being sought.

- [ ] **Do this:** play the long clip for ~30 seconds, pause, and reload.
      **Expect:** the entity appears under **Continue watching**, and playing it
      resumes near where you left off rather than at zero.
      **Prove it:** `curl -s localhost:8910/api/progress` lists it under
      `entries`.
      **[auto]** read the transport's **source** clock, not the element's — a
      player that ignores the stored position and starts at zero looks identical
      otherwise. `resume-verify.mjs` already does this.

- [ ] **Do this:** click the transport's start-over control.
      **Expect:** the stored position is cleared, the entity leaves Continue
      watching, and the next Play starts at zero.
      **[auto]** assert `entries` no longer names the entity.

- [ ] **Do this:** let the clip run into its last few percent, then reload.
      **Expect:** it does **not** appear in Continue watching — watched-through
      clears the row.
      **[auto]** assert its absence from `entries`.

- [ ] **Do this (gated installs only):** with `--auth-mode proxy` behind the TLS
      example in `deploy/tls/`, report a position as one client certificate and
      read it back as another.
      **Expect:** each identity keeps its own place, and Continue watching is
      per viewer.
      **Prove it:** covered by unit tests through the gate middleware, but not by
      this walk-through.

## Phase 6 — the API as a client sees it

The UI uses the same API anything else does, so a phase that drives it directly
catches what a click cannot.

- [ ] **Do this:** request a library scan.
      **Prove it:** `curl -s -X POST localhost:8910/api/scan` → a 200, and
      `/metrics` counts it.
      **[auto]** assert the status and that a second scan is idempotent — the
      entity count must not change.

- [ ] **Do this:** send a capability manifest that no encoder can satisfy, e.g.
      `"video_codecs":["av1"]` against this host.
      **Expect:** a `409` naming what it cannot do, not a 500 and not a silent
      software fallback.
      **[auto]** assert the status *and* the error code.

- [ ] **Do this:** send a manifest with a codec name that does not exist.
      **Expect:** a `400` before anything reaches ffmpeg.
      **[auto]** assert the status.

- [ ] **Do this:** call `GET /api/system/capabilities`.
      **Expect:** the accepted and rejected encoder families with their reasons,
      and `subtitle_ocr_enabled` matching whether tesseract is installed.
      **[auto]** assert the shape, and that `subtitle_ocr_enabled` agrees with
      what Phase 4 observed — the two must not disagree.

- [ ] **Do this:** with `--rate-limit 1 --rate-limit-burst 2`, make three calls
      quickly.
      **Expect:** `200, 200, 429` with `Retry-After`, `/api/health` untouched,
      and `astraeus_rate_limited_total` going up by one.
      **[auto]** this is the manual run in `docs/handoff.md`; fold it in here so
      the walk-through covers it.

## Phase 7 — access, when a gate is configured

Skip this phase unless you started the server with a gate. It is here so the
walk-through is complete for a deployed install, and so the automation knows the
phase exists and can be told to skip it.

- [ ] **Do this (`--auth-mode token`):** call any API route with no token, then
      with the right one.
      **Expect:** `401` then `200`; `/api/health` needs no token.
      **[auto]** assert both, and that an unauthenticated `/api/health` still
      answers — a health check that needs credentials makes a limiter look like
      an outage.

- [ ] **Do this (`--access-policy`):** as an identity granted one library, list
      libraries and entities.
      **Expect:** only the granted library and its entities; the other library
      answers `404`, not `403`, so the API is not a way to enumerate what exists.
      **[auto]** assert the counts and the 404.

- [ ] **Do this (`--access-policy`):** try to register a library and to scan as
      a non-admin.
      **Expect:** `403` on both. An admin who is not granted visibility of a
      library can still scan it — the two grants are separate, so check both
      directions.
      **[auto]** assert both statuses.

- [ ] **Do this:** turn subtitles on for an entity in a library you may see, then
      revoke the grant and fetch the subtitle URL again.
      **Expect:** the session URL is a capability, so the revocation takes effect
      on the *re-check* rather than leaving the stream running.
      **[auto]** assert the status changes.

## Phase 8 — deployment, if you are checking a release

- [ ] **Do this:** against the built image or the installed unit, hit
      `/api/health` and play one entity.
      **Expect:** it works, and the version reports the tag rather than `dev`.
      **[needs real media]** for the transcode half.

- [ ] **Do this:** `systemd-analyze security astraeus` inside the guest.
      **Expect:** the score `deploy/README.md` quotes.
      **[human]** — provisioning a VM is outside what this walk-through drives.

---

## What is deliberately not here

- **What is composited into a frame after a burn or an OCR pass.** A browser
  cannot be asked; that is the Go integration tests' job, and they assert pixels.
- **Hardware encoders.** No GPU on this host, so every hardware step is skipped
  by the server's own probe and the reasons are reported. The walk-through
  asserts that the reasons appear, not that the encoders work.
- **Firefox and Safari.** Not installed; the harnesses are Chromium-only, and
  the native-HLS branch has never been observed firing.

## Converting this to Playwright

The phases map one-to-one onto Playwright tests, and `flows.json` is the list to
iterate. Three things are worth deciding before writing them:

1. **One test per phase, or one file per phase.** The phases are ordered but
   nearly independent after Phase 2, so per-phase files parallelise; Phase 0 must
   still run first because everything else needs a server.
2. **Playwright's own fixtures for the server.** Start the server in a fixture
   with `--stream-root` inside the workspace, wait for `listening` rather than
   sleeping, and tear it down by PID. Do **not** `pkill -f` a pattern that
   appears in the test's own command line — that has bitten this project twice.
3. **`page.route` for the capture steps.** The steps that assert a playback
   request carry a field are best done by intercepting `fetch` rather than
   scraping the network log, which is what `burn-verify.mjs` already does.

The `[human]` steps should stay human even after the conversion: keep them in
this document as the things a person still checks, and let the suite cover
everything else.
