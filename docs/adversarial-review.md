# Astraeus Media — adversarial review

Date: 2026-10-08. Reviewed at commit `aad2cb1` (7 commits, ~13.4k lines of Go and ~3.4k lines of
JavaScript). Three independent passes were run — Go backend, web front end, and a competitive
gap analysis against Jellyfin and Plex — and the significant findings were then re-checked by
hand. The two detailed source reports are kept alongside this one in `docs/review/`. Verification status is stated per finding: **[verified]** means I reproduced or read the
code path myself, **[reported]** means it comes from a review pass and was not independently
reproduced.

> **Status note (added later).** Fix-list items 1–5 of §7 have been implemented, in commit
> `8ff0b03`, and the remaining §7 items since: the project now has an MIT `LICENSE` and a
> third-party notices file, and `internal/library` has been split so that persistence sits in
> `internal/library/sqlite`, metadata retrieval in `internal/metadata`, and the filename rules in
> `internal/library/naming`. The body below is preserved unchanged as the record of the state at
> the reviewed commit, with the two detailed source reports alongside it in `docs/review/`, each
> carrying its own point-in-time header.

---

## 1. Verdict

The engineering is much better than the product. What exists is a well-factored kernel with an
unusual instinct for operational honesty — encoders are *proved* at startup rather than assumed,
negotiation explains itself per axis, nothing claims metadata it does not have. What is missing
is most of what a person actually touches: accounts, resume, search, multi-audio, burnt-in
subtitles, correct HDR, and every client that is not a browser.

**It is not a Jellyfin killer and the server is the wrong place to fight.** Jellyfin's moat is
roughly eight official clients plus a published API and SDKs; that is the bulk of its value, and
it is years of work for one developer. A perfect server with no TV client loses to a mediocre
server with one. The realistic path is in §6.

Blunt version: this is an excellent *engine* and an early *product*. The gap between those two
words is the whole review.

---

## 2. Confirmed defects

### Critical

**C1. SQLite is opened with no pragmas. [verified]**

`cmd/astraeus-server/main.go:149` — `sqlx.Connect("sqlite", c.dbPath)`. Measured against
`modernc.org/sqlite`:

| | journal_mode | busy_timeout | foreign_keys |
|---|---|---|---|
| as the app opens it | `delete` | **0** | **0** |
| with pragmas | `wal` | 5000 | 1 |

Consequences, in order of severity:

- `busy_timeout=0` means any write contention fails **instantly** with `SQLITE_BUSY` rather than
  waiting. The backend review reproduced this: four concurrent scans, three failed with
  `database is locked (5)`. A single user will not see it; a periodic scan overlapping a manual
  one will.
- `journal_mode=delete` makes writers block readers and is far slower than WAL. A 2000-file scan
  measured 45.4s against 2.1s with WAL — a **21×** difference. Even a single-threaded 3000-insert
  transaction was 3.3× slower in my own check.
- `foreign_keys=0` means every FK in the schema is inert decoration.

Why it survived: `internal/api/server_test.go:60` opens its test database the same way, so the
green suite is structurally blind to it. One-line fix in the DSN; the highest
value-per-minute change in this document.

**C2. No limit on streaming sessions.** `internal/streaming/hls.go`, `internal/api/server.go`.

Every `POST /api/entities/{id}/playback` starts an ffmpeg process that lives at least until the
idle reaper (2 minutes). Nothing caps concurrent sessions, per client or globally. A loop of 100
requests is 100 ffmpeg processes and 100 directories. This is a remote resource-exhaustion vector
against anything the access gate exposes. `DELETE /api/streams/{id}` (added in `aad2cb1`) lets a
*cooperative* client clean up, but the server must still enforce a ceiling.

### High

**H1. `FindEntity` cannot use its own index. [reported]** — the query uses `parent_id IS ?`
while the index is on `COALESCE(parent_id, '')`, so `EXPLAIN` shows only `library_id` used and
every identity lookup scans all entities in the library. Scan cost goes superlinear
(500 files → 294ms, 2000 → 2.13s).

**H2. Overlapping libraries steal each other's objects. [reported]** — with `movies=/media` and
`shows=/media/Shows`, the global unique index on `file_path` means whichever library scans second
takes the object, and the first library's entity ends up with none: `GET /playback` returns
400 `no_media` forever.

**H3. Deleted files are never pruned. [reported]** — removing a file from disk leaves its object
and entity in the database indefinitely. They stay in listings and in the enrichment queue.

**H4. Autoplay rejection leaves a terminal error state, so Play starts a second session.
[verified]** — `web/app.js` `onPlayRejection` sets `state.playback.status = "error"`.
`doPlay` branches on `loading`, `ready`, `playing`, `paused` and `segmented` — but not `error` —
so pressing Play falls through to `negotiatePlayback` and starts a **new ffmpeg session** for a
stream that is already loaded and merely paused. The message shown to the user is actually
specific and good ("The browser blocked playback until you interact with the page. Press Play to
start."), so this is a wasted transcode and a confusing state, not a misleading message. Six-line
fix.

**H5. `loadHlsLibrary()` can return a promise that never settles. [verified]** — on the
"script loaded but did not expose `window.Hls`" path the promise is rejected and the module-level
promise is nulled, but the `<script>` is left in `<head>`. The next call creates a fresh promise
whose executor hits the stale-script branch and `return`s **without resolving or rejecting**. The
player then waits on it forever with `status = "loading"`: layer open, transport dead, no error,
no toast. Reachable when something returns 200 without defining `window.Hls` (truncated asset, or
a proxy serving an error page with 200). The plain 404 path is handled correctly.

**H6. A failed load is rendered as a truthful-looking empty library. [reported]** — if the API is
unreachable at boot, `state.libraries` becomes `[]` and the canvas says "No libraries yet" with a
"Create one with POST /api/libraries" invitation; if an entity-list fetch fails, the library
canvas says "This library is empty" and offers a **Scan library** button. The entity view does
this correctly ("Could not load this entity" + Retry), so it is an inconsistency, not a style
choice. Telling a user their library is empty when the server is down is the kind of lie this
project is otherwise careful not to tell.

**H7. Unvalidated client codec names reach ffmpeg, and failures surface as 500s. [reported]** — a
manifest with `audio_codecs: ["totally-bogus"]` produces `-c:a totally-bogus`; `EncoderFor`
ignores the probed capability list for software encoders, so the negotiation says `Deliverable`
and the request then fails at 500 `stream_start_failed` instead of a 4xx. Not injection — ffmpeg
is invoked with an argv array and no shell — but a bad-input path reported as a server fault.

**H8. No HTTP server timeouts. [reported]** — `http.Server` has no `ReadTimeout`,
`WriteTimeout` or `IdleTimeout` (`main.go:355`), so slow clients can hold connections open
indefinitely.

### Medium

- **M1. The `--stream-root` sweep is the only data-loss risk in the codebase. [reported]** — the
  reaper deletes *any* subdirectory older than the TTL, not just ones that look like sessions. If
  the flag is ever pointed at a shared or mistyped directory, it deletes user data. A marker file
  inside each generated directory would make it safe.
- **M2. Two time bases in the player sidebar. [reported]** — `seekableBounds()` is media time, but
  `renderPlaybackSection` uses it as a duration. After a seek to 1:00:00 in a transcode the chip
  reads "produced 0:30" instead of "produced 1:00:30". `syncTransport` fixes only the element it
  owns, so the other stays wrong all session.
- **M3. The subtitle choice resets on every re-negotiation. [reported]** — each quality change and
  each forward seek past produced content re-attaches tracks and re-applies the server's `default`
  disposition. A viewer who chose **Off** gets subtitles switched back on; one who chose another
  language is moved.
- **M4. The seek bar is dead when the source duration is unknown. [reported]** — `duration_seconds`
  is `omitempty`, so a container without one yields `max=0` and a disabled range, even though the
  produced window is known and seeking already re-negotiates.
- **M5. The viewer's IP leaks to whoever set the artwork URL. [reported]** — `remoteArt`/`artNode`
  emit metadata-supplied absolute `http(s)` URLs as `<img src>`, so opening a library contacts a
  third party directly. The server-side image proxy already exists; route through it, or set
  `referrerpolicy="no-referrer"`.
- **M6. No CSP or security headers anywhere. [verified]** — no `Content-Security-Policy`,
  `X-Content-Type-Options` or `X-Frame-Options` in the Go server. The XSS story rests entirely on
  an innerHTML-free discipline that currently holds (there is no `innerHTML`,
  `insertAdjacentHTML`, `outerHTML`, `document.write` or `eval` in `web/`). That is a good
  discipline but a fragile sole defence; a `default-src 'self'` policy is a few middleware lines.
  `web/vendor/hls.min.js.sha256` is also present but never verified at runtime.
- **M7. A slow entity-list response can overwrite a newer library's list. [reported]** — the
  assignment happens before the token check, so two fast navigations can paint library B's header
  with library A's cards.
- **M8. Failure paths that leave the player unusable. [reported]** — a failed quality switch has
  already destroyed the media element and hls instance before the request fails, leaves
  `status = "error"` with a dead `pb.url`, and does not restore `sessionStart`; only a full
  re-negotiation recovers.
- **M9. `max_bitrate_kbps` is advertised but does nothing. [verified]** — the field is declared,
  defaulted to 120000, validated and echoed back in the response, but appears nowhere in
  `Negotiate` or in the ffmpeg arguments. A client that sets it gets no effect and no warning.
  Either honour it or remove it from the API; advertising a limit that is not enforced is worse
  than not having one.
- **M10. Entity rendering is O(n²) in library size. [reported]** — `childCountOf` scans every
  entity once per card, so 300 series with 20 000 episodes is ~6M comparisons per repaint, and
  there are 4–5 further full passes. `content-visibility` saves paint, not this.
- **M11. Keyboard users cannot Tab into the player once controls hide. [reported]** — the overlay
  is `inert` when hidden, and the reveal-on-keydown does not move focus, so Tab walks the topbar
  and nav and never reaches the transport.
- **M12. Closing the player drops focus to `<body>`. [reported]** — the teardown clears the
  overlay including the focused control, and the focus key is read *after* the clear, so nothing is
  restored.
- **M13. The error banner is a live region only while hidden. [reported]** — `role="alert"` is set
  while the element is `display:none` and the text is written before unhiding, so screen readers
  may never announce failures.
- **M14. Contrast on small text.** `--ink-faint` (`#565f7a`) is ≈3.1:1 against the background but
  is used for 11px `.library-path` — real information. AA wants 4.5:1.

### Low

Dead code (`effectiveHeight`, write-only `seeking`/`fullscreen` state, unreachable note branches);
a duplicated section number in `app.js`; a comment describing a "CSP-free world" that no longer
matches anything; `checkHealth` running only at boot despite `role="status"`; duplicate
`fullscreenchange`/`webkitfullscreenchange` registration; root-relative media URLs while
`web/README.md` promises sub-path mounting; `AddCounter` panicking on a negative delta; missing
`WaitDelay` on the probe/subtitle/capability subprocesses (the HLS path has it); migrations
running a full-table dedup on every start.

---

## 3. Testing

- **The suite cannot see C1 by construction** — the test harness opens SQLite identically.
- **Tautological test**: `internal/images/proxy_test.go:390` asserts `errors.Is(x, x)`, which is
  true for any error. It cannot fail.
- **Dead-code test**: the `probe_cache` `Invalidate` test covers a path nothing calls.
- **Flaky by design**: `waitForSegment` can probe a partially written `.ts`.
- **Leaks**: `subtitle_test.go` leaves `os.MkdirTemp` directories behind.
- **Missing**: no Range-request test (despite direct play depending on it), no concurrency test
  (which is why C1 and H1 survived), no integration test combining the access gate with the API.
- **The front end has no tests at all.** `app.js` is one IIFE with no exports, so nothing is
  reachable from a test runner; coverage is via the CDP harnesses only. The highest-value seam is
  to move the pure helpers and the whole source-time mapping (`sourceStart`, `producedWindow`,
  `currentSourceTime`, `seekToSource`'s reachable-vs-renegotiate decision) into `web/core.js`
  behind `globalThis.AstraeusCore` — the pattern `icons.js` already uses — and test it with
  `node --test`. That mapping is the invariant the player rests on, and it is where M2 and M4 live.

---

## 4. Code smells and structure

- **`web/app.js` is 3,838 lines in one file** with no modules and no build step. It is organised
  with numbered sections and is more disciplined than that size suggests, but it is past the point
  where a change can be made confidently. `web/core.js` for pure logic is the cheapest first cut.
- **`render()` does not own the player chrome.** `selectSubtitle` calls `render()` with a comment
  saying it repaints the radio group — but the radio group lives in the overlay, which `render()`
  never touches. Several call sites rebuild the chrome by hand. This is the most likely source of
  the next "the control lies about state" bug.
- **State latches without an owner.** The auto-hide bug fixed in `aad2cb1` and its follow-up were
  both the same shape: a boolean set by one event and cleared by another that may never arrive.
  The overlay now has a single owner and a self-healing re-check; the pattern is worth applying
  deliberately elsewhere.
- **Five empty placeholder directories** (`deploy/`, `configs/`, `docs/`, `pkg/`, `api/`) suggest
  intended structure that does not exist. Either fill or remove them.
- **No LICENSE, no Dockerfile, no systemd unit, no CI, no Makefile.** Packaging is not a feature
  but it is a prerequisite for anyone else running this.
- **README and TODO drift from the code**: the bitrate cap is described as if it works, and TODO
  claims segmented seeking is bounded when the API has supported offsets since `8865edc`.

---

## 5. What is genuinely solid — do not touch

- **Injection defence.** No HTML injection sink anywhere in `web/`; `el()` takes literal attribute
  keys so attribute injection is impossible; the icon registry uses `createElementNS` and
  `hasOwnProperty`. ffmpeg is always invoked as an argv array — no shell, so no command injection.
  Path handling uses allowlists (`NamedSegmentRe`, artwork basenames and sizes).
- **The access gate.** Peer-address trust rather than a spoofable header, `X-Forwarded-For`
  deliberately ignored, constant-time token comparison, fails closed, and audit metrics for both
  grant and deny.
- **ffmpeg session lifecycle.** Bounded stderr, `WaitDelay`, cancel-then-wait, idle reaping, and a
  verified software fallback when a hardware encoder fails at runtime.
- **Negotiation purity.** A pure function with per-axis reasons and concrete targets, tested
  independently of ffmpeg.
- **No leaks in the front end.** No `createObjectURL`; hls is stopped, detached and destroyed;
  stale callbacks are rejected by identity; teardown nulls the URL before touching the element so
  its own error events cannot be misread.
- **The observability registry.** Nil-safe, correct cumulative buckets, KPIs declared at startup.

---

## 6. What "Jellyfin killer" would actually take

Honest inventory: library scanning, negotiation, direct play, HLS with a single rendition, TMDB
metadata for film and TV, artwork proxying, WebVTT subtitles, the access gate and metrics. Absent:
**users and per-user state, resume, search, collections, playlists, music, live TV, a plugin
surface, an ABR ladder, HDR/DV handling, multi-audio, image-subtitle burn-in, downloads, and every
client that is not a browser.** Roughly: the engine is at parity for one narrow shape; everything
a user touches daily is partial or missing.

The gap that matters most is not a feature — it is **clients**. Jellyfin ships apps for Android,
iOS, Android TV/Fire TV, tvOS, Roku, webOS, Tizen, Kodi and desktop, plus an OpenAPI spec and
Kotlin/TS SDKs. That is why third-party clients exist. Writing six clients is years, and it is the
majority of Jellyfin's value. No amount of server quality closes it.

Where the incumbents are genuinely weak — the openings worth attacking:

- **Jellyfin's 10.11 scan regression**: 2.5 minutes became 30 minutes, and up to 17 hours on a
  very large library, with SQLite lock errors. Predictable, fast, idempotent scanning is a real
  wedge, and C1 alone is a 21× win on it.
- **Tone mapping and Dolby Vision**: wrong colours and DV Profile 7 direct-play regressions remain
  open upstream.
- **Transparency**: long threads exist asking *why* a stream remuxed. This project already answers
  that per axis.
- **Verified hardware encoders**: a recurring class of upstream failure. This project proves each
  encoder at startup and falls back, which is marketing-grade and currently invisible.

**The one credible route to actually displacing Jellyfin is to speak its API.** Implement the
client-facing subset (auth, users, items, `PlaybackInfo`, `/Videos/{id}/stream`, session
reporting) well enough that Jellyfin's own and third-party clients connect unchanged, then
compete on server quality. It is large but *bounded*, it avoids writing clients, and it converts
the existing strengths into the product. The cost is permanent second-implementation status and
upstream API churn.

**Realistic near-term positioning:** the observable, honest media engine — Prometheus KPIs,
verified encoders, per-decision reasons, fast scans, one static binary, no telemetry, no account,
no paywall. That is a defensible niche today and a credible foundation for the API-compatibility
bet later. It is not "a Jellyfin killer", and claiming otherwise would be the first dishonest
thing in this codebase.

---

## 7. What I would do, in order

| # | Change | Effort | Why first |
|---|---|---|---|
| 1 | SQLite DSN pragmas (WAL, busy_timeout, foreign_keys) | ~30 min | Verified 21× scan cost and instant `SQLITE_BUSY`; one line; the suite cannot catch it |
| 2 | Cap concurrent sessions and validate client codec names | ~half day | The only remote resource-exhaustion vector; turns 500s into 4xx |
| 3 | Prune deleted files during a scan | ~half day | Ghost entries in every listing and the enrich queue |
| 4 | Marker-file the stream-root sweep | ~1 hour | The only path that can delete user data |
| 5 | Fix the front-end failure paths (H4, H5, H6, M3) | ~1 day | Small, self-contained, and each one removes a lie or a hang |

Then: `web/core.js` plus `node --test` for the timeline maths; per-user watch state and resume
(the hard part — restarting ffmpeg at an offset — already works, so this is mostly persistence);
multi-audio and subtitle burn-in; HDR detection and tone mapping; and finally packaging
(Docker, systemd, LICENSE, releases), without which none of the rest reaches anyone.
