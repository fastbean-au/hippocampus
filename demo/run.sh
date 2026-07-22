#!/usr/bin/env bash
#
# Builds and launches the hippocampus service together with the load generator. The service uses
# demo/config.json (port 8300, database under demo/data, a two-minute sleep cycle, and demo-tuned
# consolidation settings); the generator drives it until interrupted, pausing whenever the
# database reaches the 1 GiB cap. Ctrl-C stops both.
#
# The service runs with its HTTP/JSON gateway on port 8080, which serves the web console (the
# front-facing part of the demo) at http://localhost:8080/ui. By default the script also launches
# an OpenSearch container (needs docker or podman) so the console's content-search tab works; set
# SEARCH=0 to skip it, or the demo runs without content search if no container runtime is found.
#
# By default the script also launches an all-in-one grafana/otel-lgtm collector (needs docker or
# podman) and ships the service's metrics/traces to it, so a soak run can be watched in Grafana at
# http://localhost:3000. Set OBSERVABILITY=0 to skip it, or the demo runs without observability if
# no container runtime is found.

set -euo pipefail

cd "$(dirname "$0")/.."

DEMO_DIR="demo"
BIN_DIR="${DEMO_DIR}/bin"
DATA_DIR="${DEMO_DIR}/data"
PORT=8300
GATEWAY_PORT=8080
MAX_BYTES="${MAX_BYTES:-$((1024 * 1024 * 1024))}"

SEARCH="${SEARCH:-1}"
OBSERVABILITY="${OBSERVABILITY:-1}"
OTEL_CONTAINER="hippocampus-demo-otel-lgtm"
OS_CONTAINER="hippocampus-demo-opensearch"
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

echo "building hippocampus and the generator"
go build -o "${BIN_DIR}/hippocampus" ./cmd/hippocampus
go build -o "${BIN_DIR}/generator" ./demo/generator

SERVICE_PID=""
GENERATOR_PID=""

cleanup() {
    trap - INT TERM EXIT
    echo ""
    echo "shutting down"

    # Stop the generator first and wait for it to exit, so no new RPCs are in flight when the
    # service drains. Then signal the service and let it shut down gracefully (drain in-flight RPCs,
    # checkpoint the database) on its own clock rather than racing the generator's traffic.
    if [[ -n ${GENERATOR_PID} ]]; then
        kill "${GENERATOR_PID}" 2> /dev/null || true
        wait "${GENERATOR_PID}" 2> /dev/null || true
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

        # The service reads this via viper's HIPPOCAMPUS_* env overrides, enabling the secondary
        # content-search index against the container we just started without editing the config.
        export HIPPOCAMPUS_OPENSEARCH_ENABLED=true
    else
        echo "note: neither docker nor podman is available and nothing is serving :9200 - running" >&2
        echo "      without OpenSearch, so the console's content-search tab will be inactive" >&2
        echo "      (set SEARCH=0 to silence this)" >&2
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
    DASHBOARD_DIR="${PWD}/docker/observability"
    PROVISION_DIR="/otel-lgtm/grafana/conf/provisioning/dashboards/custom"
    "${CONTAINER_RUNTIME}" run -d --rm --name "${OTEL_CONTAINER}" \
        -p 3000:3000 -p 4317:4317 \
        -e "GF_DASHBOARDS_DEFAULT_HOME_DASHBOARD_PATH=${PROVISION_DIR}/hippocampus.json" \
        -v "${DASHBOARD_DIR}/hippocampus-dashboard.json:${PROVISION_DIR}/hippocampus.json:ro" \
        -v "${DASHBOARD_DIR}/dashboards-provider.yaml:/otel-lgtm/grafana/conf/provisioning/dashboards/custom.yaml:ro" \
        grafana/otel-lgtm:latest > /dev/null
    OTEL_STARTED=1

    echo "waiting for the collector's OTLP endpoint on port 4317"
    for _ in $(seq 1 100); do
        if (echo > "/dev/tcp/127.0.0.1/4317") 2> /dev/null; then
            break
        fi

        sleep 0.5
    done

    # The service reads these via viper's HIPPOCAMPUS_* env overrides, enabling metrics/traces and
    # pointing them at the collector without editing demo/config.json.
    export HIPPOCAMPUS_OBSERVABILITY_METRICS_ENABLED=true
    export HIPPOCAMPUS_OBSERVABILITY_TRACING_ENABLED=true
    export HIPPOCAMPUS_OBSERVABILITY_OTLP_ENDPOINT="localhost:4317"
    export HIPPOCAMPUS_OBSERVABILITY_OTLP_INSECURE=true

    echo "grafana will be available at http://localhost:3000"
fi

echo "starting hippocampus"
"${BIN_DIR}/hippocampus" -c "${DEMO_DIR}/config.json" &
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

echo ""
echo "  web console (the demo UI): http://localhost:${GATEWAY_PORT}/ui"
if [[ ${HIPPOCAMPUS_OPENSEARCH_ENABLED:-} == "true" ]]; then
    echo "  content search is live (OpenSearch) - use the console's Search tab"
fi
if [[ -n ${OTEL_STARTED} ]]; then
    echo "  grafana dashboard:         http://localhost:3000"
fi
echo ""

echo "starting the generator"
"${BIN_DIR}/generator" --address "localhost:${PORT}" --data_dir "${DATA_DIR}" --max_bytes "${MAX_BYTES}" "$@" &
GENERATOR_PID=$!

echo "service pid ${SERVICE_PID}, generator pid ${GENERATOR_PID} - ctrl-c to stop"
wait "${GENERATOR_PID}" "${SERVICE_PID}"
