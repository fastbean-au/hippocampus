#!/usr/bin/env python3
"""Bump the Homebrew tap formulae to a new release.

Rewrites the download URLs (version) and their sha256 lines in each formula, reading the hashes
from the release's checksums.txt. Called by the release workflow after the binaries job has
published the assets, but is a plain script so it can be run and tested by hand:

    deploy/homebrew/bump-formulae.py v0.21.0 checksums.txt ../homebrew-tap/Formula

The formulae pair each `url "…/download/vX/<asset>_vX_<os>_<arch>.tar.gz"` line with the `sha256`
line immediately below it. For every url line we rewrite the version token, then look the new asset
filename up in the checksums map and rewrite the following sha256. A url whose asset is absent from
checksums.txt is a hard error (a formula must never keep a stale hash against a new url).
"""

import re
import sys
from pathlib import Path

# url line: capture the leading `url "`, the fixed download prefix up to the version, and the
# asset filename after it. The version segment (v…) is dropped and rewritten from NEW_VERSION.
URL_RE = re.compile(
    r'^(?P<indent>\s*url ")'
    r"(?P<prefix>https://github\.com/[^\"]*/download/)"
    r"v[^/]+/"
    r"(?P<file>[^\"]+)"
    r'(?P<suffix>")\s*$'
)

SHA_RE = re.compile(r'^(?P<indent>\s*sha256 ")(?P<hash>[0-9a-f]{64})(?P<suffix>")\s*$')

# The version token inside an asset filename, e.g. the v0.20.0 in hippo_v0.20.0_darwin_arm64.tar.gz.
FILE_VERSION_RE = re.compile(r"_v[^_]+_")


def load_checksums(path: Path) -> dict[str, str]:
    """Parse `sha256  ./filename` lines into a {filename: sha256} map."""
    out: dict[str, str] = {}

    for line in path.read_text().splitlines():
        line = line.strip()

        if not line:
            continue

        digest, _, name = line.partition(" ")
        name = name.strip().lstrip("./")

        if len(digest) == 64 and name:
            out[name] = digest

    return out


def bump_formula(path: Path, new_version: str, checksums: dict[str, str]) -> bool:
    """Rewrite one formula in place. Returns True if the file changed."""
    lines = path.read_text().splitlines(keepends=True)
    pending_file: str | None = None
    changed = False

    for i, line in enumerate(lines):
        url_match = URL_RE.match(line)

        if url_match:
            new_file = FILE_VERSION_RE.sub(f"_{new_version}_", url_match["file"])
            pending_file = new_file
            newline = "\n" if line.endswith("\n") else ""
            rebuilt = (
                f'{url_match["indent"]}{url_match["prefix"]}{new_version}/'
                f'{new_file}{url_match["suffix"]}{newline}'
            )

            if rebuilt != line:
                lines[i] = rebuilt
                changed = True

            continue

        sha_match = SHA_RE.match(line)

        if sha_match and pending_file is not None:
            if pending_file not in checksums:
                raise SystemExit(
                    f"{path.name}: no checksum for {pending_file} in checksums.txt"
                )

            digest = checksums[pending_file]
            pending_file = None

            if digest != sha_match["hash"]:
                newline = "\n" if line.endswith("\n") else ""
                lines[i] = f'{sha_match["indent"]}{digest}{sha_match["suffix"]}{newline}'
                changed = True

    if changed:
        path.write_text("".join(lines))

    return changed


def main() -> None:
    if len(sys.argv) != 4:
        raise SystemExit(
            "usage: bump-formulae.py <vX.Y.Z> <checksums.txt> <tap-Formula-dir>"
        )

    new_version, checksums_path, formula_dir = sys.argv[1], sys.argv[2], sys.argv[3]

    if not new_version.startswith("v"):
        raise SystemExit(f"version must be tag-shaped (vX.Y.Z), got: {new_version}")

    checksums = load_checksums(Path(checksums_path))
    any_changed = False

    for name in ("hippocampus.rb", "hippocampus-cli.rb", "hippocampus-mcp.rb"):
        path = Path(formula_dir) / name

        if not path.exists():
            raise SystemExit(f"formula not found: {path}")

        if bump_formula(path, new_version, checksums):
            any_changed = True
            print(f"bumped {name} -> {new_version}")
        else:
            print(f"{name} already at {new_version}")

    if not any_changed:
        print("no formula changes")


if __name__ == "__main__":
    main()
