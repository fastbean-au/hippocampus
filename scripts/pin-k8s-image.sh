#!/usr/bin/env bash
# Pin the hippocampus image in every Kubernetes overlay to one release (TODO-3 item 170).
#
#   scripts/pin-k8s-image.sh 0.52.0 [repository root]
#
# The overlays pin the tag through kustomize's `images:` stanza rather than running `:latest`, which
# let a node that pulled afresh run a newer build than its peers - and a newer build migrates the
# schema forward, after which a rollback is refused. scripts/release.sh runs this as it cuts a
# release, and cmd/hippocampus/k8s_test.go holds every overlay to the newest released version.
#
# Only the newTag under `- name: ghcr.io/fastbean-au/hippocampus` changes; any other image an
# overlay pins is left alone. Idempotent.
set -euo pipefail

version="${1:?usage: pin-k8s-image.sh VERSION [ROOT]}"
version="${version#v}"
root="${2:-$(cd "$(dirname "$0")/.." && pwd)}"

if ! printf '%s' "$version" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$'; then
	echo "pin-k8s-image: '$version' is not a semver version (X.Y.Z)" >&2
	exit 1
fi

shopt -s nullglob
overlays=("$root"/deploy/k8s/overlays/*/kustomization.yaml)

if [ "${#overlays[@]}" -eq 0 ]; then
	echo "pin-k8s-image: no overlays under $root/deploy/k8s/overlays" >&2
	exit 1
fi

for file in "${overlays[@]}"; do
	VERSION="$version" perl -0pi -e '
		my $count = s{(-\s+name:\s+ghcr\.io/fastbean-au/hippocampus\s*\n\s+newTag:\s+)"?[^"\n]*"?}{$1"$ENV{VERSION}"}g;
		die "no ghcr.io/fastbean-au/hippocampus image stanza\n" unless $count;
	' "$file" || {
		echo "pin-k8s-image: $file has no images stanza for ghcr.io/fastbean-au/hippocampus" >&2
		exit 1
	}

	echo "pinned ${file#"$root"/} to $version"
done
