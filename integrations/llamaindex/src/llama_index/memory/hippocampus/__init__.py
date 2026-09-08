"""Hippocampus as a LlamaIndex long-term memory block.

    from hippocampus import Hippocampus
    from llama_index.memory.hippocampus import hippocampus_memory

    client = Hippocampus("localhost:50051")
    memory = hippocampus_memory(client, session_id="support-session-1", group="support-bot")

    agent = FunctionAgent(llm=llm, tools=tools)
    response = await agent.run("what did we decide about the rollback?", memory=memory)

What the pairing is for: the block writes each turn to the store and retrieves by relevance, and
retrieval **recalls** what it returns - so a fact the agent keeps using is reinforced and one it
never reaches for decays out of the store on its own. There is no eviction policy in this package
because there does not need to be one.

Every call into the service is blocking gRPC run through `asyncio.to_thread`: the published client
is synchronous, and doing that in one place here is better than a second, async client to keep in
step with the contract.
"""

from llama_index.memory.hippocampus._version import __version__
from llama_index.memory.hippocampus.block import (
    DEFAULT_SIGNIFICANCE,
    STORE_MEMORIES_LIMIT,
    HippocampusMemoryBlock,
)
from llama_index.memory.hippocampus.client import MemoryClient
from llama_index.memory.hippocampus.memory import hippocampus_memory

__all__ = [
    "__version__",
    "DEFAULT_SIGNIFICANCE",
    "STORE_MEMORIES_LIMIT",
    "HippocampusMemoryBlock",
    "MemoryClient",
    "hippocampus_memory",
]
