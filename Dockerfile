# syntax=docker/dockerfile:1

# Astraeus Media in one image: the server, the web UI it serves, and the ffmpeg
# it shells out to for probing, transcoding and subtitle extraction.
#
# The binary is pure Go with no cgo - the SQLite driver is modernc.org/sqlite -
# so the runtime image needs no toolchain and no libc beyond Debian's.

# ---- build -----------------------------------------------------------------
FROM golang:1.26-bookworm AS build

WORKDIR /src

# The module files are copied first so the dependency layer survives a source
# change, which is most of the build time.
COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal
COPY web ./web

# CGO_ENABLED=0 is what makes a static binary, and -trimpath keeps the build
# reproducible by keeping this machine's paths out of it.
RUN CGO_ENABLED=0 GOOS=linux go build \
        -trimpath -ldflags "-s -w" \
        -o /out/astraeus-server ./cmd/astraeus-server

# ---- runtime ---------------------------------------------------------------
FROM debian:bookworm-slim

# ffmpeg is a hard dependency, not a convenience: without it the server can still
# list a library but cannot probe a file, so it reports that at startup and
# refuses playback. ca-certificates is for the TMDB metadata provider over HTTPS;
# curl exists only for the container health check.
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
        ffmpeg ca-certificates curl \
 && rm -rf /var/lib/apt/lists/*

# A fixed uid so a bind-mounted volume can be owned predictably from the host.
# It is deliberately above the system range: the port is above 1024, so there is
# nothing a system account would buy, and a high uid is easier to match on a
# host whose own user is 1000.
RUN useradd --uid 10001 --create-home --home-dir /home/astraeus astraeus

COPY --from=build /out/astraeus-server /usr/local/bin/astraeus-server
COPY --from=build /src/web /app/web

# Everything the server writes lives under one directory: the database, the HLS
# session directories, and the artwork and subtitle caches. Media itself is
# mounted read-only and is never written to.
RUN mkdir -p /data && chown astraeus:astraeus /data
VOLUME ["/data"]

USER astraeus
WORKDIR /home/astraeus

# 0.0.0.0 rather than loopback: a container's port has to be reachable from
# outside it to be published. The access gate is still off by default, which is
# why the runbook pairs every published port with --auth-mode.
EXPOSE 8642

# The binary is the entrypoint so the default arguments below can be overridden
# without repeating the path: `docker run astraeus-media scan --path /media`.
# Without an ENTRYPOINT, "serve" would be looked up in $PATH and the container
# would exit before logging anything.
ENTRYPOINT ["/usr/local/bin/astraeus-server"]
CMD ["serve", \
     "--addr", "0.0.0.0:8642", \
     "--web-dir", "/app/web", \
     "--db", "/data/astraeus.db", \
     "--stream-root", "/data/streams", \
     "--image-cache", "/data/images", \
     "--subtitle-cache", "/data/subtitles"]

HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
    CMD curl -fsS http://127.0.0.1:8642/api/health || exit 1
