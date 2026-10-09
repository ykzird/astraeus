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
# The server's own wording for an accepted encoder. This is a fallback only:
# `/api/system/capabilities` is authoritative when it answered.
HW_ACCEPT_RE = re.compile(
    r'msg="hardware encoder (?:verified|accepted|usable)"\s+encoder=(\S+)')
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

    # ---- ffmpeg command line --------------------------------------------
    ffmpeg_argv_section(bundle, report)

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


def classify_hw_failure(encoder: str, reason: str) -> tuple[str, str]:
    """Map ffmpeg's complaint to a cause and a vendor-agnostic next step.

    The point is to separate the three failures that look alike from outside but
    have different fixes: the device was never passed in, the device is there but
    the userspace driver is not, or the driver is there but something else about
    the encode failed. None of these branches is written against a particular
    vendor; they are written against the message, so an Intel, AMD or NVIDIA
    host all land on the right one.
    """
    lowered = reason.lower()
    family = "nvidia" if "nvenc" in encoder.lower() else (
        "vaapi" if "vaapi" in encoder.lower() else (
            "qsv" if "qsv" in encoder.lower() else "other"))

    # `firstComplaint` returns ffmpeg's first non-blank stderr line, and for a
    # failed encode that is often the input banner rather than the error. The
    # rejection then reads as a failure with no stated cause, which is a
    # weakness in the diagnostic rather than in the encoder.
    if re.search(r"(^|[: ]\s*)(Input #\d|Guessed Channel|Stream mapping)", reason):
        return ("reason not captured",
                "the recorded reason is ffmpeg's input banner, not its error, so this "
                "rejection does not say why the encoder failed. The real message is in the "
                "same ffmpeg run's stderr, which the probe does not keep. Work around it by "
                "running the same command by hand inside the container with "
                "`ffmpeg -loglevel error ...`.")

    if "cannot load" in lowered or "libnvidia" in lowered:
        return ("missing userspace driver",
                "the NVIDIA encode library (`libnvidia-encode.so`, and `libcuda.so` behind it) "
                "cannot be loaded. This is separate from the device: `/dev/nvidia*` can be "
                "present and the library still absent. The base image is Debian with no CUDA "
                "userspace, so those libraries have to be injected by the NVIDIA container "
                "toolkit — the container needs `--gpus all` (or `--runtime=nvidia`), and the "
                "toolkit must be installed on the host. Confirm with `nvidia-smi` *inside* "
                "the container; if that works, the device plumbing is right and what is "
                "missing is specifically the encode library.")
    if "unknown libva error" in lowered or "failed to initialise vaapi" in lowered:
        return ("missing userspace driver",
                "the device is reachable but libva has no vendor backend for it. This is a "
                "driver-package gap, not a missing device: the runtime image ships ffmpeg's "
                "VAAPI support but deliberately no `*_drv_video.so`. Install the driver for "
                "the GPU that owns the render node — `intel-media-va-driver-non-free` or "
                "`i965-va-driver` for Intel, `mesa-va-drivers` for AMD or older Intel — or "
                "rebuild the image with it. Mounting `/dev/dri` alone will never be enough.")
    if "no such file" in lowered or "cannot open" in lowered or "permission denied" in lowered:
        return ("device not passed in",
                "the device node is absent or unreadable inside the container. Pass it in: "
                "`--device /dev/dri` for VAAPI/QuickSync, or `--gpus all` for NVENC.")
    if "device creation failed" in lowered or "no device" in lowered:
        return ("device not passed in",
                "ffmpeg could not create the hardware device. Check that the right device is "
                "passed in for this family and that its userspace driver is installed.")
    if "maxrate" in lowered or "bitrate" in lowered or "cbr" in lowered \
            or "ceiling" in lowered or "kbps" in lowered:
        return ("ceiling not enforced",
                "the family ignored the bitrate ceiling, which is why the startup probe "
                "rejects it: an unenforced limit is worse than a slower encoder (S-1).")
    if family == "qsv" and "vaapi" in lowered:
        return ("backend unavailable", "QuickSync is driven through VAAPI on Linux.")
    return ("encode failed", "ffmpeg failed for a reason above; the message is the evidence.")


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

    # The probe's verdict. `/api/system/capabilities` is authoritative for what
    # the server will negotiate, but an empty `hardware_acceleration` is not
    # evidence that nothing was accepted — it is silence. Only a non-empty list
    # overrides the log; otherwise keep any acceptance the log recorded and say
    # that the two sources are not saying the same thing.
    api_answered = isinstance(capabilities, dict)
    api_says_accepted = list(hardware_accel) if api_answered else []
    if api_says_accepted:
        accepted = api_says_accepted
        log_only = []
    else:
        log_only = list(accepted)
        accepted = []
        if not api_answered:
            report.caveat("`/api/system/capabilities` did not answer, so the hardware verdict "
                          "comes from the startup log alone and is weaker evidence")

    if accepted:
        named = ", ".join(f"`{a}`" for a in accepted)
        report.line(f"**Hardware acceleration accepted by the probe:** {named}")
        report.conclude(f"the probe accepted {named} — this is real-hardware evidence, which "
                        "the project has not had for any family before")
    elif log_only:
        # The log claimed an acceptance the API did not confirm. Report the
        # disagreement rather than picking a winner silently: the API is what
        # negotiation actually consults, but a log line saying otherwise is a
        # real signal that something is inconsistent.
        named = ", ".join(f"`{a}`" for a in log_only)
        report.line(f"**The startup log mentions an accepted encoder ({named}) but the API's "
                    "`hardware_acceleration` is empty.** The API is authoritative for what the "
                    "server will negotiate, so treat this as not accepted — and keep the log "
                    "line, because the two disagreeing is itself worth knowing.")
        report.caveat("the startup log and `/api/system/capabilities` disagree about which "
                      "encoder was accepted")
        if no_hardware:
            report.conclude(
                f"no hardware encoder was accepted; the log names {rejected_count} rejected "
                "families")
    elif api_answered and "hardware_acceleration" in capabilities:
        # The API answered and named nothing, which is a verdict: this host has
        # no usable hardware encoder.
        report.line("**No hardware encoder was accepted by the probe.**")
        if no_hardware:
            report.conclude(
                f"no hardware encoder proved usable; the server is transcoding on the CPU "
                f"(the log names {rejected_count} rejected families)")
    else:
        report.line("**No hardware encoder was accepted by the probe.**")
        if no_hardware:
            report.conclude(
                f"no hardware encoder proved usable; the server is transcoding on the CPU "
                f"(the log names {rejected_count} rejected families)")

    # An encoder family that merely appears in `video_encoders` is compiled in,
    # not proved: say so, because the distinction is the whole point of the probe.
    offered = [e for e in encoders if e.split("_")[-1] in FAMILIES]
    if offered and not accepted:
        report.line(f"Hardware encoders ffmpeg offers but the probe did not accept: "
                    f"{', '.join(f'`{e}`' for e in offered)}.")

    if render_node:
        report.line(f"**Render node:** `{render_node}`")
    elif not accepted:
        report.line("**Render node:** none recorded")

    # One row per encoder: the log and the API report the same rejections, and
    # the API's wording is usually the fuller one, so prefer the longest reason
    # seen for each encoder rather than listing it twice.
    best_reason: dict[str, str] = {}
    for encoder, reason in list(rejects) + [
            (str(e.get("encoder")), str(e.get("reason")))
            for e in api_rejects if isinstance(e, dict)]:
        if not encoder:
            continue
        if encoder not in best_reason or len(reason) > len(best_reason[encoder]):
            best_reason[encoder] = reason

    if best_reason:
        report.line()
        report.line("Rejections, with ffmpeg's own complaint and what it means:")
        report.line()
        report.line("| encoder | cause | ffmpeg said |")
        report.line("| --- | --- | --- |")
        for encoder in sorted(best_reason):
            cause, _advice = classify_hw_failure(encoder, best_reason[encoder])
            report.line(f"| `{encoder}` | {cause} | {best_reason[encoder]} |")

        # Group by the fix, not by the cause label: VAAPI and NVENC are both
        # "missing userspace driver" but need different packages installed, so
        # naming every encoder under either fix would be wrong about one of them.
        by_fix: dict[tuple[str, str], list[str]] = defaultdict(list)
        for encoder, reason in best_reason.items():
            by_fix[classify_hw_failure(encoder, reason)].append(encoder)
        report.line()
        report.line("### What each failure needs")
        report.line()
        for (cause, why), names in sorted(by_fix.items()):
            joined = ", ".join(f"`{e}`" for e in sorted(names))
            report.line(f"- **{cause}** ({joined}): {why}")
            report.conclude(f"{cause} — {joined} rejected for that reason")
    if api_rejects:
        report.line()
        report.line("`rejected_encoders` from the API (verbatim):")
        report.line()
        report.line("| encoder | reason |")
        report.line("| --- | --- |")
        for entry in api_rejects:
            if isinstance(entry, dict):
                report.line(f"| `{entry.get('encoder', '?')}` | {entry.get('reason', '')} |")
    report.line()
    return {"accepted": accepted, "rejects": rejects, "render_node": render_node,
            "encoders": encoders, "no_hardware": no_hardware,
            "rejected_count": rejected_count,
            "best_reason": best_reason}


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

    def section_failed(section_name: str) -> bool:
        """True when the section records an ls error rather than a listing."""
        text = section(section_name, dri_body)
        if not text.strip():
            return True
        return "No such file" in text or "cannot access" in text

    def section_has(section_name: str, pattern: str) -> bool:
        return (not section_failed(section_name)
                and re.search(pattern, section(section_name, dri_body)) is not None)

    # `/dev/dri` is a directory and `ls -l` may print bare names for an
    # unprivileged process, so the explicit `ls /dev/dri/renderD*` section is
    # the reliable signal and the listing is the fallback.
    dri_in_container = section_has("render nodes", r"/dev/dri/renderD\d")
    if not dri_in_container:
        dri_in_container = section_has(
            "/dev/dri", r"/dev/dri/(card|renderD)\d|^\S{10}\s+\S+\s+\S+\s+\S+\s+\S+\s+card\d")
    nvidia_in_container = section_has("nvidia nodes", r"/dev/nvidia\d")

    # Whether a GPU was actually requested, as opposed to merely being present
    # on the host. `--gpus all` populates DeviceRequests and leaves Devices
    # empty; `--runtime=nvidia` does neither, so the runtime name is included.
    gpu_explicitly_requested = (has_json_value(requests_raw)
                                or "nvidia" in runtime.lower())
    dri_explicitly_requested = has_json_value(devices_raw) or has_json_value(requests_raw)
    gpu_accepted = (any("nvenc" in e for e in hw.get("accepted", []))
                    or any(e.lower().endswith("_nvenc") for e in hw.get("encoders", [])))

    # The cause of each rejection, so the GPU section can tell "the device was
    # never passed in" apart from "the device is here but its driver is not".
    reject_causes = {classify_hw_failure(encoder, reason)[0]
                     for encoder, reason in hw.get("rejects", [])}
    vaapi_rejected = [e for e in hw.get("rejects", []) if "vaapi" in e[0].lower()]
    vaapi_needs_driver = any(
        classify_hw_failure(encoder, reason)[0] == "missing userspace driver"
        for encoder, reason in vaapi_rejected)
    nvidia_needs_driver = any(
        classify_hw_failure(encoder, reason)[0] == "missing userspace driver"
        for encoder, reason in hw.get("rejects", []) if "nvenc" in encoder.lower())

    if host_nodes and not dri_in_container:
        report.line("> **The host has a render node that the container cannot see.** "
                    "VAAPI cannot be used: the device is not in the container, whatever the "
                    "host driver offers. Pass it in with `--device /dev/dri`.")
        report.conclude("the container was not given `/dev/dri`, so VAAPI is unavailable "
                        "even though the host exposes a render node")
    elif host_nodes and dri_in_container and vaapi_needs_driver:
        report.line("> **The render node is mounted and VAAPI still fails.** The device is not "
                    "the problem — `libva` has no vendor backend inside the container. The "
                    "runtime image deliberately ships ffmpeg's VAAPI support but not the "
                    "userspace driver, so this is expected unless the image or the container "
                    "adds one. Note this failure mode means mounting `/dev/dri` is necessary "
                    "but *not* sufficient.")
        report.conclude("`/dev/dri` is visible but the VAAPI userspace driver is missing inside "
                        "the container, so VAAPI is rejected for a driver reason rather than a "
                        "device reason")
    elif host_nodes and dri_in_container and not dri_explicitly_requested:
        report.line("- The container can see `/dev/dri`, and VAAPI was neither accepted nor "
                    "offered — check the rejections above for the driver-level reason "
                    "(an Intel host needs `intel-media-va-driver` / `mesa-va-drivers` inside "
                    "the container, which the image does not ship).")

    if host_nvidia:
        if nvidia_in_container:
            report.line("- The container can see NVIDIA device nodes, so the toolkit is "
                        "configured. Read the NVENC rejection above for what ffmpeg said.")
        elif gpu_explicitly_requested:
            report.line("> The container was configured for a GPU but no `/dev/nvidia*` node "
                        "appears inside it. Run `nvidia-smi` inside the container; if that "
                        "fails, the container toolkit or the `--gpus` argument is the suspect.")
            report.conclude("a GPU was requested but the device nodes are absent inside the "
                            "container; the NVIDIA container toolkit is the suspect")
        else:
            report.line("> **The host has an NVIDIA GPU and the container was never given "
                        "it.** There is no `--gpus` request and no `--device /dev/nvidia*`, "
                        "so NVENC cannot work however the image is built.")
            report.conclude("the container was not given the NVIDIA GPU (no `--gpus` / "
                            "`--runtime=nvidia`), so NVENC is unavailable")
        if gpu_accepted:
            report.line("- NVENC was nonetheless accepted by the probe, so the GPU is "
                        "reaching ffmpeg some other way.")
        elif nvidia_needs_driver:
            report.line("- NVENC failed for a driver reason rather than a device one: the "
                        "nodes are visible, so what is missing is the injected userspace "
                        "library, not the `--gpus` argument.")
    if not host_nodes and not host_nvidia:
        report.line("- The host itself reports no render node and no NVIDIA device, so no "
                    "hardware encoder could have worked here. CPU transcoding is the correct "
                    "outcome on this machine, not a bug.")
        report.conclude("the host has no GPU devices at all, so CPU transcoding is correct here")
    if not host_smi:
        report.line("- `nvidia-smi` is not on the host, so there is no NVIDIA driver "
                    "userspace for any container to be given.")
    if (vaapi_needs_driver or nvidia_needs_driver) and not hw.get("accepted"):
        report.caveat("the GPU-side evidence is a *failure to initialise* rather than a "
                      "successful encode, so it does not yet show that a hardware encode "
                      "works on this host. Getting one accepted family is the higher-value "
                      "result: the project has never run any of them on real hardware")
    elif hw.get("accepted"):
        report.caveat("the probe accepted a hardware encoder, but this bundle has no session "
                      "that actually used it. A bundle collected during a hardware transcode "
                      "is what shows the ceiling holding and the arguments the real session "
                      "got (S-1, S-15)")

    report.line("### Userspace drivers inside the container")
    report.line()
    driver_inventory(bundle, report)


def driver_inventory(bundle: Bundle, report: Report) -> None:
    """What the image actually carries, so a driver claim is evidence, not inference."""
    vaapi = bundle.read("inside/vaapi-drivers.txt")
    nvidia = bundle.read("inside/nvidia-libs.txt")

    def found(body: str | None, needle: str) -> bool:
        # A word-boundary match: `libcuda.so` must not be satisfied by
        # `libcudart.so`, and `libva.so` must not be satisfied by `libva-drm.so`.
        if not body:
            return False
        return re.search(rf"(?<![\w.-]){re.escape(needle)}(?![\w-])", body) is not None

    vaapi_text = vaapi or ""

    vaapi_present = [
        name for name, needle, pattern in (
            ("`libva`", "libva.so", r"(?<![\w.-])libva\.so(?![\w-])"),
            ("`iHD_drv_video.so` (Intel)", "iHD_drv_video.so", None),
            ("`i965_drv_video.so` (Intel, legacy)", "i965_drv_video.so", None),
            ("`radeonsi_drv_video.so` (AMD)", "radeonsi_drv_video.so", None),
            ("`nouveau_drv_video.so`", "nouveau_drv_video.so", None),
        )
        if (re.search(pattern, vaapi_text) if pattern else found(vaapi, needle))
    ]
    nvidia_present = [name for name, needle in (
        ("`libnvidia-encode.so`", "libnvidia-encode.so"),
        ("`libcuda.so`", "libcuda.so"),
    ) if found(nvidia, needle)]

    report.line(f"- **VAAPI backend**: "
                f"{', '.join(vaapi_present) if vaapi_present else 'none found'}")
    report.line(f"- **NVIDIA driver libraries**: "
                f"{', '.join(nvidia_present) if nvidia_present else 'none found'}")
    only_libva = [p for p in vaapi_present if p == "`libva`"]
    if only_libva and len(vaapi_present) == 1:
        report.line("  - `libva` itself is present but no vendor backend is, which is exactly "
                    "the state that makes VAAPI fail with an unknown libva error.")
    report.line()
    report.line("The raw listing is in `inside/vaapi-drivers.txt` and "
                "`inside/nvidia-libs.txt` in the bundle.")
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


def ffmpeg_argv_section(bundle: Bundle, report: Report) -> None:
    """The running ffmpeg command line is the strongest artifact in a bundle.

    Reading the flags the server actually passed settles several review findings
    that were previously only reasoned about, and it is only present if the
    collection happened while a session was live.
    """
    body = bundle.read("inside/identity-and-data.txt") or ""
    match = re.search(r"^== processes ==$(.*)", body, re.S | re.M)
    if not match:
        return
    # `ps` output is `PID ARGS`; the /proc fallback is `PID ARGS` too.
    argvs = [ln.strip() for ln in match.group(1).splitlines()
             if re.search(r"(^|\s)ffmpeg\b", ln)]
    report.line("## Running ffmpeg command line")
    report.line()
    if not argvs:
        report.line("No ffmpeg process was running when the bundle was collected, so the "
                    "arguments the server actually passed are not in evidence. Collect again "
                    "while a transcode is live to capture them — this is the strongest single "
                    "artifact for the streaming findings.")
        report.line()
        return

    argv = argvs[0]
    report.line("A session was live, and this is the command the server built:")
    report.line()
    report.line("```")
    report.line(argv[:2000])
    report.line("```")
    report.line()

    def has(flag: str) -> bool:
        return re.search(rf"(^|\s){re.escape(flag)}(\s|$)", argv) is not None

    def value_of(flag: str) -> str | None:
        found = re.search(rf"(^|\s){re.escape(flag)}\s+(\S+)", argv)
        return found.group(2) if found else None

    encoder = value_of("-c:v")
    # (finding, flag-present, what to say about it). Presence is reported as an
    # observation, not a verdict: software and VAAPI are deliberately given no
    # forced-IDR flag, and no encoder gets a bitrate target on a CRF path.
    checks: list[tuple[str, bool, str]] = [
        ("S-17", has("-protocol_whitelist"),
         "`-protocol_whitelist` is present, which is the hardening the review could not "
         "reproduce a risk for"),
        ("S-15", has("-forced-idr") or has("-forced_idr"),
         "forced-IDR: the spelling matters (NVENC `-forced-idr`, QSV/AMF `-forced_idr`), and "
         "software/VAAPI are deliberately given none"),
        ("S-1", has("-b:v") or has("-maxrate"),
         "a bitrate target or ceiling is passed, which is what made the limit bite"),
        ("S-5", not has("scale_vaapi") or has("hwupload"),
         "no software compositor is being handed hardware frames"),
    ]
    if encoder:
        checks.append(("encoder choice", True, f"the video encoder chosen was `{encoder}`"))

    report.line("| finding | observation | note |")
    report.line("| --- | --- | --- |")
    for finding, observed, description in checks:
        report.line(f"| **{finding}** | `{'present' if observed else 'absent'}` | {description} |")
    report.line()
    if encoder and encoder not in ("libx264", "libx265", "libvpx-vp9", "libsvtav1",
                                   "libaom-av1", "libopus", "aac"):
        report.conclude(f"the live session used the hardware encoder `{encoder}`, and its "
                        "argument list is in the bundle")
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
    # from the request, so every method a client invents becomes its own series.
    # Report the count as a fact and only call it a problem when the set is
    # larger than the methods a browser and a client actually send.
    methods = {labels.get("method", "") for name, labels, _ in parse_prometheus_series(metrics_body)
               if name == "astraeus_http_requests_total"}
    if methods:
        report.line(f"`astraeus_http_requests_total` currently carries "
                    f"{len(methods)} distinct `method` label value(s): "
                    f"{', '.join(f'`{m}`' for m in sorted(methods))}. "
                    "Nothing bounds that set — a client that sends a novel method adds a "
                    "permanent series (A-4).")
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
