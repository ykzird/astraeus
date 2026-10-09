#!/usr/bin/env python3
"""Write a synthetic Astraeus diagnostics bundle for testing the analyzer.

This is test scaffolding, not a deliverable: it exists so the analyzer can be
exercised without a Docker host. Run it, then point the analyzer at the
directory it prints.

    make-fixture-bundle.py <dir> [--all-rejected]

`--all-rejected` makes the bundle self-consistent for the case where no
hardware encoder was accepted: the API reports an empty
`hardware_acceleration`, every encoder is in `rejected_encoders`, and no log
line claims an acceptance. The default is a host where VAAPI works.
"""
import json
import os
import sys

ALL_REJECTED = "--all-rejected" in sys.argv[2:] or "--all-rejected" in sys.argv[1:2]
_root_arg = [a for a in sys.argv[1:] if not a.startswith("--")]
root = _root_arg[0] if _root_arg else "/tmp/astfix/bundle"


def w(rel, body):
    path = os.path.join(root, rel)
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "w", encoding="utf-8") as handle:
        handle.write(body)


w("meta/collector.txt", """bundle=astraeus-diag-testhost-20261009T220000Z
collector_version=1.0.0
collected_at_utc=20261009T220000Z
collector_host=testhost
container_id=abc123
container_name=astraeus
container_image=astraeus-media:dev
container_started_at=2026-10-09T21:00:00Z
server_port=8642
sample_seconds=30
sample_interval=5
log_tail=3000
redact=1
kernel=Linux 6.12.0 x86_64
docker_version=29.7.2
""")
w("container/state.txt", """state=running
health=healthy
restart_count=0
oom_killed=false
exit_code=0
pid=12345
image_id=sha256:deadbeef
image_version_label=dev
image_revision_label=unknown
""")
w("container/env.json", json.dumps(["PATH=/usr/local/bin", "TMDB_API_KEY=<redacted>"]))
w("container/cmd.json", json.dumps(["serve", "--addr", "0.0.0.0:8642", "--db", "/data/astraeus.db"]))
w("host/host.txt", """nproc=16
cpu_model=Intel(R) Core(TM) i7-12700
mem_total=MemTotal:       32000000 kB
kernel=Linux 6.12.0 x86_64
driver_i915_loaded=1
driver_nvidia_loaded=1
""")
w("host/gpu-visibility.txt", """host render nodes: card0 renderD128
host nvidia devices: /dev/nvidia0 /dev/nvidiactl /dev/nvidia-uvm
host nvidia-smi: NVIDIA GeForce RTX 4070, 550.54.15
host docker runtimes: {"runc":{"path":"runc"},"nvidia":{"path":"nvidia-container-runtime"}}
container devices (HostConfig.Devices): [{"PathOnHost":"/dev/dri","PathInContainer":"/dev/dri"}]
container device requests (HostConfig.DeviceRequests): null
container runtime: runc
""")
w("inside/ffmpeg-version.txt", "# command: docker exec abc123 ffmpeg -version\n# exit: 0\n"
  "ffmpeg version 5.1.9-0+deb12u1 Copyright (c) 2000-2023 the FFmpeg developers\nbuilt with gcc 12\n")
w("inside/ffmpeg-devices.txt", "# command: docker exec abc123 sh -c ls -l /dev/dri\n# exit: 0\n"
  "== /dev/dri ==\n"
  "lrwxrwxrwx 1 root root 7 /dev/dri/card0 -> ../card0\ntotal 0\n"
  "crw-rw---- 1 root video 226, 128 /dev/dri/renderD128\n"
  "== /dev/dri/by-path ==\n"
  "ls: cannot access '/dev/dri/by-path': No such file or directory\n"
  "== /dev/nvidia* ==\n"
  "ls: cannot access '/dev/nvidia*': No such file or directory\n"
  "== render nodes ==\n/dev/dri/renderD128\n"
  "== nvidia nodes ==\n"
  "ls: cannot access '/dev/nvidia[0-9]*': No such file or directory\n")
w("inside/ffmpeg-encoders.txt", """# command: docker exec abc123 ffmpeg -hide_banner -encoders
 V....D h264_nvenc           NVIDIA NVENC H.264 encoder (codec h264)
 V....D h264_vaapi           H.264/AVC (VAAPI)
 V....D h264_qsv             H.264/AVC (Intel Quick Sync Video)
 V....D libx264              libx264 H.264
""")
w("inside/vaapi-drivers.txt", """# command: docker exec abc123 sh -c ...
# exit: 0
== libva ==
lrwxrwxrwx 1 root root 17 Dec 29  2022 /usr/lib/x86_64-linux-gnu/libva.so.2 -> libva.so.2.1700.0
== vaapi backends ==
/usr/bin/ls: cannot access '/usr/lib/x86_64-linux-gnu/dri/*_drv_video.so': No such file or directory
== vainfo ==
vainfo not installed
""")
w("inside/nvidia-libs.txt", """# command: docker exec abc123 sh -c ...
# exit: 0
== libnvidia-encode/libcuda ==
-- /usr/lib/x86_64-linux-gnu
/usr/bin/ls: cannot access '/usr/lib/x86_64-linux-gnu/libnvidia*': No such file or directory
-- /usr/local/nvidia/lib64
/usr/bin/ls: cannot access '/usr/local/nvidia/lib64/libnvidia*': No such file or directory
== nvidia-smi ==
nvidia-smi not in the image
""")
if ALL_REJECTED:
    # A self-consistent all-rejected bundle: nothing in hardware_acceleration,
    # every hardware family in rejected_encoders, libva present with no vendor
    # backend. This is the shape a stock image in a container produces.
    w("api/capabilities.json", json.dumps({
        "ffmpeg": True, "ffprobe": True, "image_subtitles": "read as text",
        "subtitle_ocr_enabled": True, "render_node": "/dev/dri/renderD128",
        "hardware_acceleration": [],
        "video_encoders": ["libaom-av1", "libsvtav1", "libvpx-vp9", "libx264", "libx265", "aac"],
        "rejected_encoders": [
            {"encoder": "h264_nvenc", "reason": "exit status 1: Cannot load libnvidia-encode.so.1"},
            {"encoder": "hevc_nvenc", "reason": "exit status 1: Cannot load libnvidia-encode.so.1"},
            {"encoder": "h264_vaapi", "reason": "exit status 1: Failed to initialise VAAPI connection: -1 (unknown libva error)."},
            {"encoder": "hevc_vaapi", "reason": "exit status 1: Failed to initialise VAAPI connection: -1 (unknown libva error)."},
            {"encoder": "h264_qsv", "reason": "exit status 1: Device creation failed: -2"},
        ]}, indent=2))
else:
    w("api/capabilities.json", json.dumps({
        "ffmpeg": True, "ffprobe": True, "image_subtitles": "read as text",
        "subtitle_ocr_enabled": True, "render_node": "/dev/dri/renderD128",
        "hardware_acceleration": ["vaapi"],
        "video_encoders": ["h264_vaapi", "hevc_vaapi", "libx264", "libx265", "aac"],
        "rejected_encoders": [
            {"encoder": "h264_nvenc", "reason": "Cannot load libnvidia-encode.so.1"},
            {"encoder": "h264_qsv", "reason": "Device creation failed: -2"},
        ]}, indent=2))
w("api/health.json", json.dumps({"status": "ok", "version": "0.18.0"}))
w("metrics/prometheus.txt", """# HELP astraeus_http_requests_total Requests served.
# TYPE astraeus_http_requests_total counter
astraeus_http_requests_total{method="GET",status="200"} 412
astraeus_http_requests_total{method="GET",status="404"} 3
astraeus_http_requests_total{method="PROPFIND",status="405"} 1
astraeus_playback_decisions_total{mode="direct_play"} 7
astraeus_playback_decisions_total{mode="transcode"} 4
astraeus_stream_sessions_total{mode="transcode",outcome="started"} 4
astraeus_stream_sessions_total{mode="transcode",outcome="error"} 2
astraeus_stream_errors_total{mode="transcode"} 2
astraeus_transcode_fallbacks_total 3
astraeus_transcode_startup_seconds_count{mode="transcode"} 4
astraeus_first_segment_seconds_count{mode="transcode"} 2
astraeus_stream_sessions_active 1
astraeus_probe_errors_total 0
astraeus_scan_runs_total 2
astraeus_scan_files_total 812
astraeus_metadata_lookup_seconds_count{provider="tmdb",outcome="ok"} 55
astraeus_auth_denied_total{reason="untrusted_source"} 0
astraeus_auth_granted_total{mode="none"} 98
astraeus_rate_limited_total 0
astraeus_spans_dropped_total 0
""")

probe_lines = [
    '2026-10-09T21:00:02.000000000Z level=INFO msg="probing hardware encoders" families=5',
]
if ALL_REJECTED:
    probe_lines += [
        '2026-10-09T21:00:03.000000000Z level=WARN msg="hardware encoder rejected" encoder=h264_nvenc reason="exit status 1: Cannot load libnvidia-encode.so.1"',
        '2026-10-09T21:00:04.000000000Z level=WARN msg="hardware encoder rejected" encoder=h264_qsv reason="exit status 1: Device creation failed: -2"',
        '2026-10-09T21:00:05.000000000Z level=WARN msg="hardware encoder rejected" encoder=h264_vaapi reason="exit status 1: Failed to initialise VAAPI connection: -1 (unknown libva error)."',
        '2026-10-09T21:00:05.500000000Z level=WARN msg="no hardware encoder is usable on this host; transcoding will use the CPU" rejected=5',
    ]
else:
    probe_lines += [
        '2026-10-09T21:00:03.000000000Z level=WARN msg="hardware encoder rejected" encoder=h264_nvenc reason="Cannot load libnvidia-encode.so.1"',
        '2026-10-09T21:00:04.000000000Z level=WARN msg="hardware encoder rejected" encoder=h264_qsv reason="Device creation failed: -2"',
        '2026-10-09T21:00:05.000000000Z level=INFO msg="hardware encoder verified" encoder=h264_vaapi',
        '2026-10-09T21:00:05.500000000Z level=INFO msg="hardware encoder verified" encoder=hevc_vaapi',
    ]

log = "\n".join([
    '2026-10-09T21:00:01.000000000Z level=INFO msg="astraeus starting" version=dev',
    *probe_lines,
    '2026-10-09T21:00:06.000000000Z level=INFO msg="image_subtitles=\\"read as text\\" subtitle_ocr_enabled=true"',
    '2026-10-09T21:05:00.000000000Z level=INFO msg="http request" method=GET path=/api/health status=200',
    '2026-10-09T21:06:00.000000000Z level=ERROR msg="stream start failed" code=stream_start_failed error="Impossible to convert between the formats supported by the filter \'Parsed_scale2ref_3\'"',
    '2026-10-09T21:07:00.000000000Z level=ERROR msg="transcode fallback" reason="hardware attempt failed, retrying in software"',
    '2026-10-09T21:08:00.000000000Z level=ERROR msg="scan failed" error="creating entity \\"Film 000\\": database is locked (517)"',
    '2026-10-09T21:09:00.000000000Z level=INFO msg="ocr pass" track=2 cues=412 tesseract=tesseract',
    '2026-10-09T21:10:00.000000000Z level=ERROR msg="encoder exited" session=abc mode=transcode',
])
w("logs/container-stdout-stderr.log", log)
w("logs/container-tail.log", log)
w("logs/errors-warnings.log", "\n".join(l for l in log.splitlines() if "ERROR" in l or "WARN" in l))
w("logs/summary.txt", f"total_lines={len(log.splitlines())}\ntail_lines=3000\nerror_lines=5\n")
print(root)
