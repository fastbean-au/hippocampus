"""The client, driven over a real channel against a recording fake."""

from __future__ import annotations

import datetime
from unittest import mock

import grpc
import pytest

import hippocampus as hp
from hippocampus import Hippocampus
from hippocampus._proto import hippocampus_pb2 as pb


# ------------------------------------------------------------------- the traps


def test_a_rejected_memory_is_not_an_error(client, service):
    """Below-minimum significance is the store declining to retain, not a failure - so the call
    succeeds, the id is empty, and the result is falsey rather than raising."""

    service.responses["StoreMemory"] = pb.StoreMemoryResponse(rejected=True)

    stored = client.store_memory("barely worth saying", significance=1)

    assert stored.rejected is True
    assert stored.id == ""
    assert not stored


def test_a_stored_memory_is_truthy(client):
    stored = client.store_memory("the deploy rolled back cleanly", significance=50)

    assert stored
    assert stored.id == "m-1"


def test_timestamps_cross_the_wire_as_unixnano(client, service):
    when = datetime.datetime(2026, 9, 8, 4, 30, tzinfo=datetime.timezone.utc)

    client.store_memory("a thing that happened", significance=50, timestamp=when)

    assert service.requests["StoreMemory"].time_stamp == int(when.timestamp() * 1_000_000_000)


def test_a_never_recalled_memory_reads_back_as_none(client, service):
    service.responses["GetMemories"] = pb.GetMemoriesResponse(
        memories=[pb.Memory(id="m-1", body="x", time_recalled=0)], total_count=1
    )

    memory = client.get_memories()[0]

    assert memory.recalled_at is None
    assert memory.recall_count == 0


def test_an_open_event_reads_back_as_none(client, service):
    service.responses["GetEvents"] = pb.GetEventsResponse(
        events=[pb.Event(id="e-1", name="deploy", time_end=0)], total_count=1
    )

    assert client.get_events()[0].ended_at is None


def test_never_recalled_is_asked_with_recalled_not_a_bound(client, service):
    """recall_count_max=0 means no bound, so the tri-state filter is the only way to ask."""

    client.get_memories(recalled=False)

    request = service.requests["GetMemories"]

    assert request.recalled == pb.Bool.FALSE
    assert request.recall_count_max == 0


def test_unset_filters_apply_no_restriction(client, service):
    client.get_memories()

    request = service.requests["GetMemories"]

    assert request.recalled == pb.Bool.UNSPECIFIED
    assert request.has_event == pb.Bool.UNSPECIFIED
    assert request.is_summary == pb.Bool.UNSPECIFIED
    assert request.is_binary == pb.Bool.UNSPECIFIED


# ------------------------------------------------------------------- encodings


def test_metadata_travels_as_pairs(client, service):
    client.get_memories(metadata={"source": "slack", "project": "x"})

    assert sorted(service.requests["GetMemories"].metadata) == [
        "project=x",
        "source=slack",
    ]


def test_metadata_on_a_record_is_a_map(client, service):
    client.store_memory("x", significance=50, metadata={"source": "slack"})

    assert dict(service.requests["StoreMemory"].metadata) == {"source": "slack"}


def test_a_binary_memory_sets_the_tristate_true(client, service):
    client.store_memory("aGVsbG8=", significance=50, binary=True)

    assert service.requests["StoreMemory"].is_binary == pb.Bool.TRUE


def test_a_non_binary_memory_leaves_the_tristate_unset(client, service):
    """UNSPECIFIED and FALSE are identical to the server here, and unset is what a plain write
    has always sent."""

    client.store_memory("x", significance=50)

    assert service.requests["StoreMemory"].is_binary == pb.Bool.UNSPECIFIED


def test_search_mode_is_only_sent_when_chosen(client, service):
    client.search_memories("deploy")
    assert service.requests["SearchMemories"].mode == pb.SEARCH_MODE_UNSPECIFIED

    client.search_memories("deploy", mode=hp.SearchMode.HYBRID)
    assert service.requests["SearchMemories"].mode == pb.SEARCH_MODE_HYBRID


def test_clear_flags_are_off_unless_asked(client, service):
    client.update_memory(hp.Memory(id="m-1", body="new body"))

    request = service.requests["UpdateMemory"]

    assert request.clear_metadata is False
    assert request.clear_group is False


def test_clear_metadata_is_how_metadata_is_removed(client, service):
    """An empty map cannot say it - absent and empty are the same on the wire."""

    client.update_memory(hp.Memory(id="m-1"), clear_metadata=True, clear_group=True)

    request = service.requests["UpdateMemory"]

    assert request.clear_metadata is True
    assert request.clear_group is True


def test_placement_is_carried(client, service):
    client.store_memory("x", placement=hp.Placement.above(anchor=5))

    placement = service.requests["StoreMemory"].placement

    assert placement.mode == pb.SignificancePlacement.ABOVE
    assert placement.anchor == 5


# --------------------------------------------------------------------- results


def test_a_page_reports_the_total_beyond_it(client, service):
    service.responses["GetMemories"] = pb.GetMemoriesResponse(
        memories=[pb.Memory(id="m-1", body="x")], total_count=97
    )

    page = client.get_memories(limit=1)

    assert len(page) == 1
    assert page.total == 97
    assert [memory.id for memory in page] == ["m-1"]


def test_a_batch_reports_per_memory(client, service):
    service.responses["StoreMemories"] = pb.StoreMemoriesResponse(
        results=[
            pb.StoreMemoryResult(id="m-1"),
            pb.StoreMemoryResult(rejected=True),
            pb.StoreMemoryResult(code=6, error="already exists"),
        ],
        stored=1,
        rejected=1,
        failed=1,
    )

    batch = client.store_memories([hp.Memory("a", 50), hp.Memory("b", 1), hp.Memory("c", 50)])

    assert (batch.stored, batch.rejected, batch.failed) == (1, 1, 1)
    assert bool(batch[0]) is True
    assert batch[1].rejected is True and not batch[1].failed
    assert batch[2].failed is True and batch[2].code == 6


def test_identity_reports_the_deployment(client, service):
    service.responses["WhoAmI"] = pb.WhoAmIResponse(
        client_id="agent-1",
        role="writer",
        search_modes=[pb.SEARCH_MODE_KEYWORD, pb.SEARCH_MODE_HYBRID],
        version="v0.42.0",
    )

    identity = client.who_am_i()

    assert identity.role == "writer"
    assert identity.search_enabled
    assert identity.supports(hp.SearchMode.HYBRID)
    assert not identity.supports(hp.SearchMode.SEMANTIC)


def test_no_search_modes_means_search_is_unavailable(client, service):
    service.responses["WhoAmI"] = pb.WhoAmIResponse()

    assert client.who_am_i().search_enabled is False


def test_an_unscoped_token_is_not_scoped_to_nothing(client, service):
    """The two readings of an empty groups list are opposites, which is why group_scoped exists."""

    service.responses["WhoAmI"] = pb.WhoAmIResponse(groups=[], group_scoped=False)

    identity = client.who_am_i()

    assert identity.groups == []
    assert identity.group_scoped is False


def test_links_carry_the_total_the_decay_maths_uses(client, service):
    service.responses["GetMemoryLinks"] = pb.GetLinksResponse(
        links=[
            pb.LinkEdge(id="m-2", significance=10, direction=pb.LINK_DIRECTION_INBOUND),
        ],
        link_significance=10,
    )

    links = client.memory_links("m-1")

    assert links.link_significance == 10
    assert list(links)[0].direction == hp.LinkDirection.INBOUND


# ---------------------------------------------------------------------- errors


@pytest.mark.parametrize(
    "code,expected",
    [
        (grpc.StatusCode.NOT_FOUND, hp.NotFound),
        (grpc.StatusCode.UNAUTHENTICATED, hp.Unauthenticated),
        (grpc.StatusCode.PERMISSION_DENIED, hp.PermissionDenied),
        (grpc.StatusCode.ALREADY_EXISTS, hp.AlreadyExists),
        (grpc.StatusCode.INVALID_ARGUMENT, hp.InvalidArgument),
        (grpc.StatusCode.FAILED_PRECONDITION, hp.FailedPrecondition),
        (grpc.StatusCode.UNAVAILABLE, hp.Unavailable),
        (grpc.StatusCode.RESOURCE_EXHAUSTED, hp.ResourceExhausted),
        (grpc.StatusCode.INTERNAL, hp.ServiceError),
    ],
)
def test_status_codes_become_typed_exceptions(client, service, code, expected):
    service.fail_with = (code, "no")

    with pytest.raises(expected) as raised:
        client.get_memories()

    assert raised.value.code == code
    assert isinstance(raised.value, hp.HippocampusError)


def test_the_original_error_is_preserved(client, service):
    service.fail_with = (grpc.StatusCode.NOT_FOUND, "no such memory")

    with pytest.raises(hp.NotFound) as raised:
        client.get_memories()

    assert "no such memory" in str(raised.value)
    assert isinstance(raised.value.__cause__, grpc.RpcError)


def test_a_dead_address_raises_ours_not_grpcs():
    """A channel-level failure carries no status, and must still not escape as a raw RpcError."""

    with Hippocampus("127.0.0.1:1", timeout=1) as client:
        with pytest.raises(hp.HippocampusError):
            client.who_am_i()


# ----------------------------------------------------------- channel and creds


def test_a_token_travels_as_call_metadata(server, service):
    """Per-call rather than channel credentials, so one code path serves both transports - gRPC
    permits call credentials on a secure channel only, and plaintext behind a TLS-terminating
    sidecar is a supported deployment."""

    with Hippocampus(server, token="abc123", timeout=10) as client:
        client.who_am_i()

    assert service.metadata["WhoAmI"]["authorization"] == "Bearer abc123"


def test_no_token_sends_no_authorization_header(client, service):
    client.who_am_i()

    assert "authorization" not in service.metadata["WhoAmI"]


def test_a_supplied_channel_is_not_closed(server, service):
    """The channel belongs to whoever built it."""

    channel = grpc.insecure_channel(server)

    with Hippocampus(server, channel=channel, timeout=10) as client:
        client.who_am_i()

    # Still usable, so close() left it alone.
    Hippocampus(server, channel=channel, timeout=10).who_am_i()

    channel.close()


def _record_channel_calls(monkeypatch):
    """Capture which constructor a set of options selects, without opening a socket."""

    opened = []

    def insecure(address, options=None):
        opened.append(("insecure", options))
        return mock.MagicMock()

    def secure(address, credentials, options=None):
        opened.append(("secure", options))
        return mock.MagicMock()

    monkeypatch.setattr(grpc, "insecure_channel", insecure)
    monkeypatch.setattr(grpc, "secure_channel", secure)

    return opened


def test_a_plain_address_opens_a_plaintext_channel(monkeypatch):
    opened = _record_channel_calls(monkeypatch)

    Hippocampus("localhost:50051")

    assert [kind for kind, _ in opened] == ["insecure"]


@pytest.mark.parametrize(
    "kwargs",
    [
        {"tls": True},
        {"ca_cert": b"-----BEGIN CERTIFICATE-----"},
        {"client_cert": b"-----BEGIN CERTIFICATE-----"},
    ],
)
def test_any_tls_material_selects_a_secure_channel(monkeypatch, kwargs):
    """Supplying a CA or a client certificate without also setting tls=True must not silently open
    a plaintext channel - the material is the intent."""

    opened = _record_channel_calls(monkeypatch)

    Hippocampus("localhost:50051", **kwargs)

    assert [kind for kind, _ in opened] == ["secure"]


def test_a_server_name_override_reaches_the_channel(monkeypatch):
    opened = _record_channel_calls(monkeypatch)

    Hippocampus("10.0.0.1:50051", tls=True, server_name_override="hippocampus.internal")

    assert ("grpc.ssl_target_name_override", "hippocampus.internal") in opened[0][1]


# ------------------------------------------------------------- whole-surface


def test_every_client_method_reaches_its_rpc(client, service):
    """One call each, asserting the RPC the fake saw - which is what proves the wiring rather than
    just that a method exists."""

    calls = [
        (lambda: client.who_am_i(), "WhoAmI"),
        (lambda: client.topology(), "GetTopology"),
        (lambda: client.significance_levels(), "GetSignificanceLevels"),
        (lambda: client.store_memory("x", 50), "StoreMemory"),
        (lambda: client.store_memories([hp.Memory("x", 50)]), "StoreMemories"),
        (lambda: client.update_memory(hp.Memory(id="m-1")), "UpdateMemory"),
        (lambda: client.delete_memories(["m-1"]), "DeleteMemories"),
        (lambda: client.get_memories(), "GetMemories"),
        (lambda: client.recall_memories(["m-1"]), "RecallMemories"),
        (lambda: client.search_memories("x"), "SearchMemories"),
        (lambda: client.link_memories("m-1", [hp.Link("m-2", 5)]), "LinkMemories"),
        (lambda: client.unlink_memories("m-1", ["m-2"]), "UnlinkMemories"),
        (lambda: client.memory_links("m-1"), "GetMemoryLinks"),
        (lambda: client.store_event("deploy", 50), "StoreEvent"),
        (lambda: client.update_event(hp.Event(id="e-1")), "UpdateEvent"),
        (lambda: client.end_event("e-1"), "EndEvent"),
        (lambda: client.update_event_significance("e-1", 60), "UpdateEventSignificance"),
        (lambda: client.merge_events("e-1", "e-2"), "MergeEvents"),
        (lambda: client.delete_event("e-1"), "DeleteEvent"),
        (lambda: client.get_event("e-1"), "GetEventById"),
        (lambda: client.get_events(), "GetEvents"),
        (lambda: client.link_events("e-1", [hp.Link("e-2", 5)]), "LinkEvents"),
        (lambda: client.unlink_events("e-1", ["e-2"]), "UnlinkEvents"),
        (lambda: client.event_links("e-1"), "GetEventLinks"),
        (lambda: client.summarisation_candidates(), "GetSummarisationCandidates"),
        (
            lambda: client.replace_memories_with_summary("e-1", hp.Memory("s", 50)),
            "ReplaceMemoriesWithSummary",
        ),
        (lambda: client.summarise_memories("e-1"), "SummariseMemories"),
        (lambda: client.sleep(), "Sleep"),
        (lambda: client.purge(), "Purge"),
        (lambda: client.consolidation_status(), "GetConsolidationStatus"),
        (lambda: client.preview_consolidation(), "PreviewConsolidation"),
        (lambda: client.explain_consolidation(["m-1"]), "ExplainConsolidation"),
        (lambda: client.forgotten_memories(), "GetForgottenMemories"),
        (lambda: client.delete_forgotten_memories(all=True), "DeleteForgottenMemories"),
        (lambda: client.callback_queue(), "GetCallbackQueue"),
        (lambda: client.delete_callback_queue(all=True), "DeleteCallbackQueue"),
        (lambda: client.export(), "Export"),
        (lambda: client.import_("key"), "Import"),
        (lambda: client.import_batch(memories=[hp.Memory("x", 50)]), "ImportBatch"),
        (lambda: client.transfer(), "Transfer"),
        (lambda: client.clear("man-1"), "Clear"),
    ]

    for invoke, expected in calls:
        invoke()
        assert service.calls[-1] == expected

    served = {method.name for method in pb.DESCRIPTOR.services_by_name["Hippocampus"].methods}

    assert set(service.calls) == served
