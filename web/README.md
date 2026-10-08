# Astraeus Media — web front-end

Static, dependency-free front-end for the Astraeus media server. No build step,
no npm, no bundler, no CDN: vanilla HTML, CSS and ES2020 JavaScript, plus two
locally vendored assets (`vendor/hls.min.js` and the icon paths in `icons.js`).

| File | Purpose |
| --- | --- |
| `index.html` | The static shell: topbar + breadcrumbs, the three-column grid (nav / canvas / context), the error banner and the toast region. Every dynamic region is filled by `app.js`. |
| `styles.css` | The whole visual language. CSS custom properties in `:root` separate the two systems: the **GLASS** tokens (`--glass-*`) for translucent blurred surfaces, and the **BRUTAL** tokens (`--brutal-*`, `--shadow-hard*`) for high-contrast tactile controls. |
| `icons.js` | The icon registry (**generated**, not hand-edited): the 10 [BoxIcons](https://icon-sets.iconify.design/bx/) this UI uses, as frozen path data, plus `AstraeusIcons.icon(name)` returning an `<svg>`. Fetched from the Iconify API at authoring time and vendored — nothing is requested from a third-party origin at runtime. BoxIcons is MIT; regenerate with `node scripts/fetch-icons.mjs`. See `vendor/icons.md` for provenance and `../THIRD_PARTY_NOTICES.md` for the licence notices. |
| `core.js` | The pure timeline maths, clock formatting and subtitle-track classification — source↔media time, the produced window, `formatClock`, `subtitleDeliverable`, `subtitleNeedsBurn` — with no DOM, network or module state. Loaded before `app.js`, which reads it as `window.AstraeusCore`. |
| `core.test.js` | Unit tests for `core.js`, run with `node --test web/*.test.js`. Node's own runner and asserts; no npm dependency. |
| `app.js` | Hash router, API client with per-request timeouts, render functions for the navigation list, breadcrumbs, canvas and context panel, and the player (negotiation, overlay controls, fullscreen, seek binding, subtitles, delivery-decision reporting). |

`app.js` is deliberately organised in numbered sections so the two halves — the
browsing shell (§1–§8) and the player (§9b) — stay separable.

## How the files are served

The server serves this directory as its document root, so the pages sit at `/`
and the API is same-origin at `/api/…`:

```
GET /            -> index.html
GET /app.js      -> app.js
GET /core.js     -> core.js
GET /styles.css  -> styles.css
GET /api/...     -> JSON API
```

`app.js` builds every request against the relative base `api/`. Because the
base has no leading slash it resolves against the document URL, so the app
works whether it is mounted at `/` or under a sub-path. There is no
client-side router path to configure: navigation state lives in the URL
fragment (`#/library/{id}`, `#/entity/{id}`), which never reaches the server.

No absolute or third-party URLs are requested. The only external reference the
app will ever emit is an `<img>` whose `src` came back from the API as an
absolute `http(s)` `backdrop_path`/`poster_path`; when the provider returns a
bare TMDB-style path (the normal case, since no image CDN is configured) the
app renders a deterministic CSS gradient placeholder instead, and an `<img>`
that fails to load removes itself and falls back to that placeholder.

## Running it locally

Serve the directory with any static file server, from this folder:

```sh
python3 -m http.server 8123
# then open http://127.0.0.1:8123/
```

To exercise the UI against real data, run the API and serve this directory from
the same origin — the Go server mounts it at `/` with `--web-dir`:

```sh
# from the astraeus-media directory
./astraeus-server serve --db astraeus.db --web-dir web --addr 127.0.0.1:8098 --enrich-interval 0
```

With a separate static server on another port the page still loads, but the
`/api/…` calls fail and the UI reports that in the error banner instead of
spinning forever. A list that failed to load is rendered as "Could not load
…" with a **Retry** button rather than as an empty library, so a request
failure can never be mistaken for the viewer's media being gone.

## Playback

The player calls `POST /api/entities/{id}/playback` and plays whatever the
server decides:

* **`direct_play`** — the original file from `/api/objects/{id}/file`, which
  supports HTTP range requests, so it plays natively and seeks exactly.
* **`remux` / `transcode`** — HLS (`/hls/{session_id}/playlist.m3u8`).
  Safari/WebKit demuxes HLS natively, so it gets the playlist directly.
  Chromium and Firefox do not, so the player **lazy-loads the vendored
  `vendor/hls.min.js` (hls.js 1.7.3) over Media Source Extensions** and plays
  the playlist through it. The ~600 KB library is fetched on demand the first
  time a segmented stream is played and reused after that; it is deliberately
  not a `<script>` tag in `index.html`. The loader has a 15-second deadline, so
  a stalled fetch cannot leave the player on a spinner forever, and a failed
  load is retried on the next attempt rather than cached as a permanent failure.

Subtitle tracks returned by the playback endpoint are served as WebVTT
(`text/vtt; charset=utf-8`) and attached to the player as `<track>` elements,
which works on both the native and the MSE path. The player's subtitle menu
offers an **Off** option plus one per track. A track with a URL is delivered as
a `<track>` and toggles instantly — text tracks always, and a PGS image track
too once the server has read it into text. An image track the server *cannot*
deliver has no URL; it is offered anyway, labelled "(burned in)", and choosing it
re-negotiates a session that composites the bitmap into the picture. The two
predicates that decide which case applies — `subtitleDeliverable` and
`subtitleNeedsBurn` — are pure functions in `core.js`, so `core.test.js` covers
them without a browser. Nothing is selected unless the server marks a track as
default — and a choice the viewer made sticks: a quality change or a
re-negotiating seek rebuilds the tracks from a fresh response, so the selection
is carried across and only falls back to the server's default if that track is
no longer there.

A refused autoplay is **not** an error. Every `play()` here happens after an
`await`, outside the user-gesture task, so a blocked start is the common case:
the session is left `ready` with the stream already attached, and the next Play
press toggles playback instead of opening a second session for a stream that
already exists.

### Player chrome

Every control — play/pause, restart, skip, stop, the seek bar with its time
readout, the subtitle menu, the volume slider and mute toggle, the quality menu
and the fullscreen toggle — is overlaid on the picture inside `#player-layer`,
not parked in the context sidebar. The bar is a glass scrim so it stays legible
over bright and dark frames, and it fades out after a few idle seconds while
playing. It never hides on a paused frame, while a **keyboard** focus is inside
it, or while the pointer is resting on the control strips themselves — a pointer
merely over the picture does not pin it, which is what used to leave the bar
stuck open after clicking Play. While hidden it is `inert`, so it can never
become an invisible focus trap.

The context sidebar keeps only what is *informational*: the mode badge, the
server's `decision.reasons`, media facts (container, codecs, resolution,
duration, the height actually being decoded) and the status note.

Fullscreen is requested on the player **container**, not the bare `<video>`, so
the overlay travels with the picture. The button is driven from
`fullscreenchange`, so leaving with Esc or a swipe keeps it honest.

Keyboard shortcuts, active while the player has focus (or nothing else does):
**Space** play/pause, **←/→** seek 10s, **F** fullscreen. Keys are left alone
when a modifier is held or focus is in a text field, and a focused button still
gets its own Space. Esc is the browser's.

### Timeline, seeking and quality

A playback session can start anywhere in the source: the request carries
`start_seconds`, and the server echoes it back. **Media time 0 is source time
`sessionStart`**, so the clock, the seek bar and the quality switch all speak in
source seconds while only the `<video>` element deals in media time. The landing
point is keyframe-aligned, so the true start can sit a second or two earlier
than requested — the mapping is close, not exact.

The seek bar spans the **whole film**, not just what has been produced:

* inside the produced window it is an ordinary seek;
* beyond it, the player re-negotiates with `start_seconds` at the target and
  adopts the new session, which is what makes seeking into the unproduced part
  of a film work at all. `#player-window` shows how far ahead the current
  session can jump without re-buffering.

The quality menu offers **Auto** plus the ladder heights below the source
(1080/720/480/360), and re-negotiates with that height as a `preferred_height`
at the current source time. Each option reads "Up to Np" because the choice is a
ceiling, not a guarantee: the server returns a ladder topped there and hls.js may
settle lower when the network cannot sustain the top rung. **Auto** sends no
height at all, which asks the server to adapt as far as the browser's own box
allows. The menu is withheld entirely for `direct_play`, where nothing is
re-encoded. A switch means a short re-buffer: the control shows a busy state and
the player says "Resuming…" rather than looking broken.

**Switching stops the old session on the server.** hls.js is destroyed and the
element released, and then `DELETE /api/streams/{session_id}` cancels the old
ffmpeg, waits for it to exit and removes its directory — so a quality change
does not leave a second transcoder running. The same call is made on Stop, on
leaving the entity, and on `pagehide`/`beforeunload` (as a `fetch` with
`keepalive`, since a beacon cannot issue DELETE). Failures are swallowed: a
session the idle reaper already collected answers 404, which is not worth
surfacing, and the reaper remains the backstop.

*Watch out:* `video.canPlayType('application/vnd.apple.mpegurl')` returns
`"maybe"` in current Chromium (measured on 152) even though Blink cannot demux
HLS, so `canPlayType` alone is not a safe gate. The player therefore requires a
positive `canPlayType` **and** a non-Blink engine before it will use native HLS;
everything else goes through hls.js, and a browser with neither gets an honest
explanation instead of a silently dead player.

## Known gaps

* **Static hosting** is done by the Go server: `--web-dir web` mounts this
  directory at `/`. Nothing here needs a separate static server.
* The `Incomplete` filter is applied client-side from the full library listing
  so that the badge count, the breadcrumb index and the filtered view all come
  from one authoritative list. The API's `?status=Incomplete` variant exists
  and behaves the same for a single library.
* The filter state is not mirrored into the URL; only the selection is
  (`#/library/{id}`, `#/entity/{id}`).
* No create/delete-library UI; the nav list is read-only apart from Scan.
* No list virtualization. Grid items use `content-visibility: auto` with a
  `contain-intrinsic-size` hint, which keeps very large libraries cheap to
  paint without the complexity of a windowing implementation.
* Playback state is not persisted across navigations: leaving an entity tears
  the player down so a stream never outlives the view that started it.
* Subtitle appearance is not configurable (no font/size/background controls);
  the browser's own rendering is used as-is.
* Source time is `media time + sessionStart`, and `sessionStart` is the offset
  that was *requested*. The transcoder lands on the preceding keyframe, so a
  resumed stream can really begin a second or two earlier; the clock is a close
  approximation across a seek, not an exact one.
* The quality ladder is fixed at 1080/720/480/360 and offers only heights
  strictly below the source. There is no bitrate-only or codec choice.
* Switching quality or seeking past the produced window re-buffers. The old
  session is stopped explicitly, so the gap is the new transcode starting up,
  not the previous one being cleaned up.
* The bar's hover latch follows real pointer movement only, and is cleared both
  when a move lands on the picture and when the pointer leaves the player — or
  the window — altogether. The cost is that it cannot know a pointer is resting
  on it until the user moves, and touch devices never latch at all: the bar
  simply times out after an interaction. While pinned by focus or hover it
  re-checks on a timer, so it cannot end up stuck open.
* Only `core.js` has unit tests. The DOM-building and player-control code in
  `app.js` is covered by the CDP harnesses in `../scripts/ui-verify/`, which
  drive a real browser, and those are only run by hand — nothing in CI runs them,
  because they need Chromium and a running server.
