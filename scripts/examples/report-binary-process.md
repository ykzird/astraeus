# Astraeus diagnostics report

- Bundle: `astraeus-diag-omarchy-20261009T205909Z.tar.gz`
- Collected: 20261009T205909Z
- Deployment: a plain binary (not a container)
- Process: pid `2229363` on `omarchy`
- Binary: `/home/jok/Work/deepseek-harness/astraeus-media/astraeus-server`
- Working directory: `/home/jok/Work/deepseek-harness/astraeus-media`
- Command line: `./astraeus-server serve --db astra.db`
- Uptime: 6363s
- Kernel: Linux 7.2.5-3-omarchy x86_64
- ffmpeg: ffmpeg version n9.0.1 Copyright (c) 2000-2026 the FFmpeg developers

> **No server log was captured.** A binary started from a terminal writes to a pty, which keeps no history, so there was nothing to read.
> Its stderr went to `/dev/pts/1`. To make logs collectable next time, redirect the process to a file (`> astraeus.log 2>&1`) or run it under systemd and let `journalctl` hold them.

## Hardware encoder probe

`GET /api/system/capabilities` reported:

| field | value |
| --- | --- |
| `ffmpeg_available` | `True` |
| `ffprobe_available` | `True` |
| `subtitle_ocr_enabled` | `True` |
| `render_node` | `/dev/dri/renderD128` |
| `hardware_acceleration` | `['vaapi']` |
| `video_encoders` | `h264_vaapi`, `hevc_vaapi`, `libaom-av1`, `libsvtav1`, `libvpx-vp9`, `libx264`, `libx265` |
| `hdr_video_encoders` | `hevc_vaapi`/p010le, `libaom-av1`/yuv420p10le, `libsvtav1`/yuv420p10le, `libvpx-vp9`/yuv420p10le, `libx265`/yuv420p10le |

**Hardware acceleration accepted by the probe:** `vaapi`
**Render node:** `/dev/dri/renderD128`

Rejections, with ffmpeg's own complaint and what it means:

| encoder | cause | ffmpeg said |
| --- | --- | --- |
| `av1_amf` | reason not captured | exit status 171: Input #0, lavfi, from 'testsrc=size=320x240:rate=10:duration=1': |
| `av1_nvenc` | reason not captured | exit status 255: Input #0, lavfi, from 'testsrc=size=320x240:rate=10:duration=1': |
| `av1_qsv` | reason not captured | exit status 171: Input #0, lavfi, from 'testsrc=size=320x240:rate=10:duration=1': |
| `av1_vaapi` | reason not captured | exit status 218: Input #0, lavfi, from 'testsrc=size=320x240:rate=10:duration=1': |
| `h264_amf` | reason not captured | exit status 171: Input #0, lavfi, from 'testsrc=size=320x240:rate=10:duration=1': |
| `h264_nvenc` | reason not captured | exit status 255: Input #0, lavfi, from 'testsrc=size=320x240:rate=10:duration=1': |
| `h264_qsv` | reason not captured | exit status 171: Input #0, lavfi, from 'testsrc=size=320x240:rate=10:duration=1': |
| `hevc_amf` | reason not captured | exit status 171: Input #0, lavfi, from 'testsrc=size=320x240:rate=10:duration=1': |
| `hevc_nvenc` | reason not captured | exit status 255: Input #0, lavfi, from 'testsrc=size=320x240:rate=10:duration=1': |
| `hevc_qsv` | reason not captured | exit status 171: Input #0, lavfi, from 'testsrc=size=320x240:rate=10:duration=1': |

### What each failure needs

- **reason not captured** (`av1_amf`, `av1_nvenc`, `av1_qsv`, `av1_vaapi`, `h264_amf`, `h264_nvenc`, `h264_qsv`, `hevc_amf`, `hevc_nvenc`, `hevc_qsv`): the recorded reason is ffmpeg's input banner, not its error, so this rejection does not say why the encoder failed. The real message is in the same ffmpeg run's stderr, which the probe does not keep. Work around it by running the same command by hand inside the container with `ffmpeg -loglevel error ...`.

`rejected_encoders` from the API (verbatim):

| encoder | reason |
| --- | --- |
| `av1_amf` | exit status 171: Input #0, lavfi, from 'testsrc=size=320x240:rate=10:duration=1': |
| `av1_nvenc` | exit status 255: Input #0, lavfi, from 'testsrc=size=320x240:rate=10:duration=1': |
| `av1_qsv` | exit status 171: Input #0, lavfi, from 'testsrc=size=320x240:rate=10:duration=1': |
| `av1_vaapi` | exit status 218: Input #0, lavfi, from 'testsrc=size=320x240:rate=10:duration=1': |
| `h264_amf` | exit status 171: Input #0, lavfi, from 'testsrc=size=320x240:rate=10:duration=1': |
| `h264_nvenc` | exit status 255: Input #0, lavfi, from 'testsrc=size=320x240:rate=10:duration=1': |
| `h264_qsv` | exit status 171: Input #0, lavfi, from 'testsrc=size=320x240:rate=10:duration=1': |
| `hevc_amf` | exit status 171: Input #0, lavfi, from 'testsrc=size=320x240:rate=10:duration=1': |
| `hevc_nvenc` | exit status 255: Input #0, lavfi, from 'testsrc=size=320x240:rate=10:duration=1': |
| `hevc_qsv` | exit status 171: Input #0, lavfi, from 'testsrc=size=320x240:rate=10:duration=1': |

## GPU visibility: host vs container

```
deployment: process
host render nodes: by-path card0 card1 renderD128 renderD129 
host nvidia devices: 
host nvidia-smi: absent
container devices (HostConfig.Devices): n/a (binary deployment, no container)
container device requests (HostConfig.DeviceRequests): n/a
container runtime: n/a
```


### Userspace drivers inside the container

- **VAAPI backend**: `radeonsi_drv_video.so` (AMD), `nouveau_drv_video.so`
- **NVIDIA driver libraries**: none found

The raw listing is in `inside/vaapi-drivers.txt` and `inside/nvidia-libs.txt` in the bundle.

## Metrics

18 `astraeus_*` metric families are declared at startup, and 20 carry a sample on this instance.

| metric | value | declared | what it means here |
| --- | --- | --- | --- |
| `astraeus_http_requests_total` | 2,936 | yes | requests served |
| `astraeus_playback_decisions_total` | 4 | yes | playback negotiations, by chosen mode |
| `astraeus_stream_sessions_total` | 8 | yes | stream sessions, by mode and outcome |
| `astraeus_stream_errors_total` | 4 | yes | stream failures |
| `astraeus_transcode_fallbacks_total` | 4 | yes | hardware transcodes retried in software |
| `astraeus_transcode_startup_seconds_count` | 4 | yes | transcodes that reached a playlist |
| `astraeus_first_segment_seconds_count` | 4 | yes | streams that delivered a first segment |
| `astraeus_stream_sessions_active` | 1 | yes | sessions active now |
| `astraeus_probe_errors_total` | — | yes | probe failures |
| `astraeus_scan_runs_total` | 1 | yes | scans run |
| `astraeus_scan_files_total` | 4 | yes | files seen by scans |
| `astraeus_metadata_lookup_seconds_count` | — | yes | metadata lookups |
| `astraeus_auth_denied_total` | — | yes | gate denials |
| `astraeus_auth_granted_total` | — | yes | gate grants |
| `astraeus_rate_limited_total` | — | yes | requests refused by the rate limiter |
| `astraeus_spans_dropped_total` | — | yes | trace spans dropped |

Declared but with no sample yet (10): `astraeus_auth_denied_total`, `astraeus_auth_granted_total`, `astraeus_first_segment_seconds`, `astraeus_http_request_seconds`, `astraeus_metadata_lookup_seconds`, `astraeus_probe_errors_total`, `astraeus_rate_limited_total`, `astraeus_scan_seconds`, `astraeus_spans_dropped_total`, `astraeus_transcode_startup_seconds`

- `astraeus_playback_decisions_total`: `transcode`=4
- `astraeus_stream_sessions_total`: `failed`=4, `started`=4
- `astraeus_stream_errors_total`: `transcode`=4

> `transcode_fallbacks_total` is **4** against 8 sessions started. A fallback means a hardware attempt failed and was retried in software — the direct evidence that the hardware path is not working.

> **1 stream session(s) were active when this was scraped.** The URL in `state/` and the IDs under `/api/streams` are what to poke while it is still running.

## Running ffmpeg command line

No ffmpeg process was running when the bundle was collected, so the arguments the server actually passed are not in evidence. Collect again while a transcode is live to capture them — this is the strongest single artifact for the streaming findings.

## Review findings this bundle bears on

`astraeus_http_requests_total` currently carries 4 distinct `method` label value(s): `DELETE`, `GET`, `POST`, `PUT`. Nothing bounds that set — a client that sends a novel method adds a permanent series (A-4).

No line in the captured log matches any of the finding signatures. That is a real result for the findings whose symptom is a log line, and says nothing about the ones that are not.


## Verdict

- the probe accepted `vaapi` — this is real-hardware evidence, which the project has not had for any family before
- reason not captured — `av1_amf`, `av1_nvenc`, `av1_qsv`, `av1_vaapi`, `h264_amf`, `h264_nvenc`, `h264_qsv`, `hevc_amf`, `hevc_nvenc`, `hevc_qsv` rejected for that reason

### What this bundle cannot answer

- no server log was captured, so nothing below can cite a log line; the metrics and the API responses still stand
- the probe accepted a hardware encoder, but this bundle has no session that actually used it. A bundle collected during a hardware transcode is what shows the ceiling holding and the arguments the real session got (S-1, S-15)
