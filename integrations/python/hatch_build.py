"""Regenerate the gRPC stubs before either distribution is built.

The stubs are generated rather than committed (scripts/generate_stubs.py says why), so a build
from the repository has to produce them, and a build from an sdist must not need to: the sdist and
the wheel both carry the generated modules, and neither carries the contract, which lives two
directories up and outside any build root.

That is the whole reason this hook exists rather than a line in a release script - `pip install .`
against a clone has to work, and so does `pip install hippocampus-client` against the index, and
only one of those has a contract to generate from.
"""

import pathlib
import sys

from hatchling.builders.hooks.plugin.interface import BuildHookInterface

sys.path.insert(0, str(pathlib.Path(__file__).parent / "scripts"))

import generate_stubs  # noqa: E402


class StubBuildHook(BuildHookInterface):
    PLUGIN_NAME = "custom"

    def initialize(self, version, build_data):
        root = pathlib.Path(self.root)
        contract = root.parent.parent / "contract"

        # No contract means this is a build from the sdist, which already carries the generated
        # modules - regenerating is impossible and unnecessary. A build from the repository always
        # has one, so the absence is not ambiguous.
        if not (contract / "hippocampus.proto").is_file():
            return

        generate_stubs.generate(contract.resolve(), (root / "src").resolve())
