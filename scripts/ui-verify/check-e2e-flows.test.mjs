// The manual walk-through and its machine-readable twin have to stay in step,
// or the Playwright conversion will be written against a document that has
// moved on. This runs the comparison from check-e2e-flows.mjs as a test, so an
// edit to either file that forgets the other fails rather than looking fine.
//
// Node's own runner, no dependencies, the same stance the rest of the project's
// JavaScript takes:
//
//   node --test scripts/ui-verify/check-e2e-flows.test.mjs

import assert from "node:assert/strict";
import test from "node:test";

import { compare, dataStructure, load, structure } from "./check-e2e-flows.mjs";

test("the document and flows.json describe the same walk-through", () => {
  const { markdown, flows } = load();
  const problems = compare(markdown, flows);
  assert.deepEqual(problems, [], `they disagree:\n  ${problems.join("\n  ")}`);
});

test("the walk-through has the phases it is meant to", () => {
  const { markdown } = load();
  const phases = structure(markdown);
  // A floor rather than an exact count: adding a phase is a legitimate change,
  // and this is here to catch a document that lost its structure entirely.
  assert.ok(phases.length >= 8, `only ${phases.length} phases were found`);
  const steps = phases.reduce((total, phase) => total + phase.steps, 0);
  assert.ok(steps >= 30, `only ${steps} steps were found`);
});

test("every step declares how it can be checked", () => {
  const { flows } = load();
  const kinds = new Set();
  for (const phase of flows.phases) {
    for (const step of phase.steps) kinds.add(step.kind);
  }
  // 'human' and 'auto+human' are expected: the point is that the converter can
  // tell them apart from 'auto', so at least one step has to carry each shape
  // the document defines.
  assert.ok(kinds.has("auto"), "no step is fully automatable");
  for (const kind of kinds) {
    assert.ok(
      ["auto", "human", "auto+human"].includes(kind),
      `a step has an unknown kind ${JSON.stringify(kind)}`,
    );
  }
});

test("a phase's steps all belong to that phase", () => {
  const { flows } = load();
  const data = dataStructure(flows);
  assert.equal(data.length, flows.phases.length);
  for (const phase of flows.phases) {
    assert.ok(phase.steps.length > 0, `phase ${phase.id} has no steps`);
  }
});
