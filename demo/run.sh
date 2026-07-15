#!/usr/bin/env bash
#
# Builds and launches the hippocampus service together with the load generator. The service uses
# demo/config.json (port 8300, database under demo/data, a two-minute sleep cycle, and demo-tuned
# consolidation settings); the generator drives it until interrupted, pausing whenever the
# database reaches the 1 GiB cap. Ctrl-C stops both.

set -euo pipefail

cd "$(dirname "$0")/.."

DEMO_DIR="demo"
BIN_DIR="${DEMO_DIR}/bin"
DATA_DIR="${DEMO_DIR}/data"
PORT=8300
MAX_BYTES="${MAX_BYTES:-$((1024 * 1024 * 1024))}"

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
}
trap cleanup INT TERM EXIT

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
