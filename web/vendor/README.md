# Vendored third-party assets

## hls.js 1.7.3

HLS playback for browsers without a native HLS demuxer (Chromium, Firefox).
Safari does not need it: it demuxes HLS itself and the UI prefers that path.

| | |
| --- | --- |
| Version | 1.7.3 |
| Upstream | https://github.com/video-dev/hls.js |
| Source | `https://cdn.jsdelivr.net/npm/hls.js@1.7.3/dist/hls.min.js` |
| Build | `dist/hls.min.js` — the **full** build, not `hls.light.min.js`, because the light build omits the subtitle and alternate-audio controllers this project intends to use |
| License | Apache License 2.0 — see `hls.js.LICENSE` |
| Size | 619,692 bytes |
| SHA-256 | `a12e7ee1cd64a69dcdb314157e45dafcba705bfb0b1440b7935cb265d374423e` |

Upstream publishes no `NOTICE` file, so none is vendored; the Apache-2.0
`LICENSE` file is included as the licence requires.

### Why it is vendored rather than loaded from a CDN

The server must work on a home network with no outbound internet access, so no
shipped page may depend on a third-party origin. Keeping the file here also
means the exact version is pinned and auditable.

### How it is loaded

`app.js` injects `vendor/hls.min.js` on demand, the first time a segmented
(HLS) stream is about to play, and then reuses it. It is deliberately not a
`<script>` tag in `index.html`: the file is ~600 KB and most page views never
need it, so paying that cost on every load would be wasteful.

### Updating

```sh
V=1.7.4   # or whatever is current
curl -sL "https://cdn.jsdelivr.net/npm/hls.js@${V}/dist/hls.min.js" -o hls.min.js
curl -sL "https://cdn.jsdelivr.net/npm/hls.js@${V}/LICENSE"          -o hls.js.LICENSE
echo "$V" > .version
sha256sum hls.min.js > hls.min.js.sha256
```

Then re-verify playback in a browser and update this table.
