"""The narrow view of a store this package is allowed to hold.

`Hippocampus` exposes every RPC the contract declares, including `purge`, `sleep`, `clear` and the
whole transfer surface. This protocol is the statement of which four of them a memory block may
reach for, and it is the only thing standing between an agent's memory and the destructive half of
that surface - the same line the event-source bridges draw with their own client interface.

It is `runtime_checkable` so that a real `Hippocampus` satisfies it structurally, with no
registration and no subclassing, and so that a test - or an application wrapping the client for its
own metrics, retries or rate limiting - can pass something else that answers the same four calls.
"""

from __future__ import annotations

from typing import Any, Iterable, Mapping, Optional, Protocol, runtime_checkable


@runtime_checkable
class MemoryClient(Protocol):
    """What `HippocampusMemoryBlock` calls. `hippocampus.Hippocampus` satisfies it as it is."""

    def who_am_i(self, **kwargs: Any) -> Any:
        """Report what this deployment can serve. Used to resolve the search mode once."""

    def store_memory(self, memory: Any = None, significance: int = 0, **kwargs: Any) -> Any:
        """Store one memory. Only reached when the service has no batch write."""

    def store_memories(self, memories: Iterable[Any], **kwargs: Any) -> Any:
        """Store a batch of memories, each with its own result."""

    def search_memories(
        self,
        query: str,
        *,
        limit: int = 0,
        mode: Optional[Any] = None,
        group: Optional[str] = None,
        metadata: Optional[Mapping[str, str]] = None,
        reinforce: bool = False,
        **kwargs: Any,
    ) -> Any:
        """Search memory content, optionally recalling what is returned."""
