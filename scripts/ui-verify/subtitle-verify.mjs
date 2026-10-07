// Subtitle verification: asserts the player actually renders and switches the
// WebVTT tracks the server advertises.
//
// Usage: node subtitle-verify.mjs <baseUrl> <captionedEpisodeId> <movieId>
import { CDP } from "./cdp.mjs";

const [baseUrl, episodeId, movieId] = process.argv.slice(2);
if (!baseUrl || !episodeId || !movieId) {
  console.error("usage: node subtitle-verify.mjs <baseUrl> <episodeId> <movieId>");
  process.exit(2);
}

const PORT = process.env.CDP_PORT || "9333";
const results = [];
const consoleErrors = [];
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

function record(name, ok, detail) {
  results.push({ name, ok, detail: detail || "" });
  console.log((ok ? "PASS  " : "FAIL  ") + name + (detail ? "  -- " + detail : ""));
}

async function pageSocketUrl() {
  for (let i = 0; i < 40; i++) {
    try {
      const res = await fetch("http://127.0.0.1:" + PORT + "/json/list");
      const targets = await res.json();
      const page = targets.find((t) => t.type === "page" && t.webSocketDebuggerUrl);
      if (page) return page.webSocketDebuggerUrl;
    } catch (_) {}
    await sleep(250);
  }
  throw new Error("no debuggable page on port " + PORT);
}

// subtitleState summarises the track elements, the TextTrack list and the
// selector, which together are what a viewer actually experiences.
const SUBTITLE_STATE = `(() => {
  const v = document.querySelector("video");
  const tracks = v ? Array.from(v.querySelectorAll("track")) : [];
  const textTracks = v && v.textTracks ? Array.from(v.textTracks) : [];
  const radios = Array.from(document.querySelectorAll('[data-action="select-subtitle"]'));
  return {
    hasVideo: !!v,
    trackElements: tracks.length,
    trackSrcs: tracks.map(t => t.getAttribute("src") || ""),
    textTracks: textTracks.length,
    modes: textTracks.map(t => t.mode),
    cueCounts: textTracks.map(t => (t.cues ? t.cues.length : -1)),
    labels: textTracks.map(t => t.label),
    radios: radios.length,
    radioValues: radios.map(r => r.value),
    radioDisabled: radios.map(r => r.disabled),
    checked: (radios.find(r => r.checked) || {}).value || null
  };
})()`;

async function main() {
  const cdp = new CDP(await pageSocketUrl());
  await cdp.connect();
  await cdp.send("Runtime.enable");
  await cdp.send("Page.enable");
  cdp.on("Runtime.consoleAPICalled", (p) => {
    if (p.type === "error") {
      consoleErrors.push((p.args || []).map((a) => a.value || a.description || "").join(" "));
    }
  });
  cdp.on("Runtime.exceptionThrown", (p) => {
    consoleErrors.push("exception: " + JSON.stringify(p.exceptionDetails && p.exceptionDetails.text));
  });

  /* ---- start playback on the captioned episode ---- */
  await cdp.send("Page.navigate", { url: baseUrl + "/#/entity/" + episodeId });
  await sleep(2500);
  await cdp.click('[data-action="play"]');
  await sleep(2500);

  const s = await cdp.eval(SUBTITLE_STATE);
  record("a <track> element was attached", s.trackElements >= 1,
    "track elements=" + s.trackElements + " srcs=" + JSON.stringify(s.trackSrcs));
  record("track src points at the subtitle endpoint",
    s.trackSrcs.length > 0 && s.trackSrcs.every((u) => u.indexOf("/subtitles/") !== -1));

  // This is the risk flagged during implementation: if HTMLTrackElement.track
  // were null right after append, no TextTrack would exist and nothing would
  // ever display.
  record("track elements produced TextTracks", s.textTracks === s.trackElements,
    "elements=" + s.trackElements + " textTracks=" + s.textTracks + " labels=" + JSON.stringify(s.labels));

  record("a subtitle selector is rendered with Off plus each track",
    s.radios === s.trackElements + 1 && s.radioValues.indexOf("off") !== -1,
    "radios=" + s.radios + " values=" + JSON.stringify(s.radioValues));

  /* ---- the selected track must actually be showing, with cues loaded ---- */
  await cdp.eval("(() => { const v = document.querySelector('video'); v.currentTime = 1.0; return true; })()");
  await sleep(2000);

  const shown = await cdp.eval(SUBTITLE_STATE);
  const showingIndex = shown.modes.indexOf("showing");
  record("exactly one track is showing (or none, if the server marked no default)",
    shown.modes.filter((m) => m === "showing").length <= 1,
    "modes=" + JSON.stringify(shown.modes) + " checked=" + shown.checked);

  if (showingIndex !== -1) {
    record("the showing track loaded cues from the WebVTT",
      shown.cueCounts[showingIndex] > 0,
      "cues=" + shown.cueCounts[showingIndex] + " label=" + JSON.stringify(shown.labels[showingIndex]));
  } else {
    // No default was advertised: switch one on deliberately and re-check.
    const key = shown.radioValues.find((v) => v !== "off");
    await cdp.eval(
      "(() => { const r = document.querySelector('[data-action=\"select-subtitle\"][value=\"' + " +
        JSON.stringify(key) + " + '\"]'); if (r) r.click(); return !!r; })()"
    );
    await sleep(2500);
    const picked = await cdp.eval(SUBTITLE_STATE);
    const idx = picked.modes.indexOf("showing");
    record("selecting a track switches it on", idx !== -1, "modes=" + JSON.stringify(picked.modes));
    if (idx !== -1) {
      record("the selected track loaded cues", picked.cueCounts[idx] > 0,
        "cues=" + picked.cueCounts[idx]);
    }
  }

  /* ---- turning subtitles off must disable every track ---- */
  await cdp.eval(
    "(() => { const r = document.querySelector('[data-action=\"select-subtitle\"][value=\"off\"]');" +
      " if (r) r.click(); return !!r; })()"
  );
  await sleep(800);
  const off = await cdp.eval(SUBTITLE_STATE);
  record("selecting Off disables every track",
    off.modes.every((m) => m !== "showing"),
    "modes=" + JSON.stringify(off.modes) + " checked=" + off.checked);

  /* ---- leaving the entity must clear the tracks ---- */
  await cdp.send("Page.navigate", { url: baseUrl + "/#/entity/" + movieId });
  await sleep(2500);
  const after = await cdp.eval(SUBTITLE_STATE);
  record("navigating away removes the subtitle tracks",
    after.trackElements === 0 && after.textTracks === 0,
    "elements=" + after.trackElements + " textTracks=" + after.textTracks);

  record("no console errors during the subtitle run", consoleErrors.length === 0,
    consoleErrors.length ? consoleErrors.slice(0, 3).join(" | ") : "clean");

  const failed = results.filter((r) => !r.ok);
  console.log("\n" + (results.length - failed.length) + "/" + results.length + " checks passed");
  process.exit(failed.length ? 1 : 0);
}

main().catch((err) => {
  console.error("harness error: " + err.message);
  process.exit(3);
});
