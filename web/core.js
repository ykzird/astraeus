/* ==========================================================================
   Astraeus Media — pure front-end core
   --------------------------------------------------------------------------
   The timeline arithmetic and clock formatting the player is built on, kept
   apart from app.js so they can be unit-tested without a browser. Nothing here
   touches the DOM, the network or any module state: every function is a value
   in, value out, which is what makes it testable at all.

   Loaded as a classic script before app.js, and as a CommonJS module by
   web/core.test.js under `node --test`. No build step, no dependencies.
   ========================================================================== */

(function (root, factory) {
  "use strict";
  const api = factory();
  if (typeof module === "object" && module.exports) {
    module.exports = api;
  } else {
    root.AstraeusCore = api;
  }
})(typeof globalThis !== "undefined" ? globalThis : this, function () {
  "use strict";

  /** A finite number, or the fallback. The API is untrusted input. */
  function finiteOr(value, fallback) {
    return typeof value === "number" && isFinite(value) ? value : fallback;
  }

  /** Two digits for values under 100; anything else is left alone. */
  function pad2(value) {
    const text = String(value);
    return /^\d+$/.test(text) ? text.padStart(2, "0") : text;
  }

  /**
   * A clock for a number of seconds: `m:ss`, or `h:mm:ss` once there is an
   * hour. A negative or non-finite value reads as 0:00 rather than "NaN:NaN",
   * because the durations here come from a media element that reports NaN
   * before it has metadata.
   */
  function formatClock(seconds) {
    if (typeof seconds !== "number" || !isFinite(seconds) || seconds < 0) return "0:00";
    const total = Math.floor(seconds);
    const hours = Math.floor(total / 3600);
    const minutes = Math.floor((total % 3600) / 60);
    const secs = total % 60;
    if (hours > 0) {
      return hours + ":" + pad2(minutes) + ":" + pad2(secs);
    }
    return minutes + ":" + pad2(secs);
  }

  /**
   * Source time from media time. A segmented session's element clock restarts
   * at zero, and `sessionStart` is where that session begins in the source, so
   * source = media + sessionStart. Everything the viewer is shown goes through
   * this: showing the raw element clock would restart the timeline at every
   * seek, quality change and resume.
   */
  function sourceTime(mediaSeconds, sessionStart) {
    return finiteOr(sessionStart, 0) + finiteOr(mediaSeconds, 0);
  }

  /** Media time from source time: the inverse of sourceTime. */
  function mediaTime(sourceSeconds, sessionStart) {
    return finiteOr(sourceSeconds, 0) - finiteOr(sessionStart, 0);
  }

  /**
   * The part of the source this session can already reach without
   * re-buffering, in source seconds, or null when the element reports no
   * seekable range yet.
   */
  function producedWindow(bounds, sessionStart) {
    if (!bounds || !(finiteOr(bounds.end, 0) > 0)) return null;
    const offset = finiteOr(sessionStart, 0);
    return {
      from: offset + finiteOr(bounds.start, 0),
      to: offset + finiteOr(bounds.end, 0),
    };
  }

  /**
   * Whether the browser can show a subtitle track itself. The server hands out
   * a URL for every track it can deliver as WebVTT - text tracks always, and
   * image tracks too once it has read them - so a URL is the whole test.
   */
  function subtitleDeliverable(track) {
    return !!track && typeof track.url === "string" && track.url.length > 0;
  }

  /**
   * Whether an image-based track has no text path, so compositing it into the
   * video is the only way to show it. `text === false` says the source carries
   * pictures; the missing URL says the server cannot turn them into WebVTT.
   * Both halves matter: a server with subtitle conversion switched off hands
   * out no URL for text tracks either, and those would be refused rather than
   * burned.
   */
  function subtitleNeedsBurn(track) {
    return !!track && track.text === false && !subtitleDeliverable(track);
  }

  return {
    formatClock: formatClock,
    mediaTime: mediaTime,
    pad2: pad2,
    producedWindow: producedWindow,
    sourceTime: sourceTime,
    subtitleDeliverable: subtitleDeliverable,
    subtitleNeedsBurn: subtitleNeedsBurn,
  };
});
