// Quality verification: proves that the player's quality menu expresses a
// *ceiling on a ladder* rather than a single pinned rendition, which is the
// whole point of the preferred_height request field.
//
// What only a browser can show is the round trip a viewer actually causes: the
// menu is labelled as a cap, choosing a setting sends `preferred_height` (and
// not `max_height`), the server answers with several rungs topped at that
// height, and playback carries on. What each rung *is* is asserted by the Go
// integration test, which measures the produced segments; this harness checks
// the request and the decision the browser received.
//
// It needs an entity the browser must transcode — the quality menu is withheld
// for direct play, where nothing is re-encoded. The bundled demo's HEVC film is
// one; so is any film whose codec the browser profile cannot decode.
//
// Usage: node quality-verify.mjs <baseUrl> <transcodingEntityId>
import { CDP } from "./cdp.mjs";

const [baseUrl, entityId] = process.argv.slice(2);
if (!baseUrl || !entityId) {
  console.error("usage: node quality-verify.mjs <baseUrl> <transcodingEntityId>");
  process.exit(2);
}
const PORT = process.env.CDP_PORT || "9333";
const TIMEOUT_S = Number(process.env.QUALITY_TIMEOUT_S || 120);

const results = [];
const consoleErrors = [];
const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

function record(name, ok, detail) {
  results.push({ name, ok, detail: detail || "" });
  console.log((ok ? "PASS  " : "FAIL  ") + name + (detail ? "  -- " + detail : ""));
}
function note(text) {
  console.log("      " + text);
}

async function pageSocketUrl() {
  for (let attempt = 0; attempt < 40; attempt++) {
    try {
      const response = await fetch("http://127.0.0.1:" + PORT + "/json/list");
      const page = (await response.json()).find((t) => t.type === "page" && t.webSocketDebuggerUrl);
      if (page) return page.webSocketDebuggerUrl;
    } catch (_) {}
    await sleep(250);
  }
  throw new Error("no debuggable page on port " + PORT);
}

const SNAPSHOT = `(() => {
  const video = document.querySelector("video");
  const select = document.getElementById("player-quality");
  return {
    hasVideo: !!video,
    readyState: video ? video.readyState : 0,
    currentTime: video ? video.currentTime : 0,
    paused: video ? video.paused : true,
    qualityFound: !!select,
    qualityValue: select ? select.value : null,
    qualityDisabled: select ? select.disabled : true,
    qualityOptions: select
      ? Array.from(select.options).map((o) => ({ value: o.value, label: o.textContent.trim() }))
      : [],
  };
})()`;

// The playback request and its response are where the choice has to travel, so
// the harness wraps fetch before any page script runs and records them.
const CAPTURE_FETCH = `(() => {
  const original = window.fetch;
  window.__qualityVerify = { requests: [], responses: [] };
  window.fetch = function (input, init) {
    const url = typeof input === "string" ? input : (input && input.url) || "";
    const isPlayback = url.indexOf("/playback") !== -1;
    if (isPlayback && init && typeof init.body === "string") {
      try { window.__qualityVerify.requests.push(JSON.parse(init.body)); }
      catch (_) { window.__qualityVerify.requests.push(null); }
    }
    const pending = original.apply(this, arguments);
    if (isPlayback) {
      pending.then(function (response) {
        response.clone().json().then(function (body) {
          window.__qualityVerify.responses.push(body);
        }).catch(function () { window.__qualityVerify.responses.push(null); });
      }).catch(function () {});
    }
    return pending;
  };
})();`;

async function main() {
  const cdp = new CDP(await pageSocketUrl());
  await cdp.connect();
  await cdp.send("Runtime.enable");
  await cdp.send("Page.enable");
  cdp.on("Runtime.consoleAPICalled", (params) => {
    if (params.type === "error") {
      consoleErrors.push((params.args || []).map((a) => a.value || a.description || "?").join(" "));
    }
  });
  cdp.on("Runtime.exceptionThrown", (params) => {
    consoleErrors.push(
      params.exceptionDetails && params.exceptionDetails.text ? params.exceptionDetails.text : "exception"
    );
  });
  await cdp.send("Page.addScriptToEvaluateOnNewDocument", { source: CAPTURE_FETCH });

  await cdp.send("Page.navigate", { url: baseUrl + "/#/entity/" + encodeURIComponent(entityId) });
  await sleep(1000);
  await cdp.send("Page.reload", { ignoreCache: true });
  await sleep(1500);

  for (let attempt = 0; attempt < 60; attempt++) {
    if ((await cdp.eval(SNAPSHOT)).hasVideo) break;
    await sleep(250);
  }

  const hero = '[data-action="play"]';
  if (!(await cdp.eval("!!document.querySelector(" + JSON.stringify(hero) + ")"))) {
    record("the hero offers Play", false, "no " + hero + " on the page");
    return finish();
  }
  await cdp.click(hero);

  const deadline = Date.now() + TIMEOUT_S * 1000;
  let started = false;
  while (Date.now() < deadline) {
    const state = await cdp.eval(SNAPSHOT);
    if (state.readyState >= 2 && state.currentTime > 0.2) {
      started = true;
      break;
    }
    await sleep(1000);
  }
  if (!started) {
    record("playback started", false, "no frame within " + TIMEOUT_S + "s");
    return finish();
  }
  record("playback started", true);

  const before = await cdp.eval(SNAPSHOT);
  record(
    "the quality menu exists, so the session is being re-encoded",
    before.qualityFound,
    before.qualityFound ? "" : "no #player-quality; this entity direct-plays, so there is nothing to cap"
  );
  if (!before.qualityFound) return finish();

  const labelled = before.qualityOptions.filter((o) => o.value !== "auto").map((o) => o.label);
  record(
    "every height is offered as a cap rather than a fixed quality",
    labelled.length > 0 && labelled.every((label) => /^Up to \d+p$/.test(label)),
    "labels=" + JSON.stringify(before.qualityOptions.map((o) => o.label))
  );

  const target = before.qualityOptions.find((o) => o.value && o.value !== before.qualityValue);
  if (!target) {
    record("the menu offers a height to switch to", false, "only " + JSON.stringify(before.qualityOptions));
    return finish();
  }
  note("switching quality to " + JSON.stringify(target.label) + " (value " + target.value + ")");

  const captured = await cdp.eval("window.__qualityVerify || null");
  const requestsBefore = (captured && captured.requests ? captured.requests : []).length;

  const switched = await cdp.eval(`(() => {
    const select = document.getElementById("player-quality");
    if (!select) return false;
    select.value = ${JSON.stringify(target.value)};
    select.dispatchEvent(new Event("change", { bubbles: true }));
    return true;
  })()`);
  if (!switched) {
    record("the quality control can be driven", false, "no #player-quality");
    return finish();
  }

  let after = null;
  for (let attempt = 0; attempt < 60; attempt++) {
    await sleep(500);
    const now = await cdp.eval("window.__qualityVerify || null");
    if (now && (now.requests || []).length > requestsBefore) {
      after = now;
      break;
    }
  }
  if (!after) {
    record("choosing a quality re-negotiated the session", false, "no playback request within 30s");
    return finish();
  }
  record("choosing a quality re-negotiated the session", true);

  const request = after.requests[after.requests.length - 1];
  const response = after.responses[after.responses.length - 1];
  record(
    "the request asks for a ladder top, not a pinned rendition",
    !!request && Number(request.preferred_height) === Number(target.value) && !("max_height" in request),
    JSON.stringify({ preferred_height: request && request.preferred_height, max_height: request && request.max_height })
  );

  const renditions = response && response.decision ? response.decision.renditions : null;
  const top = Array.isArray(renditions) && renditions.length ? Number(renditions[0].height) : 0;
  record(
    "the decision came back as a ladder topped at the chosen height",
    Array.isArray(renditions) && renditions.length >= 2 && top === Number(target.value),
    "renditions=" + JSON.stringify(renditions)
  );

  const resumedFrom = (await cdp.eval(SNAPSHOT)).currentTime;
  await sleep(4000);
  const later = await cdp.eval(SNAPSHOT);
  record(
    "playback continues after the switch",
    later.currentTime > resumedFrom,
    "time " + resumedFrom.toFixed(2) + " -> " + later.currentTime.toFixed(2)
  );

  record("no console errors during the run", consoleErrors.length === 0, consoleErrors.join(" | "));
  finish();
}

function finish() {
  const failed = results.filter((result) => !result.ok);
  console.log("\n" + (results.length - failed.length) + "/" + results.length + " checks passed");
  process.exit(failed.length ? 1 : 0);
}

main().catch((error) => {
  console.error("harness error: " + error.message);
  process.exit(3);
});
