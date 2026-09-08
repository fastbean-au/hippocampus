"""A real gRPC server, backed by a recording fake, for the client tests.

The client is driven over an actual channel rather than against a mocked stub, because most of
what it does is at that boundary: metadata, deadlines, status-code translation and the encodings.
A mocked stub would confirm the wrapper calls itself.
"""

from __future__ import annotations

import concurrent.futures
from typing import Any, Dict, List, Optional

import grpc
import pytest

from hippocampus import Hippocampus
from hippocampus._proto import hippocampus_pb2 as pb
from hippocampus._proto import hippocampus_pb2_grpc as rpc


class FakeService(rpc.HippocampusServicer):
    """Records each call and answers with whatever the test queued.

    Every handler follows one shape - record the request and the caller's metadata, then return the
    queued response or a default - so a test asserts on what went onto the wire without the fake
    growing behaviour of its own. `fail_with` makes the next call return a status instead.
    """

    def __init__(self) -> None:
        self.calls: List[str] = []
        self.requests: Dict[str, Any] = {}
        self.metadata: Dict[str, Any] = {}
        self.responses: Dict[str, Any] = {}
        self.fail_with: Optional[tuple] = None

    def _record(self, name: str, request: Any, context: grpc.ServicerContext, default: Any) -> Any:
        self.calls.append(name)
        self.requests[name] = request
        self.metadata[name] = dict(context.invocation_metadata())

        if self.fail_with is not None:
            code, details = self.fail_with
            self.fail_with = None
            context.abort(code, details)

        return self.responses.get(name, default)

    # Identity and deployment.

    def WhoAmI(self, request, context):
        return self._record("WhoAmI", request, context, pb.WhoAmIResponse())

    def GetTopology(self, request, context):
        return self._record("GetTopology", request, context, pb.GetTopologyResponse())

    def GetSignificanceLevels(self, request, context):
        return self._record(
            "GetSignificanceLevels", request, context, pb.GetSignificanceLevelsResponse()
        )

    # Memories.

    def StoreMemory(self, request, context):
        return self._record("StoreMemory", request, context, pb.StoreMemoryResponse(id="m-1"))

    def StoreMemories(self, request, context):
        return self._record("StoreMemories", request, context, pb.StoreMemoriesResponse())

    def UpdateMemory(self, request, context):
        return self._record("UpdateMemory", request, context, pb.GeneralResponse(ok=True))

    def DeleteMemories(self, request, context):
        return self._record("DeleteMemories", request, context, pb.GeneralResponse(ok=True))

    def GetMemories(self, request, context):
        return self._record("GetMemories", request, context, pb.GetMemoriesResponse())

    def RecallMemories(self, request, context):
        return self._record("RecallMemories", request, context, pb.GetMemoriesResponse())

    def SearchMemories(self, request, context):
        return self._record("SearchMemories", request, context, pb.GetMemoriesResponse())

    # Memory links.

    def LinkMemories(self, request, context):
        return self._record("LinkMemories", request, context, pb.GeneralResponse(ok=True))

    def UnlinkMemories(self, request, context):
        return self._record("UnlinkMemories", request, context, pb.GeneralResponse(ok=True))

    def GetMemoryLinks(self, request, context):
        return self._record("GetMemoryLinks", request, context, pb.GetLinksResponse())

    # Events.

    def StoreEvent(self, request, context):
        return self._record("StoreEvent", request, context, pb.StoreEventResponse(id="e-1"))

    def UpdateEvent(self, request, context):
        return self._record("UpdateEvent", request, context, pb.GeneralResponse(ok=True))

    def EndEvent(self, request, context):
        return self._record("EndEvent", request, context, pb.GeneralResponse(ok=True))

    def UpdateEventSignificance(self, request, context):
        return self._record(
            "UpdateEventSignificance", request, context, pb.GeneralResponse(ok=True)
        )

    def MergeEvents(self, request, context):
        return self._record("MergeEvents", request, context, pb.GeneralResponse(ok=True))

    def DeleteEvent(self, request, context):
        return self._record("DeleteEvent", request, context, pb.GeneralResponse(ok=True))

    def GetEventById(self, request, context):
        return self._record("GetEventById", request, context, pb.GetEventResponse())

    def GetEvents(self, request, context):
        return self._record("GetEvents", request, context, pb.GetEventsResponse())

    # Event links.

    def LinkEvents(self, request, context):
        return self._record("LinkEvents", request, context, pb.GeneralResponse(ok=True))

    def UnlinkEvents(self, request, context):
        return self._record("UnlinkEvents", request, context, pb.GeneralResponse(ok=True))

    def GetEventLinks(self, request, context):
        return self._record("GetEventLinks", request, context, pb.GetLinksResponse())

    # Summarisation.

    def GetSummarisationCandidates(self, request, context):
        return self._record(
            "GetSummarisationCandidates",
            request,
            context,
            pb.GetSummarisationCandidatesResponse(),
        )

    def ReplaceMemoriesWithSummary(self, request, context):
        return self._record(
            "ReplaceMemoriesWithSummary",
            request,
            context,
            pb.ReplaceMemoriesWithSummaryResponse(),
        )

    def SummariseMemories(self, request, context):
        return self._record(
            "SummariseMemories", request, context, pb.SummariseMemoriesResponse()
        )

    # Consolidation.

    def Sleep(self, request, context):
        return self._record("Sleep", request, context, pb.GeneralResponse(ok=True))

    def Purge(self, request, context):
        return self._record("Purge", request, context, pb.GeneralResponse(ok=True))

    def GetConsolidationStatus(self, request, context):
        return self._record(
            "GetConsolidationStatus", request, context, pb.GetConsolidationStatusResponse()
        )

    def PreviewConsolidation(self, request, context):
        return self._record(
            "PreviewConsolidation", request, context, pb.PreviewConsolidationResponse()
        )

    def ExplainConsolidation(self, request, context):
        return self._record(
            "ExplainConsolidation", request, context, pb.ExplainConsolidationResponse()
        )

    def GetForgottenMemories(self, request, context):
        return self._record(
            "GetForgottenMemories", request, context, pb.GetForgottenMemoriesResponse()
        )

    def DeleteForgottenMemories(self, request, context):
        return self._record(
            "DeleteForgottenMemories", request, context, pb.DeleteForgottenMemoriesResponse()
        )

    def GetCallbackQueue(self, request, context):
        return self._record(
            "GetCallbackQueue", request, context, pb.GetCallbackQueueResponse()
        )

    def DeleteCallbackQueue(self, request, context):
        return self._record(
            "DeleteCallbackQueue", request, context, pb.DeleteCallbackQueueResponse()
        )

    # Transfer and archive.

    def Export(self, request, context):
        return self._record("Export", request, context, pb.ExportResponse())

    def Import(self, request, context):
        return self._record("Import", request, context, pb.ImportResponse())

    def ImportBatch(self, request, context):
        return self._record("ImportBatch", request, context, pb.ImportBatchResponse())

    def Transfer(self, request, context):
        return self._record("Transfer", request, context, pb.TransferResponse())

    def Clear(self, request, context):
        return self._record("Clear", request, context, pb.ClearResponse())


@pytest.fixture
def service() -> FakeService:
    return FakeService()


@pytest.fixture
def server(service: FakeService):
    server = grpc.server(concurrent.futures.ThreadPoolExecutor(max_workers=2))
    rpc.add_HippocampusServicer_to_server(service, server)

    port = server.add_insecure_port("127.0.0.1:0")
    server.start()

    yield f"127.0.0.1:{port}"

    server.stop(None)


@pytest.fixture
def client(server: str):
    with Hippocampus(server, timeout=10) as client:
        yield client
