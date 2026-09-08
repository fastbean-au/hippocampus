"""The one-call form, and how a composed `Memory` actually feeds the block.

The second half is the part worth pinning: `Memory` does not hand every turn to its blocks. A turn
goes into the short-term buffer, and only reaches a memory block when the buffer overflows its
token limit and waterfalls the oldest messages out. That is the right shape for a long-term store -
the store receives what the conversation has moved past - but it means a short conversation writes
nothing, which looks like a broken adapter until you know it.
"""

from __future__ import annotations

from conftest import FakeClient
from hippocampus import Memory as MemoryRecord
from llama_index.core.base.llms.types import ChatMessage, TextBlock
from llama_index.memory.hippocampus import HippocampusMemoryBlock, hippocampus_memory


def turn(role: str, index: int) -> ChatMessage:
    return ChatMessage(
        role=role, blocks=[TextBlock(text=f"{role} {index} with enough words to burn some tokens")]
    )


def test_the_factory_installs_one_block(client: FakeClient) -> None:
    memory = hippocampus_memory(client, session_id="s-1", group="support-bot")

    blocks = [
        block for block in memory.memory_blocks if isinstance(block, HippocampusMemoryBlock)
    ]

    assert len(blocks) == 1
    assert blocks[0].group == "support-bot"
    assert memory.session_id == "s-1"


def test_a_lowered_token_limit_carries_its_flush_size_down(client: FakeClient) -> None:
    """`Memory` rejects a flush size that is not comfortably under the limit, and the default one
    is 10% of the DEFAULT limit rather than of the one given - so lowering the limit alone is
    refused for a field the caller never passed."""

    memory = hippocampus_memory(client, token_limit=1234)

    assert memory.token_limit == 1234
    assert memory.token_flush_size == 123


async def test_a_short_conversation_writes_nothing(client: FakeClient) -> None:
    """Nothing has left the short-term buffer yet, so the store has not been asked to hold it."""

    memory = hippocampus_memory(client, session_id="s-1", token_limit=4000)

    await memory.aput(turn("user", 0))
    await memory.aput(turn("assistant", 0))

    assert client.batches == []


async def test_the_overflow_reaches_the_store_with_its_session(client: FakeClient) -> None:
    memory = hippocampus_memory(client, session_id="s-1", token_limit=40)

    for index in range(4):
        await memory.aput(turn("user", index))
        await memory.aput(turn("assistant", index))

    written = [record for batch in client.batches for record in batch]

    assert [record.body for record in written][:2] == [
        "user 0 with enough words to burn some tokens",
        "assistant 0 with enough words to burn some tokens",
    ]
    assert {record.metadata["session_id"] for record in written} == {"s-1"}


async def test_a_block_refusing_short_term_overflow_receives_nothing(
    client: FakeClient,
) -> None:
    """`accept_short_term_memory=False` leaves only direct `block.aput` as a way in."""

    memory = hippocampus_memory(
        client, session_id="s-1", token_limit=40, accept_short_term_memory=False
    )

    for index in range(4):
        await memory.aput(turn("user", index))
        await memory.aput(turn("assistant", index))

    assert client.batches == []


async def test_retrieval_comes_back_through_the_composed_memory(client: FakeClient) -> None:
    client.results = [MemoryRecord(body="we rolled back at 14:03", metadata={"role": "user"})]

    memory = hippocampus_memory(client, session_id="s-1")

    await memory.aput(turn("user", 0))

    rendered = "\n".join(str(message) for message in await memory.aget())

    assert "we rolled back at 14:03" in rendered
