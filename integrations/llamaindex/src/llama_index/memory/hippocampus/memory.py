"""The one-call form: a `Memory` with a Hippocampus block already installed."""

from __future__ import annotations

from typing import Any, Optional

from hippocampus import Hippocampus
from llama_index.core.memory import Memory

from llama_index.memory.hippocampus.block import HippocampusMemoryBlock


def hippocampus_memory(
    client: Hippocampus,
    *,
    session_id: Optional[str] = None,
    token_limit: Optional[int] = None,
    **block_kwargs: Any,
) -> Memory:
    """Build a `Memory` whose long-term half is a Hippocampus store.

        memory = hippocampus_memory(client, session_id="support-session-1", group="support-bot")

    Keyword arguments other than the two above are passed to `HippocampusMemoryBlock`. Only
    `session_id` and `token_limit` are lifted out of `Memory.from_defaults`, because those are the
    two a caller reaches for when the rest of the short-term buffer is left at its defaults;
    anything further wants `Memory.from_defaults(memory_blocks=[HippocampusMemoryBlock(...)])`
    directly, which is all this function is.
    """

    block = HippocampusMemoryBlock(client=client, **block_kwargs)

    memory_kwargs: dict = {"memory_blocks": [block]}

    if session_id is not None:
        memory_kwargs["session_id"] = session_id

    if token_limit is not None:
        memory_kwargs["token_limit"] = token_limit

        # `token_flush_size` defaults to 10% of the DEFAULT token limit, not of the one given here,
        # and `Memory` requires it to stay under `token_limit * chat_history_token_ratio`. So a
        # caller lowering the limit - which is the ordinary thing to do, and what makes the
        # waterfall reach this block sooner - is rejected for a field it never passed, and the
        # message names neither the field it set nor the fix. Deriving it at upstream's own ratio
        # is what a one-line helper owes: the same behaviour across the supported range, rather
        # than a validation error on part of it.
        memory_kwargs["token_flush_size"] = max(1, int(token_limit * 0.1))

    return Memory.from_defaults(**memory_kwargs)
