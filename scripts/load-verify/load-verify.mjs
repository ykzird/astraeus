#!/usr/bin/env node
// load-verify.mjs — put the server under synthetic load and report what it did.
//
// Measurements come from two places on purpose. The client records what a viewer
// would feel: how long a negotiation took, how long until the first segment
// arrived, how many bytes a stream moved. The server's own /metrics is scraped
// before and after every phase, so the numbers the server reports about itself
// can be held against what the client measured. A KPI that disagrees with the
// client is a bug in one of the two.
//
//   # against a server you started yourself
//   node load-verify.mjs all --base http://127.0.0.1:8940 --entity <id>
//
//   # self-contained: this command starts the server, runs, and stops it
//   node load-verify.mjs all --spawn --entity <id>
//
// Only the --spawn form can report CPU. On Linux a process is visible in /proc
// only inside the PID namespace it was started in, so a sampler running in a
// different shell than the server sees nothing at all.

import { spawn } from 'node:child_process';
import { mkdirSync, writeFileSync, openSync } from 'node:fs';
import { parse, histogram, quantile, mean, subtractHistograms, counterDeltas, gauge, seconds, millis } from './prom.mjs';
import { CpuSampler, SystemCpuSampler } from './cpu.mjs';
import { parseArgs, number, list, sleep } from './args.mjs';

const args = parseArgs(process.argv.slice(2));

if (args.help) {
  console.log(`usage: node load-verify.mjs [api|stream|all] [options]

  --base URL           server to drive (default http://127.0.0.1:8940)
  --spawn              start ./astraeus-server instead of using --base
  --db PATH            database for --spawn (use a COPY, not real.db)
  --entity ID          entity to play; required for stream and negotiate
  --streams 1,2,4      concurrent streams, run in sequence (default 1,2,4)
  --hold SECONDS       how long each stream is read for (default 20)
  --api-seconds N      duration of the API phase (default 5)
  --api-concurrency N  concurrent API clients (default 8)
  --height N           max_height in the manifest (default 1080)
  --out DIR            where snapshots are written (default .tmp/load-verify)
  --help               this text
`);
  process.exit(0);
}

const phase = args._[0] ?? 'all';
const cfg = {
  base: String(args.base ?? 'http://127.0.0.1:8940').replace(/\/$/, ''),
  spawn: Boolean(args.spawn),
  bin: String(args.bin ?? './astraeus-server'),
  db: String(args.db ?? '.tmp/load-verify/load.db'),
  webDir: String(args['web-dir'] ?? 'web'),
  addr: String(args.addr ?? '127.0.0.1:8940'),
  streamRoot: String(args['stream-root'] ?? '.tmp/load-verify/streams'),
  entity: args.entity ? String(args.entity) : null,
  streams: list(args.streams, [1, 2, 4]),
  holdMs: number(args.hold, 20) * 1000,
  apiSeconds: number(args['api-seconds'], 5),
  apiConcurrency: number(args['api-concurrency'], 8),
  height: number(args.height, 1080),
  out: String(args.out ?? '.tmp/load-verify'),
  startTimeoutMs: number(args['start-timeout'], 180) * 1000,
  segmentTimeoutMs: number(args['segment-timeout'], 60) * 1000,
  cooldownMs: number(args.cooldown, 5) * 1000,
};

mkdirSync(cfg.out, { recursive: true });
mkdirSync(cfg.streamRoot, { recursive: true });

// The manifest a browser declares. max_height with no preferred_height asks for
// exactly one rendition, which is the deterministic request and the one a
// measurement wants: a ladder would multiply the encodes and make a "streams=4"
// column mean four ladders rather than four transcodes.
function manifest(height) {
  return {
    containers: ['mp4', 'webm', 'hls'],
    video_codecs: ['h264', 'vp9', 'av1'],
    audio_codecs: ['aac', 'opus', 'mp3', 'vorbis'],
    max_width: 1920,
    max_height: height,
    max_bit_depth: 8,
    max_audio_channels: 2,
    supports_hls: true,
    subtitles: false,
  };
}

async function snapshot(label) {
  const res = await fetch(`${cfg.base}/metrics`, { signal: AbortSignal.timeout(15000) });
  const text = await res.text();
  if (label) writeFileSync(`${cfg.out}/${label}.prom`, text);
  return parse(text);
}

async function waitForHealth() {
  const deadline = Date.now() + 120000;
  while (Date.now() < deadline) {
    try {
      const res = await fetch(`${cfg.base}/api/health`, { signal: AbortSignal.timeout(3000) });
      if (res.ok) return true;
    } catch {
      // not listening yet
    }
    await sleep(500);
  }
  throw new Error(`server at ${cfg.base} did not become healthy`);
}

async function startServer() {
  const log = openSync(`${cfg.out}/server.log`, 'a');
  const child = spawn(
    cfg.bin,
    [
      'serve',
      '--db', cfg.db,
      '--web-dir', cfg.webDir,
      '--addr', cfg.addr,
      '--enrich-interval', '0',
      '--scan-interval', '0',
      '--stream-root', cfg.streamRoot,
      '--image-cache', `${cfg.out}/images`,
      '--subtitle-cache', `${cfg.out}/subs`,
    ],
    { stdio: ['ignore', log, log] },
  );
  child.on('error', (err) => {
    console.error(`failed to start ${cfg.bin}: ${err.message}`);
  });
  await waitForHealth();
  return child;
}

// percentile interpolates between the two neighbouring samples. The samples are
// the client's own timings, so this is a statement about what was observed, not
// an estimate from buckets.
function percentile(sorted, q) {
  if (sorted.length === 0) return null;
  if (sorted.length === 1) return sorted[0];
  const pos = q * (sorted.length - 1);
  const lo = Math.floor(pos);
  const hi = Math.ceil(pos);
  return sorted[lo] + (sorted[hi] - sorted[lo]) * (pos - lo);
}

function latencyLine(label, values) {
  if (values.length === 0) return `| ${label} | 0 | — | — | — | — |`;
  const sorted = [...values].sort((a, b) => a - b);
  return (
    `| ${label} | ${sorted.length} | ${millis(mean0(sorted))} | ${millis(percentile(sorted, 0.5))} | ` +
    `${millis(percentile(sorted, 0.95))} | ${millis(percentile(sorted, 0.99))} |`
  );
}

function mean0(values) {
  return values.reduce((a, b) => a + b, 0) / values.length;
}

// startCpuWatch samples both what this server used and what the whole host was
// doing, once a second, for the duration of a phase.
function startCpuWatch(comms = ['astraeus-server', 'ffmpeg']) {
  const process = new CpuSampler(comms);
  const system = new SystemCpuSampler();
  process.sample();
  system.sample();

  const watch = { process: [], system: [] };
  const timer = setInterval(() => {
    const p = process.sample();
    if (p !== null) watch.process.push(p);
    const s = system.sample();
    if (s !== null) watch.system.push(s);
  }, 1000);

  return { watch, stop: () => clearInterval(timer) };
}

function printCpu(watch) {
  if (watch.process.length) {
    console.log(
      `- CPU (server + ffmpeg): mean ${mean0(watch.process).toFixed(2)} cores, ` +
        `peak ${Math.max(...watch.process).toFixed(2)} cores over ${watch.process.length} samples`,
    );
  }
  if (watch.system.length) {
    const peak = watch.system.reduce((a, b) => (a.utilization > b.utilization ? a : b));
    const util = mean0(watch.system.map((s) => s.utilization));
    console.log(
      `- CPU (whole host): mean ${(util * 100).toFixed(1)}%, ` +
        `peak ${(peak.utilization * 100).toFixed(1)}% (${peak.busyCores.toFixed(1)} cores busy)`,
    );
  }
}

async function phaseApi() {
  const targets = ['/api/entities', '/api/libraries', '/api/progress'];
  if (cfg.entity) targets.push(`/api/entities/${cfg.entity}`);

  console.log(`\n--- API phase: ${cfg.apiConcurrency} clients for ${cfg.apiSeconds}s ---`);
  const before = await snapshot('api-before');
  const cpuWatch = startCpuWatch(['astraeus-server']);

  const deadline = Date.now() + cfg.apiSeconds * 1000;
  const samples = [];
  let cursor = 0;

  await Promise.all(
    Array.from({ length: cfg.apiConcurrency }, async () => {
      while (Date.now() < deadline) {
        const path = targets[cursor++ % targets.length];
        const started = performance.now();
        let status = 0;
        try {
          const res = await fetch(cfg.base + path, { signal: AbortSignal.timeout(15000) });
          await res.arrayBuffer();
          status = res.status;
        } catch {
          status = -1; // client-side failure: a timeout or a dropped connection
        }
        samples.push({ ms: performance.now() - started, status, path });
      }
    }),
  );

  cpuWatch.stop();
  const elapsed = cfg.apiSeconds;
  const after = await snapshot('api-after');

  const ok = samples.filter((s) => s.status === 200);
  const failed = samples.filter((s) => s.status !== 200);

  console.log('');
  console.log('| scope | n | mean | p50 | p95 | p99 |');
  console.log('| --- | ---: | ---: | ---: | ---: | ---: |');
  console.log(latencyLine('all GETs', samples.map((s) => s.ms)));
  for (const target of targets) {
    console.log(latencyLine(target, samples.filter((s) => s.path === target).map((s) => s.ms)));
  }
  console.log('');
  console.log(`- throughput: **${(samples.length / elapsed).toFixed(0)} req/s** (${samples.length} requests in ${elapsed}s)`);
  console.log(`- statuses: ${summariseStatuses(samples)}`);
  printCpu(cpuWatch.watch);
  printServerDelta(before, after, false);
}

function summariseStatuses(samples) {
  const counts = new Map();
  for (const s of samples) counts.set(s.status, (counts.get(s.status) ?? 0) + 1);
  return [...counts.entries()].sort((a, b) => b[1] - a[1]).map(([k, v]) => `${k}:${v}`).join(' ');
}

// consumeStream reads a live HLS playlist for as long as it is told to, fetching
// each segment once and timing the first. The first segment is the client-side
// counterpart of the server's fttt_latency, so the two can be compared.
async function consumeStream(playlistPath, holdMs) {
  let playlistUrl = new URL(playlistPath, cfg.base).toString();
  const deadline = Date.now() + holdMs;
  const seen = new Set();
  const started = performance.now();
  let segments = 0;
  let bytes = 0;
  let firstSegmentMs = null;
  let resolvedVariant = false;

  while (Date.now() < deadline) {
    const res = await fetch(playlistUrl, { signal: AbortSignal.timeout(cfg.segmentTimeoutMs) });
    if (!res.ok) throw new Error(`playlist ${res.status}`);
    const text = await res.text();
    const lines = text.split('\n').map((l) => l.trim()).filter(Boolean);
    const ended = lines.some((l) => l.startsWith('#EXT-X-ENDLIST'));
    const refs = lines.filter((l) => !l.startsWith('#'));

    // A master playlist points at variant playlists, which point at segments.
    // Descend once so the segment accounting is about segments either way.
    const variant = refs.find((r) => r.split('?')[0].endsWith('.m3u8'));
    if (variant && !resolvedVariant) {
      playlistUrl = new URL(variant, playlistUrl).toString();
      resolvedVariant = true;
      continue;
    }

    for (const ref of refs) {
      const url = new URL(ref, playlistUrl).toString();
      if (seen.has(url)) continue;
      seen.add(url);
      const segment = await fetch(url, { signal: AbortSignal.timeout(cfg.segmentTimeoutMs) });
      if (!segment.ok) continue;
      const body = await segment.arrayBuffer();
      bytes += body.byteLength;
      segments++;
      if (firstSegmentMs === null) firstSegmentMs = performance.now() - started;
    }

    // EXT-X-ENDLIST means the playlist is complete: a single pass has every
    // segment there will ever be, so reading it again is pure load on the
    // server. Without this the loop spins for the rest of the hold — it once
    // fetched the playlist 841 times a second.
    if (ended) break;
    await sleep(400);
  }

  return { firstSegmentMs, segments, bytes };
}

// consumeDirect reads the original file in ranges, the way a player does. Direct
// play starts no ffmpeg at all, so what a concurrent direct-play run measures is
// the read path and the HTTP layer — and the useful result is usually how little
// CPU it took, not how many bytes moved.
const DIRECT_CHUNK = 1024 * 1024;

async function consumeDirect(filePath, holdMs) {
  const url = new URL(filePath, cfg.base).toString();
  const deadline = Date.now() + holdMs;
  const started = performance.now();
  let offset = 0;
  let bytes = 0;
  let requests = 0;
  let firstSegmentMs = null;

  while (Date.now() < deadline) {
    const res = await fetch(url, {
      headers: { Range: `bytes=${offset}-${offset + DIRECT_CHUNK - 1}` },
      signal: AbortSignal.timeout(cfg.segmentTimeoutMs),
    });

    // Past the end of the file: begin again, so a fixture shorter than the hold
    // measures a sustained read rather than a single burst.
    if (res.status === 416) {
      offset = 0;
      continue;
    }
    if (!res.ok && res.status !== 206) throw new Error(`file ${res.status}`);

    const body = await res.arrayBuffer();
    requests++;
    bytes += body.byteLength;
    if (firstSegmentMs === null) firstSegmentMs = performance.now() - started;

    // A 200 means the server ignored the range and sent the whole file; a short
    // body means the end was reached. Either way the next pass starts at zero.
    if (res.status === 200 || body.byteLength < DIRECT_CHUNK) offset = 0;
    else offset += body.byteLength;
  }

  return { firstSegmentMs, segments: requests, bytes };
}

async function streamWorker(index) {
  const result = {
    index,
    mode: null,
    startupMs: null,
    firstSegmentMs: null,
    totalMs: null,
    segments: 0,
    bytes: 0,
    error: null,
    sessionId: null,
  };

  try {
    const started = performance.now();
    const res = await fetch(`${cfg.base}/api/entities/${cfg.entity}/playback`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(manifest(cfg.height)),
      signal: AbortSignal.timeout(cfg.startTimeoutMs),
    });
    result.startupMs = performance.now() - started;

    const text = await res.text();
    if (!res.ok) {
      result.error = `${res.status} ${text.slice(0, 200)}`;
      return result;
    }

    const body = JSON.parse(text);
    result.mode = body.mode;
    result.sessionId = body.session_id ?? null;

    if (!body.url) return result;

    // Direct play hands back the original file rather than a session, and it is
    // the common case for 1080p H.264 — a browser decodes it as it is. Reading
    // it in ranges is what makes that measurable rather than a negotiation time.
    if (!body.session_id) {
      const play = await consumeDirect(body.url, cfg.holdMs);
      Object.assign(result, play);
      result.totalMs = result.startupMs + (play.firstSegmentMs ?? 0);
      return result;
    }

    const play = await consumeStream(body.url, cfg.holdMs);
    Object.assign(result, play);
    result.totalMs = result.startupMs + (play.firstSegmentMs ?? 0);
    return result;
  } catch (err) {
    result.error = err.message;
    return result;
  } finally {
    if (result.sessionId) {
      try {
        await fetch(`${cfg.base}/api/streams/${result.sessionId}`, {
          method: 'DELETE',
          signal: AbortSignal.timeout(15000),
        });
      } catch {
        // the session will be reaped by its TTL; this is not worth failing on
      }
    }
  }
}

async function phaseStream(count) {
  console.log(`\n--- stream phase: ${count} concurrent, ${cfg.holdMs / 1000}s each ---`);
  const before = await snapshot(`stream${count}-before`);

  const cpuWatch = startCpuWatch();

  const started = Date.now();
  const results = await Promise.all(Array.from({ length: count }, (_, i) => streamWorker(i)));
  const elapsed = (Date.now() - started) / 1000;
  cpuWatch.stop();

  const after = await snapshot(`stream${count}-after`);

  console.log('');
  console.log('| worker | mode | negotiation | first data | total | segments | MiB | error |');
  console.log('| ---: | --- | ---: | ---: | ---: | ---: | ---: | --- |');
  for (const r of results) {
    console.log(
      `| ${r.index} | ${r.mode ?? '—'} | ${millis(r.startupMs)} | ${millis(r.firstSegmentMs)} | ` +
        `${millis(r.totalMs)} | ${r.segments} | ${(r.bytes / 1048576).toFixed(2)} | ${r.error ?? ''} |`,
    );
  }

  const startups = results.map((r) => r.startupMs).filter((v) => v !== null);
  const firsts = results.map((r) => r.totalMs).filter((v) => v !== null);
  const totalBytes = results.reduce((a, r) => a + r.bytes, 0);
  const errors = results.filter((r) => r.error);

  console.log('');
  console.log('| scope | n | mean | p50 | p95 | max |');
  console.log('| --- | ---: | ---: | ---: | ---: | ---: |');
  console.log(latencyLine('negotiation (incl. ffmpeg up)', startups));
  console.log(latencyLine('request → first segment', firsts));
  console.log('');
  console.log(`- wall clock: ${elapsed.toFixed(1)}s for ${count} streams`);
  console.log(`- delivered: ${(totalBytes / 1048576).toFixed(2)} MiB (${(totalBytes / elapsed / 1048576).toFixed(2)} MiB/s aggregate)`);
  if (errors.length) console.log(`- **errors: ${errors.length}** — ${errors.map((e) => e.error).join('; ')}`);
  else console.log('- errors: none');
  printCpu(cpuWatch.watch);

  printServerDelta(before, after, true);
}

// printServerDelta is the server's own account of the phase, so it can be set
// against the client-side table above it.
function printServerDelta(before, after, streaming) {
  const rows = [];
  const delta = (name, filter) =>
    subtractHistograms(histogram(before, name, filter), histogram(after, name, filter));

  for (const [label, name, filter] of [
    ['FTTT (all)', 'astraeus_first_segment_seconds', {}],
    ['FTTT transcode', 'astraeus_first_segment_seconds', { mode: 'transcode' }],
    ['FTTT remux', 'astraeus_first_segment_seconds', { mode: 'remux' }],
    ['transcode startup', 'astraeus_transcode_startup_seconds', {}],
  ]) {
    const d = delta(name, filter);
    if (d.count === 0) continue;
    rows.push(
      `| ${label} | ${d.count} | ${seconds(mean(d))} | ${seconds(quantile(d.buckets, 0.5))} | ` +
        `${seconds(quantile(d.buckets, 0.95))} |`,
    );
  }

  if (rows.length) {
    console.log('');
    console.log('server-observed (delta over the phase):');
    console.log('');
    console.log('| metric | n | mean | p50 | p95 |');
    console.log('| --- | ---: | ---: | ---: | ---: |');
    for (const row of rows) console.log(row);
  }

  const decisions = counterDeltas(before, after, 'astraeus_playback_decisions_total');
  const sessions = counterDeltas(before, after, 'astraeus_stream_sessions_total');
  const errors = [...counterDeltas(before, after, 'astraeus_stream_errors_total').values()].reduce((a, b) => a + b, 0);
  const http = [...counterDeltas(before, after, 'astraeus_http_requests_total').values()].reduce((a, b) => a + b, 0);

  console.log('');
  console.log(`- decisions: ${[...decisions.entries()].filter(([, v]) => v).map(([k, v]) => `${k} ${v}`).join(', ') || 'none'}`);
  if (streaming) {
    console.log(`- sessions: ${[...sessions.entries()].filter(([, v]) => v).map(([k, v]) => `${k} ${v}`).join(', ') || 'none'}`);
    console.log(`- stream errors: ${errors}`);
  }
  console.log(`- HTTP requests: ${http}`);
  if (streaming) {
    console.log(`- sessions still active at the end: ${gauge(after, 'astraeus_stream_sessions_active')}`);
  }
}

async function main() {
  if ((phase === 'stream' || phase === 'negotiate') && !cfg.entity) {
    console.error('--entity is required for the stream phase');
    process.exit(2);
  }

  console.log(`target ${cfg.base}${cfg.spawn ? ' (started here)' : ''}, output in ${cfg.out}/`);

  let server = null;
  if (cfg.spawn) {
    server = await startServer();
    console.log(`server up (pid ${server.pid}), log ${cfg.out}/server.log`);
  } else {
    await waitForHealth();
  }

  try {
    if (phase === 'api' || phase === 'all') await phaseApi();
    if (phase === 'stream' || phase === 'all') {
      for (const count of cfg.streams) {
        await phaseStream(count);
        await sleep(cfg.cooldownMs);
      }
    }
  } finally {
    if (server) {
      server.kill('SIGTERM');
      console.log('\nserver stopped');
    }
  }
}

await main();
