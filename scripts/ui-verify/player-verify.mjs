// Player verification: drives the real UI in headless Chromium over CDP and
// reports what the <video> element actually does.
//
// Usage: node player-verify.mjs <baseUrl> <episodeId> <movieId>
import { CDP } from "./cdp.mjs";

const [baseUrl, episodeId, movieId] = process.argv.slice(2);
if (!baseUrl || !episodeId || !movieId) {
  console.error("usage: node player-verify.mjs <baseUrl> <episodeId> <movieId>");
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
  throw new Error("no debuggable page appeared on port " + PORT);
}

// videoState reads everything we care about from the live element.
const VIDEO_STATE = `(() => {
  const v = document.querySelector("video");
  if (!v) return null;
  const s = { currentTime: v.currentTime, paused: v.paused, readyState: v.readyState,
              duration: v.duration, src: v.currentSrc || v.src || "",
              ended: v.ended, error: v.error ? v.error.code : null,
              seekableEnd: v.seekable && v.seekable.length ? v.seekable.end(0) : 0,
              hasHls: typeof window.Hls !== "undefined" };
  return s;
})()`;

async function main() {
  const wsUrl = await pageSocketUrl();
  const cdp = new CDP(wsUrl);
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

  /* ---------- segmented playback: mkv episode, remux -> HLS ---------- */
  await cdp.send("Page.navigate", { url: baseUrl + "/#/entity/" + episodeId });
  await sleep(2500);

  const before = await cdp.eval(VIDEO_STATE);
  const hlsLoadedBefore = await cdp.eval("typeof window.Hls !== 'undefined'");
  record("hls.js is NOT loaded before any segmented playback", hlsLoadedBefore === false,
    "window.Hls=" + hlsLoadedBefore);

  await cdp.click('[data-action="play"]');
  await sleep(1200);
  const first = await cdp.eval(VIDEO_STATE);
  record("segmented: a video element exists after pressing Play", first !== null,
    first ? "src=" + first.src.slice(0, 60) : "no <video>");

  if (first) {
    record("segmented: hls.js was loaded for the HLS stream",
      await cdp.eval("typeof window.Hls !== 'undefined'"));
    await sleep(2500);
    const later = await cdp.eval(VIDEO_STATE);
    // The demo clips are ~3s, so a long wait can reach the end and pause.
    // Progress is the property under test, not whether it is still running.
    const progressed = later.currentTime - first.currentTime;
    const reachedEnd = later.ended || (later.duration > 0 && later.currentTime >= later.duration - 0.15);
    record("segmented: playback actually advances",
      progressed > 0.2 && (!later.paused || reachedEnd),
      "currentTime " + first.currentTime.toFixed(3) + " -> " + later.currentTime.toFixed(3) +
      ", progressed=" + progressed.toFixed(3) + ", paused=" + later.paused +
      ", ended=" + later.ended + ", readyState=" + later.readyState);
    record("segmented: media was driven through MSE, not a native m3u8 src",
      later.src.startsWith("blob:"), "currentSrc=" + later.src.slice(0, 40));
    record("segmented: seekable range is known", later.seekableEnd > 0,
      "seekable.end=" + later.seekableEnd.toFixed(2));
  }

  /* ---------- direct play: mp4 movie, native ---------- */
  await cdp.send("Page.navigate", { url: baseUrl + "/#/entity/" + movieId });
  await sleep(2500);
  await cdp.click('[data-action="play"]');
  await sleep(1500);

  const d1 = await cdp.eval(VIDEO_STATE);
  record("direct: a video element exists", d1 !== null, d1 ? "src=" + d1.src.slice(0, 60) : "none");
  if (d1) {
    await sleep(1500);
    const d2 = await cdp.eval(VIDEO_STATE);
    record("direct: playback advances natively", d2.currentTime > 0.2 && !d2.paused,
      "currentTime " + d1.currentTime.toFixed(3) + " -> " + d2.currentTime.toFixed(3) +
      ", paused=" + d2.paused);
    // Pause before seeking: otherwise the clip keeps playing during the wait
    // and the assertion measures elapsed time rather than the seek.
    const seekTarget = 1.5;
    await cdp.eval(
      "(() => { const v = document.querySelector('video'); v.pause(); v.currentTime = " + seekTarget + "; return true; })()");
    await sleep(700);
    const d3 = await cdp.eval(VIDEO_STATE);
    record("direct: seeking lands on the target", Math.abs(d3.currentTime - seekTarget) < 0.35,
      "target " + seekTarget.toFixed(2) + " -> landed " + d3.currentTime.toFixed(2) +
      ", paused=" + d3.paused);
  }

  record("no console errors during the run", consoleErrors.length === 0,
    consoleErrors.length ? consoleErrors.slice(0, 3).join(" | ") : "clean");

  /* ---------- transcode: force a re-encode and play that HLS output ---------- */
  // The demo files only ever negotiate direct_play/remux with the browser
  // profile, so a transcode session is requested explicitly to exercise the
  // re-encoded HLS path through the same player machinery.
  const res = await fetch(baseUrl + "/api/entities/" + movieId + "/playback", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({
      containers: ["hls"],
      video_codecs: ["h264"],
      audio_codecs: ["aac"],
      max_height: 180,
      supports_hls: true,
    }),
  });
  const playback = await res.json();
  record("a transcode session was negotiated", playback.mode === "transcode",
    "mode=" + playback.mode + " target_height=" + ((playback.decision || {}).target_height || "?"));

  if (playback.mode === "transcode" && playback.url) {
    // Play the transcoded playlist with the same hls.js the UI uses. Muted so
    // autoplay is permitted without needing a fresh user gesture.
    const started = await cdp.eval(`(async () => {
      const video = document.createElement("video");
      video.muted = true;
      video.id = "transcode-probe";
      document.body.appendChild(video);
      const Hls = window.Hls || await import("/vendor/hls.min.js").then(() => window.Hls);
      if (!Hls || !Hls.isSupported()) return "no-mse";
      const hls = new Hls();
      window.__probeHls = hls;
      hls.loadSource(${JSON.stringify(playback.url)});
      hls.attachMedia(video);
      await new Promise((resolve, reject) => {
        hls.on(Hls.Events.MANIFEST_PARSED, resolve);
        hls.on(Hls.Events.ERROR, (e, data) => { if (data && data.fatal) reject(new Error(data.type)); });
        setTimeout(() => reject(new Error("manifest timeout")), 20000);
      });
      await video.play().catch(() => {});
      return "ok";
    })()`);
    record("the transcoded playlist loads through hls.js", started === "ok", "start=" + started);

    await sleep(2500);
    const t = await cdp.eval(`(() => {
      const v = document.getElementById("transcode-probe");
      if (!v) return null;
      return { currentTime: v.currentTime, paused: v.paused, readyState: v.readyState,
               duration: v.duration, error: v.error ? v.error.code : null };
    })()`);
    record("transcoded playback actually advances",
      t !== null && t.currentTime > 0.2 && t.error === null,
      t ? "currentTime=" + t.currentTime.toFixed(3) + ", paused=" + t.paused +
          ", readyState=" + t.readyState + ", duration=" + t.duration : "no probe element");
  }

  const failed = results.filter((r) => !r.ok);
  console.log("\n" + (results.length - failed.length) + "/" + results.length + " checks passed");
  process.exit(failed.length ? 1 : 0);
}

main().catch((err) => {
  console.error("harness error: " + err.message);
  process.exit(3);
});
