# hippocampus-client

The Python client for [Hippocampus](https://github.com/fastbean-au/hippocampus) — a memory service
that stores what matters and forgets what stops mattering.

```sh
pip install hippocampus-client
```

The distribution is `hippocampus-client` because `hippocampus` on PyPI is an unrelated library.
The import name is `hippocampus`.

## Using it

```python
from hippocampus import Hippocampus

with Hippocampus("localhost:50051") as client:
    client.store_memory("the deploy at 14:03 rolled back cleanly", significance=50)

    for memory in client.search_memories("deploy", limit=5, reinforce=True):
        print(memory.body, memory.recall_count)
```

Every RPC the service declares has a method here — the memory and event surface returns the
dataclasses in `hippocampus.models`, and the operator surface (topology, the consolidation preview,
the forgotten log, the callback queue, transfer) returns its protobuf message unchanged. Anything
not wrapped is reachable through `client.stub`.

## Three things that surprise a new client

All three are the product rather than faults, and the types are shaped to make them hard to miss.

**Insignificance is not an error.** A memory below the deployment's minimum significance is quietly
dropped: the call succeeds and the result's `rejected` is true. `Stored` is falsey in that case, so
the check reads naturally:

```python
stored = client.store_memory("barely worth saying", significance=1)

if not stored:
    print("dropped as insignificant")     # not an exception - the call succeeded
```

**Memories disappear.** A consolidation cycle deletes what has stopped mattering, so an id you hold
can stop resolving at any time. Treat a missing id as expected rather than as a bug.

**Recall is a write.** `recall_memories`, and `search_memories(reinforce=True)`, reset the decay
clock and raise effective significance. `get_memories` is the read.

## Feature-detect, do not probe

Which search modes a deployment serves, whether it has an embedded summariser, whether it
consolidates at all — these are properties of the deployment, and `who_am_i()` reports them so you
never learn them from a rejection:

```python
identity = client.who_am_i()

mode = SearchMode.HYBRID if identity.supports(SearchMode.HYBRID) else SearchMode.KEYWORD
results = client.search_memories("rollback", mode=mode)
```

Read `identity.group_scoped`, never whether `identity.groups` is empty: an empty list means
unscoped — the whole store — which is the opposite of scoped to nothing.

## Authentication and TLS

Both are off in a default deployment, so an address is often all you need. Where they are on:

```python
client = Hippocampus(
    "hippocampus.internal:50051",
    token=os.environ["HIPPOCAMPUS_TOKEN"],
    tls=True,
    ca_cert=pathlib.Path("ca.pem").read_bytes(),      # only for a private CA
)
```

`client_cert`/`client_key` present a client certificate to a listener configured to ask for one.
There is deliberately no skip-verify option — the Python gRPC stack does not offer one, and
trusting an arbitrary CA under that name would be a different thing. Use `ca_cert` for a private
CA, and `server_name_override` for a certificate whose name does not match the address you dial.

## Errors

Every failure is a `HippocampusError` subclass carrying the gRPC status code, with the original as
`__cause__`. Three mean something specific here:

| Exception            | What it usually means                                                     |
| -------------------- | ------------------------------------------------------------------------- |
| `NotFound`           | No such record — **or**, under group scoping, none this token may see      |
| `Unavailable`        | Usually a purge in progress, which is brief and worth retrying             |
| `FailedPrecondition` | The deployment cannot serve this feature — ask `who_am_i()` first          |

## Deadlines

The service bounds its own storage operations but imposes no deadline on an RPC, and consolidation,
export and transfer are long. The client applies a 30-second default to every call; pass `timeout`
to the constructor to change it, or to any method for that one call.

## Development

The gRPC stubs are **generated from `contract/hippocampus.proto` at build time and are not
committed** — a committed copy would be a second copy of the contract, free to drift from it. The
build hook regenerates them, so the install produces them and there is no separate setup step:

```sh
pip install -e '.[dev]'
python -m pytest
```

An editable install generates them once. After editing the contract, regenerate without
reinstalling:

```sh
python scripts/generate_stubs.py
```

The published sdist and wheel both carry the generated modules, so installing from PyPI needs no
protoc.

`tests/test_contract_coverage.py` is the drift guard: it fails if the contract declares an RPC this
client has no method for. The package's promise is the full RPC surface, so a new RPC is a gap here
rather than a decision.

## Documentation

- [docs/python.md](https://github.com/fastbean-au/hippocampus/blob/main/docs/python.md) — this
  client in full
- [docs/clients.md](https://github.com/fastbean-au/hippocampus/blob/main/docs/clients.md) — the
  other languages, and the behaviour notes above in more detail
- [CHANGELOG.md](https://github.com/fastbean-au/hippocampus/blob/main/CHANGELOG.md) — the
  compatibility policy this package ships under

The package version is the service release it was built from: `hippocampus-client==0.43.0` is the
client generated from the `v0.43.0` contract.
