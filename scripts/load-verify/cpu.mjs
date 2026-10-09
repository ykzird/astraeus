// Process CPU sampling from /proc, so a load run can say how much work the
// server and the ffmpeg children it forked actually did.
//
// ps reports a lifetime average, which hides a short transcode inside a long
// server lifetime. These numbers are instantaneous: they compare CPU ticks
// between two samples, so a burst is visible as a burst.

import { readFileSync, readdirSync } from 'node:fs';
import { execSync } from 'node:child_process';

const CLK_TCK = Number(execSync('getconf CLK_TCK').toString().trim()) || 100;

// listProcesses returns every visible process as {pid, ppid, comm, ticks}.
export function listProcesses() {
  const out = [];
  for (const entry of readdirSync('/proc')) {
    if (!/^\d+$/.test(entry)) continue;
    let stat;
    try {
      stat = readFileSync(`/proc/${entry}/stat`, 'utf8');
    } catch {
      continue; // the process exited between readdir and read
    }
    const open = stat.indexOf('(');
    const close = stat.lastIndexOf(')');
    if (open === -1 || close === -1) continue;
    const comm = stat.slice(open + 1, close);
    const rest = stat.slice(close + 2).split(' ');
    out.push({
      pid: Number(entry),
      ppid: Number(rest[1]),
      comm,
      ticks: Number(rest[11]) + Number(rest[12]),
    });
  }
  return out;
}

// CpuSampler measures the CPU used by the server and by ffmpeg between two
// sample() calls. `comm` matches the process name exactly.
export class CpuSampler {
  constructor(comms = ['astraeus-server', 'ffmpeg']) {
    this.comms = new Set(comms);
    this.at = 0;
    this.previous = new Map();
    this.processCount = 0;
  }

  // sample returns the CPU cores used since the previous sample (100% = one
  // core saturated). The first call primes the counters and returns null.
  //
  // Per-process deltas are summed rather than differencing a total, because
  // ffmpeg children appear and exit during a session: a total would fall when
  // one exited and report negative CPU. A process seen for the first time
  // contributes nothing to this interval — its earlier work belongs to no
  // interval this sampler observed.
  sample() {
    const now = Date.now();
    const processes = listProcesses().filter((p) => this.comms.has(p.comm));
    this.processCount = processes.length;

    const current = new Map();
    let dticks = 0;
    for (const p of processes) {
      current.set(p.pid, p.ticks);
      const before = this.previous.get(p.pid);
      if (before !== undefined) dticks += Math.max(0, p.ticks - before);
    }
    this.previous = current;

    if (this.at === 0) {
      this.at = now;
      return null;
    }

    const dt = (now - this.at) / 1000;
    this.at = now;
    if (dt <= 0) return null;
    return dticks / CLK_TCK / dt;
  }

  // peak returns the highest reading seen, for a report that wants the burst
  // rather than the mean.
  static tracker() {
    return { samples: [], add(v) { if (v !== null) this.samples.push(v); } };
  }
}

// SystemCpuSampler measures the whole machine, not just this server's processes.
// /proc/stat is not namespaced, so this includes everything else running — which
// is the point: a transcode that is slow because the host is already busy looks
// identical to one that is slow on its own merit unless both are known.
export class SystemCpuSampler {
  constructor(cpus = 1) {
    this.cpus = cpus;
    this.previous = null;
    this.at = 0;
  }

  // sample returns { utilization, busyCores } since the previous call, where
  // utilization is a fraction of the whole machine (1.0 = every core busy).
  // The first call primes the counters and returns null.
  sample() {
    const now = Date.now();
    const line = readFileSync('/proc/stat', 'utf8').split('\n')[0];
    const fields = line.trim().split(/\s+/).slice(1).map(Number);
    const total = fields.reduce((a, b) => a + b, 0);
    // idle is field 4, iowait field 5: time waiting on disk is not CPU work,
    // but counting it as idle would report a busy machine as free.
    const idle = fields[3] + (fields[4] ?? 0);

    if (this.previous === null || this.at === 0) {
      this.previous = { total, idle };
      this.at = now;
      return null;
    }

    const dTotal = total - this.previous.total;
    const dIdle = idle - this.previous.idle;
    const dt = (now - this.at) / 1000;
    this.previous = { total, idle };
    this.at = now;
    if (dTotal <= 0 || dt <= 0) return null;

    return {
      utilization: (dTotal - dIdle) / dTotal,
      busyCores: (dTotal - dIdle) / CLK_TCK / dt,
    };
  }
}
