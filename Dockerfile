# syntax=docker/dockerfile:1

# Astraeus Media in one image: the server, the web UI it serves, and the ffmpeg
# it shells out to for probing, transcoding and subtitle extraction.
#
# The binary is pure Go with no cgo - the SQLite driver is modernc.org/sqlite -
# so the runtime image needs no toolchain and no libc beyond Debian's.

# ---- build -----------------------------------------------------------------
# The builder runs on the *build* platform and cross-compiles for the target
# one, so a multi-arch build needs no emulation: the Go toolchain is native and
# only the runtime base image differs per architecture.
FROM --platform=$BUILDPLATFORM golang:1.26-bookworm AS build

WORKDIR /src

# The version the binary reports. The release workflow passes the tag; a plain
# `docker build` leaves the development default, which is what an untagged build
# honestly is. TARGETOS/TARGETARCH are defined by BuildKit from --platform.
ARG VERSION=dev
ARG TARGETOS=linux
ARG TARGETARCH

# The module files are copied first so the dependency layer survives a source
# change, which is most of the build time.
COPY go.mod go.sum ./

# go.mod's `toolchain` line is what decides the Go that compiles this, and it is
# what keeps the image and the release tarball on the same patched toolchain. A
# base image tag only carries the major.minor, so without this check a lagging
# `golang:1.26-bookworm` could quietly build the image with a Go that has the
# range-header and MIME-header vulnerabilities the tarball was rebuilt to avoid.
# `go` reads go.mod here, downloads the pinned toolchain if the image lacks it,
# and this fails the build if it cannot.
RUN go version \
 && required="$(sed -n 's/^toolchain go//p' go.mod)" \
 && current="$(go env GOVERSION | sed 's/^go//')" \
 && if [ -n "$required" ] && [ "$(printf '%s\n%s\n' "$required" "$current" | sort -V | tail -1)" != "$current" ]; then \
      echo "the builder is running Go $current but go.mod pins toolchain go$required" >&2; \
      exit 1; \
    fi

RUN go mod download

COPY cmd ./cmd
COPY internal ./internal
COPY web ./web

# CGO_ENABLED=0 is what makes a static binary, and -trimpath keeps the build
# reproducible by keeping this machine's paths out of it.
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build \
        -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
        -o /out/astraeus-server ./cmd/astraeus-server

# ---- runtime ---------------------------------------------------------------
FROM debian:bookworm-slim

# Provenance for a released image: what it is, where its source is, which
# revision built it and under what licence. The release workflow passes SOURCE,
# VERSION and REVISION; an untagged build gets defaults that say so.
ARG VERSION=dev
ARG REVISION=unknown
ARG SOURCE=https://github.com/ykzird/astraeus
LABEL org.opencontainers.image.title="Astraeus Media" \
      org.opencontainers.image.description="A single-binary media server with an embedded web UI" \
      org.opencontainers.image.source="${SOURCE}" \
      org.opencontainers.image.url="${SOURCE}" \
      org.opencontainers.image.licenses="MIT" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${REVISION}" \
      org.opencontainers.image.base.name="docker.io/library/debian:bookworm-slim"

# ffmpeg is a hard dependency, not a convenience: without it the server can still
# list a library but cannot probe a file, so it reports that at startup and
# refuses playback. tesseract is the opposite - an optional one: with it, image
# subtitle tracks (PGS and VobSub) are read into text a browser can toggle and
# search;
# without it they keep the burn-in path. It is included so the packaged server
# offers the better of the two. ca-certificates is for the TMDB metadata provider
# over HTTPS; curl exists only for the container health check.
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
        ffmpeg tesseract-ocr ca-certificates curl \
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
#
# --addr is 0.0.0.0 here and the gate is off, which together mean the container
# serves its library to anything that can reach the port. That is the right
# default for a container - bind loopback and a published port reaches nothing -
# and it puts the decision where it belongs, in the `-p` argument:
#
#   -p 127.0.0.1:8642:8642   publishes on the host's loopback only, which is the
#                            shape deploy/README.md recommends, paired with a
#                            TLS-terminating proxy for viewers
#   -p 8642:8642             publishes on every interface
#
# Inside the container the port is always reachable, so a gate is not what makes
# this safe; the published address and the proxy in front of it are. The systemd
# unit takes the other route - loopback plus the mode the operator chooses - so
# the two shapes differ here on purpose.
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
