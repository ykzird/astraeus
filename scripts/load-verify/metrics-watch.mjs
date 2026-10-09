// metrics-watch.mjs — sample /metrics on an interval and keep every scrape.
//
// This is the "record a live session" half of the harness: a person clicks
// through the UI while this runs, and the file it writes is the evidence. The
// scrapes are the server's own account of what it did — first-segment latency,
// transcode startup, decisions, request rates — with the timestamps added here
// so the session can be read back as a timeline.
//
//   node metrics-watch.mjs --out live.jsonl --interval 2
//   node metrics-watch.mjs --report live.jsonl
//
// Ctrl-C stops it; the summary prints either way.

import { appendFileSync, readFileSync, writeFileSync } from 'node:fs';
import {
  parse, sum, series, histogram, quantile, mean, seconds, round,
} from './prom.mjs';
import { CpuSampler } from './cpu.mjs';

const args = parseArgs(process.argv.slice(2));

if (args.report) {
  printReport(readFileSync(args.report, 'utf8'));
  process.exit(0);
}

const base = (args.base ?? 'http://127.0.0.1:8940').replace(/\/$/, '');
const interval = Number(args.interval ?? 2) * 1000;
const out = args.out ?? 'live-metrics.jsonl';
const duration = args.seconds ? Number(args.seconds) * 1000 : null;

writeFileSync(out, '');
const started = Date.now();
const cpu = new CpuSampler();
const readings = [];
let stop = false;

process.on('SIGINT', () => {
  stop = true;
});

console.log(`sampling ${base}/metrics every ${interval / 1000}s -> ${out}`);
console.log('time   active  sess  fttt_n  fttt_p50  trans_n  trans_max  dec  req/s  err  cpu  ff');
console.log('-----  ------  ----  ------  --------  -------  ---------  ---  -----  ---  ---  --');

let previous = null;

while (!stop && (duration === null || Date.now() - started < duration)) {
  let text;
  try {
    const response = await fetch(`${base}/metrics`, { signal: AbortSignal.timeout(5000) });
    text = await response.text();
  } catch (err) {
    console.error(`scrape failed: ${err.message}`);
    await sleep(interval);
    continue;
  }

  const at = Date.now();
  const parsed = parse(text);
  const cores = cpu.sample();
  appendFileSync(out, `${JSON.stringify({ t: at, text })}\n`);

  const elapsed = (at - started) / 1000;
  const active = sum(parsed, 'astraeus_stream_sessions_active');
  const sessions = sum(parsed, 'astraeus_stream_sessions_total');
  const fttt = histogram(parsed, 'astraeus_first_segment_seconds');
  const trans = histogram(parsed, 'astraeus_transcode_startup_seconds');
  const decisions = sum(parsed, 'astraeus_playback_decisions_total');
  const requests = sum(parsed, 'astraeus_http_requests_total');
  const errors = sum(parsed, 'astraeus_stream_errors_total');

  let rate = '—';
  if (previous) {
    const dt = (at - previous.at) / 1000;
    const dr = requests - previous.requests;
    if (dt > 0) rate = (dr / dt).toFixed(1);
  }
  previous = { at, requests };

  readings.push({ at, elapsed, active, sessions, ftttCount: fttt.count, decisions, requests, cores });

  console.log(
    `${elapsed.toFixed(0).padStart(5)}s  ` +
      `${active.toFixed(0).padStart(6)}  ` +
      `${sessions.toFixed(0).padStart(4)}  ` +
      `${String(fttt.count).padStart(6)}  ` +
      `${seconds(mean(fttt)).padStart(8)}  ` +
      `${String(trans.count).padStart(7)}  ` +
      `${seconds(quantile(trans.buckets, 1)).padStart(9)}  ` +
      `${decisions.toFixed(0).padStart(3)}  ` +
      `${rate.padStart(5)}  ` +
      `${errors.toFixed(0).padStart(3)}  ` +
      `${(cores ?? 0).toFixed(2).padStart(4)}  ` +
      `${cpu.processCount.toString().padStart(2)}`,
  );

  await sleep(interval);
}

const last = readings[readings.length - 1];
if (last) {
  const busy = readings.map((r) => r.cores).filter((c) => c !== null && c !== undefined);
  console.log('');
  console.log(`captured ${readings.length} scrapes over ${readings.at(-1).elapsed.toFixed(1)}s -> ${out}`);
  console.log(`peak CPU: ${Math.max(...busy, 0).toFixed(2)} cores`);
  printDelta(readings[0], last);
}

function printDelta(first, last) {
  console.log('');
  console.log(`requests served during the session: ${last.requests - first.requests}`);
  console.log(`playback decisions:                  ${last.decisions - first.decisions}`);
  console.log(`streaming sessions started:          ${last.sessions - first.sessions}`);
  console.log(`first-segment measurements:          ${last.ftttCount - first.ftttCount}`);
}

// printReport reads a capture back and renders the timeline of what changed,
// which is how a human-driven session is reviewed after the fact.
function printReport(file) {
  const lines = file.split('\n').filter((l) => l.trim() !== '');
  const scrapes = lines.map((l) => JSON.parse(l));
  if (scrapes.length === 0) {
    console.log('empty capture');
    return;
  }

  const first = scrapes[0].t;
  console.log('time    active  sess  dec  req  fttt_n  fttt(mean/p95)  trans_n  trans(mean/max)  note');
  console.log('------  ------  ----  ---  ---  ------  --------------  -------  ---------------  ----');

  let prev = null;
  for (const scrape of scrapes) {
    const parsed = parse(scrape.text);
    const active = sum(parsed, 'astraeus_stream_sessions_active');
    const sessions = sum(parsed, 'astraeus_stream_sessions_total');
    const decisions = sum(parsed, 'astraeus_playback_decisions_total');
    const requests = sum(parsed, 'astraeus_http_requests_total');
    const fttt = histogram(parsed, 'astraeus_first_segment_seconds');
    const trans = histogram(parsed, 'astraeus_transcode_startup_seconds');

    const notes = [];
    if (prev) {
      if (sessions > prev.sessions) notes.push(`+${sessions - prev.sessions} stream`);
      if (fttt.count > prev.fttt.count) notes.push('first segment');
      if (trans.count > prev.trans.count) notes.push('transcode up');
      if (decisions > prev.decisions) notes.push(`+${decisions - prev.decisions} decision`);
      if (active !== prev.active) notes.push(`active ${prev.active}->${active}`);
    }
    prev = { sessions, fttt, trans, decisions, active };

    console.log(
      `${((scrape.t - first) / 1000).toFixed(0).padStart(5)}s  ` +
        `${active.toFixed(0).padStart(6)}  ` +
        `${sessions.toFixed(0).padStart(4)}  ` +
        `${decisions.toFixed(0).padStart(3)}  ` +
        `${requests.toFixed(0).padStart(3)}  ` +
        `${String(fttt.count).padStart(6)}  ` +
        `${(seconds(mean(fttt)) + '/' + seconds(quantile(fttt.buckets, 0.95))).padStart(14)}  ` +
        `${String(trans.count).padStart(7)}  ` +
        `${(seconds(mean(trans)) + '/' + seconds(quantile(trans.buckets, 1))).padStart(15)}  ` +
        notes.join(', '),
    );
  }
}

function sleep(ms) {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

// parseArgs reads the small `--flag value` / `--flag` surface this script needs.
function parseArgs(argv) {
  const out = {};
  for (let i = 0; i < argv.length; i++) {
    const arg = argv[i];
    if (!arg.startsWith('--')) continue;
    const key = arg.slice(2);
    const next = argv[i + 1];
    if (next === undefined || next.startsWith('--')) out[key] = true;
    else {
      out[key] = next;
      i++;
    }
  }
  return out;
}
