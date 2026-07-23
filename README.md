# Hippocampus

> **A finite, biological-inspired memory storage engine for log retention, audit trails, and context management.**

[![Coverage Status](https://coveralls.io/repos/github/fastbean-au/hippocampus/badge.svg?branch=main)](https://coveralls.io/github/fastbean-au/hippocampus)
![Dependabot](https://img.shields.io/badge/dependabot-enabled-brightgreen)
[![Known Vulnerabilities](https://snyk.io/test/github/fastbean-au/hippocampus/badge.svg)](https://snyk.io/test/github/fastbean-au/hippocampus)
[![Go Reference](https://pkg.go.dev/badge/github.com/fastbean-au/hippocampus.svg)](https://pkg.go.dev/github.com/fastbean-au/hippocampus)
![GitHub go.mod Go version](https://img.shields.io/github/go-mod/go-version/fastbean-au/hippocampus)

![Hippocampus Architecture](docs/go-hippocampus.png)

---

## 💡 Why Hippocampus?

Traditional storage engines rely on **Time-To-Live (TTL)** or fixed FIFO queues to manage bounded disk space. But age alone is a poor indicator of value: critical system anomalies, high-impact audit events, and frequently referenced context often get purged simply because they crossed an arbitrary time threshold.

Hippocampus applies principles from human memory consolidation to solve long-term data retention under finite capacity. Rather than indiscriminately truncating or expiring data, it continuously evaluates significance, access frequency, and relationships—retaining the **highest-value context** while gracefully degrading low-value noise.

* **Relative Significance & Ranking:** Insert events dynamically relative to adjacent records (`ABOVE`, `BELOW`, or `BETWEEN`) without enforcing rigid, static importance scales.
* **Reinforcement through Recall:** Accessing or querying a record strengthens its retention weight, protecting high-demand operational data from decay.
* **Sleep & Consolidation:** Runs periodic background consolidation cycles to apply decay models, compact space, and distill clusters of episodic details into compact semantic summaries.
* **Durable & Compliance-Safe:** Embedded or centralized deployment backed by SQLite (WAL mode), PostgreSQL, or MySQL. Includes configurable minimum retention floors to guarantee compliance windows regardless of storage pressure.

---

## ⚡ 30-Second Quick Start

Try Hippocampus locally with zero external dependencies (uses pure-Go embedded SQLite):

### 1. Run the Demo Stack
```bash
git clone https://github.com/fastbean-au/hippocampus.git
cd hippocampus
./demo/run.sh
```

### 2. Access the UI & Services
* **Embedded Web Console:** Open [`http://localhost:8080/ui`](http://localhost:8080/ui) to browse, search, and observe memory consolidation in real time.
* **gRPC Endpoint:** Listening on `localhost:50051`
* **HTTP Gateway:** Listening on `localhost:8080`
* **LGTM stack:** Listening on [`http://localhost:3000`](http://localhost:3000) to view live metrics in Grafana.

---

## 🚀 Docker Setup

Run Hippocampus in containerized environments with pre-configured compose files:

```bash
# Embedded SQLite (Stateless binary, volume-backed DB)
docker compose up --build

# PostgreSQL Backed
docker compose -f docker/docker-compose.postgres.yaml up --build

# Centralized Setup (PostgreSQL + OpenSearch Content Indexing)
docker compose -f docker/docker-compose.corporate.yaml up --build

# Add an MCP-over-HTTP endpoint to the embedded stack (opt-in profile, publishes :8090)
docker compose --profile mcp up --build
```

---

## 🏗️ Deployment Topology & Scaling

Hippocampus scales cleanly using two primary deployment patterns depending on store ownership:

```
[ Isolated Multi-Tenant / Embedded ]        [ High-Throughput Centralized ]

+----------------------+                  +-------------------------+
|  Tenant A / Device   |                  |   Consolidating Node    |
| (1 Instance = 1 DB)  |                  |  (Runs Sleep/Eviction)  |
+----------------------+                  +-----------+-------------+
                                                      |
+----------------------+                 +------------+--------------+
|  Tenant B / Device   |                 | Shared DB (Postgres/MySQL)|
| (1 Instance = 1 DB)  |                 +------------+--------------+
+----------------------+                              |
                                         +------------+--------------+
                                         |   Read / Write Replicas   |
                                         | (consolidation.enabled=f) |
                                         +---------------------------+
```

1. **One Instance per Store (Recommended):** Run independent, lightweight Hippocampus instances per subsystem, client tenant, or edge node using SQLite or dedicated databases.
2. **Shared Store with Replicas:** Scale centralized stores by running **one** consolidating instance (`consolidation.enabled: true`) alongside any number of stateless read/write HTTP/gRPC replicas (`consolidation.enabled: false`).

---

## 🤖 MCP Server — Memory for LLMs

Give an AI agent a long-term memory that forgets like a human one. `cmd/hippocampus-mcp` is a
[Model Context Protocol](https://modelcontextprotocol.io) server that exposes Hippocampus to
**Claude Desktop, Claude Code, or any MCP host** — a thin gRPC-client bridge, no extra service to run.

```bash
go build -o hippocampus-mcp ./cmd/hippocampus-mcp
claude mcp add hippocampus -- ./hippocampus-mcp --address localhost:50051
```

* **Curated, safe tools:** store, recall (reinforcing), search, and browse memories and events — destructive/admin RPCs are intentionally withheld, so an agent can't purge or exfiltrate a store.
* **stdio or streamable HTTP** transports; bearer-token auth and TLS mirror the service's own.

*See **[MCP Server guide](docs/mcp.md)** for the tool reference and host configuration.*

---

## 🪵 OpenTelemetry Log Ingestion

Feed real logs into Hippocampus through the standard OpenTelemetry Collector pipeline.
[`otel/hippocampusexporter`](otel/hippocampusexporter) is a collector **logs exporter** that turns
each log record into a memory: **severity drives significance**, so the decay cycle forgets routine
`DEBUG`/`INFO` noise first and keeps `ERROR`/`FATAL`. `service.name` becomes the `group`, and records
can be bucketed into events keyed by configurable attributes.

```bash
go install go.opentelemetry.io/collector/cmd/builder@v0.157.0
cd otel/collector && builder --config builder-config.yaml   # filelog/otlp → batch → hippocampus
./_build/hippocampus-otelcol --config config.yaml
```

*See the **[collector walkthrough](otel/collector/README.md)** and the
**[exporter configuration](otel/hippocampusexporter/README.md)**.*

---

## 📚 Documentation Index

Detailed operational and architectural guides live under [`docs/`](docs/):

| Guide | Description |
| :--- | :--- |
| 🎬 **[Getting Started](docs/getting-started.md)** | Step-by-step build, initial config, and first gRPC/HTTP requests. |
| ⚙️ **[Configurability](docs/configuration.md)** | Exhaustive key reference for TLS, auth, storage drivers, and listeners. |
| 🧠 **[Memory Consolidation](docs/consolidation.md)** | Deep dive on decay algorithms, capacity targets, and summarization. |
| 🛠️ **[Operations & Deployment](docs/operations.md)** | Sizing storage, PostgreSQL/MySQL tuning, backups, and security hardening. |
| 📊 **[Performance Benchmarks](docs/performance.md)** | Throughput sweeps across SQLite, Postgres, and MySQL under heavy loads. |
| 📐 **[Use Cases & Patterns](docs/use-cases.md)** | Embedded vs. centralized topologies and data transfer strategies. |
| 🧪 **[Demonstrations](docs/demonstrations.md)** | Worked scenarios using real-world data shapes and data generators. |
| 🤖 **[MCP Server](docs/mcp.md)** | Give an LLM host (Claude Desktop/Code) memory tools via the Model Context Protocol. |

---

## 🔒 Security & Hardening

Hippocampus is production-hardened out of the box:
* **Built-in Authentication:** JWT bearer tokens with mandatory expiration (`exp`) and zero-downtime rotation via `auth.signingKeys`.
* **Transport Security:** Pinned TLS 1.2+ floor for both internal and external communication.
* **Storage Isolation:** Driver error masking behind standard gRPC status codes to prevent database schema leaks.
* **Client Isolation:** Per-client request attribution, execution query timeouts, and stream concurrency limits.

*Read the [Security Section in Operations](docs/operations.md#security) for details on proxying behind sidecars, token revocation files, and network boundaries.*

---

## ⚠️ Key Limitations

* **Single Consolidator Rule:** Only one instance may perform consolidation/decay tasks per store to prevent race conditions during database compaction.
* **Opaque Payloads:** Memory payloads are stored as raw bytes; summaries must be constructed upstream by client applications and submitted via `ReplaceMemoriesWithSummary`.
* **Eventually Consistent Search:** The OpenSearch index is secondary and asynchronous. Primary database reads remain strictly consistent, while background sweeps handle reconciliation for content search.

---

## 📄 License

Distributed under the terms specified in the repository. See `LICENSE` for details.
