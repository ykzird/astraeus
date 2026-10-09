# Astraeus diagnostics report

- Bundle: `astraeus-diag-intel-nvidia-EXAMPLE.tar.gz`
- Collected: 20261009T220000Z
- Deployment: a container
- Container: `astraeus` on `testhost`
- Image: `astraeus-media:dev` (version label: `dev`)
- Server started: 2026-10-09T21:00:00Z
- Kernel: Linux 6.12.0 x86_64
- ffmpeg: ffmpeg version 5.1.9-0+deb12u1 Copyright (c) 2000-2023 the FFmpeg developers

## Hardware encoder probe

`GET /api/system/capabilities` reported:

| field | value |
| --- | --- |
| `ffmpeg` | `True` |
| `ffprobe` | `True` |
| `image_subtitles` | `read as text` |
| `subtitle_ocr_enabled` | `True` |
| `render_node` | `/dev/dri/renderD128` |
| `hardware_acceleration` | `[]` |
| `video_encoders` | `libaom-av1`, `libsvtav1`, `libvpx-vp9`, `libx264`, `libx265`, `aac` |

**The startup log mentions an accepted encoder (`h264_vaapi`, `hevc_vaapi`) but the API's `hardware_acceleration` is empty.** The API is authoritative for what the server will negotiate, so treat this as not accepted — and keep the log line, because the two disagreeing is itself worth knowing.
**Render node:** `/dev/dri/renderD128`

Rejections, with ffmpeg's own complaint and what it means:

| encoder | cause | ffmpeg said |
| --- | --- | --- |
| `h264_nvenc` | missing userspace driver | exit status 1: Cannot load libnvidia-encode.so.1 |
| `h264_qsv` | device not passed in | Device creation failed: -2 |
| `h264_vaapi` | missing userspace driver | exit status 1: Failed to initialise VAAPI connection: -1 (unknown libva error). |
| `hevc_nvenc` | missing userspace driver | exit status 1: Cannot load libnvidia-encode.so.1 |
| `hevc_vaapi` | missing userspace driver | exit status 1: Failed to initialise VAAPI connection: -1 (unknown libva error). |

### What each failure needs

- **device not passed in** (`h264_qsv`): ffmpeg could not create the hardware device. Check that the right device is passed in for this family and that its userspace driver is installed.
- **missing userspace driver** (`h264_nvenc`, `hevc_nvenc`): the NVIDIA encode library (`libnvidia-encode.so`, and `libcuda.so` behind it) cannot be loaded. This is separate from the device: `/dev/nvidia*` can be present and the library still absent. The base image is Debian with no CUDA userspace, so those libraries have to be injected by the NVIDIA container toolkit — the container needs `--gpus all` (or `--runtime=nvidia`), and the toolkit must be installed on the host. Confirm with `nvidia-smi` *inside* the container; if that works, the device plumbing is right and what is missing is specifically the encode library.
- **missing userspace driver** (`h264_vaapi`, `hevc_vaapi`): the device is reachable but libva has no vendor backend for it. This is a driver-package gap, not a missing device: the runtime image ships ffmpeg's VAAPI support but deliberately no `*_drv_video.so`. Install the driver for the GPU that owns the render node — `intel-media-va-driver-non-free` or `i965-va-driver` for Intel, `mesa-va-drivers` for AMD or older Intel — or rebuild the image with it. Mounting `/dev/dri` alone will never be enough.

`rejected_encoders` from the API (verbatim):

| encoder | reason |
| --- | --- |
| `h264_nvenc` | exit status 1: Cannot load libnvidia-encode.so.1 |
| `hevc_nvenc` | exit status 1: Cannot load libnvidia-encode.so.1 |
| `h264_vaapi` | exit status 1: Failed to initialise VAAPI connection: -1 (unknown libva error). |
| `hevc_vaapi` | exit status 1: Failed to initialise VAAPI connection: -1 (unknown libva error). |

## GPU visibility: host vs container

```
host render nodes: by-path card0 renderD128 
host nvidia devices: /dev/nvidia0 /dev/nvidiactl /dev/nvidia-uvm 
host nvidia-smi: NVIDIA GeForce RTX 4070 Ti, 550.127.05
host docker runtimes: {"runc":{"path":"runc"},"nvidia":{"path":"nvidia-container-runtime"}}
container devices (HostConfig.Devices): [{"PathOnHost":"/dev/dri","PathInContainer":"/dev/dri","CgroupPermissions":"rwm"}]
container device requests (HostConfig.DeviceRequests): [{"Driver":"","Count":-1,"DeviceIDs":null,"Capabilities":[["gpu"]]}]
container runtime: runc
```

> **The render node is mounted and VAAPI still fails.** The device is not the problem — `libva` has no vendor backend inside the container. The runtime image deliberately ships ffmpeg's VAAPI support but not the userspace driver, so this is expected unless the image or the container adds one. Note this failure mode means mounting `/dev/dri` is necessary but *not* sufficient.
- The container can see NVIDIA device nodes, so the toolkit is configured. Read the NVENC rejection above for what ffmpeg said.
- NVENC failed for a driver reason rather than a device one: the nodes are visible, so what is missing is the injected userspace library, not the `--gpus` argument.

### Userspace drivers inside the container

- **VAAPI backend**: `libva`
- **NVIDIA driver libraries**: none found
  - `libva` itself is present but no vendor backend is, which is exactly the state that makes VAAPI fail with an unknown libva error.

The raw listing is in `inside/vaapi-drivers.txt` and `inside/nvidia-libs.txt` in the bundle.

## Metrics

1 `astraeus_*` metric families are declared at startup, and 16 carry a sample on this instance.

| metric | value | declared | what it means here |
| --- | --- | --- | --- |
| `astraeus_http_requests_total` | 416 | yes | requests served |
| `astraeus_playback_decisions_total` | 11 | no | playback negotiations, by chosen mode |
| `astraeus_stream_sessions_total` | 6 | no | stream sessions, by mode and outcome |
| `astraeus_stream_errors_total` | 2 | no | stream failures |
| `astraeus_transcode_fallbacks_total` | 3 | no | hardware transcodes retried in software |
| `astraeus_transcode_startup_seconds_count` | 4 | no | transcodes that reached a playlist |
| `astraeus_first_segment_seconds_count` | 2 | no | streams that delivered a first segment |
| `astraeus_stream_sessions_active` | 1 | no | sessions active now |
| `astraeus_probe_errors_total` | 0 | no | probe failures |
| `astraeus_scan_runs_total` | 2 | no | scans run |
| `astraeus_scan_files_total` | 812 | no | files seen by scans |
| `astraeus_metadata_lookup_seconds_count` | 55 | no | metadata lookups |
| `astraeus_auth_denied_total` | 0 | no | gate denials |
| `astraeus_auth_granted_total` | 98 | no | gate grants |
| `astraeus_rate_limited_total` | 0 | no | requests refused by the rate limiter |
| `astraeus_spans_dropped_total` | 0 | no | trace spans dropped |

Samples with no `# TYPE` line: `astraeus_auth_denied_total`, `astraeus_auth_granted_total`, `astraeus_first_segment_seconds_count`, `astraeus_metadata_lookup_seconds_count`, `astraeus_playback_decisions_total`, `astraeus_probe_errors_total`, `astraeus_rate_limited_total`, `astraeus_scan_files_total`, `astraeus_scan_runs_total`, `astraeus_spans_dropped_total`, `astraeus_stream_errors_total`, `astraeus_stream_sessions_active`, `astraeus_stream_sessions_total`, `astraeus_transcode_fallbacks_total`, `astraeus_transcode_startup_seconds_count`

- `astraeus_playback_decisions_total`: `direct_play`=7, `transcode`=4
- `astraeus_stream_sessions_total`: `error`=2, `started`=4
- `astraeus_stream_errors_total`: `transcode`=2
- `astraeus_metadata_lookup_seconds_count`: `tmdb`=55

> `transcode_fallbacks_total` is **3** against 6 sessions started. A fallback means a hardware attempt failed and was retried in software — the direct evidence that the hardware path is not working.

> **1 stream session(s) were active when this was scraped.** The URL in `state/` and the IDs under `/api/streams` are what to poke while it is still running.

## Review findings this bundle bears on

`astraeus_http_requests_total` currently carries 2 distinct `method` label value(s): `GET`, `PROPFIND`. Nothing bounds that set — a client that sends a novel method adds a permanent series (A-4).

| finding | what it is | matched |
| --- | --- | --- |
| **S-5** | VAAPI subtitle burn-in filter graph failure | 1+ lines containing `Impossible to convert between the formats` |
| **S-9** | a dead ffmpeg strands its session | 2+ lines containing `stream_start_failed / 410 / encoder exited` |
| **L-4** | scans fail with `database is locked` | 1+ lines containing `database is locked` |
| **L-13** | OCR budget and coordination | 1+ lines containing `ocr / tesseract timing` |

### S-5 — VAAPI subtitle burn-in filter graph failure

```
2026-10-09T21:06:00.000000000Z level=ERROR msg="stream start failed" code=stream_start_failed error="Impossible to convert between the formats supported by the filter 'Parsed_scale2ref_3'"
```

### S-9 — a dead ffmpeg strands its session

```
2026-10-09T21:06:00.000000000Z level=ERROR msg="stream start failed" code=stream_start_failed error="Impossible to convert between the formats supported by the filter 'Parsed_scale2ref_3'"
```
```
2026-10-09T21:10:00.000000000Z level=ERROR msg="encoder exited" session=abc mode=transcode
```

### L-4 — scans fail with `database is locked`

```
2026-10-09T21:08:00.000000000Z level=ERROR msg="scan failed" error="creating entity \"Film 000\": database is locked (517)"
```

### L-13 — OCR budget and coordination

```
2026-10-09T21:09:00.000000000Z level=INFO msg="ocr pass" track=2 cues=412 tesseract=tesseract
```


## Verdict

- device not passed in — `h264_qsv` rejected for that reason
- missing userspace driver — `h264_nvenc`, `hevc_nvenc` rejected for that reason
- missing userspace driver — `h264_vaapi`, `hevc_vaapi` rejected for that reason
- `/dev/dri` is visible but the VAAPI userspace driver is missing inside the container, so VAAPI is rejected for a driver reason rather than a device reason

### What this bundle cannot answer

- the startup log and `/api/system/capabilities` disagree about which encoder was accepted
- the GPU-side evidence is a *failure to initialise* rather than a successful encode, so it does not yet show that a hardware encode works on this host. Getting one accepted family is the higher-value result: the project has never run any of them on real hardware
