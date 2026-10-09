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

  /**
   * Whether a track in the menu can actually be chosen.
   *
   * A text track needs a URL: the player toggles a <track> the element already
   * owns, and a track with no URL has nothing to toggle. An image track needs a
   * server that will composite it, which needs a burn index - a track with
   * neither is listed but cannot be selected.
   *
   * The rule exists because the menu offered such tracks as enabled radios and
   * choosing one did nothing: `selectSubtitle` returned without a word after the
   * radio had already been repainted as checked, so the control looked broken
   * (W-6 of the 2026-10-09 review). A menu entry that cannot be honoured is
   * rendered disabled and says why.
   */
  function subtitleSelectable(track) {
    if (!track) return false;
    if (subtitleDeliverable(track)) return true;
    if (!subtitleNeedsBurn(track)) return false;
    const index = typeof track.index === "number" ? track.index : NaN;
    return isFinite(index) && index > 0;
  }

  /**
   * The session a stale negotiation left running on the server, or null.
   *
   * A negotiation that has been superseded - the viewer navigated away, pressed
   * Stop, or started another title - is discarded by the caller, but the server
   * has already started ffmpeg for it. Dropping the id meant that transcode ran
   * until the idle reaper collected it, so clicking through titles stacked
   * transcodes on a host that had been asked for one (W-4 of the 2026-10-09
   * review).
   *
   * `result` is the playback response the caller is about to discard, `current`
   * is the session that replaced this one, and `session` is the one the response
   * belongs to.
   *
   * Two things keep this from releasing something it should not. A session that
   * is still the one on screen is not stale at all, so nothing is returned. And a
   * session that has *already* been given this id - which is what happens when a
   * quality change renegotiates: `resumeSession` releases the previous id and the
   * response may name it again - must not be released a second time, because the
   * second request would stop whatever is now using that id.
   *
   * Returns null when there is nothing to release, so the caller's stale branch
   * stays a single call rather than a decision it could get wrong.
   */
  function orphanedSessionId(result, current, session) {
    const started = result && typeof result.session_id === "string" ? result.session_id : "";
    if (!started) return null;
    /* The response belongs to the session being handled; if that session is
       still the one on screen there is nothing stale about it. */
    if (current === session) return null;
    /* An older session's id must not be released by a newer response, and one
       already released must not be released again. */
    if (session && session.sessionId === started) return null;
    return started;
  }

  /**
   * Whether a broken segmented stream should be asked for again at where the
   * viewer is, rather than reported as failed.
   *
   * The recovery hls.js offers is `startLoad()`, which re-requests a playlist
   * that may no longer exist: a session the server has reaped answers 404, and
   * nothing in the player can bring it back. Re-negotiating starts a new session
   * at the current position, which is the one thing that can.
   *
   * This is the last resort *before* the failure path, not instead of it. It
   * happens once - a second failure is reported, because renegotiating in a loop
   * against a server that keeps refusing is a client bug, not a recovery - and
   * only when there is a position to resume from and a session that is still the
   * one on screen.
   */
  function shouldRenegotiateAfterFailure(options) {
    const opts = options || {};
    if (opts.sessionCurrent !== true) return false;
    if (opts.alreadyRenegotiated === true) return false;
    const position = Number(opts.position);
    return isFinite(position) && position >= 0;
  }

  /**
   * Whether a loaded entity list still belongs to the view that asked for it.
   *
   * A route change bumps `loadToken`, so a list that arrives after the viewer
   * has moved on is stale. The check used to happen in the caller, *after* the
   * helper had already written the list and its library id into shared state -
   * so the late answer of a slow library overwrote the current library's list,
   * and the caller's check then discarded nothing that mattered (W-7 of the
   * 2026-10-09 review). Checked here, the write never happens.
   */
  function entityListIsCurrent(token, currentToken) {
    /* No token means the caller is the only writer in its flow - a scan
       refreshing the list it just scanned, say - so there is nothing to be
       stale against and the write proceeds. Only a caller that was given a token
       can be overtaken, and only it is checked. */
    if (token === undefined || token === null) return true;
    return token === currentToken;
  }

  /**
   * What the shared entity state should be when there is no list at all.
   *
   * Both fields move together. Clearing the list but leaving the library id
   * pointing at the library that is gone made the next view render "0 entities"
   * for a library nobody had asked about - it read as an empty library rather
   * than as a missing one (W-8 of the 2026-10-09 review).
   */
  function clearedEntityList() {
    return { entities: [], entitiesLibraryId: null };
  }

  /**
   * The subtitle choice a failed re-negotiation should roll back to.
   *
   * `offered` is what the caller passed, when it passed anything: a caller that
   * changed the selection *before* asking for the re-negotiation is the only one
   * that still knows what was showing, because the session object already holds
   * the new choice. Falling back to the session's own value is right for the
   * other callers, which renegotiate for a reason that does not involve the
   * subtitles.
   *
   * Getting this wrong is quiet rather than loud: the rollback re-asserted the
   * choice that had just failed, so a failed burn left the menu claiming a track
   * that was not playing while the radio for the track that *was* playing sat
   * disabled (W-3 of the 2026-10-09 review).
   */
  function rollbackSubtitleSelection(offered, current) {
    return offered === undefined ? current : offered;
  }

  /**
   * Whether the page-leaving teardown still has work to do.
   *
   * pagehide and beforeunload both fire as a tab goes away, and both call the same
   * handler - so without this the final position was PUT twice and the session
   * DELETEd twice. The second DELETE is a request for a session that no longer
   * exists, which is noise in the log at best (W-10 of the 2026-10-09 review).
   *
   * `release` is the session id already sent; a different one means the viewer
   * started a new stream after the first teardown, so it is not the same work.
   */
  function unloadTeardownIsPending(options) {
    const opts = options || {};
    if (opts.releasedSessionId === null || opts.releasedSessionId === undefined) {
      return true;
    }
    return opts.releasedSessionId !== opts.sessionId;
  }

  /**
   * What to do when a page comes back from the back/forward cache.
   *
   * A restored page is the same document with everything still in memory, but its
   * stream session was released as the page was hidden - so the player has no url
   * and cannot resume on its own. Without this it sat dead, and the only way back
   * was to navigate in again. `persisted` is how a browser says it came from the
   * cache rather than from a fresh load; a fresh load rebuilds the app and must not
   * take this path, or it would try to resume a stream it never started.
   */
  function bfcacheRestore(options) {
    const opts = options || {};
    if (opts.persisted !== true) return { action: "none" };
    if (opts.hasUrl) return { action: "none" };
    const position = Number(opts.position);
    if (!isFinite(position) || position <= 0) return { action: "none" };
    return { action: "renegotiate", position: position };
  }

  /**
   * The pruning counts a scan summary should mention.
   *
   * Only the non-zero ones. Pruning is the number that says the scan *removed*
   * something - a deleted film leaving the library, a file that moved - and leaving
   * it out of the summary meant a scan that dropped half a library read exactly like
   * one that did nothing (W-10 of the 2026-10-09 review). Reporting "0 pruned" on
   * every scan would be noise, so it is the non-zero values that earn a line.
   */
  function prunedSummary(result) {
    const info = result && typeof result === "object" ? result : {};
    const out = [];
    for (const [key, label] of [
      ["entities_pruned", "entities removed"],
      ["objects_pruned", "objects removed"],
    ]) {
      const count = Number(info[key]);
      if (isFinite(count) && count > 0) {
        out.push({ count: count, label: label });
      }
    }
    return out;
  }

  /**
   * Whether the health check should run now.
   *
   * A hidden tab asks nothing: the answer is not being looked at, and a phone left
   * on this page should not poll all day. The check on becoming visible again is
   * what keeps the pill honest, and the check at startup is unconditional because a
   * page that opens hidden still has to show something when it is looked at.
   */
  function shouldCheckHealth(options) {
    const opts = options || {};
    if (opts.firstCheck === true) return true;
    return opts.hidden !== true;
  }

  /** The message a request that ran out of time produces. */
  function timeoutMessage(timeoutMs) {
    return (
      "The request to the Astraeus API timed out after " +
      Math.round(Number(timeoutMs) / 1000) +
      " seconds."
    );
  }

  /**
   * What a failed part of a fetch means, so every stage of one reports it the
   * same way.
   *
   * `stage` is "send" while the request is being made and "body" while the
   * response body is being read. An abort means the timeout fired, wherever it
   * fired. A body that failed for another reason is not a timeout and must not
   * be reported as one: the caller still has a status code, which is a better
   * answer than "network error".
   *
   * This exists because the timeout used to be cleared once the headers arrived,
   * so a stalled body was covered by nothing and the UI could spin forever while
   * the module header claimed every fetch had a timeout (W-9 of the 2026-10-09
   * review).
   */
  function fetchFailure(stage, error, timeoutMs) {
    const aborted = !!error && error.name === "AbortError";
    if (aborted) {
      return { kind: "timeout", message: timeoutMessage(timeoutMs) };
    }
    if (stage === "body") {
      return { kind: "body", message: "" };
    }
    return { kind: "network", message: "" };
  }

  /* ── progress reporting ───────────────────────────────────────────────── */

  /** A position shorter than this is not worth remembering. */
  const RESUME_MIN_SECONDS = 5;

  /** A position past this fraction of the runtime counts as finished. */
  const FINISHED_FRACTION = 0.95;

  /**
   * Decide what a progress report should do, as a pure function of the
   * playback's state and where it is.
   *
   * This exists because the decision was spread through `reportProgress` and
   * `rememberProgress`, and the two disagreed in the one case that lost data.
   * Stopping or navigating away before the media's metadata has loaded reports
   * position 0 - the timeline is not known yet - and 0 is below the resume
   * threshold, so the old code called `clearProgress` and deleted a position
   * the viewer had. Pressing Play on a film stored at 45:00 and changing your
   * mind during the load lost the bookmark, and a slow link or a `moov` atom at
   * the end of the file made that window long (W-1 of the 2026-10-09 review).
   *
   * The rule that falls out: a position of zero from a passive report means
   * "the player does not know yet", which is not the same statement as "the
   * viewer is at the beginning", and only the second one should delete
   * anything.
   *
   * `started` says whether playback has actually begun. A report from before
   * that is a report about nothing.
   *
   * There is deliberately no restart flag. Starting over clears the position
   * through `forgetProgress`, which is an explicit request from the viewer, and
   * this function is only ever asked about passive reports.
   *
   * Returns one of:
   *   "skip"   - say nothing; the player does not know where it is yet
   *   "save"   - record the position
   *   "clear"  - forget the stored position
   */
  function progressAction(input) {
    const state = input || {};
    if (state.started !== true) return "skip";

    const position = Number(state.position);
    if (!isFinite(position) || position < 0) return "skip";

    /* A finished position is not worth resuming either, so it is dropped - but
       only when the report is a real one from a player that knows its timeline.
       The `started` check above is what keeps that from firing at load time. */
    const duration = Number(state.duration);
    if (duration > 0 && position >= duration * FINISHED_FRACTION) return "clear";

    if (position < RESUME_MIN_SECONDS) {
      /* Below the threshold, and the viewer is at the very beginning of
         something the player has loaded: there is nothing to remember. */
      return "clear";
    }
    return "save";
  }

  /**
   * Decide where, if anywhere, to resume a title.
   *
   * Returns the source offset to start at, or 0 for "from the beginning".
   *
   * This lives beside progressAction because the two answer halves of the same
   * question and used to disagree: the report path treated a position in the
   * last 5% as finished, while this refused to resume only in the last half
   * second. One rule now decides both, so a position the server has cleared
   * cannot be offered back, and a position a viewer would call finished is not
   * resumed.
   *
   * `progress` is the server's stored position for this entity.
   */
  function resumeOffsetFor(progress, leaf) {
    if (leaf !== true) return 0;
    if (!progress || typeof progress !== "object") return 0;
    if (progress.finished === true) return 0;

    const position = Number(progress.position_seconds);
    if (!isFinite(position) || position < RESUME_MIN_SECONDS) return 0;

    /* A stored position can outlive the file it was measured against - a
       different release of the same film, say - so a position that looks
       finished, or past the end, is not resumed. */
    const duration = Number(progress.duration_seconds);
    if (isFinite(duration) && duration > 0 && position >= duration * FINISHED_FRACTION) return 0;

    return position;
  }

  /** How long a job may run before the UI stops waiting for it.
      Longer than the server-side work is expected to take, and short enough that
      a wedged job does not leave a spinner forever. The work itself keeps
      running either way - that is the point of accepting it. */
  const JOB_WAIT_MS = 30 * 60 * 1000;

  /**
   * Wait for a job the server accepted, and report what it produced.
   *
   * Scanning and enriching answer `202` with a job to poll rather than holding
   * the request open, so the work outlives the client's own timeout and a large
   * scan finishes instead of being cancelled by the browser giving up on the
   * response (W-2 of the 2026-10-09 review).
   *
   * The waiting is injected - `sleep(ms)` and `status(id)` - so the loop is
   * testable without a browser or a server. Returns one of:
   *
   *   {outcome: "result", value}  - the job finished and produced this
   *   {outcome: "inline", value}  - the server answered inline; there is no job
   *   {outcome: "failed", error}  - the job ran and failed, with its own message
   *   {outcome: "timeout"}        - it outlived its welcome but is still running
   *
   * `onProgress` is called with each status so a caller can show movement.
   */
  async function awaitJob(accepted, options) {
    const opts = options || {};
    const sleep = opts.sleep;
    const status = opts.status;
    const waitMs = typeof opts.waitMs === "number" ? opts.waitMs : JOB_WAIT_MS;
    const now = typeof opts.now === "function" ? opts.now : Date.now;

    /* A response with no job id is either an older server or one built without a
       runner: both answer 200 with the result inline, and that is already what
       the caller wants. */
    if (!accepted || typeof accepted.job_id !== "string" || !accepted.job_id) {
      return { outcome: "inline", value: accepted };
    }
    if (typeof sleep !== "function" || typeof status !== "function") {
      throw new Error("awaitJob needs sleep and status functions");
    }

    const deadline = now() + waitMs;
    for (;;) {
      if (now() > deadline) return { outcome: "timeout" };

      await sleep(opts.intervalMs);

      const job = await status(accepted.job_id);
      if (!job) continue;
      if (opts.onProgress) opts.onProgress(job);

      if (job.state !== "done") continue;
      if (job.error) return { outcome: "failed", error: job.error };
      return { outcome: "result", value: job.result };
    }
  }

  /* The playback engines the server can report. Named here because they are
     compared against, and comparing against a string that is never produced is a
     silent bug: the player read `pb.engine === "native"` while native HLS sets
     `"native-hls"`, so a Safari user was told they were watching "the bundled
     hls.js player" (W-5 of the 2026-10-09 review). */
  const ENGINE_NATIVE = "native";
  const ENGINE_NATIVE_HLS = "native-hls";
  const ENGINE_HLS_JS = "hls.js";

  /**
   * What to call the engine the server reported.
   *
   * Unknown values are passed through rather than labelled as hls.js: saying
   * "the bundled hls.js player" about something else is worse than saying what
   * the server said.
   */
  function engineLabel(engine) {
    switch (engine) {
      case ENGINE_NATIVE:
        return "the browser's native playback";
      case ENGINE_NATIVE_HLS:
        return "the browser's native HLS support";
      case ENGINE_HLS_JS:
        return "the bundled hls.js player over MSE";
      default:
        return typeof engine === "string" && engine ? engine : "an unknown engine";
    }
  }

  return {
    ENGINE_NATIVE: ENGINE_NATIVE,
    ENGINE_NATIVE_HLS: ENGINE_NATIVE_HLS,
    ENGINE_HLS_JS: ENGINE_HLS_JS,
    engineLabel: engineLabel,
    JOB_WAIT_MS: JOB_WAIT_MS,
    awaitJob: awaitJob,
    FINISHED_FRACTION: FINISHED_FRACTION,
    RESUME_MIN_SECONDS: RESUME_MIN_SECONDS,
    formatClock: formatClock,
    progressAction: progressAction,
    resumeOffsetFor: resumeOffsetFor,
    mediaTime: mediaTime,
    pad2: pad2,
    producedWindow: producedWindow,
    sourceTime: sourceTime,
    subtitleDeliverable: subtitleDeliverable,
    subtitleNeedsBurn: subtitleNeedsBurn,
    subtitleSelectable: subtitleSelectable,
    orphanedSessionId: orphanedSessionId,
    shouldRenegotiateAfterFailure: shouldRenegotiateAfterFailure,
    fetchFailure: fetchFailure,
    rollbackSubtitleSelection: rollbackSubtitleSelection,
    unloadTeardownIsPending: unloadTeardownIsPending,
    bfcacheRestore: bfcacheRestore,
    prunedSummary: prunedSummary,
    shouldCheckHealth: shouldCheckHealth,
    entityListIsCurrent: entityListIsCurrent,
    clearedEntityList: clearedEntityList,
    timeoutMessage: timeoutMessage,
  };
});
