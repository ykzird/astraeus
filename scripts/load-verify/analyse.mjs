// analyse.mjs — turn two /metrics snapshots into a session report.
//
//   node analyse.mjs before.prom after.prom
//
// Everything here is a delta: the server has been up longer than the session,
// so only the change between the two scrapes says what the session cost.

import { readFileSync } from 'node:fs';
import {
  parse, sum, series, histogram, quantile, mean, subtractHistograms,
  counterDeltas, gauge, seconds, round,
} from './prom.mjs';

const [beforeFile, afterFile] = process.argv.slice(2);
if (!beforeFile || !afterFile) {
  console.error('usage: node analyse.mjs <before.prom> <after.prom>');
  process.exit(2);
}

const before = parse(readFileSync(beforeFile, 'utf8'));
const after = parse(readFileSync(afterFile, 'utf8'));

// maxBucket is the bound of the last bucket that holds any observation, which
// is the most a bucket histogram can say about the maximum. Anything above it
// landed in +Inf and is reported as ">largest".
function maxBucket(buckets) {
  const total = buckets[buckets.length - 1]?.cumulative ?? 0;
  if (total <= 0) return null;
  for (const b of buckets) {
    if (b.cumulative >= total) return Number.isFinite(b.le) ? b.le : null;
  }
  return null;
}

function latencyRows(name, labelName) {
  const rows = [];
  const labelSets = new Set();
  for (const s of series(after, `${name}_count`)) {
    labelSets.add(labelName ? (s.labels[labelName] ?? '') : '');
  }
  labelSets.add(''); // the aggregate over every label set

  for (const value of labelSets) {
    const filter = labelName && value ? { [labelName]: value } : {};
    const h = histogram(after, name, filter);
    const b = histogram(before, name, filter);
    const delta = subtractHistograms(b, h);
    if (delta.count === 0) continue;
    const max = maxBucket(delta.buckets);
    rows.push({
      label: value || 'all',
      n: delta.count,
      mean: mean(delta),
      p50: quantile(delta.buckets, 0.5),
      p95: quantile(delta.buckets, 0.95),
      p99: quantile(delta.buckets, 0.99),
      max,
      sum: delta.sum,
    });
  }
  return rows;
}

function printLatency(title, name, labelName) {
  const rows = latencyRows(name, labelName);
  console.log(`\n**${title}**\n`);
  if (rows.length === 0) {
    console.log('_no observations_\n');
    return;
  }
  console.log('| ' + [labelName ?? 'scope', 'n', 'mean', 'p50', 'p95', 'p99', 'max bucket'].join(' | ') + ' |');
  console.log('| --- | ---: | ---: | ---: | ---: | ---: | ---: |');
  for (const r of rows) {
    console.log(
      `| ${r.label} | ${r.n} | ${seconds(r.mean)} | ${seconds(r.p50)} | ${seconds(r.p95)} | ` +
        `${seconds(r.p99)} | ${r.max === null ? '—' : seconds(r.max)} |`,
    );
  }
  console.log('');
}

for (const [title, name] of [
  ['HTTP request duration', 'astraeus_http_request_seconds'],
  ['First segment (FTTT)', 'astraeus_first_segment_seconds'],
  ['Transcode startup', 'astraeus_transcode_startup_seconds'],
  ['Metadata lookup', 'astraeus_metadata_lookup_seconds'],
  ['Scan', 'astraeus_scan_seconds'],
]) {
  printLatency(title, name, name === 'astraeus_http_request_seconds' ? 'method' : 'mode');
}

console.log('\n**Counters (delta over the session)**\n');
for (const [title, name] of [
  ['HTTP requests', 'astraeus_http_requests_total'],
  ['Playback decisions (mode)', 'astraeus_playback_decisions_total'],
  ['Streaming sessions (mode/outcome)', 'astraeus_stream_sessions_total'],
  ['Stream errors', 'astraeus_stream_errors_total'],
  ['Probe errors', 'astraeus_probe_errors_total'],
  ['Transcode fallbacks (hardware→software)', 'astraeus_transcode_fallbacks_total'],
  ['Rate limited', 'astraeus_rate_limited_total'],
  ['Auth granted', 'astraeus_auth_granted_total'],
  ['Auth denied', 'astraeus_auth_denied_total'],
  ['Spans dropped', 'astraeus_spans_dropped_total'],
  ['Scan runs', 'astraeus_scan_runs_total'],
  ['Scan files', 'astraeus_scan_files_total'],
]) {
  const deltas = counterDeltas(before, after, name);
  const parts = [...deltas.entries()].filter(([, v]) => v !== 0).map(([k, v]) => `${k} = ${v}`);
  const total = [...deltas.values()].reduce((a, b) => a + b, 0);
  console.log(`- **${title}** — total ${total}${parts.length ? ': ' + parts.join('; ') : ' (no change)'}`);
}

console.log('\n**Gauges at the end of the session**\n');
console.log(`- sessions active: ${gauge(after, 'astraeus_stream_sessions_active')}`);

const httpTotal = [...counterDeltas(before, after, 'astraeus_http_requests_total').values()]
  .reduce((a, b) => a + b, 0);
const sessionCount = [...counterDeltas(before, after, 'astraeus_stream_sessions_total').values()]
  .reduce((a, b) => a + b, 0);
const decisionCount = [...counterDeltas(before, after, 'astraeus_playback_decisions_total').values()]
  .reduce((a, b) => a + b, 0);

console.log('\n**Headline**\n');
console.log(`- ${httpTotal} HTTP requests, ${decisionCount} playback negotiations, ${sessionCount} streaming sessions.`);
const trans = subtractHistograms(
  histogram(before, 'astraeus_first_segment_seconds', { mode: 'transcode' }),
  histogram(after, 'astraeus_first_segment_seconds', { mode: 'transcode' }),
);
const remux = subtractHistograms(
  histogram(before, 'astraeus_first_segment_seconds', { mode: 'remux' }),
  histogram(after, 'astraeus_first_segment_seconds', { mode: 'remux' }),
);
if (trans.count) {
  console.log(
    `- FTTT transcode: mean ${seconds(mean(trans))}, p95 ${seconds(quantile(trans.buckets, 0.95))}, ` +
      `worst bucket ${seconds(maxBucket(trans.buckets))} over ${trans.count} sessions.`,
  );
}
if (remux.count) {
  console.log(`- FTTT remux: mean ${seconds(mean(remux))} over ${remux.count} session(s).`);
}
