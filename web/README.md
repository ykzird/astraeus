# Astraeus Media — web front-end

Static, dependency-free front-end for the Astraeus media server. Three files, no
build step, no npm, no bundler, no CDN: vanilla HTML, CSS and ES2020 JavaScript.

| File | Purpose |
| --- | --- |
| `index.html` | The static shell: topbar + breadcrumbs, the three-column grid (nav / canvas / context), the error banner and the toast region. Every dynamic region is filled by `app.js`. |
| `styles.css` | The whole visual language. CSS custom properties in `:root` separate the two systems: the **GLASS** tokens (`--glass-*`) for translucent blurred surfaces, and the **BRUTAL** tokens (`--brutal-*`, `--shadow-hard*`) for high-contrast tactile controls. |
| `app.js` | Hash router, API client with per-request timeouts, render functions for the navigation list, breadcrumbs, canvas and context panel, and the player (negotiation, transport, seek binding, delivery-decision reporting). |

`app.js` is deliberately organised in numbered sections so the two halves — the
browsing shell (§1–§8) and the player (§9b) — stay separable.

## How the files are served

The server serves this directory as its document root, so the pages sit at `/`
and the API is same-origin at `/api/…`:

```
GET /            -> index.html
GET /app.js      -> app.js
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
the same origin (the Go server does not mount a static handler yet — see
"Known gaps"):

```sh
# from the astraeus-media directory
./astraeus-server serve --db astraeus.db --addr 127.0.0.1:8098 --enrich-interval 0
```

With a separate static server on another port the page still loads, but the
`/api/…` calls fail and the UI reports that in the error banner instead of
spinning forever.

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
  not a `<script>` tag in `index.html`.

Subtitle tracks returned by the playback endpoint are served as WebVTT
(`text/vtt; charset=utf-8`) and attached to the player as `<track>` elements,
which works on both the native and the MSE path. The playback panel offers an
**Off** option plus one per deliverable track; image-based tracks (PGS/VobSub)
have no URL and are shown disabled with the reason. Nothing is selected unless
the server marks a track as default.

**Seeking a segmented stream is bounded by the transcoder.** The server
produces segments in order while playback runs, so the seek bar is bound to the
live `seekable` range and grows as more is produced — seeking forward is limited
to what already exists, and the UI says so.

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
