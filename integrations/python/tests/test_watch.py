"""Watch, the change feed (TODO-3 item 193): a server-streaming RPC, so unlike every other method
it returns an iterator rather than a value."""

from __future__ import annotations

import asyncio

import grpc
import pytest

from hippocampus import AsyncHippocampus, errors
from hippocampus._proto import hippocampus_pb2 as pb

EVENTS = [
    pb.WatchEvent(kind=pb.CALLBACK_KIND_MEMORY_STORED, items=[pb.WatchItem(id="m1", group="g")]),
    pb.WatchEvent(kind=pb.CALLBACK_KIND_MEMORY_FORGOTTEN, items=[pb.WatchItem(id="m1")], dropped=2),
]


def test_watch_yields_each_event_and_sends_the_kinds(client, service):
    service.responses["Watch"] = EVENTS

    events = list(client.watch(kinds=[pb.CALLBACK_KIND_MEMORY_STORED, pb.CALLBACK_KIND_MEMORY_FORGOTTEN]))

    assert [event.items[0].id for event in events] == ["m1", "m1"]
    assert events[1].dropped == 2
    assert list(service.requests["Watch"].kinds) == [
        pb.CALLBACK_KIND_MEMORY_STORED,
        pb.CALLBACK_KIND_MEMORY_FORGOTTEN,
    ]
    assert dict(service.metadata["Watch"]).get("hippocampus-client-version") is not None


def test_watch_translates_a_refusal(client, service):
    service.fail_with = (grpc.StatusCode.INVALID_ARGUMENT, "Watch does not carry MEMORIES_AT_RISK")

    with pytest.raises(errors.InvalidArgument):
        list(client.watch(kinds=[pb.CALLBACK_KIND_MEMORIES_AT_RISK]))


def test_async_watch_yields_each_event(server, service):
    service.responses["Watch"] = EVENTS

    async def scenario():
        async with AsyncHippocampus(server, timeout=10) as client:
            return [event async for event in client.watch()]

    events = asyncio.run(scenario())

    assert [event.kind for event in events] == [pb.CALLBACK_KIND_MEMORY_STORED, pb.CALLBACK_KIND_MEMORY_FORGOTTEN]
