"""Guards on the things that are only wrong once the package is published or the client moves.

None of these is reachable by driving the block: each is a property of the distribution, of the
surface this package is allowed to touch, or of a document that copies a table in the code.
"""

from __future__ import annotations

import pathlib

import pytest
from hippocampus import Hippocampus
from llama_index.memory.hippocampus import DEFAULT_SIGNIFICANCE, MemoryClient
from llama_index.memory.hippocampus import __version__

import llama_index.memory.hippocampus as adapter

ROOT = pathlib.Path(__file__).resolve().parent.parent
PACKAGE = ROOT / "src" / "llama_index" / "memory" / "hippocampus"

# tomllib is 3.11+ and the declared floor is 3.10, so without the tomli fallback these guards would
# skip on precisely the interpreter the CI matrix runs them to protect.
try:
    import tomllib
except ModuleNotFoundError:  # pragma: no cover - only on the floor interpreter
    import tomli as tomllib


@pytest.fixture(scope="module")
def pyproject():
    with (ROOT / "pyproject.toml").open("rb") as handle:
        return tomllib.load(handle)


def test_the_client_still_satisfies_the_protocol():
    """The one drift that fails at runtime and nowhere else.

    `MemoryClient` is a structural view of `Hippocampus`, so a renamed method on the client leaves
    this package importing, constructing and then failing on the first call into the store.
    """

    assert issubclass(Hippocampus, MemoryClient), (
        "hippocampus.Hippocampus no longer answers every method MemoryClient names"
    )


def test_the_reachable_surface_is_exactly_four_calls():
    """The protocol is a security statement, not a convenience.

    `Hippocampus` exposes every RPC the contract declares, `purge` and `clear` among them. What
    keeps an agent's memory away from those is this list and nothing else, so growing it is a
    decision rather than an import.
    """

    named = {
        name
        for name in dir(MemoryClient)
        if not name.startswith("_") and callable(getattr(MemoryClient, name))
    }

    assert named == {"who_am_i", "store_memory", "store_memories", "search_memories"}


def test_the_readme_significance_table_matches_the_defaults():
    """Two copies of a table nothing executes is what drifts, so the document is held to the code."""

    readme = (ROOT / "README.md").read_text()

    documented = {}

    for line in readme.splitlines():
        if not line.startswith("| `"):
            continue

        columns = [column.strip() for column in line.strip("|").split("|")]
        role, significance = columns[0].strip("`"), columns[1]

        if significance.isdigit():
            documented[role] = int(significance)

    assert documented == DEFAULT_SIGNIFICANCE, (
        "README.md's significance table and DEFAULT_SIGNIFICANCE disagree"
    )


def test_the_namespace_directories_carry_no_init():
    """An __init__.py above the leaf shadows every other llama_index integration installed."""

    for directory in (PACKAGE.parent.parent, PACKAGE.parent):
        assert not (directory / "__init__.py").exists(), (
            f"{directory.name} is a namespace package and must stay __init__.py-less"
        )


def test_the_package_root_is_the_namespace_root(pyproject):
    """Packaging the leaf instead would install `hippocampus/`, colliding with the client."""

    assert pyproject["tool"]["hatch"]["build"]["targets"]["wheel"]["packages"] == [
        "src/llama_index"
    ]


def test_the_version_is_a_placeholder_in_the_tree():
    """The release workflow stamps the tag in, so a hand-edited number here is a package claiming
    a release it was not built from."""

    assert __version__ == "0.0.0.dev0", (
        "_version.py has been edited by hand - the version comes from the release tag"
    )


def test_the_client_floor_is_declared(pyproject):
    """Both packages publish from the same tag, so a floor below the client's first release could
    only ever name a distribution that was never uploaded."""

    requirements = pyproject["project"]["dependencies"]

    assert any(
        requirement.startswith("hippocampus-client>=") for requirement in requirements
    )
    assert any(
        requirement.startswith("llama-index-core>=") for requirement in requirements
    )


def test_the_package_declares_its_types():
    assert (PACKAGE / "py.typed").is_file()


def test_the_public_surface_is_importable():
    for name in adapter.__all__:
        assert hasattr(adapter, name), f"__all__ names {name}, which is not exported"


def test_every_module_carries_the_annotations_future_import():
    """It is what keeps modern typing syntax in signatures valid on the declared floor."""

    for module in PACKAGE.glob("*.py"):
        source = module.read_text()

        if "from typing import" in source or "-> " in source:
            assert "from __future__ import annotations" in source, (
                f"{module.name} carries annotations without the future import"
            )
