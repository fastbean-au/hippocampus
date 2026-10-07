"""The asyncio client (TODO-3 item 172): the same surface as Hippocampus, over grpc.aio.

The agent frameworks this package is for are asyncio-heavy, and a blocking client there either stalls
the event loop or needs a thread per call. These drive the async client over a real channel against
the same recording fake the synchronous tests use, with asyncio.run rather than a test plugin so the
package's dev dependencies are unchanged.
"""

from __future__ import annotations

import asyncio
import datetime
import inspect

import grpc
import pytest

import hippocampus as hp
from hippocampus import AsyncHippocampus, Hippocampus
from hippocampus._proto import hippocampus_pb2 as pb


def run(coroutine):
    return asyncio.run(coroutine)


def test_the_two_clients_offer_the_same_methods():
    """One surface for both: a method on one and not the other is a drift this design rules out."""

    public = lambda cls: {name for name in dir(cls) if not name.startswith("_")}

    assert public(Hippocampus) - {"close"} == public(AsyncHippocampus) - {"close"}


def test_every_rpc_wrapper_is_awaitable(server):
    async def scenario():
        async with AsyncHippocampus(server, timeout=10) as client:
            call = client.who_am_i()

            assert inspect.isawaitable(call)

            return await call

    assert isinstance(run(scenario()), hp.Identity)


def test_a_store_and_a_listing_convert_as_the_sync_client_does(server, service):
    when = datetime.datetime(2026, 8, 12, tzinfo=datetime.timezone.utc)
    service.responses["StoreMemory"] = pb.StoreMemoryResponse(id="m-1")

    async def scenario():
        async with AsyncHippocampus(server, timeout=10) as client:
            stored = await client.store_memory("hello", 50, timestamp=when)
            page = await client.get_memories(group="g")

            return stored, page

    stored, page = run(scenario())

    assert stored and stored.id == "m-1"
    assert isinstance(page, hp.Page)
    assert service.requests["StoreMemory"].time_stamp == int(when.timestamp() * 1_000_000_000)
    assert service.requests["GetMemories"].group == "g"


def test_the_token_and_version_ride_on_every_call(server, service):
    async def scenario():
        async with AsyncHippocampus(server, token="s3cret", timeout=10) as client:
            await client.who_am_i()

    run(scenario())

    metadata = service.metadata["WhoAmI"]

    assert metadata["authorization"] == "Bearer s3cret"
    assert metadata["hippocampus-client-version"].startswith("hippocampus-client-python/")


def test_a_failure_raises_ours_not_grpcs(server, service):
    service.fail_with = (grpc.StatusCode.NOT_FOUND, "no such memory")

    async def scenario():
        async with AsyncHippocampus(server, timeout=10) as client:
            await client.recall_memories(["m-1"])

    with pytest.raises(hp.NotFound):
        run(scenario())


def test_connect_async_is_the_constructor(server):
    async def scenario():
        client = hp.connect_async(server, timeout=10)

        try:
            assert isinstance(client, AsyncHippocampus)
        finally:
            await client.close()

    run(scenario())
