# The Python client

`hippocampus-client` is the published client package. It is the one language with a published
artefact — every other language generates its own, and [Clients in other
languages](clients.md) is that recipe.

```sh
pip install hippocampus-client
```

The distribution is `hippocampus-client` because `hippocampus` on PyPI is an unrelated
memoisation library that predates this project. The **import name is `hippocampus`**, which is
what you type.

```python
from hippocampus import Hippocampus

with Hippocampus("localhost:50051") as client:
    client.store_memory("the deploy at 14:03 rolled back cleanly", significance=50)

    for memory in client.search_memories("deploy", limit=5, reinforce=True):
        print(memory.body, memory.recall_count)
```

The source is [`integrations/python/`](../integrations/python/).

## What it covers

**Every RPC the contract declares**, unlike the [MCP bridge](mcp.md), whose surface is deliberately
curated — this is a client library rather than a tool surface handed to a model, and what a token
may actually do is enforced by the service's [role tiers](configuration.md#authorisation). A
drift guard (`tests/test_contract_coverage.py`) fails the build if the contract grows an RPC with no
method here, in both directions, so a new RPC is a gap rather than a decision.

The return types split on one line:

| Surface                                                             | Returns                        |
| ------------------------------------------------------------------- | ------------------------------ |
| Memories, events, links, search, recall, identity                   | dataclasses (`Memory`, `Event`, `Page`, `Identity`, …) |
| Topology, consolidation preview/explain, forgotten log, callback queue, transfer, archive | the protobuf message unchanged |

That is the same line the MCP bridge draws, for the same reason: what an application touches every
day is worth an ergonomic type, and what an operator reads occasionally is better served by the
message whose field comments _are_ the documentation. Anything not wrapped is still reachable —
`client.stub` is the generated gRPC stub, and `hippocampus.proto` is the generated message module.

It speaks **gRPC only**. The `/v1` JSON gateway is a fine target for a hand-written client (and is
what the [Obsidian plugin](obsidian.md) uses), but a Python program has a gRPC stack and nothing is
gained by encoding through JSON.

## What the wrapper is actually for

Four of the contract's encodings catch every new client at least once. Removing them is most of
the package's value over the generated stubs.

- **Timestamps are UnixNano `int64`.** Not seconds, not milliseconds. Here they are aware
  `datetime`s in UTC, and a `datetime` you pass is encoded for you.
- **Zero is not a timestamp.** A `time_end` of 0 means "has not ended"; a `time_recalled` of 0
  means "never recalled". Both decode to `None`, so the absence is in the type rather than in a
  magic value a caller can format into a UI as 1970.
- **`Bool` is a tri-state enum**, not proto3's `bool`, because an unset bool and an explicit false
  are the same byte on the wire and the list filters must tell them apart. Here every one of them
  is an `Optional[bool]`, where `None` applies no restriction.
- **Metadata filters travel as `"key=value"` strings** (a map cannot be bound from a URL query
  string). Here they are a `dict`.

Everything else in the package follows from the three behaviours below.

## The three things that are the product, not faults

### Insignificance is not an error

A memory below the deployment's `memory.minimumSignificance` is quietly dropped, like a brain that
simply does not retain the insignificant. The call **succeeds**, the returned id is empty, and
`rejected` is true. `Stored` is falsey in that case, which is what makes the check hard to skip:

```python
stored = client.store_memory("barely worth saying", significance=1)

if not stored:
    print("dropped as insignificant")     # not an exception - the call succeeded
```

A batch reports the same thing per record. `store_memories` returns one result per memory,
positionally, and the **call** fails only for a batch-level fault — so a caller that checks only
the status learns nothing about the individual records:

```python
batch = client.store_memories([Memory(line, 40) for line in lines])

for memory, result in zip(lines, batch):
    if result.failed:
        print(f"{memory!r}: {result.error} (code {result.code})")
```

It is also **not** `import_batch`: nothing is upserted, so a memory naming an id the store already
holds fails with `ALREADY_EXISTS` in its own result rather than replacing a live row.

### Memories disappear

A consolidation cycle deletes what has stopped mattering, so an id you hold can stop resolving at
any time. Treat a missing id as expected. `explain_consolidation` is how you ask where a memory
stands before it goes — its computed value against the current threshold, and
`days_until_forgotten`:

```python
for verdict in client.explain_consolidation([m.id for m in page]).memories:
    print(verdict.memory_id, verdict.value, verdict.days_until_forgotten)
```

That RPC is `reader` tier and enumerates nothing — it answers only about ids you supply.

### Recall is a write

`recall_memories`, and `search_memories(reinforce=True)`, reset the decay clock and raise
effective significance. `get_memories` is the read. Reinforcing a search recalls **only the page
returned**, never the wider candidate set the ranking considered.

### Deleting by predicate

`delete_memories_by_filter` and `delete_events_by_filter` take the selecting arguments of
`get_memories`/`get_events`, so the listing is the dry run — the service builds one predicate for
both, and `Page.total` says how much the deletion would remove:

```python
matching = client.get_memories(group="acme", limit=1).total
result = client.delete_memories_by_filter(group="acme", delete_empty_events=True)

assert result.memories_deleted == matching  # barring concurrent writes
```

Both are `admin` tier, both are irreversible, and both **refuse a call carrying no filter** —
deleting everything is `purge`. `max_deletions` bounds one call and the response's `complete` says
whether anything still matches, so a cautious caller can work through a large partition in steps.

## Feature-detect with `who_am_i()`

Which search modes a deployment serves, whether it has an embedded summariser, whether it
consolidates at all — these are properties of the **deployment**, not the caller, and asking is how
you avoid learning them from a rejection:

```python
identity = client.who_am_i()

mode = SearchMode.HYBRID if identity.supports(SearchMode.HYBRID) else SearchMode.KEYWORD
results = client.search_memories("rollback", mode=mode)
```

`Identity` separates the two axes deliberately: `client_id`, `role`, `groups` and `group_scoped`
describe the **caller**; `search_modes`, `summariser_enabled`, `consolidation_enabled`,
`tombstones_enabled`, `callbacks_enabled`, `topology_tier` and `version` describe the
**deployment** and are the same for everyone calling that instance.

Read **`group_scoped`**, never whether `groups` is empty. An empty list means unscoped — the whole
store — which is the opposite of scoped to nothing. See [Group
scoping](configuration.md#group-scoping) for what changes when a token is scoped, in particular
that a `NotFound` may mean "not yours".

## Authentication and TLS

Both are off in a default deployment, so an address is often all that is needed.

```python
client = Hippocampus(
    "hippocampus.internal:50051",
    token=os.environ["HIPPOCAMPUS_TOKEN"],
    tls=True,
    ca_cert=pathlib.Path("ca.pem").read_bytes(),          # only for a private/self-signed CA
    client_cert=pathlib.Path("client.pem").read_bytes(),  # only where the listener asks for one
    client_key=pathlib.Path("client-key.pem").read_bytes(),
)
```

Supplying any TLS material selects a secure channel even without `tls=True` — the material is the
intent, and silently opening a plaintext connection because a flag was forgotten is the wrong
failure.

The token travels as **per-call metadata** rather than channel credentials. That is not an
implementation detail worth hiding: gRPC permits call credentials on a secure channel only, so
binding them to the channel would work against a TLS deployment and raise against a plaintext one
— and plaintext behind a TLS-terminating sidecar or mesh is a supported deployment here, not a
mistake to guard against.

There is deliberately **no skip-verify option**. The Python gRPC stack does not offer one, and
implementing something else under that name would mislead. Use `ca_cert` for a private CA, and
`server_name_override` for a certificate whose name does not match the address you dial.

## Errors

Every failure raises a `HippocampusError` subclass carrying the gRPC `code`, with the original
`grpc.RpcError` as `__cause__`. Three mean something specific to this service:

| Exception            | What it usually means                                                  |
| -------------------- | ---------------------------------------------------------------------- |
| `NotFound`           | No such record — **or**, under group scoping, none this token may see   |
| `Unavailable`        | Usually a `Purge` in progress, which is brief and worth retrying        |
| `FailedPrecondition` | The deployment cannot serve this feature; ask `who_am_i()` first        |

The rest are the obvious ones: `Unauthenticated`, `PermissionDenied`, `AlreadyExists`,
`InvalidArgument`, `ResourceExhausted`, `DeadlineExceeded`, and `ServiceError` for anything else.

## Deadlines

The service bounds its own storage operations (`storage.queryTimeoutSeconds`, 60s by default) but
imposes **no deadline on an RPC**, and `sleep`, `export`, `transfer` and `purge` are long
operations — so the wait is the client's to bound. The client applies a 30-second default to every
call. Pass `timeout` to the constructor to change it globally, or to any method for one call:

```python
client.export(timeout=600)
```

## Versioning

The package version **is** the service release it was built from: `hippocampus-client==0.43.0` is
the client generated from the `v0.43.0` contract, published by the same release workflow from the
same tag. That coupling is deliberate. The stubs are generated from
[`contract/hippocampus.proto`](../contract/hippocampus.proto) at build time and are **not
committed**, so there is no second copy of the contract anywhere to drift from it — which is the
failure a published client package invites and the one thing item 64 set out to avoid.

Pin it the way you pin the service. The [compatibility policy](../CHANGELOG.md#compatibility)
covers the contract, and pre-1.0 a deliberate break ships in a minor release and is listed under
**Breaking** in the changelog.

## Building it from a clone

The stubs are generated, so a build needs `grpcio-tools` — supplied automatically by the build
hook, which regenerates from the contract before either distribution is built. An editable install
runs that hook too, so there is no separate setup step:

```sh
cd integrations/python
pip install -e '.[dev]'
python -m pytest
```

An editable install generates the stubs once, at install time. After editing the contract,
regenerate them without reinstalling — which is why `grpcio-tools` is in the `dev` extra as well as
in `build-system.requires`, the build backend's environment being isolated from this one:

```sh
python scripts/generate_stubs.py
```

One transform happens during generation and is worth knowing about: the contract imports
protoc-gen-openapiv2's annotations for the OpenAPI document's `securityDefinitions`, and protoc
emits a matching `from protoc_gen_openapiv2.options import annotations_pb2` — an import that
**cannot be satisfied in Python**, because those annotations have no PyPI distribution.
`scripts/generate_stubs.py` strips the import and its option block before generating. They are
generator input and appear nowhere in the wire format, so no message, field number or method
changes; both removals are asserted rather than attempted, because a silently-skipped strip
produces a wheel that installs cleanly and raises on first import.

The published sdist and wheel both carry the generated modules, so installing from PyPI needs no
protoc and no access to this repository.
