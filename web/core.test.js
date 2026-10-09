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
