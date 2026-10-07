// Regenerates web/icons.js from the Iconify API.
//
//   node scripts/fetch-icons.mjs
//
// The registry is generated rather than hand-written so the exact set of icons,
// their source and their licence stay auditable. Nothing is fetched at runtime:
// the SVGs are baked into web/icons.js at build time, so the shipped UI never
// talks to a third-party origin.
//
// BoxIcons v2 (prefix "bx") by Boxicons — MIT. See web/vendor/icons.md and
// web/vendor/bx.LICENSE. The MIT notice must travel with the generated file.
import { createHash } from "node:crypto";
import { writeFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";

const PREFIX = "bx";
const SET_NAME = "BoxIcons v2";
const SET_AUTHOR = "Boxicons";
const SET_LICENCE = "MIT";
const SET_LICENCE_URL = "https://github.com/box-icons/boxicons/blob/main/LICENSE";
const SET_HOME = "https://icon-sets.iconify.design/bx/";

/* Registry key -> BoxIcons name, with what the UI uses it for. */
const WANTED = [
  ["play", "play", "hero Play button and the overlay play/pause toggle"],
  ["pause", "pause", "the overlay toggle while playback runs"],
  ["restart", "refresh", "replay from the beginning"],
  ["skip-forward", "fast-forward", "seek forward ten seconds"],
  ["stop", "stop", "stop playback and close the player"],
  ["fullscreen-enter", "fullscreen", "enter fullscreen"],
  ["fullscreen-exit", "exit-fullscreen", "leave fullscreen"],
  ["arrow-right", "chevron-right", "the decision-reason list marker"],
];

const root = join(dirname(fileURLToPath(import.meta.url)), "..");
const target = join(root, "web", "icons.js");

const names = WANTED.map(([, icon]) => icon);
const url = `https://api.iconify.design/${PREFIX}.json?icons=${names.join(",")}`;

const response = await fetch(url);
if (!response.ok) {
  console.error(`iconify returned ${response.status} for ${url}`);
  process.exit(1);
}
const payload = await response.json();

const missing = payload.not_found || [];
if (missing.length) {
  console.error(`these icons do not exist in ${PREFIX}: ${missing.join(", ")}`);
  process.exit(1);
}

// Each icon body is SVG markup; keep only the path data and any even-odd flags.
function pathsFrom(body) {
  const paths = [];
  const evenOdd = [];
  const pattern = /<path\b([^>]*)\/?>/g;
  let match;
  while ((match = pattern.exec(body)) !== null) {
    const attributes = match[1];
    const d = /\bd="([^"]+)"/.exec(attributes);
    if (!d) continue;
    paths.push(d[1]);
    evenOdd.push(/fill-rule="evenodd"/.test(attributes));
  }
  return { paths, evenOdd };
}

const entries = WANTED.map(([key, icon]) => {
  const body = payload.icons[icon].body;
  const { paths, evenOdd } = pathsFrom(body);
  if (!paths.length) throw new Error(`${icon} produced no path data`);
  const entry = { key, icon, paths, evenOdd };
  return entry;
});

function registryLiteral(entries) {
  return entries
    .map((entry) => {
      const d = entry.paths.map((p) => JSON.stringify(p)).join(", ");
      const anyEvenOdd = entry.evenOdd.some(Boolean);
      const eo = anyEvenOdd
        ? `, eo: [${entry.evenOdd.map((flag) => (flag ? 1 : 0)).join(", ")}]`
        : "";
      return `    ${JSON.stringify(entry.key)}: { d: [${d}]${eo} }`;
    })
    .join(",\n");
}

const iconTable = entries
  .map((entry) => `   ${entry.key.padEnd(17)} ${PREFIX}:${entry.icon}`)
  .join("\n");

const file = `/* ==========================================================================
   Astraeus Media — icon registry (GENERATED FILE; do not hand-edit)
   --------------------------------------------------------------------------
   ${SET_NAME} — ${SET_HOME}
   Copyright ${SET_AUTHOR}, licensed ${SET_LICENCE}.
   ${SET_LICENCE_URL}

   Regenerate with:  node scripts/fetch-icons.mjs
   Source:           ${url}

   Only the ${entries.length} icons this UI actually uses are stored, so the file stays a
   few kilobytes instead of shipping an entire set.

   Icons:

${iconTable}

   The ${SET_LICENCE} notice for this set must travel with this file; it is kept in
   web/vendor/bx.LICENSE and summarised in web/vendor/icons.md.
   ========================================================================== */

(function (global) {
  "use strict";

  var SVG_NS = "http://www.w3.org/2000/svg";
  var VIEW_BOX = "0 0 24 24";

  /* Frozen registry: each entry lists the <path> "d" attributes, plus an
     optional parallel list flagging paths that need fill-rule="evenodd". */
  var ICONS = Object.freeze({
${registryLiteral(entries)}
  });

  /* Builds an <svg> for a registry key. The icon is decorative by default, so
     that a button keeps its own accessible name; pass a label when the icon
     stands alone and has to carry the meaning itself. */
  function icon(name, options) {
    var opts = options || {};
    var svg = document.createElementNS(SVG_NS, "svg");
    svg.setAttribute("viewBox", VIEW_BOX);
    svg.setAttribute("class", opts.className ? "icon " + opts.className : "icon");
    svg.setAttribute("focusable", "false");
    if (opts.label) {
      svg.setAttribute("role", "img");
      var title = document.createElementNS(SVG_NS, "title");
      title.textContent = opts.label;
      svg.appendChild(title);
    } else {
      svg.setAttribute("aria-hidden", "true");
    }
    var spec = Object.prototype.hasOwnProperty.call(ICONS, name) ? ICONS[name] : null;
    if (!spec) return svg;
    for (var i = 0; i < spec.d.length; i += 1) {
      var path = document.createElementNS(SVG_NS, "path");
      path.setAttribute("d", spec.d[i]);
      path.setAttribute("fill", "currentColor");
      if (spec.eo && spec.eo[i]) {
        path.setAttribute("fill-rule", "evenodd");
        path.setAttribute("clip-rule", "evenodd");
      }
      svg.appendChild(path);
    }
    return svg;
  }

  global.AstraeusIcons = Object.freeze({
    icon: icon,
    names: Object.freeze(Object.keys(ICONS)),
  });
})(typeof window !== "undefined" ? window : this);
`;

writeFileSync(target, file);

const digest = createHash("sha256").update(file).digest("hex");
console.log(`wrote ${target}`);
console.log(`icons   : ${entries.length} (${entries.map((e) => e.key).join(", ")})`);
console.log(`bytes   : ${Buffer.byteLength(file)}`);
console.log(`sha256  : ${digest}`);
console.log(`source  : ${url}`);
