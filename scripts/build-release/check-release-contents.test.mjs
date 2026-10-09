// Checks that the contents list in deploy/README.md matches what the build
// actually ships.
//
// DD-14 of the 2026-10-09 review: the list named the binary, web, deploy, the
// user-facing docs and three top-level files, and omitted six top-level documents
// and the assets directory that scripts/build-release.sh copies and asserts. A
// list of what a release contains is exactly the kind of prose that drifts,
// because nothing fails when it does - the build ships the right files and the
// documentation quietly describes a different archive.
//
// The authority is the build script's own assertion list. This reads it, reads
// the list in the runbook, and requires the runbook to account for every path.

import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";

const here = dirname(fileURLToPath(import.meta.url));
const repo = join(here, "..", "..");

const buildScript = readFileSync(join(repo, "scripts", "build-release.sh"), "utf8");
const runbook = readFileSync(join(repo, "deploy", "README.md"), "utf8");

/** The paths the build asserts are present in every archive. */
function assertedPaths() {
  // The loop reads:
  //   for f in astraeus-server web README.md ... \
  //     assets/astraeus.png; do
  // so the list is everything between "for f in" and the terminating "do",
  // with shell line continuations removed.
  const block = buildScript.match(/for f in([\s\S]*?);\s*do/);
  assert.ok(block, "the assertion loop in build-release.sh was not found");
  return block[1]
    .replace(/\\\n/g, " ")
    .split(/\s+/)
    .map((part) => part.trim())
    .filter(Boolean);
}

/** The contents-list section of the runbook, as text. */
function documentedNames() {
  // From the marker to the first paragraph that is neither blank nor a bullet,
  // which is where the list ends. Matching to the first blank line instead cut
  // the list off at its first bullet - the exact kind of too-clever parsing that
  // reports a correct document as wrong.
  const start = runbook.indexOf("**What a release contains.**");
  assert.ok(start >= 0, "the contents list in deploy/README.md was not found");

  const lines = runbook.slice(start).split("\n");
  const kept = [];
  let inList = false;
  for (const line of lines.slice(1)) {
    const trimmed = line.trim();
    // A bullet is "- " with the space; a wrapped continuation of one, or a line
    // of code in a fenced block that happens to start with a dash, is not.
    if (/^-\s+/.test(trimmed)) {
      inList = true;
      kept.push(trimmed.replace(/^-\s+/, ""));
      continue;
    }
    // Inside the list, a line that is not a bullet is a continuation of the
    // bullet above - Markdown wraps prose, and the first attempt at this broke
    // the list at the first wrapped line and reported half the archive as
    // undocumented. Blank lines separate bullets; a bullet-free paragraph after
    // a blank line ends the list.
    if (!inList) continue;
    if (trimmed === "") {
      if (kept.length && kept[kept.length - 1] !== "") kept.push("");
      continue;
    }
    if (kept.length && kept[kept.length - 1] === "") break;
    kept[kept.length - 1] += " " + trimmed;
  }
  assert.ok(kept.length > 0, "the contents list has no bullets");
  return kept.filter((bullet) => bullet !== "").join("\n");
}

test("every path the build asserts is accounted for by the contents list", () => {
  const list = documentedNames();
  const missing = [];

  for (const path of assertedPaths()) {
    // Directories are documented as a directory ("the `deploy` directory") and
    // the docs pages are listed individually in one sentence, so a mention of
    // either the whole path or its top-level name counts as accounted for.
    const base = path.split("/")[0];
    const mentioned =
      list.includes("`" + path + "`") ||
      list.includes("`" + base + "`") ||
      list.includes(base);
    if (!mentioned) missing.push(path);
  }

  assert.deepEqual(
    missing,
    [],
    "these paths are shipped but not named in the contents list:\n  " + missing.join("\n  ")
  );
});

test("the contents list does not name files the build does not ship", () => {
  const asserted = assertedPaths();
  const shipped = new Set(asserted.map((path) => path.split("/")[0]));
  // The docs pages are named individually and so by basename rather than by
  // path, and checksums.txt is written by the build rather than copied from the
  // tree. Both are legitimate names for something present in the archive, so
  // they are listed here rather than treated as mistakes.
  const byBasename = new Set(asserted.map((path) => path.split("/").pop()));
  const generated = new Set(["checksums.txt"]);

  const claimed = [...documentedNames().matchAll(/`([A-Za-z0-9_.-]+\.(?:md|png|txt))`/g)].map(
    (match) => match[1]
  );
  assert.ok(claimed.length > 0, "no filenames were found in the contents list");

  const wrong = claimed.filter(
    (name) => !shipped.has(name) && !byBasename.has(name) && !generated.has(name)
  );
  assert.deepEqual(
    wrong,
    [],
    "the contents list names files the build does not ship:\n  " + wrong.join("\n  ")
  );
});
