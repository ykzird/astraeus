# Load and metrics harness

Optional developer tooling. This puts the server under synthetic load — or
records a real person using it — and reports what the server did, using its own
`/metrics` as the evidence. It is the measurement counterpart to
[`scripts/ui-verify/`](../ui-verify), which checks that playback *works*; this
one asks what it *costs*.

It needs Node (built-in `fetch`, so no npm dependencies) and a built server. It
is not shipped in a release archive.

## What it measures, and why from two places

Every phase is measured twice, deliberately:

- **the client** records what a viewer would feel — how long the negotiation
  took, how long until the first segment arrived, how many bytes were moved;
- **the server's `/metrics`** is scraped before and after the phase, so the
  numbers the server reports about itself can be held against what the client
  saw.

A KPI that disagrees with the client is a bug in one of the two, and printing
both side by side is the cheapest way to notice. (It has already earned its
keep: a unit mistake in this harness once reported a 126 ms negotiation as 126
seconds, which the server's own log contradicted.)

## Running

Against a server you started yourself:

```sh
node scripts/load-verify/load-verify.mjs all \
  --base http://127.0.0.1:8940 --entity <entityId>
```

Self-contained — the command starts the server, measures, and stops it:

```sh
cp demo.db .tmp/load-verify/load.db      # a COPY; never point this at real.db
node scripts/load-verify/load-verify.mjs all \
  --spawn --db .tmp/load-verify/load.db --entity <entityId>
```

`--spawn` is the only form that can report CPU. On Linux a process is visible in
`/proc` only inside the PID namespace it was started in, so a sampler running in
a different shell than the server sees nothing at all. If you run the server
separately, start it from the same shell as the harness if you want CPU numbers.

Options:

| Flag | Meaning |
| --- | --- |
| `--streams 1,2,4` | concurrent streams, run in sequence |
| `--hold SECONDS` | how long each stream is read for (default 20) |
| `--height N` | `max_height` in the manifest (default 1080) |
| `--api-seconds N` | duration of the API phase (default 5) |
| `--api-concurrency N` | concurrent API clients (default 8) |
| `--out DIR` | where snapshots and the server log are written |
| `--start-timeout N` | seconds to wait for a negotiation (default 180) |

The manifest declares `max_height` with no `preferred_height`, so a stream is
exactly one rendition. A ladder would multiply the encodes and make a
"streams=4" column mean four ladders rather than four transcodes.

## Recording a real session

`metrics-watch.mjs` samples `/metrics` on an interval while a person clicks
through the UI, and writes every scrape with a timestamp:

```sh
node scripts/load-verify/metrics-watch.mjs --out live.jsonl --interval 2
# ... someone uses the UI ...
node scripts/load-verify/metrics-watch.mjs --report live.jsonl
```

The report is a timeline: when each session started, when the first segment
arrived, how transcode startup moved, what the CPU did. This is the honest way to
measure a real session, and the only way to exercise paths a synthetic harness
does not know about.

`analyse.mjs` diffs two scrapes, which is what a live session's before/after
snapshots are:

```sh
curl -s localhost:8940/metrics > before.prom     # before the session
curl -s localhost:8940/metrics > after.prom      # after it
node scripts/load-verify/analyse.mjs before.prom after.prom
```

## Files

| File | Purpose |
| --- | --- |
| `load-verify.mjs` | synthetic load: API reads and concurrent HLS streams |
| `metrics-watch.mjs` | record a live human session as a metrics timeline |
| `analyse.mjs` | diff two snapshots into a session report |
| `prom.mjs` | Prometheus text parsing, histogram quantiles, deltas |
| `cpu.mjs` | per-process and whole-host CPU from `/proc` |
| `args.mjs` | the shared flag parser |

## Direct play

A 1080p H.264 file is delivered as-is, so no ffmpeg starts and the interesting
result is usually how little CPU it took rather than how many bytes moved. In
that mode the harness reads the original file in 1 MiB ranges for the length of
the hold, and the `segments` column counts those range reads. On loopback this
saturates the HTTP path rather than any real network, so read the rate as a
ceiling and the CPU figure as the point.

## Reading the numbers honestly

- **Transcode-startup percentiles are measurements now, and the rest are not.**
  The metrics that measure a transcode starting use their own bounds
  (`TranscodeBuckets`), which are fine-grained through the seconds a transcode
  actually takes; before round 14 they used `DefaultBuckets`, whose `2.5 → 5 →
  10` jump made a `p50` in that range an interpolation rather than a measurement
  (it read 7.50s where the client measured 6.10s). Everything still on
  `DefaultBuckets` - scans, requests, provider calls - is interpolated across
  those same wide bins, so treat a percentile there that lands between two
  bounds accordingly. Client-side timings do not have this problem.
- **Every HTTP request is logged at INFO**, so request logging is inside the
  measured path. That is the real configuration and worth measuring, but it
  means the API phase numbers include the log write.
- **`--hold` bounds a stream, it does not define one.** A playlist that ends
  early (`#EXT-X-ENDLIST`) is consumed once and the worker stops; a live
  transcode is polled until the hold expires.
- **A 4K transcode will saturate this host.** Four concurrent 4K transcodes is a
  deliberate stress test, not a normal workload: the machine becomes
  unresponsive and every timing is inflated by contention. Read the shape of the
  curve, not the absolute numbers.
