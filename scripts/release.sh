#!/usr/bin/env bash
#
# Cut a Hippocampus release: roll the changelog, commit it, and create the tag.
#
# This exists because the manual process drifted. Seventeen releases (v0.24.0 through v0.32.2)
# shipped without step 6 of RELEASE.md's pre-flight ever being done, so every entry since v0.23.0
# accumulated under one "[Unreleased]" heading and the compatibility promise the changelog makes -
# "read the Breaking section of every release you skip over" - became unfulfillable. Nothing caught
# it, because the tag is what triggers the release and the changelog is not on that path. So this
# script puts it on that path, and refuses to tag when the changelog is not in the state a release
# needs.
#
# It deliberately does NOT push. Pushing the tag is what starts the release workflow - builds,
# GHCR images, the Homebrew tap - and that should be a separate, deliberate keystroke. The script
# prints the command.
#
# Usage:
#   scripts/release.sh --patch | --minor | --major | --version X.Y.Z
#
#   --dry-run       show what would change; write nothing, commit nothing, tag nothing
#   --skip-checks   skip the build/vet/test pre-flight (for re-running after a failure you have
#                   already investigated - not for saving time)
#   --allow-drift   proceed even when the changelog's newest version heading is not the current tag
#
# See RELEASE.md for what the tag then triggers, and for the one-time secrets it needs.

set -euo pipefail

repo_root="$(git rev-parse --show-toplevel)"
cd "$repo_root"

changelog="CHANGELOG.md"
repo_url="https://github.com/fastbean-au/hippocampus"

bump=""
explicit_version=""
dry_run=false
skip_checks=false
allow_drift=false

die() {
	printf 'release: %s\n' "$1" >&2
	exit 1
}

note() { printf '\033[1m==>\033[0m %s\n' "$1"; }

warn() { printf '\033[33mwarning:\033[0m %s\n' "$1" >&2; }

while [ $# -gt 0 ]; do
	case "$1" in
	--patch | --minor | --major)
		bump="${1#--}"
		;;
	--version)
		shift
		[ $# -gt 0 ] || die "--version needs an argument"
		explicit_version="$1"
		;;
	--dry-run) dry_run=true ;;
	--skip-checks) skip_checks=true ;;
	--allow-drift) allow_drift=true ;;
	-h | --help)
		sed -n '3,25p' "$0" | sed 's/^# \{0,1\}//'
		exit 0
		;;
	*)
		die "unknown argument: $1 (see --help)"
		;;
	esac
	shift
done

if [ -n "$bump" ] && [ -n "$explicit_version" ]; then
	die "pass either a bump (--patch/--minor/--major) or --version, not both"
fi

if [ -z "$bump" ] && [ -z "$explicit_version" ]; then
	die "say what to release: --patch, --minor, --major, or --version X.Y.Z (see --help)"
fi

# ---------------------------------------------------------------- preconditions

branch="$(git rev-parse --abbrev-ref HEAD)"

if [ "$branch" != "main" ]; then
	die "on branch '$branch'; releases are cut from main"
fi

if [ -n "$(git status --porcelain)" ]; then
	die "working tree is not clean; commit or stash first"
fi

# A tag must point at a commit that exists on the remote, or the release workflow builds a revision
# nobody else has.
git fetch --quiet --tags origin main 2>/dev/null || warn "could not reach origin; proceeding against local refs"

if git rev-parse --verify --quiet origin/main >/dev/null; then
	if [ "$(git rev-parse HEAD)" != "$(git rev-parse origin/main)" ]; then
		die "HEAD is not origin/main; push or pull first so the tag lands on a commit others have"
	fi
fi

# ------------------------------------------------------------------- versioning

# The current release is the most recent v* tag reachable from HEAD - the same baseline CI's
# proto-breaking job uses. The obsidian plugin's own tags do not match this pattern.
current_tag="$(git describe --tags --abbrev=0 --match 'v[0-9]*' HEAD 2>/dev/null || true)"

if [ -z "$current_tag" ]; then
	current_version="0.0.0"
	note "no release tag yet; treating the current version as 0.0.0"
else
	current_version="${current_tag#v}"
fi

IFS='.' read -r cur_major cur_minor cur_patch <<<"$current_version"

if [ -n "$explicit_version" ]; then
	new_version="${explicit_version#v}"
else
	case "$bump" in
	major) new_version="$((cur_major + 1)).0.0" ;;
	minor) new_version="${cur_major}.$((cur_minor + 1)).0" ;;
	patch) new_version="${cur_major}.${cur_minor}.$((cur_patch + 1))" ;;
	esac
fi

if ! printf '%s' "$new_version" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$'; then
	die "'$new_version' is not a semver version (X.Y.Z)"
fi

new_tag="v${new_version}"

if git rev-parse --verify --quiet "refs/tags/$new_tag" >/dev/null; then
	die "tag $new_tag already exists"
fi

# Refuse to go backwards. sort -V puts the lower version first, so if the new version sorts first it
# is not ahead of the current one.
lowest="$(printf '%s\n%s\n' "$current_version" "$new_version" | sort -V | head -1)"

if [ "$new_version" != "$current_version" ] && [ "$lowest" = "$new_version" ]; then
	die "$new_tag is older than the current release $current_tag"
fi

if [ "$new_version" = "$current_version" ]; then
	die "$new_tag is the current release; nothing to cut"
fi

note "releasing $current_tag -> $new_tag"

# Pre-1.0 permits a breaking change in any minor, which is what this project has used. Say so, since
# it is the difference between a break being licensed and being a mistake.
if [ "$cur_major" = "0" ] && [ "${new_version%%.*}" = "0" ]; then
	note "pre-1.0: a breaking change is permitted in this minor (CHANGELOG.md's Compatibility section)"
fi

# --------------------------------------------------------------------- changelog
#
# Everything that inspects or rewrites the changelog is one python3 pass, so the file is parsed once
# and every check sees the same structure. It writes nothing under --dry-run.

python3 - "$changelog" "$current_version" "$new_version" "$repo_url" "$dry_run" "$allow_drift" <<'PY'
import datetime
import re
import sys

path, current_version, new_version, repo_url, dry_run, allow_drift = sys.argv[1:7]
dry_run = dry_run == "true"
allow_drift = allow_drift == "true"

text = open(path, encoding="utf-8").read()

def die(msg):
    print(f"release: {msg}", file=sys.stderr)
    sys.exit(1)

# The Unreleased heading, optionally carrying the major-increment marker RELEASE.md describes
# ("## [Unreleased] (v2.0.0)"), which is what stands the contract gate down while the work is in
# flight. Rolling the heading is what clears it again, so it must never outlive its release.
# [ \t]*$ rather than \s*$: \s matches newlines, so a greedy \s* swallows the blank line that
# follows the heading and the rewrite below then closes it up against the first "### " section.
heading = re.search(r"^## \[Unreleased\](?: \(v([0-9]+\.[0-9]+\.[0-9]+)\))?[ \t]*$", text, re.M)

if not heading:
    die("no '## [Unreleased]' heading in CHANGELOG.md")

declared = heading.group(1)

if declared and declared != new_version:
    die(
        f"the changelog declares '## [Unreleased] (v{declared})' but this release is v{new_version}. "
        "Either cut the version the changelog names, or correct the heading."
    )

# What is actually under Unreleased, up to the next version heading.
rest = text[heading.end():]
next_heading = re.search(r"^## \[", rest, re.M)
body = rest[: next_heading.start()] if next_heading else rest

# An empty section means there is nothing to release, and cutting one would publish a version whose
# changelog entry is a blank heading. That is the failure this script exists to prevent, so it is an
# error rather than a warning.
if not re.search(r"^\s*[-*]\s+\S", body, re.M):
    die(
        "the [Unreleased] section has no entries. Write what changed before cutting a release - "
        "the GitHub release notes are a commit list, and CHANGELOG.md is the curated record."
    )

# The newest version heading in the file should be the release currently tagged. When it is not, the
# changelog has drifted: entries for shipped releases are still sitting under Unreleased, and rolling
# now would file all of them under one new version.
newest = re.search(r"^## \[([0-9]+\.[0-9]+\.[0-9]+)\]", text, re.M)

# compare_base is what the new section's link reference compares against. Normally that is the
# release this one follows - but when the changelog has drifted, the entries being rolled span every
# release since the newest heading, so comparing against the current tag would under-claim what the
# section covers. Using the heading instead makes the link show exactly the range the entries
# describe.
compare_base = current_version
drift_span = None

if newest and newest.group(1) != current_version and current_version != "0.0.0":
    msg = (
        f"CHANGELOG.md's newest version heading is [{newest.group(1)}] but the current release is "
        f"v{current_version}. Releases between the two shipped without their entries being rolled "
        "out of [Unreleased], so cutting now files all of them under one version."
    )

    if not allow_drift:
        die(msg + " Reconcile the changelog, or pass --allow-drift to accept it.")

    print(f"release: warning: {msg} Proceeding because --allow-drift was given.", file=sys.stderr)

    compare_base = newest.group(1)
    drift_span = (newest.group(1), current_version)

today = datetime.date.today().isoformat()

if dry_run:
    entries = len(re.findall(r"^\s*[-*]\s+\S", body, re.M))
    sections = re.findall(r"^### (.+)$", body, re.M)
    print(f"  would roll [Unreleased] -> [{new_version}] - {today}")
    print(f"  {entries} entr(y|ies) across: {', '.join(sections) or '(no ### sections)'}")
    sys.exit(0)

# Roll the heading and open a fresh, bare Unreleased above it. Bare, with no version marker: the
# marker is a statement about work in flight and re-adding one here would declare the next release's
# shape before anyone has decided it.
# A section covering more than its own release says so. Rolling drifted entries under one version is
# a deliberate choice to record them together rather than fabricate an attribution nobody can check -
# but a reader comparing releases must not be left to infer that from the link reference alone.
note = ""

if drift_span:
    # "since v0.23.0" rather than naming the first release covered: the changelog knows the heading
    # it drifted from, not which tag came next, and an off-by-one in a compatibility note is worse
    # than a slightly looser phrasing. Wrapped to the file's ~100 columns by hand, since prettier
    # leaves prose alone.
    note = (
        f"\n\n_Covers every change since v{drift_span[0]}. The releases between it and "
        f"v{new_version}\nshipped without their entries being rolled out of `[Unreleased]`, so the "
        "changes below are\nrecorded together rather than attributed to the release each shipped "
        "in._"
    )

text = (
    text[: heading.start()]
    + "## [Unreleased]\n\n"
    + f"## [{new_version}] - {today}"
    + note
    + text[heading.end():]
)

# Link references at the foot. The Unreleased comparison moves to the new tag, and the new version
# gets its own line comparing against the release it followed.
unreleased_ref = re.search(r"^\[Unreleased\]: .*$", text, re.M)

if not unreleased_ref:
    die("no '[Unreleased]:' link reference at the foot of CHANGELOG.md")

previous = f"v{compare_base}" if current_version != "0.0.0" else None
new_ref = (
    f"[{new_version}]: {repo_url}/compare/{previous}...v{new_version}"
    if previous
    else f"[{new_version}]: {repo_url}/releases/tag/v{new_version}"
)

text = (
    text[: unreleased_ref.start()]
    + f"[Unreleased]: {repo_url}/compare/v{new_version}...HEAD\n"
    + new_ref
    + text[unreleased_ref.end():]
)

open(path, "w", encoding="utf-8").write(text)
print(f"  rolled [Unreleased] -> [{new_version}] - {today}")
PY

if [ "$dry_run" = true ]; then
	note "dry run: nothing written, nothing tagged"
	exit 0
fi

# ------------------------------------------------------------------- pre-flight
#
# After the changelog edit, so a failing check leaves the rewrite in the working tree to inspect
# rather than making you redo it. Nothing is committed until every check has passed.

if [ "$skip_checks" = false ]; then
	note "pre-flight: module hygiene, vet, format, tests"

	export GOTOOLCHAIN=auto
	unset GOROOT GOTOOLDIR 2>/dev/null || true

	go mod tidy

	if ! git diff --quiet go.mod go.sum; then
		die "go mod tidy changed go.mod/go.sum; review and commit that separately"
	fi

	go vet ./...
	test -z "$(gofmt -l .)" || die "gofmt: $(gofmt -l . | tr '\n' ' ')"
	go test ./...

	# Each integration module is released from this same tag, so a broken one must not ship.
	for module in integrations/mcp integrations/cli integrations/eventsource integrations/ingestor integrations/otel/hippocampusexporter; do
		note "pre-flight: $module"
		(cd "$module" && go build ./... && go vet ./... && go test ./...)
	done

	# The embedded console's JavaScript, which no Go test reaches.
	if command -v node >/dev/null 2>&1; then
		note "pre-flight: embedded console"
		(cd cmd/hippocampus/webuitest && node --test)
	else
		warn "node not found; skipping the embedded console's tests"
	fi
fi

# ------------------------------------------------------------- commit and tag

note "changelog diff:"
git --no-pager diff -- "$changelog" | head -40

printf '\n'
read -r -p "Commit this and tag $new_tag? [y/N] " reply

case "$reply" in
y | Y | yes | YES) ;;
*)
	note "aborted; the changelog edit is left in the working tree"
	exit 1
	;;
esac

git add "$changelog"
git commit -m "Release $new_tag"
git tag -a "$new_tag" -m "$new_tag"

note "committed and tagged $new_tag"
printf '\n'
printf 'Nothing has been pushed. Pushing the tag is what starts the release workflow:\n\n'
printf '    git push origin main\n'
printf '    git push origin %s\n\n' "$new_tag"
printf 'Then follow RELEASE.md'"'"'s "After the release" checks. The Breaking section of the\n'
printf 'changelog must also be pasted into the GitHub release body by hand - the generated\n'
printf 'notes are a commit list.\n'
