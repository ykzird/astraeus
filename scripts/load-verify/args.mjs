// Small shared helpers for the command line, so each harness does not grow its
// own slightly different flag parser.

// parseArgs reads `--flag value`, `--flag` (boolean) and positional arguments.
// Positionals land in `_`.
export function parseArgs(argv) {
  const out = { _: [] };
  for (let i = 0; i < argv.length; i++) {
    const arg = argv[i];
    if (!arg.startsWith('--')) {
      out._.push(arg);
      continue;
    }
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

// number reads a numeric flag, falling back to a default.
export function number(value, fallback) {
  const parsed = Number(value);
  return Number.isFinite(parsed) ? parsed : fallback;
}

// list splits a comma-separated flag into numbers.
export function list(value, fallback) {
  if (typeof value !== 'string') return fallback;
  return value.split(',').map((part) => Number(part.trim())).filter(Number.isFinite);
}

export function sleep(ms) {
  return new Promise((resolve) => setTimeout(resolve, ms));
}
