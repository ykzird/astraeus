# Diagnostics collection

Two scripts for getting evidence out of a running Astraeus instance and into a
form that can be read and acted on.

| Script | Runs where | What it does |
| --- | --- | --- |
| `collect-astraeus-diagnostics.sh` | the Docker **host** | gathers logs, metrics, API responses, container and host state into one `.tar.gz` |
| `analyze-astraeus-bundle.py` | anywhere | turns that tarball into a Markdown report with a verdict and the findings it bears on |

The collector is read-only with respect to the running instance. It never
restarts, reconfigures or stops anything, and the only file it writes inside the
container is a temporary copy of the database under `/tmp`, which it removes.

## Collect

```sh
# from the repository, with the container already running
scripts/collect-astraeus-diagnostics.sh

# name the container explicitly, sample for two minutes
ASTRAEUS_CONTAINER=astraeus ASTRAEUS_SAMPLE_SECONDS=120 \
  scripts/collect-astraeus-diagnostics.sh
```

It writes `astraeus-diag-<host>-<timestamp>.tar.gz` to the current directory and
prints its size and SHA-256.

### Collecting while a stream is live

Most of the useful evidence only exists while a transcode is running: the
`ffmpeg` command line the server built, `astraeus_stream_sessions_active`, and
the session directory. Start playback, then collect:

```sh
ASTRAEUS_SAMPLE_SECONDS=60 scripts/collect-astraeus-diagnostics.sh
```

### Environment variables

| Variable | Default | Purpose |
| --- | --- | --- |
| `ASTRAEUS_CONTAINER` | auto-detected | container name or id; auto-detection matches a running container whose entrypoint is `astraeus-server` |
| `ASTRAEUS_OUT_DIR` | `$PWD` | where the tarball is written |
| `ASTRAEUS_SAMPLE_SECONDS` | `30` | how long to sample `docker stats` |
| `ASTRAEUS_SAMPLE_INTERVAL` | `5` | seconds between samples |
| `ASTRAEUS_LOG_TAIL` | `3000` | lines kept in the filtered log files |
| `ASTRAEUS_COPY_DB` | `1` | copy the SQLite database (set `0` to skip) |
| `ASTRAEUS_COPY_STREAM_LISTING` | `1` | list the stream root |
| `ASTRAEUS_REDACT` | `1` | redact secrets; see below |
| `ASTRAEUS_CAPTURE_PID` | `0` | if set to a PID, sample until that process exits |

## What is in the bundle

```
meta/            collector and container provenance, base URL
logs/            the full log, plus filtered error/hardware/HTTP views
api/             /api/health, /api/system/capabilities, /api/libraries
metrics/         the Prometheus scrape
samples/         docker stats over the sampling window, and a second scrape
inside/          ffmpeg version/encoders/device nodes, running ffmpeg command
                 lines, /data listing, OCR engine, cgroup limits
container/       docker inspect: env, cmd, mounts, devices, runtime, health
host/            GPUs and drivers on the host, side by side with the container
database/        a consistent copy of astraeus.db (plus -wal and -shm)
state/           one-shot process list, stream-root listing
```

`README.sh` at the top of the bundle prints a summary of it without any tooling.

### Redaction

By default the collector replaces API keys, tokens, secrets, passwords and
`Authorization` headers with `<redacted>`, including a TMDB `api_key=` query
value in a log line. Set `ASTRAEUS_REDACT=0` only when a value is needed to
reproduce a bug, and say so when you send it.

## Analyze

```sh
scripts/analyze-astraeus-bundle.py astraeus-diag-host-20261009T203257Z.tar.gz > report.md
```

The report contains:

- the hardware encoder probe — which family was accepted and ffmpeg's own
  complaint for every one that was rejected;
- host GPUs versus what the container can see, which is where "the GPU is not
  used" is usually answered;
- the KPI table, with "declared but no sample yet" called out separately;
- the running `ffmpeg` command line, checked against the flags several review
  findings are about;
- a list of review findings whose symptoms appear in the captured logs, with the
  matching lines.

It works on an unpacked directory as well as a tarball.

## When a bundle is worth sending

It is worth sending when one of these is true. Each is something the bundle
answers and the source cannot:

1. **The startup encoder probe ran on real hardware.** `hardware_acceleration`,
   `render_node` and the `rejected_encoders` reasons are the first real evidence
   this project would have for VAAPI, QuickSync or NVENC. What the code *would*
   do is already known from the unit tests; what the driver does is not.
2. **A GPU exists on the host but not in the container.** `host/gpu-visibility.txt`
   puts both answers in one file, so the missing `--device /dev/dri` or
   `--gpus all` is visible rather than inferred.
3. **A transcode behaved badly** — `stream_start_failed`, a fallback to
   software, a session that stalled. The log plus the live ffmpeg argv plus
   `transcode_fallbacks_total` is the whole picture for one failing session.
4. **A metric or KPI question is about this instance** rather than the code:
   `first_segment_seconds` against `transcode_startup_seconds`, the
   stream error rate, label cardinality on `http_requests_total`.

It is **not** worth sending for a bug that reproduces from a fixture, or for
anything in the UI's own logic: neither is in the bundle, and `web/` has unit
tests that answer faster. A bundle from an idle server with no GPU is mostly a
confirmation that nothing happened.

## Requirements

The collector needs `docker`, `tar` and `gzip`; `jq` and `curl` are used when
present. It prefers `curl` over the published port and falls back to `docker exec`
inside the container, so it works when the port is bound to loopback or not
published at all.

The analyzer needs Python 3.10 or newer and nothing else — it reads the tarball
with the standard library.
