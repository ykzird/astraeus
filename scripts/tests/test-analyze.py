#!/usr/bin/env python3
"""Regression tests for analyze-astraeus-bundle.py.

Run with `python3 scripts/tests/test-analyze.py`, or with pytest if it is
installed. The point of these is the classification logic: a report that says
"the device is missing" when the device is present, or "accepted" when the probe
rejected everything, is worse than no report, and both of those happened while
this was being written.

The fixtures here are built on the fly so they cannot drift from what the
collector actually writes.
"""
from __future__ import annotations

import importlib.util
import json
import os
import sys
import tempfile

HERE = os.path.dirname(os.path.abspath(__file__))
ANALYZER = os.path.join(os.path.dirname(HERE), "analyze-astraeus-bundle.py")


def load_analyzer():
    spec = importlib.util.spec_from_file_location("astraeus_analyzer", ANALYZER)
    module = importlib.util.module_from_spec(spec)
    assert spec.loader is not None
    spec.loader.exec_module(module)
    return module


A = load_analyzer()


# ---------------------------------------------------------------------------
# classify_hw_failure: the message decides, not the vendor
# ---------------------------------------------------------------------------

def test_missing_nvidia_library():
    cause, advice = A.classify_hw_failure("h264_nvenc", "exit status 1: Cannot load libnvidia-encode.so.1")
    assert cause == "missing userspace driver", cause
    assert "libnvidia-encode" in advice


def test_vaapi_without_backend():
    cause, advice = A.classify_hw_failure(
        "h264_vaapi", "exit status 1: Failed to initialise VAAPI connection: -1 (unknown libva error).")
    assert cause == "missing userspace driver", cause
    assert "libva" in advice
    assert "device" in advice  # it must say the device is not the problem


def test_device_not_passed_in():
    cause, _ = A.classify_hw_failure("h264_vaapi", "No such file or directory: /dev/dri/renderD128")
    assert cause == "device not passed in", cause


def test_bitrate_ceiling_rejection():
    cause, _ = A.classify_hw_failure("h264_vaapi", "encoder produced 5256 kbps against the 308 kbps ceiling")
    assert cause == "ceiling not enforced", cause


def test_truncated_reason_is_called_out():
    """The NVENC case on this host: the banner is all `firstComplaint` kept."""
    cause, advice = A.classify_hw_failure(
        "h264_nvenc", "exit status 1: Input #0, lavfi, from 'testsrc=size=320x240:rate=10:duration=1':")
    assert cause == "reason not captured", cause
    assert "ffmpeg" in advice


def test_a_real_vaapi_error_is_not_called_truncated():
    cause, _ = A.classify_hw_failure(
        "h264_vaapi", "exit status 1: Failed to initialise VAAPI connection: -1 (unknown libva error).")
    assert cause != "reason not captured", cause


# ---------------------------------------------------------------------------
# bundle parsing: the API is authoritative over log message-matching
# ---------------------------------------------------------------------------

def build_bundle(tmp: str, capabilities: dict | None, log: str,
                 devices: str, vaapi_drivers: str = "", nvidia_libs: str = "",
                 gpu_visibility: str = "") -> str:
    root = os.path.join(tmp, "bundle")

    def write(rel: str, body: str) -> None:
        path = os.path.join(root, rel)
        os.makedirs(os.path.dirname(path), exist_ok=True)
        with open(path, "w", encoding="utf-8") as handle:
            handle.write(body)

    write("meta/collector.txt", "collected_at_utc=20261009T000000Z\ncontainer_name=c\n")
    write("container/state.txt", "state=running\n")
    write("host/host.txt", "kernel=Linux test\n")
    write("host/gpu-visibility.txt", gpu_visibility)
    write("logs/container-stdout-stderr.log", log)
    write("inside/ffmpeg-devices.txt", devices)
    if vaapi_drivers:
        write("inside/vaapi-drivers.txt", vaapi_drivers)
    if nvidia_libs:
        write("inside/nvidia-libs.txt", nvidia_libs)
    if capabilities is not None:
        write("api/capabilities.json", json.dumps(capabilities))
    return root


DEVICES_WITH_DRI_AND_NVIDIA = """# command: docker exec c sh -c ...
# exit: 0
== /dev/dri ==
total 0
crw-rw---- 1 root 983 226, 0 card0
crw-rw-rw- 1 root 987 226, 128 renderD128
== /dev/dri/by-path ==
/usr/bin/ls: cannot access '/dev/dri/by-path': No such file or directory
== /dev/nvidia* ==
crw-rw-rw- 1 root root 195, 0 /dev/nvidia0
== render nodes ==
/dev/dri/renderD128
== nvidia nodes ==
/dev/nvidia0
"""

DEVICES_WITHOUT_DRI = """# command: docker exec c sh -c ...
# exit: 0
== /dev/dri ==
/usr/bin/ls: cannot access '/dev/dri': No such file or directory
== /dev/dri/by-path ==
/usr/bin/ls: cannot access '/dev/dri/by-path': No such file or directory
== /dev/nvidia* ==
/usr/bin/ls: cannot access '/dev/nvidia*': No such file or directory
== render nodes ==
/usr/bin/ls: cannot access '/dev/dri/renderD*': No such file or directory
== nvidia nodes ==
/usr/bin/ls: cannot access '/dev/nvidia[0-9]*': No such file or directory
"""

GPU_VISIBILITY = """host render nodes: card0 renderD128 
host nvidia devices: /dev/nvidia0 
host nvidia-smi: NVIDIA GeForce RTX 4070, 550.1
host docker runtimes: {"runc":{"path":"runc"}}
container devices (HostConfig.Devices): [{"PathOnHost":"/dev/dri"}]
container device requests (HostConfig.DeviceRequests): [{"Count":-1}]
container runtime: runc
"""

LOG_CLAIMS_ACCEPTED = (
    'time=2026-10-09T00:00:01Z level=INFO msg="hardware encoder verified" encoder=h264_vaapi\n'
    'time=2026-10-09T00:00:02Z level=WARN msg="hardware encoder rejected" encoder=h264_nvenc '
    'reason="exit status 1: Cannot load libnvidia-encode.so.1"\n')


def analyze_to_text(root: str) -> str:
    bundle = A.Bundle(root)
    return A.analyze(bundle).render()


def test_api_empty_overrides_log_acceptance():
    """The false positive this was written to kill: API says none, log mentions one.

    The log must not be allowed to claim an acceptance the API contradicts, and
    the disagreement itself has to be visible rather than silently resolved.
    """
    with tempfile.TemporaryDirectory() as tmp:
        root = build_bundle(
            tmp,
            {"hardware_acceleration": [], "video_encoders": ["h264_vaapi", "libx264"],
             "rejected_encoders": [{"encoder": "h264_nvenc",
                                    "reason": "Cannot load libnvidia-encode.so.1"}]},
            LOG_CLAIMS_ACCEPTED, DEVICES_WITH_DRI_AND_NVIDIA,
            gpu_visibility=GPU_VISIBILITY)
        text = analyze_to_text(root)
        assert "**Hardware acceleration accepted by the probe:**" not in text, text
        assert "The startup log mentions an accepted encoder" in text, text
        assert "treat this as not accepted" in text, text
        assert "disagree" in text, "the log/API disagreement must be reported"


def test_api_accepted_is_reported():
    with tempfile.TemporaryDirectory() as tmp:
        root = build_bundle(
            tmp,
            {"hardware_acceleration": ["vaapi"], "video_encoders": ["h264_vaapi", "libx264"],
             "rejected_encoders": [{"encoder": "h264_nvenc",
                                    "reason": "Cannot load libnvidia-encode.so.1"}]},
            'time=2026-10-09T00:00:01Z level=INFO msg="server capability" hardware_acceleration=[vaapi]\n',
            DEVICES_WITH_DRI_AND_NVIDIA, gpu_visibility=GPU_VISIBILITY)
        text = analyze_to_text(root)
        assert "Hardware acceleration accepted by the probe:** `vaapi`" in text, text


def test_device_absent_is_named_as_the_cause():
    with tempfile.TemporaryDirectory() as tmp:
        root = build_bundle(
            tmp,
            {"hardware_acceleration": [], "video_encoders": ["libx264"],
             "rejected_encoders": [{"encoder": "h264_vaapi",
                                    "reason": "No such file or directory: /dev/dri/renderD128"}]},
            'time=2026-10-09T00:00:01Z level=WARN msg="hardware encoder rejected" '
            'encoder=h264_vaapi reason="No such file or directory: /dev/dri/renderD128"\n',
            DEVICES_WITHOUT_DRI, gpu_visibility=GPU_VISIBILITY)
        text = analyze_to_text(root)
        assert "the container cannot see" in text, text
        assert "device not passed in" in text, text


def test_device_present_but_driver_missing_is_not_blamed_on_the_device():
    """The over-claim: mounting /dev/dri made the old report say the device was absent."""
    with tempfile.TemporaryDirectory() as tmp:
        root = build_bundle(
            tmp,
            {"hardware_acceleration": [], "render_node": "/dev/dri/renderD128",
             "video_encoders": ["libx264"],
             "rejected_encoders": [{"encoder": "h264_vaapi",
                                    "reason": "Failed to initialise VAAPI connection: -1 (unknown libva error)."}]},
            'time=2026-10-09T00:00:01Z level=WARN msg="hardware encoder rejected" '
            'encoder=h264_vaapi reason="exit status 1: Failed to initialise VAAPI connection: '
            '-1 (unknown libva error)."\n',
            DEVICES_WITH_DRI_AND_NVIDIA,
            vaapi_drivers="== libva ==\nlibva.so.2\n== vaapi backends ==\n"
                          "/usr/bin/ls: cannot access '/*_drv_video.so': No such file or directory\n",
            gpu_visibility=GPU_VISIBILITY)
        text = analyze_to_text(root)
        assert "cannot see" not in text, text
        assert "mounted and VAAPI still fails" in text, text
        assert "no vendor backend" in text, text


def test_driver_inventory_reads_what_is_installed():
    with tempfile.TemporaryDirectory() as tmp:
        root = build_bundle(
            tmp,
            {"hardware_acceleration": [], "video_encoders": ["libx264"], "rejected_encoders": []},
            "time=2026-10-09T00:00:01Z level=INFO msg=x\n",
            DEVICES_WITH_DRI_AND_NVIDIA,
            vaapi_drivers="== libva ==\n/usr/lib/libva.so.2\n== vaapi backends ==\n"
                          "/usr/lib/dri/iHD_drv_video.so\n",
            nvidia_libs="== libnvidia-encode/libcuda ==\n/usr/lib/libnvidia-encode.so.1\n"
                        "/usr/lib/libcuda.so.1\n",
            gpu_visibility=GPU_VISIBILITY)
        text = analyze_to_text(root)
        assert "iHD_drv_video.so" in text, text
        assert "libnvidia-encode.so" in text, text


def test_libva_drm_does_not_count_as_libva():
    """`libva-drm.so` is not the library that provides the encode API."""
    with tempfile.TemporaryDirectory() as tmp:
        root = build_bundle(
            tmp,
            {"hardware_acceleration": [], "video_encoders": ["libx264"], "rejected_encoders": []},
            "time=2026-10-09T00:00:01Z level=INFO msg=x\n",
            DEVICES_WITH_DRI_AND_NVIDIA,
            vaapi_drivers="== libva ==\n/usr/lib/libva-drm.so.2.1700.0\n",
            gpu_visibility=GPU_VISIBILITY)
        text = analyze_to_text(root)
        assert "VAAPI backend**: none found" in text, text


def test_prometheus_help_lines_are_not_samples():
    """Counting # HELP lines as samples made a declared registry look empty."""
    body = ("# HELP astraeus_stream_sessions_total Sessions.\n"
            "# TYPE astraeus_stream_sessions_total counter\n"
            "astraeus_stream_sessions_total{mode=\"transcode\",outcome=\"started\"} 2\n")
    totals = A.parse_prometheus(body)
    assert totals.get("astraeus_stream_sessions_total") == 2, totals
    declared = A.declared_metric_families(body)
    assert declared.get("astraeus_stream_sessions_total") == "counter", declared


def main() -> int:
    tests = [v for k, v in sorted(globals().items()) if k.startswith("test_") and callable(v)]
    failures = 0
    for test in tests:
        try:
            test()
            print(f"ok   {test.__name__}")
        except AssertionError as exc:
            failures += 1
            print(f"FAIL {test.__name__}: {exc}")
        except Exception as exc:  # noqa: BLE001 - report and continue
            failures += 1
            print(f"ERROR {test.__name__}: {type(exc).__name__}: {exc}")
    print(f"\n{len(tests) - failures}/{len(tests)} passed")
    return 1 if failures else 0


if __name__ == "__main__":
    raise SystemExit(main())
