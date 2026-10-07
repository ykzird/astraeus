# UI verification harnesses

Optional developer tooling. These drive the real UI in headless Chromium over
the Chrome DevTools Protocol and assert what the `<video>` element actually
does, because "the player works" is not something a Go test can establish.

They are the evidence behind the README's playback claims. They need Node (for
its built-in `WebSocket`, so there are no npm dependencies) and a Chromium
binary.

## Running

Start the server, then run a harness against it with an entity id from
`/api/entities`:

```sh
cd astraeus-media
./astraeus-server serve --db demo.db --web-dir web --addr 127.0.0.1:8810

# in another shell
chromium --headless=new --no-sandbox --disable-gpu --mute-audio \
  --window-size=1600,1000 --remote-debugging-port=9333 \
  --remote-allow-origins='*' --user-data-dir="$(mktemp -d)" about:blank &

cd scripts/ui-verify
node player-verify.mjs        http://127.0.0.1:8810 <episodeId> <movieId>
node subtitle-verify.mjs      http://127.0.0.1:8810 <captionedEpisodeId> <movieId>
node player-chrome-verify.mjs http://127.0.0.1:8810 <entityId>
node real-media-verify.mjs    http://127.0.0.1:8810 <entityId> [timeoutSeconds]
```

`CDP_PORT` overrides the debugging port (default 9333).

## What they check

`player-verify.mjs`

- direct play starts natively and advances, and a seek lands on its target;
- a segmented (mkv → remux) stream plays through MSE with `hls.js` loaded
  lazily — `window.Hls` is undefined until the first segmented play;
- the seekable range is known;
- a **transcode** session is negotiated explicitly (the demo files only reach
  direct_play/remux with the browser profile) and its re-encoded playlist is
  played through the same `hls.js`, so the re-encode path is exercised too;
- no console errors.

`player-chrome-verify.mjs`

- the transport is overlaid on the video and there is exactly one play control,
  so the sidebar and the player cannot disagree;
- the seek bar and a fullscreen control live inside the player container;
- play/pause from the overlay and Space from the keyboard both drive the video;
- fullscreen entries target the player container (not the bare `<video>`), the
  bar is still on screen in fullscreen, and leaving fullscreen updates the
  control rather than leaving it lying;
- the bar auto-hides while playing, a hidden bar is not tabbable, and pointer
  movement brings it back.

Controls are located semantically — a fullscreen control is whatever is
labelled like one — so these checks survive renaming.

`real-media-verify.mjs`

- plays one entity through the UI and polls until the first frame, which fixed
  sleeps cannot do for a 4K transcode;
- reports the observed time to first frame, the mode badge, the server's
  reasons and whether delivery went through MSE;
- seeks within produced content, and reports the media element's state if it
  never starts.

`subtitle-verify.mjs`

- a `<track>` element is attached, and it produces a real `TextTrack`
  (guarding the case where `HTMLTrackElement.track` is not yet available);
- cues actually load from the served WebVTT;
- the selector offers Off plus each deliverable track, selecting one switches
  it on, and Off disables every track;
- navigating away removes the tracks.

## Notes

The bundled demo clips are about three seconds long, so assertions are written
to tolerate playback reaching the end during a wait; seek checks pause first.
That is a property of the fixtures, not a workaround for the app.
