"""A conversational agent's memory loop, over the asyncio client (TODO-3 item 172).

Each turn does the three things an agent does with a memory that forgets:

1. it RECALLS what is relevant - a reinforcing search, so what keeps proving useful is what keeps
   surviving the decay;
2. it would hand those memories to its model (here, it prints them);
3. it STORES the turn itself, under the conversation's event, so the conversation is one unit the
   store can later summarise or forget as a whole.

    python examples/python/agent_loop.py --address localhost:50051

It needs the client (`pip install -e integrations/python`, until the package is on PyPI). The turns
are canned so the program runs unattended; replace `TURNS` with your agent's input. The package's
test suite runs `converse` against its fake service, which is what keeps this example compiling
against the contract.
"""

from __future__ import annotations

import argparse
import asyncio
import os
import uuid
from typing import Iterable, List

from hippocampus import AsyncHippocampus

TURNS = [
    "The billing deploy at 14:03 rolled back cleanly after the health check failed.",
    "What happened with the billing deploy?",
    "Remind me to add a canary stage before the next billing deploy.",
]


async def converse(client: AsyncHippocampus, turns: Iterable[str], group: str = "agent") -> List[str]:
    """Run the turns as one conversation, returning the memory ids it stored."""

    conversation = await client.store_event(
        f"conversation {uuid.uuid4().hex[:8]}",
        significance=40,
        group=group,
    )

    stored: List[str] = []

    for turn in turns:
        # Reinforcing: the memories this returns have their decay clocks reset, because the agent is
        # about to use them. That is the whole economy of the store - use keeps a memory alive.
        relevant = await client.search_memories(turn, limit=3, group=group, reinforce=True)

        for memory in relevant.items:
            print(f"  recalled: {memory.body}")

        # A turn the agent stated is worth more than one it was asked, which is the kind of judgement
        # significance is for.
        significance = 30 if turn.endswith("?") else 60

        result = await client.store_memory(turn, significance, event_id=conversation.id, group=group)

        if result:
            stored.append(result.id)

        print(f"stored ({significance}): {turn}")

    await client.end_event(conversation.id)

    return stored


async def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--address", default="localhost:50051")
    parser.add_argument("--token", default=os.environ.get("HIPPOCAMPUS_TOKEN"))
    args = parser.parse_args()

    async with AsyncHippocampus(args.address, token=args.token) as client:
        await converse(client, TURNS)


if __name__ == "__main__":
    asyncio.run(main())
