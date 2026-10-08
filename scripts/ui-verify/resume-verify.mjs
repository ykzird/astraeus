// Resume verification: proves the two halves of resumable playback against the
// real UI.
//
// The first half is that a stored position is *resumed*: the harness writes one
// through the API, plays the entity the way a person does, and reads the
// transport's clock — which reports source time, so a resumed session starts
// near the stored position rather than at zero. A test that only checked the
// decision payload would pass even if the player ignored it.
//
// The second half is that watching is *reported*: the viewer pauses somewhere,
// and the API is asked what it now has. Nothing here trusts the UI's own state;
// every claim is read back from the server.
//
// Usage: node resume-verify.mjs <baseUrl> <entityId> [resumeSeconds]
import { CDP } from "./cdp.mjs";

const [baseUrl, entityId, resumeArg] = process.argv.slice(2);
if (!baseUrl || !entityId) {
  console.error("usage: node resume-verify.mjs <baseUrl> <entityId> [resumeSeconds]");
  process.exit(2);
}
const RESUME_SECONDS = Number(resumeArg || 300);
const PORT = process.env.CDP_PORT || "9333";
const TIMEOUT_S = Number(process.env.RESUME_TIMEOUT_S || 180);

const results = [];
const consoleErrors = [];
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

function record(name, ok, detail) {
  results.push({ name, ok, detail: detail || "" });
  console.log((ok ? "PASS  " : "FAIL  ") + name + (detail ? "  -- " + detail : ""));
}
function note(text) {
  console.log("      " + text);
}

async function pageSocketUrl() {
  for (let i = 0; i < 40; i++) {
    try {
      const res = await fetch("http://127.0.0.1:" + PORT + "/json/list");
      const page = (await res.json()).find((t) => t.type === "page" && t.webSocketDebuggerUrl);
      if (page) return page.webSocketDebuggerUrl;
    } catch (_) {}
    await sleep(250);
  }
  throw new Error("no debuggable page on port " + PORT);
}

const SNAPSHOT = `(() => {
  const v = document.querySelector("video");
  const clock = document.getElementById("player-time");
  const status = document.querySelector(".action-status") || document.querySelector("[role=status]");
  return {
    hasVideo: !!v,
    currentTime: v ? v.currentTime : 0,
    paused: v ? v.paused : true,
    readyState: v ? v.readyState : 0,
    clock: clock ? clock.textContent.trim() : null,
    status: status ? status.textContent.trim() : null,
  };
})()`;

// The transport reads "current / total", so only the first half is the position
// the viewer is at. Parsing the whole string yields NaN on the total's colon and
// silently turns every assertion into a failure.
function parseClock(text) {
  if (!text) return null;
  const first = String(text).trim().split("/")[0].trim();
  const parts = first.split(":").map((p) => Number(p.trim()));
  if (!parts.length || parts.some((p) => !isFinite(p))) return null;
  return parts.reduce((total, part) => total * 60 + part, 0);
}

async function progress() {
  const res = await fetch(baseUrl + "/api/entities/" + encodeURIComponent(entityId));
  if (!res.ok) return undefined;
  const body = await res.json();
  return body.progress || null;
}

async function putProgress(position, duration) {
  const res = await fetch(baseUrl + "/api/entities/" + encodeURIComponent(entityId) + "/progress", {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ position_seconds: position, duration_seconds: duration }),
  });
  return res.status;
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
    consoleErrors.push(params.exceptionDetails && params.exceptionDetails.text ? params.exceptionDetails.text : "exception");
  });

  // The duration the player will report; a resume needs a real one to be
  // meaningful, so take it from the source rather than inventing it.
  const probe = await fetch(baseUrl + "/api/entities/" + encodeURIComponent(entityId) + "/playback", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({
      containers: ["mp4", "webm", "hls"],
      video_codecs: ["h264", "vp9", "av1"],
      audio_codecs: ["aac", "opus", "mp3", "vorbis"],
      max_width: 1920,
      max_bitrate_kbps: 120000,
      max_bit_depth: 8,
      max_audio_channels: 2,
      supports_hdr: false,
      supports_hls: true,
      subtitles: true,
    }),
  });
  const probeBody = await probe.json().catch(() => null);
  if (probeBody && probeBody.session_id) {
    await fetch(baseUrl + "/api/streams/" + encodeURIComponent(probeBody.session_id), { method: "DELETE" }).catch(() => {});
  }
  const duration = Number(probeBody && probeBody.media_info && probeBody.media_info.duration_seconds) || 0;
  if (!(duration > RESUME_SECONDS + 60)) {
    console.error("the entity is too short to test a resume at " + RESUME_SECONDS + "s (duration " + duration + "s)");
    process.exit(2);
  }

  // Start from a known state, then store a position worth resuming.
  await fetch(baseUrl + "/api/entities/" + encodeURIComponent(entityId) + "/progress", { method: "DELETE" });
  const written = await putProgress(RESUME_SECONDS, duration);
  record("the API accepted a position to resume from", written === 204, "PUT -> " + written);
  const stored = await progress();
  record("the stored position is what a resume will use",
    stored !== null && Math.abs(Number(stored.position_seconds) - RESUME_SECONDS) < 1,
    "stored " + JSON.stringify(stored));

  // Open the entity and press Play the way a person does. The reload matters:
  // navigating to the URL the page is already on can leave the app's state - and
  // a paused session - in place, and then the hero's Play would toggle pause
  // instead of starting, so the run would prove nothing.
  await cdp.send("Page.navigate", { url: baseUrl + "/#/entity/" + encodeURIComponent(entityId) });
  await sleep(1000);
  await cdp.send("Page.reload", { ignoreCache: true });
  await sleep(1500);
  for (let i = 0; i < 60; i++) {
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

  // Wait for the clock to move at all, then read where it started.
  let started = null;
  const deadline = Date.now() + TIMEOUT_S * 1000;
  while (Date.now() < deadline) {
    const state = await cdp.eval(SNAPSHOT);
    if (state.readyState >= 2 && Number(state.currentTime) > 0.2) {
      started = state;
      break;
    }
    await sleep(1500);
  }
  if (!started) {
    record("playback started", false, "no frame within " + TIMEOUT_S + "s");
    return;
  }
  const startedAt = parseClock(started.clock);
  record("playback started", true, "clock=" + started.clock);
  record("it resumed at the stored position rather than zero",
    startedAt !== null && startedAt >= RESUME_SECONDS - 20 && startedAt <= RESUME_SECONDS + 120,
    "clock=" + started.clock + " stored=" + RESUME_SECONDS + "s");

  // Watch a little, pause, and ask the server what it was told. Pausing is a
  // report boundary, so this does not depend on the periodic timer firing.
  await sleep(4000);
  const before = await cdp.eval(SNAPSHOT);
  await cdp.eval(`(() => { const v = document.querySelector("video"); if (v) v.pause(); return true; })()`);
  await sleep(2500);

  const reported = await progress();
  const watchedTo = parseClock(before.clock);
  record("pausing reported the position to the server",
    reported !== null && watchedTo !== null && Math.abs(Number(reported.position_seconds) - watchedTo) < 30,
    "watched to " + before.clock + ", server has " + JSON.stringify(reported));

  // Starting over must clear it: the next Play should not offer the old place.
  await cdp.eval(`(() => { const b = document.querySelector('[data-action="restart"]'); if (b) { b.click(); return true; } return false; })()`);
  await sleep(3000);
  const afterRestart = await progress();
  record("starting over cleared the stored position",
    afterRestart === null || Number(afterRestart.position_seconds) < 5,
    "server has " + JSON.stringify(afterRestart));

  record("no console errors during the run", consoleErrors.length === 0,
    consoleErrors.length ? consoleErrors.join(" | ") : "clean");

  const failed = results.filter((r) => !r.ok);
  console.log("\n" + (results.length - failed.length) + "/" + results.length + " checks passed");
  if (failed.length) process.exit(1);
}

main().catch((error) => {
  console.error(error);
  process.exit(1);
});
