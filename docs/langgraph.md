# The LangGraph store

`langgraph-store-hippocampus` is a LangGraph `BaseStore` backed by Hippocampus: an agent's long-term
memory where **reading an item is what keeps it alive**. LangGraph's `refresh_ttl` — on by default —
becomes a recall, which resets the item's decay clock and raises its significance; an item the agent
never reads again decays out. The store therefore carries no TTL and no eviction of its own — the
service's consolidation cycle is both.

It lives in its own repository:

**[fastbean-au/hippocampus-langgraph](https://github.com/fastbean-au/hippocampus-langgraph)**

which is where its documentation, releases and install instructions are. It lives there for the
reason the LlamaIndex adapter does: it tracks `langgraph-checkpoint`, whose release cadence is not
this project's, and it depends on the `hippocampus-client` package rather than on the contract.

```python
from hippocampus import Hippocampus
from langgraph.store.hippocampus import HippocampusStore

store = HippocampusStore(Hippocampus("localhost:50051"), group="support-bot")
graph = builder.compile(store=store)
```

**Neither package is on PyPI yet.** Until both are published, install the client wheel attached to a
service release, then the store without its dependency check — the repository's README has the
commands.

## How an item maps onto a memory

One item is one memory. The memory's id is a UUIDv5 of the group, namespace and key, so a `get` is
one read by id and a `put` is a create that falls back to an update; the value is the body, as JSON;
and each namespace label is its own metadata key (`lg.ns.0`, `lg.ns.1`, …), so a namespace-prefix
search is an exact-match metadata filter the service evaluates and pages itself. A value filter
cannot be pushed down — the value is the body — and is applied by the store over the pages that
come back.

Every item also carries `lg.v=1`, which every read filters on, so a group shared with the LlamaIndex
block or a broker bridge never surfaces their memories as LangGraph items.

## What is still this repository's

- [`docs/python.md`](python.md) — the `hippocampus-client` package the store is built on.
- [`docs/consolidation.md`](consolidation.md) — how the store decides what to forget.
- [`docs/configuration.md#authorisation`](configuration.md#authorisation) — the tiers a token needs.
  The store is a writer: it stores, updates, recalls and deletes by id.

## Compatibility

The store names exactly seven client calls in its `MemoryClient` protocol, and that list, held by a
test, is the only thing between an agent's memory and `Purge`. It does not read the contract, so a
change here reaches it only through `hippocampus-client`; its CI pins a service release, installs the
client wheel from it and runs a live suite against that release's image, and a `repository_dispatch`
from this repository's release raises the pin and reports whether the tests still pass against it.
