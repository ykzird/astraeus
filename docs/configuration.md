# Configuration and operations

Flags, libraries, and the parts you turn on once the server is reachable by more
than you: the access gate, rate limiting, tracing and the security headers.

## Scanning

A scan is idempotent: re-running it reuses entities rather than duplicating
them, and it reports distinct entities rather than lookups. It also **prunes**:
a file that is gone stops appearing, and any entity left holding neither an
object nor a child is removed with it, working upwards so an emptied season
takes its series with it.

Pruning deletes data, so it refuses to run whenever the scan's view of the disk
might be incomplete — if any path was unreadable, or if the scan found zero
files while the library still holds entities, which is the shape of a drive
that is not mounted. Silently emptying a library is far worse than leaving a
ghost entry behind. The returned `ScanResult` reports what was removed as
`objects_pruned` and `entities_pruned`. `--scan-interval` (default `6h`,
`0` disables) re-scans every library for new files; `--enrich-interval` does the
same for metadata. `--image-cache` and `--subtitle-cache` place the artwork and
WebVTT caches.

## CLI

```
astraeus-server serve [flags]                  start the HTTP API
astraeus-server scan --path DIR --kind KIND    scan a directory
astraeus-server scan --library ID              re-scan a registered library
astraeus-server library add --name N --path D --kind KIND
astraeus-server library list [--json]
astraeus-server library rm ID
astraeus-server enrich                         run one metadata pass
astraeus-server version
```

Run any command with `-h` for its flags. Shared flags: `--db`, `--tmdb-key`,
`--log-level`, `--log-format`. Flags are per-command: there is no global `--db`,
so it has to follow the subcommand. `astraeus-server version` prints the build
identifier: a released binary prints its tag, and one built from a working tree
prints `dev`.

`serve` flags that are easy to miss because they are named in the sections below
rather than here:

| Flag | Default | Purpose |
| --- | --- | --- |
| `--addr` | `127.0.0.1:8642` | Listen address |
| `--web-dir` | `web` | Static UI directory |
| `--ffmpeg`, `--ffprobe` | `ffmpeg`, `ffprobe` | Binaries to run (both are hard dependencies) |
| `--stream-root` | temp | HLS session directories |
| `--segment-seconds` | 6 | HLS target segment duration |
| `--max-sessions` | 8 | Concurrent segmented streams |
| `--image-cache`, `--subtitle-cache` | temp | Artwork and WebVTT caches |
| `--tesseract-bin`, `--ocr-language` | `tesseract`, tesseract's own | OCR of image subtitles; a missing binary leaves them burn-only |
| `--tmdb-image-base` | TMDB's own root | Upstream artwork root |
| `--enrich-interval`, `--scan-interval` | — | Background passes; `0` disables |
| `--auth-mode`, `--auth-header`, `--trusted-proxy`, `--auth-token`, `--auth-exempt` | see [Access gate](#access-gate) | Gate configuration |
| `--rate-limit`, `--rate-limit-burst` | `0` (off) | API limit |
| `--otel-endpoint`, `--otel-service-name` | off | Trace export |

## Security headers

Every response carries `X-Content-Type-Options: nosniff`, `Referrer-Policy:
no-referrer`, `X-Frame-Options: DENY`, `Cross-Origin-Resource-Policy:
same-origin` and a `Permissions-Policy` that refuses the features this app has no
use for. Documents additionally carry a content security policy:

```
default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self';
media-src 'self' blob:; worker-src 'self' blob:; connect-src 'self';
font-src 'self'; object-src 'none'; base-uri 'none'; form-action 'none';
frame-ancestors 'none'
```

The strictness is possible because of how the front end is written, not in spite
of it: there is no inline script, no inline style, and no HTML-injection sink
anywhere in `web/`, so `'unsafe-inline'` is not needed, and the vendored hls.js
contains no `eval` either — that was checked, not assumed. `blob:` appears for
`media-src` and `worker-src` because Media Source Extensions play a blob URL and
hls.js runs its demuxer in a worker built from one. JSON responses get the
transport-level headers but **not** the policy: a policy on a JSON body is
something a client can never act on.

`img-src 'self'` is also the enforcement behind a privacy fix: artwork is only
ever loaded from this server's `/api/images/...` proxy, which fetches the
provider's image server-side. The UI no longer uses the metadata's own absolute
URL even when it is present, because fetching `image.tmdb.org` from the browser
tells that third party who is watching and from where. Verified in headless
Chromium: a third-party image is refused with an `img-src` violation, an inline
script is refused, a same-origin image is not, and the whole 32-check player
harness passes with no console errors under the policy.

`Strict-Transport-Security` is deliberately absent: this server speaks plain
HTTP, where browsers ignore it. A reverse proxy that terminates TLS is where it
belongs — [`deploy/tls/README.md`](../deploy/tls/README.md) is the runbook for
that, and for the identity the gate needs a proxy to assert.

## Access gate

Per the specification, authentication is delegated to an identity-aware proxy
rather than a built-in user database. `--auth-mode` selects the policy:

| Mode | Behaviour |
| --- | --- |
| `none` (default) | No gate. Correct for a trusted LAN; **never** expose this to the internet |
| `proxy` | Believe an identity header — but only when the request arrives from a configured trusted address |
| `token` | Require `Authorization: Bearer <token>`, compared in constant time |

```sh
# Behind Tailscale (tailscale serve sets Tailscale-User-Login) or Cloudflare Access
./astraeus-server serve --auth-mode proxy --trusted-proxy 100.64.0.0/10,127.0.0.1/32

# For API clients and scripts
ASTRAEUS_AUTH_TOKEN=$(openssl rand -hex 32) ./astraeus-server serve --auth-mode token
```

The security property that matters: **an identity header is only believed from a
trusted address.** Headers are trivially forgeable by anyone who can reach the
port, so trusting them without that check would be worse than no gate at all.
`X-Forwarded-For` is deliberately ignored for the same reason — the address the
connection actually came from is the only trustworthy one. Configuration fails
closed: `proxy` mode without `--trusted-proxy`, or `token` mode without a token,
refuses to start.

`--auth-exempt` (default `/api/health`) lists exact paths that bypass the gate,
so liveness probes keep working. `/metrics` is **not** exempt: point Prometheus
at it with a token or let it through the proxy.

Identity is recorded on every request log line, so the gate is auditable, and
`astraeus_auth_granted_total` / `astraeus_auth_denied_total{reason}` show what
the gate is doing.

A browser cannot attach a bearer token to a plain navigation, so browser access
belongs behind `proxy` mode; `token` mode suits clients and automation.

### Which libraries a viewer may see

The gate decides whether a request is admitted. `--access-policy` decides what an
admitted viewer may see and change, which is what makes more than one person
share an install:

```
# /etc/astraeus/access-policy.conf
default: none            # an unlisted viewer sees nothing (the default)
admin: jok@example.com   # may scan, enrich, add and remove libraries

jok@example.com: *       # "*" is every library, including ones added later
alice@example.com: Movies, Documentaries
bob@example.com: Kids    # a name, matched case-insensitively
```

```sh
./astraeus-server serve --auth-mode proxy --trusted-proxy 127.0.0.1/32 \
  --access-policy /etc/astraeus/access-policy.conf
```

Four things are worth knowing:

- **Without a file, nothing changes.** Every admitted viewer sees and may change
  every library, which is what an install has always done. A file that cannot be
  read or parsed is an error and the server refuses to start, because starting
  open when the operator asked for restricted is the one failure that matters.
- **A grant is a name or an id.** A name is matched case-insensitively, an id
  exactly, and a name that does not exist yet is not an error — it matches
  nothing until the library is added. `*` means every library, present and future.
- **Visibility and administration are separate.** An admin may scan, enrich and
  change libraries; that grants nothing to look at. The operator above is granted
  both deliberately. A hidden library is reported as **404, not 403**, so the API
  is not a way to discover which libraries exist.
- **Changing the file needs a restart.** The policy is read once at startup and
  the loaded policy is named on the startup line (`viewers=2 admins=1
  default=none`), so a typo in a path is visible rather than silent.

Only the visibility of a library is enforced per viewer. What a viewer may do
*inside* a library it can see — play anything in it, report progress on it — is
not restricted further, and there is no per-library administration.

## Rate limiting

`--rate-limit` (requests per second per client, default `0` = off) bounds how
often the API may be called, and `--rate-limit-burst` sets how many requests a
client may make at once (default: the rate rounded up, which is the smallest
bucket a normal page load still fits in).

```sh
./astraeus-server serve --rate-limit 20 --rate-limit-burst 40
```

The limit is a token bucket, so a client that behaves gets its allowance back
rather than being cut off for the rest of a window. Only `/api/` is limited:
the UI, its assets and the HLS segments are *delivery*, and a page load pulls
several files at once while one playback fetches a segment every few seconds.
Sessions are capped separately by `--max-sessions`, and `/api/health` stays open
for liveness probes. A refusal is a `429` in the API's error shape with a
`Retry-After: 1`, and is counted in `astraeus_rate_limited_total`.

**What a client is depends on the gate.** With an identity from `proxy` mode the
bucket is per person, which is the point: behind a proxy every request arrives
from the proxy's own address, so an address-keyed limit would be one global
bucket for everyone it serves. With no gate, the peer address is used — the same
address the access gate trusts, with `X-Forwarded-For` ignored for the same
reason. In `token` mode every API client shares the `token` identity and so
shares one bucket. A request the server cannot attribute at all is allowed
rather than charged to a bucket it shares with everyone else.

The limiter is per process. Several servers behind one proxy each hold their own
buckets, so the effective limit is the sum; a shared limit would need a shared
store.

## Tracing

`--otel-endpoint` (or `OTEL_EXPORTER_OTLP_ENDPOINT`) turns on trace export over
**OTLP/HTTP**; empty disables it, which is the default. `--otel-service-name`
sets the resource's `service.name` (default `astraeus-media`).

```sh
./astraeus-server serve --otel-endpoint http://127.0.0.1:4318
```

Every HTTP request becomes a **server span** carrying the method, path, peer
address and response status, and an incoming W3C `traceparent` is continued, so a
trace started by a proxy or another service keeps its id here. Inside a request,
a playback negotiation and the streaming session it starts are **child spans**
(the second carries the mode, the video action and the session id), so the slow
part — forking ffmpeg and waiting for the first segment — is visible as the part
that took the time. The request log line carries `trace_id` and `span_id`, which
is what connects a log entry to the trace it belongs to. Spans are batched and
posted to `<endpoint>/v1/traces`.

The encoder and the batcher are hand-rolled rather than the OpenTelemetry SDK —
the same trade the Prometheus exposition makes — so the dependency list stays at
three modules. Three consequences are worth knowing:

- A full export queue **drops spans and counts them** in
  `astraeus_spans_dropped_total` instead of adding latency to a request that has
  already finished, and a collector that is down is logged rather than retried
  into a growing queue.
- What is sent is traces only. Metrics stay on `/metrics`; logs stay on stderr.
- There is no sampling beyond honouring a parent's sampled flag, no baggage, and
  no propagation to the outbound calls the server itself makes.

Verified end to end against a real collector: Jaeger all-in-one in a container
listed `astraeus-media` as a service, received a span per request, and showed a
span carrying an incoming `traceparent` as its child.
