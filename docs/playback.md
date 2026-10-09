# Playback and delivery

How a file reaches a player: whether it can be sent as it is, has to be
repackaged, or has to be re-encoded, and how the capability manifest, dynamic
range, audio tracks, subtitles and the adaptive ladder decide that. The
[specification](../SPECIFICATION.md) explains why it is built this way; this page
explains what it does.

## Playback compatibility

`direct_play` serves the original file with range support, which any browser can
play when the codecs are ones it supports (typically mp4/H.264/AAC).

`remux` and `transcode` produce **HLS**. Safari demuxes HLS natively; Chromium
and Firefox do not, so the UI loads the vendored **hls.js** (MSE) for those
browsers. See [`web/vendor/README.md`](../web/vendor/README.md) for the pinned
version, licence and provenance, and
[`THIRD_PARTY_NOTICES.md`](../THIRD_PARTY_NOTICES.md) for the licence notices that
must travel with any redistribution. No shipped page depends on a third-party origin
at runtime.

Seek behaviour on a segmented stream is not limited to what has been produced:
the player's seek bar spans the whole film, and a seek beyond the produced
window re-negotiates the session at that offset rather than clamping. The
landing point is keyframe-aligned, so a resumed stream can begin a second or two
earlier than requested.

## Negotiation

`POST /api/entities/{id}/playback` takes an optional `ClientCapability` body:

```json
{
  "containers": ["mp4", "webm", "hls"],
  "video_codecs": ["h264", "vp9"],
  "audio_codecs": ["aac", "opus"],
  "max_width": 1920,
  "max_height": 1080,
  "preferred_height": 0,
  "max_bitrate_kbps": 120000,
  "max_bit_depth": 8,
  "max_audio_channels": 2,
  "supports_hdr": false,
  "audio_track_index": 0,
  "burn_subtitle_index": 0,
  "supports_hls": true,
  "subtitles": true
}
```

The body may also carry `start_seconds`, the offset into the source at which the
session should begin; the response echoes it back as `start_seconds` so a client
can keep a continuous timeline across a seek or a quality change. A negative
value, or one at or past the end of the media, is rejected with `400`.

Omit the body to use the built-in browser profile, which caps at **1920x1080,
8-bit, stereo**. Those three limits are not arbitrary: they are the ones a
browser cannot be relied on to exceed. A 4K source would otherwise be
re-encoded at 4K (about four times the CPU) for a client that usually cannot
decode it; 10-bit H.264 ("High 10") and 5.1 AAC are refused outright by
Chromium's media pipeline, and a refused audio append tears the whole
MediaSource down, so the player attaches, fetches every segment, and never
shows a frame. A client that genuinely wants more can ask for it in its
manifest. The response carries the
decision and the reason for every choice:

```json
{
  "mode": "remux",
  "url": "/hls/5d9866ee-.../playlist.m3u8",
  "session_id": "5d9866ee-...",
  "decision": {
    "mode": "remux",
    "deliverable": true,
    "container": "hls",
    "video_action": "copy",
    "audio_action": "copy",
    "reasons": [
      "client cannot open container \"matroska\"",
      "remux: the streams are compatible but the container is not, so they are copied into HLS unchanged"
    ]
  }
}
```

Modes:

- **`direct_play`** — the client handles the source container and codecs, so the
  original file is served as-is and the client does the buffering.
- **`remux`** — streams are compatible but the container is not; they are copied
  into HLS unchanged, so quality is bit-identical.
- **`transcode`** — at least one stream is incompatible, or the source is larger
  than the client accepts, or its bitrate is above what the client will take; only
  the offending streams are re-encoded. This is also where an HDR source is tone
  mapped for a client that cannot show HDR.

A `409` means the client's declaration makes delivery impossible (for example it
needs re-encoding but cannot play HLS, or its bitrate limit leaves nothing for
video after the audio); the reasons explain why.

The decision also carries the concrete targets it chose — `target_height` for a
downscale, `target_audio_channels` for a downmix, `target_dynamic_range` for
the delivered video's dynamic range, `target_bitrate_kbps` for a bitrate ceiling,
`target_audio_stream_index` for the audio track that was actually mapped, and
`renditions` for a ladder — so a client can see not just that it will be
re-encoded but what it will get. A `tone_map` flag says the picture was converted from HDR to SDR, which is a
visible change rather than a quality trade-off.

A manifest naming a codec that does not exist is refused with `400` before it
can reach ffmpeg. A codec that exists but that this host cannot encode is a
negotiation outcome, not a bad request: the server picks another codec the
client accepts if there is one, and otherwise answers `409` saying so.

An omitted limit means **unrestricted**, so a client that says nothing about
channels gets the source's own channel count. That is the right default for a
native player, which may well want the 5.1 mix, but it is a trap for a browser:
Chromium refuses a 5.1 AAC `SourceBuffer`, and because the audio append fails
first it tears down the whole `MediaSource`, so the player attaches, fetches
every segment and never shows a frame. A browser client should declare
`max_audio_channels: 2` and `max_bit_depth: 8` — the built-in profile already
does.

`supports_hdr` is the one field that is **not** inferred from anything else, and
it is off in the built-in profile. Nothing about an arbitrary browser proves it
can render HDR, so assuming it would hand a PQ stream to a compositor that shows
it washed out. A client that really can — a television app, a browser on an HDR
display reporting through its own manifest — says so. Declaring it alongside
`max_bit_depth: 8` is a contradiction and is refused with `400`: HDR is stored at
ten bits or more.

`max_bitrate_kbps` is a limit on the **whole** stream, and it is acted on rather
than echoed. The audio's share is reserved first — the source's own rate when the
audio is being copied and the container states it, otherwise the 192 kbps this
server encodes audio at — and the video is held to the remainder with a VBV
ceiling, which every encoder family honours while keeping its own quality
settings. So a source the client can decode but cannot afford is re-encoded
rather than copied, a source that already fits is left alone, and an unknown
source bitrate is not assumed to exceed the limit (the reasons say the limit could
not be checked). A limit under 100 kbps is refused as malformed, and one that
leaves nothing for video after the audio is a `409` with the arithmetic in the
reason.

## HDR and dynamic range

A high dynamic range source is not merely a brighter one. PQ (SMPTE ST 2084) and
HLG code values describe light through a different transfer function, so SDR
output has to convert them; ffmpeg's default behaviour is to reinterpret the
values and tag the result as the source was tagged. The picture then reaches the
player looking washed out — milky blacks, flat contrast — **and the stream
claims to be HDR while containing 8-bit data**, which is what makes it wrong
rather than merely different.

Dynamic range is decided from the source's **transfer function**, not its bit
depth or its primaries. A 10-bit BT.2020 SDR master is stored exactly like an SDR
one and is never tone mapped; guessing HDR from a wide-gamut tag would reduce the
contrast of a correct picture, which is worse than leaving a mis-tagged file
alone.

| Source | Client | Result |
| --- | --- | --- |
| SDR | any | Unchanged. No tone mapping, whatever the bit depth |
| HDR | declares `supports_hdr` | **Kept**: direct play, remux, or a 10-bit re-encode, tagged BT.2020 with the source's PQ or HLG transfer. Dolby Vision's dynamic metadata is not carried through a re-encode and the reasons say so |
| HDR | does not | **Tone mapped to SDR**: 8-bit BT.709, tagged `bt709`/`bt709`/`bt709` |

Tone mapping is the documented software chain — `zscale` to linear light, the
`hable` tone curve, `zscale` back to BT.709 — because it needs no GPU. The
libplacebo filter tone maps better and understands BT.2390 and Dolby Vision, but
it requires a working Vulkan device and fails the whole transcode where one is
missing, so it is deliberately not used.

Keeping HDR is a *separately proved* capability. A video encoder that works at
8 bits can still refuse 10, so at startup each encoder is probed a second time
with a 10-bit pixel format and a real session's options; `hdr_video_encoders` is
that list. If a client asks for HDR and no encoder here proved 10-bit — or the
only one that did has since failed — the stream is tone mapped to SDR with the
reason attached, because failing a request that a working software encoder could
have served is the wrong answer.

Two caveats are reported rather than hidden. **Dolby Vision profile 5** stores
IPTPQc2, not PQ, so tone mapping it without a Dolby Vision converter produces
approximate colour, and the reasons say exactly that. Re-encoding a **profile 8**
stream keeps its HDR10 base layer but not the dynamic metadata. Mastering-display
and content-light metadata are not carried through a re-encode either.

Detection reads ffprobe's `color_transfer`, `color_primaries`, `color_space` and
the stream's `DOVI configuration record`; all of it appears in the playback
response's `media_info` as `dynamic_range`, `dolby_vision_profile` and
`dolby_vision_base_layer_hdr10`.

## Hardware acceleration

Five families are supported, preferred in this order: **NVENC** (NVIDIA),
**QuickSync** (Intel), **VideoToolbox** (Apple), **VAAPI** (AMD and Intel on
Linux) and **AMF** (AMD). A machine with more than one — an NVIDIA card beside an
Intel iGPU — reports all of them and uses the first that works.

A hardware encoder is used only after it has **proved it can encode**: at startup
each candidate is run against a fraction of a second of test video, with *the
same options a real session would use*, and one that cannot open a session is
dropped from the reported capabilities. Verifying with the real flags is the part
that matters. An encoder can be compiled in, be listed, and still reject the
options it is given, and a probe that tested something else would pass while
playback failed.

Neither signal on its own is trustworthy. `ffmpeg -encoders` lists what was
*compiled in* — this project's own development machine lists NVENC, AMF,
QuickSync and VAAPI and can use none of them. A populated `/dev/dri` says nothing
about the GPU vendor either, since an AMD machine exposes a device directory
exactly as an Intel one does.

**When hardware transcoding does not happen**, the reason is reported rather than
hidden. Startup logs a line per rejected encoder with ffmpeg's own complaint, and
`GET /api/system/capabilities` carries the same thing:

```console
$ ./astraeus-server serve --db astraeus.db
WARN hardware encoder rejected encoder=h264_nvenc reason="exit status 255: Cannot load libcuda.so.1"
WARN hardware encoder rejected encoder=h264_qsv reason="exit status 171: Error creating a MFX session: -9."
WARN no hardware encoder is usable on this host; transcoding will use the CPU rejected=12
```

That is the difference between a fixable driver problem and a mystery. A machine
with an NVIDIA card and no driver does not look like a machine with no GPU.

VAAPI additionally needs a DRM render node; one is located at startup and passed
to both the probe and every session, before the input, because `hwupload` has no
device to upload to without it.

If a hardware encoder is chosen and still fails at runtime, the session is
retried once in software and `astraeus_transcode_fallbacks_total` counts it,
rather than failing the request when a working software path exists.

`GET /api/system/capabilities` reports what survived that check, including the
`video_encoders` and `audio_encoders` this host can actually offer, and
`hdr_video_encoders`, which lists the encoders that also produced a **10-bit**
stream — each with the pixel format it accepted. `subtitle_ocr_enabled` reports
whether image subtitles can be read into text on this host (see
[Image subtitles](#image-subtitles)).

## Audio tracks

A media file can carry several audio tracks, and the server lists all of them in
`media_info.audio_tracks` — `index` (the ffmpeg stream index, which is what a
client sends back), `codec`, `channels`, `bitrate_kbps`, `language`, `title` and
`default`. `audio_track_index` in the request chooses one; **omitting it or
sending 0 means the server's choice**, which is the track the file marks
`default`, falling back to the first. That distinction matters: a file whose
second track is the one marked default is common, and "the first stream wins"
delivers the wrong language on a file that says which one it means.

The choice is honoured in what arrives, not just in what is reported. Every
audio decision — codec compatibility, channel count, the bitrate the audio
reserves — is made about the *chosen* track, and a session maps it by its stream
index. **A chosen track cannot come from direct play**, because direct play serves
the original file and the player then picks a track itself; so choosing one
repackages into HLS with the picture copied and no re-encoding, which is cheap.
When the chosen track is one the client cannot decode, it is re-encoded and
downmixed as usual.

An `audio_track_index` naming a track the file does not have is a `400` listing
the indices that do exist, because the client already had that list. A player can
therefore offer the choice without probing anything itself.

## Image subtitles

Text tracks are served to the browser as WebVTT. Image-based tracks (PGS, VobSub)
carry pictures rather than text, so no browser can render one as a subtitle
track. There are two ways to show one, and the server chooses the better one it
can actually do.

**OCR reads PGS and VobSub into text.** When `tesseract` is installed (that is,
when `subtitle_ocr_enabled` is true), an image track is advertised with a URL like
a text track and served as WebVTT: the subtitle stream is demuxed with ffmpeg,
its bitmaps are decoded by `internal/subtitles`, each cue's picture is turned
into dark glyphs on a white page, and tesseract returns the words. The result is
an ordinary `<track>` — the viewer can toggle it, restyle it and search it, and
switching it on costs nothing but a fetch. That is the whole point: a burn can do
none of those things.

The two readers differ in where a track keeps its palette. PGS carries its own
inside the picture stream, so its extraction is a raw `.sup`. A VobSub track does
not: the palette lives in the container, as the `size:`/`palette:` text of a
`.idx` sidecar, or the same text in Matroska's codec private. A Matroska source
is therefore kept as Matroska — its subtitle stream is copied into a standalone
Matroska file and the palette travels with it — and any other source is demuxed
into raw SPU packets with the codec private read out beside them, because an
image-only copy would drop the palette and leave nothing to render.

The runtime dependency is treated as optional, not assumed. Without tesseract the
server does not fail: it logs that image subtitles will be burned in, withholds
the URL, and answers `GET /api/objects/{id}/subtitles/{track}.vtt` with the same
`415 subtitle_format_unsupported` it always did. `--tesseract-bin` names the
executable and `--ocr-language` passes a language (unset means tesseract's own
default, which is `eng` when only the base package is installed).

**A burn is the fallback.** When there is no OCR engine, or the track is a codec
the reader does not decode, `burn_subtitle_index` in the playback request names
the ffmpeg stream index to composite into the video (`media_info.subtitles`
reports each track, with `text: false` marking the image ones). The decision then
carries `burned_subtitle_index`, forces a transcode and pins **one** rendition:
the bitmap is composited once in a single filter graph, so a ladder would mean
burning only one rung. The composite scales the subtitle to the picture with
`scale2ref`, so a downscaled re-encode places and sizes it correctly, and it is
applied *after* the plan's own filters — which is what makes it right for a tone
map, since a subtitle bitmap is SDR white and must be laid over the finished SDR
picture rather than converted with it. The subtitle is decoded from a second
opening of the input, because asking one input for both the video and the
subtitle stream in the same graph does not deliver subtitle frames.

What OCR covers, honestly:

- **PGS and VobSub, not DVB.** Two decoders exist: HDMV PGS, and VobSub
  (`dvd_subtitle`), whose control sequence, run-length fields and container
  palette are decoded by `internal/subtitles`. DVB subtitles are still
  burn-only: their palette and composition live in the stream and no decoder for
  them has been written, so they keep the `415` refusal and are offered as a
  burn rather than being advertised and then failing inside the extractor. The
  message names the format.
- **VobSub needs a container that carries the palette.** A bare MPEG-PS stream
  has none, and the extraction says so rather than rendering every pixel
  transparent; a disc rip's `.idx` sidecar is the ordinary source of one.
- **It is OCR, so it can be wrong.** Recognition of a small or unusual font can
  misread a word, and a picture that is not text can be read as some. The
  fixtures are sized so the recogniser reads them exactly; a real disc's subtitles
  have not been tried here.
- **A burn is irreversible for the session.** Turning the subtitles off asks the
  server for a session without the burn, which is another re-encode, not a
  toggle. The player does this at the current position. An OCR'd track turns off
  instantly.
- **A text track named for burning is not burned.** It is delivered as a
  selectable track instead — better in every way — and the reasons say so.
- **A stream index the file does not have is a `400`** listing the tracks it
  does, and a text track named for burning is a `400` too, because that is a
  category error rather than an unsupported one.

`internal/testfixtures/pgs` writes the PGS fixtures the tests use, because no
image-subtitle sample ships with the project and ffmpeg cannot encode a bitmap
subtitle from text; `scripts/pgsgen` exposes it for browser fixtures, and
`-text` makes it draw real letters so the OCR path has something to read.

## Adaptive bitrate

**A quality choice is a ceiling, and a pin is a separate request.** Three height
fields, three meanings:

- `preferred_height` — "adapt, but do not go above this." The client gets a
  **ladder topped at that height** (clipped by its own box and the source), and
  its player is free to step down when the network cannot sustain the top rung.
  This is what the quality menu sends when a viewer picks a setting.
- `max_height` **alone** — "give me exactly this." One rendition, no ladder. It
  is the deterministic request, and on a small host it is also the cheap one: a
  ladder is up to three encodes, a pin is one.
- **neither** — "adapt as far as my box allows." A ladder topped at the client's
  own ceiling or the source.

`max_height` alongside `preferred_height` is the hard ceiling that ladder stays
under, so a manifest can say "my screen is 1080" and "I chose 720" at once. A
`burn_subtitle_index` still pins one rendition whatever the heights say, because
the bitmap is composited once; the preference then chooses that single encode's
height.

A ladder has up to three rungs — the target height, two thirds of it, and half —
each with its own encoder settings, its own VBV ceiling from a conventional
bitrate table scaled by the client's own limit, and its own forced segment
boundaries. One ffmpeg process produces all of them, and the client is handed
`master.m3u8`; a player that understands HLS then switches rungs on its own. The
decision reports the rungs as `renditions`, and `target_height` and
`target_bitrate_kbps` describe the top one, so a client that reads only those
still sees a coherent answer. Every decision explains itself in `reasons`,
including which height field chose the top rung.

Rungs that would be too small to encode or too close to the rung above to be a
real choice are dropped, and a source with no room below it gets no ladder at
all rather than two renditions nobody can tell apart. Each rung is a separate
encode of the same source, so a ladder costs roughly what its rungs add up to —
on a small host, pinning a height is the cheaper way to watch.

**A ladder re-encodes the audio, once per rung.** ffmpeg's HLS muxer refuses to
put one copied elementary stream in two variants (`Same elementary stream found
more than once in two different variant definitions`), so an audio track that a
single-rendition session would simply copy has to be encoded for each rung. On
ffmpeg 5.1 and 6.1 — the versions Debian bookworm and Ubuntu ship, including this
project's container — that refusal is not something the session survives: the
master playlist comes out with fewer rungs than the decision promised and a
client never sees the lower ones. ffmpeg 9 tolerates it, which is why this was
invisible on the development host until CI ran the ladder test against a
different version. The extra cost is audio encodes, which are cheap next to the
video ones.

One honest caveat: a VBV ceiling bounds the average, not every instant. A buffer
twice the ceiling lets the encoder spend what it has saved, so over a segment
shorter than the buffer the measured rate can exceed the ceiling — on a
six-second test segment a 500 kbps limit measured 632 kbps muxed, while the same
source unlimited measured 3.3 Mbps. Over a real stream the average converges.
