# Use cases & deployment modes

![Hippocampus](go-hippocampus.png)

## When Hippocampus fits

Hippocampus is for **long-term retention under a finite budget**, where you want to keep the most
significant information indefinitely but cannot (or do not want to) keep everything, and where a
fixed TTL is too blunt an instrument. Instead of "delete after N days", it keeps what matters based
on significance, age, how often something is recalled, and how it relates to other records — and
forgets the rest, most-worthless-first, to stay within a capacity bound. When a _minimum_ guarantee
is also needed — keep everything for at least N days no matter what — an optional
[retention floor](consolidation.md#minimum-retention) provides it, overriding capacity so recent
data is never reaped early.

It suits data that has a long tail of value: most of it becomes irrelevant quickly, a fraction stays
important for a long time, and which is which is not known up front. Some shapes that fit:

- **Operational / audit event history** — keep every deploy, incident, and config change while they
  matter; let routine, low-significance chatter decay; reinforce (recall) the records an
  investigation touches so they survive. `group` labels scope events to a system or team. Where a
  compliance window applies, a retention floor guarantees nothing is dropped before it elapses.
- **Agent / assistant memory** — a bounded long-term memory for an LLM agent: store observations as
  memories, group them into events, recall the relevant ones on each interaction (which reinforces
  them), and let the unused ones fade. Summarization condenses a pile of related-but-quiet memories
  into a single "gist" memory rather than dropping the detail outright.
- **Per-device / edge telemetry** — retain a device's own significant history locally within a fixed
  storage budget, and periodically transfer it to a central store.

It is **not** a general-purpose database, a cache, or a system of record for data you must never
lose: forgetting is the point, and the service has no visibility into memory _content_ (bodies are
opaque blobs — the caller supplies any summary text).

## Deployment modes

### Embedded / edge / IoT (SQLite)

A single statically-linked binary plus one SQLite file, no external dependencies. Runs on-device or
alongside an application. Immediate durability (WAL), app-driven compaction, and both the byte
capacity target and the WAL-triggered checkpoint are available to bound on-disk size under bursty
writes. This is the default (`storage.driver: sqlite`).

Ideal where each producer keeps its _own_ bounded memory — one instance per device or per process.

### Centralised / corporate (Postgres or MySQL, optional OpenSearch)

A server-backed deployment: `storage.driver: postgres` or `mysql`, typically behind TLS with
authentication, exposing both gRPC and the HTTP/JSON gateway for clients that would rather not speak
gRPC. Add the optional OpenSearch secondary index (`opensearch.enabled`) for content search over
memory bodies (`SearchMemories`) — the primary store stays authoritative and the index is
best-effort and rebuildable. See the [Operations guide](operations.md) for driver selection and
sizing (notably the MySQL InnoDB buffer-pool note).

Provided compose stacks: `docker/docker-compose.postgres.yaml`, `docker/docker-compose.mysql.yaml`, and
`docker/docker-compose.opensearch.yaml`.

### Instance per tenant / subsystem

Because decay, capacity pressure, and eviction are **global** dynamics over a store, tenancy is _not_
built into the service — one noisy tenant sharing a store would make everyone else forget faster.
Instead, run **one instance per tenant** (or per subsystem, per environment). Containerization makes
one container + one SQLite volume (or one Postgres database) per tenant trivial, and it gives perfect
isolation of the memory dynamics, per-tenant capacity/decay tuning, and clean per-tenant deletion
(drop the volume). This is also horizontal scaling by sharding, without leader election.

## The embedded → centralised topology

The transfer/archive RPCs exist for a common pattern: many **embedded** instances (edge/IoT) that
periodically ship their accumulated memories to a **centralised** instance for aggregation and
longer retention.

- On the edge: `Export` (to S3) or `Transfer` (direct gRPC to the central instance's `ImportBatch`),
  each capturing a point-in-time snapshot and, optionally, clearing exactly what it captured (records
  written or recalled mid-transfer survive to the next run).
- At the centre: `Import` (from S3) or the `ImportBatch` the direct transfer drives — full-state,
  idempotent by id, preserving timestamps, recall history, groups, summary flags, and relationships.

Because record ids compare byte-for-byte across all three drivers, the same records keep their
identity across the move — and the same path serves **driver migration** (e.g. export from SQLite,
import into Postgres). See the [Operations guide](operations.md#backup-restore-and-migration).

## Seeing it on real data

For worked examples that load recognisable data — a Dickens novel as a narrative, synthetic service
logs whose severity drives what survives — in both the embedded and centralised modes, see
[Demonstrations](demonstrations.md).
