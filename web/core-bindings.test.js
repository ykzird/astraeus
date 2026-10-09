// Guards the seam between core.js and app.js.
//
// core.js is the unit-tested part; app.js is the only thing that runs in a
// browser, and it is not covered by any test here. The seam between them is a
// destructuring assignment, and a name that core.js exports but app.js never
// binds is not a missing import in the usual sense - JavaScript lets the file
// load, and the failure happens later as a ReferenceError at the moment that code
// path runs.
//
// That is exactly what shipped in v0.19.0: `entityListIsCurrent` and
// `clearedEntityList` were added to core.js and called from app.js, but neither
// was added to the destructuring. The first one surfaced as "Scanning "movies"
// failed. entityListIsCurrent is not defined" - from a code path (a scan
// refreshing its own library) that the happy path never reaches, so a manual test
// of ordinary browsing would not have caught it either.
//
// This checks the binding, not the behaviour: every name core.js exposes and
// app.js references must appear on the left-hand side of a destructuring from
// window.AstraeusCore. A static check is the right tool here because a real load
// would need a DOM large enough to be a liability of its own, and would still
// only exercise the one code path the stub happens to reach.

import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";

const HERE = dirname(fileURLToPath(import.meta.url));
const read = (name) => readFileSync(join(HERE, name), "utf8");

/** The names core.js puts on window.AstraeusCore. */
function coreExportNames(source) {
  // The export object literal is the last `return {` before the module closes;
  // entries read `name: name,`.
  const tail = source.slice(source.lastIndexOf("return {"));
  const names = new Set();
  for (const match of tail.matchAll(/^\s{4}([A-Za-z_$][\w$]*)\s*:/gm)) {
    names.add(match[1]);
  }
  return names;
}

/** The names app.js binds from window.AstraeusCore, exported and local alike. */
function boundNames(source) {
  const bound = new Set();
  for (const block of source.matchAll(/const \{([^}]*)\} = window\.AstraeusCore;/gs)) {
    for (const part of block[1].split(",")) {
      const trimmed = part.trim();
      if (!trimmed) continue;
      const match = /^([A-Za-z_$][\w$]*)(?:\s*:\s*([A-Za-z_$][\w$]*))?$/.exec(trimmed);
      if (match) {
        bound.add(match[1]);
        bound.add(match[2] || match[1]);
      }
    }
  }
  return bound;
}

/** Source with comments removed, so prose about a name is not a reference. */
function code(source) {
  return source
    .replace(/\/\*[\s\S]*?\*\//g, " ")
    .replace(/(^|[^:])\/\/.*$/gm, "$1 ");
}

test("every core name app.js references is bound from the core", () => {
  const core = read("core.js");
  const app = code(read("app.js"));
  const bound = boundNames(app);

  const referencedButUnbound = [];
  for (const name of coreExportNames(core)) {
    if (bound.has(name)) continue;
    // Is it mentioned anywhere in the code at all? If not, app.js simply does not
    // use it, which is fine - core.js carries helpers app.js has no call for.
    if (!new RegExp(`\\b${name}\\b`).test(app)) continue;
    referencedButUnbound.push(name);
  }

  assert.deepEqual(
    referencedButUnbound,
    [],
    `app.js references these names from core.js but never binds them from ` +
      `window.AstraeusCore, so they resolve to nothing and throw a ReferenceError ` +
      `at the moment that code path runs:\n  ${referencedButUnbound.join("\n  ")}`
  );
});

test("the check itself would catch the bug it was written for", () => {
  // A guard that cannot fail is worse than none, so this drives the same logic
  // over a source pair shaped like the one that shipped broken.
  const core = "return {\n    entityListIsCurrent: entityListIsCurrent,\n    other: other,\n  };";
  const appBroken = code("if (!entityListIsCurrent(token)) return false;");
  const appFixed = code(
    "const { entityListIsCurrent: entityListIsCurrent, } = window.AstraeusCore;\n" +
      "if (!entityListIsCurrent(token)) return false;"
  );

  const names = coreExportNames(core);
  assert.ok(names.has("entityListIsCurrent"), "the fixture must expose the name");

  const broken = [...names].filter(
    (n) => !boundNames(appBroken).has(n) && new RegExp(`\\b${n}\\b`).test(appBroken)
  );
  assert.deepEqual(broken, ["entityListIsCurrent"], "the unbound reference must be reported");

  const fixed = [...names].filter(
    (n) => !boundNames(appFixed).has(n) && new RegExp(`\\b${n}\\b`).test(appFixed)
  );
  assert.deepEqual(fixed, [], "a correctly bound reference must not be reported");
});
