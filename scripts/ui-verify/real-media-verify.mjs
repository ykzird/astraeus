// Real-media verification: plays one entity through the UI and polls until it
// actually starts, which a fixed sleep cannot do for a 4K transcode.
//
// Usage: node real-media-verify.mjs <baseUrl> <entityId> [timeoutSeconds]
import { CDP } from "./cdp.mjs";

const [baseUrl, entityId, timeoutArg] = process.argv.slice(2);
if (!baseUrl || !entityId) {
  console.error("usage: node real-media-verify.mjs <baseUrl> <entityId> [timeoutSeconds]");
  process.exit(2);
}
const TIMEOUT_S = Number(timeoutArg || 300);
const PORT = process.env.CDP_PORT || "9333";

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

const STATE = `(() => {
  const v = document.querySelector("video");
  const badge = document.querySelector(".mode-badge");
  const reasons = Array.from(document.querySelectorAll(".reasons li")).map(li => li.textContent);
  const subs = Array.from(document.querySelectorAll("[data-subtitle-key]")).map(r => r.value);
  const disabledSubs = Array.from(document.querySelectorAll("[data-subtitle-key]"))
    .filter(r => r.disabled).map(r => r.value);
  return {
    hasVideo: !!v,
    currentTime: v ? v.currentTime : 0,
    paused: v ? v.paused : true,
    readyState: v ? v.readyState : 0,
    duration: v ? v.duration : 0,
    src: v ? (v.currentSrc || v.src || "") : "",
    seekableEnd: v && v.seekable && v.seekable.length ? v.seekable.end(0) : 0,
    videoError: v && v.error ? v.error.code : null,
    mode: badge ? badge.textContent.trim() : null,
    reasons: reasons,
    subtitleOptions: subs,
    disabledSubtitleOptions: disabledSubs
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

  console.log("Opening the entity and waiting for the UI to render...");
  await cdp.send("Page.navigate", { url: baseUrl + "/#/entity/" + entityId });

  // Wait for the entity to load (the Play button only exists once it has).
  let ready = false;
  for (let i = 0; i < 40; i++) {
    ready = await cdp.eval(`!!document.querySelector('[data-action="play"]')`);
    if (ready) break;
    await sleep(500);
  }
  record("the entity loaded and offered a Play control", ready === true);

  const before = await cdp.eval(STATE);
  note("mode badge before play: " + JSON.stringify(before.mode));
  note("subtitle options: " + JSON.stringify(before.subtitleOptions) +
    " (disabled: " + JSON.stringify(before.disabledSubtitleOptions) + ")");

  console.log("\nPressing Play and polling for first frame (up to " + TIMEOUT_S + "s)...");
  const started = Date.now();
  await cdp.click('[data-action="play"]');

  let state = null;
  let firstFrameMs = null;
  let lastLogged = 0;
  while (Date.now() - started < TIMEOUT_S * 1000) {
    state = await cdp.eval(STATE);
    const elapsed = Date.now() - started;

    if (state.videoError !== null) break;
    if (state.currentTime > 0.15 && state.readyState >= 2) {
      firstFrameMs = elapsed;
      break;
    }
    if (elapsed - lastLogged > 5000) {
      lastLogged = elapsed;
      note(
        "t+" + Math.round(elapsed / 1000) + "s  readyState=" + state.readyState +
        " currentTime=" + state.currentTime.toFixed(3) +
        " seekable=" + state.seekableEnd.toFixed(1) +
        (state.src ? " src=" + state.src.slice(0, 32) : " (no src yet)")
      );
    }
    await sleep(1000);
  }

  if (state === null) {
    record("the player produced a state", false, "never evaluated");
  } else if (state.videoError !== null) {
    record("playback started", false, "media error code " + state.videoError);
  } else {
    record("playback started on real media", firstFrameMs !== null,
      firstFrameMs !== null
        ? "first frame after " + (firstFrameMs / 1000).toFixed(1) + "s"
        : "no frame within " + TIMEOUT_S + "s; readyState=" + state.readyState);
  }

  const after = await cdp.eval(STATE);
  record("the decision is displayed in the UI", !!after.mode, "badge: " + JSON.stringify(after.mode));
  if (after.reasons.length) {
    note("server reasons:");
    for (const r of after.reasons) note("  - " + r);
  }
  record("delivery went through MSE (blob source)", after.src.startsWith("blob:"),
    "src=" + after.src.slice(0, 40));

  // Let some content accumulate, then seek *backwards within produced content*
  // so the test measures seeking, not the transcode catching up.
  if (firstFrameMs !== null) {
    await sleep(6000);
    const seekState = await cdp.eval(STATE);
    note("after buffering: currentTime=" + seekState.currentTime.toFixed(2) +
      " seekable=" + seekState.seekableEnd.toFixed(2));

    const target = Math.max(0.5, Math.min(seekState.currentTime - 1.5, seekState.seekableEnd - 0.5));
    if (target > 0.2 && seekState.seekableEnd > 1) {
      await cdp.eval(
        "(() => { const v = document.querySelector('video'); v.pause(); v.currentTime = " +
          target + "; return true; })()"
      );
      await sleep(3000);
      const landed = await cdp.eval(STATE);
      record("seeking within produced content works",
        Math.abs(landed.currentTime - target) < 1.5,
        "target " + target.toFixed(2) + " -> landed " + landed.currentTime.toFixed(2));
    } else {
      note("skipped the seek check: only " + seekState.seekableEnd.toFixed(2) + "s produced so far");
    }
  }

  record("no console errors during the run", consoleErrors.length === 0,
    consoleErrors.length ? consoleErrors.slice(0, 3).join(" | ") : "clean");

  const failed = results.filter((r) => !r.ok);
  console.log("\n" + (results.length - failed.length) + "/" + results.length + " checks passed");
  process.exit(failed.length ? 1 : 0);
}

main().catch((err) => {
  console.error("harness error: " + err.message);
  process.exit(3);
});
