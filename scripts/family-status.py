#!/usr/bin/env python3
#
# Report, for every repository in the Hippocampus family, whether it has anything left to release.
#
# WHY THIS EXISTS. The service's release workflow fires a `repository_dispatch` at each satellite,
# and each opens a pull request re-pinning what it tracks of this one. That pull request is where
# the machinery stopped: it merged to `main` and nothing tagged, nothing published, and nothing
# anywhere said so. On 2026-09-20 all three satellites were carrying two unreleased service bumps
# (v0.48.0 and v0.49.0) and the only way to learn that was to open three repositories and read
# their commit lists against their tags.
#
# WHAT THE `pin@release` COLUMN IS FOR, and it is the reason this script reads tags as well as
# branches. Two satellites declare that their version line CONTINUES the service's, which is what
# makes `hippocampus-otel-collector v0.48.0` mean "the collector built against service v0.48.0".
# Nothing enforced it, and it was false in both: `hippocampus-llamaindex v0.48.0` pinned service
# v0.47.0, `hippocampus-otel-collector v0.48.0` required v0.47.0, and `hippocampus-obsidian 0.3.0`
# declared v0.47.0 - all three cut before the v0.48.0 bump had merged. A version number claiming a
# correspondence it does not have is worse than no number, because nothing about it looks wrong. So
# the release tag is compared against the pin the tag itself carries, and a disagreement is
# reported.
#
# It is READ-ONLY. Nothing here tags, merges, dispatches or writes: it answers a question, and the
# two automations it exists to watch over (each satellite's release-on-bump job, and the hub's
# weekly family-status workflow) are what act on the answer.
#
# GITHUB_TOKEN is OPTIONAL, unlike the sibling family scripts - every repository here is public, so
# an anonymous run works and simply shares the lower rate limit. Pass one to avoid that:
#
#   scripts/family-status.py
#   GITHUB_TOKEN=$(gh auth token) scripts/family-status.py
#   scripts/family-status.py --check            # exit non-zero when a repository needs releasing
#   scripts/family-status.py --verbose          # list the unreleased commits
#   scripts/family-status.py hippocampus-obsidian
#   scripts/family-status.py --dispatch-targets v1.2.3   # which satellites a release should tell
#
import argparse
import base64
import json
import os
import re
import sys
import urllib.error
import urllib.request
from datetime import datetime, timezone

OWNER = "fastbean-au"
HUB = "hippocampus"

API = "https://api.github.com"

# Exit codes are a contract with the family-status workflow, which reports an outstanding release as
# a standing issue and a broken run as a failed job. They must not collide: `SystemExit("message")`
# exits 1, which is why nothing here raises that.
OUTSTANDING = 1
BROKEN = 2


def fail(message):
    """Stop, reporting that the question could not be answered rather than answering it."""
    print(f"family-status: {message}", file=sys.stderr)

    sys.exit(BROKEN)


# How each repository names the service version it is built against, and what it does with a
# release tag. `dispatched` is the set the hub's `notify-satellites` job sends the tag to, and
# cmd/hippocampus/family_test.go holds that column against the workflow's own list in both
# directions - a satellite added to the dispatch and not to this table would go stale invisibly,
# which is the exact failure `hippocampus-gen` demonstrates below.
#
# `line` says what a release tag means:
#
#   hub     - this repository IS the service; its tags are the reference every pin names.
#   service - the tag follows the service's, so releasing is mechanical and is automated in that
#             repository by a release-on-bump job.
#   own     - the tag is that repository's own user-facing version, so releasing is a judgement
#             call and stays manual.
#   none    - nothing is released from tags here.
REPOS = [
    # name, pin path, pin kind, line, dispatched
    (HUB, None, None, "hub", False),
    ("hippocampus-llamaindex", "SERVICE_VERSION", "file", "service", True),
    ("hippocampus-otel-collector", "hippocampusexporter/go.mod", "gomod", "service", True),
    ("hippocampus-obsidian", "contract/SERVICE_VERSION", "file", "own", True),
    # Built from `main` as containers rather than from tags, so merging its bump IS its release -
    # which is why it is dispatched to but has no release line. It was the family's cautionary tale
    # until it gained a bump workflow, having sat eleven releases behind on v0.36.1.
    ("hippocampus-gen", "go.mod", "gomod", "none", True),
    # Bumped by the service's own `bump-homebrew` job, so a stale pin here means that job did not
    # run rather than that somebody forgot.
    ("homebrew-tap", "Formula/hippocampus.rb", "formula", "none", False),
    # Deploys `:latest` and pins nothing; whether it needs a paragraph is the release pre-flight's
    # step 7, not a version comparison.
    ("hippocampus-demo-site", None, None, "none", False),
]

# What a release actually ships, for a repository where that is not "everything on main". Only the
# repositories on their OWN version line need this: for the two whose line follows the service's,
# releasing is mechanical and any commit is worth carrying, and they tag themselves anyway.
#
# WHY IT MATTERS. `hippocampus-obsidian` receives a re-vendored contract on every service release,
# and vendoring it changes nothing a user runs: `main.js` is built from `src/` at release time, and
# the contract document is read by the conformance suite, not by the plugin. Reported as work
# awaiting a release it becomes a permanently red line, and a report that is always red is one that
# stops being read - which is the failure this whole check exists to avoid, arriving by the other
# door.
#
# `package.json` is deliberately ABSENT, and that is the one entry worth re-examining if this list
# is ever edited: the plugin declares no runtime `dependencies`, so esbuild bundles nothing from it
# and a dependency bump there is build tooling. The moment a runtime dependency appears that stops
# being true, so `ships_nothing` asks that question of the file rather than assuming the answer.
SHIPS = {
    "hippocampus-obsidian": ("src/", "styles.css", "manifest.json"),
}

# What each satellite actually CONSUMES of the service, as path prefixes in this repository. A pin is
# behind only when one of these paths changed between it and the newer tag - not merely when the
# version number is lower - and the same question decides whether `notify-satellites` tells the
# satellite about a release at all (`--dispatch-targets`). One answer for both is the point: a report
# that called a pin stale for a release nobody dispatched would be a standing issue with no action.
#
# WHY. Most releases, and almost every patch, change nothing a satellite builds against: v0.51.1 was
# console-only, and dispatching it would have opened four pull requests re-pinning an identical
# contract, republished hippocampus-gen's five images, and raised the MINIMUM service version the
# Obsidian plugin declares for no reason. Judging by the paths rather than by the increment is also
# what keeps the one real exception right - a patch fixing a `types` bound is compiled into the
# collector only through its pin, and it is dispatched because `types/` moved, whatever the number.
#
# Go satellites get their imports' packages and nothing else: `go list -deps ./contract ./types`
# reaches no other package in this module. The root go.mod is deliberately absent - a dependency bump
# here raises nothing a satellite needs, and each satellite keeps its own dependencies current.
#
# A repository with no entry falls back to the version comparison, which is the safe direction: it
# is told about every release. cmd/hippocampus/family_test.go requires every dispatched satellite to
# have one anyway, and every prefix to exist, because a misspelt prefix fails the OTHER way - it
# matches nothing, and the satellite is never told again.
SURFACE = {
    "hippocampus-llamaindex": ("contract/hippocampus.proto", "integrations/python/"),
    "hippocampus-otel-collector": ("contract/", "types/"),
    "hippocampus-obsidian": ("contract/hippocampus.swagger.json",),
    "hippocampus-gen": ("contract/",),
}

# What raises the pin, for a repository nothing dispatches to. Without this a stale pin there
# reports the cause it does not have ("nothing will raise this"), when in fact something should have.
RAISED_BY = {
    "homebrew-tap": "the service's own bump-homebrew job",
}

# The file whose runtime dependencies are bundled into what ships. Read whenever it is among what
# changed, which is the only case where its answer can matter.
BUNDLED_MANIFEST = "package.json"

GOMOD_PIN = re.compile(r"^\s*github\.com/fastbean-au/hippocampus (v\S+)$", re.MULTILINE)
FORMULA_PIN = re.compile(r"/releases/download/(v[^/]+)/")
SEMVER = re.compile(r"^v?(\d+)\.(\d+)\.(\d+)")


def api(path, token, allow_missing=False):
    """GET one API path, returning the decoded JSON (or None for an allowed 404)."""
    request = urllib.request.Request(f"{API}/{path}")
    request.add_header("Accept", "application/vnd.github+json")
    request.add_header("X-GitHub-Api-Version", "2022-11-28")

    if token:
        request.add_header("Authorization", f"Bearer {token}")

    try:
        with urllib.request.urlopen(request, timeout=30) as response:
            return json.load(response)

    except urllib.error.HTTPError as err:
        if err.code == 404 and allow_missing:
            return None

        # A rate-limited anonymous run is the one failure worth naming precisely, because the fix
        # is a token rather than a retry.
        if err.code in (403, 429):
            fail(f"GitHub refused {path} (HTTP {err.code}) - set GITHUB_TOKEN to raise the rate limit")

        fail(f"GitHub returned HTTP {err.code} for {path}")

    except urllib.error.URLError as err:
        fail(f"could not reach GitHub for {path}: {err.reason}")


def file_at(repo, path, ref, token):
    """Read one file from a repository at a ref, or None when it is not there."""
    body = api(f"repos/{OWNER}/{repo}/contents/{path}?ref={ref}", token, allow_missing=True)

    if body is None or "content" not in body:
        return None

    return base64.b64decode(body["content"]).decode("utf-8", "replace")


def extract_pin(kind, text):
    """Pull the pinned service version out of whichever file carries it."""
    if text is None:
        return None

    if kind == "file":
        return text.strip() or None

    if kind == "gomod":
        found = GOMOD_PIN.search(text)

        return found.group(1) if found else None

    if kind == "formula":
        found = FORMULA_PIN.search(text)

        return found.group(1) if found else None

    return None


def version(tag):
    """A comparable tuple for a vX.Y.Z tag, or None when it is not one."""
    if not tag:
        return None

    found = SEMVER.match(tag)

    return tuple(int(g) for g in found.groups()) if found else None


_moved = {}


def surface_moved(repo, pin, target, token):
    """Has anything `repo` consumes changed between the service version it pins and `target`?

    Every uncertainty answers yes - an unreadable pin, a comparison GitHub cannot make, a file list
    at the endpoint's cap - because the cost of a needless bump is a pull request, while the cost of
    a missed one is a satellite built against a contract that has moved under it.
    """
    pinned, wanted = version(pin), version(target)

    if not pinned or not wanted:
        return True

    if pinned >= wanted:
        return False

    surface = SURFACE.get(repo)

    if not surface:
        return True

    key = (pin, target)

    if key not in _moved:
        compare = api(f"repos/{OWNER}/{HUB}/compare/{pin}...{target}", token, allow_missing=True)

        if compare is None:
            _moved[key] = None
        else:
            files = compare.get("files", [])

            # The endpoint stops at 300 files without saying so, and a partial list cannot show that
            # a path did NOT change.
            if len(files) >= 300:
                _moved[key] = None
            else:
                paths = set()

                for f in files:
                    paths.add(f["filename"])

                    # A rename moves a path away as well as to somewhere.
                    if f.get("previous_filename"):
                        paths.add(f["previous_filename"])

                _moved[key] = paths

    paths = _moved[key]

    if paths is None:
        return True

    return any(p.startswith(prefix) for p in paths for prefix in surface)


def dispatch_targets(tag, token):
    """The dispatched satellites a release of `tag` changes something for, one per line."""
    if not version(tag):
        fail(f"{tag} is not a vX.Y.Z service tag")

    for name, pin_path, pin_kind, _line, dispatched in REPOS:
        if not dispatched:
            continue

        pin = extract_pin(pin_kind, file_at(name, pin_path, "main", token))

        if surface_moved(name, pin, tag, token):
            print(name)
            print(f"{name}: pinned {dash(pin)}, and what it consumes changed by {tag}", file=sys.stderr)
        else:
            print(f"{name}: pinned {dash(pin)}, and nothing it consumes changed by {tag}", file=sys.stderr)

    return 0


def age_in_days(stamp):
    if not stamp:
        return None

    when = datetime.strptime(stamp, "%Y-%m-%dT%H:%M:%SZ").replace(tzinfo=timezone.utc)

    return (datetime.now(timezone.utc) - when).days


class Status:
    """Everything the report knows about one repository."""

    def __init__(self, name, line, dispatched):
        self.name = name
        self.line = line
        self.dispatched = dispatched

        self.release = None
        self.released_days = None
        self.pin_at_release = None
        self.pin_on_main = None
        self.unreleased = 0
        self.commits = []
        self.changed = []
        self.files_truncated = False
        self.bundles_dependencies = False
        self.pin_behind = False

    @property
    def ships_nothing(self):
        """Is everything unreleased here invisible to whoever installs the release?

        Only asked of a repository on its own version line, and only when the comparison was
        complete: a truncated file list cannot show that nothing shipped, and the honest answer to
        "I could not tell" is to report it rather than to go quiet.
        """
        ships = SHIPS.get(self.name)

        if self.line != "own" or not ships or self.unreleased == 0:
            return False

        if self.files_truncated or not self.changed:
            return False

        if self.bundles_dependencies:
            return False

        return not any(f.startswith(prefix) for f in self.changed for prefix in ships)

    @property
    def needs_release(self):
        if self.line not in ("service", "own"):
            return False

        return self.unreleased > 0 and not self.ships_nothing

    @property
    def pin_is_stale(self):
        """Is `main` pinned to something older than the hub's newest release?"""
        return self.pin_on_main is not None and self.pin_behind

    @property
    def tag_disagrees(self):
        """Does the release tag claim a service version its own pin does not carry?"""
        if self.line != "service" or not self.release or not self.pin_at_release:
            return False

        return version(self.release) != version(self.pin_at_release)


def collect(repo, pin_path, pin_kind, line, dispatched, hub_tag, token):
    status = Status(repo, line, dispatched)

    latest = api(f"repos/{OWNER}/{repo}/releases/latest", token, allow_missing=True)

    if latest:
        status.release = latest.get("tag_name")
        status.released_days = age_in_days(latest.get("published_at"))

    if pin_path:
        status.pin_on_main = extract_pin(pin_kind, file_at(repo, pin_path, "main", token))

        if status.release:
            status.pin_at_release = extract_pin(
                pin_kind, file_at(repo, pin_path, status.release, token)
            )

    # What is on main that no release carries. The comparison is against the release tag rather
    # than against a date, because a tag is what the release workflow actually publishes from.
    if status.release:
        compare = api(
            f"repos/{OWNER}/{repo}/compare/{status.release}...main", token, allow_missing=True
        )

        if compare:
            status.unreleased = compare.get("ahead_by", 0)
            status.commits = [
                c["commit"]["message"].splitlines()[0] for c in compare.get("commits", [])
            ]
            status.changed = [f["filename"] for f in compare.get("files", [])]

            # The comparison endpoint caps both lists - 300 files, 250 commits - and says so only by
            # arriving at the cap, so an incomplete answer has to be inferred. It matters in one
            # direction: a partial file list can never prove that nothing shipped.
            status.files_truncated = (
                len(status.changed) >= 300 or compare.get("total_commits", 0) > len(status.commits)
            )

    # One extra read, and only where it can change the verdict: a repository on its own line whose
    # manifest is among what changed. A build-tooling bump leaves the release identical; a runtime
    # dependency is bundled into it and does not.
    if SHIPS.get(repo) and BUNDLED_MANIFEST in status.changed:
        manifest = file_at(repo, BUNDLED_MANIFEST, "main", token)

        if manifest is None:
            # Unreadable means unknown, and unknown must not read as "nothing shipped".
            status.bundles_dependencies = True
        else:
            try:
                status.bundles_dependencies = bool(json.loads(manifest).get("dependencies"))

            except json.JSONDecodeError:
                status.bundles_dependencies = True

    # Behind by number is not behind: a pin that only misses releases changing nothing it consumes
    # is current, and is not dispatched to either (see SURFACE).
    pinned, hub_latest = version(status.pin_on_main), version(hub_tag)
    status.pin_behind = bool(
        pinned
        and hub_latest
        and pinned < hub_latest
        and surface_moved(repo, status.pin_on_main, hub_tag, token)
    )

    return status


def dash(value):
    return value if value else "—"


def report(rows, hub_tag, legend, verbose):
    print(f"Hippocampus family — release status (hub: {hub_tag})\n")

    header = ("repo", "release", "age", "pin@release", "pin@main", "unreleased")
    widths = [max(len(header[i]), *(len(r[i]) for r in rows)) for i in range(len(header))]

    print("  ".join(h.ljust(w) for h, w in zip(header, widths)).rstrip())

    for row in rows:
        print("  ".join(c.ljust(w) for c, w in zip(row, widths)).rstrip())

    # The two markers are the findings rather than decoration, so they are spelled out whenever one
    # is on screen - a reader meeting `v0.47.0!` for the first time should not have to guess.
    if legend:
        print()

        for line in legend:
            print(f"  {line}")

    if verbose:
        print()

        for status in verbose:
            if not status.commits:
                continue

            print(f"{status.name} — unreleased on main:")

            for subject in status.commits:
                print(f"    {subject}")

            print()


def main():
    parser = argparse.ArgumentParser(
        description="Report what each repository in the Hippocampus family has left to release."
    )
    parser.add_argument(
        "--check",
        action="store_true",
        help="exit non-zero when a repository has unreleased work or a tag that misreports its pin",
    )
    parser.add_argument(
        "--verbose", action="store_true", help="list the unreleased commits for each repository"
    )
    parser.add_argument(
        "--dispatch-targets",
        metavar="TAG",
        help="print the dispatched satellites for which a release of TAG changes something they "
        "consume, one per line, and exit (the notify-satellites job's filter)",
    )
    parser.add_argument("repos", nargs="*", help="limit the report to these repositories")
    args = parser.parse_args()

    token = os.environ.get("GITHUB_TOKEN") or os.environ.get("GH_TOKEN") or ""

    if args.dispatch_targets:
        return dispatch_targets(args.dispatch_targets, token)

    selected = REPOS

    if args.repos:
        known = {r[0] for r in REPOS}

        for name in args.repos:
            if name not in known:
                fail(f"{name} is not in the family; known: {', '.join(sorted(known))}")

        selected = [r for r in REPOS if r[0] in args.repos]

    hub = api(f"repos/{OWNER}/{HUB}/releases/latest", token, allow_missing=True) or {}
    hub_tag = hub.get("tag_name")

    statuses = [collect(*repo, hub_tag, token) for repo in selected]

    rows = [
        (
            s.name,
            dash(s.release),
            f"{s.released_days}d" if s.released_days is not None else "—",
            dash(s.pin_at_release) + ("!" if s.tag_disagrees else ""),
            dash(s.pin_on_main) + ("↓" if s.pin_is_stale else ""),
            (str(s.unreleased) + ("*" if s.ships_nothing else "")) if s.unreleased else "—",
        )
        for s in statuses
    ]

    legend = []

    if any(s.tag_disagrees for s in statuses):
        legend.append("!  the release tag names a service version its own pin does not carry")

    if any(s.pin_is_stale for s in statuses):
        legend.append(f"↓  main is pinned behind {dash(hub_tag)} on something it consumes")

    if any(s.ships_nothing for s in statuses):
        legend.append("*  unreleased, but none of it changes what a release ships")

    report(rows, dash(hub_tag), legend, statuses if args.verbose else None)

    actions = []

    for s in statuses:
        if s.needs_release:
            if s.line == "service":
                want = s.pin_on_main or hub_tag
                actions.append(
                    f"{s.name}: {s.unreleased} unreleased — release {want}. Its release-on-bump job "
                    f"tags this from the pin when one lands on main, so a gap here means that job "
                    f"did not run: re-run 'Release on bump' from that repository's Actions tab, "
                    f"which derives {want} again and does the rest."
                )
            else:
                actions.append(
                    f"{s.name}: {s.unreleased} unreleased — its version line is its own, so this is "
                    f"a judgement call: bump manifest.json, versions.json and package.json together, "
                    f"then push the bare-semver tag."
                )

        if s.tag_disagrees:
            actions.append(
                f"{s.name}: release {s.release} pins service {s.pin_at_release} — the tag claims a "
                f"correspondence it does not have, and only a later release can correct it."
            )

        if s.pin_is_stale and s.dispatched:
            actions.append(
                f"{s.name}: main pins {s.pin_on_main}, behind {hub_tag} — the bump pull request is "
                f"unmerged or was never opened. Re-run its bump workflow with tag {hub_tag}."
            )

        if s.pin_is_stale and not s.dispatched:
            raiser = RAISED_BY.get(s.name)

            actions.append(
                f"{s.name}: pins {s.pin_on_main}, behind {hub_tag} — "
                + (
                    f"{raiser} should have raised it, so check that it ran."
                    if raiser
                    else "it receives no dispatch, so nothing will raise this on its own."
                )
            )

    # Reported, never actioned: a repository whose unreleased commits ship nothing is information
    # about why its number is behind, not a task. Keeping the two apart is what lets the standing
    # issue close.
    noted = [
        f"{s.name}: {s.unreleased} unreleased, none of it touching {', '.join(SHIPS[s.name])} — "
        f"a release would ship an identical plugin, so there is nothing here to cut."
        for s in statuses
        if s.ships_nothing
    ]

    if actions:
        print("\nAction needed:\n")

        for action in actions:
            print(f"  - {action}")
    else:
        print("\nNothing outstanding: every release line is level with what it would ship.")

    if noted:
        print("\nNoted:\n")

        for note in noted:
            print(f"  - {note}")

    if args.check and actions:
        return OUTSTANDING

    return 0


if __name__ == "__main__":
    sys.exit(main())
