# Hippocampus

> **Hippocampus provides human-like memory for digital data. It uses events with linked memories, each of which have their own significance rating on an open scale, reinforced through recall, and a sleep cycle to shed memories that are no longer worth keeping. It is a lossy data store by design, meant for long-term storage.**

[![Coverage Status](https://coveralls.io/repos/github/fastbean-au/hippocampus/badge.svg?branch=main)](https://coveralls.io/github/fastbean-au/hippocampus)
![Dependabot](https://img.shields.io/badge/dependabot-enabled-brightgreen)
[![Known Vulnerabilities](https://snyk.io/test/github/fastbean-au/hippocampus/badge.svg)](https://snyk.io/test/github/fastbean-au/hippocampus)
[![Go Reference](https://pkg.go.dev/badge/github.com/fastbean-au/hippocampus.svg)](https://pkg.go.dev/github.com/fastbean-au/hippocampus)
![GitHub go.mod Go version](https://img.shields.io/github/go-mod/go-version/fastbean-au/hippocampus)

<img src="docs/go-hippocampus.png" width="400" height="200" align="left" alt="Go gopher riding a seahorse" />

<br clear="left" />

## 🔭 See it running — [hippocampus-demo.com](https://hippocampus-demo.com)

## 📈 What forgetting costs you

A store that forgets is only worth having if what it keeps is what you turn out to need. That is
measurable, so it has been measured — by replaying an agent workload fitted to a real corpus into a
live instance and scoring the survivors against the standard cache-replacement baselines at the same
store size:

> **Every access-based policy is statistically indistinguishable from random** at retaining the
> memories that matter but are not touched often. LRU scores 20.2% against random's 19.9%; LFU
> manages 18.4%. Hippocampus scores 27.6% at the same store size, and **+11.1 points over LRU** at a
> larger one.

Importance is not in the access log, so a policy reading only the access log cannot see it. Method,
baselines, the checks that stop it being circular, and the limitations: **[Retention
quality](docs/retention.md)**.

---

## 📚 Documentation

The guides below cover the service itself; the components that ship _around_ it are in the second
table. If you are deciding rather than deploying, read **[Use cases & deployment
modes](docs/use-cases.md)** first — its _Worth knowing before you start_ section is the set of
properties that shape what you can build on this.

### Core

| Guide                                                | Description                                                                                          |
| :--------------------------------------------------- | :--------------------------------------------------------------------------------------------------- |
| 🎬 **[Getting Started](docs/getting-started.md)**    | Step-by-step build, initial config, and first gRPC/HTTP requests.                                    |
| 📐 **[Use Cases & Patterns](docs/use-cases.md)**     | Embedded vs. centralised topologies and data transfer strategies.                                    |
| 🧠 **[Memory Consolidation](docs/consolidation.md)** | Deep dive on decay algorithms, capacity targets, and summarisation.                                  |
| 🧙 **[Configuration wizard](docs/config-wizard.md)** | Build a config and its deployment artefacts in the browser, with a live forgetting preview.          |
| ⚙️ **[Configurability](docs/configuration.md)**      | Exhaustive key reference for TLS, auth, storage drivers, and listeners.                              |
| 🛠️ **[Operations & Deployment](docs/operations.md)** | Containers, Kubernetes, packages, sizing, backups, shutdown, and observability.                      |
| 🔒 **[Security](docs/security.md)**                  | What is off by default, auth and role tiers, hardening checklist, and where content can leave.       |
| 📈 **[Retention quality](docs/retention.md)**        | What a bounded store keeps, measured against the standard cache-replacement baselines.               |
| 📊 **[Performance Benchmarks](docs/performance.md)** | Throughput sweeps across SQLite, Postgres, and MySQL under heavy loads.                              |
| 🧪 **[Demonstrations](docs/demonstrations.md)**      | The hosted demo, plus worked scenarios using real-world data shapes and generators.                  |
| 🖥️ **[Web console](docs/console.md)**                | The console every instance serves at `/ui` — what each tab answers, and where its numbers come from. |

### Tertiary

| Guide                                           | Description                                                                                       |
| :---------------------------------------------- | :------------------------------------------------------------------------------------------------ |
| 🤖 **[MCP Server](docs/mcp.md)**                | Give an LLM host (e.g. Claude, ChatGPT, Gemini, etc) memory tools via the Model Context Protocol. |
| 🔌 **[Event Sourcing](docs/eventsource.md)**    | Bridge NATS, MQTT, RabbitMQ, Kafka, or the Bluesky firehose in, storing each message as a memory. |
| 🚦 **[Ingestor](docs/ingestor.md)**             | Stage data at the edge and promote completed events into a central store under CEL rules.         |
| 💻 **[CLI](docs/cli.md)**                       | Drive a running service from the shell over either transport.                                     |
| 🧬 **[Clients & Codegen](docs/clients.md)**     | Generate a Python, TypeScript, or any-language client from the proto or OpenAPI document.         |
| 📓 **[Obsidian Integration](docs/obsidian.md)** | Use Hippocampus as a memory layer for an Obsidian vault via the plugin or the MCP bridge.         |
