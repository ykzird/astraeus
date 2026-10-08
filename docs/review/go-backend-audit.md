# Adversarial review — astraeus-media Go backend

> **Point in time.** This is the report as delivered, against commit `aad2cb1`. Line
> numbers and type names refer to the tree at that commit and have since moved:
> the metadata provider types are now `internal/metadata` (`Provider`, `Chain`,
> `Mock`, `TMDB`), and the SQLite repository is `internal/library/sqlite`.
> For what has been fixed since, and what has not, see
> [`../adversarial-review.md`](../adversarial-review.md). The body below is
> deliberately unedited.


Scope: all production `*.go` under `cmd/` and `internal/` (~6.4k lines), plus `go.mod`; `web/` excluded.
Method: full read; `mise exec -- go vet ./...` (clean); `mise exec -- go test -tags=integration -race -count=1 ./...` (all green);
throwaway tests written **in a scratch copy** of the repo (original untouched) to confirm or falsify suspicions.
Confidence labels are explicit: **[verified]** = reproduced with a test or measured; **[read]** = established by careful reading, no runtime check.

---

## Critical

### C1. SQLite is opened with no busy timeout, no WAL, and default `synchronous` — concurrent writes fail and scans are ~20× slower than they need to be
`cmd/astraeus-server/main.go:149` (`sqlx.Connect("sqlite", c.dbPath)`, plain path, no `_pragma`).

modernc.org/sqlite defaults `busy_timeout=0`, `journal_mode=delete`, `foreign_keys=0` unless the DSN says otherwise
(verified in the driver's own `dsn_test.go`: `verifyPragma(t, "", "busy_timeout", "0")`).
`database/sql` opens as many connections as it likes. The server therefore has no SQLite-level serialisation: the scan
scheduler (`main.go:273`), the metadata worker (`main.go:266`), and every HTTP write share the file with a zero-length
lock wait.

Why it matters — this is not theoretical:

* **[verified]** 4 concurrent `ScanLibrary` calls on the shipped DSN, 3 of 4 failed with
  `commit transaction: database is locked (5) (SQLITE_BUSY)` / `creating entity "Alien": database is locked (5)`.
  With `?_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)` the same test passed with 0 errors.
* **[verified]** Scan throughput, 2000 small files: **45.4 s** on the shipped DSN vs **2.1 s** with
  `journal_mode(WAL)&synchronous(NORMAL)` — a 21× difference caused purely by the per-file commit fsync of the
  rollback journal (`Scanner.ingestFile` opens one transaction per file, `internal/library/scanner.go:168`).
* The failure mode is load-dependent, so it will appear in production (two admins scanning, scan overlapping enrich)
  and never in CI.

Fix (one line, biggest single win in the codebase):
```go
dsn := c.dbPath + "?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(1)"
db, err := sqlx.Connect("sqlite", dsn)
```
Plus `db.SetMaxOpenConns(1)` **or** keep several readers and accept WAL semantics. Note `db.SetMaxOpenConns(1)` alone
would also fix the contention but not the throughput. Note also `internal/api/server_test.go:60` copies the same
`sqlx.Connect` call, so the test suite is structurally unable to catch this.

### C2. No cap on streaming sessions — any authenticated client can exhaust processes, CPU and disk
`internal/streaming/hls.go:280-377` (`startOnce` registers a session and spawns ffmpeg unconditionally);
`internal/api/server.go:564`.

There is no `maxSessions`, no per-client limit, and no rejection path. Each `POST /api/entities/{id}/playback` that
negotiates remux/transcode leaves a live `ffmpeg` until the reaper runs (default TTL 2 min, reaper interval
`TTL/2 = 1 min`, `hls.go:484-502`). A loop of 100 requests against one movie starts 100 ffmpeg processes that each
write segments to disk. The API is gated instance-wide, so "authenticated client" here means *any* browser that got
through `--auth-mode none` (the default) or proxy/token. There is no rate limiting (admitted in `SPECIFICATION.md:234`),
so nothing else bounds this either.

Fix: a configurable `--max-stream-sessions` (default e.g. 4–8) checked in `Manager.startOnce` before
`cmd.Start()`; return a typed `ErrTooManySessions` and map it to `429`/`503` in `handlePlayback`. Optionally count per
`entityID` so one film cannot be transcoded twice.

---

## High

### H1. `FindEntity` cannot use the identity index — scanning is superlinear, and the index that exists is dead weight
`internal/library/sqlite_repository.go:479-482` vs the index created at `:159-160`.

Query: `WHERE library_id = ? AND parent_id IS ? AND type = ? AND name = ?`
Index: `(library_id, COALESCE(parent_id, ''), type, name)`

`parent_id IS ?` is a different expression from `COALESCE(parent_id,'')`, so SQLite can only use column 1.
**[verified]** `EXPLAIN QUERY PLAN`:
`SEARCH media_entities USING INDEX idx_media_entities_library (library_id=?)` — the parent/type/name filters are
applied in memory over *every entity in the library*, once per file (2–4 lookups per episode file).
`idx_media_entities_identity` is never chosen by any query in the repository. **[verified]** scaling with the query plan
held constant (WAL): 500 files = 294 ms, 2000 files = 2.13 s — 4× the files, 7.2× the time.

Fix: make the query match the expression (`WHERE library_id=? AND COALESCE(parent_id,'')=? AND type=? AND name=?`, binding
`""` for top level), or add `CREATE INDEX ... ON media_entities(library_id, parent_id, type, name)` and keep the
`COALESCE` index only for uniqueness. Then batch transactions (C1) and the 2000-file scan should land under a second.

### H2. Two overlapping libraries silently steal each other's `media_objects`
`internal/library/scanner.go:202-212`; global unique index `idx_media_objects_path` at `sqlite_repository.go:158`.

`media_objects.file_path` is unique across the whole database, and `ingestFile` *reassigns* an existing object to
whichever leaf entity the current library derived (`existing.MediaEntityID = leafEntity.ID`). If two libraries overlap
(e.g. `--kind movies /media` and `--kind shows /media/Shows`, or a typo'd path), the file's object ping-pongs between
the Movie entity and the Episode entity on every scan; whichever library `ScanAll` visits last owns it.

**[verified]** with `movies=/media`, `shows=/media/Shows` and one `Breaking Bad S01E01` file: after one `ScanAll` there are
4 entities and the Movie entity has **0 objects**, so `POST /api/entities/{movieID}/playback` returns `400 no_media`
forever (`server.go:486-490`). `ListLibraries` orders by name, so the loser is deterministic, not random.

Fix: include the library in the object identity (unique index on `(library_id, file_path)` via a denormalised column
or a join), or refuse to register a library whose path is nested inside / contains an existing one
(`handleCreateLibrary`, `main.go`). Repointing an existing object to a different entity should be a deliberate repair,
not a scan side effect.

### H3. Deleted files are never pruned — the library only ever grows
`internal/library/scanner.go:52-113` (`ScanLibrary` reconciles files it *finds*); no `DELETE` of
`media_entities`/`media_objects` exists outside `DeleteLibrary` and `Migrate`'s one-off dedup.

A renamed or deleted film stays in the DB forever: it is listed in `/api/entities`, its entity is counted
`Incomplete`, the metadata worker will (for a movie) happily fetch metadata for a title that no longer exists, and
playing it 404s at `server.go:587-591`. The `ScanResult` even reports `FilesSeen` without any notion of removals, so the
administrator cannot see what happened.

Fix: during a scan, record the set of `media_objects.file_path` seen for the library and, after a complete successful
walk, delete objects (and leaf entities with no objects) that were not seen **only when the scan was not aborted**
(honour `ctx.Err()`; the current walk returns early on cancellation, `scanner.go:69-71`, which must not be taken as
"the file is gone"). Add counters to `ScanResult` and a test for delete-then-rescan.

### H4. Unvalidated client codec names reach ffmpeg; negotiation claims "deliverable" for codecs the host cannot encode
`internal/streaming/negotiate.go:398-425`, `internal/streaming/hls.go:592-596`, `internal/api/server.go:554-568`.

Two related defects, both **[verified]**:

1. `PreferredAudioCodec()` falls back to `available[0]` for a codec outside the preference list, and
   `BuildFFmpegArgsAt` passes it straight to the command line: a manifest with `"audio_codecs":["totally-bogus"]` and a
   DTS source produced `... -c:v copy -c:a totally-bogus -b:a 192k ...` with `err == nil`. This is **not** shell
   injection (one argv element; no way to add flags), but it is unvalidated input driving a subprocess, and it fails at
   runtime with a `500 stream_start_failed` instead of a 4xx.
2. `EncoderFor` checks `ServerCapability.VideoEncoders` only for *hardware* encoders; software candidates are returned
   unconditionally. With an empty `ServerCapability{}` it returns `libx264` / `libx265` / `libvpx-vp9` / `libsvtav1`
   regardless of what the host's ffmpeg has. A client declaring only `vp9`, or an unknown `mpeg4`, gets
   `Deliverable=true, mode=transcode`, and then `BuildFFmpegArgsAt` errors — a 500 for a client error.

Note the test suite tests exactly the safe half: `negotiate_test.go:307-316` asserts an unknown *video* codec errors,
and there is no equivalent for audio.

Fix: validate the target codecs against `ServerCapability.VideoEncoders`/the audio allowlist in `Negotiate` (or at the
top of `BuildFFmpegArgsAt`), fall back to an encoder the host actually has, and if none exists return a typeable error
that `handlePlayback` maps to `409 not_deliverable` / `415`. Never pass an unvalidated string to `-c:a`/`-c:v`.

### H5. `http.Server` has no read/idle/write timeouts
`cmd/astraeus-server/main.go:355-359` sets only `ReadHeaderTimeout: 10s`.

Every other timeout is absent, so a client can hold a connection open indefinitely by dribbling a body (the
`MaxBytesReader` bounds size, not time), and idle keep-alive connections are never reaped. With C2 (unbounded sessions)
this compounds. Fix: add `ReadTimeout`, `WriteTimeout` (careful: a long transcoded response is not long — segments are
small; `WriteTimeout` of e.g. 60 s is fine), and `IdleTimeout: 120s`.

---

## Medium

### M1. A movie folder whose name contains a period before the year is mis-parsed
`internal/library/naming.go:158-166` (`ParseMovieName` strips `filepath.Ext`), used from `scanner.go:265-292`.

`filepath.Ext("Dr. No (1962)")` is `". No (1962)"`, so the title becomes `"Dr"` and the year is lost; the folder then
loses to the file name (or, worse, the file name has no title). **[verified]**
`PlacementFor(movies, "Dr. No (1962)/1080p.mkv", …)` → title `"1080p"`, `Meta == nil`. The control case
`"Blade Runner 2049 (2017)/1080p.mkv"` → `"Blade Runner 2049"` + year 2017. Affects "Dr. No", "Mr. & Mrs. Smith",
"WALL·E" (no dot) etc. Fix: only strip a *known video extension* (`IsVideoFile`/`filepath.Ext` ∈ `VideoExtensions`)
rather than any suffix after the last dot.

### M2. Library paths are entirely client-controlled, and `os.Stat` errors disclose the filesystem
`internal/api/server.go:274-282`.

`POST /api/libraries` takes any `path`, `os.Stat`s it and echoes the raw error
(`"path is not readable: stat /etc/shadow: permission denied"` vs `"path is not a directory"` vs success). An
authenticated caller can therefore (a) probe the host filesystem, and (b) register `/` as a movies library, scan it, and
direct-play every video file it contains. Under the documented single-gate model every authenticated principal is an
administrator, so this is a design decision rather than a hole — but it is undocumented, and it is the mechanism by which
H2 (overlapping libraries) happens. Fix at minimum: return a generic message for stat failures, and optionally confine
paths to a configured root (`--library-root`) or refuse `/`, `$HOME` and paths containing another registered library.

### M3. `--stream-root` is swept destructively: *any* subdirectory older than the TTL is deleted at startup
`internal/streaming/hls.go:189-211`.

No name/marker check: every directory under the root whose mtime is older than `SessionTTL` (2 min) is `os.RemoveAll`'d.
`--stream-root /var/lib/astraeus` or `--stream-root /srv` silently deletes unrelated data on the next start. The existing
test endorses this by creating `stale-session`/`fresh-session` and expecting the old one removed
(`hls_test.go:22-60`). Fix: require a marker file (e.g. `.astraeus-stream-root`) created by `NewManager`, refuse to sweep
a non-empty root without it, and/or delete only names matching the session-id shape.

### M4. List endpoints have no pagination and unmarshal metadata JSON for every row on every request
`internal/api/server.go:373-380`, `internal/api/server.go:356-371`, `internal/library/sqlite_repository.go:492-517`.

`GET /api/entities` scans the whole table, JSON-decodes every entity's metadata, builds a second slice of decorated
resources (`decorateEntities`, `server.go:653-659`), and serialises it. There is no `limit`/`offset`/cursor, no
`ETag`, and no `Cache-Control`, so a large library means a large response per navigation. Fix: add `?limit=&offset=` (or
a `since` cursor) with a hard cap, and consider `items`+`total` envelope. Low effort, prevents a future cliff.

### M5. Errors leak internal paths and ffmpeg/ffprobe diagnostics to clients
`internal/api/server.go:520-521` (`"the media file could not be inspected: "+err.Error()`),
`internal/api/server.go:567` (`stream_start_failed` with the raw error, which for HLS includes
`ffmpeg stopped before producing a playlist: <stderr>` and the input path, `hls.go:410-414`),
`internal/api/server.go:771` (`subtitle_extraction_failed` with the raw ffmpeg error).

The log line already carries the detail (`server.go:518`, `566`, `769`); the response should not. Fix: return a stable
message plus a correlation id; keep the raw error in the log only.

### M6. `handleScanLibrary` / `handleScanAll` run the whole scan on the request goroutine, and a client disconnect aborts it
`internal/api/server.go:324-354`; cancellation is checked per file (`scanner.go:69-71`).

For a large library this is a multi-minute request with no timeout, and the lifecycle is tied to a browser tab. A
disconnect leaves the library half-scanned with no record beyond a log line. Fix: run scans as a background job with a
job id and a `GET /api/scans/{id}` status endpoint (or at least decouple the scan from the request context and log
completion/failure), and serialise scans per library so the periodic pass and the manual pass cannot overlap.

### M7. Dead/unused configuration and caching API
* `ClientCapability.MaxBitrateKbps` is documented as "caps the acceptable bitrate" (`capability.go:25-26`), is set to
  120 Mbps in `BrowserCapability`, is validated (`:79-81`) and is normalised (`:94`) — and is never read by `Negotiate`
  even though `MediaInfo.BitrateKbps` exists (`probe.go:71,212-215`). Either implement the axis (add a reason and force a
  transcode/remux) or delete the field.
* `CachingProber.Invalidate` (`probe_cache.go:44-48`) has no production caller. The cache is keyed by path and never
  invalidated, so a file replaced in place keeps its old codec/resolution until restart (`TODO.md` acknowledges this).
  Wire `Invalidate` into the scanner when `size`/`mtime` changes, or delete the method and the test that covers it.

### M8. Documentation contradicts the code for an omitted `max_bit_depth`
`internal/streaming/capability.go:27-31` and `README.md:252` ("An omitted limit means **unrestricted**"),
`TODO.md` "Known gaps" (same claim); code at `negotiate.go:116-117` treats `MaxBitDepth == 0` as "at most 8-bit"
(any >8-bit source is transcoded), while `MaxAudioChannels == 0` *is* treated as unrestricted (`negotiate.go:106-107`).

The code's behaviour is the safe one for a browser, but two identically-documented fields behave oppositely and both
docs are wrong. Fix the docs (and `TODO.md`), or, if 0 really should mean unrestricted, gate 10-bit on
`MaxBitDepth > 0`.

### M9. The metrics exposition holds the metrics lock across the network write
`internal/observability/metrics.go:180-197`: `WriteTo` takes `m.mu.RLock`, builds the body, and calls
`io.WriteString(w, …)` before the deferred unlock.

A slow Prometheus scrape therefore blocks every `IncCounter`/`ObserveHistogram` in the process for its duration
(including the post-handler request logging in `server.go:191-197`). Fix: build the string under the lock and write it
after releasing it.

### M10. Metrics cardinality is driven by a client-controlled label
`internal/library/library_scanner.go:114-119,125` labels `astraeus_scan_seconds`/`astraeus_scan_runs_total` with the
library *name*, which is arbitrary client input at `POST /api/libraries`. Creating many libraries (and triggering a
scan) grows the metrics maps without bound for the process lifetime. Low severity given a global gate, but the fix is
cheap: label by library *kind*, or by a bounded id set, and keep the name in logs.

---

## Low

* **L1 `AddCounter` panics on a negative delta** (`metrics.go:90-94`). Not reachable today, but a panic inside a request
  path or a background worker is a process-killer; prefer clamping to 0 and logging, or `IncCounter` only.
* **L2 No `WaitDelay` on the other subprocesses.** `hls.go:322` sets it (correctly — `cmd.Stderr` is a non-file writer,
  so `exec` uses a pipe and a copy goroutine), but `probe.go:138-148`, `subtitles/extractor.go:119-128` and
  `negotiate.go:300/355` do not. If a killed child ever leaves a grandchild holding the inherited pipe, `Run()` waits
  past the context deadline. Add `cmd.WaitDelay = 5 * time.Second` in all four places.
* **L3 FK constraints are inert.** `FOREIGN KEY` clauses exist (`sqlite_repository.go:97-108`) but `PRAGMA
  foreign_keys` is never enabled, so orphan `parent_id`/`media_entity_id` rows are possible. Enable it in the DSN (see
  C1) after checking that existing data satisfies it.
* **L4 `Migrate` re-runs the full-table dedup and status repair on every start** (`sqlite_repository.go:139-152`). A
  full scan of `media_objects` at every boot; make it conditional on a migration version rather than unconditional.
* **L5 `--trusted-proxy 0.0.0.0/0` (or `::/0`) silently disables the identity check.** `access/gate.go:294-315` accepts
  any prefix and `New` only checks for "at least one". Refuse (or loudly warn about) prefixes with `Bits() == 0`.
* **L6 `decodeJSON` accepts trailing garbage** (`server.go:825-841`): a second `Decode` is never attempted, so
  `{...}{...}` is accepted. Cosmetic, but it also means a truncated/multi-part body is silently ignored.
* **L7 Unknown `?status=` values return `200 []`** (`server.go:812-823`) while an unknown library is a `404`. A typo'd
  filter looks like "no data"; reject unknown statuses with `400`.
* **L8 `GET /api/health` does not touch the database** (`server.go:230-232`), so it is liveness only — a server whose DB
  is gone still reports `ok`. Either document it as liveness or add `GET /api/ready`.
* **L9 `api.NewServer` panics for a nil `Scanner`/`Worker`** on the scan/enrich routes (`server.go:331`, `430`), while
  `Scheduler`, `Prober`, `Streams`, `Images`, `Metrics` and `Subtitles` are all nil-guarded. `main` always sets them, so
  this is latent, but the inconsistency is a trap for tests/embedders.
* **L10 The image proxy caches whatever the upstream returns** (`images/proxy.go:166-220`): no `Content-Type` check, so a
  200 HTML error page from a misconfigured `--tmdb-image-base` is cached and then served as `image/jpeg` with
  `Cache-Control: immutable` (`proxy.go:158`). Require an `image/*` content type before committing the cache file.
* **L11 `handleCreateLibrary` is check-then-insert** (`server.go:284-302`): two concurrent creates for the same path
  produce a `UNIQUE constraint failed` → `500 internal_error` instead of `409`. Map the constraint error (or rely on
  `INSERT ... ON CONFLICT`).
* **L12 Two `MetadataWorker` instances are constructed** (`main.go:175` for the API, `main.go:263-267` for the periodic
  pass, with a different interval). It works, but the object the API reports through is not the one that actually runs.
  Create one and pass the same pointer to both.

---

## Testing: what I would change

**Verified problems**

* **Tautological test**: `internal/images/proxy_test.go:390-398` — `if !errors.Is(ErrInvalidRequest, ErrInvalidRequest) || !errors.Is(ErrUpstream, ErrUpstream)`.
  `errors.Is(x, x)` is true by construction for a non-nil sentinel; this test cannot fail unless the vars are nil. It
  asserts nothing about the feature (error distinguishability for *callers*).
* **Test for dead code**: `internal/streaming/probe_cache_test.go:92-110` exercises `Invalidate`, which nothing in
  production calls (M7). Green coverage of a feature that does not exist in the running system.
* **Test that passes because the hazard is not exercised**: `internal/api/server_test.go:60` builds the DB exactly as
  production does (no pragmas), and there is **no concurrency test anywhere** — that is why C1 survived a green
  `-race` suite.
* **Partial-file flakiness**: `internal/streaming/integration_test.go:291-310` `waitForSegment` returns the first `.ts`
  with `Size() > 0` and callers immediately `ffprobe` it (`:222`, `:356`, `:412`). ffmpeg writes segments in place, so a
  partially written segment can be probed — a race that will flake on a loaded machine.
* **Temp-dir leak**: `internal/api/subtitle_test.go:23-29` uses `os.MkdirTemp("", …)` with no cleanup (should be
  `t.TempDir()`); every run leaves a directory behind.
* **Wrong comment in a test**: `integration_test.go:186-193` says "A client that cannot decode H.264 forces a real video
  transcode" while declaring `VideoCodecs: []string{"h264"}`; the actual trigger is `MaxHeight: 120`. The comment will
  mislead the next person to touch the test.
* **Assertions on implementation detail** (`negotiate_test.go:245-359`, exact argv substrings) are defensible here —
  the command line *is* the contract with ffmpeg — but they are brittle: any flag reordering fails the test without a
  behaviour change. Prefer a small argv-parsing helper.

**Highest-value tests to add** (in order)

1. Concurrency test: N goroutines scanning one library and one reader hitting `/api/entities` on the shipped DB setup —
   would have caught C1.
2. Session-cap test: `Manager.startOnce` past the limit must be refused (C2).
3. Deleted-file test: scan, delete a file, rescan, assert the entity/object are gone and the counters report it (H3).
4. Overlapping-library test: two libraries over one file must not steal the object / must be refused (H2).
5. `Range` request test on `/api/objects/{id}/file` (`bytes=0-3`, `bytes=-3`, out-of-range → 416): today the README
   claim at line 36 and the TODO claim are only guaranteed by `http.ServeFile`, with no regression test.
6. Gate + API integration: one test that wraps `api.NewServer(...).Handler()` in `gate.Middleware` and asserts an
   unauthenticated `/api/entities` is refused while `/api/health` passes — currently the two packages are tested only in
   isolation.

---

## What is genuinely solid — do not touch

* **`internal/access`**: the trust model is exactly right. Identity headers are believed only from a configured trusted
  address, `clientAddr` uses the connection peer and `.Unmap()`s IPv4-mapped IPv6, `X-Forwarded-For`/`X-Real-IP` are
  deliberately ignored (with a test proving it), the token uses `subtle.ConstantTimeCompare`, exempt paths are exact
  matches, and `New` fails closed. The test table covers forged headers, untrusted sources, IPv6, blank identities and
  unparseable addresses. The only gap is L5 (a `0.0.0.0/0` prefix).
* **Path-traversal surface**: every client-influenced path is allow-listed (`images.fileNameRe` + size map,
  `NamedSegmentRe`, integer subtitle index) and file paths that reach `os.Stat`/`ServeFile` come from the database, not
  the request. Command construction for ffmpeg is argv-based with typed/numeric values; I could not construct an
  injection (see H4 for the *unvalidated value* issue, which is not injection).
* **ffmpeg session lifecycle**: `exec.CommandContext` on a context derived from the server context (not the request) so a
  session outlives its request; `cmd.Stderr` assigned a bounded `stderrCollector` (8 KiB) which makes `Wait` collect all
  output; `cmd.WaitDelay = 5s`; `Stop` cancels and then waits on `done` so the process and directory cannot outlive the
  session; `ReapLoop` reaps idle sessions and drains on shutdown; `sweepStaleDirectories` handles crash leftovers
  (modulo M3). The "hardware listed but unable to open a session → retry in software" path is a genuinely good design
  and is tested with a stub ffmpeg.
* **`internal/observability`**: a clean, correct hand-rolled exposition — cumulative histogram buckets, sorted output,
  correct label escaping, nil-receiver safety on every method, and a KPI registry declared at startup so an idle server
  still exposes every metric.
* **`internal/images` and `internal/subtitles` caches**: atomic temp-file + rename, size cap read one byte past the
  limit, no cache write on failure, and the subtitle cache key includes size *and* mtime so a replaced file is
  re-extracted. Both are tested at the behaviour level (cache hit, oversized, upstream failure, restart survival).
* **Repository/migration design**: parameterised queries only, per-file transaction with rollback
  (`TestWithTx_RollsBackOnError`), NULL-safe `parent_id IS ?` lookups, legacy timestamp layouts, and a genuinely
  thoughtful legacy upgrade path (column addition, name backfill, status repair, dedup before the unique index is
  created).
* **Negotiation** is a pure function with an explicit reason per axis, and the 10-bit/stereo/bitrate-of-thought detail
  (Chromium refusing High-10 and 5.1 AAC) is real domain knowledge encoded as tests.

---

## Top 5 things to fix first

| # | Fix | Effort | Payoff |
| - | --- | ------ | ------ |
| 1 | **C1** — SQLite DSN pragmas (`busy_timeout`, `journal_mode=WAL`, `synchronous=NORMAL`); optionally `SetMaxOpenConns` | ~30 min + a concurrency test | Removes `SQLITE_BUSY` failures under normal concurrency; 2000-file scan 45 s → 2 s (verified 21×) |
| 2 | **H3** — prune objects/entities for files that disappeared, guarded against aborted scans | ~half a day + tests | Stops the library and the metadata queue from growing forever; removes the "ghost film" class of bugs |
| 3 | **H4** — validate/limit target codecs against `ServerCapability`, never pass an unvalidated string to `-c:a`/`-c:v`, map "no encoder" to 4xx | ~2 h + tests | Turns a client error (500) into a correct refusal and removes unvalidated input from the subprocess |
| 4 | **C2** — `--max-stream-sessions` cap with a typed error → 429/503 | ~2 h + test | Closes the easiest remote resource-exhaustion path (ffmpeg processes + disk) |
| 5 | **H1** — align the `FindEntity` query with the identity index (or add the matching index) | ~1 h + EXPLAIN/perf test | Removes the remaining superlinear term in scanning; makes re-scans of large libraries cheap |

Runner-up if you have an hour spare: **M3** (marker file for `--stream-root`) — it is the only finding here that can
delete user data.
