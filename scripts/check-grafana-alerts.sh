#!/usr/bin/env bash
#
# Provision the shipped Grafana alert rules into a real Grafana and assert it accepts every one.
#
# This exists because nothing else in the repository executes that file. cmd/hippocampus/alerts_test.go
# holds it to its Prometheus twin and to the README, and checks the wiring somebody thought to check -
# but three copies of a rule can agree perfectly on something Grafana will not accept, and that is
# exactly what shipped in 0.49.0: a 41-character uid, refused by Grafana's own validation (the limit
# is 40). A refused provisioning file does not cost one rule. The alerting provisioner fails, and a
# failed provisioner takes the whole server down with it. Grafana's validation is the authority, so
# this asks Grafana (TODO-2 item 132.1).
#
# It runs the image the compose stacks and demo/run.sh run, mounting the file at the same path, so
# it tests what an operator gets rather than a reconstruction of it. Two things about that image
# matter here:
#
#   - It runs each component through `run_with_logging`, which DISCARDS Grafana's output unless
#     ENABLE_LOGS_GRAFANA=true. Without it a provisioning failure is a container reporting `running`,
#     every other component healthy, no Grafana process and no error anywhere. It is set below so a
#     failure here prints the reason.
#   - It provisions the Prometheus datasource the rules query under uid `prometheus`. A Prometheus
#     with no data in it is what makes the evaluation check meaningful: every expression returns
#     nothing, which is healthy, so a rule reporting an error is a rule Grafana cannot evaluate.
#
# The checks, in order: Grafana answers /api/health at all; the provisioned rules are exactly the
# ones deploy/observability/prometheus-alerts.yaml names (by title, so a missing rule is named rather
# than counted); and every one has been evaluated at least once without an error.
#
# Usage:
#   scripts/check-grafana-alerts.sh
#
# Environment:
#   CONTAINER_RUNTIME   docker or podman (default: whichever is on the path, docker first)
#   GRAFANA_IMAGE       the image to run (default: grafana/otel-lgtm:latest, as the stacks do)
#   GRAFANA_PORT        host port for Grafana's HTTP API (default: 3300, clear of a running stack's 3000)
#   STARTUP_TIMEOUT     seconds to wait for Grafana to answer (default: 180)
#   EVALUATION_TIMEOUT  seconds to wait for every rule to evaluate cleanly (default: 180)

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GRAFANA_RULES="${ROOT}/deploy/compose/observability/alerting-rules.yaml"
PROMETHEUS_RULES="${ROOT}/deploy/observability/prometheus-alerts.yaml"

GRAFANA_IMAGE="${GRAFANA_IMAGE:-grafana/otel-lgtm:latest}"
GRAFANA_PORT="${GRAFANA_PORT:-3300}"
STARTUP_TIMEOUT="${STARTUP_TIMEOUT:-180}"
EVALUATION_TIMEOUT="${EVALUATION_TIMEOUT:-180}"
CONTAINER="hippocampus-grafana-alerts-check-$$"
GRAFANA="http://127.0.0.1:${GRAFANA_PORT}"

if [[ -z ${CONTAINER_RUNTIME:-} ]]; then
	if command -v docker >/dev/null 2>&1; then
		CONTAINER_RUNTIME=docker
	elif command -v podman >/dev/null 2>&1; then
		CONTAINER_RUNTIME=podman
	else
		echo "neither docker nor podman is on the path" >&2

		exit 1
	fi
fi

for tool in curl jq; do
	if ! command -v "${tool}" >/dev/null 2>&1; then
		echo "${tool} is required" >&2

		exit 1
	fi
done

# The verdict goes after Grafana's output rather than before it, so it is the last thing on screen
# instead of scrolling away under twenty-five log lines.
fail() {
	echo "--- grafana output ---" >&2
	"${CONTAINER_RUNTIME}" logs "${CONTAINER}" 2>&1 | grep -iv 'loki\|tempo\|pyroscope\|otelcol\|prometheus' | tail -25 >&2 || true
	echo "FAIL: $*" >&2

	exit 1
}

cleanup() {
	"${CONTAINER_RUNTIME}" rm -f "${CONTAINER}" >/dev/null 2>&1 || true
}

trap cleanup EXIT

# The rule names the Prometheus file declares are the expected set. Read from that file rather than
# from the Grafana one so the check cannot pass by agreeing with the thing it is checking; the two
# are held to each other by alerts_test.go.
expected="$(sed -n 's/^ *- alert: *//p' "${PROMETHEUS_RULES}" | sort)"
expected_count="$(printf '%s\n' "${expected}" | grep -c .)"

echo "starting ${GRAFANA_IMAGE} (${CONTAINER_RUNTIME}) with ${expected_count} rules provisioned"

"${CONTAINER_RUNTIME}" run -d --name "${CONTAINER}" \
	-p "127.0.0.1:${GRAFANA_PORT}:3000" \
	-e ENABLE_LOGS_GRAFANA=true \
	-v "${GRAFANA_RULES}:/otel-lgtm/grafana/conf/provisioning/alerting/hippocampus.yaml:ro" \
	"${GRAFANA_IMAGE}" >/dev/null

# --- Grafana is up -------------------------------------------------------------------------------

deadline=$((SECONDS + STARTUP_TIMEOUT))

until curl -fsS "${GRAFANA}/api/health" >/dev/null 2>&1; do
	if ((SECONDS >= deadline)); then
		fail "Grafana did not answer /api/health within ${STARTUP_TIMEOUT}s - a provisioning file it refuses stops the server starting"
	fi

	sleep 2
done

echo "grafana is up"

# --- The provisioned rules are the shipped ones -------------------------------------------------

provisioned="$(curl -fsS -u admin:admin "${GRAFANA}/api/v1/provisioning/alert-rules" | jq -r '.[].title' | sort)"

if [[ ${provisioned} != "${expected}" ]]; then
	echo "missing from Grafana:" >&2
	comm -23 <(printf '%s\n' "${expected}") <(printf '%s\n' "${provisioned}") >&2
	echo "provisioned but not in the Prometheus file:" >&2
	comm -13 <(printf '%s\n' "${expected}") <(printf '%s\n' "${provisioned}") >&2

	fail "Grafana's provisioned rules differ from the ${expected_count} the Prometheus file declares"
fi

echo "all ${expected_count} rules provisioned"

# --- Every rule evaluates -----------------------------------------------------------------------

# A rule Grafana provisions can still fail every evaluation - an expression its PromQL parser
# refuses, a condition naming a node that does not exist. Against an empty Prometheus every
# expression returns nothing, which is the healthy state (noDataState is OK on every rule), so any
# health other than ok is the rule itself - eventually. The image starts Grafana and its Prometheus
# side by side, and a rule evaluated before the Prometheus answers reports an error that is the
# image's start-up order rather than the rule. So this waits for every rule to be evaluated AND
# healthy, and only a rule still failing at the deadline fails the check; rules re-evaluate every
# minute, so a transient error clears on its own.
deadline=$((SECONDS + EVALUATION_TIMEOUT))

while true; do
	rules="$(curl -fsS -u admin:admin "${GRAFANA}/api/prometheus/grafana/api/v1/rules" | jq -c '[.data.groups[].rules[]]' || true)"
	pending="$(jq -r '.[] | select(.health != "ok" or ((.lastEvaluation // "") | startswith("0001-") or . == "")) | "\(.name): \(.health) \(.lastError // "")"' <<<"${rules:-[]}")"

	if [[ -n ${rules} && -z ${pending} ]]; then
		break
	fi

	if ((SECONDS >= deadline)); then
		echo "${pending}" >&2

		fail "rules not evaluated cleanly within ${EVALUATION_TIMEOUT}s"
	fi

	sleep 5
done

echo "all ${expected_count} rules evaluated cleanly"
