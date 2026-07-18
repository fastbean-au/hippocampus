# Operations & deployment guide

![Hippocampus](go-hippocampus.png)

This guide covers running Hippocampus in production: the deployment model, choosing and sizing a
storage backend, capacity tuning, backup and migration, shutdown, observability, and security. For
the exhaustive list of configuration keys see [Configurability](configuration.md#configurability) in the
README; for a first run see [Getting started](getting-started.md).

## Deployment model: one consolidating instance per store

Exactly one running process may run **consolidation** against a given store at a time, because
decay, capacity pressure, and eviction are global dynamics over the whole store. The primary scaling
model is therefore **one instance per store** (per tenant, per subsystem, per device) — not multiple
instances over one database. On the server drivers, that store can instead be shared by one
consolidating instance plus read/write replicas; see
[Horizontal scaling with replicas](#horizontal-scaling-with-replicas) below.

Single ownership is enforced at startup, and a second _consolidating_ instance pointed at the same
store fails fast:

| Driver     | Store                                    | Exclusion mechanism                                               |
| ---------- | ---------------------------------------- | ----------------------------------------------------------------- |
| `sqlite`   | one database file in `storage.directory` | single connection; the file is owned by the process               |
| `postgres` | the database named in the DSN            | a session-scoped `pg_advisory_lock` held for the process lifetime |
| `mysql`    | the schema named in the DSN              | a session-scoped `GET_LOCK` held for the process lifetime         |

### Horizontal scaling with replicas

When a single store is too large for one instance, a `postgres`/`mysql` database can be shared:
start exactly one instance with `consolidation.enabled: true` (the default — it takes the lock above
and runs every sleep cycle) and any number of additional instances with `consolidation.enabled:
false`. A replica opens the shared database **without** the lock, serves the full read/write RPC and
HTTP surface, and never consolidates (the manual `Sleep` RPC returns `FailedPrecondition`). Because
only the one consolidating instance forgets, replicas cannot race it over the global decay/eviction
state.

Operational notes:

- Start the consolidating instance first so it owns schema creation and any in-place migration; the
  replicas assume the schema already exists.
- Put a load balancer in front of all instances and route reads and writes to any of them.
- The assignment is static, not dynamic leader election. If the consolidating instance dies,
  **promote** a replica by restarting it with `consolidation.enabled: true` — it takes the now-free
  lock. Run every instance under a supervisor so a fail-stop is followed by a restart.
- SQLite cannot be shared, so `consolidation.enabled: false` there is not a scaling replica — it just
  yields an instance that never consolidates. Startup logs a warning to that effect.

### The instance-lock keepalive (server drivers)

The Postgres/MySQL lock lives on a dedicated pinned connection. Because both lock kinds are
session-scoped, anything that kills that session (a failover, a network reset, a connection-pooler
idle policy, MySQL's `wait_timeout`) would silently release the lock while the service kept running —
inviting a second instance to start and corrupt shared data. To prevent that, the service runs a
**keepalive**: every 60 seconds it pings the lock connection. The ping doubles as activity that keeps
the session from being reaped in the first place; if the session has died anyway, the service
attempts exactly one reacquisition and, if it cannot retake the lock, **exits immediately**
(`log.Fatal`) rather than run without it.

**Operational implication:** if a Postgres/MySQL-backed instance exits with a log line like
_"lost the single-instance lock and could not reacquire it"_, that is the safety mechanism working —
another instance holds the lock, or the database is unreachable. Investigate why the lock session
died (failover, network, an idle-timeout that outpaced the keepalive) and restart once the cause is
resolved. Run under a supervisor (systemd, Kubernetes, Docker restart policy) so a fail-stop is
followed by a clean restart.

## Choosing a storage driver

Set `storage.driver` to `sqlite` (default), `postgres`, or `mysql`. All three are pure Go, so the
binary is statically linked with CGO disabled.

|                                    | SQLite                        | Postgres                        | MySQL (8.0.20+)             |
| ---------------------------------- | ----------------------------- | ------------------------------- | --------------------------- |
| Best for                           | embedded / edge / single-node | centralised, server-managed     | centralised, server-managed |
| Dependencies                       | none (one file)               | a Postgres server               | a MySQL server              |
| Durability                         | WAL mode, immediate           | server-managed                  | server-managed              |
| `consolidation.capacityBytes`      | yes                           | yes                             | yes                         |
| `consolidation.walTriggerBytes`    | yes                           | rejected at startup             | rejected at startup         |
| On-disk footprint for large bodies | uncompressed                  | **TOAST-compressed** (smallest) | uncompressed (largest)      |

`walTriggerBytes` is SQLite-only — it measures SQLite's on-disk WAL file, which the server drivers
have no equivalent of; they reject the setting at startup rather than silently ignore it.

## Sizing and capacity tuning

### `capacityBytes` is measured on _uncompressed logical_ bytes

The byte-capacity target (`consolidation.capacityBytes`, with hysteresis floor
`consolidation.capacityBytesFloor` — see [Capacity target](consolidation.md#capacity-target)) is
compared against the store's **live logical size**, not the physical file size:

- **SQLite** — database pages excluding the freelist (the size the file would have after a full
  vacuum).
- **Postgres / MySQL** — an estimate summed from the live rows themselves: each row's payload
  (`octet_length`) plus a fixed per-row overhead. This is deliberately _not_ a file-size measure:
  neither server returns space to the filesystem after `DELETE` (they reuse it internally), so a
  file-size reading would plateau at its high-water mark and make eviction chase a figure that can
  never drop.

Two consequences for sizing:

1. `octet_length` counts **uncompressed** bytes. Postgres TOAST-compresses large values on disk, so
   its **physical** file can be much smaller than the logical figure eviction targets; MySQL/InnoDB
   stores bodies uncompressed, so its physical footprint is roughly the logical figure. For the same
   data, expect the MySQL on-disk footprint to be **several times** the Postgres one.
2. Budget disk and memory against the **physical** footprint of your chosen driver, but tune
   `capacityBytes` against the logical size.

### MySQL: size the InnoDB buffer pool to the working set

This is the single most important MySQL tuning knob for Hippocampus. In a soak test, the default
`innodb_buffer_pool_size` of **128 MB** against a ~**300 MB** on-disk dataset drove ~24 % of page
reads to disk and pushed read-query p95 latency from ~20 ms to ~500 ms — while writes stayed fast.
Raising the buffer pool to hold the working set restored read p95 to ~60 ms (in line with SQLite and
Postgres).

**Size `innodb_buffer_pool_size` at or above the physical dataset size.** Because InnoDB does not
compress, that physical size is close to the uncompressed logical size — so budget the buffer pool
against `capacityBytes` (plus index and overhead headroom), not against a Postgres-sized footprint.
It can be resized online (`SET GLOBAL innodb_buffer_pool_size = …`) without a restart.

### Postgres

Postgres needed no special tuning in the same soak: autovacuum kept pace with heavy delete churn, so
the physical database stayed bounded while eviction held the logical size at the target. Ensure
autovacuum is enabled (the default) and not throttled below the delete rate.

### Sleep cadence vs. write rate

`sleep.periodSeconds` sets how often consolidation and eviction run. Growth _between_ cycles is
unbounded, so under a high sustained write rate the store can overshoot `capacityBytes` before the
next cycle. Options: shorten `sleep.periodSeconds`, or (SQLite only) set
`consolidation.walTriggerBytes` to force an out-of-cycle checkpoint when the WAL outgrows a bound.
A non-positive `sleep.periodSeconds` disables timed cycles entirely — a supported mode for an
import-only or manually-driven instance.

If you set a [minimum retention](consolidation.md#minimum-retention) floor
(`consolidation.minimumRetentionInDays`), note that it **overrides** `capacityBytes`: eviction will
not delete data inside the retention window, so a retained working set larger than `capacityBytes`
holds the store above the target by design. Size `capacityBytes` (and the physical disk/buffer-pool
budget above) to fit `minimumRetentionInDays × peak write rate`, or the store can grow past the
capacity target until retained data ages out.

## Backup, restore, and migration

Two complementary approaches:

- **Standard backups.** For SQLite, the database file in `storage.directory` is the store — copy it
  (ideally with the service stopped, or via SQLite's online backup). For Postgres/MySQL, use the
  server's normal backup tooling (`pg_dump`, `mysqldump`, snapshots).
- **The transfer/archive RPCs** (see the RPC mapping in the README): `Export` writes a gzip
  length-delimited-proto archive to S3; `Import` reads one back; `Transfer` streams the whole store
  directly into another instance's `ImportBatch`; `Clear` deletes exactly what a prior
  `Export`/`Transfer` captured. These preserve full state (timestamps, recall history, groups,
  summary flags, relationships) and are idempotent by id.

**Driver migration** (e.g. SQLite → Postgres) uses the same path: `Export` from the source, `Import`
into a fresh target. Record ids compare byte-for-byte across all three drivers, so identity is
preserved across the move.

## Graceful shutdown

On `SIGINT`/`SIGTERM` the service shuts down in order: stop the HTTP gateway, drain in-flight gRPC
calls (`GracefulStop`, bounded to 10 s so a stuck call cannot hang shutdown — e.g. a long
`Export`/`Transfer` gets to finish), stop the background sleep loop and stats ticker (waiting for any
in-flight sleep cycle to drain), flush observability, then close the database — which releases the
Postgres/MySQL instance lock. A supervised restart can then start a fresh instance immediately.

## Observability

OpenTelemetry tracing and metrics are optional and exported over OTLP/gRPC (see
[Observability](configuration.md#observability)). Metrics worth alerting on in production:

- `hippocampus.capacity_pressure` and `hippocampus.used_bytes` — how full the store is; sustained
  high pressure means eviction is doing heavy work and the store is at its bound.
- `hippocampus.sleeps` (with the `success` attribute) and `hippocampus.sleep_duration` — a run of
  `success=false`, or a duration climbing toward `sleep.periodSeconds`, signals trouble.
- `hippocampus.memories_evicted` / `hippocampus.events_evicted` — eviction volume per cycle, with
  `hippocampus.bytes_evicted` the estimated bytes reclaimed (how much has been reaped).
- `hippocampus.memory_body_bytes` — a histogram of stored memory-body sizes (how much data each
  write carries); the sum tracks ingest volume and the distribution surfaces outlier blobs.
- The `hippocampus.memories.count` / `hippocampus.events.count` gauges — store growth.
- `hippocampus.panics_recovered` (by `transport`) — a gRPC or gateway handler panicked and was
  recovered (the request got `Internal`/`500` and the process survived); any non-zero value is a
  bug worth investigating.

For local viewing (evaluation, soak testing, dashboard work) every compose stack carries an
optional all-in-one `grafana/otel-lgtm` collector (Grafana + Prometheus + Tempo + Loki) behind a
compose `observability` profile — off by default, so a stack run without it never attempts an
export or logs a failure. Start it and tell the service to ship to it in one command:

```sh
OBSERVABILITY=true docker compose --profile observability up --build
```

Grafana is then at `http://localhost:3000`, opening on a pre-built **Hippocampus** dashboard
(provisioned from `docker/observability/`, set as the home page) that charts ingest, forgetting
(consolidation/eviction volume and bytes reclaimed), capacity/used-bytes, and sleep-cycle duration
from the metrics above. The demo soak harness has the same switch: `OBSERVABILITY=1 ./demo/run.sh`
launches the collector (via docker or podman) with the same dashboard and points the service at it. Metrics stay off unless the collector is present,
which is what keeps a plain run quiet — enabling export without a reachable OTLP endpoint is the
only thing that produces export-failure log lines.

Even with observability off, failing requests are visible in the logs at the default `info` level:
a failing RPC logs at Warn (Info for client-fault codes such as `NotFound`/`InvalidArgument`) with
the method, status code, duration, and error, and the HTTP gateway logs `5xx` responses at Warn
(all requests at Debug). Set `stats.intervalSeconds` (default 300; 0 disables) to control the
periodic event/memory count log line.

Identify the running build with `hippocampus --version`, the `version:` line at startup, or the
`version` field in the `GET /healthz` body — all report the module version plus the VCS
revision/time embedded at build time. When observability is on, the same version is the OTEL
`service.version` resource attribute.

Health surfaces are unauthenticated and always reachable: the gRPC `grpc.health.v1.Health` service
and the gateway's `GET /healthz` (**liveness** — process up, never touches the database) and
`GET /readyz` (**readiness** — also pings the store, `503` when it is unreachable, and mirrored by
the gRPC serving status). Point a restart/liveness probe at `/healthz` and a load-balancer/readiness
probe at `/readyz` — see [Health and readiness](configuration.md#health-and-readiness). On the server
drivers, also set `storage.queryTimeoutSeconds` (see below) so a hung database fails operations
promptly instead of tying up request goroutines and pooled connections.

### Bounding query time on the server drivers

`storage.queryTimeoutSeconds` (0 = off) bounds every statement and transaction. Leave it off for
embedded SQLite (a local file rarely hangs); set it on Postgres/MySQL, where a network partition,
storage stall, or lock pileup can otherwise block a request goroutine — and its pooled connection —
indefinitely, eventually wedging the instance. Size it **above** the longest legitimate operation:
the full-store consolidation scan is the tallest pole, so time a sleep cycle on a
representative store and leave generous headroom, or a cycle may be aborted mid-scan.

This server-owned bound is independent of, and complementary to, the caller's own context: an RPC's
deadline or a client that hangs up now propagates all the way to the database driver, so the
server-side work for an abandoned request is aborted rather than run to completion. Whichever bound
fires first — the client's deadline or `queryTimeoutSeconds` — ends the operation. The sleep cycle
is deliberately server-owned (it is not tied to the `Sleep` RPC's deadline), so a manual `Sleep`
call returning does not cut a consolidation short.

### Connection pool sizing (server drivers)

`storage.pool.maxOpenConns` (default 25) and `storage.pool.maxIdleConns` (0 → defaults to
`maxOpenConns`) cap the `database/sql` connection pool on the Postgres/MySQL drivers. Without a cap
`database/sql` opens unlimited connections, so a burst of concurrent RPCs can exhaust the server's
connection slots — one hot replica then starves every other instance, and the consolidator's
instance-lock keepalive, into `too many connections` errors. SQLite is single-connection and ignores
these settings.

Size the pool per instance, then check the fleet total: the **sum of `maxOpenConns` across every
instance** sharing the database must stay below the server's `max_connections`, with headroom for
the consolidator's pinned keepalive connection and any superuser/monitoring reserve. For example,
with Postgres's default `max_connections` of 100 and five instances, 25 each already reaches the
ceiling — lower `maxOpenConns` or raise `max_connections`. Keep `maxIdleConns` at (or near)
`maxOpenConns` so steady load reuses connections instead of churning them open and closed.

## Security

- **Authentication** (`auth.method`: `none` / `hmac` / `idp`) and **TLS** (`tls.enabled`) are both
  optional and off by default — see [Authentication](configuration.md#authentication) and
  [TLS](configuration.md#tls). Enable both for any deployment exposed beyond localhost.
- With `hmac`, tokens are minted by the `--mint-token` CLI. Signing secrets rotate without a flag
  day via `auth.signingKeys` (several `kid`-tagged secrets trusted at once, `auth.activeKid`
  selecting the one that signs), and individual tokens or clients are revoked ahead of their TTL by
  a polled `auth.revocationFile` (by `jti`, by `client_id`, or per-client before a cutoff timestamp)
  — see [Key rotation](configuration.md#key-rotation-hmac) and
  [Revocation](configuration.md#revocation). The revocation file also applies under `idp`, as a
  local override when the provider's own revocation lags; otherwise `idp` rotation and revocation
  are handled by the provider.
- If auth is enabled without TLS the service only warns — it assumes TLS is terminated upstream (a
  proxy or service mesh). Never send bearer tokens in plaintext. When `tls.enabled`, both listeners
  share one certificate and enforce a TLS 1.2 minimum.
- Under `hmac`, use a long random `auth.signingSecret` — at least 32 bytes; a shorter secret is
  brute-forceable and the service warns at startup.
- **Web console (`/ui`).** The HTTP gateway serves an embedded single-page console at `/ui`. The
  static page loads without a token (it carries none — the operator pastes the bearer token into it,
  which is then kept in the browser's `localStorage` and sent with each `/v1` call), but every action
  it performs still goes through auth and the purge gate like any other request. Because the token
  lives in the browser, serve `/ui` only over TLS and treat it as a trusted-operator tool, not a
  public endpoint; put it behind your ingress' access controls if the gateway is internet-facing.
- **Body-size limits on an exposed gateway.** `memory.limit.sizeBytes` caps a memory body; left
  unset there is no cap. The native gRPC transport bounds a whole request at its 4 MiB default, but
  the HTTP gateway does not by default — set `gateway.maxRequestBytes` to a transport-level ceiling
  (and/or `memory.limit.sizeBytes`) when the gateway is reachable by untrusted callers. Keep the
  ceiling above your largest legitimate `ImportBatch`/`Transfer` body.
