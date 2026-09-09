"""The record types, and the round trips through the wire form."""

from __future__ import annotations

import datetime

import hippocampus as hp
from hippocampus._proto import hippocampus_pb2 as pb


def test_a_memory_round_trips():
    when = datetime.datetime(2026, 9, 8, 4, 30, tzinfo=datetime.timezone.utc)

    memory = hp.Memory(
        body="the deploy rolled back cleanly",
        significance=50,
        id="m-1",
        event_id="e-1",
        group="deploys",
        metadata={"source": "ci"},
        timestamp=when,
        binary=False,
        links=[hp.Link("m-2", 10)],
        external_bytes=40960,
    )

    decoded = hp.Memory.from_proto(memory.to_proto())

    assert decoded.body == memory.body
    assert decoded.significance == 50
    assert decoded.event_id == "e-1"
    assert decoded.group == "deploys"
    assert decoded.metadata == {"source": "ci"}
    assert decoded.timestamp == when
    assert decoded.links == [hp.Link("m-2", 10)]

    # The external capacity axis: a size this store only points at, carried both ways so an agent
    # writing pointer-memories can read back what it recorded.
    assert decoded.external_bytes == 40960


def test_an_event_round_trips():
    started = datetime.datetime(2026, 9, 8, 4, tzinfo=datetime.timezone.utc)

    event = hp.Event(
        name="deploy",
        significance=60,
        id="e-1",
        description="the 14:03 deploy",
        group="deploys",
        metadata={"env": "prod"},
        started_at=started,
    )

    decoded = hp.Event.from_proto(event.to_proto())

    assert decoded.name == "deploy"
    assert decoded.started_at == started
    assert decoded.ended_at is None
    assert decoded.metadata == {"env": "prod"}


def test_empty_strings_decode_as_absent():
    """event_id and group are optional, and the wire has no way to say so but an empty string."""

    decoded = hp.Memory.from_proto(pb.Memory(id="m-1", body="x"))

    assert decoded.event_id is None
    assert decoded.group is None


def test_enums_take_their_values_from_the_contract():
    """Restating the numbers here would let a renumbered value drift silently."""

    assert int(hp.SearchMode.KEYWORD) == pb.SEARCH_MODE_KEYWORD
    assert int(hp.SearchMode.SEMANTIC) == pb.SEARCH_MODE_SEMANTIC
    assert int(hp.SearchMode.HYBRID) == pb.SEARCH_MODE_HYBRID
    assert int(hp.LinkDirection.BOTH) == pb.LINK_DIRECTION_BOTH
    assert int(hp.LinkDirection.INBOUND) == pb.LINK_DIRECTION_INBOUND
    assert int(hp.LinkDirection.OUTBOUND) == pb.LINK_DIRECTION_OUTBOUND
    assert int(hp.SortDirection.ASC) == pb.SORT_DIRECTION_ASC
    assert int(hp.SortDirection.DESC) == pb.SORT_DIRECTION_DESC
    assert int(hp.SignificanceExtremum.HIGHEST) == pb.SIGNIFICANCE_EXTREMUM_HIGHEST
    assert int(hp.SignificanceExtremum.LOWEST) == pb.SIGNIFICANCE_EXTREMUM_LOWEST
    assert int(hp.PlacementMode.ABOVE) == pb.SignificancePlacement.ABOVE
    assert int(hp.PlacementMode.BELOW) == pb.SignificancePlacement.BELOW
    assert int(hp.PlacementMode.BETWEEN) == pb.SignificancePlacement.BETWEEN


def test_no_enum_exposes_the_unspecified_value():
    """UNSPECIFIED is how the wire says "not set", which in this client is None. Offering it as a
    value would give a caller two ways to say the same thing and one of them reads as a choice."""

    for enumeration in (
        hp.SearchMode,
        hp.LinkDirection,
        hp.SortDirection,
        hp.SignificanceExtremum,
        hp.PlacementMode,
    ):
        assert 0 not in [int(member) for member in enumeration]


def test_placement_helpers_set_their_mode():
    assert hp.Placement.above(anchor=5).mode == hp.PlacementMode.ABOVE
    assert hp.Placement.below(anchor=5).mode == hp.PlacementMode.BELOW

    between = hp.Placement.between(anchor=5, upper=6)

    assert between.mode == hp.PlacementMode.BETWEEN
    assert (between.anchor, between.upper) == (5, 6)


def test_a_page_is_iterable_and_reports_its_total():
    page = hp.Page(items=[1, 2], total=97)

    assert list(page) == [1, 2]
    assert len(page) == 2
    assert page.total == 97
    assert page


def test_an_empty_page_is_falsey():
    assert not hp.Page()
