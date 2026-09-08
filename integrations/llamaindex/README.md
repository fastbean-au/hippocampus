# llama-index-memory-hippocampus

A [LlamaIndex](https://docs.llamaindex.ai) long-term memory block backed by
[Hippocampus](https://github.com/fastbean-au/hippocampus) — a memory service that stores what
matters and forgets what stops mattering.

```sh
pip install llama-index-memory-hippocampus
```

The import name is `llama_index.memory.hippocampus`, following LlamaIndex's convention for an
integration package.

## Using it

```python
from hippocampus import Hippocampus
from llama_index.memory.hippocampus import hippocampus_memory

client = Hippocampus("localhost:50051")
memory = hippocampus_memory(client, session_id="support-session-1", group="support-bot")

response = await agent.run("what did we decide about the rollback?", memory=memory)
```

`hippocampus_memory` is a convenience over the block itself, which composes with any other:

```python
from llama_index.core.memory import Memory
from llama_index.memory.hippocampus import HippocampusMemoryBlock

memory = Memory.from_defaults(
    session_id="support-session-1",
    memory_blocks=[
        HippocampusMemoryBlock(client=client, group="support-bot", limit=8),
    ],
)
```

The client is passed in already connected rather than built from an address, so this package does
not restate the connection options (token, TLS, private CA, client certificate, deadlines) that
`Hippocampus` already documents. The block does not own the channel and never closes it.

## Why a forgetting store fits this slot

Retrieval **recalls** what it returns. A recall in Hippocampus is a write: it resets the memory's
decay clock and raises its effective significance. So a fact the agent keeps reaching for is
reinforced by being used, and one it never retrieves decays out of the store on its own.

That is the entire retention policy, and it is why there is no eviction, no cap and no TTL in this
package. The consequences are worth stating plainly:

- **Memories disappear.** A consolidation cycle deletes what has stopped mattering, so a memory
  this block wrote can stop existing at any time. Nothing here treats that as an error, and a
  long-term memory block is the one slot where nothing breaks when it happens — facts folded into
  a prompt by relevance are _supposed_ to thin out.
- **`reinforce=False` breaks the loop.** It leaves a store that forgets precisely the memories the
  agent has been using. It exists for a read-only observer, not for tuning.
- **Insignificance is not a failure.** A message below the deployment's minimum significance is
  quietly dropped: the write succeeds with an empty id. It is logged at debug and never raised.

## What gets stored

One memory per chat message, at a significance taken from the role:

| Role            | Default significance | Stored |
| --------------- | -------------------- | ------ |
| `user`          | 50                   | yes    |
| `assistant`     | 40                   | yes    |
| everything else | —                    | no     |

The `significance` map is the selector as well as the ranking, so a role absent from it is not
stored at all — adding `"tool": 30` is what starts storing tool output. System messages are absent
deliberately: a system prompt is configuration resent on every call, not something the agent
learned.

`significance_fn` overrides the map per message and may return `None` to skip one, which is how a
caller stores only what it judges worth keeping:

```python
HippocampusMemoryBlock(
    client=client,
    significance_fn=lambda message: 70 if "decision:" in message.content else None,
)
```

The role and the session id are recorded as **metadata**, never folded into the body. A body is
what the content index tokenises, so wrapping it in `<message role='user'>` would put `message`,
`role` and `user` into the index for every memory — three terms that match everything and rank
nothing. The role is restored as a prefix when the memory is rendered back into a prompt.

## Retrieval

The query is the last `retrieval_context_window` messages joined together, and `limit` memories
come back ranked by relevance blended with the store's own significance and recall count.

`mode` is resolved **once**, at first retrieval, against what `who_am_i()` says the deployment can
serve — richest first, so an instance that gains an embedding model starts doing hybrid retrieval
with no code change. Asking rather than trying is the point: a mode with no backend is refused per
call, so a block configured for one would fail every retrieval for the life of the process while
looking like a retrieval bug. Naming a mode the deployment cannot serve raises once, at first use,
naming what it does serve.

Retrieval is **not** scoped to the current session by default. Remembering across conversations is
what a long-term block is for; `scope_to_session=True` restricts it to the session at hand.

## Truncation

When the composed memory exceeds its token limit, this block drops retrieved memories from the end
rather than discarding itself wholesale, which is the base class's behaviour. Results arrive
ranked, so the end costs least. Give the block a non-zero `priority` for that to be reachable —
`priority=0` means never truncate.

## Async

Every call into the service is blocking gRPC run through `asyncio.to_thread`. The published client
is synchronous, and doing that in one place here is better than maintaining a second, async client
in step with the contract.

## Development

The package depends on `hippocampus-client`, which is published from the same tag. From a clone:

```sh
pip install -e ../python          # the client, from this repository
pip install -e '.[dev]' --no-deps
pip install llama-index-core pytest pytest-asyncio
python -m pytest
```

`--no-deps` is what makes a clone work before the client has been published for that release: pip
would otherwise refuse the locally installed `0.0.0.dev0` against the declared floor.

The tests drive the block against a fake client and need no service.

## Documentation

- [docs/llamaindex.md](https://github.com/fastbean-au/hippocampus/blob/main/docs/llamaindex.md) —
  this adapter in full
- [docs/python.md](https://github.com/fastbean-au/hippocampus/blob/main/docs/python.md) — the
  client underneath it
- [CHANGELOG.md](https://github.com/fastbean-au/hippocampus/blob/main/CHANGELOG.md) — the
  compatibility policy this package ships under

The package version is the service release it was built from.
