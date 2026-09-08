"""Generate the gRPC stubs for the client package from contract/hippocampus.proto.

The stubs are NOT committed. Item 64's rule is that a published client must ship from the same
tag the contract ships from, and a committed copy of generated code is exactly the "fifth copy of
the contract that drifts" that rule exists to prevent - so the build regenerates from the one
source of truth every time, and the working tree holds none of it.

One transform is applied on the way past, and it is the reason this is a script rather than a
protoc invocation in a Makefile. hippocampus.proto imports protoc-gen-openapiv2's annotations to
carry the OpenAPI document's securityDefinitions, and protoc emits a corresponding
`from protoc_gen_openapiv2.options import annotations_pb2` into hippocampus_pb2.py - an import that
is unsatisfiable in Python, because those annotations have no PyPI distribution (googleapis-common-
protos supplies google.api, and nothing supplies this). Generating without stripping it produces a
package that installs cleanly and raises ModuleNotFoundError on first import.

The option is generator input: it shapes the OpenAPI document protoc-gen-openapiv2 writes and
appears nowhere in the wire format, so removing it changes no message, no field number and no
method. docs/clients.md documents the same removal as the escape hatch for a toolchain that cannot
resolve the annotation protos.

Both removals are asserted rather than attempted, because the failure mode of a silent no-op is
a released wheel that cannot be imported: a reformatted proto that this script no longer matches
must stop the build here, not at a user's interpreter.
"""

import argparse
import pathlib
import re
import shutil
import subprocess
import sys
import tempfile

PACKAGE_INIT = '''"""Generated protobuf and gRPC stubs - do not edit.

Written by scripts/generate_stubs.py at build time from contract/hippocampus.proto, and absent
from the source tree. See that script for why the openapiv2 annotations are stripped on the way
past, and hippocampus.proto (the re-export module) for how these are reached.
"""
'''

# Where the generated stubs land, relative to the source root - and, because protoc derives a
# module's import path from the proto's path, also where the contract is staged before generation.
PACKAGE_PATH = "hippocampus/_proto"

OPENAPIV2_IMPORT = re.compile(
    r'^import "protoc-gen-openapiv2/options/annotations\.proto";\n',
    re.MULTILINE,
)

OPENAPIV2_OPTION = re.compile(
    r"^option \(grpc\.gateway\.protoc_gen_openapiv2\.options\.openapiv2_swagger\) = \{.*?^\};\n",
    re.MULTILINE | re.DOTALL,
)


def strip_openapiv2(source: str) -> str:
    """Remove the openapiv2 import and its option block, insisting that both were present."""

    stripped, imports = OPENAPIV2_IMPORT.subn("", source)
    if imports != 1:
        raise SystemExit(
            f"expected exactly 1 openapiv2 import in the contract, removed {imports} - "
            "the proto's formatting has changed and this script must be updated"
        )

    stripped, options = OPENAPIV2_OPTION.subn("", stripped)
    if options != 1:
        raise SystemExit(
            f"expected exactly 1 openapiv2_swagger option in the contract, removed {options} - "
            "the proto's formatting has changed and this script must be updated"
        )

    return stripped


def generate(contract: pathlib.Path, root: pathlib.Path) -> None:
    """Stage a stripped copy of the contract and generate stubs into `root`/hippocampus/_proto."""

    out = root / PACKAGE_PATH

    proto = contract / "hippocampus.proto"
    if not proto.is_file():
        raise SystemExit(f"contract not found at {proto}")

    out.mkdir(parents=True, exist_ok=True)

    with tempfile.TemporaryDirectory() as tmp:
        stage = pathlib.Path(tmp)

        # The contract is staged under the package path it will be generated into, rather than at
        # the root of the include directory, and that is what fixes the generated modules' imports.
        # protoc derives them from a proto's PATH: staged bare, hippocampus_pb2_grpc.py gets
        # `import hippocampus_pb2`, a top-level import that resolves only if the stub directory is
        # itself on sys.path and never inside a subpackage. Staged here, it gets
        # `from hippocampus._proto import hippocampus_pb2`, which is what the installed package
        # actually is. The alternative - rewriting the import in the generated file afterwards - is
        # the usual workaround for this and edits generated code to do it.
        nested = stage / PACKAGE_PATH
        nested.mkdir(parents=True)
        (nested / "hippocampus.proto").write_text(strip_openapiv2(proto.read_text()))

        # google/api is kept rather than stripped alongside the openapiv2 options: it does have a
        # PyPI distribution (googleapis-common-protos, which grpcio-status already pulls in), and
        # keeping it leaves the HTTP bindings readable in the descriptor for anything that wants
        # to know how an RPC maps onto /v1.
        shutil.copytree(contract / "google", stage / "google")

        command = [
            sys.executable,
            "-m",
            "grpc_tools.protoc",
            f"-I{stage}",
            f"--python_out={root}",
            f"--pyi_out={root}",
            f"--grpc_python_out={root}",
            f"{PACKAGE_PATH}/hippocampus.proto",
        ]

        subprocess.run(command, check=True)

    (out / "__init__.py").write_text(PACKAGE_INIT)

    written = sorted(p.name for p in out.glob("*.py*") if p.is_file())
    print(f"generated {', '.join(written)} into {out}")


def main() -> None:
    here = pathlib.Path(__file__).resolve().parent
    root = here.parent

    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--contract",
        type=pathlib.Path,
        default=root.parent.parent / "contract",
        help="directory holding hippocampus.proto (default: the repository's contract/)",
    )
    parser.add_argument(
        "--out",
        type=pathlib.Path,
        default=root / "src",
        help=f"source root to write {PACKAGE_PATH}/ into",
    )

    args = parser.parse_args()

    generate(args.contract.resolve(), args.out.resolve())


if __name__ == "__main__":
    main()
