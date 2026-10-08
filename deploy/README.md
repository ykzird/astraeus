# Deploying Astraeus Media

Two supported shapes: a **container** (everything included, including ffmpeg) and
a **systemd service** (a binary, ffmpeg from your distribution, and a unit file).
Both run the same server; pick by how you already run things.

Whatever you pick, four facts decide whether the install is sound:

- **ffmpeg and ffprobe are dependencies, not extras.** The server reports their
  absence at startup and refuses playback without them.
- **tesseract is optional, and its absence changes behaviour rather than breaking
  anything.** With it, image subtitle tracks (PGS) are read into text a browser
  can toggle and search; without it they are offered as a burn-in instead, and
  `/api/system/capabilities` reports `subtitle_ocr_enabled: false`. The container
  image includes it; a systemd install can add it with
  `apt-get install tesseract-ocr` (or the distribution's equivalent).
- **`--auth-mode` defaults to `none`.** That is right for a trusted LAN and wrong
  for anything else. Both shapes below turn the gate on before anything is
  published.
- **The database is the state.** Segments and caches are disposable; the SQLite
  file is what a backup has to capture.

---

## Container

```sh
docker build -t astraeus-media:0.18.0 .

# The image's default command serves on :8642 with every writable path inside
# /data. This one has no access gate, so keep it on loopback.
docker run -d --name astraeus \
  -p 127.0.0.1:8642:8642 \
  -v /srv/media:/media:ro \
  -v astraeus-data:/data \
  astraeus-media:0.18.0
```

Flags are passed through the entrypoint, so the server's own options can be
appended to any run — which is also how a library is registered:

```sh
# Register and scan a library that is mounted read-only at /media.
docker exec astraeus astraeus-server scan \
  --db /data/astraeus.db --path /media/movies --kind movies --name Movies
```

The image's default command already sets every writable path inside one volume:

| Path | Contents |
| --- | --- |
| `/data/astraeus.db` | the library database (back this up) |
| `/data/streams` | HLS session directories, swept at startup and reaped when idle |
| `/data/images`, `/data/subtitles` | artwork and subtitle caches (disposable) |
| `/media` | your library, mounted read-only |
| `/app/web` | the web UI the binary serves |

A realistic run, with the gate on and the library read-only:

```sh
docker run -d --name astraeus \
  -p 127.0.0.1:8642:8642 \
  -e ASTRAEUS_AUTH_TOKEN="$(openssl rand -hex 32)" \
  -v /srv/media:/media:ro \
  -v astraeus-data:/data \
  astraeus-media:0.18.0 \
  serve --addr 0.0.0.0:8642 --web-dir /app/web \
        --db /data/astraeus.db --stream-root /data/streams \
        --image-cache /data/images --subtitle-cache /data/subtitles \
        --auth-mode token
```

Publishing on `127.0.0.1` and putting a reverse proxy in front is the intended
shape; `-p 8642:8642` publishes it to every interface, which without
`--auth-mode` hands anyone who can reach the port the whole library.

**Which gate mode you choose changes what "per viewer" means.** Playback progress
is keyed on the identity the gate attaches, so `--auth-mode proxy` (Tailscale or
Cloudflare Access forwarding an identity header) gives each person their own
place. `--auth-mode token` reports every API client as `token`, which is right
for a script and means they share one place; `none` has a single `local` viewer
and is the correct choice only where there is one viewer.

**Rate limiting follows the same rule.** `--rate-limit 20` bounds API calls per
second per client. Behind `proxy` mode that is per person; in `token` mode every
API client shares one bucket, and with the gate off the peer address is used —
which, behind a reverse proxy, is the proxy's own address, so the limit becomes
global. The limiter is per process, so several replicas behind one proxy each
hold their own buckets.

**Tracing is opt-in and needs to reach the collector.** `--otel-endpoint
http://collector:4318` (or `OTEL_EXPORTER_OTLP_ENDPOINT`) exports spans over
OTLP/HTTP; empty disables it. In a container, that endpoint has to be reachable
from inside the container's network namespace — a collector on the host is not
`127.0.0.1` from there. The request log line then carries `trace_id` and
`span_id`, so `docker logs` and the trace viewer line up.

**Hardware acceleration.** A container does not see the GPU unless it is passed
in: add `--device /dev/dri` for VAAPI, and the container's ffmpeg must be able to
load the vendor driver (the image ships ffmpeg's VAAPI support but not
`mesa-va-drivers` / `intel-media-va-driver`). The startup probe answers this
honestly — `GET /api/system/capabilities` lists what actually encoded, and
`rejected_encoders` carries ffmpeg's own complaint for what did not.

**What was verified in the image:** built and run; `/api/health` answers and the
health check turns `healthy`; a mounted library scans; all three delivery paths
were exercised with the image's own **ffmpeg 5.1.9** — direct play, an HDR source
tone mapped to 8-bit `bt709` for a browser profile, an HDR source remuxed at
10-bit `bt2020`/`smpte2084` for a manifest declaring `supports_hdr`, and an HDR
*re-encode* (forced by a downscale) at 10-bit `bt2020`/`smpte2084` as well.
**OCR was verified in the image too**, with its own **tesseract 5.3.0**: startup
logged `image_subtitles="read as text"`, `subtitle_ocr_enabled` was true, and a
mounted PGS fixture served `/api/objects/{id}/subtitles/1.vtt` with the caption
the fixture drew — a second data point beside the host's ffmpeg 9.0 and
tesseract 5.5.3.

---

## systemd

Assumes a Linux host with `ffmpeg` installed from the distribution, and an
extracted release archive to run these steps from: every file they name is
inside one (`astraeus-server`, `web/`, `deploy/` and the project documents).

```sh
# 1. An account with no shell, and the three directories the unit names.
sudo useradd --system --home /var/lib/astraeus --shell /usr/sbin/nologin astraeus
sudo install -d -o astraeus -g astraeus /var/lib/astraeus /var/cache/astraeus
sudo install -d -o astraeus -g astraeus /var/cache/astraeus/images /var/cache/astraeus/subtitles

# 2. The binary and the web UI it serves, and the documentation the README
#    links to. The archive carries the user-facing pages and not the project's
#    working documents, so the README's link to docs/handoff.md resolves in a
#    checkout and not in this installed tree; the trailing chmod is because this
#    repository's own files are not world-readable.
sudo install -m 0755 astraeus-server /usr/local/bin/astraeus-server
sudo install -d /usr/local/share/astraeus
sudo cp -r web /usr/local/share/astraeus/web
sudo install -d -m 0755 /usr/local/share/doc/astraeus
sudo cp -r README.md SPECIFICATION.md TODO.md CONTEXT.md CONTRIBUTING.md \
  SECURITY.md LICENSE THIRD_PARTY_NOTICES.md docs deploy \
  /usr/local/share/doc/astraeus/
sudo chmod -R a+rX /usr/local/share/doc/astraeus

# 3. The token the unit reads, and the unit itself.
sudo install -d -m 0755 /etc/astraeus
printf 'ASTRAEUS_AUTH_TOKEN=%s\n' "$(openssl rand -hex 32)" \
  | sudo tee /etc/astraeus/astraeus.env >/dev/null
sudo chmod 0600 /etc/astraeus/astraeus.env
sudo install -m 0644 deploy/astraeus.service /etc/systemd/system/astraeus.service

# 4. Check what you are about to start, then start it.
sudo systemd-analyze verify /etc/systemd/system/astraeus.service
sudo systemctl daemon-reload && sudo systemctl enable --now astraeus
systemctl status astraeus

# The port opens only after the startup probe has run ffmpeg once per encoder
# family, which takes seconds on a slow or emulated host - so wait for it rather
# than treating the line above as proof that it is already serving.
for i in $(seq 1 30); do curl -sf localhost:8642/api/health && break; sleep 1; done
```

The unit binds **127.0.0.1:8642** and enables `--auth-mode token`, so nothing is
reachable from off-host until you put a TLS-terminating reverse proxy in front of
it. `TMDB_API_KEY` goes in the same environment file if you want real metadata
instead of the synthetic fallback.

### What the unit hardens, and what has been observed

`ProtectSystem=strict` with `ReadWritePaths` limits writes to the two state
directories; `CapabilityBoundingSet=` is empty; `ProtectHome`, `ProtectProc`,
`ProtectKernel*`, `RestrictAddressFamilies`, `RemoveIPC` and a private `/tmp` are
on; `UMask=0077` keeps what the service writes to itself. `systemd-analyze
security` scores it **1.6 (OK)**.

Two deliberate choices are worth knowing:

- **`PrivateDevices` is off**, because it would hide `/dev/dri` and break VAAPI.
  If you do not use hardware acceleration, turning it on is free.
- **`SystemCallFilter=@system-service` is the one setting that could stop
  ffmpeg.** A syscall outside the list fails with `EPERM` and kills the
  transcode. If a session fails that way, the ffmpeg diagnostics in the session
  error name it, and the fix is to relax that single line. The filter is the
  standard one for a service of this kind, and it is now known to admit a
  `libx264` re-encode on a CPU-only host — the case most installs will hit.
  Hardware encoders are the part that remains unexercised.

**This unit has been started, and made to transcode, on a clean Debian 12 VM**
(QEMU, with no KVM). Following the runbook above verbatim, installing the
published `v0.17.0` release archive: `systemd-analyze verify` was clean, the
service came up and stayed up, and `/api/health` answered
`{"service":"astraeus","status":"ok"}`. `systemd-analyze security astraeus`, run
*inside* that guest rather than with `--offline=yes`, scores the same
**1.6 (OK)**.

The syscall filter was tested the way it matters — by making the service run
ffmpeg, which a direct play would not do. Asking for a target height below the
source's produced `"mode":"transcode"`, and the ffmpeg child was caught alive and
read back its own sandbox:

```
cgroup:       0::/system.slice/astraeus.service
Seccomp:      2          (28 filters)
CapEff:       0000000000000000
CapBnd:       0000000000000000
NoNewPrivs:   1
```

The filter is therefore applied to ffmpeg itself, not only to the server that
forks it, and the encode finished with no `EPERM` in the journal — the produced
segment ffprobed as H.264 at exactly the height that was requested. What this
does *not* cover is any hardware encoder: that guest had no render node, so it
rejected all six hardware encoders at startup and transcoded on the CPU. On a
host with a GPU, the first hardware transcode is still the test.

One thing worth knowing if you go looking for it: the unit declares **no
`StateDirectory` or `CacheDirectory`**. Step 1's `install -d` creates the two
writable trees, and the service creates `streams/` (0700) and the database (0600)
itself, under `UMask=0077`.

---

## Releases

A release is a tag. Pushing one that starts with `v` runs
`.github/workflows/release.yml`, which gates on the unit tests, builds the
archives, publishes a GitHub Release and pushes a multi-arch image to GHCR:

```sh
git tag -a v0.18.0 -m "v0.18.0"
git push origin v0.18.0
```

The same workflow has a `dry_run` option in the Actions tab: it builds every
artifact and the image and publishes nothing, which is how to check a change to
it without spending a tag.

**What a release contains.** One `astraeus-server_<version>_<os>_<arch>.tar.gz`
per platform — `linux/amd64` and `linux/arm64` by default — each holding the
binary, the `web` directory it serves (a server without a UI is half a server),
the `deploy` directory with the systemd unit, the user-facing pages of `docs/`
(`index.md`, `playback.md`, `configuration.md`, `api.md` and `development.md`),
`LICENSE` and `THIRD_PARTY_NOTICES.md`, plus a `checksums.txt` covering them.
The archive is self-sufficient: the systemd runbook above is meant to be followed
from an extracted one, and every path it names is in there. It deliberately does
**not** carry the repository's working documents — `docs/handoff.md` and the
reviews — which are about building the server rather than using it:

```sh
sha256sum -c checksums.txt          # from inside the extracted release dir
./astraeus-server version           # astraeus-server 0.18.0
```

The image is published as both `:<version>` and `:latest`, one manifest covering
amd64 and arm64, with build provenance and an SBOM attested alongside it. The
package is private by default; make it public in the repository's package
settings if anonymous pulls are wanted. The version is baked in at build time
(`-ldflags "-X main.version=..."`), so a binary built from a working tree reports
`dev` rather than a number it cannot back up.

**Running the build by hand** is deliberate: the workflow calls the same script
you can, and every step below is a command that was run on the development host
before the repository had a remote.

```sh
# This host reaches go through mise; CI has it on PATH.
mise exec -- scripts/build-release.sh 0.18.0
mise exec -- scripts/build-release.sh 0.18.0 linux/amd64 linux/arm64 darwin/arm64
```

**What was verified, and what was not.** The archives were built for amd64 and
arm64, the checksums verified with `sha256sum -c`, the amd64 binary run (printing
its injected version) and the arm64 one confirmed as an AArch64 ELF; the image
was built with the version and OCI labels and run. The workflow YAML passes
`actionlint`, and CI scans every push for secrets with trufflehog while
Dependabot keeps the Go modules, the workflow action pins and the container base
images current. CI now runs on GitHub for every push to `main` and every pull
request, and it earned its keep immediately: the first run failed the ladder
integration test against the runner's ffmpeg and the failure reproduced against
the container's own 5.1.9, showing that a ladder had been serving two rungs of
three. **The release workflow has now run**, on `v0.17.0`, and it found two
defects that only a real run could surface: the archive did not carry the `deploy`
directory or the documents this runbook installs, and the job that creates the
GitHub Release had no repository context for `gh`, so it died with `fatal: not a
git repository`. Both are fixed, and the release now publishes the two archives,
the checksums file, generated notes and the multi-arch image. The workflow's
`dry_run` option was green before the tag was spent — but it skips the publish job
by design, which is exactly where the second defect was hiding.

---

## Backups and upgrades

```sh
# The database is the state. Stop the server, or copy it with SQLite's own
# backup API so a write in flight cannot tear the copy.
sqlite3 /var/lib/astraeus/astraeus.db ".backup /var/backups/astraeus-$(date +%F).db"
```

Schema migrations live in the binary and run at startup; a database written by an
older build is upgraded in place. **The server does not keep a backup before it
does**, so take one first if the database matters — the command above is the one
to run. Upgrading is then: replace the binary and the `web` directory, restart.
Nothing under `/var/cache` needs to be preserved.

One upgrade changes shape rather than adding tables: playback progress became
per viewer, which SQLite cannot do in place, so the table is rebuilt in a
transaction and the rows written before it are kept under the single `local`
viewer. Nothing is lost, but a viewer who used to resume through an identity-aware
gate will not find those older positions under their own identity, because the
identity that wrote them was never recorded.

---

## Not covered yet

- **TLS.** Put Caddy, nginx or Tailscale in front; the server speaks plain HTTP.
- **Multiple users or per-user libraries.** The gate is instance-wide: it decides
  whether a request is admitted, not what it may see, so every admitted user sees
  the whole library. Playback progress is per viewer, but access is not.
- **Kubernetes manifests, Windows or macOS packaging.** Release archives are
  built for Linux, and the image is Linux-only; `scripts/build-release.sh` takes
  extra `goos/goarch` arguments if that changes.
- **Hardware encoders have never run.** VAAPI, NVENC, AMF and VideoToolbox are
  implemented and unit-tested, and the startup probe is what validates them on
  the machine that starts the server — but no host with a GPU has been available,
  so the CPU path is the only one observed end to end.
