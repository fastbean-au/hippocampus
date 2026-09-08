"""Guards on the things that are only wrong once the package is published.

None of these is testable by importing the client and calling it - they are properties of the
distribution, and each has a failure that shows up first in somebody's `pip install`.
"""

from __future__ import annotations

import pathlib
import re

import pytest

import hippocampus

ROOT = pathlib.Path(__file__).resolve().parent.parent

# tomllib is 3.11+ and the declared floor is 3.10, so without the tomli fallback these guards
# would skip on precisely the interpreter the CI matrix runs them to protect.
try:
    import tomllib
except ModuleNotFoundError:  # pragma: no cover - only on the floor interpreter
    import tomli as tomllib


@pytest.fixture(scope="module")
def pyproject():
    with (ROOT / "pyproject.toml").open("rb") as handle:
        return tomllib.load(handle)


def _requirement(pyproject, name: str) -> str:
    for requirement in pyproject["project"]["dependencies"]:
        if requirement.startswith(name):
            return requirement

    raise AssertionError(f"{name} is not a declared dependency")


def test_the_protobuf_floor_matches_what_the_stubs_demand(pyproject):
    """The generated stubs open with a ValidateProtobufRuntimeVersion call naming the grpcio-tools
    that wrote them, and an older runtime raises on import rather than misbehaving subtly. So the
    declared floor is not a preference: regenerating with a newer toolchain raises the demand, and
    a floor left behind ships a wheel that cannot be imported on a resolvable dependency set."""

    generated = (
        ROOT / "src" / "hippocampus" / "_proto" / "hippocampus_pb2.py"
    ).read_text()

    demanded = re.search(
        r"ValidateProtobufRuntimeVersion\(\s*[^,]+,\s*(\d+),\s*(\d+),\s*(\d+),",
        generated,
    )

    assert demanded, "the generated stubs no longer declare a runtime version to check against"

    stub_version = tuple(int(part) for part in demanded.groups())

    declared = re.search(r">=\s*([\d.]+)", _requirement(pyproject, "protobuf"))

    assert declared, "protobuf's requirement declares no lower bound"

    floor = tuple(int(part) for part in declared.group(1).split("."))

    assert floor >= stub_version, (
        f"the stubs require protobuf {'.'.join(map(str, stub_version))} but pyproject.toml "
        f"declares >={declared.group(1)} - raise the floor to match the toolchain that generated "
        "them"
    )


def test_the_version_is_a_placeholder_in_the_tree():
    """The release workflow stamps the tag in. A hand-edited number here is what would let the
    package and the contract claim different versions of one release, which is the thing item 64
    forbids."""

    assert hippocampus.__version__ == "0.0.0.dev0", (
        "src/hippocampus/_version.py has been edited by hand - the version comes from the release "
        "tag, stamped by the release workflow"
    )


def test_the_generated_stubs_are_not_committed():
    """They are regenerated from the contract on every build, and a committed copy is the fifth
    copy of the contract that drifts."""

    ignore = (ROOT / ".gitignore").read_text()

    assert "_proto/" in ignore


def test_the_dev_extra_can_generate_the_stubs(pyproject):
    """Regenerating after a contract edit runs the script directly, which needs grpcio-tools in
    THIS environment - the build backend gets an isolated one, so build-system.requires does not
    put it here."""

    dev = pyproject["project"]["optional-dependencies"]["dev"]

    assert any(requirement.startswith("grpcio-tools") for requirement in dev), (
        "the dev extra cannot run scripts/generate_stubs.py without grpcio-tools"
    )


def test_the_package_declares_its_types():
    assert (ROOT / "src" / "hippocampus" / "py.typed").is_file()


def test_the_public_surface_is_importable():
    """__all__ naming something the package does not export is a broken `from hippocampus import
    *` and a broken documentation page."""

    for name in hippocampus.__all__:
        assert hasattr(hippocampus, name), f"__all__ names {name}, which is not exported"


def test_the_supported_python_floor_is_honest(pyproject):
    """The floor is a promise the dependency set has to be able to keep.

    3.9 was the obvious guess and it was wrong: protobuf 7.x and current grpcio both declare
    >=3.10, so a package claiming 3.9 would be unresolvable on the one interpreter it had gone out
    of its way to promise. The CI matrix runs the floor and the newest release for exactly this -
    a floor nothing tests is a floor that is wrong, and it fails at a user's `pip install`.
    """

    assert pyproject["project"]["requires-python"] == ">=3.10"

    declared = {
        classifier.rsplit(" :: ", 1)[1]
        for classifier in pyproject["project"]["classifiers"]
        if classifier.startswith("Programming Language :: Python :: 3.")
    }

    assert "3.9" not in declared, "3.9 is classified as supported but the dependencies exclude it"


def test_every_module_carries_the_annotations_future_import():
    """It is what keeps modern typing syntax in signatures valid on the declared floor."""

    for module in (ROOT / "src" / "hippocampus").glob("*.py"):
        source = module.read_text()

        if "from typing import" in source or "-> " in source:
            assert "from __future__ import annotations" in source, (
                f"{module.name} carries annotations without the future import"
            )
