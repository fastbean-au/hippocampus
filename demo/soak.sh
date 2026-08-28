#!/usr/bin/env bash
#
# The soak harness (TODO item 20). Runs the service under the demo generator for a bounded number
# of hours, samples it on a timer, and writes a report that reaches a verdict rather than leaving
# one to be inferred.
#
#   ./demo/soak.sh --hours 4                       # SQLite + OpenSearch, the default profile
#   ./demo/soak.sh --hours 4 --profile sqlite      # no search backend
#   ./demo/soak.sh --hours 4 --profile postgres    # needs SOAK_POSTGRES_DSN
#   ./demo/soak.sh --hours 4 --profile mysql       # needs SOAK_MYSQL_DSN
#
# Everything after -- is passed through to the generator, e.g.
#
#   ./demo/soak.sh --hours 4 -- --bursty_workers 6
#
# WHY THIS EXISTS, since a soak that only proves "it did not crash" is not worth four hours: the
# single soak run on record (item 20.4, 2026-07-15) predates links, the forgotten log, the retention
# scan, a reconcile sweep that now re-embeds, the topology heartbeat, and item 84's delete outbox
# and reverse sweep. The sleep cycle has gone from one scan to roughly six. None of that is
# something a unit test can notice, and the last run's entire finding was "a clean log" - which is
# also what a goroutine leak, a degrading sleep cycle, and an eviction loop that oscillates instead
# of converging all produce.
#
# WHY SQLITE + OPENSEARCH IS THE DEFAULT PROFILE: item 84 - the index leaking stale documents under
# sustained write load - was found on a live host, at 20.7x more documents than rows, and the fix
# for it (a transactional delete outbox plus a bidirectional sweep) has never run under the load
# that produced the defect. This profile is the only one that exercises it, and demo/config.json
# has OpenSearch off, so before this script there was no soak path that touched it at all.
#
# The run drives demo/run.sh rather than starting anything itself, so there is one implementation
# of the launch-and-shutdown sequence. This script contributes the bounded duration, the sampling,
# the disk guard, and the report.

set -euo pipefail

cd "$(dirname "$0")/.."

PROFILE="sqlite-opensearch"
HOURS="4"
INTERVAL_MINUTES="5"
OUT_ROOT="${OUT_ROOT:-demo/soak-runs}"
KEEP_INDEX=""

# Trends are computed over the samples after this. Lowering it is for exercising the harness on a
# short run; a real soak leaves it at the report's own default.
WARMUP_MINUTES="${WARMUP_MINUTES:-15}"
GENERATOR_ARGS=()
GENERATOR_ARGS_TEXT=""

# Ports are soak-specific rather than the demo's, so a soak and a demo can run side by side - which
# matters because a four-hour run should not require giving up the machine.
export PORT="${PORT:-8400}"
export GATEWAY_PORT="${GATEWAY_PORT:-8480}"
export GRAFANA_PORT="${GRAFANA_PORT:-3030}"
export OTLP_PORT="${OTLP_PORT:-4347}"
export PROMETHEUS_PORT="${PROMETHEUS_PORT:-9099}"
export OTEL_CONTAINER="${OTEL_CONTAINER:-hippocampus-soak-otel-lgtm}"
export OS_CONTAINER="${OS_CONTAINER:-hippocampus-soak-opensearch}"

OPENSEARCH_URL="${OPENSEARCH_URL:-http://127.0.0.1:9200}"
OPENSEARCH_INDEX="${OPENSEARCH_INDEX:-hippocampus-soak}"

# The run aborts rather than filling the host. The index this profile exercises is precisely the
# thing that grew 700 MB/day on the deployment that motivated item 84, so running out of disk is a
# foreseeable outcome of this test rather than an unrelated mishap.
DISK_FLOOR_BYTES="${DISK_FLOOR_BYTES:-$((3 * 1024 * 1024 * 1024))}"

usage() {
    sed -n '2,40p' "$0" | sed 's/^#\{1,2\} \{0,1\}//'
}

while [[ $# -gt 0 ]]; do
    case "$1" in
        --profile)
            PROFILE="$2"
            shift 2
            ;;

        --hours)
            HOURS="$2"
            shift 2
            ;;

        --interval-minutes)
            INTERVAL_MINUTES="$2"
            shift 2
            ;;

        --out)
            OUT_ROOT="$2"
            shift 2
            ;;

        --keep-index)
            KEEP_INDEX=1
            shift
            ;;

        --warmup-minutes)
            WARMUP_MINUTES="$2"
            shift 2
            ;;

        -h | --help)
            usage

            exit 0
            ;;

        --)
            shift
            GENERATOR_ARGS=("$@")
            GENERATOR_ARGS_TEXT="$*"

            break
            ;;

        *)
            echo "unknown argument: $1" >&2
            usage >&2

            exit 2
            ;;
    esac
done

DURATION_SECONDS=$(awk -v h="${HOURS}" 'BEGIN { printf "%d", h * 3600 }')
INTERVAL_SECONDS=$(awk -v m="${INTERVAL_MINUTES}" 'BEGIN { printf "%d", m * 60 }')

if [[ ${DURATION_SECONDS} -lt 60 || ${INTERVAL_SECONDS} -lt 10 ]]; then
    echo "a run must be at least a minute long and sample no faster than every 10 seconds" >&2

    exit 2
fi

DRIVER=""
USE_OPENSEARCH=""

case "${PROFILE}" in
    sqlite-opensearch)
        DRIVER="sqlite"
        USE_OPENSEARCH=1
        ;;

    sqlite)
        DRIVER="sqlite"
        ;;

    postgres)
        DRIVER="postgres"
        ;;

    mysql)
        DRIVER="mysql"
        ;;

    *)
        echo "unknown profile: ${PROFILE} (sqlite-opensearch, sqlite, postgres, mysql)" >&2

        exit 2
        ;;
esac

# OPENSEARCH=1 adds the search backend to a server-driver profile. Kept as an env override rather
# than six profile names, since the driver and the search backend are independent choices.
if [[ ${OPENSEARCH:-} == "1" ]]; then
    USE_OPENSEARCH=1
fi

DSN=""
if [[ ${DRIVER} == "postgres" ]]; then
    DSN="${SOAK_POSTGRES_DSN:-}"

    if [[ -z ${DSN} ]]; then
        echo "the postgres profile needs SOAK_POSTGRES_DSN, e.g." >&2
        echo "  SOAK_POSTGRES_DSN='postgres://postgres:postgres@127.0.0.1:55432/soak?sslmode=disable'" >&2

        exit 2
    fi
fi
if [[ ${DRIVER} == "mysql" ]]; then
    DSN="${SOAK_MYSQL_DSN:-}"

    if [[ -z ${DSN} ]]; then
        echo "the mysql profile needs SOAK_MYSQL_DSN, e.g." >&2
        echo "  SOAK_MYSQL_DSN='root:root@tcp(127.0.0.1:33306)/soak?parseTime=true'" >&2

        exit 2
    fi
fi

RUN_ID="$(date +%Y%m%d-%H%M%S)-${PROFILE}"
OUT_DIR="${OUT_ROOT}/${RUN_ID}"

# The generated config needs an ABSOLUTE storage.directory, because the service resolves it against
# its own working directory rather than this script's. Prefixing PWD unconditionally was wrong, and
# not harmlessly so: an absolute --out produced "/Users/.../hippocampus//tmp/...", which quietly
# created the soak's entire store INSIDE THE REPOSITORY, under a directory tree mirroring the path
# that was asked for. Found by a `git status` showing 86 MB of databases staged for commit.
case "${OUT_DIR}" in
    /*)
        DATA_DIR_ABS="${OUT_DIR}/data"
        ;;

    *)
        DATA_DIR_ABS="${PWD}/${OUT_DIR}/data"
        ;;
esac
CONFIG_FILE="${OUT_DIR}/config.json"
SAMPLES_CSV="${OUT_DIR}/samples.csv"
RUN_LOG="${OUT_DIR}/run.log"
META_JSON="${OUT_DIR}/meta.json"
REPORT_MD="${OUT_DIR}/report.md"

mkdir -p "${OUT_DIR}" "${DATA_DIR_ABS}"

# The config is GENERATED from demo/config.json rather than being a fourth checked-in copy of it,
# and the generated file is archived with the run. Both halves matter: the soak is then provably
# running the demo's own tuning (the compressed decay clock, the byte cap, the sleep period) and
# differs only where the profile says, and the report can point at the exact file that produced it
# instead of at a config that may have been edited since.
python3 - "${CONFIG_FILE}" "${DRIVER}" "${DSN}" "${USE_OPENSEARCH:-}" "${OPENSEARCH_INDEX}" "${PORT}" "${GATEWAY_PORT}" "${DATA_DIR_ABS}" <<'PYEOF'
import json
import sys

out, driver, dsn, use_opensearch, index, port, gateway_port, data_dir = sys.argv[1:9]

with open("demo/config.json", encoding="utf-8") as handle:
    config = json.load(handle)

config["port"] = int(port)
config.setdefault("gateway", {})["port"] = int(gateway_port)

storage = config.setdefault("storage", {})
storage["driver"] = driver
storage["directory"] = data_dir

if driver == "postgres":
    storage.setdefault("postgres", {})["dsn"] = dsn
if driver == "mysql":
    storage.setdefault("mysql", {})["dsn"] = dsn

search = config.setdefault("opensearch", {})
search["enabled"] = bool(use_opensearch)
search["index"] = index

# Metrics are the harness's only view of the subject, so they are turned on here rather than left
# to run.sh's env overrides - a soak whose sampling silently depended on an env var that a future
# edit stopped exporting would produce an empty report and a passing exit code.
observability = config.setdefault("observability", {})
observability.setdefault("metrics", {})["enabled"] = True
observability["metrics"]["exportIntervalSeconds"] = 30
observability.setdefault("tracing", {})["enabled"] = False

# The memory-count gauge is served from the stats cache, refreshed at most once per this interval
# (item 25.9 - counting is a full scan on the server drivers, so it is deliberately not done per
# export). The default of five minutes is right for a deployment and too coarse here: the index
# divergence check compares that count against a LIVE OpenSearch count, so a stale figure reads as
# a leak. Sixty seconds is the harness paying for its own measurement, which a test may do and a
# deployment should not.
config.setdefault("stats", {})["intervalSeconds"] = 60

with open(out, "w", encoding="utf-8") as handle:
    json.dump(config, handle, indent=2)
    handle.write("\n")
PYEOF

export CONFIG_FILE
export DATA_DIR="${OUT_DIR}/data"

if [[ -n ${USE_OPENSEARCH} ]]; then
    export SEARCH=1
else
    export SEARCH=0
fi

export OBSERVABILITY=1

# The generator's own pause cap. Set above the store's byte capacity so that eviction, not the
# generator, is what bounds the store - otherwise the run would measure the generator's watcher
# rather than the decay path.
export MAX_BYTES="${MAX_BYTES:-$((4 * 1024 * 1024 * 1024))}"

RUN_PID=""
SAMPLER_STARTED=""
EXIT_REASON="completed"
STARTED_AT="$(date +%s)"

# stop_run ends the service and the generator through demo/run.sh's own cleanup, which shuts the
# two down in the correct order (generator first, so nothing is in flight while the service drains
# and checkpoints). SIGTERM rather than SIGINT deliberately: a backgrounded script inherits SIGINT
# ignored, so an INT here would not reach the trap at all.
stop_run() {
    if [[ -z ${RUN_PID} ]]; then
        return
    fi

    if ! kill -0 "${RUN_PID}" 2> /dev/null; then
        RUN_PID=""

        return
    fi

    echo "stopping the run (pid ${RUN_PID})"
    kill -TERM "${RUN_PID}" 2> /dev/null || true

    for _ in $(seq 1 120); do
        if ! kill -0 "${RUN_PID}" 2> /dev/null; then
            break
        fi

        sleep 1
    done

    kill -KILL "${RUN_PID}" 2> /dev/null || true
    wait "${RUN_PID}" 2> /dev/null || true
    RUN_PID=""
}

# finish always writes a report, whatever ended the run. An interrupted soak still has hours of
# samples in it and they answer the same questions, only over a shorter window - discarding them
# because the operator pressed Ctrl-C would be the harness throwing away its own result.
finish() {
    trap - INT TERM EXIT

    # Sampled BEFORE the shutdown: the end-of-run reading is the one both leak checks compare
    # against, and taken after the service exits it would describe a process that is no longer
    # there. RSS in particular simply disappears.
    if [[ -n ${SAMPLER_STARTED} ]]; then
        record_sample
    fi

    stop_run
    write_meta

    if [[ -n ${USE_OPENSEARCH} && -z ${KEEP_INDEX} ]]; then
        echo "removing the soak index ${OPENSEARCH_INDEX}"
        curl -sf -X DELETE "${OPENSEARCH_URL}/${OPENSEARCH_INDEX}" > /dev/null 2>&1 || true
    fi

    echo ""
    python3 demo/soak/report.py --csv "${SAMPLES_CSV}" --log "${RUN_LOG}" --meta "${META_JSON}" \
        --warmup-minutes "${WARMUP_MINUTES}" --out "${REPORT_MD}" || true

    echo ""
    echo "report:  ${REPORT_MD}"
    echo "samples: ${SAMPLES_CSV}"
    echo "log:     ${RUN_LOG}"
}

interrupted() {
    EXIT_REASON="interrupted"

    finish

    exit 130
}

write_meta() {
    python3 - "${META_JSON}" "${PROFILE}" "${DRIVER}" "${USE_OPENSEARCH:-}" "${CONFIG_FILE}" "${STARTED_AT}" \
        "${INTERVAL_MINUTES}" "${EXIT_REASON}" "${GENERATOR_ARGS_TEXT}" <<'PYEOF'
import json
import subprocess
import sys
import time

(
    out,
    profile,
    driver,
    use_opensearch,
    config,
    started,
    interval,
    exit_reason,
    generator_args,
) = sys.argv[1:10]

started = int(started)
finished = int(time.time())
elapsed = finished - started

try:
    version = subprocess.run(
        ["git", "describe", "--tags", "--always", "--dirty"],
        capture_output=True,
        text=True,
        timeout=10,
        check=False,
    ).stdout.strip()
except (OSError, subprocess.SubprocessError):
    version = ""

with open(out, "w", encoding="utf-8") as handle:
    json.dump(
        {
            "profile": profile,
            "driver": driver,
            "search": "opensearch" if use_opensearch else "none (SQL fallback or no-op)",
            "config": config,
            "version": version,
            "started": time.strftime("%Y-%m-%d %H:%M:%S", time.localtime(started)),
            "finished": time.strftime("%Y-%m-%d %H:%M:%S", time.localtime(finished)),
            "duration": "%dh%02dm" % (elapsed // 3600, (elapsed % 3600) // 60),
            "interval": "%s minutes" % interval,
            "exit_reason": exit_reason,
            "generator_args": generator_args or "(none)",
        },
        handle,
        indent=2,
    )
    handle.write("\n")
PYEOF
}

# service_pid finds the service run.sh started, matched on the generated config path so it can
# never pick up a demo instance running beside this one.
service_pid() {
    pgrep -f "demo/bin/hippocampus -c ${CONFIG_FILE}" 2> /dev/null | head -1 || true
}

record_sample() {
    local pid
    pid="$(service_pid)"

    local opensearch_arg=""
    if [[ -n ${USE_OPENSEARCH} ]]; then
        opensearch_arg="${OPENSEARCH_URL}"
    fi

    # Never allowed to end the run: a sample that fails is a gap in the CSV, and the report reads a
    # gap as unknown. Losing four hours because Prometheus was restarting would be absurd.
    python3 demo/soak/sample.py \
        --prometheus "http://127.0.0.1:${PROMETHEUS_PORT}" \
        --opensearch "${opensearch_arg}" \
        --index "${OPENSEARCH_INDEX}" \
        --pid "${pid}" \
        --disk-path "${OUT_DIR}" \
        --started "${STARTED_AT}" >> "${SAMPLES_CSV}" 2> /dev/null || true
}

trap interrupted INT TERM
trap finish EXIT

echo "soak run ${RUN_ID}"
SEARCH_LABEL="none"
if [[ -n ${USE_OPENSEARCH} ]]; then
    SEARCH_LABEL="opensearch"
fi

echo "  profile:   ${PROFILE} (driver ${DRIVER}, search ${SEARCH_LABEL})"
echo "  duration:  ${HOURS}h, sampling every ${INTERVAL_MINUTES}m"
echo "  config:    ${CONFIG_FILE}"
echo "  output:    ${OUT_DIR}"
echo ""

if [[ -n ${USE_OPENSEARCH} && -z ${KEEP_INDEX} ]]; then
    # A run must start from a known index state or the divergence check is measuring the last run
    # as well as this one. Deleting only this soak's own index leaves any standing test cluster's
    # other indices untouched.
    curl -sf -X DELETE "${OPENSEARCH_URL}/${OPENSEARCH_INDEX}" > /dev/null 2>&1 || true
fi

python3 demo/soak/sample.py --header > "${SAMPLES_CSV}"

echo "starting the run (output tees to ${RUN_LOG})"
if [[ ${#GENERATOR_ARGS[@]} -gt 0 ]]; then
    bash demo/run.sh "${GENERATOR_ARGS[@]}" > "${RUN_LOG}" 2>&1 &
else
    bash demo/run.sh > "${RUN_LOG}" 2>&1 &
fi
RUN_PID=$!

echo "waiting for the gateway on port ${GATEWAY_PORT}"
READY=""
for _ in $(seq 1 300); do
    if curl -sf "http://127.0.0.1:${GATEWAY_PORT}/healthz" > /dev/null 2>&1; then
        READY=1

        break
    fi

    if ! kill -0 "${RUN_PID}" 2> /dev/null; then
        echo "the run exited before becoming ready - see ${RUN_LOG}" >&2
        tail -30 "${RUN_LOG}" >&2
        EXIT_REASON="failed to start"

        exit 1
    fi

    sleep 1
done

if [[ -z ${READY} ]]; then
    echo "the gateway never became ready - see ${RUN_LOG}" >&2
    EXIT_REASON="never became ready"

    exit 1
fi

# The clock starts at readiness, not at launch, so that the warm-up the report discards is the
# service's own and not the container startup ahead of it.
STARTED_AT="$(date +%s)"
DEADLINE=$((STARTED_AT + DURATION_SECONDS))

# BSD date wants -r for an epoch, GNU date wants -d @; try the one and fall back to the other.
DEADLINE_TEXT="$(date -r "${DEADLINE}" '+%H:%M:%S' 2> /dev/null || date -d "@${DEADLINE}" '+%H:%M:%S' 2> /dev/null || true)"

echo "running until ${DEADLINE_TEXT:-the deadline}"
echo "  console: http://localhost:${GATEWAY_PORT}/ui"
echo "  grafana: http://localhost:${GRAFANA_PORT}"
echo ""

SAMPLER_STARTED=1
record_sample

NOW="${STARTED_AT}"

while [[ ${NOW} -lt ${DEADLINE} ]]; do
    sleep "${INTERVAL_SECONDS}"

    if ! kill -0 "${RUN_PID}" 2> /dev/null; then
        echo "the run exited early - see ${RUN_LOG}" >&2
        EXIT_REASON="the service or generator exited early"

        break
    fi

    record_sample

    FREE="$(df -k "${OUT_DIR}" 2> /dev/null | awk 'NR == 2 { print $4 * 1024 }' || true)"
    if [[ -n ${FREE} && ${FREE} -lt ${DISK_FLOOR_BYTES} ]]; then
        FREE_TEXT="$(awk -v b="${FREE}" 'BEGIN { printf "%.1f GiB", b / 1073741824 }' || true)"

        echo "free disk (${FREE_TEXT}) fell below the floor - stopping" >&2
        EXIT_REASON="stopped at the disk floor"

        break
    fi

    NOW="$(date +%s)"
    ELAPSED=$((NOW - STARTED_AT))

    printf '  %dh%02dm elapsed\n' $((ELAPSED / 3600)) $(((ELAPSED % 3600) / 60))
done
