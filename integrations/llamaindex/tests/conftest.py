"""A fake store, so the whole suite runs with no service and no network.

The fake returns the client package's own result types rather than stand-ins, because half of what
this adapter does is read those types correctly - a rejected write is a falsey `Stored`, a batch
reports per record, and a `Page` is iterable. Substituting a mock for them would test the mock.
"""

from __future__ import annotations

from typing import Any, Dict, Iterable, List, Optional

import pytest
from hippocampus import BatchResult, BatchStored, Identity, Memory, Page, SearchMode, Stored


class FakeClient:
    """Records what it was asked and answers with whatever the test set up."""

    def __init__(
        self,
        *,
        search_modes: Optional[List[SearchMode]] = None,
        results: Optional[List[Memory]] = None,
    ) -> None:
        self.search_modes = (
            [SearchMode.KEYWORD] if search_modes is None else list(search_modes)
        )
        self.results = list(results or [])

        # What was asked, in order.
        self.batches: List[List[Memory]] = []
        self.singles: List[Memory] = []
        self.searches: List[Dict[str, Any]] = []
        self.who_am_i_calls = 0

        # What to answer with. A callable takes the memories and returns the response.
        self.batch_response: Any = None
        self.batch_error: Optional[BaseException] = None
        self.rejects: bool = False

    def who_am_i(self, **_: Any) -> Identity:
        self.who_am_i_calls += 1

        return Identity(search_modes=list(self.search_modes))

    def store_memory(self, memory: Any = None, significance: int = 0, **_: Any) -> Stored:
        self.singles.append(memory)

        if self.rejects:
            return Stored(rejected=True)

        return Stored(id=f"single-{len(self.singles)}")

    def store_memories(self, memories: Iterable[Memory], **_: Any) -> BatchStored:
        batch = list(memories)
        self.batches.append(batch)

        if self.batch_error is not None:
            error, self.batch_error = self.batch_error, None

            raise error

        if self.batch_response is not None:
            return self.batch_response(batch)

        results = [BatchResult(id=f"m{index}") for index in range(len(batch))]

        return BatchStored(results=results, stored=len(results))

    def search_memories(self, query: str, **kwargs: Any) -> Page:
        self.searches.append({"query": query, **kwargs})

        return Page(items=list(self.results), total=len(self.results))


@pytest.fixture
def client() -> FakeClient:
    return FakeClient()
