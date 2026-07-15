# Hippocampus

![Hippocampus](docs/go-hippocampus.png)

[![Coverage Status](https://coveralls.io/repos/github/fastbean-au/hippocampus/badge.svg?branch=main)](https://coveralls.io/github/fastbean-au/hippocampus?branch=main)
![Dependabot](https://img.shields.io/badge/dependabot-enabled-brightgreen)
[![Known Vulnerabilities](https://snyk.io/test/github/fastbean-au/hippocampus/badge.svg)](https://snyk.io/test/github/fastbean-au/hippocampus)
[![Go Reference](https://pkg.go.dev/badge/github.com/fastbean-au/hippocampus.svg)](https://pkg.go.dev/github.com/fastbean-au/hippocampus)
![GitHub go.mod Go version](https://img.shields.io/github/go-mod/go-version/fastbean-au/hippocampus)

- [Hippocampus](#hippocampus)
  - [Overview](#overview)
  - [Use case](#use-case)
  - [Current state](#current-state)
  - [Documentation](#documentation)
  - [Demo](#demo)
  - [Docker](#docker)
  - [Horizontal scaling](#horizontal-scaling)
  - [Future directions](#future-directions)
  - [Limitations](#limitations)

Reference and guides live under [`docs/`](docs/): [Getting started](docs/getting-started.md),
[Configurability](docs/configuration.md), [Memory consolidation](docs/consolidation.md),
[Operations & deployment](docs/operations.md), and [Use cases](docs/use-cases.md).

## Overview

Hippocampus is an information or memory storage system that works with finite storage to retain the most significant information based on the memories' significance, age, how often they are recalled, and how they relate to other memories.

This service attempts to *somewhat* emulate the workings of human memory, which is to say that the memory is finite, and over time details are lost except for more significant events (or, conversely, the more significant the event the more details will be retained); it does eschew the unreliable in terms of the inaccurate or more fallible nature of human memory. Sleep is used to preserve and consolidate memories. Recalling a memory reinforces it, making it harder to forget. Sleep can also surface events whose memories have piled up and gone quiet as candidates for summarization (see [Summarization](docs/consolidation.md#summarization)) — condensing many memories into one that carries the gist, echoing how human memory consolidates repeated or detailed episodic experience into a single semantic memory.

Memories may be associated with events and each memory and event has significance. Memories that are not associated with events may have a lower significance than those that are associated with an event even when those memories themselves have the same significance.

Events and memories can also carry an optional freeform `group` label (up to 128 characters — a system, subsystem, org unit, owner, whatever fits the deployment). It gives related events and memories shared context beyond event membership, and `GetEvents`, `GetMemories`, and `SearchMemories` accept a `group` to restrict results to one grouping. The label plays no part in consolidation, decay, or capacity decisions.

While the intention is to limit storage over the long-term, growth between sleep cycles is unbounded: a configurable capacity applies increasing pressure on the deletion threshold as the store fills, and an optional byte-based capacity target (see [Capacity target](docs/consolidation.md#capacity-target)) evicts the least valuable memories each sleep cycle to bound the store's size. Data is stored in an embedded SQLite database in WAL mode, so every acknowledged write is durable immediately; the sleep process compacts the database, returning the space freed by consolidation to the filesystem.

## Use case

Where long-term retention of data is desired but infinite storage is either not available or is undesirable and TTL does not provide fine enough control.

## Current state

This service is being hardened for production. It supports optional [JWT bearer-token authentication](docs/configuration.md#authentication) and [TLS](docs/configuration.md#tls), but token issuance is still a CLI-only, single-shared-secret mechanism with no per-client revocation or rotation — real multi-tenant credential management is still future work (see [Future directions](#future-directions)). There are also [Limitations](#limitations) which should be considered before using in a production environment.

## Documentation

The full documentation lives under [`docs/`](docs/):

- [Getting started](docs/getting-started.md) — build, a minimal configuration, and first requests
  over the HTTP/JSON gateway.
- [Configurability](docs/configuration.md) — the exhaustive reference for every configuration key
  (observability, HTTP gateway, authentication, TLS, storage drivers, content search, and the
  transfer/archive surface).
- [Memory consolidation](docs/consolidation.md) — the value model, the six deletion algorithms,
  the byte-capacity target, checkpoint-triggered eviction, and summarization.
- [Operations & deployment](docs/operations.md) — the deployment model (one consolidating instance,
  optional read/write replicas) and its lock keepalive,
  choosing and sizing a storage driver (including MySQL InnoDB buffer-pool tuning), capacity tuning,
  backup/restore/migration, graceful shutdown, observability, and security.
- [Use cases & deployment modes](docs/use-cases.md) — embedded/edge vs. centralised topologies and
  the embedded→centralised transfer pattern.

## Demo

To see the service under sustained, realistic load, run `./demo/run.sh`. It builds and launches the service together with a load generator that stores bursty, slow, and event-less memories, queries and recalls them, and exercises every RPC, capped at 1 GiB of on-disk data. See [demo/README.md](demo/README.md) for details.

## Docker

`Dockerfile` builds a small Alpine-based image (all three storage drivers are pure Go, so the binary
is statically compiled with CGO disabled). The image bakes in `docker/config.sqlite.json`, runs
as a non-root user, exposes 50051 (gRPC) and 8080 (HTTP gateway), and health-checks itself against
the gateway's `/healthz`.

One compose file per storage driver:

- `docker compose up --build` — embedded SQLite, with the database file persisted in a named
  volume mounted at `/data`.
- `docker compose -f docker/docker-compose.postgres.yaml up --build` — PostgreSQL, mounting
  `docker/config.postgres.json` over the baked-in config. The hippocampus container is stateless;
  persistence lives in the postgres service's volume, and startup waits on its health check.
- `docker compose -f docker/docker-compose.mysql.yaml up --build` — MySQL, the same shape with
  `docker/config.mysql.json` and a `mysql:8.4` service.

To run with different settings, mount your own config over `/etc/hippocampus/config.json` the
same way the postgres compose file does.

## Horizontal scaling

The primary way to scale Hippocampus is **one instance per store** — per tenant, subsystem, or
device. Decay, capacity pressure, and eviction are global dynamics over a store, so an instance owns
its store's memory dynamics entirely; running many small instances (each its own SQLite file or
PostgreSQL/MySQL database) shards the load cleanly with no coordination. This is the recommended
model and the one the [containerization](#docker) and [transfer/archive](docs/configuration.md#transfer-and-archive)
surfaces are built around.

For the remaining case — a **single store too large for one instance** — a PostgreSQL or MySQL
database can be shared by several instances that split the work by role:

- **One consolidating instance.** Started with `consolidation.enabled: true` (the default), it holds
  the single-consolidator lock and runs every sleep cycle (consolidation, eviction, summarization).
  Only one such instance may run against a given database; a second refuses to start, as before.
- **Any number of read/write replicas.** Started with `consolidation.enabled: false`, each opens the
  shared database *without* the lock and serves the full RPC/HTTP surface — create, recall, query,
  search, import — but never runs a sleep cycle, and rejects the manual `Sleep` RPC with
  `FailedPrecondition`. Because forgetting is driven solely by the one consolidating instance, the
  replicas cannot race it or each other over the global decay/eviction state.

Put a load balancer in front of the instances; direct writes and reads at any of them. Start the
consolidating instance first so it owns schema creation and any in-place migration. This is a
deliberately simple, statically-assigned split rather than dynamic leader election with automatic
failover: if the consolidating instance dies, promote a replica by restarting it with
`consolidation.enabled: true` (it will take the now-free lock). This mode requires the `postgres` or
`mysql` driver — SQLite is a single embedded file and cannot be shared between processes, so
`consolidation.enabled: false` there simply yields an instance that never consolidates (a startup
warning says so).

## Future directions

Delivered since this list was first written: a pluggable storage layer with SQLite, PostgreSQL, and
MySQL drivers; an optional OpenSearch secondary content-search index; and RS256/JWKS token
verification against an external identity provider (`auth.method: idp`). Still open:

- Per-client credential management — issuance, rotation, and revocation. Token issuance today is
  CLI-only from a single shared secret (`hmac`), or delegated to an identity provider (`idp`); there
  is no built-in per-client registry or revocation.
- An S3-backed *primary* store (the transfer/archive surface already covers export/import to S3).
- Dynamic leader election with automatic failover for the shared-database
  [horizontal-scaling](#horizontal-scaling) mode. Today the consolidating instance is assigned
  statically (`consolidation.enabled`); promoting a replica after a failure is a manual restart.

## Limitations

- **One consolidating instance per store.** Decay, capacity pressure, and eviction are global
  dynamics over a store, so exactly one instance may run consolidation against it at a time; it is
  enforced at startup (a second consolidating instance fails fast). Scale either by running one
  instance per store (per tenant, subsystem, or device), or — on the `postgres`/`mysql` drivers — by
  adding read/write replicas (`consolidation.enabled: false`) alongside the single consolidating
  instance. See [Horizontal scaling](#horizontal-scaling) and the
  [Operations guide](docs/operations.md).
- **No visibility into memory content.** Memory bodies are opaque to the service, so it cannot
  generate summaries itself — a client supplies the summary text for `ReplaceMemoriesWithSummary`.
- **Credential management is basic.** See Future directions above — no per-client revocation or
  rotation under `hmac`; use `idp` for provider-managed rotation.
