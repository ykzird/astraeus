// Burn-in verification: proves that an image-based subtitle track can be chosen
// in the player and that choosing it really asks the server to composite it.
//
// The server-side composite is asserted at the pixel level by the Go
// integration test (internal/streaming/burn_integration_test.go), because a
// browser cannot be asked what is baked into the picture. What only a browser
// can show is the other half: the menu offers the track, the choice travels as
// `burn_subtitle_index`, the decision comes back with `burned_subtitle_index`,
// the control reflects it, and playback keeps going.
//
// It needs an entity with a PGS or VobSub track. See README.md for a fixture
// recipe built from scripts/pgsgen.
//
// Usage: node burn-verify.mjs <baseUrl> <entityId>
import { CDP } from "./cdp.mjs";

const [baseUrl, entityId] = process.argv.slice(2);
if (!baseUrl || !entityId) {
  console.error("usage: node burn-verify.mjs <baseUrl> <entityId>");
  process.exit(2);
}
const PORT = process.env.CDP_PORT || "9333";
const TIMEOUT_S = Number(process.env.BURN_TIMEOUT_S || 120);

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
  const radios = Array.from(document.querySelectorAll('[data-action="select-subtitle"]'));
  return {
    hasVideo: !!video,
    readyState: video ? video.readyState : 0,
    currentTime: video ? video.currentTime : 0,
    paused: video ? video.paused : true,
    subtitleOptions: radios.map((r) => ({
      key: r.dataset.subtitleKey,
      label: (r.closest("label") ? r.closest("label").textContent.trim() : ""),
      checked: r.checked,
    })),
  };
})()`;

// The playback request and response are what the choice has to travel in, so the
// harness wraps fetch before any page script runs and records them. Reading the
// app's module state is not possible, and scraping the DOM alone could not tell
// a real re-negotiation from a repainted radio.
const CAPTURE_FETCH = `(() => {
  const original = window.fetch;
  window.__burnVerify = { requests: [], responses: [] };
  window.fetch = function (input, init) {
    const url = typeof input === "string" ? input : (input && input.url) || "";
    const isPlayback = url.indexOf("/playback") !== -1;
    if (isPlayback && init && typeof init.body === "string") {
      try { window.__burnVerify.requests.push(JSON.parse(init.body)); }
      catch (_) { window.__burnVerify.requests.push(null); }
    }
    const pending = original.apply(this, arguments);
    if (isPlayback) {
      pending.then(function (response) {
        response.clone().json().then(function (body) {
          window.__burnVerify.responses.push(body);
        }).catch(function () { window.__burnVerify.responses.push(null); });
      }).catch(function () {});
    }
    return pending;
  };
})();`;

// burnedIndexIn reads the decision's burn, treating an absent field as "none":
// the field is omitempty, so Number(undefined) would otherwise be NaN and a
// correct "no burn" answer would read as a failure.
function burnedIndexIn(response) {
  const raw = response && response.decision ? Number(response.decision.burned_subtitle_index) : 0;
  return Number.isFinite(raw) && raw > 0 ? raw : 0;
}

async function captured(cdp) {
  return cdp.eval("window.__burnVerify || null");
}

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
    const state = await cdp.eval(SNAPSHOT);
    if (state.hasVideo) break;
    await sleep(250);
  }

  const hero = '[data-action="play"]';
  if (!(await cdp.eval("!!document.querySelector(" + JSON.stringify(hero) + ")"))) {
    record("the hero offers Play", false, "no " + hero + " on the page");
    return;
  }
  await cdp.click(hero);

  // Playback has to be live before the controls can be switched.
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
    return;
  }
  record("playback started", true);

  // The first negotiation's track list is the server's own answer about which
  // tracks are images, so the harness does not guess from the markup.
  const before = await captured(cdp);
  const firstResponse = (before.responses || []).find((r) => r && Array.isArray(r.subtitles));
  if (!firstResponse) {
    record("the server reported its subtitle tracks", false, "no playback response captured");
    return;
  }
  const imageTracks = firstResponse.subtitles.filter((track) => track.text === false);
  record(
    "the entity has an image subtitle track",
    imageTracks.length > 0,
    JSON.stringify(firstResponse.subtitles.map((t) => ({ index: t.index, codec: t.codec, text: t.text })))
  );
  if (!imageTracks.length) return;
  const imageTrack = imageTracks[0];
  note("image track " + imageTrack.index + " (" + imageTrack.codec + ")");

  const optionsNow = (await cdp.eval(SNAPSHOT)).subtitleOptions;
  const burnOption = optionsNow.find((option) => String(option.key) === String(imageTrack.index));
  record(
    "the image track is offered, and says it will be burned in",
    !!burnOption && /burned in/i.test(burnOption.label),
    burnOption ? JSON.stringify(burnOption.label) : "no option for index " + imageTrack.index
  );
  if (!burnOption) return;

  // Choose it the way the menu does: check the radio and let the delegated
  // change handler run.
  const requestsBefore = (before.requests || []).length;
  const selected = await cdp.eval(`(() => {
    const radio = Array.from(document.querySelectorAll('[data-action="select-subtitle"]'))
      .find((r) => r.dataset.subtitleKey === ${JSON.stringify(String(imageTrack.index))});
    if (!radio) return false;
    radio.checked = true;
    radio.dispatchEvent(new Event("change", { bubbles: true }));
    return true;
  })()`);
  if (!selected) {
    record("the image track can be selected", false, "radio not found");
    return;
  }

  // Wait for the re-negotiation the switch triggers.
  let after = null;
  for (let attempt = 0; attempt < 60; attempt++) {
    await sleep(500);
    const now = await captured(cdp);
    if (now && (now.requests || []).length > requestsBefore) {
      after = now;
      break;
    }
  }
  if (!after) {
    record("selecting the track re-negotiated the session", false, "no new playback request within 30s");
    return;
  }
  record("selecting the track re-negotiated the session", true);

  const burnRequest = after.requests[after.requests.length - 1];
  record(
    "the request asks the server to burn that stream index",
    !!burnRequest && Number(burnRequest.burn_subtitle_index) === Number(imageTrack.index),
    JSON.stringify(burnRequest && burnRequest.burn_subtitle_index)
  );
  const burnResponse = after.responses[after.responses.length - 1];
  const burnedIndex = burnedIndexIn(burnResponse);
  record(
    "the decision reports the stream it is burning",
    burnedIndex === Number(imageTrack.index),
    "decision.burned_subtitle_index=" + burnedIndex
  );

  // The control must show the burn: it is not a <track>, so only the decision
  // can make the radio checked.
  await sleep(2000);
  const afterState = await cdp.eval(SNAPSHOT);
  const checked = afterState.subtitleOptions.find((option) => option.checked);
  record(
    "the menu shows the burned track as the current choice",
    !!checked && String(checked.key) === String(imageTrack.index),
    checked ? JSON.stringify(checked.key) : "nothing checked"
  );

  // And playback has to keep going: a burn restarts the transcode at the
  // viewer's position, which is the whole point of re-negotiating.
  const resumedFrom = afterState.currentTime;
  await sleep(4000);
  const later = await cdp.eval(SNAPSHOT);
  record(
    "playback continues after the burn starts",
    later.currentTime > resumedFrom,
    "time " + resumedFrom.toFixed(2) + " -> " + later.currentTime.toFixed(2)
  );

  // Turning it off is a switch in the other direction: no burn may be asked for,
  // and the decision must come back without one.
  const offRequests = (await captured(cdp)).requests.length;
  await cdp.eval(`(() => {
    const radio = Array.from(document.querySelectorAll('[data-action="select-subtitle"]'))
      .find((r) => r.dataset.subtitleKey === "off");
    if (!radio) return false;
    radio.checked = true;
    radio.dispatchEvent(new Event("change", { bubbles: true }));
    return true;
  })()`);

  let turnedOff = null;
  for (let attempt = 0; attempt < 60; attempt++) {
    await sleep(500);
    const now = await captured(cdp);
    if (now && now.requests.length > offRequests) {
      turnedOff = now;
      break;
    }
  }
  if (!turnedOff) {
    record("turning subtitles Off stops the burn", false, "no new playback request within 30s");
  } else {
    const offRequest = turnedOff.requests[turnedOff.requests.length - 1];
    const offResponse = turnedOff.responses[turnedOff.responses.length - 1];
    const stillBurning = burnedIndexIn(offResponse);
    record(
      "turning subtitles Off asks for no burn and gets none",
      !offRequest.burn_subtitle_index && stillBurning === 0,
      JSON.stringify({ sent: offRequest.burn_subtitle_index, got: stillBurning })
    );
  }

  record("no console errors during the run", consoleErrors.length === 0, consoleErrors.join(" | "));

  const failed = results.filter((result) => !result.ok);
  console.log("\n" + (results.length - failed.length) + "/" + results.length + " checks passed");
  if (failed.length) process.exit(1);
}

main().catch((error) => {
  console.error(error);
  process.exit(1);
});
