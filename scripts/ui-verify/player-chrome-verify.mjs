// Verifies the player chrome: controls overlaid on the video, and fullscreen.
//
// Usage: node player-chrome-verify.mjs <baseUrl> <entityId> [timeoutSeconds]
//
// Selectors are resolved semantically (a fullscreen control is whatever looks
// like one) so this does not depend on class names.
import { CDP } from "./cdp.mjs";

const [baseUrl, entityId, timeoutArg] = process.argv.slice(2);
if (!baseUrl || !entityId) {
  console.error("usage: node player-chrome-verify.mjs <baseUrl> <entityId> [timeoutSeconds]");
  process.exit(2);
}
const TIMEOUT_S = Number(timeoutArg || 120);
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
  throw new Error("no page on port " + PORT);
}

/* Semantic snapshot of the player and its chrome. */
const SNAPSHOT = `(() => {
  const video = document.querySelector("video");
  const player = document.getElementById("player-layer") ||
                 (video ? video.parentElement : null);
  // Prefer the documented hook; fall back to semantics so renames do not break
  // the harness.
  const bar = (() => {
    const explicit = document.getElementById("player-bar");
    if (explicit) return explicit;
    const toggle = document.getElementById("player-toggle") ||
                   document.querySelector('[data-action="play"]');
    let node = toggle;
    while (node && node !== document.body) {
      if (node.querySelector && node.querySelector('input[type="range"]')) return node;
      node = node.parentElement;
    }
    return toggle ? toggle.parentElement : null;
  })();
  const fullscreenButton = Array.from(document.querySelectorAll("button")).find((b) =>
    /fullscreen|full screen/i.test((b.getAttribute("aria-label") || "") + " " + (b.textContent || "")));
  const range = player ? player.querySelector('input[type="range"]') : null;

  // The overlay is the element that carries the hidden state; the bar inside it
  // keeps its own computed opacity, so it cannot be used to detect hiding.
  const overlay = document.getElementById("player-overlay") || bar;
  const overlayStyle = overlay ? getComputedStyle(overlay) : null;
  const controlsVisible = (() => {
    if (!overlay) return false;
    if (overlay.hasAttribute("hidden")) return false;
    if (overlay.hasAttribute("inert")) return false;
    if (overlay.classList.contains("is-hidden") || overlay.classList.contains("controls-hidden")) return false;
    if (overlayStyle.visibility === "hidden" || Number(overlayStyle.opacity) < 0.05) return false;
    return true;
  })();
  const focusablesReachable = (() => {
    if (!overlay || overlay.hasAttribute("inert")) return 0;
    // A hidden bar must not be tabbable, or keyboard users meet an invisible trap.
    return Array.from(overlay.querySelectorAll("button, input")).filter((el) => {
      if (el.disabled) return false;
      const s = getComputedStyle(el);
      return s.visibility !== "hidden" && Number(s.opacity) > 0.05 && el.tabIndex >= 0;
    }).length;
  })();

  // Every icon must actually draw something at a usable size: a wrong registry
  // key renders as an empty <svg>, which is easy to miss by eye.
  const iconNodes = Array.from((overlay || document).querySelectorAll("svg.icon"));
  const iconAudit = {
    total: iconNodes.length,
    drawn: iconNodes.filter((svg) => {
      const path = svg.querySelector("path");
      return path && (path.getAttribute("d") || "").length > 10;
    }).length,
    sized: iconNodes.filter((svg) => {
      const r = svg.getBoundingClientRect();
      return r.width >= 6 && r.height >= 6;
    }).length,
    hidden: iconNodes.filter((svg) => getComputedStyle(svg).display === "none").length
  };
  const fullscreenPath = fullscreenButton ? fullscreenButton.querySelector("path") : null;
  const fullscreenIconD = fullscreenPath ? (fullscreenPath.getAttribute("d") || "").slice(0, 60) : null;

  const mute = document.getElementById("player-mute");
  const volume = document.getElementById("player-volume");
  const quality = document.getElementById("player-quality");
  const timeText = document.getElementById("player-time");
  return {
    hasVideo: !!video,
    muted: video ? video.muted : null,
    volume: video ? video.volume : null,
    muteFound: !!mute,
    mutePressed: mute ? mute.getAttribute("aria-pressed") : null,
    muteLabel: mute ? (mute.getAttribute("aria-label") || "") : null,
    volumeFound: !!volume,
    volumeValue: volume ? Number(volume.value) : null,
    qualityFound: !!quality,
    qualityOptions: quality ? Array.from(quality.options).map((o) => ({ value: o.value, label: o.textContent.trim() })) : [],
    qualityValue: quality ? quality.value : null,
    timeText: timeText ? timeText.textContent.trim() : null,
    storedAudio: (() => { try { return localStorage.getItem("astraeus.audio"); } catch (e) { return null; } })(),
    iconAudit: iconAudit,
    fullscreenIconD: fullscreenIconD,
    paused: video ? video.paused : null,
    currentTime: video ? video.currentTime : null,
    duration: video ? video.duration : null,
    readyState: video ? video.readyState : null,
    playControls: document.querySelectorAll('[data-action="play"]').length,
    // The hero offers its own Play action before playback starts; that is a
    // different affordance from the player's transport, so only the player's
    // own controls have to be unique.
    playControlsInPlayer: Array.from(document.querySelectorAll('[data-action="play"]'))
      .filter((el) => player && player.contains(el)).length,
    barFound: !!bar,
    barInsidePlayer: !!(bar && player && player.contains(bar)),
    barClass: bar ? bar.className : null,
    overlayClass: overlay ? overlay.className : null,
    overlayInert: overlay ? overlay.hasAttribute("inert") : null,
    controlsVisible: controlsVisible,
    focusablesInBar: focusablesReachable,
    fullscreenButton: !!fullscreenButton,
    fullscreenLabel: fullscreenButton ? (fullscreenButton.getAttribute("aria-label") || fullscreenButton.textContent || "").trim() : null,
    fullscreenInsidePlayer: !!(fullscreenButton && player && player.contains(fullscreenButton)),
    fullscreenPressed: fullscreenButton ? fullscreenButton.getAttribute("aria-pressed") : null,
    rangeInPlayer: !!range,
    rangeDisabled: range ? range.disabled : null,
    rangeValue: range ? Number(range.value) : null,
    rangeMax: range ? Number(range.max) : null,
    fullscreenElement: (() => {
      const el = document.fullscreenElement;
      if (!el) return null;
      return el === player ? "player" : (el.tagName || "other");
    })(),
    // The sidebar must hold information only: no transport, no seek bar.
    contextPanelControls: (() => {
      const sidebar = document.getElementById("context-body");
      if (!sidebar) return -1;
      return sidebar.querySelectorAll('[data-action="play"], [data-action="restart"], [data-action="skip"], [data-action="stop-playback"], [data-action="select-subtitle"], input[type="range"]').length;
    })(),
    contextPanelSeek: (() => {
      const sidebar = document.getElementById("context-body");
      return sidebar ? sidebar.querySelectorAll('input[type="range"]').length : -1;
    })()
  };
})()`;

async function pressKey(cdp, key, code, keyCode, text) {
  const base = { key, code, windowsVirtualKeyCode: keyCode, nativeVirtualKeyCode: keyCode };
  await cdp.send("Input.dispatchKeyEvent", { type: "rawKeyDown", ...base, ...(text ? { text } : {}) });
  await cdp.send("Input.dispatchKeyEvent", { type: "keyUp", ...base });
}

async function main() {
  const cdp = new CDP(await pageSocketUrl());
  await cdp.connect();
  await cdp.send("Runtime.enable");
  await cdp.send("Page.enable");
  cdp.on("Runtime.consoleAPICalled", (p) => {
    if (p.type === "error") consoleErrors.push((p.args || []).map((a) => a.value || a.description || "").join(" "));
  });
  cdp.on("Runtime.exceptionThrown", (p) => {
    consoleErrors.push("exception: " + JSON.stringify(p.exceptionDetails && p.exceptionDetails.text));
  });

  await cdp.send("Page.navigate", { url: baseUrl + "/#/entity/" + entityId });
  for (let i = 0; i < 40; i++) {
    if (await cdp.eval(`!!document.querySelector('[data-action="play"]')`)) break;
    await sleep(500);
  }

  console.log("Pressing Play and waiting for the first frame...");
  // Start playback the way a person does: a real mouse click on the hero
  // button, with the pointer left exactly where it clicked. That is what put
  // the overlay under a stationary cursor and kept the bar on screen forever.
  const heroBox = await cdp.eval(`(() => {
    const b = document.querySelector('#canvas-content [data-action="play"]') ||
              document.querySelector('[data-action="play"]');
    if (!b) return null;
    const r = b.getBoundingClientRect();
    return { x: Math.round(r.x + r.width / 2), y: Math.round(r.y + r.height / 2) };
  })()`);
  if (heroBox) {
    for (const type of ["mousePressed", "mouseReleased"]) {
      await cdp.send("Input.dispatchMouseEvent", { type, x: heroBox.x, y: heroBox.y, button: "left", clickCount: 1 });
    }
  } else {
    await cdp.click('[data-action="play"]');
  }
  let state = null;
  const started = Date.now();
  while (Date.now() - started < TIMEOUT_S * 1000) {
    state = await cdp.eval(SNAPSHOT);
    if (state && state.readyState >= 2 && state.currentTime > 0.2) break;
    await sleep(1000);
  }
  if (!state || state.readyState < 2) {
    record("playback started", false, "no frame within " + TIMEOUT_S + "s");
    process.exit(1);
  }
  record("playback started", true, "currentTime=" + state.currentTime.toFixed(2));

  /* ---- the reported bug: the bar must hide even if the mouse never moves ---- */
  // Deliberately no pointer movement here.
  await sleep(6000);
  const untouched = await cdp.eval(SNAPSHOT);
  record("the bar hides without the pointer ever moving (the reported bug)",
    untouched.controlsVisible === false,
    "still visible after 6s with the pointer untouched; overlayClass=" +
      JSON.stringify(untouched.overlayClass));
  if (untouched.controlsVisible === false) {
    // Bring it back for the checks that follow.
    const c = await cdp.eval(`(() => { const el = document.getElementById("player-layer"); const r = el.getBoundingClientRect(); return { x: Math.round(r.x + r.width/2), y: Math.round(r.y + r.height/2) }; })()`);
    await cdp.send("Input.dispatchMouseEvent", { type: "mouseMoved", x: 1, y: 1 });
    await cdp.send("Input.dispatchMouseEvent", { type: "mouseMoved", x: c.x, y: c.y });
    await sleep(600);
  }

  /* ---- the chrome lives inside the player ---- */
  record("the player has exactly one play control",
    state.playControlsInPlayer === 1,
    "in player=" + state.playControlsInPlayer + ", total on page=" + state.playControls);
  record("a control bar exists inside the player container",
    state.barFound && state.barInsidePlayer, "barClass=" + JSON.stringify(state.barClass));
  record("the seek bar is inside the player container", state.rangeInPlayer === true);
  record("a fullscreen control exists and is inside the player",
    state.fullscreenButton && state.fullscreenInsidePlayer, "label=" + JSON.stringify(state.fullscreenLabel));
  record("the context sidebar holds no transport controls",
    state.contextPanelControls === 0,
    "controls in sidebar=" + state.contextPanelControls + ", seek bars=" + state.contextPanelSeek);

  record("every icon in the player draws a path at a usable size",
    state.iconAudit.total > 0 &&
      state.iconAudit.drawn === state.iconAudit.total &&
      state.iconAudit.sized === state.iconAudit.total &&
      state.iconAudit.hidden === 0,
    "total=" + state.iconAudit.total + ", drawn=" + state.iconAudit.drawn +
      ", sized=" + state.iconAudit.sized + ", display-none=" + state.iconAudit.hidden);

  /* ---- controls actually drive the video ---- */
  const beforeToggle = await cdp.eval(SNAPSHOT);
  await cdp.click("#player-toggle");
  await sleep(900);
  const afterToggle = await cdp.eval(SNAPSHOT);
  record("the overlay play control toggles playback",
    afterToggle.paused !== beforeToggle.paused,
    "paused " + beforeToggle.paused + " -> " + afterToggle.paused);
  if (afterToggle.paused) {
    // Leave it playing for the auto-hide check.
    await cdp.click("#player-toggle");
    await sleep(800);
  }

  /* ---- fullscreen ---- */
  const fsBox = await cdp.eval(`(() => {
    const b = Array.from(document.querySelectorAll("button")).find((x) =>
      /fullscreen|full screen/i.test((x.getAttribute("aria-label") || "") + " " + (x.textContent || "")));
    if (!b) return null;
    const r = b.getBoundingClientRect();
    return { x: r.x + r.width / 2, y: r.y + r.height / 2 };
  })()`);
  if (fsBox) {
    for (const type of ["mousePressed", "mouseReleased"]) {
      await cdp.send("Input.dispatchMouseEvent", { type, x: fsBox.x, y: fsBox.y, button: "left", clickCount: 1 });
    }
  }
  await sleep(1200);
  const fs = await cdp.eval(SNAPSHOT);
  record("fullscreen enters on the player container",
    fs.fullscreenElement === "player",
    "fullscreenElement=" + JSON.stringify(fs.fullscreenElement));
  if (fs.fullscreenElement === "player") {
    record("the control bar is still on screen in fullscreen",
      fs.barFound && fs.barInsidePlayer, "barClass=" + JSON.stringify(fs.barClass));
    record("the fullscreen control reflects the state",
      String(fs.fullscreenPressed) === "true", "aria-pressed=" + JSON.stringify(fs.fullscreenPressed));
    record("the fullscreen icon swaps to the exit glyph",
      !!fs.fullscreenIconD && fs.fullscreenIconD !== state.fullscreenIconD,
      "glyph changed=" + (fs.fullscreenIconD !== state.fullscreenIconD));

    // Leave fullscreen the way a user would, and check the control catches up.
    await cdp.eval("document.exitFullscreen && document.exitFullscreen()");
    await sleep(1000);
    const exited = await cdp.eval(SNAPSHOT);
    record("leaving fullscreen updates the control",
      exited.fullscreenElement === null && String(exited.fullscreenPressed) !== "true",
      "fullscreenElement=" + JSON.stringify(exited.fullscreenElement) +
      " aria-pressed=" + JSON.stringify(exited.fullscreenPressed));
  } else {
    note("fullscreen was not entered; this may be a headless limitation rather than an app fault");
  }

  /* ---- keyboard ---- */
  await cdp.send("Page.bringToFront");
  await cdp.eval("(() => { const el = document.activeElement; if (el && el.blur) el.blur(); return document.activeElement ? document.activeElement.tagName : null; })()");
  const beforeKey = await cdp.eval(SNAPSHOT);
  await pressKey(cdp, " ", "Space", 32, " ");
  await sleep(900);
  const afterKey = await cdp.eval(SNAPSHOT);
  record("Space toggles playback from the keyboard",
    afterKey.paused !== beforeKey.paused,
    "paused " + beforeKey.paused + " -> " + afterKey.paused);
  if (afterKey.paused) {
    await pressKey(cdp, " ", "Space", 32, " ");
    await sleep(600);
  }

  /* ---- audio controls ---- */
  record("a mute control exists", state.muteFound === true,
    "aria-pressed=" + JSON.stringify(state.mutePressed) + " label=" + JSON.stringify(state.muteLabel));

  const beforeMute = await cdp.eval(SNAPSHOT);
  await cdp.click("#player-mute");
  await sleep(500);
  const afterMute = await cdp.eval(SNAPSHOT);
  record("the mute control mutes and unmutes the element",
    afterMute.muted !== beforeMute.muted,
    "muted " + beforeMute.muted + " -> " + afterMute.muted);
  if (afterMute.muted) {
    await cdp.click("#player-mute");
    await sleep(400);
  }

  record("a volume control exists", state.volumeFound === true, "value=" + state.volumeValue);
  const volumeSet = await cdp.eval(`(() => {
    const v = document.getElementById("player-volume");
    if (!v) return null;
    v.value = "0.35";
    v.dispatchEvent(new Event("input", { bubbles: true }));
    v.dispatchEvent(new Event("change", { bubbles: true }));
    return true;
  })()`);
  await sleep(500);
  const afterVolume = await cdp.eval(SNAPSHOT);
  record("the volume control drives the element and is remembered",
    volumeSet === true && Math.abs((afterVolume.volume || 0) - 0.35) < 0.06 &&
      typeof afterVolume.storedAudio === "string" && afterVolume.storedAudio.length > 0,
    "video.volume=" + afterVolume.volume + " stored=" + JSON.stringify(afterVolume.storedAudio));

  /* ---- quality control, and the resume it depends on ---- */
  if (state.qualityFound) {
    record("the quality menu offers more than one choice",
      state.qualityOptions.length >= 2,
      "options=" + JSON.stringify(state.qualityOptions.map((o) => o.label)));

    // The readout is "current / total", so only the first half is the position.
    const parseClock = (text) => {
      if (!text) return null;
      const current = String(text).split("/")[0].trim();
      const parts = current.split(":").map((n) => Number(n));
      if (!parts.length || parts.some((n) => Number.isNaN(n))) return null;
      return parts.reduce((acc, n) => acc * 60 + n, 0);
    };

    // Seek well past anything the transcoder has produced. That has to
    // re-negotiate at the offset rather than clamp, which is the same machinery
    // a quality switch depends on.
    const SEEK_TARGET = 600;
    note("seeking to " + SEEK_TARGET + "s, past everything produced so far");
    await cdp.eval(`(() => {
      const seek = document.getElementById("player-seek");
      if (!seek) return null;
      seek.value = String(${SEEK_TARGET});
      seek.dispatchEvent(new Event("input", { bubbles: true }));
      seek.dispatchEvent(new Event("change", { bubbles: true }));
      return true;
    })()`);

    let afterSeek = null;
    const seekDeadline = Date.now() + 90000;
    while (Date.now() < seekDeadline) {
      afterSeek = await cdp.eval(SNAPSHOT);
      if (afterSeek.readyState >= 2 && afterSeek.currentTime > 0.2) break;
      await sleep(1500);
    }
    const seeked = parseClock(afterSeek ? afterSeek.timeText : null);
    record("seeking past produced content resumes near the target",
      seeked !== null && Math.abs(seeked - SEEK_TARGET) < 120,
      "clock " + (afterSeek ? afterSeek.timeText : "?") + " (target " + SEEK_TARGET + "s)");

    const before = seeked;

    // Choose a different rendition, preferring the lowest to make the switch cheap.
    const target = state.qualityOptions
      .filter((o) => o.value && o.value !== state.qualityValue)
      .sort((a, b) => Number(a.value) - Number(b.value))[0];
    note("switching quality to " + (target ? target.label : "(none available)"));

    if (target) {
      await cdp.eval(`(() => {
        const q = document.getElementById("player-quality");
        q.value = ${JSON.stringify(target.value)};
        q.dispatchEvent(new Event("change", { bubbles: true }));
      })()`);

      // A switch re-negotiates, so allow time for the new transcode to start.
      let resumed = null;
      const deadline = Date.now() + 90000;
      while (Date.now() < deadline) {
        resumed = await cdp.eval(SNAPSHOT);
        if (resumed.readyState >= 2 && resumed.currentTime > 0.2) break;
        await sleep(1500);
      }
      record("quality switching keeps playing",
        resumed !== null && resumed.readyState >= 2 && resumed.currentTime > 0.2,
        resumed ? "currentTime=" + (resumed.currentTime || 0).toFixed(2) + " readyState=" + resumed.readyState : "no state");

      const after = parseClock(resumed ? resumed.timeText : null);
      // The whole point of start_seconds: the viewer does not go back to the title card.
      record("quality switching resumes near where the viewer was, not at zero",
        before !== null && after !== null && after > 20 && Math.abs(after - before) < 120,
        "clock " + (afterSeek ? afterSeek.timeText : "?") + " -> " + (resumed ? resumed.timeText : "?"));
    }
  } else {
    note("no quality menu found (expected only for a transcoded session)");
  }

  /* ---- auto-hide while playing ---- */
  const playing = await cdp.eval(SNAPSHOT);
  if (!playing.paused) {
    // The bar deliberately stays up while the pointer rests on it, so move the
    // pointer off the player first - otherwise this measures nothing.
    await cdp.send("Input.dispatchMouseEvent", { type: "mouseMoved", x: 4, y: 4 });
    await sleep(5000);
    const idle = await cdp.eval(SNAPSHOT);
    if (idle.controlsVisible === false) {
        record("the bar auto-hides while playing", true,
        "overlayClass=" + JSON.stringify(idle.overlayClass) + " inert=" + idle.overlayInert);
      record("a hidden bar is not tabbable",
        idle.focusablesInBar === 0, "focusable controls=" + idle.focusablesInBar);
      // Moving back over the player must bring it back.
      const centre = await cdp.eval(`(() => {
        const el = document.getElementById("player-layer");
        if (!el) return null;
        const r = el.getBoundingClientRect();
        return { x: Math.round(r.x + r.width / 2), y: Math.round(r.y + r.height / 2) };
      })()`);
      if (centre) {
        await cdp.send("Input.dispatchMouseEvent", { type: "mouseMoved", x: 1, y: 1 });
        await cdp.send("Input.dispatchMouseEvent", { type: "mouseMoved", x: centre.x, y: centre.y });
      }
      await sleep(900);
      const revealed = await cdp.eval(SNAPSHOT);
      record("moving the pointer back over the player brings the bar back",
        revealed.controlsVisible === true, "visible=" + revealed.controlsVisible);
    } else {
      record("the bar auto-hides while playing", false,
        "still visible after 6s idle (opacity/class based check)");
    }
  } else {
    note("skipped the auto-hide check: playback is paused");
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
