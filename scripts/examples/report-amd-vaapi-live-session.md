# Astraeus diagnostics report

- Bundle: `astraeus-diag-omarchy-20261009T203518Z.tar.gz`
- Collected: 20261009T203518Z
- Deployment: a container
- Container: `astraeus-diag-e2e` on `omarchy`
- Image: `astraeus-diag-test:latest` (version label: `dev`)
- Server started: 2026-10-09T20:33:52.999190156Z
- Kernel: Linux 7.2.5-3-omarchy x86_64
- ffmpeg: ffmpeg version 5.1.9-0+deb12u1 Copyright (c) 2000-2026 the FFmpeg developers

## Hardware encoder probe

`GET /api/system/capabilities` reported:

| field | value |
| --- | --- |
| `ffmpeg_available` | `True` |
| `ffprobe_available` | `True` |
| `subtitle_ocr_enabled` | `True` |
| `video_encoders` | `libaom-av1`, `libsvtav1`, `libvpx-vp9`, `libx264`, `libx265` |
| `hdr_video_encoders` | `libaom-av1`/yuv420p10le, `libsvtav1`/yuv420p10le, `libvpx-vp9`/yuv420p10le, `libx265`/yuv420p10le |

**No hardware encoder was accepted by the probe.**
**Render node:** none recorded

Rejections, with ffmpeg's own complaint and what it means:

| encoder | cause | ffmpeg said |
| --- | --- | --- |
| `h264_nvenc` | reason not captured | exit status 1: Input #0, lavfi, from 'testsrc=size=320x240:rate=10:duration=1': |
| `h264_qsv` | reason not captured | exit status 1: Input #0, lavfi, from 'testsrc=size=320x240:rate=10:duration=1': |
| `h264_vaapi` | reason not captured | exit status 1: Input #0, lavfi, from 'testsrc=size=320x240:rate=10:duration=1': |
| `hevc_nvenc` | reason not captured | exit status 1: Input #0, lavfi, from 'testsrc=size=320x240:rate=10:duration=1': |
| `hevc_qsv` | reason not captured | exit status 1: Input #0, lavfi, from 'testsrc=size=320x240:rate=10:duration=1': |
| `hevc_vaapi` | reason not captured | exit status 1: Input #0, lavfi, from 'testsrc=size=320x240:rate=10:duration=1': |

### What each failure needs

- **reason not captured** (`h264_nvenc`, `h264_qsv`, `h264_vaapi`, `hevc_nvenc`, `hevc_qsv`, `hevc_vaapi`): the recorded reason is ffmpeg's input banner, not its error, so this rejection does not say why the encoder failed. The real message is in the same ffmpeg run's stderr, which the probe does not keep. Work around it by running the same command by hand inside the container with `ffmpeg -loglevel error ...`.

`rejected_encoders` from the API (verbatim):

| encoder | reason |
| --- | --- |
| `h264_nvenc` | exit status 1: Input #0, lavfi, from 'testsrc=size=320x240:rate=10:duration=1': |
| `h264_qsv` | exit status 1: Input #0, lavfi, from 'testsrc=size=320x240:rate=10:duration=1': |
| `h264_vaapi` | exit status 1: Input #0, lavfi, from 'testsrc=size=320x240:rate=10:duration=1': |
| `hevc_nvenc` | exit status 1: Input #0, lavfi, from 'testsrc=size=320x240:rate=10:duration=1': |
| `hevc_qsv` | exit status 1: Input #0, lavfi, from 'testsrc=size=320x240:rate=10:duration=1': |
| `hevc_vaapi` | exit status 1: Input #0, lavfi, from 'testsrc=size=320x240:rate=10:duration=1': |

## GPU visibility: host vs container

```
host render nodes: 
host nvidia devices: 
host nvidia-smi: absent
host docker runtimes: {"io.containerd.runc.v2":{"path":"runc","status":{"org.opencontainers.runtime-spec.features":"{\"ociVersionMin\":\"1.0.0\",\"ociVersionMax\":\"1.3.0\",\"hooks\":[\"prestart\",\"c …[truncated]
container devices (HostConfig.Devices): []
container device requests (HostConfig.DeviceRequests): null
container runtime: runc
```

Long values above are truncated here; the untruncated file is `host/gpu-visibility.txt` in the bundle.

- The host itself reports no render node and no NVIDIA device, so no hardware encoder could have worked here. CPU transcoding is the correct outcome on this machine, not a bug.

### Userspace drivers inside the container

- **VAAPI backend**: none found
- **NVIDIA driver libraries**: none found

The raw listing is in `inside/vaapi-drivers.txt` and `inside/nvidia-libs.txt` in the bundle.

## Metrics

18 `astraeus_*` metric families are declared at startup, and 13 carry a sample on this instance.

| metric | value | declared | what it means here |
| --- | --- | --- | --- |
| `astraeus_http_requests_total` | 27 | yes | requests served |
| `astraeus_playback_decisions_total` | 3 | yes | playback negotiations, by chosen mode |
| `astraeus_stream_sessions_total` | 3 | yes | stream sessions, by mode and outcome |
| `astraeus_stream_errors_total` | — | yes | stream failures |
| `astraeus_transcode_fallbacks_total` | — | yes | hardware transcodes retried in software |
| `astraeus_transcode_startup_seconds_count` | 3 | yes | transcodes that reached a playlist |
| `astraeus_first_segment_seconds_count` | 1 | yes | streams that delivered a first segment |
| `astraeus_stream_sessions_active` | 3 | yes | sessions active now |
| `astraeus_probe_errors_total` | — | yes | probe failures |
| `astraeus_scan_runs_total` | — | yes | scans run |
| `astraeus_scan_files_total` | — | yes | files seen by scans |
| `astraeus_metadata_lookup_seconds_count` | — | yes | metadata lookups |
| `astraeus_auth_denied_total` | — | yes | gate denials |
| `astraeus_auth_granted_total` | — | yes | gate grants |
| `astraeus_rate_limited_total` | — | yes | requests refused by the rate limiter |
| `astraeus_spans_dropped_total` | — | yes | trace spans dropped |

Declared but with no sample yet (14): `astraeus_auth_denied_total`, `astraeus_auth_granted_total`, `astraeus_first_segment_seconds`, `astraeus_http_request_seconds`, `astraeus_metadata_lookup_seconds`, `astraeus_probe_errors_total`, `astraeus_rate_limited_total`, `astraeus_scan_files_total`, `astraeus_scan_runs_total`, `astraeus_scan_seconds`, `astraeus_spans_dropped_total`, `astraeus_stream_errors_total` …

- `astraeus_playback_decisions_total`: `transcode`=3
- `astraeus_stream_sessions_total`: `started`=3

> **3 stream session(s) were active when this was scraped.** The URL in `state/` and the IDs under `/api/streams` are what to poke while it is still running.

## Running ffmpeg command line

A session was live, and this is the command the server built:

```
1001 ffmpeg -hide_banner -loglevel error -y -protocol_whitelist file,pipe,data,crypto -i /media/movies/Test Movie (2024).mp4 -map 0:v:0 -map 0:1? -c:v libvpx-vp9 -crf 31 -b:v 0 -vf scale=trunc(iw/2)*2:trunc(ih/2)*2 -pix_fmt yuv420p -force_key_frames expr:gte(t,n_forced*6) -c:a libopus -b:a 192k -f hls -hls_time 6 -hls_list_size 0 -hls_playlist_type event -hls_segment_filename /data/streams/e84d0bab-c6e5-4260-ba0a-fee244617c3c/seg%05d.ts /data/streams/e84d0bab-c6e5-4260-ba0a-fee244617c3c/playlist.m3u8
```

| finding | observation | note |
| --- | --- | --- |
| **S-17** | `present` | `-protocol_whitelist` is present, which is the hardening the review could not reproduce a risk for |
| **S-15** | `absent` | forced-IDR: the spelling matters (NVENC `-forced-idr`, QSV/AMF `-forced_idr`), and software/VAAPI are deliberately given none |
| **S-1** | `present` | a bitrate target or ceiling is passed, which is what made the limit bite |
| **S-5** | `present` | no software compositor is being handed hardware frames |
| **encoder choice** | `present` | the video encoder chosen was `libvpx-vp9` |


## Review findings this bundle bears on

`astraeus_http_requests_total` currently carries 2 distinct `method` label value(s): `GET`, `POST`. Nothing bounds that set — a client that sends a novel method adds a permanent series (A-4).

| finding | what it is | matched |
| --- | --- | --- |
| **L-13** | OCR budget and coordination | 1+ lines containing `ocr / tesseract timing` |

### L-13 — OCR budget and coordination

```
2026-10-09T20:33:54.771251739Z time=2026-10-09T20:33:54.771Z level=INFO msg="subtitle conversion enabled" cache=/data/subtitles ocr=tesseract image_subtitles="read as text"
```


## Verdict

- no hardware encoder proved usable; the server is transcoding on the CPU (the log names 6 rejected families)
- reason not captured — `h264_nvenc`, `h264_qsv`, `h264_vaapi`, `hevc_nvenc`, `hevc_qsv`, `hevc_vaapi` rejected for that reason
- the host has no GPU devices at all, so CPU transcoding is correct here
