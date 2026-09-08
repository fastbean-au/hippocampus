"""The drift guard: every RPC the contract declares has a method on the client.

This is the Python counterpart of the MCP bridge's TestServer_EndToEnd, and it exists for the same
reason. The client's promise is the FULL RPC surface - unlike the MCP bridge, whose surface is
curated - so an RPC arriving in the contract without one here is a gap, and it is exactly the gap
nobody notices, because the package still builds and every existing call still works.

The map is checked in both directions. A new RPC with no entry fails, and an entry naming a method
the client does not have fails - so a renamed method cannot leave a stale row behind.
"""

from __future__ import annotations

import pytest

from hippocampus import Hippocampus
from hippocampus._proto import hippocampus_pb2 as pb

# RPC name -> the method on Hippocampus that serves it.
COVERAGE = {
    "WhoAmI": "who_am_i",
    "GetTopology": "topology",
    "GetSignificanceLevels": "significance_levels",
    "StoreMemory": "store_memory",
    "StoreMemories": "store_memories",
    "UpdateMemory": "update_memory",
    "DeleteMemories": "delete_memories",
    "GetMemories": "get_memories",
    "RecallMemories": "recall_memories",
    "SearchMemories": "search_memories",
    "LinkMemories": "link_memories",
    "UnlinkMemories": "unlink_memories",
    "GetMemoryLinks": "memory_links",
    "StoreEvent": "store_event",
    "UpdateEvent": "update_event",
    "EndEvent": "end_event",
    "UpdateEventSignificance": "update_event_significance",
    "MergeEvents": "merge_events",
    "DeleteEvent": "delete_event",
    "GetEventById": "get_event",
    "GetEvents": "get_events",
    "LinkEvents": "link_events",
    "UnlinkEvents": "unlink_events",
    "GetEventLinks": "event_links",
    "GetSummarisationCandidates": "summarisation_candidates",
    "ReplaceMemoriesWithSummary": "replace_memories_with_summary",
    "SummariseMemories": "summarise_memories",
    "Sleep": "sleep",
    "Purge": "purge",
    "GetConsolidationStatus": "consolidation_status",
    "PreviewConsolidation": "preview_consolidation",
    "ExplainConsolidation": "explain_consolidation",
    "GetForgottenMemories": "forgotten_memories",
    "DeleteForgottenMemories": "delete_forgotten_memories",
    "GetCallbackQueue": "callback_queue",
    "DeleteCallbackQueue": "delete_callback_queue",
    "Export": "export",
    "Import": "import_",
    "ImportBatch": "import_batch",
    "Transfer": "transfer",
    "Clear": "clear",
}


def contract_rpcs():
    return [
        method.name
        for method in pb.DESCRIPTOR.services_by_name["Hippocampus"].methods
    ]


@pytest.mark.parametrize("rpc", contract_rpcs())
def test_every_rpc_has_a_client_method(rpc):
    assert rpc in COVERAGE, (
        f"{rpc} is in the contract with no method on Hippocampus - the package covers the full "
        "RPC surface, so add one and name it here"
    )

    assert callable(getattr(Hippocampus, COVERAGE[rpc], None)), (
        f"COVERAGE maps {rpc} to Hippocampus.{COVERAGE[rpc]}, which does not exist"
    )


def test_coverage_names_no_rpc_the_contract_dropped():
    assert set(COVERAGE) == set(contract_rpcs())


def test_the_fake_serves_every_rpc():
    """The end-to-end fake must answer every RPC, or a client method is tested against
    UNIMPLEMENTED and the test still passes for the wrong reason."""

    from conftest import FakeService

    for rpc in contract_rpcs():
        assert rpc in FakeService.__dict__, f"FakeService does not implement {rpc}"
