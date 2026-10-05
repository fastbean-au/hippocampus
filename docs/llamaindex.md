# The LlamaIndex adapter

`llama-index-memory-hippocampus` is a LlamaIndex `BaseMemoryBlock` backed by Hippocampus: an agent's
long-term memory slot where **retrieval is what keeps a fact alive**. A fact the agent keeps reaching
for is recalled by being used; one it never retrieves decays out. The adapter therefore carries no
eviction, no cap and no TTL of its own — the store's consolidation cycle is the eviction policy.

It lives in its own repository:

**[fastbean-au/hippocampus-llamaindex](https://github.com/fastbean-au/hippocampus-llamaindex)**

which is where its documentation, releases and install instructions are. It moved there because it
tracks `llama-index-core`, whose release cadence is not this project's, and because it depends on the
`hippocampus-client` package rather than on the contract — so it was already a downstream consumer
rather than a part of the service.

**Neither package is on PyPI yet, and that makes the adapter awkward to install.** It requires
`hippocampus-client>=0.43`, and a client installed from a git tag reports `0.0.0.dev0`, so pip refuses
the pair. Until both are published, install the client from a tag (see
[the Python client](python.md)), then the adapter without its dependency check:

```sh
pip install "llama-index-core>=0.12.36"
pip install --no-deps "llama-index-memory-hippocampus @ git+https://github.com/fastbean-au/hippocampus-llamaindex"
```

## What is still this repository's

- [`docs/python.md`](python.md) — the `hippocampus-client` package the adapter is built on, which is
  still developed here because its gRPC stubs are generated from `contract/hippocampus.proto` at
  build time.
- [`docs/consolidation.md`](consolidation.md) — how the store decides what to forget, which is the
  behaviour the whole slot is built around.
- [`docs/configuration.md#authorisation`](configuration.md#authorisation) — the tiers a token needs.
  The adapter is a writer: it stores and it recalls.

## Compatibility

The adapter names exactly four client calls, and that list is the only thing between an agent's
memory and `Purge`. It does not read the contract, so a change here reaches it only through
`hippocampus-client`; its CI pins a service release and installs the client wheel from it, and a
`repository_dispatch` from this repository's release raises that pin and reports whether the tests
still pass against it.
