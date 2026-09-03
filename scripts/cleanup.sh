#!/usr/bin/env bash
#
# Reclaim the disk a Hippocampus build, test, demo or soak run leaves behind.
#
# This exists because the leavings are large, scattered across four places that nothing ties
# together, and individually invisible - a compose stack's abandoned data volume looks exactly like
# a live one, and a dangling image layer has no name to notice. Left alone they accumulated to
# ~23 GB on one developer machine and took it to 263 MB free.
#
# The container half has a trap worth knowing about, and is the reason this is a script rather than
# a line in a README. On macOS the podman VM's disk is a SPARSE file: pruning images and volumes
# frees space inside the guest filesystem and returns none of it to the host, so `podman system df`
# reports gigabytes reclaimed while `df` on the Mac does not move. Only a trim inside the guest
# punches the holes back out of the sparse file. So every path here that deletes container storage
# is followed by that trim, and skipping it makes the whole exercise look like it did nothing.
#
# What it refuses to touch, all of it regenerable-looking but not regenerable:
#
#   ~/.hippocampus          a personal standalone instance's store - real memories, not test data
#   ~/go/pkg/mod            the module cache; re-downloading it needs a network and a while
#   named volumes           the test containers' databases (hippo-test-*, hippo-agent-pg, ...),
#                           which the env-gated integration tests expect to still be there
#   anything in use         volumes attached to a container are refused by the engine itself
#
# Usage:
#   scripts/cleanup.sh [--dry-run] [--images] [--build-cache] [--trunk] [--all]
#
#   (no flags)      the pure garbage: repo build/demo output, dangling image layers, orphaned
#                   anonymous volumes, then the trim. Nothing here costs a re-download.
#   --images        also remove tagged images no container uses (ollama, otel-lgtm, opensearch,
#                   node - several GB, and a slow first compose run afterwards)
#   --build-cache   also clear the Go build cache (`go clean -cache`); the next build is cold
#   --trunk         also clear ~/.cache/trunk; the next `trunk check` re-downloads its linters
#   --all           all of the above
#   --dry-run       list what would go, with sizes; delete nothing
#
# The three opt-in flags are opt-in for one reason: each trades disk for time later. The default
# set trades nothing.

set -euo pipefail

repo_root="$(git rev-parse --show-toplevel)"
cd "${repo_root}"

dry_run=false
do_images=false
do_build_cache=false
do_trunk=false

die() {
	printf 'cleanup: %s\n' "$1" >&2
	exit 1
}

note() { printf '\033[1m==>\033[0m %s\n' "$1"; }

warn() { printf '\033[33mwarning:\033[0m %s\n' "$1" >&2; }

while [[ $# -gt 0 ]]; do
	case "$1" in
	--dry-run) dry_run=true ;;
	--images) do_images=true ;;
	--build-cache) do_build_cache=true ;;
	--trunk) do_trunk=true ;;
	--all)
		do_images=true
		do_build_cache=true
		do_trunk=true
		;;
	-h | --help)
		sed -n '2,/^set -euo/p' "$0" | sed 's/^# \{0,1\}//;$d'
		exit 0
		;;
	*) die "unknown argument: $1" ;;
	esac
	shift
done

# ------------------------------------------------------------------- helpers

size_kb() { du -sk "$1" 2>/dev/null | cut -f1; }

human_kb() {
	awk -v kb="${1:-0}" 'BEGIN {
		if (kb >= 1048576) printf "%.1f GB", kb / 1048576
		else if (kb >= 1024) printf "%.0f MB", kb / 1024
		else printf "%d KB", kb
	}'
}

free_kb() { df -k . | awk 'NR == 2 { print $4 }'; }

# in_use reports whether any process holds the path open, so a cleanup run during a live demo or
# soak does not delete the store out from under it. lsof is absent on some systems; when we cannot
# tell, we assume it is free rather than refusing to clean anything ever.
in_use() {
	command -v lsof >/dev/null 2>&1 || return 1
	lsof +D "$1" >/dev/null 2>&1
}

remove_path() {
	local path="$1"
	local label="$2"
	local kb

	[[ -e ${path} ]] || return 0

	kb="$(size_kb "${path}")"

	if [[ -d ${path} ]] && in_use "${path}"; then
		warn "skipping ${label} (${path}) - a process still has it open"

		return 0
	fi

	if ${dry_run}; then
		note "would remove ${label} - $(human_kb "${kb}")"

		return 0
	fi

	note "removing ${label} - $(human_kb "${kb}")"

	# Vendored Go module trees under some of these are mode 0444 inside read-only directories,
	# which rm refuses outright; make them writable first or the removal half-completes.
	chmod -R u+w "${path}" 2>/dev/null || true
	rm -rf "${path}"
}

# ------------------------------------------------------- repo build and demo output

# Every path here is gitignored, named by the script that creates it, and rebuilt on demand.
note "repository build and demo output"

remove_path demo/bin "demo binaries"
remove_path demo/data "demo store"
remove_path demo/data-bluesky "bluesky demo store"
remove_path demo/soak-runs "soak run output"

for stray in hippocampus-mcp integrations/mcp/mcp integrations/cli/cli integrations/cli/hippo; do
	remove_path "${stray}" "stray binary ${stray}"
done

# Coverage profiles and test binaries, which land wherever the test ran.
while IFS= read -r artefact; do
	remove_path "${artefact}" "${artefact}"
done < <(find . -type f \( -name '*.test' -o -name '*.coverprofile' -o -name 'coverage.*' \) \
	-not -path './.git/*' 2>/dev/null)

# ------------------------------------------------------------- container storage

# Clean whichever engines are installed; artefacts belong to the one that built them, and a machine
# with both has leavings in both.
trimmed=false

for engine in podman docker; do
	command -v "${engine}" >/dev/null 2>&1 || continue
	"${engine}" info >/dev/null 2>&1 || continue

	note "${engine}: dangling image layers"

	if ${dry_run}; then
		"${engine}" images -f dangling=true --format '{{.Size}}\t{{.ID}}' 2>/dev/null |
			sed 's/^/    would remove  /'
	else
		"${engine}" image prune -f >/dev/null 2>&1 || warn "${engine} image prune failed"
	fi

	# Anonymous volumes - the 64-hex-named ones a compose run abandons. Two filters, and both
	# matter: dangling=true excludes every volume still attached to a container (the test
	# databases are attached to stopped containers, which is exactly how they survive a reboot),
	# and the name pattern then excludes the NAMED unused ones, which are somebody's compose
	# stack state rather than garbage. Relying on the engine to refuse an in-use volume would
	# work for the deletion but would make --dry-run overstate what it is about to do.
	note "${engine}: orphaned anonymous volumes"

	while IFS= read -r volume; do
		[[ -n ${volume} ]] || continue

		if ${dry_run}; then
			printf '    would remove  %s\n' "${volume}"

			continue
		fi

		"${engine}" volume rm "${volume}" >/dev/null 2>&1 || true
	done < <("${engine}" volume ls -q --filter dangling=true 2>/dev/null |
		grep -E '^[0-9a-f]{64}$' || true)

	if ${do_images}; then
		note "${engine}: images no container uses"

		if ${dry_run}; then
			"${engine}" images --format '{{.Size}}\t{{.Repository}}:{{.Tag}}' 2>/dev/null |
				sed 's/^/    candidate     /'
		else
			"${engine}" image prune -a -f >/dev/null 2>&1 || warn "${engine} image prune -a failed"
		fi
	fi

	# The trim described in this script's header. Without it everything above frees nothing that
	# the host can see. Only podman's macOS VM needs it; Docker Desktop reclaims its own disk.
	if [[ ${engine} == podman ]] && ! ${dry_run}; then
		if podman machine list --format '{{.Running}}' 2>/dev/null | grep -q true; then
			note "podman: trimming the VM disk so the host sees the space"
			podman machine ssh 'sudo fstrim -av' >/dev/null 2>&1 ||
				warn "fstrim failed; the space is free inside the VM but not on the host"
			trimmed=true
		fi
	fi
done

if ! ${dry_run} && ! ${trimmed} && command -v podman >/dev/null 2>&1; then
	warn "no running podman machine to trim; on macOS the space stays inside the VM's disk image"
fi

# --------------------------------------------------------------- toolchain caches

if ${do_build_cache}; then
	if command -v go >/dev/null 2>&1; then
		cache="$(go env GOCACHE 2>/dev/null || true)"

		if [[ -n ${cache} ]] && [[ -d ${cache} ]]; then
			if ${dry_run}; then
				note "would clear the Go build cache - $(human_kb "$(size_kb "${cache}")")"
			else
				note "clearing the Go build cache - $(human_kb "$(size_kb "${cache}")")"
				go clean -cache
			fi
		fi
	else
		warn "go not found; skipping the build cache"
	fi
fi

if ${do_trunk}; then
	remove_path "${HOME}/.cache/trunk" "the trunk tool cache"
fi

# ----------------------------------------------------------------------- report

printf '\n'

if ${dry_run}; then
	note "dry run - nothing was deleted"
	printf '\n'
	printf 'Free space is %s. Re-run without --dry-run to reclaim the above.\n' \
		"$(human_kb "$(free_kb)")"
else
	note "done - $(human_kb "$(free_kb)") free"
fi

if ! ${do_images} || ! ${do_build_cache} || ! ${do_trunk}; then
	printf '\n'
	printf 'Not touched (each costs a re-download or a cold rebuild):'
	${do_images} || printf '\n    --images       tagged images no container uses'
	${do_build_cache} || printf '\n    --build-cache  the Go build cache'
	${do_trunk} || printf '\n    --trunk        ~/.cache/trunk'
	printf '\n'
fi
