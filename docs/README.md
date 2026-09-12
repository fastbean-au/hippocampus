# Documentation

Everything written about Hippocampus, arranged by what you're trying to do rather than
alphabetically. The [project README](../README.md) is the one-page summary; this is the map.

**New here?** [Use cases & deployment modes](use-cases.md) is where to decide whether the store fits
— its _Worth knowing before you start_ section is the set of properties that shape what you can
build on this. Then [Install](install.md) and [Getting started](getting-started.md) to have one
running.

## Installing and running

| Guide                                            | What it answers                                                                                   |
| :----------------------------------------------- | :------------------------------------------------------------------------------------------------ |
| **[Install](install.md)**                        | Homebrew, packages, containers, release binaries or source — and how to tell it's working.        |
| **[Getting started](getting-started.md)**        | Running with no configuration at all, then a minimal one, and the first requests.                 |
| **[Use cases & deployment modes](use-cases.md)** | Which topology fits — embedded, centralised, per-tenant, edge-to-centre, or retention controller. |
| **[Configuration wizard](config-wizard.md)**     | Build a config and its deployment artefacts in the browser, with a live forgetting preview.       |
| **[Configurability](configuration.md)**          | The exhaustive key reference: storage, listeners, auth, TLS, search, callbacks, topology.         |
| **[Operations & deployment](operations.md)**     | Supervision, containers, Kubernetes, driver choice, sizing, backup, shutdown, observability.      |
| **[Security](security.md)**                      | What's off by default, auth and role tiers, the hardening checklist, where content can leave.     |

The deployment artefacts each carry their own README:
[`deploy/systemd`](../deploy/systemd/README.md) (the packaged unit and its sandbox),
[`deploy/launchd`](../deploy/launchd/README.md) (a per-user macOS agent),
[`deploy/k8s`](../deploy/k8s/README.md) (Kustomize base and overlays),
[`deploy/observability`](../deploy/observability/README.md) (the shipped alert rules), and
[`deploy/compose`](../deploy/compose/) (the Compose stacks per driver).

## How it decides what to forget

| Guide                                        | What it answers                                                                                  |
| :------------------------------------------- | :----------------------------------------------------------------------------------------------- |
| **[Memory consolidation](consolidation.md)** | The value model, the six decay algorithms, the capacity axes, eviction, and summarisation.       |
| **[Retention quality](retention.md)**        | Of everything it threw away, how much did you need later — measured against LRU, LFU and random. |
| **[Performance](performance.md)**            | Throughput sweeps across SQLite, Postgres and MySQL, and how the sleep cycle copes.              |

## Watching it work

| Guide                                   | What it answers                                                                         |
| :-------------------------------------- | :-------------------------------------------------------------------------------------- |
| **[Web console](console.md)**           | The console every instance serves at `/ui` — each tab, and where its numbers come from. |
| **[Demonstrations](demonstrations.md)** | The hosted demo, plus worked scenarios over real-world data shapes and generators.      |

The load generator and the compressed-clock demo stack are in [`demo/`](../demo/README.md).

## Talking to it

| Guide                               | What it answers                                                                         |
| :---------------------------------- | :-------------------------------------------------------------------------------------- |
| **[CLI (`hippo`)](cli.md)**         | Drive a running service from the shell, over gRPC or the JSON gateway.                  |
| **[Python client](python.md)**      | `pip install hippocampus-client` — the published client, covering the full RPC surface. |
| **[Clients & codegen](clients.md)** | Generate a TypeScript, Java, Rust or any-language client from the proto or OpenAPI doc. |
| **[MCP server](mcp.md)**            | Give an LLM host memory tools via the Model Context Protocol.                           |

## Feeding it

| Guide                                | What it answers                                                                           |
| :----------------------------------- | :---------------------------------------------------------------------------------------- |
| **[Event sourcing](eventsource.md)** | Bridge NATS, MQTT, RabbitMQ, Kafka or the Bluesky firehose in, one memory per message.    |
| **[Ingestor](ingestor.md)**          | Stage data at the edge and promote completed events into a central store under CEL rules. |
| **[LlamaIndex](llamaindex.md)**      | `pip install llama-index-memory-hippocampus` — an agent's memory, reinforced by use.      |
| **[Obsidian](obsidian.md)**          | Use the store as a memory layer for a note vault, via the plugin or the MCP bridge.       |
| **[Object storage](objectstore.md)** | Govern a bucket you do not hold: reinforce on read, delete on forget.                     |

Each integration in this repository is a self-contained subproject with its own README:
[`integrations/cli`](../integrations/cli/README.md),
[`integrations/mcp`](../integrations/mcp/README.md),
[`integrations/python`](../integrations/python/README.md),
[`integrations/eventsource`](../integrations/eventsource/README.md),
[`integrations/ingestor`](../integrations/ingestor/README.md), and
[`integrations/objectstore`](../integrations/objectstore/README.md).

Three more live in their own repositories, because each tracks a release train that is not this
one's — see the pages above, which say what they are and where they went:
[hippocampus-obsidian](https://github.com/fastbean-au/hippocampus-obsidian),
[hippocampus-llamaindex](https://github.com/fastbean-au/hippocampus-llamaindex), and
[hippocampus-otel-collector](https://github.com/fastbean-au/hippocampus-otel-collector).

## The project itself

- [`CHANGELOG.md`](../CHANGELOG.md) — the curated record, and the **Compatibility** section saying
  what a version number covers.
- [`RELEASE.md`](../RELEASE.md) — how a release is cut, and what a deliberate break requires.
- [`SECURITY.md`](../SECURITY.md) — supported versions and how to report a vulnerability.
- [`contract/hippocampus.proto`](../contract/hippocampus.proto) — the gRPC contract every client is
  generated from.
