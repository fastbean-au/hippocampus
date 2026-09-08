# The LlamaIndex adapter

`llama-index-memory-hippocampus` is a [LlamaIndex](https://docs.llamaindex.ai) **long-term memory
block** backed by a Hippocampus store. It is the framework adapter over the [Python
client](python.md): the client is how a program calls this service, and this is how an agent uses
it as its memory without being written against it at all.

```sh
pip install llama-index-memory-hippocampus
```

The import name is `llama_index.memory.hippocampus`, following LlamaIndex's convention for an
integration package (`llama-index-<category>-<name>`). The source is
[`integrations/llamaindex/`](../integrations/llamaindex/).

```python
from hippocampus import Hippocampus
from llama_index.memory.hippocampus import hippocampus_memory

client = Hippocampus("localhost:50051")
memory = hippocampus_memory(client, session_id="support-session-1", group="support-bot")

response = await agent.run("what did we decide about the rollback?", memory=memory)
```

## Why this slot, and not a conversation session

Every agent framework has somewhere to plug memory in, and they are not interchangeable. This one
was chosen because a **forgetting** store fits it and breaks the others.

A conversation transcript — an OpenAI Agents SDK `Session`, or any chat-history store — has a
structural invariant. The item list handed back to the model must be well-formed: a tool call needs
its matching tool result. Those are two records with two significances, and a consolidation cycle
will eventually delete one of them, after which the next model call receives a malformed history
and errors. It is fixable, by pinning a turn into one event and leaning on
`consolidation.minimumRetentionInDays` — but the fix is precisely a constraint on forgetting, so
the adapter would demonstrate the thesis by suppressing it.

A long-term memory block has no such invariant. Facts retrieved by relevance and folded into a
prompt are _supposed_ to thin out, and nothing is corrupted when a low-value one goes. That is not
a tolerable degradation, it is the intended behaviour of both halves at once.

LlamaIndex also has more than one slot — memory now, a vector store or a docstore later — so this
opens a door rather than filling the only opening.

## The loop

Retrieval **recalls** what it returns, and a recall is a write: it resets the memory's decay clock
and raises its effective significance (see [Consolidation](consolidation.md)). So a fact the
agent keeps reaching for is reinforced by the act of being used, and one it never retrieves decays
out on its own.

That is the entire retention policy. There is no eviction, no cap and no TTL in the adapter,
because the store already has all three and they are driven by what the agent actually did.

```text
  agent turn ──> block writes a memory ──> significance decays with age
                                                    │
  agent question ──> block searches ──> returns ──> recall resets the clock, raises significance
                                                    │
                                          never returned ──> consolidation forgets it
```

Three consequences follow, and all three are the product rather than faults:

- **Memories disappear.** A memory this block wrote can stop existing at any time. Nothing in the
  adapter treats that as an error.
- **`reinforce=False` breaks the loop**, leaving a store that forgets precisely the memories the
  agent has been using. It exists for a read-only observer, not for tuning.
- **Insignificance is not a failure.** A message below the deployment's
  `memory.minimumSignificance` is quietly dropped: the write succeeds with an empty id. The adapter
  logs it at debug and never raises.

## What gets stored

One memory per chat message, at a significance taken from its role:

| Role            | Default significance | Stored |
| --------------- | -------------------- | ------ |
| `user`          | 50                   | yes    |
| `assistant`     | 40                   | yes    |
| everything else | —                    | no     |

The `significance` map is the **selector as well as the ranking** — one knob rather than two, so
there is no way to configure a role that is written and then ranked by a default nobody chose. A
role absent from it is not stored, and adding `"tool": 30` is what starts storing tool output.
System messages are absent deliberately: a system prompt is configuration resent on every call, not
something the agent learned.

`significance_fn` overrides the map per message and may return `None` to skip one:

```python
HippocampusMemoryBlock(
    client=client,
    significance_fn=lambda message: 70 if "decision:" in message.content else None,
)
```

The numbers are relative and mean nothing on their own — what a user said is ranked above the
assistant's restatement of it because the restatement is recoverable and the original is not.
`client.significance_levels()` reports the scale a deployment is already using.

### The role goes in metadata, not the body

A memory body is what the [content index](configuration.md#content-search) tokenises. Wrapping it
as `<message role='user'>…</message>` — which is what the framework's own vector memory block does
— would put `message`, `role` and `user` into the index for **every** memory: three terms that
match everything and rank nothing. Metadata is filterable and unindexed, which is exactly what a
role is. It is restored as a prefix when the memory is rendered back into a prompt.

The session id is recorded the same way, under `session_metadata_key`.

## Retrieval

The query is the last `retrieval_context_window` messages joined together, and `limit` memories
come back ranked by relevance blended with the store's own significance and recall count
([Ranking](configuration.md#ranking)).

**The mode is resolved once**, at the first retrieval, from `who_am_i().search_modes` — richest
first, so an instance that later gains an embedding model starts doing hybrid retrieval with no
code change. Asking rather than trying is the point: a mode with no backend is refused per call, so
a block configured for one would fail every retrieval for the life of the process while looking
like a retrieval bug. Naming a mode the deployment cannot serve raises once, at first use, naming
what it does serve.

**Retrieval is not scoped to the current session by default.** Remembering across conversations is
what a long-term block is for; recording which conversation a memory came from and restricting
reads to it are different questions, and `scope_to_session=True` is the second one.

Fixed `metadata` is stamped on writes _and_ filtered on reads, so what a block reads back is what
that block wrote — which is how two agents share a store without reading each other's memories.
For a partition that is enforced rather than agreed, use `group` with a
[group-scoped token](configuration.md#group-scoping).

## How a composed `Memory` feeds the block

Worth knowing before concluding the adapter is broken: `Memory` does **not** hand every turn to its
memory blocks. A turn goes into the short-term buffer, and only reaches a block when that buffer
overflows its token limit and waterfalls the oldest messages out. A short conversation therefore
writes nothing.

That is the right shape here — the store receives what the conversation has moved past — but if you
want a turn written as it happens, call the block directly:

```python
block = HippocampusMemoryBlock(client=client, group="support-bot")

await block.aput([user_message, assistant_message], session_id="support-session-1")
```

`accept_short_term_memory=False` (a field of the base class) turns the waterfall off entirely,
leaving the direct call as the only way in.

Lowering `token_limit` is how you make the waterfall reach the store sooner, and
`hippocampus_memory` lowers the flush size with it. That is not a preference: `Memory`'s own default
flush size is 10% of the _default_ limit rather than of the one you passed, and it must stay
comfortably under `token_limit * chat_history_token_ratio` — so lowering the limit on its own is
rejected for a field you never set, with a message naming neither.

## Truncation

When the composed memory exceeds its token limit, this block drops retrieved memories **from the
end** rather than discarding itself wholesale, which is the base class's behaviour. Results arrive
ranked, so the end costs least. Give the block a non-zero `priority` for that to be reachable at
all — `priority=0` means never truncate.

## What the adapter is allowed to do

`Hippocampus` exposes every RPC the contract declares, `purge` and `clear` among them. The block
holds a `MemoryClient` instead — a `runtime_checkable` protocol naming exactly four calls:

| Call              | Why                                            |
| ----------------- | ---------------------------------------------- |
| `who_am_i`        | resolve the search mode once                   |
| `store_memories`  | write a batch of turns                         |
| `store_memory`    | the fallback for a service with no batch write |
| `search_memories` | retrieve, and reinforce what is retrieved      |

That list is the only thing standing between an agent's memory and the destructive half of the
surface, which is the same line the [event-source bridges](eventsource.md) draw with their own
client interface — and a test holds it to exactly four, so growing it is a decision rather than an
import. It is structural, so a real `Hippocampus` satisfies it as it is, and an application wrapping
the client for its own metrics, retries or rate limiting can pass that instead.

## Connection, authentication and TLS

The client is passed in **already connected** rather than built from an address here, so this
package does not restate the ten connection parameters that
[`Hippocampus`](python.md#authentication-and-tls) already documents — a second copy of them is a
second thing to keep current. The block does not own the channel and never closes it.

```python
client = Hippocampus(
    "hippocampus.internal:50051",
    token=os.environ["HIPPOCAMPUS_TOKEN"],
    tls=True,
)
```

The token needs **writer** tier: the block stores memories, and a reader's retrieval does not
reinforce unless the deployment sets `auth.readerRecallReinforces`
([Authorisation](configuration.md#authorisation)), which would quietly break the loop above.

## Async

Every call into the service is blocking gRPC run through `asyncio.to_thread`. The published client
is synchronous, and doing that in one place here is better than maintaining a second, async client
in step with the contract.

## Versioning

The package version is the service release it was built from, stamped from the tag by the release
workflow exactly as the client's is. It depends on `hippocampus-client` with a floor rather than a
pin: a newer client is a newer contract, and nothing here reads a field an older one lacked.

The `llama-index-core` floor is `0.12.36`, the release that first exported `BaseMemoryBlock`. CI
runs the suite against that floor as well as the newest release, on the same reasoning the client
package tests its interpreter floor — a floor nothing installs is a floor that is wrong.

## Building it from a clone

```sh
cd integrations/llamaindex
pip install -e ../python                       # the client, from this repository
pip install llama-index-core pytest pytest-asyncio
pip install -e . --no-deps
python -m pytest
```

`--no-deps` is what makes a clone work before the client has been published for that release: pip
would otherwise refuse the locally installed `0.0.0.dev0` against the declared floor. The tests
drive the block against a fake client and need no service.
