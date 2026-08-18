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

Decay, recall reinforcement, and consolidation are slow by design — they play out over days. The
demo instances run the same build with the decay clock compressed, so the whole cycle happens in
minutes and you can watch it. Every console takes a read-only sign-in: **`demo` / `demo`**.

Start with [**the Bluesky console**](https://bluesky.hippocampus-demo.com/ui) — the only one running
on data nobody here controls. Headlines from verified news organisations arrive from a curated feed,
every one stored at the same significance; likes and reposts stream off the firehose to reinforce
them, replies are kept with them as a thread, related coverage is linked so a story survives as a
cluster, and what nobody came back to decays away. Engagement is the only differentiator, which
makes it the whole model on real attention.

Also: [**Book console**](https://book.hippocampus-demo.com/ui) — a novel re-read daily, episodic
detail consolidating into summaries as it ages · [**Logs
console**](https://logs.hippocampus-demo.com/ui) — a log trickle against a byte capacity target ·
[**Grafana**](https://grafana.hippocampus-demo.com) — live telemetry from the stacks ·
[**Config builder**](https://config-builder.hippocampus-demo.com) — build a config in the browser

_See **[Demonstrations](docs/demonstrations.md)** for what each one shows — and for loading the same
data into an instance of your own._

**Run the same thing locally** with `./demo/run.sh` from a clone: the service and a load generator
on embedded SQLite, decay clock compressed, console on `localhost:8080/ui`. Then
**[Getting started](docs/getting-started.md)** turns it into an instance you keep — install or
build, a minimal `config.json`, and your first requests.

---

## 📚 Documentation

The guides below cover the service itself; the components that ship _around_ it are in the second
table. If you are deciding rather than deploying, read **[Use cases & deployment
modes](docs/use-cases.md)** first — its _Worth knowing before you start_ section is the set of
properties that shape what you can build on this.

| Guide                                                | Description                                                                                       |
| :--------------------------------------------------- | :------------------------------------------------------------------------------------------------ |
| 🎬 **[Getting Started](docs/getting-started.md)**    | Step-by-step build, initial config, and first gRPC/HTTP requests.                                 |
| 📐 **[Use Cases & Patterns](docs/use-cases.md)**     | Embedded vs. centralised topologies and data transfer strategies.                                 |
| 🧠 **[Memory Consolidation](docs/consolidation.md)** | Deep dive on decay algorithms, capacity targets, and summarisation.                               |
| ⚙️ **[Configurability](docs/configuration.md)**      | Exhaustive key reference for TLS, auth, storage drivers, and listeners.                           |
| 🧙 **[Configuration wizard](docs/config-wizard.md)** | Build a config and its deployment artefacts in the browser, with a live forgetting preview.       |
| 🛠️ **[Operations & Deployment](docs/operations.md)** | Containers, Kubernetes, packages, sizing, backups, shutdown, and observability.                   |
| 🔒 **[Security](docs/security.md)**                  | What is off by default, auth and role tiers, hardening checklist, and where content can leave.    |
| 📊 **[Performance Benchmarks](docs/performance.md)** | Throughput sweeps across SQLite, Postgres, and MySQL under heavy loads.                           |
| 🧪 **[Demonstrations](docs/demonstrations.md)**      | The hosted demo, plus worked scenarios using real-world data shapes and generators.               |
| 🧬 **[Clients & Codegen](docs/clients.md)**          | Generate a Python, TypeScript, or any-language client from the proto or OpenAPI document.         |
| 💻 **[CLI](docs/cli.md)**                            | Drive a running service from the shell over either transport.                                     |
| 🤖 **[MCP Server](docs/mcp.md)**                     | Give an LLM host (Claude Desktop/Code) memory tools via the Model Context Protocol.               |
| 🔌 **[Event Sourcing](docs/eventsource.md)**         | Bridge NATS, MQTT, RabbitMQ, Kafka, or the Bluesky firehose in, storing each message as a memory. |
| 🚦 **[Ingestor](docs/ingestor.md)**                  | Stage data at the edge and promote completed events into a central store under CEL rules.         |
| 📓 **[Obsidian Integration](docs/obsidian.md)**      | Use Hippocampus as a memory layer for an Obsidian vault via the plugin or the MCP bridge.         |

What changed between releases — and what a version number does and does not promise — is in
**[CHANGELOG.md](CHANGELOG.md)**. Hippocampus is pre-1.0, so read the **Breaking** section of any
release you skip over.

### Around the service

None of these is part of the service. Each ships separately — its own module, binary, and image —
and talks to a running instance over its normal API, so adding one costs the service nothing. The
configuration wizard is the exception: it connects to nothing at all, and only writes files.

| Component                                       | What it does                                                                                                   | Guide                                                          |
| :---------------------------------------------- | :------------------------------------------------------------------------------------------------------------- | :------------------------------------------------------------- |
| **`hippo` CLI** (`integrations/cli`)            | The full RPC surface as noun-verb subcommands, over gRPC or the HTTP gateway.                                  | [cli.md](docs/cli.md)                                          |
| **MCP server** (`integrations/mcp`)             | Long-term memory for Claude Desktop/Code or any MCP host — a curated, non-destructive tool surface.            | [mcp.md](docs/mcp.md)                                          |
| **Configuration wizard** (`cmd/config-wizard`)  | Build a `config.json` and its deployment artefacts in the browser, charting the decay curve before you commit. | [config-wizard.md](docs/config-wizard.md)                      |
| **OTEL logs exporter** (`integrations/otel`)    | A collector exporter turning each log record into a memory — severity drives significance.                     | [collector walkthrough](integrations/otel/collector/README.md) |
| **Broker bridges** (`integrations/eventsource`) | NATS, MQTT, RabbitMQ, Kafka, and the Bluesky firehose, each message stored as a memory.                        | [eventsource.md](docs/eventsource.md)                          |
| **Ingestor** (`integrations/ingestor`)          | Stage data at the edge; promote completed events into a central store under CEL rules.                         | [ingestor.md](docs/ingestor.md)                                |
| **Obsidian plugin** (`integrations/obsidian`)   | A bounded, self-consolidating memory layer for a note vault.                                                   | [obsidian.md](docs/obsidian.md)                                |

---

## 🔒 Security

Authentication (JWT bearer tokens, HMAC or RS256/JWKS against any OIDC provider), OIDC single
sign-on for the web console, per-RPC `reader`/`writer`/`admin` tiers, group-scoped tokens, rate
limiting, and TLS are all built in and enforced identically on gRPC and the HTTP gateway.

**They are off by default**, and nothing turns itself on — the default install is an embedded store
on localhost with no dependencies, so a first run needs no token. Anything reachable beyond
localhost therefore needs a deliberate pass over the security guide, which also covers key rotation
and revocation, the console's actual boundary, where memory content can leave the process, and what
the service does not do (no encryption at rest, no mutual TLS on the listeners, no separate audit
log).

_**[Security](docs/security.md)** is the guide and its hardening checklist; the key reference is
**[Configurability](docs/configuration.md)**. To report a vulnerability, see
**[SECURITY.md](SECURITY.md)**._

---

## 📄 License

Distributed under the terms specified in the repository. See `LICENSE` for details.
