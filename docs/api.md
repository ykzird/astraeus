# HTTP API

The JSON API the web UI uses, and the same one you can drive with `curl`. Every
route below is also listed where it is implemented, in `internal/api`.

## Endpoints

| Method | Path | Purpose |
| --- | --- | --- |
| GET | `/api/health` | Liveness |
| GET | `/api/system/capabilities` | ffmpeg/ffprobe presence, video and audio encoders, hardware path |
| GET | `/api/libraries` | List libraries |
| POST | `/api/libraries` | Register `{name, path, kind}` |
| GET | `/api/libraries/{id}` | One library |
| DELETE | `/api/libraries/{id}` | Remove a library and its entities |
| POST | `/api/libraries/{id}/scan` | Scan, returns a `ScanResult` |
| POST | `/api/scan` | Re-scan every library, returns a `ScanOutcome` each |
| DELETE | `/api/streams/{id}` | Stop a streaming session now, `204` |
| GET | `/api/libraries/{id}/entities` | Entities, optionally `?status=Incomplete` |
| GET | `/api/entities` | All entities, optionally `?status=` |
| GET | `/api/entities/{id}` | Entity with its objects, children and parent |
| POST | `/api/metadata/enrich` | Run one enrichment pass |
| POST | `/api/entities/{id}/playback` | Negotiate playback, returns a URL and subtitle tracks |
| GET | `/api/progress` | This viewer's resumable positions, most recently watched first |
| PUT | `/api/entities/{id}/progress` | Record where the viewer got to (resumable playback) |
| DELETE | `/api/entities/{id}/progress` | Forget the caller's position, which is what starting over means |
| GET | `/api/objects/{id}/file` | The original file (range requests supported) |
| GET | `/api/objects/{id}/subtitles/{file}` | One subtitle track as WebVTT; `{file}` is `<ffmpeg stream index>.vtt` |
| GET | `/api/images/{size}/{file}` | Poster/backdrop artwork, proxied and cached |
| GET | `/metrics` | Prometheus metrics |
| GET | `/hls/{session}/{file}` | Playlist and segments of a live session |

## Resume and watch state

Where a viewer got to is stored per viewer and entity and reported by `PUT
/api/entities/{id}/progress` with `{"position_seconds": 754.5,
"duration_seconds": 7025}`; `GET /api/entities/{id}` then carries a `progress`
object with the same numbers plus `percent` and `finished`. `DELETE` forgets the
caller's position.

Three rules keep it honest rather than merely present:

- A position in the last 5% of the media is **finished**, and finishing *clears*
  the position instead of storing it. Resuming three seconds from the end is
  worse than starting the next thing.
- A position beyond the end of the media is a `400` with the arithmetic in the
  message. A player reporting its final frame a moment after the end is fine;
  numbers that are simply wrong are not stored for the next resume to trust.
- Progress belongs to the **viewer the request authenticated as**, so two people
  watching the same film each keep their own place. The row cascades when the
  entity is pruned, so a deleted film cannot leave a bookmark behind.

The viewer is the identity the access gate verified and put on the request. With
the gate disabled there is a single viewer, named `local`; a client cannot claim
someone else's timeline because no handler ever reads an identity header, only
the context the gate fills. In `token` mode the gate reports every bearer-token
client as `token`, so API clients share one place — per-viewer state needs
`proxy` mode. A database written before this behaviour keeps its rows under
`local`, because the identity that wrote them was never recorded.

The player resumes on Play and reports while watching. It reports a position
under five seconds as a *clear* rather than a store, so a viewer who sampled ten
seconds of something does not get offered a resume at the beginning.

`GET /api/progress` is what makes a stored position findable: it lists one
viewer's positions worth resuming, most recently watched first, with each entity
attached so a client can render a row without a second request. Watched-through
entries are excluded by the query rather than filtered afterwards — a finished
row consuming one of the limit's slots is how a listing of one comes back empty.
The player's sidebar renders it as a **Continue watching** section, hidden
entirely when there is nothing to continue, and refreshes when progress changes.

Entity payloads also carry `poster_url` and `backdrop_url` pointing at the local
image proxy, so a client never has to know the metadata provider's URL scheme.

`size` must be one of the allow-listed TMDB renditions (`w45`, `w92`, `w154`,
`w185`, `w300`, `w342`, `w500`, `w780`, `w1280`, `h632`, `original`), and
`file` must be a bare image basename; anything else is rejected before it can
reach either the cache or the upstream origin.

## Metrics

`/metrics` exposes the specification's KPI registry in Prometheus text format.
The whole registry is declared at startup, so an idle server still reports every
metric and absence-based alerting works.

| Metric | Meaning |
| --- | --- |
| `astraeus_first_segment_seconds` | `fttt_latency`: stream preparation to the first segment delivered |
| `astraeus_transcode_startup_seconds` | `transcode_startup_time`: ffmpeg start to playlist available |
| `astraeus_metadata_lookup_seconds` | `metadata_latency`, labelled by provider and outcome |
| `astraeus_stream_errors_total` ÷ `astraeus_stream_sessions_total` | `stream_error_rate` |
| `astraeus_playback_decisions_total` | Negotiations by chosen mode |
| `astraeus_scan_seconds`, `astraeus_scan_files_total`, `astraeus_scan_runs_total` | Scanning |
| `astraeus_http_requests_total`, `astraeus_http_request_seconds` | HTTP |
| `astraeus_stream_sessions_active`, `astraeus_probe_errors_total` | Operational |
| `astraeus_transcode_fallbacks_total` | Hardware transcodes retried in software |
| `astraeus_auth_granted_total`, `astraeus_auth_denied_total{reason}` | Access gate grants and denials |
| `astraeus_rate_limited_total` | API requests refused by the rate limiter |
| `astraeus_spans_dropped_total` | Trace spans dropped because the export queue was full |

Errors are `{"code": "...", "message": "..."}` with a matching status code.
