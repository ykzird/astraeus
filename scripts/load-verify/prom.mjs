// Minimal Prometheus text-format reader and histogram maths.
//
// The /metrics endpoint is the source of truth for what the server observed, so
// the harness reads it rather than trusting its own client-side clock alone.
// No dependencies: the exposition is simple enough to parse here.

// parse returns { meta, samples } where meta maps a metric name to {type, help}
// and samples is a flat list of { name, labels, value }.
export function parse(text) {
  const meta = new Map();
  const samples = [];

  for (const raw of text.split('\n')) {
    const line = raw.trim();
    if (line === '') continue;

    if (line.startsWith('#')) {
      const m = /^#\s+(HELP|TYPE)\s+(\S+)\s*(.*)$/.exec(line);
      if (!m) continue;
      const [, kind, name, rest] = m;
      const entry = meta.get(name) ?? { type: '', help: '' };
      if (kind === 'HELP') entry.help = rest;
      else entry.type = rest.trim();
      meta.set(name, entry);
      continue;
    }

    const sep = line.lastIndexOf(' ');
    if (sep === -1) continue;
    const value = Number(line.slice(sep + 1));
    if (!Number.isFinite(value)) continue;
    const { name, labels } = parseSeries(line.slice(0, sep));
    samples.push({ name, labels, value });
  }

  return { meta, samples };
}

// parseSeries splits `name{a="1",b="2"}` into a name and a label map. Label
// values may contain commas, spaces and escaped quotes, so this scans rather
// than splitting on punctuation.
function parseSeries(text) {
  const open = text.indexOf('{');
  if (open === -1) return { name: text, labels: {} };

  const name = text.slice(0, open);
  const inner = text.slice(open + 1, text.lastIndexOf('}'));
  const labels = {};

  let i = 0;
  while (i < inner.length) {
    const eq = inner.indexOf('=', i);
    if (eq === -1) break;
    const key = inner.slice(i, eq).trim();
    let j = eq + 1;
    while (j < inner.length && inner[j] === ' ') j++;
    if (inner[j] !== '"') break;
    j++;
    let value = '';
    while (j < inner.length) {
      const c = inner[j];
      if (c === '\\') {
        value += inner[j + 1] ?? '';
        j += 2;
        continue;
      }
      if (c === '"') break;
      value += c;
      j++;
    }
    labels[key] = value;
    i = inner.indexOf(',', j);
    if (i === -1) break;
    i++;
  }

  return { name, labels };
}

// series returns every sample of one metric, optionally filtered by labels.
export function series(parsed, name, filter = {}) {
  return parsed.samples.filter(
    (s) =>
      s.name === name &&
      Object.entries(filter).every(([k, v]) => s.labels[k] === v),
  );
}

// sum adds every sample of a metric, optionally filtered. A counter that has no
// series yet sums to 0, which is what "this has not happened" means.
export function sum(parsed, name, filter = {}) {
  return series(parsed, name, filter).reduce((acc, s) => acc + s.value, 0);
}

// histogram collects the `_bucket`, `_sum` and `_count` series of one histogram
// into a single value. Buckets come back in ascending `le` order, cumulative.
export function histogram(parsed, name, filter = {}) {
  // Series are aggregated by `le`, not concatenated: several label sets (a
  // per-mode histogram, say) each carry their own cumulative buckets, and
  // stacking them would sort duplicates into the distribution and report a
  // percentile belonging to neither. Adding the cumulative counts per bound is
  // what "all of these histograms together" means.
  const byLe = new Map();
  for (const s of series(parsed, `${name}_bucket`, filter)) {
    const le = s.labels.le === '+Inf' ? Infinity : Number(s.labels.le);
    byLe.set(le, (byLe.get(le) ?? 0) + s.value);
  }
  const buckets = [...byLe.entries()]
    .map(([le, cumulative]) => ({ le, cumulative }))
    .sort((a, b) => a.le - b.le);
  const sumValue = sum(parsed, `${name}_sum`, filter);
  const count = sum(parsed, `${name}_count`, filter);
  return { buckets, sum: sumValue, count };
}

// quantile estimates a quantile from cumulative buckets by interpolating inside
// the bucket the rank falls in. This is Prometheus's own histogram_quantile
// algorithm, so the numbers here are comparable with a Grafana panel.
export function quantile(buckets, q) {
  if (buckets.length === 0) return null;
  const total = buckets[buckets.length - 1].cumulative;
  if (total <= 0) return null;
  const rank = q * total;

  let lowerBound = 0;
  let lowerCount = 0;
  for (const bucket of buckets) {
    if (bucket.cumulative >= rank) {
      if (!Number.isFinite(bucket.le)) break;
      const inBucket = bucket.cumulative - lowerCount;
      const fraction = inBucket > 0 ? (rank - lowerCount) / inBucket : 0;
      return lowerBound + fraction * (bucket.le - lowerBound);
    }
    if (Number.isFinite(bucket.le)) lowerBound = bucket.le;
    lowerCount = bucket.cumulative;
  }
  return null;
}

// mean is the histogram's own sum/count, which needs no estimation.
export function mean(hist) {
  return hist.count > 0 ? hist.sum / hist.count : null;
}

// subtractHistograms returns after-minus-before, which is what one load phase
// contributed. Buckets are matched by `le`; a bucket present in only one side is
// treated as absent from the other.
export function subtractHistograms(before, after) {
  const byLe = new Map();
  for (const b of before.buckets) byLe.set(b.le, { le: b.le, cumulative: -b.cumulative });
  for (const b of after.buckets) {
    const entry = byLe.get(b.le) ?? { le: b.le, cumulative: 0 };
    entry.cumulative += b.cumulative;
    byLe.set(b.le, entry);
  }
  return {
    buckets: [...byLe.values()].sort((a, b) => a.le - b.le),
    sum: after.sum - before.sum,
    count: after.count - before.count,
  };
}

// counterDeltas reports each counter's growth between two scrapes, keyed by its
// rendered label set. Counters that did not move are still reported, because
// "still zero" is a result.
export function counterDeltas(before, after, name) {
  const key = (s) =>
    Object.entries(s.labels)
      .sort(([a], [b]) => a.localeCompare(b))
      .map(([k, v]) => `${k}="${v}"`)
      .join(',');

  const out = new Map();
  for (const s of series(after, name)) {
    const prior = series(before, name, s.labels)[0]?.value ?? 0;
    out.set(key(s) || '{}', s.value - prior);
  }
  for (const s of series(before, name)) {
    const k = key(s) || '{}';
    if (!out.has(k)) out.set(k, -s.value);
  }
  return out;
}

// gauge returns a gauge's current value, or 0 when it has no series yet.
export function gauge(parsed, name, filter = {}) {
  return sum(parsed, name, filter);
}

// round trims a float for display.
export function round(value, digits = 3) {
  if (value === null || value === undefined || !Number.isFinite(value)) return null;
  const f = 10 ** digits;
  return Math.round(value * f) / f;
}

// seconds renders a duration the way a person reads it. Its input is seconds,
// which is what a Prometheus histogram carries.
export function seconds(value) {
  if (value === null || value === undefined || !Number.isFinite(value)) return '—';
  if (value < 1) return `${Math.round(value * 1000)}ms`;
  return `${value.toFixed(2)}s`;
}

// millis renders a client-side duration, which `performance.now()` reports in
// milliseconds. Keeping this separate from seconds() is not fussiness: feeding a
// millisecond value to seconds() reports a 126 ms negotiation as 126 seconds,
// which is exactly the sort of confidently wrong number this harness exists to
// avoid.
export function millis(value) {
  if (value === null || value === undefined || !Number.isFinite(value)) return '—';
  if (value < 1000) return `${value.toFixed(value < 10 ? 1 : 0)}ms`;
  return `${(value / 1000).toFixed(2)}s`;
}

// summary prints the histograms and counters worth reading after a run.
export function summarise(parsed, { title }) {
  const lines = [];
  const row = (label, hist) => {
    if (!hist || hist.count === 0) {
      lines.push(`| ${label} | 0 | — | — | — | — |`);
      return;
    }
    lines.push(
      `| ${label} | ${hist.count} | ${seconds(mean(hist))} | ${seconds(quantile(hist.buckets, 0.5))} | ` +
        `${seconds(quantile(hist.buckets, 0.95))} | ${seconds(quantile(hist.buckets, 0.99))} |`,
    );
  };

  lines.push(`### ${title}`);
  lines.push('');
  lines.push('| Metric | n | mean | p50 | p95 | p99 |');
  lines.push('| --- | ---: | ---: | ---: | ---: | ---: |');
  row('first segment (FTTT)', histogram(parsed, 'astraeus_first_segment_seconds'));
  row('transcode startup', histogram(parsed, 'astraeus_transcode_startup_seconds'));
  row('HTTP request', histogram(parsed, 'astraeus_http_request_seconds'));
  row('metadata lookup', histogram(parsed, 'astraeus_metadata_lookup_seconds'));
  row('scan', histogram(parsed, 'astraeus_scan_seconds'));
  return lines.join('\n');
}
