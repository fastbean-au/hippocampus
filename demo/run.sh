#!/usr/bin/env bash
#
# Builds and launches the hippocampus service together with the load generator. The service uses
# demo/config.json (port 8300, database under demo/data, a two-minute sleep cycle, and demo-tuned
# consolidation settings); the generator drives it until interrupted, pausing whenever the
# database reaches the 1 GiB cap. Ctrl-C stops both.
#
# Set OBSERVABILITY=1 to also launch an all-in-one grafana/otel-lgtm collector (needs docker or
# podman) and ship the service's metrics/traces to it, so a soak run can be watched in Grafana at
# http://localhost:3000. Left unset, metrics stay off and nothing is exported.

set -euo pipefail

cd "$(dirname "$0")/.."

DEMO_DIR="demo"
BIN_DIR="${DEMO_DIR}/bin"
DATA_DIR="${DEMO_DIR}/data"
PORT=8300
MAX_BYTES="${MAX_BYTES:-$((1024 * 1024 * 1024))}"

OBSERVABILITY="${OBSERVABILITY:-}"
OTEL_CONTAINER="hippocampus-demo-otel-lgtm"
OTEL_STARTED=""
CONTAINER_RUNTIME=""

observability_on() {
    [[ -n ${OBSERVABILITY} && ${OBSERVABILITY} != "false" && ${OBSERVABILITY} != "0" ]]
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

    if [[ -n ${GENERATOR_PID} ]]; then
        kill "${GENERATOR_PID}" 2> /dev/null || true
    fi
    if [[ -n ${SERVICE_PID} ]]; then
        kill "${SERVICE_PID}" 2> /dev/null || true
    fi

    wait 2> /dev/null || true

    if [[ -n ${OTEL_STARTED} ]]; then
        echo "stopping the otel-lgtm collector"
        "${CONTAINER_RUNTIME}" stop "${OTEL_CONTAINER}" 2> /dev/null || true
    fi
}
trap cleanup INT TERM EXIT

if observability_on; then
    if command -v docker > /dev/null 2>&1; then
        CONTAINER_RUNTIME="docker"
    elif command -v podman > /dev/null 2>&1; then
        CONTAINER_RUNTIME="podman"
    else
        echo "OBSERVABILITY is set but neither docker nor podman is available" >&2
        exit 1
    fi

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

echo "starting the generator"
"${BIN_DIR}/generator" --address "localhost:${PORT}" --data_dir "${DATA_DIR}" --max_bytes "${MAX_BYTES}" "$@" &
GENERATOR_PID=$!

echo "service pid ${SERVICE_PID}, generator pid ${GENERATOR_PID} - ctrl-c to stop"
wait "${GENERATOR_PID}" "${SERVICE_PID}"
