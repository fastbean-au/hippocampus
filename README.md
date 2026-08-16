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

---

## ⚡ Quick start

The demo stack builds the service and a load generator and runs them against the embedded SQLite
driver, with the decay clock compressed so forgetting is visible in minutes. If a container runtime
is present it also starts OpenSearch and a Grafana/OTEL collector; without one it simply runs
without them.

```bash
git clone https://github.com/fastbean-au/hippocampus.git
cd hippocampus
./demo/run.sh
```

Then open the embedded web console at [`localhost:8080/ui`](http://localhost:8080/ui) — its **Now**
and **Decay** tabs show what the last cycle forgot and where each memory stands — and Grafana at
[`localhost:3000`](http://localhost:3000). The demo serves its HTTP/JSON gateway on `8080` and gRPC
on `8300`. `SEARCH=0` and `OBSERVABILITY=0` skip the OpenSearch and collector containers; see
[`demo/README.md`](demo/README.md).

For an instance of your own rather than a demo:

```bash
brew install fastbean-au/tap/hippocampus   # macOS/Linux; then `brew services start hippocampus`
docker compose up --build                  # embedded SQLite, database in a named volume
go run ./cmd/hippocampus --gateway-port 8080   # from a clone, on built-in defaults
```

_**[Getting started](docs/getting-started.md)** walks through a build, a minimal `config.json`, and
your first requests. **[Operations & deployment](docs/operations.md)** covers the Compose stacks,
the Kubernetes overlays, the `.deb`/`.rpm` packages, driver choice, tuning, and hardening._

---

## 🧩 Around the service

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

## 📚 Documentation

| Guide                                                | Description                                                                                       |
| :--------------------------------------------------- | :------------------------------------------------------------------------------------------------ |
| 🎬 **[Getting Started](docs/getting-started.md)**    | Step-by-step build, initial config, and first gRPC/HTTP requests.                                 |
| 📐 **[Use Cases & Patterns](docs/use-cases.md)**     | Embedded vs. centralised topologies and data transfer strategies.                                 |
| 🧠 **[Memory Consolidation](docs/consolidation.md)** | Deep dive on decay algorithms, capacity targets, and summarisation.                               |
| ⚙️ **[Configurability](docs/configuration.md)**      | Exhaustive key reference for TLS, auth, storage drivers, and listeners.                           |
| 🧙 **[Configuration wizard](docs/config-wizard.md)** | Build a config and its deployment artefacts in the browser, with a live forgetting preview.       |
| 🛠️ **[Operations & Deployment](docs/operations.md)** | Containers, Kubernetes, packages, sizing, backups, observability, and security hardening.         |
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

---

## 🔒 Security

Authentication (JWT bearer tokens, HMAC or RS256/JWKS against any OIDC provider), OIDC single
sign-on for the web console, per-RPC `reader`/`writer`/`admin` tiers, group-scoped tokens, rate
limiting, and TLS are all built in and enforced identically on gRPC and the HTTP gateway. Auth, TLS,
and rate limiting are **off by default** — turn them on for anything reachable beyond localhost.

_**[Operations · Security](docs/operations.md#security)** is the checklist; the key reference is
**[Configurability](docs/configuration.md)**._

---

## ⚠️ Worth knowing before you start

- **Forgetting is the point.** This is not a system of record for data you must never lose. Where a
  guarantee is needed, a [retention floor](docs/consolidation.md#minimum-retention) overrides even
  capacity pressure.
- **One consolidator per store.** Only one instance may run decay against a given store; it is
  enforced at startup on every driver. Replicas scale reads and writes around it. See the
  [deployment model](docs/operations.md#deployment-model-one-consolidating-instance-per-store).
- **Payloads are opaque.** The service does not read memory bodies, so summaries come from the
  client — unless you enable the optional embedded LLM
  ([Ollama](docs/consolidation.md#embedded-llm-ollama)).
- **Content search is a secondary index.** Primary reads are strictly consistent; the optional
  OpenSearch index is asynchronous and best-effort, though hits are always re-read from the primary
  store so stale entries drop out. The built-in SQLite index is maintained inside the write itself
  and is not subject to that. See [Content search](docs/configuration.md#content-search).
- **A shared store is a shared trust domain.** Group scoping is a _soft_ partition: records are
  scoped, but the decay dynamics stay store-global. Hard isolation is one instance per tenant — read
  [the trust boundary](docs/operations.md#group-scoping-and-the-trust-boundary) first.

---

## 📄 License

Distributed under the terms specified in the repository. See `LICENSE` for details.
