"""A LlamaIndex long-term memory block backed by Hippocampus.

The block is a thin adapter over the published `hippocampus-client` package. Retrieval is a
content search against the store; writing is a batch `StoreMemories` call. Nothing here decides
what to keep, because that is the service's job - which is the whole reason this pairing works:

- **Retrieval reinforces.** A search here recalls the memories it returns, which resets their decay
  clock and raises their effective significance. A fact the agent actually uses therefore survives,
  and one it never retrieves decays out on its own. That is the loop; `reinforce=False` turns it
  off and leaves a store that forgets the things it is being used for.
- **The block never deletes.** There is no eviction policy, no cap and no TTL in this file. A
  consolidation cycle on the service removes what has stopped mattering, so a memory this block
  wrote can stop existing at any time, and that is expected rather than an error to handle.
- **Insignificance is not a failure.** A message below the deployment's minimum significance is
  quietly dropped by the store: the write succeeds with an empty id. It is logged at debug and
  counted, never raised.

The reason a *long-term memory block* is the right slot for a forgetting store, and a conversation
transcript is not: facts retrieved by relevance and folded into a prompt are supposed to thin out,
and nothing is corrupted when a low-value one goes. A transcript has a structural invariant a
forgetting store breaks - a tool call needs its matching tool result, and the two are separate
memories with separate significance.
"""

from __future__ import annotations

import asyncio
import logging
from typing import Any, Callable, Dict, List, Optional

import grpc
from hippocampus import Memory as MemoryRecord
from hippocampus import SearchMode
from hippocampus.errors import HippocampusError
from llama_index.core.base.llms.types import ChatMessage, TextBlock
from llama_index.core.bridge.pydantic import Field, PrivateAttr, field_validator
from llama_index.core.memory import BaseMemoryBlock
from llama_index.core.prompts import (
    BasePromptTemplate,
    PromptTemplate,
    RichPromptTemplate,
)

from llama_index.memory.hippocampus.client import MemoryClient

logger = logging.getLogger(__name__)

# The role map is the selector as well as the ranking: a role with no entry here is not stored at
# all. One knob rather than two, so there is no way to configure a role that is written and then
# ranked by a default nobody chose. System messages are absent deliberately - a system prompt is
# configuration that is resent on every call, not something the agent learned.
#
# The numbers are relative and mean nothing on their own. What a user said is ranked above the
# assistant's restatement of it because the restatement is recoverable and the original is not.
# A deployment's own scale is what matters, so `significance_levels()` is worth reading before
# changing them.
DEFAULT_SIGNIFICANCE: Dict[str, int] = {
    "user": 50,
    "assistant": 40,
}

DEFAULT_SEPARATOR = "\n\n"

DEFAULT_RETRIEVED_TEXT_TEMPLATE = RichPromptTemplate("{{ text }}")

# StoreMemories' documented cap. Chunking here rather than letting the service refuse means a
# caller flushing a large backlog through `aput` never has to know the number.
STORE_MEMORIES_LIMIT = 500

# Richest first. `mode=None` resolves to the best thing the deployment actually serves, so an
# instance that gains an embedding model starts doing hybrid retrieval without a code change, and
# one that has none is not asked for a mode it would refuse.
_MODE_PREFERENCE = (SearchMode.HYBRID, SearchMode.SEMANTIC, SearchMode.KEYWORD)

# Only ever used to decide how many retrieved memories to drop in `atruncate`. The caller measures
# the real saving with its own tokenizer afterwards and loops, so an estimate is enough here and a
# tokenizer dependency of our own would buy nothing.
_CHARS_PER_TOKEN = 4


class HippocampusMemoryBlock(BaseMemoryBlock[str]):
    """Long-term memory over a Hippocampus store.

        from hippocampus import Hippocampus
        from llama_index.core.memory import Memory
        from llama_index.memory.hippocampus import HippocampusMemoryBlock

        client = Hippocampus("localhost:50051")

        memory = Memory.from_defaults(
            session_id="support-session-1",
            memory_blocks=[HippocampusMemoryBlock(client=client, group="support-bot")],
        )

    The client is passed in already connected rather than built from an address here, so that this
    class does not restate the ten connection parameters (token, TLS, private CA, client
    certificate, deadlines) that `Hippocampus` already documents - a second copy of them is a
    second thing to keep current. The block does not own the channel and never closes it.
    """

    name: str = Field(
        default="HippocampusMemory",
        description="The name of the memory block.",
    )
    description: Optional[str] = Field(
        default="Memories recalled from long-term storage, most relevant first.",
        description="A description of the memory block.",
    )
    client: MemoryClient = Field(
        description="A connected hippocampus.Hippocampus client. Not owned or closed by the block."
    )
    group: Optional[str] = Field(
        default=None,
        description=(
            "The store's group label stamped on every memory written, and the one reads are "
            "restricted to. Under a group-scoped token the service stamps its own and this is "
            "better left unset."
        ),
    )
    metadata: Dict[str, str] = Field(
        default_factory=dict,
        description=(
            "Fixed metadata stamped on every memory written, and filtered on when reading - so "
            "what this block reads back is what this block wrote."
        ),
    )
    session_metadata_key: str = Field(
        default="session_id",
        description=(
            "Metadata key recording which conversation a memory came from. Empty disables the "
            "recording. Retrieval does not filter on it unless scope_to_session is set."
        ),
    )
    scope_to_session: bool = Field(
        default=False,
        description=(
            "Restrict retrieval to the current session. Off by default: remembering across "
            "conversations is what a long-term block is for."
        ),
    )
    role_metadata_key: str = Field(
        default="role",
        description=(
            "Metadata key recording which role produced a memory. Empty disables it, which also "
            "removes the role prefix from retrieved text."
        ),
    )
    significance: Dict[str, int] = Field(
        default_factory=lambda: dict(DEFAULT_SIGNIFICANCE),
        description=(
            "Role to significance. A role absent from this map is not stored, so adding "
            "\"tool\": 30 is what starts storing tool output."
        ),
    )
    significance_fn: Optional[Callable[[ChatMessage], Optional[int]]] = Field(
        default=None,
        description=(
            "Per-message significance, consulted before the role map. Returning None skips the "
            "message entirely, which is how a caller stores only what it judges worth keeping."
        ),
    )
    limit: int = Field(
        default=5,
        description="How many memories a retrieval returns.",
    )
    retrieval_context_window: int = Field(
        default=5,
        description="How many trailing messages are joined into the retrieval query.",
    )
    mode: Optional[SearchMode] = Field(
        default=None,
        description=(
            "Search mode. None resolves once, at first retrieval, to the richest mode the "
            "deployment reports it can serve."
        ),
    )
    reinforce: bool = Field(
        default=True,
        description=(
            "Whether a retrieval recalls what it returns, resetting its decay clock. Turning this "
            "off leaves a store that forgets the memories the agent is actually using."
        ),
    )
    format_template: BasePromptTemplate = Field(
        default=DEFAULT_RETRIEVED_TEXT_TEMPLATE,
        description="Template the joined retrieved text is rendered through.",
    )
    separator: str = Field(
        default=DEFAULT_SEPARATOR,
        description="Separator between retrieved memories. Also what atruncate splits on.",
    )

    # Resolved once from who_am_i(), because it is a property of the deployment rather than of a
    # call. Two concurrent first retrievals may each resolve it; they reach the same answer, so
    # the race is not worth a lock.
    _resolved_mode: Optional[SearchMode] = PrivateAttr(default=None)

    # A service predating StoreMemories answers Unimplemented, and a service does not grow an RPC
    # while it is running - so the fallback latches rather than paying a failed call per batch.
    _batch_unsupported: bool = PrivateAttr(default=False)

    @field_validator("format_template", mode="before")
    @classmethod
    def _validate_format_template(cls, value: Any) -> Any:
        """Accept a bare string, the way VectorMemoryBlock does."""

        if isinstance(value, str):
            if "{{" in value and "}}" in value:
                return RichPromptTemplate(value)

            return PromptTemplate(value)

        return value

    # ------------------------------------------------------------------ writing

    async def _aput(self, messages: List[ChatMessage]) -> None:
        """Store messages as memories, one memory per message."""

        records = [
            record for record in (self._record(message) for message in messages) if record
        ]

        if not records:
            return

        for start in range(0, len(records), STORE_MEMORIES_LIMIT):
            await self._store(records[start : start + STORE_MEMORIES_LIMIT])

    def _record(self, message: ChatMessage) -> Optional[MemoryRecord]:
        """Turn one chat message into a memory, or None if it is not one worth storing."""

        body = _text_of(message)

        if not body:
            return None

        role = str(message.role.value)
        significance = self._significance_of(message, role)

        if significance is None:
            return None

        metadata = dict(self.metadata)

        # The role goes in metadata rather than into the body. A body is what the content index
        # tokenises, so wrapping it in "<message role='user'>" would put `message`, `role` and
        # `user` into the index for every memory - three terms that match everything and rank
        # nothing. Metadata is filterable and unindexed, which is exactly what a role is for.
        if self.role_metadata_key:
            metadata[self.role_metadata_key] = role

        session_id = message.additional_kwargs.get("session_id")

        if self.session_metadata_key and session_id:
            metadata[self.session_metadata_key] = str(session_id)

        return MemoryRecord(
            body=body,
            significance=significance,
            group=self.group,
            metadata=metadata,
        )

    def _significance_of(self, message: ChatMessage, role: str) -> Optional[int]:
        """The significance to store a message at, or None to skip it."""

        if self.significance_fn is not None:
            return self.significance_fn(message)

        return self.significance.get(role)

    async def _store(self, records: List[MemoryRecord]) -> None:
        """Write one chunk, reporting per-record outcomes without failing the caller's turn."""

        if self._batch_unsupported:
            await self._store_individually(records)

            return

        try:
            batch = await asyncio.to_thread(self.client.store_memories, records)
        except HippocampusError as error:
            if error.code is not grpc.StatusCode.UNIMPLEMENTED:
                raise

            logger.info(
                "%s: this service predates StoreMemories - storing one memory per call from here on",
                self.name,
            )
            self._batch_unsupported = True

            await self._store_individually(records)

            return

        if batch.rejected:
            logger.debug(
                "%s: the store declined %d of %d memories as insignificant",
                self.name,
                batch.rejected,
                len(records),
            )

        for index, result in enumerate(batch):
            if result.failed:
                logger.warning(
                    "%s: memory %d of %d was not stored: %s",
                    self.name,
                    index + 1,
                    len(records),
                    result.error,
                )

    async def _store_individually(self, records: List[MemoryRecord]) -> None:
        """The fallback for a service with no batch write."""

        for record in records:
            stored = await asyncio.to_thread(self.client.store_memory, record)

            if not stored:
                logger.debug("%s: the store declined a memory as insignificant", self.name)

    # ---------------------------------------------------------------- retrieval

    async def _aget(
        self,
        messages: Optional[List[ChatMessage]] = None,
        session_id: Optional[str] = None,
        **block_kwargs: Any,
    ) -> str:
        """Retrieve the memories most relevant to the conversation so far, reinforcing them."""

        if not messages:
            return ""

        query = self._query_text(messages)

        if not query:
            return ""

        mode = await self._mode_for_deployment()

        page = await asyncio.to_thread(
            self.client.search_memories,
            query,
            limit=self.limit,
            mode=mode,
            group=self.group,
            metadata=self._read_filter(session_id) or None,
            reinforce=self.reinforce,
        )

        if not page:
            return ""

        text = self.separator.join(self._render(memory) for memory in page)

        return self.format_template.format(text=text)

    def _query_text(self, messages: List[ChatMessage]) -> str:
        """The query, built from the trailing messages rather than only the last one."""

        window = self.retrieval_context_window

        if window > 1 and len(messages) > window:
            messages = messages[-window:]

        return " ".join(text for text in (_text_of(message) for message in messages) if text)

    def _read_filter(self, session_id: Optional[str]) -> Dict[str, str]:
        """The metadata a retrieval restricts itself to."""

        metadata = dict(self.metadata)

        if self.scope_to_session and self.session_metadata_key and session_id:
            metadata[self.session_metadata_key] = str(session_id)

        return metadata

    def _render(self, memory: MemoryRecord) -> str:
        """One retrieved memory as prompt text, with its role restored from metadata."""

        role = memory.metadata.get(self.role_metadata_key) if self.role_metadata_key else None

        if role:
            return f"{role}: {memory.body}"

        return memory.body

    async def _mode_for_deployment(self) -> SearchMode:
        """Resolve the search mode once against what this deployment reports it can serve.

        Asking rather than trying is the point: a mode with no backend is refused per call with
        FAILED_PRECONDITION, so a block configured for one would fail on every retrieval for the
        life of the process while looking like a retrieval bug.
        """

        if self._resolved_mode is not None:
            return self._resolved_mode

        identity = await asyncio.to_thread(self.client.who_am_i)
        available = list(identity.search_modes)

        if not available:
            raise ValueError(
                f"{self.name}: this Hippocampus deployment serves no content search, so a memory "
                "block cannot retrieve from it - who_am_i() reports an empty search_modes. "
                "Configure a content-search backend on the service and try again."
            )

        if self.mode is not None:
            if self.mode not in available:
                served = ", ".join(mode.name for mode in available)

                raise ValueError(
                    f"{self.name}: this deployment cannot serve SearchMode.{self.mode.name}; "
                    f"it serves {served}. Leave mode unset to take the richest one it has."
                )

            self._resolved_mode = self.mode

            return self._resolved_mode

        for mode in _MODE_PREFERENCE:
            if mode in available:
                self._resolved_mode = mode

                return mode

        # Unreachable while SearchMode and _MODE_PREFERENCE agree, but a deployment reporting a
        # mode this package has never heard of should not fall through to None.
        self._resolved_mode = available[0]

        return self._resolved_mode

    # -------------------------------------------------------------- truncation

    async def atruncate(self, content: str, tokens_to_truncate: int) -> Optional[str]:
        """Drop retrieved memories from the end until roughly enough tokens have gone.

        The base class's behaviour is to discard the block wholesale, which throws away the most
        relevant memory to save the least relevant one's tokens. Results arrive ranked, so dropping
        from the end costs the least. The caller measures the real saving with its own tokenizer
        and loops, so the estimate here only has to be close.
        """

        if not content:
            return None

        chunks = content.split(self.separator)

        while chunks and tokens_to_truncate > 0:
            tokens_to_truncate -= max(1, len(chunks.pop()) // _CHARS_PER_TOKEN)

        if not chunks:
            return None

        return self.separator.join(chunks)


def _text_of(message: ChatMessage) -> str:
    """The text of a message, ignoring image, audio and document blocks.

    A memory body is a proto3 string, so only text can be stored; a message that is nothing but an
    image contributes no memory rather than an empty one.
    """

    return "".join(
        block.text for block in message.blocks if isinstance(block, TextBlock)
    ).strip()
