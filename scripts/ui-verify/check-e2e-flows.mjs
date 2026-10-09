#!/usr/bin/env node
// Keeps E2E-MANUAL.md and flows.json from drifting apart.
//
// The manual walk-through is prose and flows.json is the same walk-through as
// data. They describe the same steps, and nothing stops an edit to one from
// being forgotten in the other, so this checks the structure they share: the
// phases, in order, with the same number of steps each. A mismatch names the
// phase, because a step added or removed early shifts every later one.
//
// It deliberately does not try to parse the prose for each step's kind. The
// markers are written for a reader, not for a parser, and half of this file's
// own history was spent failing to read them reliably. The kind lives in
// flows.json, which is data, and this checks that the data is well formed: a
// step without a kind cannot be converted to an automated test, so it is an
// error rather than a silent omission.
//
//   node --test scripts/ui-verify/check-e2e-flows.test.mjs
//   node scripts/ui-verify/check-e2e-flows.mjs          # prints the report

import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));

/** The phases and their step counts, in document order. */
export function structure(markdown) {
  const phases = [];
  let current = null;
  for (const line of markdown.split("\n")) {
    const phase = line.match(/^##\s+(Phase\s+\d+)\s+—\s+(.*)$/);
    if (phase) {
      current = { id: phase[1], title: phase[2].trim(), steps: 0 };
      phases.push(current);
      continue;
    }
    if (/^#{1,6}\s/.test(line)) {
      current = null;
      continue;
    }
    if (current && /^-\s+\[[ x]\]/.test(line)) current.steps++;
  }
  return phases;
}

/** The phases and their step counts, from the data. */
export function dataStructure(flows) {
  return flows.phases.map((phase) => ({
    id: phase.id,
    title: phase.title,
    steps: phase.steps.length,
  }));
}

const KNOWN_KINDS = new Set(["auto", "human", "auto+human"]);

/**
 * Compare the two descriptions. Returns a list of human-readable problems; an
 * empty list means they agree.
 */
export function compare(markdown, flows) {
  const problems = [];
  const prose = structure(markdown);
  const data = dataStructure(flows);

  if (prose.length !== data.length) {
    problems.push(
      `the document describes ${prose.length} phases and the data ${data.length}`,
    );
  }

  const shared = Math.min(prose.length, data.length);
  for (let i = 0; i < shared; i++) {
    if (prose[i].steps !== data[i].steps) {
      problems.push(
        `phase ${i + 1} (${prose[i].title}) has ${prose[i].steps} step(s) in the document and ${data[i].steps} in the data`,
      );
    }
  }

  // A step without a kind cannot be converted to an automated test, so it is an
  // error rather than an omission that would be discovered later.
  for (const phase of flows.phases) {
    for (const [index, step] of phase.steps.entries()) {
      if (!step.kind) {
        problems.push(`phase ${phase.id} step ${index + 1} (${step.id}) declares no kind`);
      } else if (!KNOWN_KINDS.has(step.kind)) {
        problems.push(
          `phase ${phase.id} step ${index + 1} (${step.id}) has kind ${JSON.stringify(step.kind)}, which is not one of ${[...KNOWN_KINDS].join(", ")}`,
        );
      }
    }
  }

  // A step that needs media the demo library does not have must say how to get
  // it, or the converted suite will be scheduled against the demo library and
  // fail for a fixture reason that reads like a product bug. The requirement is
  // that the data says *which* fixture, not how many steps are marked in the
  // prose: counting markers made this check a source of false failures, because
  // a phase-level marker legitimately covers several steps.
  const fixtures = new Set(Object.keys(flows.fixtures ?? {}));
  for (const phase of flows.phases) {
    for (const [index, step] of phase.steps.entries()) {
      if (!step.needsRealMedia) continue;
      if (step.fixture || phase.needsRealMedia) continue;
      problems.push(
        `phase ${phase.id} step ${index + 1} (${step.id}) needs real media but names no fixture, and its phase does not either`,
      );
    }
  }
  for (const phase of flows.phases) {
    if (phase.fixture && !fixtures.has(phase.fixture)) {
      problems.push(`phase ${phase.id} names an unknown fixture ${JSON.stringify(phase.fixture)}`);
    }
    for (const step of phase.steps) {
      if (step.fixture && !fixtures.has(step.fixture)) {
        problems.push(`phase ${phase.id} step (${step.id}) names an unknown fixture ${JSON.stringify(step.fixture)}`);
      }
    }
  }

  return problems;
}

export function load(directory = here) {
  return {
    markdown: readFileSync(join(directory, "E2E-MANUAL.md"), "utf8"),
    flows: JSON.parse(readFileSync(join(directory, "flows.json"), "utf8")),
  };
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  const { markdown, flows } = load();
  const problems = compare(markdown, flows);
  const phases = structure(markdown);
  const steps = phases.reduce((total, phase) => total + phase.steps, 0);
  const kinds = flows.phases.flatMap((phase) => phase.steps.map((step) => step.kind));
  const auto = kinds.filter((k) => k === "auto" || k === "auto+human").length;
  console.log(`${phases.length} phases, ${steps} steps; ${auto} automatable, ${kinds.length - auto} human`);
  if (problems.length > 0) {
    console.error("\nthe document and the data disagree:");
    for (const problem of problems) console.error(`  - ${problem}`);
    process.exit(1);
  }
  console.log("the document and the data agree");
}
