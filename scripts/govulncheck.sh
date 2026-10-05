#!/usr/bin/env bash
#
# Run govulncheck over every Go module in the repository and fail on any vulnerability the code
# actually reaches, less a short list of reviewed exceptions.
#
# A wrapper rather than a bare `govulncheck ./...` for two reasons. The repository is six modules,
# and `./...` from the root never descends into the five under integrations/, each of which has a
# dependency tree of its own. And govulncheck has no way to suppress a finding: a report that has
# been read and judged not to apply would otherwise hold the build red until the dependency moves,
# and a gate that is red for a known reason is a gate people stop reading.
#
# Only symbol-level findings fail the run - a vulnerable function the call graph reaches - which is
# what govulncheck's own exit status counts. A vulnerable module that is merely required, or a
# vulnerable package that is imported but whose affected function is never called, is not.
#
# An exception must say why it does not apply, and it expires on its own: when the advisory stops
# being reported (the dependency was bumped), the run fails until the entry is deleted, so the list
# cannot quietly accumulate entries nobody needs.
#
# Usage:
#   scripts/govulncheck.sh
#
# Needs govulncheck on the PATH (`go install golang.org/x/vuln/cmd/govulncheck@<version>`) and jq.

set -euo pipefail

cd "$(dirname "$0")/.."

modules=(
	.
	integrations/mcp
	integrations/cli
	integrations/eventsource
	integrations/ingestor
	integrations/objectstore
)

# Reviewed exceptions: one line per advisory id, "<id> <why it does not apply here>". A list rather
# than an associative array because macOS still ships bash 3.2.
#
# GO-2026-6443: the panic is in internal/xds/server.RouteAndProcess, on servers configured with xDS
# routing; the advisory also lists the transport symbols every gRPC server reaches, which is all
# govulncheck sees here. No module in this repository links an xDS package. Remove when grpc v1.85.0
# is tagged and taken - v1.83.2 carries the fix too, but grpc-gateway v2.31.0 requires v1.84.0
# (TODO-3 item 135).
exceptions='
GO-2026-6443 xDS routing only; no module links xDS
'

# exception prints the reason an advisory is excepted, or nothing.
exception() {
	awk -v id="$1" '$1 == id { $1 = ""; sub(/^ /, ""); print }' <<<"${exceptions}"
}

for cmd in govulncheck jq; do
	if ! command -v "${cmd}" >/dev/null; then
		echo "error: ${cmd} is not on the PATH" >&2

		exit 2
	fi
done

seen=" "
failed=0

for module in "${modules[@]}"; do
	echo "==> ${module}"

	# govulncheck exits non-zero when it finds something; the findings are judged below instead.
	report="$(cd "${module}" && govulncheck -format json ./... || true)"

	if ! jq -e 'select(.config) | .config' <<<"${report}" >/dev/null; then
		echo "error: govulncheck produced no report for ${module}" >&2

		exit 2
	fi

	reached="$(jq -r 'select(.finding) | select(.finding.trace[0].function) | .finding.osv' \
		<<<"${report}" | sort -u)"

	for id in ${reached}; do
		seen="${seen}${id} "
		reason="$(exception "${id}")"

		if [[ -n "${reason}" ]]; then
			echo "    ${id}: excepted (${reason})"

			continue
		fi

		echo "    ${id}: REACHABLE - https://pkg.go.dev/vuln/${id}"
		failed=1
	done

	if [[ -z "${reached}" ]]; then
		echo "    no reachable vulnerabilities"
	fi
done

for id in $(awk 'NF { print $1 }' <<<"${exceptions}"); do
	if [[ "${seen}" != *" ${id} "* ]]; then
		echo "error: exception ${id} is no longer reported by any module; delete it from $0" >&2
		failed=1
	fi
done

exit "${failed}"
