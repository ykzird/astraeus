// OCR verification: proves that an image subtitle the server has read into text
// reaches the viewer as a real, toggleable WebVTT track - the whole point of
// OCR, since a burn cannot be toggled, restyled or searched.
//
// It complements burn-verify.mjs. That harness needs a server with no OCR
// engine (the server exposes no URL for image tracks, so the menu offers a
// burn); this one needs a server with one (the image track carries a URL, so the
// menu offers an ordinary track).
//
// What only a browser can show is that the recognised words arrive as cue text:
// the menu has no "(burned in)" suffix, choosing the track attaches a <track>
// with no re-negotiation at all, the browser loads cues from the served WebVTT,
// and the caption the fixture drew is the caption on screen. Whether the OCR
// itself is accurate is the Go integration test's job.
//
// It needs an entity with a PGS track carrying real text. See README.md for the
// fixture recipe, which uses `scripts/pgsgen -text`.
//
// Usage: node ocr-verify.mjs <baseUrl> <entityId> [expectedCaption]
import { CDP } from "./cdp.mjs";

const [baseUrl, entityId, expectedArg] = process.argv.slice(2);
if (!baseUrl || !entityId) {
  console.error("usage: node ocr-verify.mjs <baseUrl> <entityId> [expectedCaption]");
  process.exit(2);
}
const EXPECTED = expectedArg || process.env.OCR_EXPECT || "ASTRAEUS MEDIA";
const PORT = process.env.CDP_PORT || "9333";
const TIMEOUT_S = Number(process.env.OCR_TIMEOUT_S || 120);

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

// SNAPSHOT reports the menu, the attached track elements and the caption the
// browser currently has on screen, which together are what a viewer sees.
const SNAPSHOT = `(() => {
  const video = document.querySelector("video");
  const radios = Array.from(document.querySelectorAll('[data-action="select-subtitle"]'));
  const elements = video ? Array.from(video.querySelectorAll("track")) : [];
  const textTracks = video && video.textTracks ? Array.from(video.textTracks) : [];
  const showing = textTracks.filter((t) => t.mode === "showing");
  const activeText = [];
  for (const track of showing) {
    if (!track.cues) continue;
    for (const cue of Array.from(track.cues)) {
      if (video.currentTime >= cue.startTime && video.currentTime < cue.endTime) {
        activeText.push(cue.text);
      }
    }
  }
  return {
    hasVideo: !!video,
    readyState: video ? video.readyState : 0,
    currentTime: video ? video.currentTime : 0,
    trackElements: elements.length,
    trackSrcs: elements.map((t) => t.getAttribute("src") || ""),
    loadedCues: textTracks.map((t) => (t.cues ? t.cues.length : -1)),
    modes: textTracks.map((t) => t.mode),
    cueTexts: textTracks.map((t) => (t.cues ? Array.from(t.cues).map((c) => c.text) : [])),
    activeText: activeText,
    subtitleOptions: radios.map((r) => ({
      key: r.dataset.subtitleKey,
      label: (r.closest("label") ? r.closest("label").textContent.trim() : ""),
      checked: r.checked,
    })),
  };
})()`;

// Playback requests are captured so the harness can prove that choosing an OCR'd
// track is an instant <track> switch and not a re-negotiation.
const CAPTURE_FETCH = `(() => {
  const original = window.fetch;
  window.__ocrVerify = { requests: [], responses: [] };
  window.fetch = function (input, init) {
    const url = typeof input === "string" ? input : (input && input.url) || "";
    const isPlayback = url.indexOf("/playback") !== -1;
    if (isPlayback && init && typeof init.body === "string") {
      try { window.__ocrVerify.requests.push(JSON.parse(init.body)); }
      catch (_) { window.__ocrVerify.requests.push(null); }
    }
    const pending = original.apply(this, arguments);
    if (isPlayback) {
      pending.then(function (response) {
        response.clone().json().then(function (body) {
          window.__ocrVerify.responses.push(body);
        }).catch(function () { window.__ocrVerify.responses.push(null); });
      }).catch(function () {});
    }
    return pending;
  };
})();`;

async function main() {
  // The capability comes first: without an OCR engine the entity is burn-only
  // and this harness has nothing to check, which must be said rather than
  // reported as a front-end failure.
  const capabilities = await (await fetch(baseUrl + "/api/system/capabilities")).json();
  record(
    "the server reports an OCR engine",
    capabilities.subtitle_ocr_enabled === true,
    "subtitle_ocr_enabled=" + capabilities.subtitle_ocr_enabled
  );
  if (capabilities.subtitle_ocr_enabled !== true) {
    note("start the server with --tesseract-bin pointing at an installed tesseract");
    console.log("\n1/1 checks passed");
    process.exit(1);
  }

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

  // The server's own track list says which tracks are images and which of them
  // it can now deliver as text.
  const captured = await cdp.eval("window.__ocrVerify || null");
  const playback = (captured && captured.responses ? captured.responses : []).find(
    (r) => r && Array.isArray(r.subtitles)
  );
  if (!playback) {
    record("the server reported its subtitle tracks", false, "no playback response captured");
    return finish();
  }
  const imageTracks = playback.subtitles.filter((track) => track.text === false);
  record(
    "the entity has an image subtitle track",
    imageTracks.length > 0,
    JSON.stringify(playback.subtitles.map((t) => ({ index: t.index, codec: t.codec, text: t.text, url: t.url })))
  );
  if (!imageTracks.length) return finish();

  const imageTrack = imageTracks.find((track) => typeof track.url === "string" && track.url.length > 0);
  record(
    "the image track is advertised as a downloadable text track",
    !!imageTrack,
    imageTrack ? imageTrack.url : "no image track carries a url"
  );
  if (!imageTrack) return finish();
  note("image track " + imageTrack.index + " (" + imageTrack.codec + ") -> " + imageTrack.url);

  const beforeSelect = await cdp.eval(SNAPSHOT);
  const option = beforeSelect.subtitleOptions.find((o) => String(o.key) === String(imageTrack.index));
  record(
    "the menu offers it as an ordinary track, not a burn",
    !!option && !/burned in/i.test(option.label),
    option ? JSON.stringify(option.label) : "no option for index " + imageTrack.index
  );
  if (!option) return finish();

  const requestsBefore = (captured.requests || []).length;
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
    return finish();
  }

  // A text track must switch on in place: no new playback request, because
  // there is nothing to re-encode.
  await sleep(1500);
  const afterSelect = await cdp.eval(SNAPSHOT);
  const later = await cdp.eval("window.__ocrVerify || null");
  const newRequests = (later.requests || []).length - requestsBefore;
  record(
    "selecting it attaches the served track instead of re-negotiating",
    newRequests === 0 && afterSelect.trackSrcs.indexOf(imageTrack.url) !== -1,
    "new playback requests=" + newRequests + " srcs=" + JSON.stringify(afterSelect.trackSrcs)
  );

  const showingIndex = afterSelect.modes.indexOf("showing");
  record("exactly one subtitle track is showing", showingIndex !== -1 && afterSelect.modes.filter((m) => m === "showing").length === 1,
    "modes=" + JSON.stringify(afterSelect.modes) + " checked=" + JSON.stringify(afterSelect.subtitleOptions.find((o) => o.checked) || null));

  // Wait for the cues: the WebVTT is produced on first request, which runs
  // ffmpeg and the recogniser, so it can take a few seconds.
  let caption = "";
  let cueCount = -1;
  const cueDeadline = Date.now() + TIMEOUT_S * 1000;
  while (Date.now() < cueDeadline) {
    const state = await cdp.eval(SNAPSHOT);
    const index = state.modes.indexOf("showing");
    if (index !== -1) {
      cueCount = state.loadedCues[index];
      if (state.activeText && state.activeText.length) {
        caption = state.activeText.join(" ");
        break;
      }
    }
    await sleep(1000);
  }
  record("the browser loaded cues from the OCR output", cueCount > 0, "cues=" + cueCount);
  record(
    "the caption on screen is the text the fixture drew",
    caption.toUpperCase().indexOf(EXPECTED.toUpperCase()) !== -1,
    "active cue text " + JSON.stringify(caption) + ", expected to contain " + JSON.stringify(EXPECTED)
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
