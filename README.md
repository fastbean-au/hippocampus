# Hippocampus

> **A finite, biologically-inspired memory storage engine for log retention, audit trails, and context management.**

[![Coverage Status](https://coveralls.io/repos/github/fastbean-au/hippocampus/badge.svg?branch=main)](https://coveralls.io/github/fastbean-au/hippocampus)
![Dependabot](https://img.shields.io/badge/dependabot-enabled-brightgreen)
[![Known Vulnerabilities](https://snyk.io/test/github/fastbean-au/hippocampus/badge.svg)](https://snyk.io/test/github/fastbean-au/hippocampus)
[![Go Reference](https://pkg.go.dev/badge/github.com/fastbean-au/hippocampus.svg)](https://pkg.go.dev/github.com/fastbean-au/hippocampus)
![GitHub go.mod Go version](https://img.shields.io/github/go-mod/go-version/fastbean-au/hippocampus)

![Hippocampus Architecture](docs/go-hippocampus.png)

Time-to-live and fixed FIFO queues manage bounded disk space by treating age as a proxy for value —
so the anomaly, the audit event, and the context everyone keeps coming back to get purged for
crossing an arbitrary threshold. Hippocampus scores each record instead, on significance, age,
recall, and what it is linked to, and runs a periodic **sleep** cycle that forgets the low-value
tail first. The store stays inside a finite budget; what it keeps is what turned out to matter.

- **Relative significance** — rank a record `ABOVE`, `BELOW`, or `BETWEEN` existing ones and the
  server opens a gap for it, instead of forcing everything onto a fixed importance scale.
- **Reinforcement through recall** — reading a record strengthens it: the decay clock resets and its
  effective significance rises, so frequently-referenced data resists decay.
- **Sleep and consolidation** — background cycles apply the decay model, compact the store, and can
  distil a pile of episodic detail into one semantic summary (optionally with an embedded LLM).
- **Retrieval ranked by value** — content search out of the box on the default embedded install,
  with significance and recall blended into the result order. Add OpenSearch and an embedding model
  for semantic and hybrid retrieval.
- **Durable and compliance-safe** — embedded or centralised, over SQLite (WAL), PostgreSQL, or
  MySQL, with retention floors that guarantee a compliance window regardless of storage pressure.

_Where it fits and where it does not: **[Use cases & deployment modes](docs/use-cases.md)**. How the
forgetting actually works: **[Memory consolidation](docs/consolidation.md)**._

---

## 🔭 See it running — [hippocampus-demo.com](https://hippocampus-demo.com)

---

## 📚 Documentation

The guides below cover the service itself; the components that ship _around_ it are in the second
table. If you are deciding rather than deploying, read **[Use cases & deployment
modes](docs/use-cases.md)** first — its _Worth knowing before you start_ section is the set of
properties that shape what you can build on this.

### Core

| Guide                                                | Description                                                                                       |
| :--------------------------------------------------- | :------------------------------------------------------------------------------------------------ |
| 🎬 **[Getting Started](docs/getting-started.md)**    | Step-by-step build, initial config, and first gRPC/HTTP requests.                                 |
| 📐 **[Use Cases & Patterns](docs/use-cases.md)**     | Embedded vs. centralised topologies and data transfer strategies.                                 |
| 🧠 **[Memory Consolidation](docs/consolidation.md)** | Deep dive on decay algorithms, capacity targets, and summarisation.                               |
| 🧙 **[Configuration wizard](docs/config-wizard.md)** | Build a config and its deployment artefacts in the browser, with a live forgetting preview.       |
| ⚙️ **[Configurability](docs/configuration.md)**      | Exhaustive key reference for TLS, auth, storage drivers, and listeners.                           |
| 🛠️ **[Operations & Deployment](docs/operations.md)** | Containers, Kubernetes, packages, sizing, backups, shutdown, and observability.                   |
| 🔒 **[Security](docs/security.md)**                  | What is off by default, auth and role tiers, hardening checklist, and where content can leave.    |
| 📊 **[Performance Benchmarks](docs/performance.md)** | Throughput sweeps across SQLite, Postgres, and MySQL under heavy loads.                           |
| 🧪 **[Demonstrations](docs/demonstrations.md)**      | The hosted demo, plus worked scenarios using real-world data shapes and generators.               |
| 🖥️ **[Web console](docs/console.md)**                | The console every instance serves at `/ui` — what each tab answers, and where its numbers come from. |


### Tertiary

| Guide                                                | Description                                                                                       |
| :--------------------------------------------------- | :------------------------------------------------------------------------------------------------ |
| 🤖 **[MCP Server](docs/mcp.md)**                     | Give an LLM host (e.g. Claude, ChatGPT, Gemini, etc) memory tools via the Model Context Protocol.               |
| 🔌 **[Event Sourcing](docs/eventsource.md)**         | Bridge NATS, MQTT, RabbitMQ, Kafka, or the Bluesky firehose in, storing each message as a memory. |
| 🚦 **[Ingestor](docs/ingestor.md)**                  | Stage data at the edge and promote completed events into a central store under CEL rules.         |
| 💻 **[CLI](docs/cli.md)**                            | Drive a running service from the shell over either transport.                                     |
| 🧬 **[Clients & Codegen](docs/clients.md)**          | Generate a Python, TypeScript, or any-language client from the proto or OpenAPI document.         |
| 📓 **[Obsidian Integration](docs/obsidian.md)**      | Use Hippocampus as a memory layer for an Obsidian vault via the plugin or the MCP bridge.         |
