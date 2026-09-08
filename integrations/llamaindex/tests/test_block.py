"""What the block does with messages, and what it asks the store for."""

from __future__ import annotations

import logging
from typing import List

import grpc
import pytest
from conftest import FakeClient
from hippocampus import BatchResult, BatchStored, Memory, SearchMode
from hippocampus.errors import FailedPrecondition, ServiceError
from llama_index.core.base.llms.types import ChatMessage, ImageBlock, MessageRole, TextBlock
from llama_index.memory.hippocampus import (
    STORE_MEMORIES_LIMIT,
    HippocampusMemoryBlock,
)


def message(role: str, text: str, **kwargs) -> ChatMessage:
    return ChatMessage(role=role, blocks=[TextBlock(text=text)], additional_kwargs=kwargs)


def block(client: FakeClient, **kwargs) -> HippocampusMemoryBlock:
    return HippocampusMemoryBlock(client=client, **kwargs)


# ------------------------------------------------------------------- writing


async def test_stores_one_memory_per_message(client: FakeClient) -> None:
    await block(client).aput([message("user", "we rolled back at 14:03")])

    assert len(client.batches) == 1

    stored = client.batches[0][0]

    assert stored.body == "we rolled back at 14:03"
    assert stored.significance == 50


async def test_a_role_absent_from_the_significance_map_is_not_stored(
    client: FakeClient,
) -> None:
    """The map is the selector as well as the ranking, so system and tool are skipped by default."""

    await block(client).aput(
        [
            message("system", "you are a helpful assistant"),
            message("tool", '{"status": "ok"}'),
            message("user", "keep this"),
        ]
    )

    bodies = [memory.body for memory in client.batches[0]]

    assert bodies == ["keep this"]


async def test_adding_a_role_starts_storing_it(client: FakeClient) -> None:
    await block(client, significance={"tool": 30}).aput(
        [message("user", "dropped now"), message("tool", "kept now")]
    )

    assert [memory.body for memory in client.batches[0]] == ["kept now"]
    assert client.batches[0][0].significance == 30


async def test_significance_fn_overrides_the_map_and_may_skip(client: FakeClient) -> None:
    def judge(chat_message: ChatMessage) -> int | None:
        return 90 if "decision" in chat_message.content else None

    await block(client, significance_fn=judge).aput(
        [message("user", "a decision was made"), message("user", "small talk")]
    )

    assert [memory.significance for memory in client.batches[0]] == [90]


async def test_the_role_goes_in_metadata_not_the_body(client: FakeClient) -> None:
    """Folding the role into the body would put its words into the content index of every memory."""

    await block(client).aput([message("assistant", "the rollback is complete")])

    stored = client.batches[0][0]

    assert stored.body == "the rollback is complete"
    assert stored.metadata["role"] == "assistant"


async def test_the_session_is_recorded_in_metadata(client: FakeClient) -> None:
    await block(client).aput([message("user", "hello")], session_id="s-1")

    assert client.batches[0][0].metadata["session_id"] == "s-1"


async def test_fixed_metadata_and_group_are_stamped(client: FakeClient) -> None:
    await block(client, group="support-bot", metadata={"app": "helpdesk"}).aput(
        [message("user", "hello")]
    )

    stored = client.batches[0][0]

    assert stored.group == "support-bot"
    assert stored.metadata["app"] == "helpdesk"


async def test_non_text_blocks_contribute_nothing(client: FakeClient) -> None:
    """A memory body is a proto3 string, so a message that is only an image is not a memory."""

    await block(client).aput(
        [ChatMessage(role=MessageRole.USER, blocks=[ImageBlock(url="http://example/x.png")])]
    )

    assert client.batches == []


async def test_an_empty_message_is_not_stored(client: FakeClient) -> None:
    await block(client).aput([message("user", "   ")])

    assert client.batches == []


async def test_a_batch_is_chunked_at_the_service_limit(client: FakeClient) -> None:
    messages = [message("user", f"memory {index}") for index in range(STORE_MEMORIES_LIMIT + 3)]

    await block(client).aput(messages)

    assert [len(batch) for batch in client.batches] == [STORE_MEMORIES_LIMIT, 3]


async def test_a_rejected_write_is_not_an_error(client: FakeClient, caplog) -> None:
    """Below-minimum-significance is the store declining, and the call succeeded."""

    client.batch_response = lambda batch: BatchStored(
        results=[BatchResult(rejected=True) for _ in batch],
        rejected=len(batch),
    )

    with caplog.at_level(logging.DEBUG):
        await block(client).aput([message("user", "barely worth saying")])

    assert not [record for record in caplog.records if record.levelno >= logging.WARNING]


async def test_a_failed_record_warns_rather_than_failing_the_turn(
    client: FakeClient, caplog
) -> None:
    client.batch_response = lambda batch: BatchStored(
        results=[BatchResult(code=3, error="body too long") for _ in batch],
        failed=len(batch),
    )

    with caplog.at_level(logging.WARNING):
        await block(client).aput([message("user", "x" * 10)])

    assert "body too long" in caplog.text


async def test_the_batch_write_falls_back_and_latches(client: FakeClient) -> None:
    """A service predating StoreMemories answers Unimplemented, and does not grow the RPC later."""

    client.batch_error = ServiceError("unknown method", grpc.StatusCode.UNIMPLEMENTED)

    memory_block = block(client)

    await memory_block.aput([message("user", "first")])

    assert [memory.body for memory in client.singles] == ["first"]
    assert len(client.batches) == 1

    await memory_block.aput([message("user", "second")])

    assert [memory.body for memory in client.singles] == ["first", "second"]
    assert len(client.batches) == 1, "the batch RPC should not be tried again"


async def test_any_other_write_failure_is_raised(client: FakeClient) -> None:
    client.batch_error = FailedPrecondition("nope", grpc.StatusCode.FAILED_PRECONDITION)

    with pytest.raises(FailedPrecondition):
        await block(client).aput([message("user", "hello")])


# ----------------------------------------------------------------- retrieval


async def test_retrieval_returns_the_memories_with_their_roles(client: FakeClient) -> None:
    client.results = [
        Memory(body="we rolled back at 14:03", metadata={"role": "user"}),
        Memory(body="the rollback completed", metadata={"role": "assistant"}),
    ]

    text = await block(client).aget([message("user", "what happened to the deploy?")])

    assert text == "user: we rolled back at 14:03\n\nassistant: the rollback completed"


async def test_retrieval_reinforces_by_default(client: FakeClient) -> None:
    """Recall is the write that resets the decay clock - it is the whole point of the pairing."""

    client.results = [Memory(body="something")]

    await block(client).aget([message("user", "anything")])

    assert client.searches[0]["reinforce"] is True


async def test_reinforcement_can_be_turned_off(client: FakeClient) -> None:
    client.results = [Memory(body="something")]

    await block(client, reinforce=False).aget([message("user", "anything")])

    assert client.searches[0]["reinforce"] is False


async def test_the_query_is_the_trailing_window(client: FakeClient) -> None:
    messages = [message("user", f"turn {index}") for index in range(6)]

    await block(client, retrieval_context_window=2).aget(messages)

    assert client.searches[0]["query"] == "turn 4 turn 5"


async def test_no_messages_and_no_text_retrieve_nothing(client: FakeClient) -> None:
    memory_block = block(client)

    assert await memory_block.aget([]) == ""
    assert await memory_block.aget(None) == ""
    assert await memory_block.aget([message("user", "  ")]) == ""
    assert client.searches == []


async def test_no_results_render_as_nothing(client: FakeClient) -> None:
    assert await block(client).aget([message("user", "anything")]) == ""


async def test_retrieval_spans_sessions_by_default(client: FakeClient) -> None:
    """Remembering across conversations is what a long-term block is for."""

    client.results = [Memory(body="something")]

    await block(client).aget([message("user", "anything")], session_id="s-1")

    assert client.searches[0]["metadata"] is None


async def test_scope_to_session_restricts_retrieval(client: FakeClient) -> None:
    client.results = [Memory(body="something")]

    await block(client, scope_to_session=True).aget(
        [message("user", "anything")], session_id="s-1"
    )

    assert client.searches[0]["metadata"] == {"session_id": "s-1"}


async def test_fixed_metadata_filters_reads(client: FakeClient) -> None:
    """What this block reads back is what this block wrote."""

    client.results = [Memory(body="something")]

    await block(client, metadata={"app": "helpdesk"}).aget([message("user", "anything")])

    assert client.searches[0]["metadata"] == {"app": "helpdesk"}


async def test_a_custom_format_template_wraps_the_retrieved_text(client: FakeClient) -> None:
    client.results = [Memory(body="a fact")]

    text = await block(
        client, role_metadata_key="", format_template="Recalled:\n{text}"
    ).aget([message("user", "anything")])

    assert text == "Recalled:\na fact"


# ------------------------------------------------------------ mode resolution


async def test_the_mode_is_the_richest_the_deployment_serves(client: FakeClient) -> None:
    client.search_modes = [SearchMode.KEYWORD, SearchMode.SEMANTIC, SearchMode.HYBRID]
    client.results = [Memory(body="something")]

    await block(client).aget([message("user", "anything")])

    assert client.searches[0]["mode"] is SearchMode.HYBRID


async def test_a_keyword_only_deployment_gets_keyword(client: FakeClient) -> None:
    client.results = [Memory(body="something")]

    await block(client).aget([message("user", "anything")])

    assert client.searches[0]["mode"] is SearchMode.KEYWORD


async def test_the_mode_is_resolved_once(client: FakeClient) -> None:
    """It is a property of the deployment, and who_am_i costs a round trip per retrieval."""

    client.results = [Memory(body="something")]

    memory_block = block(client)

    await memory_block.aget([message("user", "one")])
    await memory_block.aget([message("user", "two")])

    assert client.who_am_i_calls == 1


async def test_an_unservable_mode_raises_once_naming_what_is_served(
    client: FakeClient,
) -> None:
    client.search_modes = [SearchMode.KEYWORD]

    with pytest.raises(ValueError) as raised:
        await block(client, mode=SearchMode.SEMANTIC).aget([message("user", "anything")])

    assert "SEMANTIC" in str(raised.value)
    assert "KEYWORD" in str(raised.value)


async def test_a_deployment_with_no_search_says_so(client: FakeClient) -> None:
    client.search_modes = []

    with pytest.raises(ValueError) as raised:
        await block(client).aget([message("user", "anything")])

    assert "search_modes" in str(raised.value)


# ----------------------------------------------------------------- truncation


async def test_truncation_drops_the_least_relevant_first(client: FakeClient) -> None:
    """Results arrive ranked, so the end costs least - the base class discards the whole block."""

    memory_block = block(client)
    content = memory_block.separator.join(["first" * 4, "second" * 4, "third" * 4])

    truncated = await memory_block.atruncate(content, tokens_to_truncate=5)

    assert truncated == memory_block.separator.join(["first" * 4, "second" * 4])


async def test_truncating_everything_returns_none(client: FakeClient) -> None:
    memory_block = block(client)

    assert await memory_block.atruncate("a\n\nb", tokens_to_truncate=1000) is None
    assert await memory_block.atruncate("", tokens_to_truncate=1) is None
