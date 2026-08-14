#!/usr/bin/env bash
#
# Builds and launches the hippocampus service together with the Bluesky firehose bridge, so the
# decay model can be watched running on real data arriving in real time.
#
# What it demonstrates: every post arrives with the SAME significance, and the engagement that
# follows it - a like, a repost, a reply - reinforces that post's memory. So what survives is only
# what people came back to. Unengaged posts turn over every few minutes, a post with a few likes
# lasts noticeably longer, and a post that keeps drawing attention never leaves at all.
#
# The service uses demo/config.bluesky.json (port 8300, database under demo/data-bluesky, a
# one-minute sleep cycle, and a decay clock compressed so one age unit is about three minutes rather
# than a day). The web console is at http://localhost:8080/ui - the Decay tab is the one to watch.
#
# PRIVACY. This connects to the LIVE PUBLIC Bluesky firehose and writes real people's public posts,
# keyed by their DID, into demo/data-bluesky on this machine. Nothing is published anywhere. Post
# deletions are honoured by default, so a post withdrawn upstream is deleted here too. To stop:
# Ctrl-C. To remove the data: rm -rf demo/data-bluesky.
#
# Environment overrides:
#   LANGS=en                 keep only posts DECLARING these languages (default: all). The
#                            declaration comes from the posting client, usually from the account's
#                            interface language rather than from the text, so a French post
#                            declaring "en" is kept and stored with lang=en. Read it as a volume
#                            control, not a guarantee - see docs/eventsource.md
#   DIDS=did:plc:...         follow only these accounts instead of the whole network
#   FEED=at://...            take posts from a curated feed generator instead of the firehose
#                            (engagement still comes from the firehose). Much lower volume and
#                            every memory is a readable headline, so the decay clock in
#                            config.bluesky.json wants slowing down to match - see docs/eventsource.md
#   TOPIC_LINKS=0            do not relate posts that share topic terms (default: on)
#   EVENTS=thread|none       thread modelling (default: thread)
#   SIGNIFICANCE=10          the significance every post arrives with
#   CAPACITY_BYTES=1200000   the byte cap eviction works to (see "tuning" below)
#   CAPACITY_MEMORIES=800    the row count capacity pressure is measured against
#   JETSTREAM_URL=wss://...  a different (or self-hosted) Jetstream endpoint
#   SEARCH=0                 skip the OpenSearch container
#   OBSERVABILITY=0          skip the grafana/otel-lgtm collector
#
# TUNING. The capacity defaults are MEASURED, not calculated: a run on the open firehose settles at
# roughly 1,270 memories using about 1.9 MB (~1,500 bytes each, since UsedBytes counts live SQLite
# pages including indexes, not payload). The caps are set just under that so both eviction and
# capacity pressure actually engage - set them far above the equilibrium and neither ever fires, and
# the Decay tab shows a flat pressure of x1.00 all session.
#
# The equilibrium moves with the arrival rate, so narrowing the stream (LANGS, DIDS) or changing
# SIGNIFICANCE moves it too: a post's lifetime is about 34.6 x significance / pressure seconds, and
# the store settles at arrival-rate x lifetime. The script prints the observed ratio five minutes in;
# if eviction never fires, lower CAPACITY_BYTES, and if it fires every cycle, raise it.

set -euo pipefail

cd "$(dirname "$0")/.."

DEMO_DIR="demo"
BIN_DIR="${DEMO_DIR}/bin"
DATA_DIR="${DEMO_DIR}/data-bluesky"
PORT=8300
GATEWAY_PORT=8080
HEALTH_PORT=8090

LANGS="${LANGS:-}"
DIDS="${DIDS:-}"
FEED="${FEED:-}"
TOPIC_LINKS="${TOPIC_LINKS:-1}"
EVENTS="${EVENTS:-thread}"
SIGNIFICANCE="${SIGNIFICANCE:-10}"
CAPACITY_BYTES="${CAPACITY_BYTES:-1200000}"
CAPACITY_MEMORIES="${CAPACITY_MEMORIES:-800}"
JETSTREAM_URL="${JETSTREAM_URL:-}"

SEARCH="${SEARCH:-1}"
OBSERVABILITY="${OBSERVABILITY:-1}"
OTEL_CONTAINER="hippocampus-bluesky-otel-lgtm"
OS_CONTAINER="hippocampus-bluesky-opensearch"
OTEL_STARTED=""
OS_STARTED=""
CONTAINER_RUNTIME=""

search_on() {
    [[ -n ${SEARCH} && ${SEARCH} != "false" && ${SEARCH} != "0" ]]
}

observability_on() {
    [[ -n ${OBSERVABILITY} && ${OBSERVABILITY} != "false" && ${OBSERVABILITY} != "0" ]]
}

# detect_container_runtime picks docker or podman into CONTAINER_RUNTIME, leaving it empty when
# neither is present. Both the OpenSearch and the observability paths need a runtime, so it is
# resolved once up front.
detect_container_runtime() {
    if command -v docker > /dev/null 2>&1; then
        CONTAINER_RUNTIME="docker"
    elif command -v podman > /dev/null 2>&1; then
        CONTAINER_RUNTIME="podman"
    fi
}

mkdir -p "${BIN_DIR}" "${DATA_DIR}"

echo "building hippocampus and the bluesky bridge"
go build -o "${BIN_DIR}/hippocampus" ./cmd/hippocampus

# The bridge lives in the eventsource module, which is a separate Go module with its own dependency
# tree - hence the subshell rather than a second ./... build from the root.
(
    cd integrations/eventsource
    go build -o "../../${BIN_DIR}/hippocampus-bluesky-bridge" ./cmd/bluesky
)

SERVICE_PID=""
BRIDGE_PID=""

cleanup() {
    trap - INT TERM EXIT
    echo ""
    echo "shutting down"

    # Stop the bridge first and wait for it to exit, so no new RPCs are in flight when the service
    # drains (and so the bridge gets to flush its buffered recalls). Then signal the service and let
    # it shut down gracefully on its own clock.
    if [[ -n ${BRIDGE_PID} ]]; then
        kill "${BRIDGE_PID}" 2> /dev/null || true
        wait "${BRIDGE_PID}" 2> /dev/null || true
    fi
    if [[ -n ${SERVICE_PID} ]]; then
        kill "${SERVICE_PID}" 2> /dev/null || true
        wait "${SERVICE_PID}" 2> /dev/null || true
    fi

    if [[ -n ${OS_STARTED} ]]; then
        echo "stopping the opensearch container"
        "${CONTAINER_RUNTIME}" stop "${OS_CONTAINER}" 2> /dev/null || true
    fi
    if [[ -n ${OTEL_STARTED} ]]; then
        echo "stopping the otel-lgtm collector"
        "${CONTAINER_RUNTIME}" stop "${OTEL_CONTAINER}" 2> /dev/null || true
    fi
}
trap cleanup INT TERM EXIT

if search_on || observability_on; then
    detect_container_runtime
fi

if search_on; then
    if curl -sf "http://127.0.0.1:9200/_cluster/health" > /dev/null 2>&1; then
        # Something is already serving OpenSearch on :9200 (e.g. a standing test cluster). Reuse it
        # rather than starting a colliding container - and leave OS_STARTED unset so cleanup does
        # not stop a container this script did not start.
        echo "reusing the OpenSearch already listening on http://localhost:9200"
        export HIPPOCAMPUS_OPENSEARCH_ENABLED=true
    elif [[ -n ${CONTAINER_RUNTIME} ]]; then
        echo "starting the opensearch container (${CONTAINER_RUNTIME})"
        "${CONTAINER_RUNTIME}" rm -f "${OS_CONTAINER}" > /dev/null 2>&1 || true
        "${CONTAINER_RUNTIME}" run -d --rm --name "${OS_CONTAINER}" \
            -p 9200:9200 \
            -e "discovery.type=single-node" \
            -e "DISABLE_SECURITY_PLUGIN=true" \
            -e "OPENSEARCH_JAVA_OPTS=-Xms512m -Xmx512m" \
            opensearchproject/opensearch:3.1.0 > /dev/null
        OS_STARTED=1

        echo "waiting for opensearch to report healthy on port 9200"
        OS_READY=""
        for _ in $(seq 1 120); do
            if curl -sf "http://127.0.0.1:9200/_cluster/health" > /dev/null 2>&1; then
                OS_READY=1

                break
            fi

            sleep 1
        done

        if [[ -z ${OS_READY} ]]; then
            echo "warning: opensearch did not report healthy in time; content search may lag" >&2
        fi

        export HIPPOCAMPUS_OPENSEARCH_ENABLED=true
    else
        echo "note: neither docker nor podman is available and nothing is serving :9200 - running" >&2
        echo "      without OpenSearch, so the console's content-search tab will fall back to the" >&2
        echo "      store-backed index (set SEARCH=0 to silence this)" >&2
    fi
fi

OBSERVABILITY_RUNTIME_AVAILABLE=""
if observability_on; then
    if [[ -n ${CONTAINER_RUNTIME} ]]; then
        OBSERVABILITY_RUNTIME_AVAILABLE=1
    else
        echo "note: neither docker nor podman is available - running without observability, so" >&2
        echo "      metrics/traces will not be exported (set OBSERVABILITY=0 to silence this)" >&2
    fi
fi

if [[ -n ${OBSERVABILITY_RUNTIME_AVAILABLE} ]]; then
    echo "starting the otel-lgtm collector (${CONTAINER_RUNTIME})"
    "${CONTAINER_RUNTIME}" rm -f "${OTEL_CONTAINER}" > /dev/null 2>&1 || true
    DASHBOARD_DIR="${PWD}/deploy/compose/observability"
    PROVISION_DIR="/otel-lgtm/grafana/conf/provisioning/dashboards/custom"
    "${CONTAINER_RUNTIME}" run -d --rm --name "${OTEL_CONTAINER}" \
        -p 3000:3000 -p 4317:4317 \
        -e "GF_DASHBOARDS_DEFAULT_HOME_DASHBOARD_PATH=${PROVISION_DIR}/hippocampus.json" \
        -v "${DASHBOARD_DIR}/hippocampus-dashboard.json:${PROVISION_DIR}/hippocampus.json:ro" \
        -v "${DASHBOARD_DIR}/dashboards-provider.yaml:/otel-lgtm/grafana/conf/provisioning/dashboards/custom.yaml:ro" \
        -v "${DASHBOARD_DIR}/alerting-rules.yaml:/otel-lgtm/grafana/conf/provisioning/alerting/hippocampus.yaml:ro" \
        grafana/otel-lgtm:latest > /dev/null
    OTEL_STARTED=1

    echo "waiting for the collector's OTLP endpoint on port 4317"
    for _ in $(seq 1 100); do
        if (echo > "/dev/tcp/127.0.0.1/4317") 2> /dev/null; then
            break
        fi

        sleep 0.5
    done

    export HIPPOCAMPUS_OBSERVABILITY_METRICS_ENABLED=true
    export HIPPOCAMPUS_OBSERVABILITY_TRACING_ENABLED=true
    export HIPPOCAMPUS_OBSERVABILITY_OTLP_ENDPOINT="localhost:4317"
    export HIPPOCAMPUS_OBSERVABILITY_OTLP_INSECURE=true

    echo "grafana will be available at http://localhost:3000"
fi

# Capacity is the knob that decides whether eviction is visible in a session, and the right value
# depends on the measured bytes-per-memory - so it is an override rather than a config edit.
export HIPPOCAMPUS_CONSOLIDATION_CAPACITYBYTES="${CAPACITY_BYTES}"
export HIPPOCAMPUS_CONSOLIDATION_CAPACITYBYTESFLOOR="$((CAPACITY_BYTES * 8 / 10))"
export HIPPOCAMPUS_CONSOLIDATION_CAPACITYMEMORIES="${CAPACITY_MEMORIES}"

echo "starting hippocampus"
"${BIN_DIR}/hippocampus" -c "${DEMO_DIR}/config.bluesky.json" &
SERVICE_PID=$!

echo "waiting for the service on port ${PORT}"
for _ in $(seq 1 50); do
    if (echo > "/dev/tcp/127.0.0.1/${PORT}") 2> /dev/null; then
        break
    fi

    if ! kill -0 "${SERVICE_PID}" 2> /dev/null; then
        echo "service failed to start" >&2
        exit 1
    fi

    sleep 0.2
done

cat << BANNER

  ------------------------------------------------------------------------
  This connects to the LIVE PUBLIC Bluesky firehose and stores real
  people's public posts in ${DATA_DIR} on this machine.

  Nothing is published anywhere, and upstream deletions are honoured.
  Ctrl-C stops it; 'rm -rf ${DATA_DIR}' removes the data.

  To narrow it: LANGS=en ./demo/bluesky.sh
                DIDS=did:plc:yourhandle ./demo/bluesky.sh
  ------------------------------------------------------------------------

  web console (the demo UI): http://localhost:${GATEWAY_PORT}/ui
    -> the Decay tab is the one to watch
BANNER

if [[ -n ${OTEL_STARTED} ]]; then
    echo "  grafana dashboard:         http://localhost:3000"
fi
echo ""

BRIDGE_ARGS=(
    --address "localhost:${PORT}"
    --significance "${SIGNIFICANCE}"
    --group bluesky
    --group-from-subject=false
    --metadata source=bluesky
    --metadata-header did
    --metadata-header handle
    --metadata-header lang
    --metadata-header embed
    --events "${EVENTS}"
    --recall
    --honour-deletes
    --health-port "${HEALTH_PORT}"
)

if [[ -n ${LANGS} ]]; then
    BRIDGE_ARGS+=(--langs "${LANGS}")
fi
if [[ -n ${FEED} ]]; then
    BRIDGE_ARGS+=(--feed "${FEED}")
fi
# Topic links are what make consolidation.linkRecallPropagation (set in config.bluesky.json) do
# anything: without edges there is nothing for a recall to spread along. They are far better with
# FEED set, where nearly every post carries a link card whose URL slug is an editorial keyword list;
# on the open firehose most posts have no card and the terms come from the text instead.
if [[ -n ${TOPIC_LINKS} && ${TOPIC_LINKS} != "false" && ${TOPIC_LINKS} != "0" ]]; then
    BRIDGE_ARGS+=(--topic-links)
fi
if [[ -n ${DIDS} ]]; then
    BRIDGE_ARGS+=(--dids "${DIDS}")
fi
if [[ -n ${JETSTREAM_URL} ]]; then
    BRIDGE_ARGS+=(--jetstream-url "${JETSTREAM_URL}")
fi
if [[ -n ${OTEL_STARTED} ]]; then
    BRIDGE_ARGS+=(--metrics --otlp-endpoint localhost:4317 --metrics-group bluesky)
fi

echo "starting the bluesky bridge"
"${BIN_DIR}/hippocampus-bluesky-bridge" "${BRIDGE_ARGS[@]}" "$@" &
BRIDGE_PID=$!

echo "service pid ${SERVICE_PID}, bridge pid ${BRIDGE_PID} - ctrl-c to stop"

# Report the measured store shape once, five minutes in, so the capacity knob can be tuned to what
# this machine actually observes rather than to an estimate. Backgrounded so it never delays or
# blocks the run, and entirely best-effort.
(
    sleep 300

    if ! stats=$(curl -sf -X POST "http://127.0.0.1:${GATEWAY_PORT}/v1/consolidation/explain" \
        -H 'content-type: application/json' -d '{}' 2> /dev/null); then
        exit 0
    fi

    python3 - "${stats}" "${CAPACITY_BYTES}" << 'PY' 2> /dev/null || true
import json, sys

d = json.loads(sys.argv[1])
cap = int(sys.argv[2])
used = int(d.get("usedBytes", 0))
count = int(d.get("memoryCount", 0))
pressure = float(d.get("capacityPressure", 0))

if not count:
    sys.exit(0)

print("")
print(f"  [tuning] {count} memories using {used/1e6:.1f} MB "
      f"({used/count:.0f} bytes each), capacity pressure x{pressure:.2f}")

if used < cap * 0.5:
    print(f"  [tuning] well under the {cap/1e6:.0f} MB cap - eviction may never fire; "
          f"try CAPACITY_BYTES={int(used*1.2)}")
PY
) &

wait "${BRIDGE_PID}" "${SERVICE_PID}"
