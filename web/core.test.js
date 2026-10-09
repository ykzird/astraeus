// Unit tests for the front end's pure core. Run with:
//
//	node --test web/*.test.js
//
// No dependency is involved: the test runner and the asserts are Node's own, and
// core.js exports itself as a CommonJS module when there is no browser.
const test = require("node:test");
const assert = require("node:assert/strict");

const core = require("./core.js");

test("pad2 pads only under three digits", () => {
  assert.equal(core.pad2(0), "00");
  assert.equal(core.pad2(5), "05");
  assert.equal(core.pad2(12), "12");
  assert.equal(core.pad2(100), "100");
  assert.equal(core.pad2("7"), "07");
});

test("formatClock reads as a clock at every scale", () => {
  assert.equal(core.formatClock(0), "0:00");
  assert.equal(core.formatClock(9), "0:09");
  assert.equal(core.formatClock(59), "0:59");
  assert.equal(core.formatClock(60), "1:00");
  assert.equal(core.formatClock(65.9), "1:05");
  assert.equal(core.formatClock(3599), "59:59");
  assert.equal(core.formatClock(3600), "1:00:00");
  assert.equal(core.formatClock(3661), "1:01:01");
  assert.equal(core.formatClock(7325), "2:02:05");
});

test("formatClock refuses to print NaN at a viewer", () => {
  // A media element reports NaN for its duration until it has metadata, which
  // is exactly when a placeholder would be most visible.
  for (const value of [undefined, null, NaN, Infinity, -Infinity, "60", -1]) {
    assert.equal(core.formatClock(value), "0:00", "for " + String(value));
  }
});

test("sourceTime adds the session offset, tolerating a missing clock", () => {
  assert.equal(core.sourceTime(10, 100), 110);
  assert.equal(core.sourceTime(0, 0), 0);
  assert.equal(core.sourceTime(NaN, 100), 100);
  assert.equal(core.sourceTime(10, undefined), 10);
  assert.equal(core.sourceTime(NaN, undefined), 0);
});

test("mediaTime is the inverse of sourceTime", () => {
  assert.equal(core.mediaTime(110, 100), 10);
  assert.equal(core.mediaTime(0, 0), 0);
  // A source time before the session start is a negative media time: the
  // element cannot seek there, and a caller must notice rather than clamp
  // silently, because clamping would hide a wrong offset.
  assert.equal(core.mediaTime(50, 100), -50);
  assert.equal(core.mediaTime(NaN, 100), -100);

  for (const [media, offset] of [[10, 100], [0, 0], [1234.5, 60], [5, 0]]) {
    assert.ok(
      Math.abs(core.mediaTime(core.sourceTime(media, offset), offset) - media) < 1e-9,
      `round trip for ${media} at ${offset}`
    );
  }
});

test("producedWindow shifts the reachable range into source time", () => {
  assert.deepEqual(core.producedWindow({ start: 0, end: 60 }, 100), { from: 100, to: 160 });
  assert.deepEqual(core.producedWindow({ start: 5, end: 30 }, 0), { from: 5, to: 30 });
  assert.deepEqual(core.producedWindow({ start: 5, end: 30 }, undefined), { from: 5, to: 30 });
});

test("producedWindow is null when nothing is reachable", () => {
  // No seekable range yet: the player must not draw a produced band at all
  // rather than draw one from zero to somewhere invented.
  assert.equal(core.producedWindow({ start: 0, end: 0 }, 100), null);
  assert.equal(core.producedWindow({ start: 0, end: NaN }, 100), null);
  assert.equal(core.producedWindow(null, 100), null);
  assert.equal(core.producedWindow(undefined, 100), null);
});

test("subtitleDeliverable needs a URL from the server", () => {
  assert.equal(core.subtitleDeliverable({ index: 2, text: true, url: "/x.vtt" }), true);
  assert.equal(core.subtitleDeliverable({ index: 2, text: true, url: "" }), false);
  assert.equal(core.subtitleDeliverable({ index: 3, text: false }), false);
  assert.equal(core.subtitleDeliverable(null), false);
  assert.equal(core.subtitleDeliverable(undefined), false);
});

test("subtitleNeedsBurn is an image track the server could not read", () => {
  // A PGS track with no URL: the only way to show it is a burn.
  assert.equal(core.subtitleNeedsBurn({ index: 3, text: false }), true);
  // The same track once the server has OCR'd it: a URL means <track>, no burn.
  assert.equal(core.subtitleNeedsBurn({ index: 3, text: false, url: "/3.vtt" }), false);
  // A text track is delivered as a track, URL or not.
  assert.equal(core.subtitleNeedsBurn({ index: 2, text: true }), false);
  assert.equal(core.subtitleNeedsBurn({ index: 2, text: true, url: "/2.vtt" }), false);
  // "Off" is not a track.
  assert.equal(core.subtitleNeedsBurn(null), false);
  assert.equal(core.subtitleNeedsBurn(undefined), false);
});

// ── progress reporting ──────────────────────────────────────────────────────
//
// W-1 of the 2026-10-09 review: stopping or navigating away before the media's
// metadata had loaded reported position 0, and 0 is below the resume threshold,
// so the report call cleared the stored position. A bookmark at 45:00 was
// deleted by pressing Play and changing your mind during the load. The decision
// is now this pure function, so the case is testable without a browser.

test("progressAction says nothing before playback has started", () => {
  // This is the W-1 case: the player does not know its timeline yet, so it must
  // not be read as "the viewer is at the beginning".
  assert.equal(core.progressAction({ started: false, position: 0 }), "skip");
  assert.equal(core.progressAction({ started: false, position: 12 }), "skip");
  assert.equal(core.progressAction({ started: false }), "skip");
  assert.equal(core.progressAction({}), "skip");
  assert.equal(core.progressAction(), "skip");
});

test("progressAction saves a position worth resuming", () => {
  assert.equal(core.progressAction({ started: true, position: 45 * 60, duration: 5400 }), "save");
  assert.equal(
    core.progressAction({ started: true, position: core.RESUME_MIN_SECONDS, duration: 5400 }),
    "save"
  );
});

test("progressAction clears at the very beginning of a loaded timeline", () => {
  // Past the start and below the threshold, with the media loaded: the viewer
  // is at the beginning, and there is nothing to resume.
  assert.equal(core.progressAction({ started: true, position: 0, duration: 5400 }), "clear");
  assert.equal(
    core.progressAction({ started: true, position: core.RESUME_MIN_SECONDS - 0.1, duration: 5400 }),
    "clear"
  );
});

test("progressAction clears a finished position", () => {
  const duration = 1000;
  assert.equal(
    core.progressAction({ started: true, position: duration * core.FINISHED_FRACTION, duration }),
    "clear"
  );
  assert.equal(core.progressAction({ started: true, position: duration - 1, duration }), "clear");
  // Just under the finished fraction is still resumable.
  assert.equal(
    core.progressAction({ started: true, position: duration * core.FINISHED_FRACTION - 1, duration }),
    "save"
  );
});

test("progressAction refuses a position that is not a number", () => {
  assert.equal(core.progressAction({ started: true, position: NaN }), "skip");
  assert.equal(core.progressAction({ started: true, position: Infinity }), "skip");
  assert.equal(core.progressAction({ started: true, position: -1 }), "skip");
  assert.equal(core.progressAction({ started: true, position: "45" }), "save");
});

test("progressAction shares its thresholds with the UI", () => {
  // The numbers are exported so app.js cannot use a different rule from the one
  // this function decides with.
  assert.equal(core.RESUME_MIN_SECONDS, 5);
  assert.equal(core.FINISHED_FRACTION, 0.95);
});

test("resumeOffsetFor refuses anything the report path would call finished", () => {
  // The two used to disagree: this refused only within half a second of the
  // end, while progressAction cleared anything in the last 5%. A position the
  // server has cleared must not be offered back.
  const duration = 1000;
  const finished = duration * core.FINISHED_FRACTION;

  assert.equal(core.resumeOffsetFor({ position_seconds: finished, duration_seconds: duration }, true), 0);
  assert.equal(core.resumeOffsetFor({ position_seconds: duration - 1, duration_seconds: duration }, true), 0);
  assert.equal(core.resumeOffsetFor({ position_seconds: duration, duration_seconds: duration }, true), 0);

  // And a position just short of that is still resumable.
  assert.equal(
    core.resumeOffsetFor({ position_seconds: finished - 1, duration_seconds: duration }, true),
    finished - 1
  );
});

test("resumeOffsetFor resumes a real position", () => {
  assert.equal(
    core.resumeOffsetFor({ position_seconds: 45 * 60, duration_seconds: 5400 }, true),
    45 * 60
  );
});

test("resumeOffsetFor refuses a position that is not worth resuming", () => {
  assert.equal(core.resumeOffsetFor(null, true), 0);
  assert.equal(core.resumeOffsetFor(undefined, true), 0);
  assert.equal(core.resumeOffsetFor({ position_seconds: 2, duration_seconds: 5400 }, true), 0);
  assert.equal(core.resumeOffsetFor({ position_seconds: NaN, duration_seconds: 5400 }, true), 0);
  // A finished flag wins over a plausible position.
  assert.equal(
    core.resumeOffsetFor({ position_seconds: 600, duration_seconds: 5400, finished: true }, true),
    0
  );
});

test("resumeOffsetFor refuses a non-leaf entity", () => {
  // A series or a season has no position of its own; resuming one would start
  // playback on a container.
  assert.equal(core.resumeOffsetFor({ position_seconds: 600, duration_seconds: 5400 }, false), 0);
});

// ── accepted jobs ───────────────────────────────────────────────────────────
//
// W-2: scanning and enriching answer 202 with a job to poll, because holding the
// request open meant the client's own timeout cancelled the work. The waiting is
// a decision - how long, what a failure means, what an inline answer means - so
// it lives here with the clock and the status call injected.

// Accepted stands in for the 202 body the server answers with.
const accepted = { job_id: "job-7", key: "scan:lib-1", state: "queued", new: true };

// statuses builds a status function that returns each state in turn, repeating
// the last one if it is asked again.
function statuses(...states) {
  let index = 0;
  return function () {
    const state = states[Math.min(index, states.length - 1)];
    index++;
    return state;
  };
}

const noSleep = function () {
  return Promise.resolve();
};

test("awaitJob returns the result once the job is done", async () => {
  const outcome = await core.awaitJob(accepted, {
    sleep: noSleep,
    status: statuses(
      { state: "queued" },
      { state: "running" },
      { state: "done", result: { files_seen: 12 } }
    ),
  });

  assert.equal(outcome.outcome, "result");
  assert.deepEqual(outcome.value, { files_seen: 12 });
});

test("awaitJob reports a failed job with the work's own message", async () => {
  const outcome = await core.awaitJob(accepted, {
    sleep: noSleep,
    status: statuses({ state: "done", error: "the library path is gone" }),
  });

  assert.equal(outcome.outcome, "failed");
  assert.equal(outcome.error, "the library path is gone");
});

test("awaitJob passes an inline answer straight through", async () => {
  // A server with no runner answers 200 with the result, and an older server
  // answers 200 with it too. Neither has a job to wait for.
  const inline = { files_seen: 3 };
  const outcome = await core.awaitJob(inline, {
    sleep: noSleep,
    status: function () {
      throw new Error("an inline answer must not be polled");
    },
  });

  assert.equal(outcome.outcome, "inline");
  assert.deepEqual(outcome.value, inline);
});

test("awaitJob reports progress while it waits", async () => {
  const seen = [];
  const outcome = await core.awaitJob(accepted, {
    sleep: noSleep,
    status: statuses({ state: "running" }, { state: "done", result: "ok" }),
    onProgress: function (job) {
      seen.push(job.state);
    },
  });

  assert.equal(outcome.outcome, "result");
  assert.deepEqual(seen, ["running", "done"]);
});

test("awaitJob gives up on a job that never finishes", async () => {
  // A clock the test controls: each call advances a minute, so the deadline is
  // reached without waiting for it.
  let clock = 0;
  const outcome = await core.awaitJob(accepted, {
    sleep: noSleep,
    status: statuses({ state: "running" }),
    waitMs: 1000,
    now: function () {
      clock += 600;
      return clock;
    },
  });

  assert.equal(outcome.outcome, "timeout");
});

test("awaitJob keeps waiting while the job has not finished", async () => {
  let polls = 0;
  const outcome = await core.awaitJob(accepted, {
    sleep: noSleep,
    status: function () {
      polls++;
      // Not done for the first three polls.
      if (polls < 4) return { state: "running" };
      return { state: "done", result: "finished" };
    },
  });

  assert.equal(outcome.outcome, "result");
  assert.equal(outcome.value, "finished");
  assert.equal(polls, 4);
});

test("awaitJob tolerates a status call that returns nothing", async () => {
  let polls = 0;
  const outcome = await core.awaitJob(accepted, {
    sleep: noSleep,
    status: function () {
      polls++;
      if (polls === 1) return null; // a 404 for a job forgotten mid-poll
      return { state: "done", result: "ok" };
    },
  });

  assert.equal(outcome.outcome, "result");
  assert.equal(outcome.value, "ok");
});

test("awaitJob requires the two injected functions when there is a job", async () => {
  await assert.rejects(function () {
    return core.awaitJob(accepted, {});
  }, /needs sleep and status/);
});

// ── the playback engine ─────────────────────────────────────────────────────
//
// W-5: the player compared `pb.engine === "native"` while native HLS sets
// `"native-hls"`, so a Safari user was told they were watching "the bundled
// hls.js player over MSE". Comparing against a string that is never produced
// fails silently, which is why the names and the wording live here.

test("engineLabel names native HLS as native, not as hls.js", () => {
  assert.match(core.engineLabel(core.ENGINE_NATIVE_HLS), /native HLS/);
  assert.doesNotMatch(core.engineLabel(core.ENGINE_NATIVE_HLS), /hls\.js/);
});

test("engineLabel names each engine the player actually sets", () => {
  // The values have to match what app.js assigns, or the label is decoration.
  assert.equal(core.ENGINE_NATIVE, "native");
  assert.equal(core.ENGINE_NATIVE_HLS, "native-hls");
  assert.equal(core.ENGINE_HLS_JS, "hls.js");

  assert.match(core.engineLabel(core.ENGINE_NATIVE), /native/);
  assert.match(core.engineLabel(core.ENGINE_HLS_JS), /hls\.js/);
});

test("engineLabel passes an unknown engine through rather than guessing", () => {
  // Saying "the bundled hls.js player" about something else is worse than
  // saying what the server said.
  assert.equal(core.engineLabel("some-future-engine"), "some-future-engine");
  assert.doesNotMatch(core.engineLabel("some-future-engine"), /hls\.js/);
  // And a missing value is not an engine.
  assert.equal(core.engineLabel(null), "an unknown engine");
  assert.equal(core.engineLabel(""), "an unknown engine");
});

// ── which subtitle tracks can be chosen ─────────────────────────────────────
//
// W-6: a track the player could not honour was offered as a working radio, and
// choosing it repainted the control and then returned without a word. The rule
// lives here so the menu and the selection path cannot disagree about it.

test("a text track with a URL is selectable", () => {
  assert.equal(
    core.subtitleSelectable({ index: 2, text: true, url: "/api/objects/x/subtitles/2.vtt" }),
    true
  );
});

test("a text track with no URL is not selectable", () => {
  // The player toggles a <track> the element already owns; with no URL there is
  // nothing to toggle, and the click did nothing.
  assert.equal(core.subtitleSelectable({ index: 2, text: true }), false);
  assert.equal(core.subtitleSelectable({ index: 2, text: true, url: "" }), false);
});

test("an image track is selectable when it can be burned", () => {
  assert.equal(core.subtitleSelectable({ index: 3, text: false, codec: "hdmv_pgs_subtitle" }), true);
});

test("an image track with no usable index is not selectable", () => {
  assert.equal(core.subtitleSelectable({ index: 0, text: false }), false);
  assert.equal(core.subtitleSelectable({ text: false }), false);
  assert.equal(core.subtitleSelectable(null), false);
});

test("nothing that is unselectable may be shown as checked", () => {
  // The invariant the bug violated. Whatever the stored preference says, a
  // track the menu cannot honour must not render as the current choice.
  const unselectable = [
    { index: 2, text: true },
    { index: 0, text: false },
    null,
  ];
  for (const track of unselectable) {
    assert.equal(core.subtitleSelectable(track), false);
    // And the menu's rule follows from it: checked requires selectable.
    const checked = core.subtitleSelectable(track) && true;
    assert.equal(checked, false);
  }
});

// ── the transcode a stale negotiation leaves behind ─────────────────────────
//
// W-4: navigating during a negotiation discarded the response and its
// session_id, but the server had already started ffmpeg for it. That transcode
// ran until the idle reaper collected it, so clicking through titles stacked
// them on a host that had been asked for one at a time.

test("a superseded negotiation's session is released", () => {
  const abandoned = { title: "Dune" };
  const current = { title: "Arrival" };
  assert.equal(
    core.orphanedSessionId({ session_id: "sess-orphan" }, current, abandoned),
    "sess-orphan"
  );
});

test("a session still on screen is not released", () => {
  // The response belongs to the session being handled, so there is nothing
  // stale about it and the caller takes the normal path.
  const session = { title: "Dune" };
  assert.equal(core.orphanedSessionId({ session_id: "sess-live" }, session, session), null);
});

test("a response with no session id releases nothing", () => {
  // Direct play has no server session, so there is nothing to stop.
  const abandoned = { title: "Dune" };
  const current = { title: "Arrival" };
  assert.equal(core.orphanedSessionId({}, current, abandoned), null);
  assert.equal(core.orphanedSessionId(null, current, abandoned), null);
  assert.equal(core.orphanedSessionId({ session_id: "" }, current, abandoned), null);
});

test("an id already released is not released again", () => {
  // `resumeSession` releases the previous id before it renegotiates, and the
  // response can name that same id again. A second DELETE for it would stop
  // whatever is now using it.
  const renegotiating = { title: "Dune", sessionId: "sess-same" };
  const current = { title: "Dune" }; // a new session object for the same title
  assert.equal(
    core.orphanedSessionId({ session_id: "sess-same" }, current, renegotiating),
    null
  );
  // A different id from the same session is still released.
  assert.equal(
    core.orphanedSessionId({ session_id: "sess-new" }, current, renegotiating),
    "sess-new"
  );
});

// ── recovering from a reaped session ────────────────────────────────────────
//
// S-10: a fast remux finishes producing, hls.js stops polling, the server reaps
// the idle session, and the player reports "Recovery was not possible" under a
// viewer who is still watching. The TTL is longer and configurable now, but a
// session can still expire, and the one thing that recovers it is a new session
// at the current position rather than a retry against a playlist that is gone.

test("a failure with a position to resume from is renegotiated", () => {
  assert.equal(
    core.shouldRenegotiateAfterFailure({ sessionCurrent: true, position: 754.5 }),
    true
  );
  // A position of zero is still a position: restarting from the beginning is a
  // better answer than a dead player.
  assert.equal(core.shouldRenegotiateAfterFailure({ sessionCurrent: true, position: 0 }), true);
});

test("renegotiation happens once, not in a loop", () => {
  // A server that keeps refusing is a real failure, and retrying forever against
  // it is a client bug rather than a recovery.
  assert.equal(
    core.shouldRenegotiateAfterFailure({
      sessionCurrent: true,
      position: 100,
      alreadyRenegotiated: true,
    }),
    false
  );
});

test("a failure belonging to a replaced session is not renegotiated", () => {
  // The viewer navigated away while the stream was breaking. Starting a new
  // session would play something they are no longer looking at.
  assert.equal(
    core.shouldRenegotiateAfterFailure({ sessionCurrent: false, position: 100 }),
    false
  );
});

test("a failure with no usable position is not renegotiated", () => {
  assert.equal(core.shouldRenegotiateAfterFailure({ sessionCurrent: true }), false);
  assert.equal(
    core.shouldRenegotiateAfterFailure({ sessionCurrent: true, position: NaN }),
    false
  );
  assert.equal(
    core.shouldRenegotiateAfterFailure({ sessionCurrent: true, position: -1 }),
    false
  );
});

// ── what a failed fetch means ───────────────────────────────────────────────
//
// W-9: the API timeout was cleared as soon as the headers arrived, so a response
// body that stalled was covered by nothing and the UI could spin forever - while
// the module header claimed every fetch had a timeout. The classifier lives here
// so both stages of a fetch report a timeout the same way, and so the case that
// used to be reported as a network error can be tested.

test("an abort means the timeout, wherever it fired", () => {
  const aborted = { name: "AbortError" };
  assert.equal(core.fetchFailure("send", aborted, 20000).kind, "timeout");
  // The stage that was previously uncovered.
  assert.equal(core.fetchFailure("body", aborted, 20000).kind, "timeout");
});

test("a body failure that is not a timeout is not reported as one", () => {
  // The status code the caller already has is a better answer than "network
  // error", so a body that failed for another reason is its own kind.
  const ended = { name: "TypeError", message: "network error" };
  assert.equal(core.fetchFailure("body", ended, 20000).kind, "body");
  assert.equal(core.fetchFailure("body", null, 20000).kind, "body");
});

test("a send failure that is not a timeout is a network failure", () => {
  assert.equal(core.fetchFailure("send", { name: "TypeError" }, 20000).kind, "network");
  assert.equal(core.fetchFailure("send", null, 20000).kind, "network");
});

test("the timeout message says how long it waited", () => {
  assert.match(core.timeoutMessage(20000), /20 seconds/);
  assert.match(core.timeoutMessage(1500), /2 seconds/);
});

// ── a stale entity list must not be written ─────────────────────────────────
//
// W-7: the load token was checked by the caller *after* the helper had written
// the list into shared state, so a slow library's answer arriving after the
// viewer moved on had already overwritten the list on screen. W-8: clearing the
// list without clearing its library id made a library nobody had asked about
// render as "0 entities".

test("a list from a superseded route is not current", () => {
  assert.equal(core.entityListIsCurrent(3, 4), false);
  assert.equal(core.entityListIsCurrent(4, 4), true);
});

test("a caller with no token writes unconditionally", () => {
  // A scan refreshing the list it just scanned is the only writer in its flow,
  // so there is nothing for it to be stale against. Without this, every such
  // caller would silently stop loading entities - which is the bug the first
  // version of this introduced.
  assert.equal(core.entityListIsCurrent(undefined, 7), true);
  assert.equal(core.entityListIsCurrent(null, 7), true);
});

test("clearing the entity state clears both fields", () => {
  const cleared = core.clearedEntityList();
  assert.deepEqual(cleared.entities, []);
  assert.equal(cleared.entitiesLibraryId, null);
  // The invariant: no list, no library - a library id pointing at nothing is
  // what made the next view claim an empty library.
  assert.ok(!cleared.entitiesLibraryId);
});
