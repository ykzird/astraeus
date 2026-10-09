# UI verification harnesses

Optional developer tooling. These drive the real UI in headless Chromium over
the Chrome DevTools Protocol and assert what the `<video>` element actually
does, because "the player works" is not something a Go test can establish.

They are the evidence behind the README's playback claims. They need Node (for
its built-in `WebSocket`, so there are no npm dependencies) and a Chromium
binary.

## Start here: the manual walk-through

`E2E-MANUAL.md` is one ordered pass over the whole application, written to be
**run by hand first** and automated afterwards. Where the harnesses below each
check one feature deeply, it checks that the product works as a product, in the
order a person meets it, and every step says whether a machine can assert it
(`[auto]`), only a person can judge it (`[human]`), or both.

`flows.json` is the same walk-through as data, for the Playwright conversion;
`check-e2e-flows.test.mjs` fails when the two drift apart, and CI runs it. Run it
yourself with:

```sh
node --test scripts/ui-verify/check-e2e-flows.test.mjs
node scripts/ui-verify/check-e2e-flows.mjs          # prints the counts and any drift
```

The harnesses below are the deep per-feature checks the walk-through leans on,
and several phases say so rather than repeating their work.

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
node burn-verify.mjs          http://127.0.0.1:8810 <imageSubtitleEntityId>
node ocr-verify.mjs           http://127.0.0.1:8810 <imageSubtitleEntityId> [caption]
node quality-verify.mjs       http://127.0.0.1:8810 <transcodingEntityId>
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

`burn-verify.mjs`

- the entity really has an image subtitle track, according to the server's own
  `subtitles[]` (`text: false`), rather than the harness guessing from markup;
- that track is offered in the menu and labelled as burned in;
- choosing it re-negotiates, and the captured request carries
  `burn_subtitle_index` equal to the track's index while the captured decision
  carries the same `burned_subtitle_index`;
- the menu shows the burned track as the current choice, which only the decision
  can do — a burn is not a `<track>`;
- playback continues across the switch, and choosing Off re-negotiates with no
  burn index and gets a decision without one.

It wraps `window.fetch` before the page loads to capture the playback request and
response, because a repainted radio cannot distinguish a real re-negotiation from
a stuck menu. What is composited into the picture is asserted by the Go
integration test instead — a browser cannot be asked what is baked into a frame.

**It needs a server with no OCR engine**, because an image track the server can
read is offered as an ordinary text track, not a burn. Point the server at a
binary that does not exist to exercise the burn path:

```sh
./astraeus-server serve --db burn.db --web-dir web --addr 127.0.0.1:8810 \
  --tesseract-bin /nonexistent/tesseract
```

`ocr-verify.mjs`

- asks the server for `subtitle_ocr_enabled` first, and says so rather than
  failing if the engine is not installed;
- the entity really has an image subtitle track (`text: false`) according to the
  server's own `subtitles[]`, and that track now carries a URL;
- the menu offers it as an ordinary track — no "(burned in)" suffix;
- choosing it attaches the served `<track>` and makes **no** playback request at
  all, because there is nothing to re-encode;
- the browser loads cues from the OCR output and the caption on screen contains
  the text the fixture drew, which is the only place the recognised words can be
  observed as a viewer would;
- no console errors.

It needs a server with tesseract installed, and an entity with a PGS track
carrying real text (the `-text` fixture below).

`quality-verify.mjs`

- the session really is being re-encoded, so the quality menu exists at all (it
  is withheld for direct play) — the harness fails with a note rather than
  passing vacuously when given an entity that direct-plays;
- every height is labelled `Up to Np`, because the choice is a ceiling the
  player may stay under rather than a guarantee of that rendition;
- choosing one sends `preferred_height` and **not** `max_height`, which is the
  difference between capping a ladder and pinning a single encode;
- the decision that comes back carries several `renditions` topped at the chosen
  height;
- playback continues across the switch, and the console stays clean.

It needs an entity the browser profile must transcode — the bundled demo's HEVC
film is one, and so is any file whose codec the profile cannot decode.

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

`burn-verify.mjs` needs an entity with a PGS track, and nothing generates one but
this repository. `scripts/pgsgen` writes the `.sup`; mux it into a clip long
enough to watch, then scan it:

```sh
go run ./scripts/pgsgen -out fixture.sup -start-ms 500 -end-ms 55000
ffmpeg -f lavfi -i "color=c=0x202020:s=640x360:r=15" -t 60 \
       -c:v libx264 -preset ultrafast -pix_fmt yuv420p base.mp4
ffmpeg -i base.mp4 -i fixture.sup -map 0:v -map 1:s -c:v copy -c:s copy \
       "Image Subtitles (2026).mkv"
./astraeus-server scan --db burn.db --path "$PWD" --kind movies --name Burn
```

A dark picture is deliberate: the fixture draws a white rectangle, so a burned
segment is unmistakable in a frame.

`ocr-verify.mjs` needs the same shape of entity, but with real letters in the
track, which is what `-text` draws. The default glyph size is chosen for
tesseract, so leave `-text-scale` alone unless the caption reads wrong:

```sh
go run ./scripts/pgsgen -text "ASTRAEUS MEDIA" -x 70 -y 150 \
       -start-ms 500 -end-ms 55000 -out caption.sup
ffmpeg -f lavfi -i "color=c=0x202020:s=640x360:r=15" -t 60 \
       -c:v libx264 -preset ultrafast -pix_fmt yuv420p base.mp4
ffmpeg -i base.mp4 -i caption.sup -map 0:v -map 1:s -c:v copy -c:s copy \
       "OCR Subtitles (2026).mkv"
./astraeus-server scan --db ocr.db --path "$PWD" --kind movies --name OCR
```

Run it against a server with tesseract (`--tesseract-bin` defaults to
`tesseract`), and pass the caption if it is not the default above. The harness
asserts the words the fixture drew; if the server's `subtitle_ocr_enabled` is
false it stops and says so rather than reporting front-end failures.
