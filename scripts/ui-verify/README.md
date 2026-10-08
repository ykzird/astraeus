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
node resume-verify.mjs        http://127.0.0.1:8810 <entityId> [resumeSeconds]
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
- the seek bar and a fullscreen control live inside the player container, while
  the context sidebar holds no transport controls;
- the mute, volume and quality controls exist, the mute and volume controls
  drive the element, and the volume choice is remembered;
- every icon drawn in the player has real path data at a usable size, so a bad
  registry key cannot hide as an empty square;
- play/pause from the overlay and Space from the keyboard both drive the video;
- fullscreen entries target the player container (not the bare `<video>`), the
  bar is still on screen in fullscreen, the control and its glyph follow the
  state, and leaving fullscreen updates the control rather than leaving it
  lying;
- the bar auto-hides while playing, a hidden bar is not tabbable, and pointer
  movement brings it back;
- a seek past produced content and a quality switch both re-negotiate at the
  offset — playback resumes near where the viewer was, and the viewer's subtitle
  and audio choices survive the switch;
- the audio menu appears when the entity really has more than one audio track,
  and switching it keeps playing and resumes at the offset. When it is absent,
  the harness asks the server how many tracks the entity has, so a control that
  silently disappeared cannot pass for a film with one track.

Up to 32 checks; the count depends on the entity. A `direct_play` session has no
quality menu, so the seek, quality and audio-switch checks are skipped with a
note, and the audio-menu check is skipped for an entity with one audio track —
which is why the menu's absence is asserted against the server's own track list
rather than simply accepted.

Controls are located semantically — a fullscreen control is whatever is
labelled like one — so these checks survive renaming.

`real-media-verify.mjs`

- plays one entity through the UI and polls until the first frame, which fixed
  sleeps cannot do for a 4K transcode;
- reports the observed time to first frame, the mode badge, the server's
  reasons and whether delivery went through MSE;
- seeks within produced content, and reports the media element's state if it
  never starts.

`resume-verify.mjs`

- writes a position through `PUT /api/entities/{id}/progress`, plays the entity
  the way a person does, and reads the transport's **source** clock — so it fails
  if the player ignores the stored position and starts at zero anyway;
- pauses after a few seconds and asks the server what it now holds, because a
  player that stores nothing is indistinguishable from one that resumes nothing;
- clicks the transport's start-over control and checks the position is gone, so
  the next Play cannot offer the old place;
- reports a console error as a failure, like the other harnesses.

It needs an entity longer than the resume position plus a minute — a resume at
300s in a three-second clip proves nothing — and exits with usage status if the
entity is too short.

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

A harness prints its checks and then keeps running, because the CDP socket stays
open and nothing calls `process.exit` on success. Wrap a run in `timeout`, or
read the printed summary and kill it — the summary is written before the socket
matters.

`resume-verify.mjs` needs an entity longer than its resume position plus a
minute, which the three-second demo clips are not. A cheap fixture is a
three-minute synthetic clip, which direct plays if it is H.264/mp4 and takes the
segmented path if the same stream is remuxed to Matroska:

```sh
ffmpeg -f lavfi -i "testsrc2=size=1280x720:rate=24" \
       -f lavfi -i "sine=frequency=440:sample_rate=48000" \
       -t 180 -c:v libx264 -preset veryfast -pix_fmt yuv420p -c:a aac \
       -movflags +faststart "fixture.mp4"
ffmpeg -i "fixture.mp4" -c copy "fixture.mkv"
```
