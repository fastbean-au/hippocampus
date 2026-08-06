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

## Running as a service

The binary is a foreground process: it runs until it receives `SIGINT`/`SIGTERM`, then shuts down
gracefully (see [Graceful shutdown](#graceful-shutdown)). For anything past a manual run, put it
under a process supervisor that restarts it on failure and starts it at boot — the same supervision
the sections above assume. Point the supervisor's liveness check at `GET /healthz`. The examples
below run the compiled binary directly; the containerised path is instead any of the
[Docker compose stacks](../README.md) with a `restart:` policy.

On macOS or Linux the quickest supervised setup is Homebrew — `brew install
fastbean-au/tap/hippocampus` then `brew services start hippocampus`, which generates and manages the
launchd/systemd definition for you (the manual equivalents below are for a non-Homebrew install, or
when you want the full systemd hardening).

### macOS (launchd)

The repo ships a ready-to-use per-user LaunchAgent and macOS config under
[`deploy/launchd/`](../deploy/launchd/) (see its README for the install/manage commands). The
annotated version below shows the shape:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>             <string>au.example.hippocampus</string>
    <key>ProgramArguments</key>
    <array>
        <string>/usr/local/bin/hippocampus</string>
        <string>-c</string>
        <string>/opt/hippocampus/config.json</string>
    </array>
    <key>RunAtLoad</key>         <true/>
    <key>KeepAlive</key>         <true/>
    <key>ProcessType</key>      <string>Background</string>
    <key>StandardOutPath</key>  <string>/opt/hippocampus/logs/out.log</string>
    <key>StandardErrorPath</key><string>/opt/hippocampus/logs/err.log</string>
</dict>
</plist>
```

`RunAtLoad` starts it at login and `KeepAlive` restarts it if it exits. Manage it with:

```sh
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/au.example.hippocampus.plist  # load + start
launchctl kickstart -k gui/$(id -u)/au.example.hippocampus                            # restart after upgrade/config change
launchctl bootout   gui/$(id -u) ~/Library/LaunchAgents/au.example.hippocampus.plist  # stop + unload
```

### Linux (systemd)

The release publishes `.deb`/`.rpm` packages (built from [`deploy/nfpm/`](../deploy/nfpm/)) that
install the binary, a hardened unit, and a default config in one step — the recommended path:

```sh
sudo dpkg -i hippocampus_<version>_amd64.deb      # Debian/Ubuntu
sudo rpm -i  hippocampus-<version>.x86_64.rpm     # RHEL/Fedora/SUSE

sudoedit /etc/hippocampus/config.json             # review before first start
sudo systemctl enable --now hippocampus
```

The package never auto-enables the service (you review the config first) and marks the config a
conffile, so your edits survive upgrades. The shipped unit
([`deploy/systemd/hippocampus.service`](../deploy/systemd/hippocampus.service)) runs under a
transient unprivileged user (`DynamicUser`, with `StateDirectory` owning `/var/lib/hippocampus`) and
carries the full sandbox — dropped capabilities, `NoNewPrivileges`, `ProtectSystem=strict`, private
tmp/devices, a `@system-service` syscall filter — the systemd counterpart to the k8s pod
`securityContext`. See [`deploy/systemd/README.md`](../deploy/systemd/README.md).

To install by hand instead, or on a distro without the packages, a minimal unit at
`/etc/systemd/system/hippocampus.service` is enough to get started (the shipped unit adds the
hardening above):

```ini
[Unit]
Description=Hippocampus
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=/usr/local/bin/hippocampus -c /etc/hippocampus/config.json
Restart=on-failure
RestartSec=5
User=hippocampus
Group=hippocampus

[Install]
WantedBy=multi-user.target
```

`systemctl enable --now hippocampus` starts it and enables it at boot; `systemctl restart
hippocampus` after an upgrade. Run it as a dedicated unprivileged user that owns
`storage.directory`. If the store is on a server driver or an external content-search cluster, order
the unit after that dependency (`After=`) so it does not start before its backing service is
reachable.

### Running more than one instance on one host

Several independent stores can run side by side on one machine — each is its own process owning its
own store (a demo instance beside a personal one, say). This is orthogonal to the
single-consolidator rule above, which concerns two instances sharing _one_ store; here each instance
owns a _separate_ store. Give **each its own `storage.directory` (SQLite) or DSN, its own `port`, and
its own `gateway.port`**: the defaults (`50051`/`8080`) collide, and the second instance to start
fails to bind the port. An external content-search cluster likewise needs a distinct
`opensearch.index` (or a separate cluster) per instance so their documents do not intermingle.

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

### `capacityBytes` is measured on _stored logical_ bytes

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

Three consequences for sizing:

1. The figure counts each body **as the service stored it**. With
   [`storage.compression.enabled`](configuration.md#body-compression) on — the default — a compressed
   body counts at its compressed size on all three drivers, so the saving shows up directly as
   headroom: the same `capacityBytes` holds several times more memories, and eviction starts later.
   Turning compression off makes the figure the body as the client sent it, which will make an
   existing store's used bytes appear to jump as new writes land uncompressed.
2. Postgres additionally TOAST-compresses large values on disk beneath `octet_length`, so its
   **physical** file can be smaller again than the figure eviction targets; MySQL/InnoDB does not, so
   its physical footprint is roughly the counted figure. For the same data with service-side
   compression off, expect the MySQL on-disk footprint to be **several times** the Postgres one.
3. Budget disk and memory against the **physical** footprint of your chosen driver, but tune
   `capacityBytes` against the counted size.

### MySQL: size the InnoDB buffer pool to the working set

This is the single most important MySQL tuning knob for Hippocampus. In a soak test, the default
`innodb_buffer_pool_size` of **128 MB** against a ~**300 MB** on-disk dataset drove ~24 % of page
reads to disk and pushed read-query p95 latency from ~20 ms to ~500 ms — while writes stayed fast.
Raising the buffer pool to hold the working set restored read p95 to ~60 ms (in line with SQLite and
Postgres).

**Size `innodb_buffer_pool_size` at or above the physical dataset size.** Because InnoDB does not
compress, that physical size is close to the counted logical size — so budget the buffer pool
against `capacityBytes` (plus index and overhead headroom), not against a Postgres-sized footprint.
It can be resized online (`SET GLOBAL innodb_buffer_pool_size = …`) without a restart.

[`storage.compression.enabled`](configuration.md#body-compression) is the other lever here, and the
reason it is on by default matters most on MySQL: it shrinks the bodies InnoDB stores, so the same
buffer pool covers a several-times-larger store. It is the closest MySQL gets to Postgres' TOAST
behaviour, trading a little CPU for I/O that is far more expensive here. Leave it on before reaching
for a bigger buffer pool.

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

### Previewing what would be forgotten

Tuning consolidation means choosing between six methods crossed with `aggressiveness`,
`deletionThreshold`, `unitsOfAgeInDays`, `capacityPressureExponent` and two significance weights —
and the only feedback used to be the data that had already gone. `PreviewConsolidation` closes that
loop: it reports what a cycle **would** forget, against your store and your configuration, and
deletes nothing.

```bash
hippo sleep --dry-run              # what would go
hippo sleep --dry-run --limit 500  # ...detailing more of it
```

It answers three questions:

- **How much.** `memories_consolidated`, `memories_evicted`, `events_deleted` and `bytes_freed` are
  always complete counts, whatever `--limit` is set to.
- **Which, and why.** A bounded sample of individual memories — id, group, event, significance, the
  computed `value`, the `threshold` it was compared against, and the `rule` that claimed it. The
  sample is ordered least-valuable-first, so a truncated one shows the memories furthest past the
  threshold rather than an arbitrary slice. `rule` separates the two paths, which have different
  answers: `CONSOLIDATION` means the memory decayed below the threshold (tune the decay settings),
  while `EVICTION` means it was still valuable enough to keep and went only because the store is
  over `capacityBytes` (raise the capacity, or store less).
- **Against what.** `capacity_pressure`, the scaled `deletion_threshold`, `used_bytes` and
  `capacity_bytes` — the inputs the decisions were made against, so the numbers can be read
  alongside the configuration that produced them.

**`memories_retained` / `retained_bytes` are the figures to watch** if you have set a retention
floor. They count what is currently inside the retention window and therefore exempt from _both_
paths. Because retention overrides the capacity target (above), `retained_bytes` approaching
`capacity_bytes` is the early warning that the target is becoming unreachable — eviction will run,
find nothing it is allowed to delete, and leave the store over its capacity. That is visible here
before it becomes a disk problem. The same pair is exported continuously as the
`hippocampus.retained_bytes` / `hippocampus.memories.retained`
[metrics](#observability), so you can alert on it rather than only discover it by asking.

Three properties worth knowing. The preview applies the cycle's own ordering — consolidation runs
first, so its memories are excluded from the eviction pool and their bytes are already reclaimed
before eviction is considered, and each memory therefore appears once under the rule that would
actually claim it. It deliberately does **not** join an in-flight cycle: a preview that attached
itself to a running `Sleep` would be describing a run that was at that moment deleting. The cost is
that a cycle can start while the preview scans, so a preview describes the store as it was — which
is why it reports the inputs it decided against.

And **concurrent previews share one scan.** Calls arriving while a preview is already running join
it rather than each starting a scan of their own, keyed on the sample size (so a caller asking for
more rows is never handed a shorter list). This matters most on the `sqlite` driver, whose
connection pool is a single connection by design: without it, a client polling the preview could
crowd out the sleep cycle's own queries. It cannot block a cycle from _starting_ — the preview
shares no lock with the cycle — but it does contend for that connection, bounded by
`storage.queryTimeoutSeconds`. One consequence: if the caller whose scan the others joined
disconnects, the callers waiting on it see that cancellation and should retry.

Bodies are never returned; a dry run reports what would be lost, and is not a way to read the
store. It is `admin`-tier for the same reason `Export` is: it enumerates ids, groups and
significances from across the whole store.

### Where a memory stands

A dry run answers "what goes next". `ExplainConsolidation` answers the other half — "where does
_this_ memory stand, and how long has it got" — for memories you already have in hand:

```bash
hippo memory explain --id abc123 --id def456
hippo memory explain --curve-significance 40        # no ids: just the curve and the current inputs
```

Per memory it reports the `value` the decay algorithm gives it, the `threshold` that value is
measured against, its `effective_significance` (its own significance plus its event's, the weighted
relationship significance and the weighted recall count — the number the decay actually acts on),
and `days_until_forgotten`: how long it has at today's threshold and pressure, assuming it is not
recalled again. Two flags override the value comparison and are reported separately, because a
memory far below the threshold that is nonetheless safe is otherwise baffling: `retained` (inside
`minimumRetentionInDays`, so neither path may take it) and `below_minimum_age` (younger than
`minimumAgeInDays`, so value-based consolidation is deferred).

With `--curve-significance` it also returns the **decay curve** for that significance — sampled from
the same code that makes the decisions, so a plot of it cannot drift from what the service will do —
along with the age at which the curve crosses the threshold. Left unbounded, the span is chosen to
show that crossing rather than an arbitrary window.

Unlike the preview it enumerates nothing, so it needs only the `reader` tier. Bodies are never
returned. The decision inputs it reports (capacity pressure, used bytes) are refreshed periodically
rather than per call — both cost a scan, and this is asked far more often — so they can lag a cycle
that has just finished by a few seconds. And, like the preview, it is refused on a replica
(`consolidation.enabled: false`), whose configuration is not the one its store is consolidated
under.

The embedded console's **Decay** tab is this RPC: a value column in the memory and search tables
(with what is due now, what is held by retention, and how long the rest have), the current capacity
pressure and threshold, and the plotted curve. Clicking a memory's value plots that memory's own
curve. For an administrator the same tab also runs the dry run above.

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
calls (`GracefulStop`, bounded by `shutdown.timeoutSeconds` — default 10 s — so a stuck call cannot
hang shutdown, e.g. a long `Export`/`Transfer` gets to finish), stop the background sleep loop and
stats ticker (waiting for any in-flight sleep cycle to drain), flush observability, then close the
database — which releases the Postgres/MySQL instance lock. A supervised restart can then start a
fresh instance immediately. `shutdown.timeoutSeconds` bounds each of the gateway-drain, gRPC
graceful-stop, and observability-flush phases; raise it for a slower store whose in-flight work
legitimately needs longer to finish.

## Observability

OpenTelemetry tracing and metrics are optional and exported over OTLP/gRPC (see
[Observability](configuration.md#observability)). Metrics worth alerting on in production:

- `hippocampus.capacity_pressure` and `hippocampus.used_bytes` — how full the store is; sustained
  high pressure means eviction is doing heavy work and the store is at its bound.
  `hippocampus.capacity_bytes` carries the configured target alongside it, so an alert can compare
  the two without hard-coding the limit in the query.
- **`hippocampus.retained_bytes` against `hippocampus.capacity_bytes` — the one to alert on if you
  set a retention floor.** Retention _overrides_ the capacity target, so once retained bytes reach
  the capacity the store cannot be brought back under it however hard eviction runs; eviction will
  keep running, find nothing it is allowed to delete, and the store will grow until the retained
  data ages out. `hippocampus.memories.retained` is the same measure as a count. Both are recorded
  once per sleep cycle, and **only when both `consolidation.minimumRetentionInDays` and
  `consolidation.capacityBytes` are set** — the pair costs one aggregate scan per cycle and means
  nothing without a capacity target to be measured against, so nothing else pays for it. The
  service also logs a warning when retained bytes reach the capacity, for deployments not running a
  metrics stack.
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
(provisioned from `deploy/compose/observability/`, set as the home page) that charts ingest, forgetting
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
drivers, review `storage.queryTimeoutSeconds` (see below) so a hung database fails operations
promptly instead of tying up request goroutines and pooled connections.

### Bounding query time on the server drivers

`storage.queryTimeoutSeconds` (default 60; 0 = off) bounds every statement and transaction. The
default is comfortably above a full consolidation scan at the benchmarked sizes, so it protects
against a network partition, storage stall, or lock pileup — any of which can otherwise block a
request goroutine, and its pooled connection, indefinitely, eventually wedging the instance — while
leaving normal operations untouched. Raise it above the longest legitimate operation on a larger
store: the full-store consolidation scan is the tallest pole, so time a sleep cycle on a
representative store and leave generous headroom, or a cycle may be aborted mid-scan. Set it to 0 to
disable the bound entirely (reasonable for embedded SQLite, where a local file rarely hangs).

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
  share one certificate and enforce a TLS 1.2 minimum. Behind such a sidecar/mesh, bind the
  listeners to loopback only with `bindAddress`/`gateway.bindAddress` (`127.0.0.1`) so nothing
  reaches them except the local proxy. The same applies to the **Transfer client**: setting
  `transfer.token` without `transfer.tls` sends the token in plaintext to the target, and the
  service warns at startup — enable `transfer.tls` unless TLS to the target is terminated by a mesh.
- **Scope each token to a role tier.** Every RPC requires a minimum tier — `reader`, `writer`, or
  `admin` (nesting, `reader ⊂ writer ⊂ admin`) — carried in the token's `roles` claim and enforced
  on both transports; see [Authorisation](configuration.md#authorisation). Issue `reader` tokens to
  read-only consumers and reserve `admin` (which alone may `Purge`/`Sleep`/`Clear`/`Transfer`/
  `Export`) for operators. `Import`/`ImportBatch` are `writer`-tier and still bypass write-path
  validation to restore archives faithfully, so grant `writer` only to trusted loaders. Authorisation
  is default-closed: a token whose roles resolve to no tier is denied everything, so **on upgrade,
  re-mint pre-existing tokens with a `--role`**. The verified `client_id` is logged on every failing
  request (and, on the HTTP gateway, every request), so a leaked or misbehaving token can be traced
  to the client it was issued to.
- **gRPC transport hardening.** If the gRPC port is exposed beyond trusted callers, cap the
  concurrent HTTP/2 streams one connection may open with `maxConcurrentStreams`, and enforce a
  keepalive policy (`keepalive.minTimeSeconds`, `keepalive.permitWithoutStream`) so an abusive
  client cannot ping the server into a resource spiral — see
  [HTTP gateway](configuration.md#http-gateway). Both default to grpc-go's own defaults.
- Under `hmac`, use a long random `auth.signingSecret` — at least 32 bytes; a shorter secret is
  brute-forceable and the service warns at startup.
- **Web console (`/ui`).** The HTTP gateway serves an embedded single-page console at `/ui`. The
  static page loads without a token (it carries none — the operator pastes the bearer token into it,
  which is then kept in the browser's `localStorage` and sent with each `/v1` call), but every action
  it performs still goes through auth, [authorisation](configuration.md#authorisation), and the purge
  gate like any other request. On the token you paste it calls `GET /v1/whoami` and adapts what it
  offers to the effective role — hiding the write controls for a `reader` and showing the role in the
  header — but that is a convenience only; the server still enforces the tier on every RPC, so a
  hidden control is not a security boundary. Because the token lives in the browser, serve `/ui` only
  over TLS and treat it as a trusted-operator tool, not a public endpoint; put it behind your ingress'
  access controls if the gateway is internet-facing.
- **Body-size limits on an exposed gateway.** `memory.limit.sizeBytes` caps a memory body; left
  unset there is no cap. The native gRPC transport bounds a whole request at its 4 MiB default, but
  the HTTP gateway does not by default — set `gateway.maxRequestBytes` to a transport-level ceiling
  (and/or `memory.limit.sizeBytes`) when the gateway is reachable by untrusted callers. Keep the
  ceiling above your largest legitimate `ImportBatch`/`Transfer` body.
- **Embedded LLM summariser (`ollama.enabled`).** Off by default; when on, the summariser is the one
  component that reads memory content, and it sends the text bodies of an event's memories to the
  configured Ollama server (`SummariseMemories`, and — with `ollama.autoSummarise` — the sleep
  cycle). Treat that as memory content leaving the process: run Ollama on a private network or the
  same host (`http://localhost:11434`), not a shared or third-party endpoint, and reach it over TLS
  if it is remote. `ollama.autoSummarise` rewrites stored memories automatically during sleep, so
  leave it off unless that behaviour is intended. See
  [Summarisation → Embedded LLM (Ollama)](consolidation.md#embedded-llm-ollama).
