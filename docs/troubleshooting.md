# Troubleshooting

Every failure in this list is one the server reports on purpose, with a code or a
log line that names it. Start with the code: `{"code": "..."}` in an API response,
or `code=` in the request log. The sections below are in the order people
usually meet them.

## `403 untrusted_source`

**Symptom.** A request through a reverse proxy answers `403` with
`"code": "untrusted_source"` and `"requests must arrive through the configured
access proxy"`, or `"the client address could not be determined"`.

**What it means.** In `--auth-mode proxy` the gate believes an identity header
only from an address `--trusted-proxy` names. The header may be present and
perfectly valid; it is still not believable from anywhere else. The refusal is
deliberately not more specific, because telling an untrusted caller how to become
trusted is the thing this check exists to prevent.

**The IPv6 trap.** A single prefix does not cover both loopback forms. A proxy
that connects over IPv6 arrives as `::1`, which `127.0.0.1/32` does not contain,
so the request is refused for a reason that has nothing to do with the header:

```sh
# Refuses a proxy that dials ::1
--trusted-proxy 127.0.0.1/32

# Covers both
--trusted-proxy 127.0.0.1/32,::1/128
```

`TestTrustedProxy_AddressForms` pins all four combinations. Two things make this
trap look intermittent in practice:

- The IPv4-mapped form is **not** a trap: `::ffff:127.0.0.1` is unmapped to
  `127.0.0.1` before any comparison, so the IPv4 prefix matches it. The same proxy
  can produce either form depending on how it dials.
- `::1/128` alone refuses an IPv4 proxy, symmetrically. List both unless you know
  which family your proxy uses.

**Check what the server sees.** Every admitted request logs its peer address, so
`journalctl -u astraeus | grep 'http request'` shows what arrived. A refusal is
not in that log — the gate answers before it — which is why the denial is counted
by `astraeus_auth_denied_total{reason="untrusted_source"}` instead.

## `415 subtitle_format_unsupported`

**Symptom.** A subtitle request for an image-based track answers `415` with a
message naming the codec.

**Two causes, and the message says which.**

- `"this server has no OCR engine installed to read it"` — the track is a picture
  (PGS or VobSub) and reading it needs `tesseract`. Install it, or point at one
  with `--tesseract-bin`, and restart: the refusal is decided at startup, not per
  request.
- `"the OCR reader has no decoder for it (X is burn-only)"` — an engine is
  installed but this codec is `dvb_subtitle`, which the reader has no decoder for.
  This is not a misconfiguration; burn it in instead.

**Is the track image-based at all?** `GET /api/system/capabilities` in
`image_subtitles` says whether text can be read on this host, and the playback
response omits a URL for a track it cannot serve. The 415 falls back rather than
failing: an unreadable image track stays available for burn-in and the session
keeps working.

## Streaming fails with `stream_start_failed`

**Symptom.** Playback negotiation succeeds, then the session start answers `500`
with `"code": "stream_start_failed"`.

**What to read.** The message is ffmpeg's own complaint, and the startup log is
the other half. `500` here means ffmpeg was started and exited, which is usually a
disagreement about the source rather than the request:

- **A mis-tagged HDR source** is the common case. A file the probe reports as HDR
  whose stream is not, or the reverse, makes the filter chain fail on the first
  frame. `ffprobe -v error -show_streams <file>` is the second opinion.
- **A `409` instead of a `500`** means the server rejected the plan before ffmpeg
  ran — an unknown codec name, or one this host cannot encode. That is a different
  problem with a different fix, and it is deliberately not a `500`.
- **`429 too_many_sessions`** means exactly that: `--max-sessions` is reached.
  Retry later, or raise it. It is a refusal, not a fault.

Playback never silently does something else. If a burn-in cannot work, the
startup log says so and the URL is withheld rather than handing the client a track
that will not render.

## The browser stalls instead of failing

**Symptom.** Playback starts, then the player freezes with no error. No API call
fails.

**What it means.** A capability manifest that omits `max_audio_channels` or
`max_bit_depth` is treated as **unrestricted**, so the server may hand the client
a stream it cannot decode — a 5.1 AAC track to a browser that only manages
stereo. The `<video>` element does not error; it waits forever.

Send the fields, or send no body at all and let the built-in browser profile
apply. `docs/playback.md` documents the manifest and the profile's caps
(1920x1080, 8-bit, stereo). This is the one failure in this list that the server
cannot detect, because the client never tells it what went wrong.

## Hardware transcoding is not being used

**Symptom.** Transcoding happens on the CPU when a GPU is present.

**Read the startup log.** Every encoder the probe turned down is logged with the
reason, and the only thing that decides whether a family is used is that probe —
not `ffmpeg -encoders`, which lists what was *compiled in*:

```
level=WARN msg="hardware encoder rejected" encoder=h264_vaapi reason="..."
level=WARN msg="no hardware encoder is usable on this host; transcoding will use the CPU" rejected=6
```

Common causes, in the order they are usually true:

- **No render node.** `/dev/dri` is absent or empty. In a container, `--device
  /dev/dri` is required, and the image ships ffmpeg's VAAPI support but not the
  vendor userspace driver.
- **The userspace driver is missing.** Intel and AMD both need their VA-API
  driver package installed on the host; ffmpeg alone is not enough.
- **The encoder cannot hold the bitrate ceiling.** A family whose own probe shows
  it ignoring `-maxrate` is rejected at startup, because an unenforced limit is
  worse than a slower one. The reason string says so.
- **`--device-dir` points elsewhere.** It defaults to `/dev/dri`.

Five families are probed — VAAPI, QuickSync, NVENC, AMF and VideoToolbox — and
none of them has been run on real hardware in this project's own testing, so the
probe is the first real evidence either way. `deploy/README.md` covers the
container and unit settings that could stop an encoder that would otherwise work.

## Nothing here matches, or you need help

The sections above cover the failures the server names. When a problem does not
appear in them, or when the answer is "it is slow" rather than "it refused", the
useful next step is a bundle rather than a paragraph of description.

`scripts/collect-astraeus-diagnostics.sh` gathers the logs, metrics, API
responses and host or container state into one `.tar.gz`, and
`scripts/analyze-astraeus-bundle.py` turns that into a Markdown report with a
verdict and the specific findings it bears on:

```sh
# with the server already running; it detects whether that is a container or a binary
scripts/collect-astraeus-diagnostics.sh

# then, anywhere
python3 scripts/analyze-astraeus-bundle.py astraeus-diagnostics-*.tar.gz
```

The collector is read-only with respect to the running instance: it never
restarts, reconfigures or stops anything. Both scripts work against a binary
deployment and a container one, and the report reads the same either way.
[`scripts/README-diagnostics.md`](https://github.com/ykzird/astraeus/blob/main/scripts/README-diagnostics.md)
covers the environment variables, what each part of the bundle is for, and how to
read the report.
