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
PROCESS="${ASTRAEUS_PROCESS:-}"
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
# process discovery (a binary run directly, not in a container)
# ---------------------------------------------------------------------------

# Print the PID of a running astraeus-server. Preference: an explicit pid or
# name in ASTRAEUS_PROCESS, then whatever is listening, then the process table.
discover_process() {
    if [ -n "$PROCESS" ]; then
        case "$PROCESS" in
            *[!0-9]*) pgrep -f "$PROCESS" 2>/dev/null | head -1 ;;
            *) printf '%s' "$PROCESS" ;;
        esac | {
            read -r pid
            if [ -z "$pid" ] || [ ! -d "/proc/$pid" ]; then
                die "ASTRAEUS_PROCESS=$PROCESS: no such running process" 1
            fi
            printf '%s' "$pid"
        }
        return 0
    fi

    local pid
    # `pgrep -f astraeus-server` also matches this script's own command line in
    # some shells, so require the process name to match as well.
    pid="$(pgrep -x astraeus-server 2>/dev/null | head -1)"
    [ -n "$pid" ] || pid="$(pgrep -f '[a]straeus-server serve' 2>/dev/null | head -1)"
    if [ -z "$pid" ]; then
        die "no running astraeus-server found. Set ASTRAEUS_PROCESS=<pid|name>, or use ASTRAEUS_CONTAINER=<name> for a container." 1
    fi
    printf '%s' "$pid"
}

process_bin() {
    readlink -f "/proc/$1/exe" 2>/dev/null
}

process_cwd() {
    readlink -f "/proc/$1/cwd" 2>/dev/null
}

process_argv() {
    tr '\0' ' ' <"/proc/$1/cmdline" 2>/dev/null
}

# The port the process is listening on, from its own file descriptors. The
# command line only carries one when --addr was passed.
process_port() {
    local pid="$1" port
    port="$(printf '%s' "$(process_argv "$pid")" | tr ' ' '\n' \
        | grep -oE '^[0-9]{1,5}$' | head -1)"
    [ -n "$port" ] || port="$(ss -ltnp 2>/dev/null | grep "pid=$pid," \
        | grep -oE ':[0-9]+' | head -1 | tr -d ':')"
    [ -n "$port" ] || port="$(tr '\0' '\n' <"/proc/$pid/environ" 2>/dev/null \
        | grep -iE '^(ASTRAEUS_)?(ADDR|PORT)=' | head -1 | cut -d= -f2 | grep -oE '[0-9]+$')"
    [ -n "$port" ] || port=8642
    printf '%s' "$port"
}

# How long the process has been up, from /proc/<pid>/stat field 22 (start time
# in clock ticks since boot), which is the only monotonic answer available.
process_uptime() {
    local pid="$1" start now hz
    start="$(awk '{print $22}' "/proc/$pid/stat" 2>/dev/null)"
    [ -n "$start" ] || return 0
    hz="$(getconf CLK_TCK 2>/dev/null || printf '100')"
    now="$(awk '{print $1}' /proc/uptime 2>/dev/null)"
    [ -n "$now" ] || return 0
    awk -v n="$now" -v s="$start" -v hz="$hz" 'BEGIN { printf "%d", n - s/hz }'
}

# Read /proc/<pid>/stat into a compact, one-line-per-sample row. Fields per
# proc(5): 14 utime, 15 stime, 20 num_threads, 23 vsize (bytes), 24 rss (pages).
# The executable name in field 2 is parenthesised and may contain spaces, but it
# is field 2 either way, so the numeric fields are stable.
process_sample() {
    local pid="$1"
    awk '{printf "%s\t%s\t%s\t%s\t%s", $14, $15, $20, $23, $24}' "/proc/$pid/stat" 2>/dev/null
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

# Try the published address, then the container network, then localhost. In
# process mode the server is on this host, so the address flag or the default
# loopback is all there is.
detect_base_url() {
    local id="$1" port="$2" url candidate
    if [ "$MODE" = "process" ]; then
        candidates=("http://127.0.0.1:$port")
        local addr
        addr="$(printf '%s' "$(process_argv "$PID")" | tr ' ' '\n' \
            | grep -A1 -x -- '--addr' | tail -1)"
        case "$addr" in
            *:[0-9]*) candidates=("http://${addr%:*}:${addr##*:}" "${candidates[@]}") ;;
        esac
        for candidate in "${candidates[@]}"; do
            if curl -fsS --max-time "$CURL_TIMEOUT" "$candidate/api/health" >/dev/null 2>&1; then
                printf '%s' "$candidate"
                return 0
            fi
        done
        return 1
    fi
    url="$(published_base_url "$id" "$port")"
    candidates=()
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

have tar || die "tar is not on PATH"
have gzip || die "gzip is not on PATH"

# Which shape is this instance? A container and a plain binary expose the same
# HTTP surface but nothing else in common: one is read through `docker`, the
# other through /proc. Prefer the container when Docker is usable, so a host
# running both collects the one the operator named.
MODE=""
if [ -n "$PROCESS" ]; then
    MODE="process"
elif [ -n "$CONTAINER" ]; then
    MODE="container"
elif have $DOCKER && $DOCKER info >/dev/null 2>&1; then
    if CONTAINER_ID="$(discover_container 2>/dev/null)"; then
        MODE="container"
    elif PID="$(pgrep -x astraeus-server 2>/dev/null | head -1)"; then
        MODE="process"
    fi
elif PID="$(pgrep -x astraeus-server 2>/dev/null | head -1)"; then
    MODE="process"
fi

case "$MODE" in
    container) ;;
    process) ;;
    "")
        die "found no Astraeus instance. Start one, or set ASTRAEUS_CONTAINER=<name> for a container or ASTRAEUS_PROCESS=<pid|name> for a binary." 1
        ;;
esac

HOST="$(hostname 2>/dev/null || printf 'unknown')"
STAMP="$(date -u +%Y%m%dT%H%M%SZ)"
STAGE="$(mktemp -d "${TMPDIR:-/tmp}/astraeus-diag.XXXXXX")"
BUNDLE_NAME="astraeus-diag-${HOST}-${STAMP}"
trap 'rm -rf "$STAGE"' EXIT

if [ "$MODE" = "container" ]; then
    [ -n "${CONTAINER_ID:-}" ] || CONTAINER_ID="$(discover_container)"
    $DOCKER info >/dev/null 2>&1 || die "$DOCKER cannot talk to a daemon (are you in the docker group?)"
    CONTAINER_NAME="$(container_field "$CONTAINER_ID" '{{.Name}}' | sed 's#^/##')"
    IMAGE="$(container_field "$CONTAINER_ID" '{{.Config.Image}}')"
    STARTED="$(container_field "$CONTAINER_ID" '{{.State.StartedAt}}')"
    PORT="$(server_port "$CONTAINER_ID")"
else
    [ -n "${PID:-}" ] || PID="$(discover_process)"
    CONTAINER_ID=""
    CONTAINER_NAME="astraeus-server"
    IMAGE="$(process_bin "$PID")"
    STARTED="$(process_uptime "$PID")s ago (uptime), pid $PID"
    PORT="$(process_port "$PID")"
fi

# How to reach "inside the deployment": docker exec for a container, direct
# execution for a process. Defined here so `run IN ...` works below.
if [ "$MODE" = "container" ]; then
    IN() { $DOCKER exec "$CONTAINER_ID" "$@"; }
else
    IN() { "$@"; }
fi

say "Astraeus diagnostics collector v$SCRIPT_VERSION"
say "  mode      : $MODE"
if [ "$MODE" = "container" ]; then
    say "  container : $CONTAINER_NAME ($CONTAINER_ID)"
    say "  image     : $IMAGE"
else
    say "  process   : pid $PID"
    say "  binary    : $IMAGE"
    say "  cwd       : $(process_cwd "$PID")"
    say "  argv      : $(process_argv "$PID")"
fi
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
    printf 'mode=%s\n' "$MODE"
    if [ "$MODE" = "container" ]; then
        printf 'container_id=%s\n' "$CONTAINER_ID"
        printf 'container_name=%s\n' "$CONTAINER_NAME"
        printf 'container_image=%s\n' "$IMAGE"
        printf 'container_started_at=%s\n' "$STARTED"
    else
        printf 'process_pid=%s\n' "$PID"
        printf 'process_binary=%s\n' "$IMAGE"
        printf 'process_cwd=%s\n' "$(process_cwd "$PID")"
        printf 'process_argv=%s\n' "$(process_argv "$PID")"
        printf 'process_uptime_seconds=%s\n' "$(process_uptime "$PID")"
    fi
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
    printf '# Run it from anywhere: it reads the files beside itself.\n'
    printf 'set -e\n'
    printf 'cd "$(dirname "$0")"\n'
    printf 'echo "=== collector.txt ==="\ncat meta/collector.txt\n'
    printf 'echo; echo "=== container/state.txt ==="\ncat container/state.txt 2>/dev/null || true\n'
    printf 'echo; echo "=== process/state.txt ==="\ncat process/state.txt 2>/dev/null || true\n'
    printf 'echo; echo "=== metrics: astraeus_* ==="\ngrep -E "^astraeus_" metrics/prometheus.txt 2>/dev/null || true\n'
    printf 'echo; echo "=== logs: warnings and errors ==="\n'
    printf 'grep -iE "(level=(WARN|ERROR)|panic|fatal)" logs/container-stdout-stderr.log 2>/dev/null | tail -100 || true\n'
    printf 'echo; echo "Run scripts/analyze-astraeus-bundle.py on the tarball for the full report."\n'
} >"$STAGE/README.sh"
chmod +x "$STAGE/README.sh"

if [ "$MODE" = "process" ]; then
    # --- 2p. process state, the /proc equivalent of `docker inspect` ----------
    mkdir -p "$STAGE/process" "$STAGE/container"
    {
        printf 'pid=%s\n' "$PID"
        printf 'binary=%s\n' "$(process_bin "$PID")"
        printf 'cwd=%s\n' "$(process_cwd "$PID")"
        printf 'argv=%s\n' "$(process_argv "$PID")"
        printf 'uptime_seconds=%s\n' "$(process_uptime "$PID")"
        printf 'uid=%s\n' "$(awk '/^Uid:/{print $2}' "/proc/$PID/status" 2>/dev/null)"
        printf 'state=%s\n' "$(awk '/^State:/{print $2, $3}' "/proc/$PID/status" 2>/dev/null)"
        printf 'threads=%s\n' "$(awk '/^Threads:/{print $2}' "/proc/$PID/status" 2>/dev/null)"
        printf 'vm_rss_kb=%s\n' "$(awk '/^VmRSS:/{print $2}' "/proc/$PID/status" 2>/dev/null)"
        printf 'vm_peak_kb=%s\n' "$(awk '/^VmPeak:/{print $2}' "/proc/$PID/status" 2>/dev/null)"
        printf 'fd_count=%s\n' "$(ls "/proc/$PID/fd" 2>/dev/null | wc -l | tr -d ' ')"
    } >"$STAGE/process/state.txt"
    run process/cgroup.txt sh -c "cat /proc/$PID/cgroup 2>&1; echo '== limits =='; cat /sys/fs/cgroup\$(sed -n 's/^0:://p' /proc/$PID/cgroup)/memory.max 2>/dev/null"
    run process/environ.txt sh -c "tr '\\0' '\\n' < /proc/$PID/environ 2>&1 | sort"
    run process/mountinfo.txt sh -c "cat /proc/$PID/mountinfo 2>&1 | awk '{print \$5, \$9}' | head -40"
    run process/dri-and-media.txt sh -c "/usr/bin/ls -l /dev/dri 2>&1; echo '== media mounts =='; findmnt -rno TARGET,SOURCE 2>/dev/null | head -20 || true"
    # The command line and env are what identify a binary deployment, so put a
    # readable copy where the report looks for it.
    printf 'mode=process (not a container; no docker inspect applies)\n' \
        >"$STAGE/container/state.txt"
else
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
fi

# --- 3. logs -----------------------------------------------------------------
mkdir -p "$STAGE/logs"
say "[1/7] logs"
FULL_LOG="$STAGE/logs/container-stdout-stderr.log"
if [ "$MODE" = "container" ]; then
    $DOCKER logs --timestamps "$CONTAINER_ID" >"$FULL_LOG" 2>&1
else
    # The binary's own stdout/stderr. A server started in a terminal writes to a
    # pty and keeps no history, so there is usually nothing here - say that
    # rather than writing an empty file and letting the reader guess.
    : >"$FULL_LOG"
    {
        printf 'mode=process\n'
        printf 'stdout=%s\n' "$(readlink "/proc/$PID/fd/1" 2>/dev/null)"
        printf 'stderr=%s\n' "$(readlink "/proc/$PID/fd/2" 2>/dev/null)"
        printf 'journal_unit=%s\n' "$(systemctl status "$PID" 2>/dev/null | head -1 || true)"
    } >"$STAGE/logs/process-output.txt"
    # If the operator started it under a shell that redirected to a file, that
    # file is not discoverable from here; if it is a systemd unit, it is.
    UNIT="$(systemctl list-units --type=service --all --no-legend 2>/dev/null \
        | awk '{print $1}' | grep -i astraeus | head -1)"
    if [ -n "$UNIT" ]; then
        journalctl -u "$UNIT" --no-pager -o short-iso 2>/dev/null >"$FULL_LOG" || true
        printf 'journal_unit=%s\n' "$UNIT" >>"$STAGE/logs/process-output.txt"
        note "captured from journalctl -u $UNIT"
    fi
fi
LOG_LINES="$(wc -l <"$FULL_LOG" | tr -d ' ')"
# Derive the filtered views from the full capture rather than asking the daemon
# again: `docker logs --tail` without --timestamps drops the stamps, and the
# filtered files are the ones a person actually reads.
tail -n "$LOG_TAIL" "$FULL_LOG" >"$STAGE/logs/container-tail.log"
grep -iE 'level=(WARN|ERROR)|panic|fatal|"error"| error=' "$FULL_LOG" \
    >"$STAGE/logs/errors-warnings.log" 2>/dev/null || true
grep -iE 'hardware encoder rejected|no hardware encoder|render_node|server capability|image_subtitles|ocr|tesseract|ffmpeg' "$FULL_LOG" \
    >"$STAGE/logs/hardware-and-ocr.log" 2>/dev/null || true
grep -iE 'http request' "$FULL_LOG" >"$STAGE/logs/http-requests.log" 2>/dev/null || true
{
    printf 'total_lines=%s\n' "$LOG_LINES"
    printf 'tail_lines=%s\n' "$LOG_TAIL"
    printf 'first_line=%s\n' "$(head -1 "$FULL_LOG")"
    printf 'last_line=%s\n' "$(tail -1 "$FULL_LOG")"
    printf 'warn_error_lines=%s\n' "$(wc -l <"$STAGE/logs/errors-warnings.log" | tr -d ' ')"
    printf 'http_request_lines=%s\n' "$(wc -l <"$STAGE/logs/http-requests.log" | tr -d ' ')"
} >"$STAGE/logs/summary.txt"
if [ "$LOG_LINES" -eq 0 ]; then
    if [ "$MODE" = "container" ]; then
        warn "the container log is empty. If the daemon uses the journald or a plugin \
logging driver, \`docker logs\` cannot read it; collect it from that driver instead."
    else
        warn "no log was captured: the binary is writing to $(readlink "/proc/$PID/fd/2" 2>/dev/null || printf 'an unknown destination') \
and a terminal keeps no history. Set the server's log destination before restarting it, or \
collect logs from the terminal's scrollback, or use \`--log-format json\` with a redirect."
    fi
fi
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
if [ "$MODE" = "container" ]; then
    run inside/server-binary.txt IN /usr/local/bin/astraeus-server version
else
    # The image path does not exist on a host; `version` is a flag on the same
    # binary the process is running.
    run inside/server-binary.txt IN "$(process_bin "$PID")" version
fi
run inside/ffmpeg-version.txt IN ffmpeg -version
run inside/ffmpeg-encoders.txt IN ffmpeg -hide_banner -encoders
run inside/ffmpeg-hwaccels.txt IN ffmpeg -hide_banner -hwaccels
run inside/ffmpeg-devices.txt IN sh -c \
    'echo "== /dev/dri =="; /usr/bin/ls -l /dev/dri 2>&1; echo "== /dev/dri/by-path =="; /usr/bin/ls -l /dev/dri/by-path 2>&1; echo "== /dev/nvidia* =="; /usr/bin/ls -l /dev/nvidia* 2>&1; echo "== render nodes =="; /usr/bin/ls /dev/dri/renderD* 2>&1; echo "== nvidia nodes =="; /usr/bin/ls /dev/nvidia[0-9]* 2>&1'
run inside/tesseract-version.txt IN tesseract --version
if [ "$MODE" = "container" ]; then
    run inside/identity-and-data.txt IN sh -c \
        'id; echo "== /data =="; /usr/bin/ls -la /data; echo "== /data/streams =="; /usr/bin/ls -la /data/streams 2>/dev/null | head -50; echo "== /data/subtitles =="; /usr/bin/ls -la /data/subtitles 2>/dev/null | head -50; echo "== processes =="; if command -v ps >/dev/null 2>&1; then ps -eo pid,ppid,etime,pcpu,pmem,args; else echo "ps is not installed; reading /proc instead"; for d in /proc/[0-9]*; do [ -r "$d/cmdline" ] || continue; printf "%s " "${d#/proc/}"; tr "\0" " " <"$d/cmdline"; echo; done; fi'
else
    run inside/identity-and-data.txt IN sh -c \
        'id; echo "== cwd =="; pwd; echo "== cwd listing =="; /usr/bin/ls -la | head -40; echo "== processes =="; ps -eo pid,ppid,etime,pcpu,pmem,args 2>/dev/null | grep -E "astraeus|ffmpeg" | grep -v grep'
fi
run inside/cgroup-memory.txt IN sh -c \
    'cat /sys/fs/cgroup/memory.max 2>/dev/null; cat /sys/fs/cgroup/memory.current 2>/dev/null; cat /sys/fs/cgroup/cpu.max 2>/dev/null; nproc'
# Which userspace drivers the deployment actually carries. A statement about
# the device is not enough for a hardware diagnosis: `--device /dev/dri` alone
# still needs a VAAPI backend, and NVENC needs the libraries the NVIDIA
# container toolkit injects. This is the difference between "not passed in" and
# "not installed".
run inside/vaapi-drivers.txt IN sh -c \
    'echo "== libva =="; /usr/bin/ls -l /usr/lib/x86_64-linux-gnu/libva*.so* 2>&1 | head; echo "== vaapi backends (multiarch) =="; /usr/bin/ls -l /usr/lib/x86_64-linux-gnu/dri/*_drv_video.so 2>&1; echo "== vaapi backends (/usr/lib/dri) =="; /usr/bin/ls -l /usr/lib/dri/*_drv_video.so 2>&1; echo "== vainfo =="; command -v vainfo >/dev/null && vainfo 2>&1 | head -30 || echo "vainfo not installed"'
run inside/nvidia-libs.txt IN sh -c \
    'echo "== libnvidia-encode/libcuda =="; for d in /usr/lib/x86_64-linux-gnu /usr/lib64 /usr/local/nvidia/lib64; do echo "-- $d"; /usr/bin/ls -l "$d"/libnvidia* "$d"/libcuda* 2>&1 | head -20; done; echo "== nvidia-smi =="; command -v nvidia-smi >/dev/null && nvidia-smi 2>&1 | head -20 || echo "nvidia-smi not installed"'

# --- 6. database and stream state -------------------------------------------
say "[4/7] database snapshot"
mkdir -p "$STAGE/database"
if [ "$MODE" = "container" ]; then
    DB_PATH="$($DOCKER inspect -f '{{json .Config.Cmd}}' "$CONTAINER_ID" 2>/dev/null \
        | tr ',' '\n' | grep -A1 '"--db"' | tail -1 | tr -d '" ' )"
    [ -n "$DB_PATH" ] || DB_PATH=/data/astraeus.db
else
    # From the process's own argv, resolved against its working directory.
    DB_PATH="$(printf '%s' "$(process_argv "$PID")" | tr ' ' '\n' \
        | grep -A1 -x -- '--db' | tail -1)"
    [ -n "$DB_PATH" ] || DB_PATH="$(tr '\0' '\n' <"/proc/$PID/environ" 2>/dev/null \
        | sed -n 's/^ASTRAEUS_DB=//p' | head -1)"
    [ -n "$DB_PATH" ] || DB_PATH=astraeus.db
    case "$DB_PATH" in
        /*) ;;
        *) DB_PATH="$(process_cwd "$PID")/$DB_PATH" ;;
    esac
fi
printf 'db_path=%s\n' "$DB_PATH" >"$STAGE/database/path.txt"
if [ "$COPY_DB" = "1" ]; then
    # SQLite in WAL mode: copy all three files together so the snapshot is not a
    # torn read. In process mode there is no copy step to make inside a
    # namespace, so the copy is direct - which also means the source paths have
    # to be quoted everywhere.
    if [ "$MODE" = "container" ]; then
        copy_ok=0
        if $DOCKER exec "$CONTAINER_ID" sh -c \
            'rm -rf /tmp/astraeus-diag-db && mkdir -p /tmp/astraeus-diag-db && cp "$1" /tmp/astraeus-diag-db/ && for s in -wal -shm; do [ -f "$1$s" ] && cp "$1$s" /tmp/astraeus-diag-db/; done; /usr/bin/ls -la /tmp/astraeus-diag-db' \
            sh "$DB_PATH" \
            >"$STAGE/database/copy.log" 2>&1; then
            if $DOCKER cp "$CONTAINER_ID:/tmp/astraeus-diag-db/." "$STAGE/database/" >/dev/null 2>&1; then
                $DOCKER exec "$CONTAINER_ID" rm -rf /tmp/astraeus-diag-db
                copy_ok=1
            else
                warn "docker cp of the database failed; see database/copy.log"
            fi
        else
            warn "could not copy the database inside the container; see database/copy.log"
        fi
    else
        copy_ok=0
        {
            printf 'db_path=%s\n' "$DB_PATH"
            for suffix in "" "-wal" "-shm"; do
                src="$DB_PATH$suffix"
                if [ -f "$src" ]; then
                    if cp "$src" "$STAGE/database/$(basename "$DB_PATH")$suffix" 2>>"$STAGE/database/copy.log"; then
                        printf 'copied %s\n' "$src"
                    else
                        printf 'FAILED %s\n' "$src"
                    fi
                else
                    printf 'absent %s\n' "$src"
                fi
            done
        } >>"$STAGE/database/copy.log" 2>&1
        [ -f "$STAGE/database/$(basename "$DB_PATH")" ] && copy_ok=1
    fi
    if [ "$copy_ok" = "1" ]; then
        if have sqlite3 && [ -f "$STAGE/database/$(basename "$DB_PATH")" ]; then
            # A integrity check is the difference between a snapshot and a
            # plausible-looking corrupt file.
            sqlite3 "$STAGE/database/$(basename "$DB_PATH")" 'PRAGMA integrity_check;' \
                >"$STAGE/database/integrity-check.txt" 2>&1 || true
        fi
        note "database copied: $(du -sh "$STAGE/database" 2>/dev/null | cut -f1)"
    fi
else
    note "database copy disabled (ASTRAEUS_COPY_DB=$COPY_DB)"
fi

mkdir -p "$STAGE/state"
if [ "$COPY_STREAM_LISTING" = "1" ]; then
    STREAM_ROOT=""
    if [ "$MODE" = "container" ]; then
        STREAM_ROOT="$($DOCKER inspect -f '{{json .Config.Cmd}}' "$CONTAINER_ID" 2>/dev/null \
            | tr ',' '\n' | grep -A1 '"--stream-root"' | tail -1 | tr -d '" ')"
    else
        STREAM_ROOT="$(printf '%s' "$(process_argv "$PID")" | tr ' ' '\n' \
            | grep -A1 -x -- '--stream-root' | tail -1)"
        case "$STREAM_ROOT" in
            "") STREAM_ROOT="$(process_cwd "$PID")/.streams" ;;
            /*) ;;
            *) STREAM_ROOT="$(process_cwd "$PID")/$STREAM_ROOT" ;;
        esac
    fi
    printf 'stream_root=%s\n' "$STREAM_ROOT" >"$STAGE/state/stream-root.txt"
    if [ "$MODE" = "container" ]; then
        $DOCKER exec "$CONTAINER_ID" sh -c \
            'find /data/streams -maxdepth 2 -printf "%T@ %s %p\n" 2>/dev/null | sort -n | tail -200' \
            >"$STAGE/state/stream-root-listing.txt" 2>&1 || true
    else
        find "$STREAM_ROOT" -maxdepth 2 -printf '%T@ %s %p\n' 2>/dev/null \
            | sort -n | tail -200 >"$STAGE/state/stream-root-listing.txt" 2>&1 || true
    fi
fi
{
    printf 'timestamp_utc=%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    if [ "$MODE" = "container" ]; then
        printf '\n# docker stats (one shot)\n'
        $DOCKER stats --no-stream --format \
            'container={{.Name}} cpu={{.CPUPerc}} mem={{.MemUsage}} mem_perc={{.MemPerc}} net={{.NetIO}} block={{.BlockIO}} pids={{.PIDs}}' \
            "$CONTAINER_ID" 2>&1
        printf '\n# docker top\n'
        $DOCKER top "$CONTAINER_ID" 2>&1 | head -40
    else
        printf '\n# astraeus-server (/proc)\n'
        cat "$STAGE/process/state.txt" 2>/dev/null
        printf '\n# related processes\n'
        ps -eo pid,ppid,etime,pcpu,pmem,args 2>/dev/null \
            | grep -E 'astraeus-server|[f]fmpeg' || true
        printf '\n# io\n'
        cat "/proc/$PID/io" 2>/dev/null || true
    fi
} >"$STAGE/state/processes.txt"

# --- 7. sampling -------------------------------------------------------------
say "[5/7] sampling (${SAMPLE_SECONDS}s)"
mkdir -p "$STAGE/samples"
STATS_HEADER='timestamp_utc\tcpu_percent\tmem_usage\tmem_percent\tpids'
printf "$STATS_HEADER\n" >"$STAGE/samples/docker-stats.tsv"
printf 'timestamp_utc\tutime_ticks\tstime_ticks\tthreads\tvsize_bytes\trss_pages\n' \
    >"$STAGE/samples/process-stats.tsv"

# One sample of whichever shape this deployment is.
sample_once() {
    local ts child
    ts="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    if [ "$MODE" = "container" ]; then
        $DOCKER stats --no-stream --format \
            "{{.CPUPerc}}\t{{.MemUsage}}\t{{.MemPerc}}\t{{.PIDs}}" "$CONTAINER_ID" 2>/dev/null \
            | sed "s/^/$ts\t/" >>"$STAGE/samples/docker-stats.tsv"
    else
        printf '%s\t%s\n' "$ts" "$(process_sample "$PID")" >>"$STAGE/samples/process-stats.tsv"
        # The transcoder is a separate process and is where the CPU goes, so
        # sample the children too rather than reporting the server as idle.
        for child in $(pgrep -P "$PID" 2>/dev/null); do
            printf '%s\tchild=%s\t%s\n' "$ts" "$child" "$(process_sample "$child")" \
                >>"$STAGE/samples/process-children.tsv"
        done
    fi
}

if [ "$CAPTURE_PID" -gt 0 ] 2>/dev/null; then
    note "capture mode: PID $CAPTURE_PID; finish the stream interaction to stop"
    DEADLINE=$(( $(date +%s) + 600 ))
    while kill -0 "$CAPTURE_PID" 2>/dev/null && [ "$(date +%s)" -lt "$DEADLINE" ]; do
        sample_once
        sleep 2
    done
else
    END=$(( $(date +%s) + SAMPLE_SECONDS ))
    while [ "$(date +%s)" -lt "$END" ]; do
        sample_once
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
run host/dri-nodes.txt sh -c "/usr/bin/ls -l /dev/dri 2>&1; echo '---'; for d in /dev/dri/renderD*; do [ -e \"\$d\" ] || continue; echo \"== \$d\"; (command -v vainfo >/dev/null && vainfo --display drm --device \"\$d\" 2>&1 | head -40) || echo 'vainfo not installed'; done"
run host/nvidia-smi.txt sh -c "command -v nvidia-smi >/dev/null && nvidia-smi 2>&1 || echo 'nvidia-smi not installed (no NVIDIA driver userspace on this host)'"
run host/nvidia-container-toolkit.txt sh -c "command -v nvidia-ctk >/dev/null && nvidia-ctk --version 2>&1 || echo 'nvidia-ctk not installed'; echo '---'; cat /etc/nvidia-container-runtime/config.toml 2>/dev/null || echo 'no /etc/nvidia-container-runtime/config.toml'"
if have $DOCKER && $DOCKER info >/dev/null 2>&1; then
    run host/docker-info.txt sh -c "$DOCKER info 2>&1 | grep -iE 'runtime|nvidia|driver|server version|storage driver|cgroup|kernel|operating system|architecture' || true"
    run host/docker-runtimes.txt sh -c "$DOCKER info --format '{{json .Runtimes}}' 2>&1 || true"
else
    run host/docker-info.txt sh -c "echo 'docker is not usable on this host; this is a binary deployment'"
fi

# A GPU that is visible on the host but absent in the container is the single
# most likely explanation for "the GPU is not being used", so put both answers
# side by side where a reader cannot miss the disagreement.
{
    printf 'deployment: %s\n' "$MODE"
    printf 'host render nodes: %s\n' "$(/usr/bin/ls /dev/dri 2>/dev/null | tr '\n' ' ')"
    printf 'host nvidia devices: %s\n' "$(/usr/bin/ls /dev/nvidia* 2>/dev/null | tr '\n' ' ')"
    printf 'host nvidia-smi: %s\n' "$(command -v nvidia-smi >/dev/null && nvidia-smi --query-gpu=name,driver_version --format=csv,noheader 2>/dev/null | tr '\n' '; ' || printf 'absent')"
    if [ "$MODE" = "container" ]; then
        printf 'host docker runtimes: %s\n' "$($DOCKER info --format '{{json .Runtimes}}' 2>/dev/null)"
        printf 'container devices (HostConfig.Devices): %s\n' "$(container_field "$CONTAINER_ID" '{{json .HostConfig.Devices}}')"
        printf 'container device requests (HostConfig.DeviceRequests): %s\n' "$(container_field "$CONTAINER_ID" '{{json .HostConfig.DeviceRequests}}')"
        printf 'container runtime: %s\n' "$(container_field "$CONTAINER_ID" '{{.HostConfig.Runtime}}')"
    else
        # No container sits between the process and the devices, so there is no
        # visibility question to answer - and saying so is the answer.
        printf 'container devices (HostConfig.Devices): n/a (binary deployment, no container)\n'
        printf 'container device requests (HostConfig.DeviceRequests): n/a\n'
        printf 'container runtime: n/a\n'
    fi
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
if have python3; then
    say "  python3 scripts/analyze-astraeus-bundle.py $(basename "$BUNDLE") > report.md"
else
    say "  (python3 is not on this host; README.sh inside the bundle summarises it)"
fi
exit 0
