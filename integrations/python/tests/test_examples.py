"""The runnable examples under examples/, exercised against the fake service (TODO-3 item 172).

An example that no longer matches the client is worse than none: it is the first thing a new user
copies. Running it here keeps it compiling against the contract on every push.
"""

from __future__ import annotations

import asyncio
import importlib.util
from pathlib import Path

from hippocampus import AsyncHippocampus
from hippocampus._proto import hippocampus_pb2 as pb

EXAMPLES = Path(__file__).resolve().parents[3] / "examples" / "python"


def load(name: str):
    spec = importlib.util.spec_from_file_location(name, EXAMPLES / f"{name}.py")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)

    return module


def test_the_agent_loop_runs_against_the_contract(server, service):
    agent_loop = load("agent_loop")

    service.responses["StoreEvent"] = pb.StoreEventResponse(id="conversation-1")
    service.responses["StoreMemory"] = pb.StoreMemoryResponse(id="memory-1")

    async def scenario():
        async with AsyncHippocampus(server, timeout=10) as client:
            return await agent_loop.converse(client, ["a statement.", "a question?"])

    stored = asyncio.run(scenario())

    assert stored == ["memory-1", "memory-1"]
    assert service.requests["SearchMemories"].reinforce is True
    assert service.requests["StoreMemory"].event_id == "conversation-1"
    assert service.requests["StoreMemory"].significance == 30
    assert "EndEvent" in service.calls
