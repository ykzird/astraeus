// Unit tests for the front end's pure core. Run with:
//
//	node --test web/
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
