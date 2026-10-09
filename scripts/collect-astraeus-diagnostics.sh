#!/usr/bin/env bash
#
# collect-astraeus-diagnostics.sh - gather everything a maintainer needs to
# explain a running Astraeus container, into one tarball that can be sent over.
#
# It is read-only with respect to the running instance: it copies files out of
# the container, reads /metrics and the API, and never restarts, reconfigures or
# stops anything. The one file it writes inside the container is a temporary
# copy of the database under /tmp, which is left in place only if the copy
# failed.
#
#   ./scripts/collect-astraeus-diagnostics.sh                 # 30 s of sampling
#   ASTRAEUS_CONTAINER=astraeus ASTRAEUS_SAMPLE_SECONDS=120 \
#       ./scripts/collect-astraeus-diagnostics.sh
#
# Output: ./astraeus-diag-<host>-<timestamp>.tar.gz
#
# Exit codes: 0 success (with or without warnings), 1 nothing to collect,
# 2 the environment cannot run the collector at all.

set -u -o pipefail

SCRIPT_VERSION="1.0.0"

CONTAINER="${ASTRAEUS_CONTAINER:-}"
OUT_DIR="${ASTRAEUS_OUT_DIR:-$PWD}"
SAMPLE_SECONDS="${ASTRAEUS_SAMPLE_SECONDS:-30}"
SAMPLE_INTERVAL="${ASTRAEUS_SAMPLE_INTERVAL:-5}"
LOG_TAIL="${ASTRAEUS_LOG_TAIL:-3000}"
CURL_TIMEOUT="${ASTRAEUS_CURL_TIMEOUT:-10}"
COPY_DB="${ASTRAEUS_COPY_DB:-1}"
COPY_STREAM_LISTING="${ASTRAEUS_COPY_STREAM_LISTING:-1}"
REDACT="${ASTRAEUS_REDACT:-1}"
# When >0, finish with an active-session snapshot instead of sampling: start the
# line below, paste the stream URL into a player, and hit Ctrl-C. See README.md.
CAPTURE_PID="${ASTRAEUS_CAPTURE_PID:-0}"

WARNINGS=0
DOCKER=docker

# ---------------------------------------------------------------------------
# output helpers
# ---------------------------------------------------------------------------

say()  { printf '%s\n' "$*"; }
note() { printf '  %s\n' "$*"; }
warn() { WARNINGS=$((WARNINGS + 1)); printf 'WARN: %s\n' "$*" >&2; }
die()  { printf 'ERROR: %s\n' "$*" >&2; exit "${2:-2}"; }

have() { command -v "$1" >/dev/null 2>&1; }

# write <relative path> <<'EOF' ... EOF
write_file() {
    local rel="$1"
    mkdir -p "$STAGE/$(dirname "$rel")"
    cat >"$STAGE/$rel"
}

# run <relative path> <command...> - capture stdout+stderr, record failures
run() {
    local rel="$1"; shift
    local out
    out="$("$@" 2>&1)"
    local rc=$?
    write_file "$rel" <<EOF
# command: $*
# exit: $rc
$out
EOF
    return $rc
}

# Pretty-print a JSON stream when jq is available, otherwise pass it through.
# The reader's `jq` is a convenience, not a requirement of the bundle.
pretty() {
    jq . 2>/dev/null || cat
}

# ---------------------------------------------------------------------------
# redaction
# ---------------------------------------------------------------------------

# Redaction is one sed program, because the shapes it has to cover look nothing
# alike: an environment block, a coloured slog line, a URL query and a header.
redact_stream() {
    if [ "$REDACT" != "1" ]; then
        cat
        return
    fi
    sed -E \
        -e "s/((api_key|apikey|api-key|token|key|secret|password|authorization|bearer)[\"'\'']*[=:][[:space:]]*[\"'\'']?)[^\"'\''&[:space:]]+/\1<redacted>/Ig" \
        -e "s/(api\.themoviedb\.org[^[:space:]]*[?&]api_key=)[^&[:space:]]+/\1<redacted>/Ig" \
        -e "s/(-{1,2}auth-token[= ][[:space:]]*)[^[:space:]]+/\1<redacted>/Ig" \
        -e "s/(Authorization:[[:space:]]*)[^[:space:]]+(.*)/\1<redacted>\2/Ig"
}

# Redact a whole file in place, and say so in a marker line.
redact_file() {
    local f="$1"
    [ -f "$f" ] || return 0
    if [ "$REDACT" != "1" ]; then
        write_file "${f#"$STAGE"/}.redaction" <<EOF
Redaction was disabled (ASTRAEUS_REDACT=$REDACT): secrets may be present.
EOF
        return 0
    fi
    local tmp
    tmp="$(mktemp)"
    if redact_stream <"$f" >"$tmp"; then
        mv "$tmp" "$f"
    else
        rm -f "$tmp"
        warn "redaction of $f failed; it is left as collected"
    fi
}

redact_tree() {
    [ "$REDACT" = "1" ] || return 0
    local f
    while IFS= read -r -d '' f; do
        redact_file "$f"
    done < <(find "$STAGE" -type f ! -name '*.tar.gz' ! -path "$STAGE/checksums.sha256" -print0)
}

# ---------------------------------------------------------------------------
# container discovery
# ---------------------------------------------------------------------------

# Print the id of a running container that looks like Astraeus. Preference:
# an explicit name, then the entrypoint binary, then the image name.
discover_container() {
    if [ -n "$CONTAINER" ]; then
        if ! $DOCKER inspect "$CONTAINER" >/dev/null 2>&1; then
            die "ASTRAEUS_CONTAINER=$CONTAINER: no such container"
        fi
        printf '%s' "$CONTAINER"
        return 0
    fi

    local ids id entry image
    ids="$($DOCKER ps -q 2>/dev/null)"
    local best=""
    while IFS= read -r id; do
        [ -n "$id" ] || continue
        entry="$(container_field "$id" '{{.Config.Entrypoint}}' 2>/dev/null)"
        image="$(container_field "$id" '{{.Config.Image}}' 2>/dev/null)"
        case "$entry $image" in
            *astraeus-server*)
                if [ -z "$best" ] || [ -z "$entry" ]; then
                    best="$id"
                fi
                ;;
            *astraeus*|*Astraeus*)
                [ -n "$best" ] || best="$id"
                ;;
        esac
    done <<<"$ids"

    if [ -z "$best" ]; then
        norm="$(printf '%s' "$ids" | tr '\n' ' ')"
        die "no running container looks like Astraeus (running: ${norm:-none}). Set ASTRAEUS_CONTAINER=<name|id>." 1
    fi
    printf '%s' "$best"
}

container_field() {
    $DOCKER inspect -f "$2" "$1" 2>/dev/null
}

# ---------------------------------------------------------------------------
# endpoints
# ---------------------------------------------------------------------------

# Ask the container for its listen port. The image defaults to 8642 but the
# tester may have overridden --addr.
server_port() {
    local id="$1" port
    port="$($DOCKER inspect -f '{{range $p, $_ := .NetworkSettings.Ports}}{{$p}} {{end}}' "$id" 2>/dev/null \
        | tr ' ' '\n' | grep -oE '^[0-9]+' | head -1)"
    [ -n "$port" ] || port="$($DOCKER inspect -f '{{range $p, $_ := .Config.ExposedPorts}}{{$p}} {{end}}' "$id" 2>/dev/null \
        | tr ' ' '\n' | grep -oE '^[0-9]+' | head -1)"
    [ -n "$port" ] || port=8642
    printf '%s' "$port"
}

# Print the base URL that answers from this host, or nothing.
published_base_url() {
    local id="$1" port="$2" ip
    ip="$($DOCKER inspect -f "{{with index .NetworkSettings.Ports \"$port/tcp\"}}{{range .}}{{.HostIp}}:{{.HostPort}} {{end}}{{end}}" "$id" 2>/dev/null \
        | tr ' ' '\n' | grep -v '^$' | head -1)"
    [ -n "$ip" ] || return 0
    case "$ip" in
        0.0.0.0:*|::*|\[::\]:*) ip="127.0.0.1:${ip##*:}" ;;
    esac
    printf 'http://%s' "$ip"
}

# Try the published address, then the container network, then localhost.
detect_base_url() {
    local id="$1" port="$2" url candidate
    url="$(published_base_url "$id" "$port")"
    local candidates=()
    [ -n "$url" ] && candidates+=("$url")
    candidates+=("http://127.0.0.1:$port")
    local cip
    cip="$($DOCKER inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}} {{end}}' "$id" 2>/dev/null | tr ' ' '\n' | grep -v '^$' | head -1)"
    [ -n "$cip" ] && candidates+=("http://$cip:$port")
    for candidate in "${candidates[@]}"; do
        if curl -fsS --max-time "$CURL_TIMEOUT" "$candidate/api/health" >/dev/null 2>&1; then
            printf '%s' "$candidate"
            return 0
        fi
    done
    return 1
}

# GET a path from inside the container with curl, which is in the image for its
# own health check. This is the fallback when nothing is published.
fetch_via_container() {
    local id="$1" path="$2" port="$3"
    $DOCKER exec "$id" curl -fsS --max-time "$CURL_TIMEOUT" "http://127.0.0.1:$port$path" 2>&1
}

# GET a path and store the body with a small provenance header.
fetch() {
    local rel="$1" base="$2" path="$3"
    local body rc
    body="$(curl -fsS --max-time "$CURL_TIMEOUT" -D /tmp/astraeus-hdr.$$ "$base$path" 2>&1)"
    rc=$?
    {
        printf '# GET %s\n# exit: %d\n' "$base$path" "$rc"
        [ -f /tmp/astraeus-hdr.$$ ] && sed 's/^/# header: /' /tmp/astraeus-hdr.$$
        printf '\n'
        printf '%s\n' "$body"
    } | write_file "$rel"
    rm -f /tmp/astraeus-hdr.$$
    return $rc
}

# ---------------------------------------------------------------------------
# main
# ---------------------------------------------------------------------------

case "${1:-}" in
    --version)
        printf '%s\n' "$SCRIPT_VERSION"
        exit 0
        ;;
    -h|--help)
        sed -n '2,24p' "$0" | sed 's/^# \{0,1\}//'
        exit 0
        ;;
esac

have $DOCKER || die "$DOCKER is not on PATH; this script runs on the Docker host"
$DOCKER info >/dev/null 2>&1 || die "$DOCKER cannot talk to a daemon (are you in the docker group?)"
have tar || die "tar is not on PATH"
have gzip || die "gzip is not on PATH"

CONTAINER_ID="$(discover_container)"
CONTAINER_NAME="$(container_field "$CONTAINER_ID" '{{.Name}}' | sed 's#^/##')"
IMAGE="$(container_field "$CONTAINER_ID" '{{.Config.Image}}')"
STARTED="$(container_field "$CONTAINER_ID" '{{.State.StartedAt}}')"
PORT="$(server_port "$CONTAINER_ID")"

HOST="$(hostname 2>/dev/null || printf 'unknown')"
STAMP="$(date -u +%Y%m%dT%H%M%SZ)"
STAGE="$(mktemp -d "${TMPDIR:-/tmp}/astraeus-diag.XXXXXX")"
BUNDLE_NAME="astraeus-diag-${HOST}-${STAMP}"
trap 'rm -rf "$STAGE"' EXIT

say "Astraeus diagnostics collector v$SCRIPT_VERSION"
say "  container : $CONTAINER_NAME ($CONTAINER_ID)"
say "  image     : $IMAGE"
say "  started   : $STARTED"
say "  port      : $PORT"
say "  sampling  : ${SAMPLE_SECONDS}s every ${SAMPLE_INTERVAL}s"
say "  staging   : $STAGE"
say ""

# --- 1. provenance ---------------------------------------------------------
mkdir -p "$STAGE/meta"
{
    printf 'bundle=%s\n' "$BUNDLE_NAME"
    printf 'collector_version=%s\n' "$SCRIPT_VERSION"
    printf 'collected_at_utc=%s\n' "$STAMP"
    printf 'collector_host=%s\n' "$HOST"
    printf 'container_id=%s\n' "$CONTAINER_ID"
    printf 'container_name=%s\n' "$CONTAINER_NAME"
    printf 'container_image=%s\n' "$IMAGE"
    printf 'container_started_at=%s\n' "$STARTED"
    printf 'server_port=%s\n' "$PORT"
    printf 'sample_seconds=%s\n' "$SAMPLE_SECONDS"
    printf 'sample_interval=%s\n' "$SAMPLE_INTERVAL"
    printf 'log_tail=%s\n' "$LOG_TAIL"
    printf 'redact=%s\n' "$REDACT"
    printf 'kernel=%s\n' "$(uname -srm)"
    printf 'docker_version=%s\n' "$($DOCKER version --format '{{.Server.Version}}' 2>/dev/null)"
    printf 'git_commit=%s\n' "$(git -C "$(dirname "$0")/.." rev-parse HEAD 2>/dev/null || printf 'unknown')"
    printf 'git_dirty=%s\n' "$(git -C "$(dirname "$0")/.." status --porcelain 2>/dev/null | wc -l | tr -d ' ')"
} >"$STAGE/meta/collector.txt"

{
    printf '#!/bin/sh\n'
    printf '# What this bundle is, generated by collect-astraeus-diagnostics.sh.\n'
    printf 'set -e\n'
    printf 'echo "=== collector.txt ==="\ncat meta/collector.txt\n'
    printf 'echo; echo "=== container/state.txt ==="\ncat container/state.txt 2>/dev/null || true\n'
    printf 'echo; echo "=== metrics: astraeus_* ==="\ngrep -E "^astraeus_" metrics/prometheus.txt 2>/dev/null || true\n'
    printf 'echo; echo "=== logs: warnings and errors ==="\n'
    printf 'grep -iE "(level=(WARN|ERROR)|panic|fatal)" logs/container-stdout-stderr.log 2>/dev/null | tail -100 || true\n'
    printf 'echo; echo "Run scripts/analyze-astraeus-bundle.py for the full report."\n'
} >"$STAGE/README.sh"
chmod +x "$STAGE/README.sh"

# --- 2. container definition and state -------------------------------------
mkdir -p "$STAGE/container"
run container/inspect.json "$DOCKER" inspect "$CONTAINER_ID"
$DOCKER inspect -f '{{json .State}}' "$CONTAINER_ID" 2>/dev/null \
    | pretty | write_file container/state.json
$DOCKER inspect -f '{{json .HostConfig}}' "$CONTAINER_ID" 2>/dev/null \
    | jq '{
            Runtime: .Runtime,
            Binds: .Binds,
            Devices: .Devices,
            DeviceRequests: .DeviceRequests,
            PortBindings: .PortBindings,
            NetworkMode: .NetworkMode,
            RestartPolicy: .RestartPolicy,
            Memory: .Memory,
            NanoCpus: .NanoCpus,
            CpuQuota: .CpuQuota,
            Privileged: .Privileged,
            ReadonlyRootfs: .ReadonlyRootfs,
            CapAdd: .CapAdd
        }' 2>/dev/null | pretty | write_file container/hostconfig-summary.json
$DOCKER inspect -f '{{json .Config.Env}}' "$CONTAINER_ID" 2>/dev/null \
    | pretty | write_file container/env.json
$DOCKER inspect -f '{{json .Config.Cmd}}' "$CONTAINER_ID" 2>/dev/null | write_file container/cmd.json
$DOCKER inspect -f '{{json .Config.Entrypoint}}' "$CONTAINER_ID" 2>/dev/null | write_file container/entrypoint.json
$DOCKER inspect -f '{{json .Mounts}}' "$CONTAINER_ID" 2>/dev/null \
    | pretty | write_file container/mounts.json
{
    printf 'state=%s\n' "$(container_field "$CONTAINER_ID" '{{.State.Status}}')"
    printf 'health=%s\n' "$(container_field "$CONTAINER_ID" '{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}')"
    printf 'restart_count=%s\n' "$(container_field "$CONTAINER_ID" '{{.RestartCount}}')"
    printf 'oom_killed=%s\n' "$(container_field "$CONTAINER_ID" '{{.State.OOMKilled}}')"
    printf 'exit_code=%s\n' "$(container_field "$CONTAINER_ID" '{{.State.ExitCode}}')"
    printf 'pid=%s\n' "$(container_field "$CONTAINER_ID" '{{.State.Pid}}')"
    printf 'image_id=%s\n' "$(container_field "$CONTAINER_ID" '{{.Image}}')"
    printf 'image_version_label=%s\n' "$(container_field "$CONTAINER_ID" '{{index .Config.Labels "org.opencontainers.image.version"}}')"
    printf 'image_revision_label=%s\n' "$(container_field "$CONTAINER_ID" '{{index .Config.Labels "org.opencontainers.image.revision"}}')"
} >"$STAGE/container/state.txt"
$DOCKER inspect -f '{{json .State.Health}}' "$CONTAINER_ID" 2>/dev/null \
    | pretty | write_file container/health.json
run container/image-inspect.json "$DOCKER" image inspect "$(container_field "$CONTAINER_ID" '{{.Image}}')"

# --- 3. logs -----------------------------------------------------------------
mkdir -p "$STAGE/logs"
say "[1/7] logs"
$DOCKER logs --timestamps "$CONTAINER_ID" >"$STAGE/logs/container-stdout-stderr.log" 2>&1
LOG_LINES="$(wc -l <"$STAGE/logs/container-stdout-stderr.log" | tr -d ' ')"
$DOCKER logs --timestamps --tail "$LOG_TAIL" "$CONTAINER_ID" >"$STAGE/logs/container-tail.log" 2>&1
$DOCKER logs --timestamps --tail "$LOG_TAIL" "$CONTAINER_ID" 2>&1 \
    | grep -iE 'level=(WARN|ERROR)|panic|fatal|error' >"$STAGE/logs/errors-warnings.log" 2>&1 || true
$DOCKER logs --timestamps --tail "$LOG_TAIL" "$CONTAINER_ID" 2>&1 \
    | grep -iE 'hardware encoder rejected|no hardware encoder|render_node|encoder performed|image_subtitles|ocr|tesseract' \
    >"$STAGE/logs/hardware-and-ocr.log" 2>&1 || true
{
    printf 'total_lines=%s\n' "$LOG_LINES"
    printf 'tail_lines=%s\n' "$LOG_TAIL"
    printf 'first_line=%s\n' "$(head -1 "$STAGE/logs/container-stdout-stderr.log")"
    printf 'last_line=%s\n' "$(tail -1 "$STAGE/logs/container-stdout-stderr.log")"
    printf 'error_lines=%s\n' "$(wc -l <"$STAGE/logs/errors-warnings.log" | tr -d ' ')"
} >"$STAGE/logs/summary.txt"
note "$LOG_LINES lines captured"

# --- 4. HTTP endpoints -------------------------------------------------------
say "[2/7] HTTP endpoints"
mkdir -p "$STAGE/api" "$STAGE/metrics"
BASE_URL="$(detect_base_url "$CONTAINER_ID" "$PORT")" || BASE_URL=""
if [ -n "$BASE_URL" ]; then
    note "base url: $BASE_URL"
    printf '%s\n' "$BASE_URL" >"$STAGE/meta/base-url.txt"
    fetch api/health.json          "$BASE_URL" /api/health               >/dev/null 2>&1 || warn "GET /api/health failed"
    fetch api/capabilities.json    "$BASE_URL" /api/system/capabilities  >/dev/null 2>&1 || warn "GET /api/system/capabilities failed"
    fetch metrics/prometheus.txt   "$BASE_URL" /metrics                  >/dev/null 2>&1 || warn "GET /metrics failed"
    fetch api/libraries.json       "$BASE_URL" /api/libraries            >/dev/null 2>&1 || true
else
    warn "no endpoint answered from this host; falling back to docker exec curl"
    printf 'published=none (collected through docker exec)\n' >"$STAGE/meta/base-url.txt"
    fetch_via_container "$CONTAINER_ID" /api/health "$PORT" >"$STAGE/api/health.json" 2>&1 || warn "GET /api/health failed"
    fetch_via_container "$CONTAINER_ID" /api/system/capabilities "$PORT" >"$STAGE/api/capabilities.json" 2>&1 || warn "GET /api/system/capabilities failed"
    fetch_via_container "$CONTAINER_ID" /metrics "$PORT" >"$STAGE/metrics/prometheus.txt" 2>&1 || warn "GET /metrics failed"
    fetch_via_container "$CONTAINER_ID" /api/libraries "$PORT" >"$STAGE/api/libraries.json" 2>&1 || true
fi
# Strip the provenance header block before anything parses these as JSON.
for f in "$STAGE"/api/*.json; do
    [ -f "$f" ] || continue
    sed -i '/^# /d' "$f" 2>/dev/null || true
done
if have jq; then
    for f in "$STAGE"/api/*.json; do
        [ -s "$f" ] || continue
        if jq . "$f" >"$f.pretty" 2>/dev/null; then
            mv "$f.pretty" "$f"
        else
            rm -f "$f.pretty"
        fi
    done
fi

# --- 5. in-container fact-finding -------------------------------------------
say "[3/7] in-container fact-finding"
mkdir -p "$STAGE/inside"
run inside/server-binary.txt "$DOCKER" exec "$CONTAINER_ID" /usr/local/bin/astraeus-server version
run inside/ffmpeg-version.txt "$DOCKER" exec "$CONTAINER_ID" ffmpeg -version
run inside/ffmpeg-encoders.txt "$DOCKER" exec "$CONTAINER_ID" ffmpeg -hide_banner -encoders
run inside/ffmpeg-hwaccels.txt "$DOCKER" exec "$CONTAINER_ID" ffmpeg -hide_banner -hwaccels
run inside/ffmpeg-devices.txt "$DOCKER" exec "$CONTAINER_ID" sh -c \
    'ls -l /dev/dri 2>&1; echo "--- /dev/nvidia* ---"; ls -l /dev/nvidia* 2>&1; echo "--- /dev/dri by-id ---"; ls -l /dev/dri/by-path 2>&1'
run inside/tesseract-version.txt "$DOCKER" exec "$CONTAINER_ID" tesseract --version
run inside/identity-and-data.txt "$DOCKER" exec "$CONTAINER_ID" sh -c \
    'id; echo "--- /data ---"; ls -la /data; echo "--- /data/streams ---"; ls -la /data/streams 2>/dev/null | head -50; echo "--- /data/subtitles ---"; ls -la /data/subtitles 2>/dev/null | head -50; echo "--- ffmpeg processes ---"; ps -eo pid,ppid,etime,pcpu,pmem,args 2>/dev/null | grep -E "ffmpeg|astraeus" | grep -v grep'
run inside/cgroup-memory.txt "$DOCKER" exec "$CONTAINER_ID" sh -c \
    'cat /sys/fs/cgroup/memory.max 2>/dev/null; cat /sys/fs/cgroup/memory.current 2>/dev/null; cat /sys/fs/cgroup/cpu.max 2>/dev/null; nproc'

# --- 6. database and stream state -------------------------------------------
say "[4/7] database snapshot"
mkdir -p "$STAGE/database"
DB_PATH="$($DOCKER inspect -f '{{json .Config.Cmd}}' "$CONTAINER_ID" 2>/dev/null \
    | tr ',' '\n' | grep -A1 '"--db"' | tail -1 | tr -d '" ' )"
[ -n "$DB_PATH" ] || DB_PATH=/data/astraeus.db
printf 'db_path=%s\n' "$DB_PATH" >"$STAGE/database/path.txt"
if [ "$COPY_DB" = "1" ]; then
    # SQLite in WAL mode: copy all three files together, in one exec, so the
    # snapshot is not a torn read.
    if $DOCKER exec "$CONTAINER_ID" sh -c \
        'rm -rf /tmp/astraeus-diag-db && mkdir -p /tmp/astraeus-diag-db && cp "$1" /tmp/astraeus-diag-db/ && for s in -wal -shm; do [ -f "$1$s" ] && cp "$1$s" /tmp/astraeus-diag-db/; done; ls -la /tmp/astraeus-diag-db' \
        sh "$DB_PATH" \
        >"$STAGE/database/copy.log" 2>&1; then
        if $DOCKER cp "$CONTAINER_ID:/tmp/astraeus-diag-db/." "$STAGE/database/" >/dev/null 2>&1; then
            $DOCKER exec "$CONTAINER_ID" rm -rf /tmp/astraeus-diag-db
            note "database copied: $(du -sh "$STAGE/database" 2>/dev/null | cut -f1)"
        else
            warn "docker cp of the database failed; see database/copy.log"
        fi
    else
        warn "could not copy the database inside the container; see database/copy.log"
    fi
else
    note "database copy disabled (ASTRAEUS_COPY_DB=$COPY_DB)"
fi

mkdir -p "$STAGE/state"
if [ "$COPY_STREAM_LISTING" = "1" ]; then
    $DOCKER exec "$CONTAINER_ID" sh -c \
        'find /data/streams -maxdepth 2 -printf "%T@ %s %p\n" 2>/dev/null | sort -n | tail -200' \
        >"$STAGE/state/stream-root-listing.txt" 2>&1 || true
fi
{
    printf 'timestamp_utc=%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    printf '\n# docker stats (one shot)\n'
    $DOCKER stats --no-stream --format \
        'container={{.Name}} cpu={{.CPUPerc}} mem={{.MemUsage}} mem_perc={{.MemPerc}} net={{.NetIO}} block={{.BlockIO}} pids={{.PIDs}}' \
        "$CONTAINER_ID" 2>&1
    printf '\n# docker top\n'
    $DOCKER top "$CONTAINER_ID" 2>&1 | head -40
} >"$STAGE/state/processes.txt"

# --- 7. sampling -------------------------------------------------------------
say "[5/7] sampling (${SAMPLE_SECONDS}s)"
mkdir -p "$STAGE/samples"
if [ "$CAPTURE_PID" -gt 0 ] 2>/dev/null; then
    note "capture mode: PID $CAPTURE_PID; finish the stream interaction to stop"
    DEADLINE=$(( $(date +%s) + 600 ))
    while kill -0 "$CAPTURE_PID" 2>/dev/null && [ "$(date +%s)" -lt "$DEADLINE" ]; do
        $DOCKER stats --no-stream --format \
            "{{.CPUPerc}}\t{{.MemUsage}}\t{{.MemPerc}}\t{{.PIDs}}" "$CONTAINER_ID" 2>/dev/null \
            | sed "s/^/$(date -u +%Y-%m-%dT%H:%M:%SZ)\t/" >>"$STAGE/samples/docker-stats.tsv"
        sleep 2
    done
else
    END=$(( $(date +%s) + SAMPLE_SECONDS ))
    printf 'timestamp_utc\tcpu_percent\tmem_usage\tmem_percent\tpids\n' >"$STAGE/samples/docker-stats.tsv"
    while [ "$(date +%s)" -lt "$END" ]; do
        $DOCKER stats --no-stream --format \
            "{{.CPUPerc}}\t{{.MemUsage}}\t{{.MemPerc}}\t{{.PIDs}}" "$CONTAINER_ID" 2>/dev/null \
            | sed "s/^/$(date -u +%Y-%m-%dT%H:%M:%SZ)\t/" >>"$STAGE/samples/docker-stats.tsv"
        sleep "$SAMPLE_INTERVAL"
    done
fi
if [ -n "$BASE_URL" ]; then
    curl -fsS --max-time "$CURL_TIMEOUT" "$BASE_URL/metrics" 2>/dev/null \
        >"$STAGE/samples/metrics-after-sampling.txt" || true
fi

# --- 8. host facts -----------------------------------------------------------
say "[6/7] host facts"
mkdir -p "$STAGE/host"
{
    printf 'nproc=%s\n' "$(nproc 2>/dev/null)"
    printf 'cpu_model=%s\n' "$(grep -m1 'model name' /proc/cpuinfo 2>/dev/null | cut -d: -f2- | sed 's/^ //')"
    printf 'mem_total=%s\n' "$(grep -m1 MemTotal /proc/meminfo 2>/dev/null)"
    printf 'kernel=%s\n' "$(uname -srm)"
    printf 'driver_i915_loaded=%s\n' "$(lsmod 2>/dev/null | grep -c '^i915')"
    printf 'driver_nvidia_loaded=%s\n' "$(lsmod 2>/dev/null | grep -c '^nvidia')"
} >"$STAGE/host/host.txt"
run host/lspci-vga.txt sh -c "lspci -nnk 2>/dev/null | grep -A3 -iE 'vga|3d|display' || true"
run host/dri-nodes.txt sh -c "ls -l /dev/dri 2>&1; echo '---'; for d in /dev/dri/renderD*; do [ -e \"\$d\" ] || continue; echo \"== \$d\"; (command -v vainfo >/dev/null && vainfo --display drm --device \"\$d\" 2>&1 | head -40) || echo 'vainfo not installed'; done"
run host/nvidia-smi.txt sh -c "command -v nvidia-smi >/dev/null && nvidia-smi 2>&1 || echo 'nvidia-smi not installed (no NVIDIA driver userspace on this host)'"
run host/nvidia-container-toolkit.txt sh -c "command -v nvidia-ctk >/dev/null && nvidia-ctk --version 2>&1 || echo 'nvidia-ctk not installed'; echo '---'; cat /etc/nvidia-container-runtime/config.toml 2>/dev/null || echo 'no /etc/nvidia-container-runtime/config.toml'"
run host/docker-info.txt sh -c "$DOCKER info 2>&1 | grep -iE 'runtime|nvidia|driver|server version|storage driver|cgroup|kernel|operating system|architecture' || true"
run host/docker-runtimes.txt sh -c "$DOCKER info --format '{{json .Runtimes}}' 2>&1 || true"

# A GPU that is visible on the host but absent in the container is the single
# most likely explanation for "the GPU is not being used", so put both answers
# side by side where a reader cannot miss the disagreement.
{
    printf 'host render nodes: %s\n' "$(ls /dev/dri 2>/dev/null | tr '\n' ' ')"
    printf 'host nvidia devices: %s\n' "$(ls /dev/nvidia* 2>/dev/null | tr '\n' ' ')"
    printf 'host nvidia-smi: %s\n' "$(command -v nvidia-smi >/dev/null && nvidia-smi --query-gpu=name,driver_version --format=csv,noheader 2>/dev/null | tr '\n' '; ' || printf 'absent')"
    printf 'host docker runtimes: %s\n' "$($DOCKER info --format '{{json .Runtimes}}' 2>/dev/null)"
    printf 'container devices (HostConfig.Devices): %s\n' "$(container_field "$CONTAINER_ID" '{{json .HostConfig.Devices}}')"
    printf 'container device requests (HostConfig.DeviceRequests): %s\n' "$(container_field "$CONTAINER_ID" '{{json .HostConfig.DeviceRequests}}')"
    printf 'container runtime: %s\n' "$(container_field "$CONTAINER_ID" '{{.HostConfig.Runtime}}')"
} >"$STAGE/host/gpu-visibility.txt"

# --- 9. packaging ------------------------------------------------------------
say "[7/7] packaging"
redact_tree
write_file meta/redaction.txt <<EOF
redact=$REDACT
policy: api keys, tokens, secrets, passwords, authorization headers and
TMDB \`api_key=\` query values are replaced with <redacted>. Disable with
ASTRAEUS_REDACT=0 only if a value is needed to reproduce a bug.
EOF

mkdir -p "$OUT_DIR"
BUNDLE="$OUT_DIR/$BUNDLE_NAME.tar.gz"
( cd "$STAGE" && find . -type f ! -name 'checksums.sha256' -print0 | sort -z | xargs -0 sha256sum ) \
    >"$STAGE/checksums.sha256" 2>/dev/null || true
( cd "$STAGE" && tar czf "$BUNDLE" . ) || die "tar failed" 2

SIZE="$(du -h "$BUNDLE" | cut -f1)"
HASH="$(sha256sum "$BUNDLE" | cut -d' ' -f1)"
say ""
say "Bundle: $BUNDLE"
say "  size   : $SIZE"
say "  sha256 : $HASH"
if [ "$WARNINGS" -gt 0 ]; then
    say "  warnings: $WARNINGS (collection continued; the bundle says where)"
else
    say "  warnings: none"
fi
say ""
say "Check it before sending:"
say "  tar tzf $(basename "$BUNDLE") | head -40"
say "  scripts/analyze-astraeus-bundle.py $(basename "$BUNDLE") > report.md"
exit 0
