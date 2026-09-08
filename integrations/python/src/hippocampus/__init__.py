"""Hippocampus client - a memory service that forgets what stops mattering.

    from hippocampus import Hippocampus

    with Hippocampus("localhost:50051") as client:
        client.store_memory("the deploy at 14:03 rolled back cleanly", significance=50)

        for memory in client.search_memories("deploy", limit=5, reinforce=True):
            print(memory.body, memory.recall_count)

Three things about this service surprise a client written against an ordinary store, and all three
are the product rather than faults:

- Memories disappear. A consolidation cycle deletes what has stopped mattering, so an id a client
  holds can stop resolving at any time. Treat a missing id as expected.
- Insignificance is not an error. A memory below the deployment's minimum significance is quietly
  dropped: the call succeeds and the result's `rejected` is true. `Stored` is falsey in that case,
  so `if client.store_memory(...)` reads correctly.
- Recall is a write. `recall_memories`, and `search_memories(reinforce=True)`, reset the decay
  clock and raise effective significance. `get_memories` is the read.

Feature-detect with `who_am_i()` rather than probing - which search modes this deployment serves,
whether it has an embedded summariser, whether it consolidates at all.
"""

from hippocampus._version import __version__
from hippocampus.client import DEFAULT_TIMEOUT, Hippocampus, connect
from hippocampus.errors import (
    AlreadyExists,
    DeadlineExceeded,
    FailedPrecondition,
    HippocampusError,
    InvalidArgument,
    NotFound,
    PermissionDenied,
    ResourceExhausted,
    ServiceError,
    Unauthenticated,
    Unavailable,
)
from hippocampus.models import (
    BatchResult,
    BatchStored,
    Event,
    Identity,
    Link,
    LinkDirection,
    LinkEdge,
    Links,
    Memory,
    Page,
    Placement,
    PlacementMode,
    SearchMode,
    SignificanceExtremum,
    SortDirection,
    Stored,
)

# The generated messages, for the operator-surface responses this package returns unwrapped and
# for anything reached through `Hippocampus.stub` directly.
from hippocampus._proto import hippocampus_pb2 as proto  # noqa: E402

__all__ = [
    "__version__",
    "DEFAULT_TIMEOUT",
    "Hippocampus",
    "connect",
    "proto",
    # Records.
    "Event",
    "Link",
    "LinkEdge",
    "Links",
    "Memory",
    "Placement",
    # Results.
    "BatchResult",
    "BatchStored",
    "Identity",
    "Page",
    "Stored",
    # Enumerations.
    "LinkDirection",
    "PlacementMode",
    "SearchMode",
    "SignificanceExtremum",
    "SortDirection",
    # Errors.
    "AlreadyExists",
    "DeadlineExceeded",
    "FailedPrecondition",
    "HippocampusError",
    "InvalidArgument",
    "NotFound",
    "PermissionDenied",
    "ResourceExhausted",
    "ServiceError",
    "Unauthenticated",
    "Unavailable",
]
