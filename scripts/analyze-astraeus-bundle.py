#!/usr/bin/env python3
"""Turn an Astraeus diagnostics bundle into a Markdown report.

Reads the tarball (or an unpacked directory) produced by
scripts/collect-astraeus-diagnostics.sh and answers the questions the bundle was
collected for:

  * which hardware encoder family, if any, this host actually proved, and why
    the others were rejected;
  * what the container can see of the GPUs the host has;
  * which review findings the captured evidence bears on, positively or
    negatively.

It never invents a conclusion: every statement in the output is tied to a file
and a line in the bundle, and a missing file is reported as missing rather than
as "nothing found".

Usage:
    analyze-astraeus-bundle.py BUNDLE [--format md|text] [--out REPORT.md]
"""

from __future__ import annotations

import argparse
import json
import os
import re
import sys
import tarfile
import tempfile
from collections import Counter, defaultdict
from datetime import datetime, timezone

# ---------------------------------------------------------------------------
# bundle access
# ---------------------------------------------------------------------------

class Bundle:
    """Read-only view of a bundle, whether it is a tarball or a directory."""

    def __init__(self, root: str):
        self.root = root
        self._listing: list[str] = []
        for dirpath, _dirnames, filenames in os.walk(root):
            for name in filenames:
                full = os.path.join(dirpath, name)
                self._listing.append(os.path.relpath(full, root))
        self._listing.sort()

    def listing(self) -> list[str]:
        return list(self._listing)

    def read(self, rel: str) -> str | None:
        full = os.path.join(self.root, rel)
        if not os.path.isfile(full):
            return None
        try:
            with open(full, "r", encoding="utf-8", errors="replace") as handle:
                return handle.read()
        except OSError:
            return None

    def lines(self, rel: str) -> list[str]:
        body = self.read(rel)
        return body.splitlines() if body is not None else []

    def glob(self, prefix: str) -> list[str]:
        return [p for p in self._listing if p.startswith(prefix)]

    def read_json(self, rel: str):
        """Parse a bundle file as JSON, tolerating the collector's header."""
        body = self.read(rel)
        if body is None:
            return None
        body = strip_header(body)
        try:
            return json.loads(body)
        except (json.JSONDecodeError, ValueError):
            return None


def strip_header(body: str) -> str:
    return "\n".join(line for line in body.splitlines() if not line.startswith("# "))


def open_bundle(path: str) -> tuple[Bundle, tempfile.TemporaryDirectory | None]:
    if os.path.isdir(path):
        return Bundle(path), None
    if not tarfile.is_tarfile(path):
        sys.exit(f"error: {path} is neither a directory nor a tar archive")
    tmp = tempfile.TemporaryDirectory(prefix="astraeus-bundle.")
    with tarfile.open(path) as archive:
        # Refuse members that would escape the extraction root. A member named
        # "." is the archive root itself, which tar stores as "./".
        root_real = os.path.realpath(tmp.name)
        for member in archive.getmembers():
            name = member.name.lstrip("./") or "."
            target = os.path.realpath(os.path.join(root_real, name))
            if target != root_real and not target.startswith(root_real + os.sep):
                sys.exit(f"error: refusing archive member outside the bundle: {member.name}")
        archive.extractall(tmp.name, filter="data")
    return Bundle(tmp.name), tmp


# ---------------------------------------------------------------------------
# small parsers
# ---------------------------------------------------------------------------

def parse_kv(body: str | None) -> dict[str, str]:
    out: dict[str, str] = {}
    if not body:
        return out
    for line in body.splitlines():
        line = line.strip()
        if not line or line.startswith("#") or "=" not in line:
            continue
        key, _, value = line.partition("=")
        out[key.strip()] = value.strip()
    return out


# A `#` line is either a provenance header the collector added or a Prometheus
# HELP/TYPE line, and calling one of those a sample is what made a fully
# declared registry look like four series.
COMMENT_LINE = re.compile(r"^\s*#")


def parse_prometheus(body: str | None) -> dict[str, float]:
    """Flatten a Prometheus exposition into sample-name -> summed value."""
    totals: dict[str, float] = defaultdict(float)
    if not body:
        return {}
    for line in body.splitlines():
        line = line.strip()
        if not line or COMMENT_LINE.match(line):
            continue
        name, _, value = line.rpartition(" ")
        name = name.split("{", 1)[0].strip()
        if not name:
            continue
        try:
            totals[name] += float(value)
        except ValueError:
            continue
    return dict(totals)


def declared_metric_families(body: str | None) -> dict[str, str]:
    """The # TYPE lines: what the server declares at startup, idle or not."""
    families: dict[str, str] = {}
    if not body:
        return families
    for line in body.splitlines():
        match = re.match(r"^#\s*TYPE\s+(\S+)\s+(\S+)", line.strip())
        if match:
            families[match.group(1)] = match.group(2)
    return families


def parse_prometheus_series(body: str | None) -> list[tuple[str, dict[str, str], float]]:
    """Keep the labels, for metrics where the label is the interesting part."""
    series: list[tuple[str, dict[str, str], float]] = []
    if not body:
        return series
    for line in body.splitlines():
        line = line.strip()
        if not line or COMMENT_LINE.match(line):
            continue
        head, _, value = line.rpartition(" ")
        try:
            number = float(value)
        except ValueError:
            continue
        name = head
        labels: dict[str, str] = {}
        match = re.match(r"^([a-zA-Z_:][a-zA-Z0-9_:]*)\{(.*)\}$", head)
        if match:
            name = match.group(1)
            for pair in re.findall(r'([a-zA-Z_][a-zA-Z0-9_]*)="((?:[^"\\]|\\.)*)"', match.group(2)):
                labels[pair[0]] = pair[1]
        series.append((name, labels, number))
    return series


LOG_LEVEL_RE = re.compile(r"level=(DEBUG|INFO|WARN|ERROR)", re.IGNORECASE)
# `docker logs --timestamps` prefixes an RFC3339 stamp, and the server's own
# text handler emits `time=...`; either can lead a line, and most lines have
# both.
TS_RE = re.compile(r"^(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?Z?)\s*")
TIME_FIELD_RE = re.compile(r"^time=(\S+)\s+")


def split_log_line(line: str) -> tuple[str, str]:
    match = TS_RE.match(line)
    if match:
        return match.group(1), line[match.end():]
    match = TIME_FIELD_RE.match(line)
    if match:
        return match.group(1), line[match.end():]
    return "", line


# ---------------------------------------------------------------------------
# analysis
# ---------------------------------------------------------------------------

# The families the startup probe tries, in the order encoders.go prefers them.
FAMILIES = ["nvenc", "qsv", "amf", "videotoolbox", "vaapi"]

HW_REJECT_RE = re.compile(
    r'msg="hardware encoder rejected"\s+encoder=(\S+)\s+reason="([^"]*)"', re.S)
HW_NONE_RE = re.compile(
    r'msg="no hardware encoder is usable on this host[^"]*"\s+rejected=(\d+)')
HW_ACCEPT_RE = re.compile(
    r'msg="hardware encoder (?:accepted|verified|usable)"\s+encoder=(\S+)')
RENDER_NODE_RE = re.compile(r'render_node="?([^"\s]+)"?')

# Signatures of review findings that a running instance can exhibit. A pattern
# that matches a routine INFO line would turn this table into noise, so each is
# written to match the error or the specific field the finding is about.
FINDING_SIGNATURES: list[tuple[str, str, str, re.Pattern[str]]] = [
    ("S-3", "codec copied into a segment container that cannot carry it",
     "segmentContainerCanCarry / bin_data",
     re.compile(r"segmentContainerCanCarry|bin_data|cannot carry these codecs", re.I)),
    ("S-5", "VAAPI subtitle burn-in filter graph failure",
     "Impossible to convert between the formats",
     re.compile(r"Impossible to convert between the formats", re.I)),
    ("S-7", "Opus/Vorbis falling through to ffmpeg's experimental native encoder",
     "experimental encoder", re.compile(r"experimental.*(opus|vorbis)|Encoder .* is experimental", re.I)),
    ("S-9", "a dead ffmpeg strands its session",
     "stream_start_failed / 410 / encoder exited",
     re.compile(r"stream_start_failed|encoder exited|session (has )?failed", re.I)),
    ("S-10", "pausing after the encoder finishes kills playback",
     "playlist stopped growing", re.compile(r"playlist.*(never|not) grow|no new segments", re.I)),
    ("S-11", "a capacity refusal recorded as a hardware failure (or a 429)",
     "too_many_sessions / max sessions",
     re.compile(r"too_many_sessions|max.?sessions|capacity", re.I)),
    ("S-12", "a failed start counted twice",
     "stream error recording", re.compile(r"stream error (recorded|counted)", re.I)),
    ("S-13", "orphan session directories accumulate",
     "orphan / sweep / stale session", re.compile(r"orphan(ed)? session|sweep|stale session", re.I)),
    ("S-15", "forced keyframes are not IDR on NVENC/QSV/AMF",
     "forced-idr", re.compile(r"forced[-_]idr", re.I)),
    ("S-17", "ffmpeg protocols not restricted",
     "protocol_whitelist", re.compile(r"protocol_whitelist|Protocol not on whitelist", re.I)),
    ("A-9", "missing server timeouts",
     "idle/header timeout", re.compile(r"(idle|read|write|header) timeout|deadline exceeded", re.I)),
    ("A-B1", "SIGTERM during an active download exits 1",
     "shutdown", re.compile(r"SIGTERM|shutting down|signal (received|terminated)", re.I)),
    ("A-B3", "a library path is not canonicalised", "library path",
     re.compile(r"library path|canonicalis|canonicaliz", re.I)),
    ("A-B7", "scans or enrich passes are not serialised",
     "job already running", re.compile(r"job .*already (running|in flight)|deduplicat", re.I)),
    ("L-4", "scans fail with `database is locked`",
     "database is locked", re.compile(r"database is locked", re.I)),
    ("L-5", "a symlinked library root scans zero files",
     "symlink / saw 0 files", re.compile(r"symlink|saw 0 files|zero files", re.I)),
    ("L-6", "remakes collapse into one entity",
     "duplicate identity / unique constraint",
     re.compile(r"duplicate identit|UNIQUE constraint failed", re.I)),
    ("L-7", "one unplaceable file disables pruning",
     "refusing to prune", re.compile(r"refus\w* to prune|not pruning|prune (refused|skipped)", re.I)),
    ("L-9", "migration fails on a legacy database",
     "migration error", re.compile(r"migrat\w*.*(fail|error)|error.*migrat", re.I)),
    ("L-10", "the TMDB key leaks into a log line",
     "an unredacted api_key", re.compile(r"api_key=(?!<redacted>)[^&\s\"']+", re.I)),
    ("L-12", "the non-Matroska VobSub path cannot work",
     "vobsub extraction failure",
     re.compile(r"vobsub.*(fail|no decoder|unsupported|not supported)", re.I)),
    ("L-13", "OCR budget and coordination",
     "ocr / tesseract timing", re.compile(r"\bocr\b|tesseract", re.I)),
    ("L-14", "`--ffprobe` is not passed to the subtitle service",
     "subtitle extraction invoking ffprobe",
     re.compile(r"(subtitle|extract).*ffprobe|ffprobe.*(subtitle|extract)", re.I)),
    ("L-15", "overlapping libraries trade objects on every scan",
     "objects moved between libraries",
     re.compile(r"object.*(moved|traded|reassign)|(moved|traded|reassign).*object", re.I)),
    ("L-16", "the Matroska parser panics on tiny input",
     "panic / bounds", re.compile(r"panic:|index out of range|slice bounds out of range", re.I)),
    ("L-18", "metadata worker robustness",
     "provider failure", re.compile(r"(provider|tmdb).*(fail|error|timeout)", re.I)),
    ("W-4", "navigating during negotiation orphans a transcode",
     "client disconnected / context canceled",
     re.compile(r"context canceled|client (disconnected|closed|gone)", re.I)),
]


class Report:
    def __init__(self, bundle: Bundle):
        self.bundle = bundle
        self.out: list[str] = []
        self.conclusions: list[str] = []
        self.caveats: list[str] = []
        self.finding_hits: Counter[str] = Counter()

    def line(self, text: str = "") -> None:
        self.out.append(text)

    def conclude(self, text: str) -> None:
        self.conclusions.append(text)

    def caveat(self, text: str) -> None:
        self.caveats.append(text)

    def render(self) -> str:
        return "\n".join(self.out).rstrip() + "\n"


def analyze(bundle: Bundle) -> Report:
    report = Report(bundle)
    meta = parse_kv(bundle.read("meta/collector.txt"))
    state = parse_kv(bundle.read("container/state.txt"))
    ffmpeg_version_lines = [
        ln for ln in bundle.lines("inside/ffmpeg-version.txt") if ln.startswith("ffmpeg version")]
    ffmpeg_version = ffmpeg_version_lines[0] if ffmpeg_version_lines else "not captured"
    host = parse_kv(bundle.read("host/host.txt"))

    # ---- header ----------------------------------------------------------
    report.line("# Astraeus diagnostics report")
    report.line()
    report.line(f"- Bundle: `{os.path.basename(bundle.root.rstrip('/')) or 'bundle'}`")
    report.line(f"- Collected: {meta.get('collected_at_utc', 'unknown')}")
    report.line(f"- Container: `{meta.get('container_name', '?')}` on `{meta.get('collector_host', '?')}`")
    report.line(f"- Image: `{meta.get('container_image', '?')}`"
                f" (version label: `{state.get('image_version_label', '?')}`)")
    report.line(f"- Server started: {meta.get('container_started_at', state.get('started_at', '?'))}")
    report.line(f"- Kernel: {host.get('kernel', meta.get('kernel', '?'))}")
    report.line(f"- ffmpeg in the image: {ffmpeg_version}")
    report.line()

    log_path = "logs/container-stdout-stderr.log"
    log_lines = bundle.lines(log_path)
    if not log_lines:
        report.caveat("the container log is empty or missing; most of the analysis below needs it")
        report.line("> **No container log was captured.** Check `logs/` in the bundle; the "
                    "container may use a logging driver that `docker logs` cannot read.")
        report.line()

    # ---- hardware verdict ------------------------------------------------
    hw = hardware_verdict(bundle, report, log_lines)

    # ---- GPU visibility --------------------------------------------------
    gpu_visibility(bundle, report, log_lines, hw)

    # ---- metrics ---------------------------------------------------------
    metrics_body = bundle.read("metrics/prometheus.txt")
    metrics_table(bundle, report)

    # ---- findings --------------------------------------------------------
    findings_section(bundle, report, log_lines, metrics_body)

    # ---- summary ---------------------------------------------------------
    report.line()
    report.line("## Verdict")
    report.line()
    for conclusion in report.conclusions:
        report.line(f"- {conclusion}")
    if report.caveats:
        report.line()
        report.line("### What this bundle cannot answer")
        report.line()
        for caveat in report.caveats:
            report.line(f"- {caveat}")
    return report


def hardware_verdict(bundle: Bundle, report: Report, log_lines: list[str]) -> dict:
    """Decide which encoder family the startup probe actually proved."""
    report.line("## Hardware encoder probe")
    report.line()

    rejects: list[tuple[str, str]] = []
    accepted: list[str] = []
    rejected_count = None
    no_hardware = False
    render_node = ""

    joined = "\n".join(log_lines)
    for match in HW_REJECT_RE.finditer(joined):
        encoder, reason = match.group(1), re.sub(r"\s+", " ", match.group(2)).strip()
        if (encoder, reason) not in rejects:
            rejects.append((encoder, reason))
    for match in HW_ACCEPT_RE.finditer(joined):
        if match.group(1) not in accepted:
            accepted.append(match.group(1))
    match = HW_NONE_RE.search(joined)
    if match:
        no_hardware = True
        rejected_count = match.group(1)
    match = RENDER_NODE_RE.search(joined)
    if match:
        render_node = match.group(1)

    # The capabilities endpoint is the authoritative snapshot when it answered.
    capabilities = bundle.read_json("api/capabilities.json")
    encoders: list[str] = []
    hardware_accel: list[str] = []
    api_rejects: list[dict] = []
    if isinstance(capabilities, dict):
        encoders = [str(e) for e in capabilities.get("video_encoders", []) or []]
        hardware_accel = [str(h) for h in capabilities.get("hardware_acceleration", []) or []]
        api_rejects = capabilities.get("rejected_encoders", []) or []
        render_node = capabilities.get("render_node", render_node) or render_node

    if isinstance(capabilities, dict):
        report.line("`GET /api/system/capabilities` reported:")
        report.line()
        report.line("| field | value |")
        report.line("| --- | --- |")
        for key in ("ffmpeg", "ffprobe", "image_subtitles", "subtitle_ocr_enabled",
                    "render_node", "hardware_acceleration"):
            if key in capabilities:
                report.line(f"| `{key}` | `{capabilities[key]}` |")
        report.line(f"| `video_encoders` | {', '.join(f'`{e}`' for e in encoders) or '(none)'} |")
        report.line()
    else:
        report.caveat("`/api/system/capabilities` did not answer, so the encoder set is "
                      "inferred from the startup log alone")

    if accepted:
        report.line(f"**Accepted by the probe:** {', '.join(f'`{a}`' for a in accepted)}")
        report.conclude(
            "the probe accepted " + ", ".join(f"`{a}`" for a in accepted)
            + " — this is the first real-hardware evidence the project has for that family")
    elif encoders and any(e.split("_")[-1] in FAMILIES for e in encoders):
        named = [e for e in encoders if e.split("_")[-1] in FAMILIES]
        report.line(f"**Hardware encoders offered:** {', '.join(f'`{e}`' for e in named)}")
        report.conclude("the probe accepted " + ", ".join(f"`{e}`" for e in named))
    else:
        report.line("**No hardware encoder was accepted by the probe.**")
        if no_hardware:
            report.conclude(
                f"no hardware encoder proved usable; the server is transcoding on the CPU "
                f"(the log names {rejected_count} rejected families)")
        report.caveat("hardware was offered every chance to fail here: a family that is "
                      "rejected is rejected for a stated reason, so the reasons below are "
                      "the evidence")

    if render_node:
        report.line(f"**Render node:** `{render_node}`")
    elif not accepted:
        report.line("**Render node:** none recorded")

    if rejects:
        report.line()
        report.line("Rejections, with ffmpeg's own complaint:")
        report.line()
        report.line("| encoder | reason |")
        report.line("| --- | --- |")
        for encoder, reason in rejects:
            report.line(f"| `{encoder}` | {reason} |")
    if api_rejects:
        report.line()
        report.line("`rejected_encoders` from the API:")
        report.line()
        report.line("| encoder | reason |")
        report.line("| --- | --- |")
        for entry in api_rejects:
            if isinstance(entry, dict):
                report.line(f"| `{entry.get('encoder', '?')}` | {entry.get('reason', '')} |")
    report.line()
    return {"accepted": accepted, "rejects": rejects, "render_node": render_node,
            "encoders": encoders}


def gpu_visibility(bundle: Bundle, report: Report, log_lines: list[str], hw: dict) -> None:
    report.line("## GPU visibility: host vs container")
    report.line()
    vis = bundle.read("host/gpu-visibility.txt")
    if vis:
        # Some of the values here are whole JSON documents (the runtimes map),
        # so show the shape of each line rather than thousands of characters.
        report.line("```")
        for line in vis.strip().splitlines():
            trimmed = line if len(line) <= 200 else line[:200] + " …[truncated]"
            report.line(trimmed)
        report.line("```")
        if any(len(line) > 200 for line in vis.splitlines()):
            report.line()
            report.line("Long values above are truncated here; the untruncated file is "
                        "`host/gpu-visibility.txt` in the bundle.")
        report.line()
    else:
        report.caveat("`host/gpu-visibility.txt` is missing; the collector did not reach the "
                      "host-facts stage")

    host_values = {}
    for line in (vis or "").splitlines():
        if ":" in line:
            key, _, value = line.partition(":")
            host_values[key.strip()] = value.strip()

    host_nodes = host_values.get("host render nodes", "")
    host_nvidia = host_values.get("host nvidia devices", "")
    host_smi = host_values.get("host nvidia-smi", "")
    runtime = host_values.get("container runtime", "")
    devices_raw = host_values.get("container devices (HostConfig.Devices)", "null")
    requests_raw = host_values.get("container device requests (HostConfig.DeviceRequests)", "null")

    def has_json_value(raw: str) -> bool:
        """True when an inspect field holds something other than empty/null."""
        return bool(re.search(r"[{\[]", raw or "")) or (raw or "").strip() not in ("", "null", "[]")

    # What the container can actually see, read from inside it. This is the
    # fact that matters; the inspect fields only explain why.
    dri_body = bundle.read("inside/ffmpeg-devices.txt") or ""

    def section(name: str, body: str) -> str:
        """The lines between `== name ==` and the next `== ... ==` header."""
        match = re.search(rf"^== {re.escape(name)} ==$(.*?)(?=^== |\Z)",
                          body, re.S | re.M)
        return match.group(1) if match else ""

    def section_has_node(section_name: str, device: str) -> bool:
        text = section(section_name, dri_body)
        if not text or "No such file" in text or "cannot access" in text:
            return False
        return re.search(rf"/dev/{device}\b", text) is not None

    dri_in_container = section_has_node("/dev/dri", r"dri/[a-z]")
    nvidia_in_container = section_has_node("/dev/nvidia*", r"nvidia\d")

    # `--gpus all` injects nodes through DeviceRequests and does *not* populate
    # HostConfig.Devices, so testing Devices alone reports a false negative.
    # A GPU that the probe actually accepted outranks every inspect field.
    gpu_requested = (has_json_value(devices_raw) or has_json_value(requests_raw)
                     or "nvidia" in runtime.lower()
                     or any("nvenc" in e for e in hw.get("accepted", []))
                     or any(e.lower().endswith("_nvenc") for e in hw.get("encoders", [])))
    dri_requested = (has_json_value(devices_raw) or has_json_value(requests_raw)
                     or "vaapi" in hw.get("accepted", [])
                     or any(e.lower().endswith("_vaapi") for e in hw.get("encoders", [])))

    if host_nodes and not dri_in_container:
        report.line("> **The host has a render node that the container cannot see.** "
                    "VAAPI needs `--device /dev/dri`; without it `h264_vaapi` is rejected "
                    "whatever the host driver offers. This is the first thing to fix.")
        report.conclude("the container was not given `/dev/dri`, so VAAPI is unavailable "
                        "even though the host exposes a render node")
    elif host_nodes and dri_in_container and not dri_requested:
        report.line("- The container can see `/dev/dri`, and VAAPI was neither accepted nor "
                    "offered — check the rejections above for the driver-level reason "
                    "(an Intel host needs `intel-media-va-driver` / `mesa-va-drivers` inside "
                    "the container, which the image does not ship).")

    if host_nvidia:
        if not nvidia_in_container and not gpu_requested:
            report.line("> **The host has an NVIDIA GPU and the container was not given it.** "
                        "The image is Debian with no CUDA userspace, so NVENC needs both "
                        "`--gpus all` (or `--runtime=nvidia`) and the driver libraries the "
                        "container toolkit injects. Without them `h264_nvenc` cannot load "
                        "`libnvidia-encode.so`.")
            report.conclude("the container was not given the NVIDIA GPU (`--gpus all` / the "
                            "NVIDIA runtime), so NVENC is unavailable")
        elif not nvidia_in_container and gpu_requested:
            report.line("> The container was configured for a GPU but no `/dev/nvidia*` node "
                        "appears inside it. Re-run `nvidia-smi` inside the container; if that "
                        "fails, the toolkit or the project's container config is the suspect.")
            report.conclude("a GPU was requested but the device nodes are absent inside the "
                            "container; the NVIDIA container toolkit is the suspect")

    if not host_nodes and not host_nvidia:
        report.line("- The host itself reports no render node and no NVIDIA device, so no "
                    "hardware encoder could have worked here. CPU transcoding is the correct "
                    "outcome on this machine, not a bug.")
        report.conclude("the host has no GPU devices at all, so CPU transcoding is correct here")
    if not host_smi:
        report.line("- `nvidia-smi` is not on the host, so there is no NVIDIA driver "
                    "userspace for any container to be given.")
    if (host_nodes or host_nvidia) and not hw.get("accepted"):
        report.caveat("the host has a GPU but no hardware encoder was accepted, so the "
                      "rejections above are the evidence to read: each one is ffmpeg's own "
                      "complaint about that specific device — which is exactly the "
                      "never-before-measured claim in the review")
    report.line()


def metrics_table(bundle: Bundle, report: Report) -> None:
    report.line("## Metrics")
    report.line()
    body = bundle.read("metrics/prometheus.txt")
    if body is None:
        report.caveat("`/metrics` was not captured; the KPI table and the counter evidence "
                      "are unavailable")
        report.line("`/metrics` was not captured.")
        report.line()
        return
    totals = parse_prometheus(body)
    series = parse_prometheus_series(body)
    declared = declared_metric_families(body)
    astraeus_declared = {name: kind for name, kind in declared.items()
                         if name.startswith("astraeus_")}
    astraeus_samples = {name for name in totals if name.startswith("astraeus_")}

    report.line(f"{len(astraeus_declared)} `astraeus_*` metric families are declared at "
                f"startup, and {len(astraeus_samples)} carry a sample on this instance.")
    report.line()

    interesting = [
        ("http_requests_total", "requests served"),
        ("playback_decisions_total", "playback negotiations, by chosen mode"),
        ("stream_sessions_total", "stream sessions, by mode and outcome"),
        ("stream_errors_total", "stream failures"),
        ("transcode_fallbacks_total", "hardware transcodes retried in software"),
        ("transcode_startup_seconds_count", "transcodes that reached a playlist"),
        ("first_segment_seconds_count", "streams that delivered a first segment"),
        ("stream_sessions_active", "sessions active now"),
        ("probe_errors_total", "probe failures"),
        ("scan_runs_total", "scans run"),
        ("scan_files_total", "files seen by scans"),
        ("metadata_lookup_seconds_count", "metadata lookups"),
        ("auth_denied_total", "gate denials"),
        ("auth_granted_total", "gate grants"),
        ("rate_limited_total", "requests refused by the rate limiter"),
        ("spans_dropped_total", "trace spans dropped"),
    ]
    report.line("| metric | value | declared | what it means here |")
    report.line("| --- | --- | --- | --- |")
    seen = set()
    for name, meaning in interesting:
        full = "astraeus_" + name
        if full in seen:
            continue
        seen.add(full)
        family = full.rsplit("_count", 1)[0].rsplit("_sum", 1)[0].rsplit("_bucket", 1)[0]
        is_declared = family in astraeus_declared or full in astraeus_declared
        value = totals.get(full)
        rendered = fmt_number(value) if value is not None else "—"
        report.line(f"| `{full}` | {rendered} | {'yes' if is_declared else 'no'} | {meaning} |")

    # A declared family with no sample is the absence-based alerting contract,
    # so say plainly whether it holds on this instance.
    missing = sorted(set(astraeus_declared) - astraeus_samples)
    if missing:
        report.line()
        report.line(f"Declared but with no sample yet ({len(missing)}): "
                    + ", ".join(f"`{m}`" for m in missing[:12])
                    + (" …" if len(missing) > 12 else ""))
    undeclared = sorted(astraeus_samples - set(astraeus_declared))
    undeclared = [name for name in undeclared
                  if re.sub(r"_(bucket|count|sum)$", "", name) not in astraeus_declared]
    if undeclared:
        report.line()
        report.line("Samples with no `# TYPE` line: "
                    + ", ".join(f"`{name}`" for name in undeclared))

    report.line()
    # Labels carry the interesting detail for the decision and session counters.
    for metric, dimension in (("astraeus_playback_decisions_total", "mode"),
                              ("astraeus_stream_sessions_total", "outcome"),
                              ("astraeus_stream_errors_total", "mode"),
                              ("astraeus_metadata_lookup_seconds_count", "provider")):
        rows = [(labels, value) for name, labels, value in series if name == metric]
        if not rows:
            continue
        rendered = ", ".join(
            f"`{labels.get(dimension, '?')}`={fmt_number(value)}"
            for labels, value in sorted(rows, key=lambda row: row[0].get(dimension, "")))
        report.line(f"- `{metric}`: {rendered}")

    fallbacks = totals.get("astraeus_transcode_fallbacks_total", 0)
    sessions = totals.get("astraeus_stream_sessions_total", 0)
    if fallbacks and sessions:
        report.line()
        report.line(f"> `transcode_fallbacks_total` is **{fmt_number(fallbacks)}** against "
                    f"{fmt_number(sessions)} sessions started. A fallback means a hardware "
                    "attempt failed and was retried in software — the direct evidence that "
                    "the hardware path is not working.")

    active = totals.get("astraeus_stream_sessions_active", 0)
    if active:
        report.line()
        report.line(f"> **{fmt_number(active)} stream session(s) were active when this was "
                    "scraped.** The URL in `state/` and the IDs under `/api/streams` are what "
                    "to poke while it is still running.")
    report.line()


def findings_section(bundle: Bundle, report: Report, log_lines: list[str],
                     metrics_body: str | None) -> None:
    report.line("## Review findings this bundle bears on")
    report.line()
    tail = log_lines[-20000:] if len(log_lines) > 20000 else log_lines
    hits: list[tuple[str, str, str, list[str]]] = []
    for finding, title, needle, pattern in FINDING_SIGNATURES:
        matched = [ln for ln in tail if pattern.search(ln)]
        if matched:
            hits.append((finding, title, needle, matched[:3]))

    # A-4 is a metrics finding, not a log one: the HTTP method is taken straight
    # from the request, so any method a client invents becomes its own series.
    methods = {labels.get("method", "") for name, labels, _ in parse_prometheus_series(metrics_body)
               if name == "astraeus_http_requests_total"}
    if methods:
        unusual = sorted(m for m in methods if m not in {"GET", "POST", "PUT", "DELETE", "HEAD", "OPTIONS", "PATCH"})
        report.line(f"HTTP methods present in `astraeus_http_requests_total`: "
                    f"{', '.join(f'`{m}`' for m in sorted(methods))}.")
        if unusual:
            report.line()
            report.line(f"> **A-4 is live.** The label set contains "
                        f"{', '.join(f'`{m}`' for m in unusual)}, which no browser sends. Each "
                        "one is a permanent series in the registry — a client can grow the "
                        "label set without bound.")
            report.conclude("A-4 is reproducible on this instance: unusual HTTP methods are "
                            "already in the `astraeus_http_requests_total` label set")
        report.line()

    if not hits:
        report.line("No line in the captured log matches any of the finding signatures. That "
                    "is a real result for the findings whose symptom is a log line, and says "
                    "nothing about the ones that are not.")
        report.line()
        return

    report.line("| finding | what it is | matched |")
    report.line("| --- | --- | --- |")
    for finding, title, needle, examples in hits:
        report.line(f"| **{finding}** | {title} | {len(examples)}+ lines containing `{needle}` |")
    report.line()
    for finding, title, needle, examples in hits:
        report.line(f"### {finding} — {title}")
        report.line()
        for example in examples:
            stamp, rest = split_log_line(example)
            prefix = f"{stamp} " if stamp else ""
            report.line(f"```\n{prefix}{rest.strip()[:400]}\n```")
        report.line()


def fmt_number(value: float) -> str:
    if value == int(value):
        return f"{int(value):,}"
    return f"{value:,.3f}"


# ---------------------------------------------------------------------------
# entry point
# ---------------------------------------------------------------------------

def main(argv: list[str]) -> int:
    parser = argparse.ArgumentParser(
        description="Turn an Astraeus diagnostics bundle into a Markdown report.")
    parser.add_argument("bundle", help="tarball or unpacked directory")
    parser.add_argument("--out", help="write the report here instead of stdout")
    args = parser.parse_args(argv)

    if not os.path.exists(args.bundle):
        sys.exit(f"error: {args.bundle} does not exist")

    bundle, tmp = open_bundle(args.bundle)
    try:
        report = analyze(bundle)
        text = report.render()
    finally:
        if tmp is not None:
            tmp.cleanup()

    if args.out:
        with open(args.out, "w", encoding="utf-8") as handle:
            handle.write(text)
        print(f"wrote {args.out}")
    else:
        sys.stdout.write(text)
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
