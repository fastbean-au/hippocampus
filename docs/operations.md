# Operations & deployment guide

![Hippocampus](go-hippocampus.png)

This guide covers running Hippocampus in production: the deployment model, choosing and sizing a
storage backend, capacity tuning, backup and migration, shutdown, observability, and security. For
the exhaustive list of configuration keys see
[Configurability](configuration.md#configurability); for a first run see
[Getting started](getting-started.md).

## Deployment model: one consolidating instance per store

Exactly one running process may run **consolidation** against a given store at a time, because
decay, capacity pressure, and eviction are global dynamics over the whole store. The primary scaling
model is therefore **one instance per store** (per tenant, per subsystem, per device) — not multiple
instances over one database. On the server drivers, that store can instead be shared by one
consolidating instance plus read/write replicas; see
[Horizontal scaling with replicas](#horizontal-scaling-with-replicas) below.

Single ownership is enforced at startup, and a second _consolidating_ instance pointed at the same
store fails fast:

| Driver     | Store                                    | Exclusion mechanism                                                      |
| ---------- | ---------------------------------------- | ------------------------------------------------------------------------ |
| `sqlite`   | one database file in `storage.directory` | an exclusive OS lock on `hippocampus.lock` held for the process lifetime |
| `postgres` | the database named in the DSN            | a session-scoped `pg_advisory_lock` held for the process lifetime        |
| `mysql`    | the schema named in the DSN              | a session-scoped `GET_LOCK` held for the process lifetime                |

### The SQLite storage lock

SQLite's WAL mode deliberately permits several processes to write one database file, so nothing in
the storage engine itself would stop a second instance from running its own decay and eviction
schedule against the same store. A file-backed `sqlite` open therefore takes an exclusive operating
system lock (`flock` on Linux/macOS, `LockFileEx` on Windows) on a `hippocampus.lock` file beside the
database, and a second process pointed at the same `storage.directory` refuses to start, naming the
holder:

```text
another hippocampus instance already holds the storage lock on './data'
(held by pid 41207 host arrakis since 2026-08-06T05:59:08Z) - the sqlite driver is
single-instance, so give this instance its own storage.directory or move the store to the
postgres/mysql driver, which supports one consolidating instance plus replicas
```

Four things are worth knowing about it:

- **There is no stale lock to clear.** The lock is held by the kernel for as long as the process has
  the file open, so it is released the instant that process exits — including a `SIGKILL`, an OOM
  kill, or a crash. The file is left behind and still names the dead process; that text is
  diagnostics for the error message above and never the lock itself, so it can be ignored (and is
  emptied on a clean shutdown). **Never delete `hippocampus.lock` to "release" a lock** — it does
  nothing, and it removes the diagnostics.
- **It applies to every `sqlite` instance, not only consolidating ones.** SQLite cannot be shared
  between instances at all; `consolidation.enabled: false` on this driver means only that the one
  instance never consolidates, not that a second may join it. Sharing one store between instances
  requires `postgres` or `mysql` — see [Horizontal scaling with
  replicas](#horizontal-scaling-with-replicas).
- **Read-only tooling is exempt.** `--backfill-search` against an OpenSearch index, and an operator's
  `sqlite3` shell, open the database read-only, take no lock, and are unaffected by one being held —
  which is what keeps them safe to run beside a live service. The `--backfill-search` rebuild of the
  _SQLite_ content index is the exception: it writes to the service's own database, so it opens
  read-write, takes the lock, and now fails loudly instead of writing underneath a running instance.
- **It guards a directory, not a host.** Two instances with different `storage.directory` values do
  not contend; see [Running more than one instance on one
  host](#running-more-than-one-instance-on-one-host). A network filesystem is a poor place for a
  SQLite store for reasons that predate this lock, but note that the lock is only as reliable as
  that filesystem's lock support.

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
- Each instance registers itself in the shared database every `topology.heartbeatSeconds`, so every
  instance can name its peers and report which of them holds the consolidator role
  (`hippo topology`, the console's **Deployment** tab). That is also what makes the failure mode
  this section's first paragraph invites — **every** instance started with
  `consolidation.enabled: false`, so nothing ever forgets and the store grows without bound — visible
  rather than silent: it is reported as a warning on the topology response and logged at `WARN`. The
  same registry reports the opposite fault, two instances claiming the role, which is what a broken
  lock or two tiers pointed at different databases looks like. See
  [Deployment topology](configuration.md#deployment-topology).
- Promoting a replica is therefore checkable: after the restart, `hippo topology` against any
  instance should name exactly one consolidator, and the promoted instance's own store node should
  report the lock as held by it.

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
below run the compiled binary directly; the containerised path is instead a
[Compose stack or Kubernetes overlay](#containers-and-kubernetes).

On macOS or Linux the quickest supervised setup is Homebrew — `brew install
fastbean-au/tap/hippocampus` then `brew services start hippocampus`, which generates and manages the
launchd/systemd definition for you (the manual equivalents below are for a non-Homebrew install, or
when you want the full systemd hardening). The tap also carries the two client binaries, which need
no supervision: `fastbean-au/tap/hippocampus-cli` (the [`hippo` CLI](cli.md)) and
`fastbean-au/tap/hippocampus-mcp` (the [MCP bridge](mcp.md)). The service formula installs a default
embedded-SQLite config that is preserved across upgrades; see the
[tap repo](https://github.com/fastbean-au/homebrew-tap).

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

## Containers and Kubernetes

### Docker Compose

The repository ships a Compose stack per storage driver, plus two that add the OpenSearch
content-search index. All of them build the image from the repo's multi-stage
[`Dockerfile`](../Dockerfile) (statically linked, CGO disabled, non-root) and expose `50051` (gRPC)
and `8080` (HTTP gateway). The per-stack configs sit beside each file in
[`deploy/compose/`](../deploy/compose/); the image's baked-in default is
`deploy/compose/config.sqlite.json`.

| Stack                                                               | Command                                                                              |
| ------------------------------------------------------------------- | ------------------------------------------------------------------------------------ |
| Embedded SQLite, database in a named volume (the default)           | `docker compose up --build`                                                          |
| PostgreSQL                                                          | `docker compose -f deploy/compose/docker-compose.postgres.yaml up --build`           |
| MySQL                                                               | `docker compose -f deploy/compose/docker-compose.mysql.yaml up --build`              |
| SQLite + OpenSearch content search (security disabled — demo only)  | `docker compose -f deploy/compose/docker-compose.opensearch.yaml up --build`         |
| The same with the OpenSearch security plugin (HTTPS + basic auth)   | `docker compose -f deploy/compose/docker-compose.opensearch-secured.yaml up --build` |
| PostgreSQL + OpenSearch + an OTEL collector (the "corporate" stack) | `docker compose -f deploy/compose/docker-compose.corporate.yaml up --build`          |

Three opt-in Compose profiles layer onto them, all off by default:

- **`observability`** — an all-in-one `grafana/otel-lgtm` service (Grafana on `:3000`, OTLP on
  `:4317`) with the [Hippocampus dashboard and alert rules](#alert-rules) provisioned in. The
  service's metrics/traces are wired to it by `HIPPOCAMPUS_OBSERVABILITY_*` env overrides gated on
  the same variable, so nothing is exported (and no export failure is logged) unless the collector
  is up: `OBSERVABILITY=true docker compose --profile observability up --build`.
- **`ollama`** — the embedded [LLM summariser](consolidation.md#embedded-llm-ollama) beside the
  service: `OLLAMA=true docker compose --profile ollama up --build`, then
  `docker compose exec ollama ollama pull llama3.2` once to fetch a model.
- **`mcp`** (SQLite stack only) — an [MCP-over-HTTP endpoint](mcp.md) on `:8090` that dials the
  service over the Compose network: `docker compose --profile mcp up --build`. It is
  unauthenticated, like the rest of that demo stack; the common local pattern is instead the stdio
  transport spawned by the MCP host.

These stacks are demonstration-shaped — no auth, no TLS, and a bundled database — so treat them as a
starting point rather than a deployment. For anything long-running, give the service a `restart:`
policy so a [fail-stop](#deployment-model-one-consolidating-instance-per-store) is followed by a
clean restart, and point the healthcheck at `/healthz`.

### Kubernetes

[`deploy/k8s/`](../deploy/k8s/) carries plain [Kustomize](https://kubectl.docs.kubernetes.io/references/kustomize/)
manifests — no Helm, nothing beyond `kubectl` — with one overlay per deployment model:

```sh
# Embedded SQLite: one StatefulSet (1 replica) + a PersistentVolumeClaim
kubectl apply -k deploy/k8s/overlays/sqlite

# Centralised: one consolidator Deployment + N replica Deployments over a shared PostgreSQL
kubectl apply -k deploy/k8s/overlays/postgres
```

Both build on a shared `base/` (namespace, a token-less `ServiceAccount`, and the client-facing
`Service`), take their `config.json` through a content-hashed `configMapGenerator` so an edit rolls
the pods, and receive secrets (the DSN, the signing key) and the consolidator/replica split as
`HIPPOCAMPUS_*` env overrides rather than baked into the ConfigMap. Pods run non-root with a
read-only root filesystem — the counterpart to the shipped systemd unit's sandbox — and probe
`/healthz` (liveness) and `/readyz` (readiness, database-aware).

The workload kinds differ for the reason the [deployment
model](#deployment-model-one-consolidating-instance-per-store) gives: the SQLite overlay is a
`StatefulSet` pinned at one replica because a second pod sharing the volume would fail on the
storage lock, while the Postgres overlay scales its replica Deployment freely and leaves
consolidation to the single consolidator. Neither overlay ships an `Ingress` — expose the ports
through whatever the cluster already uses. See [`deploy/k8s/README.md`](../deploy/k8s/README.md).

## Choosing a storage driver

Set `storage.driver` to `sqlite` (default), `postgres`, or `mysql`. All three are pure Go, so the
binary is statically linked with CGO disabled.

|                                    | SQLite                        | Postgres                        | MySQL (8.0.20+)             |
| ---------------------------------- | ----------------------------- | ------------------------------- | --------------------------- |
| Best for                           | embedded / edge / single-node | centralised, server-managed     | centralised, server-managed |
| Dependencies                       | none (one file)               | a Postgres server               | a MySQL server              |
| Durability                         | WAL mode, immediate           | server-managed                  | server-managed              |
| Instances per store                | one (lockfile)                | one consolidator + replicas     | one consolidator + replicas |
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
damped link contributions and the weighted recall count — the number the decay actually acts on),
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

### What was forgotten — the forgotten log

The dry run says what would go and the explanation says where a memory stands; both speak only
about memories that are still there. The **forgotten log** is the third and it is the only one that
can speak about a memory that is not: one record per memory a cycle actually deleted, kept in the
store beside the memories that survived.

It is optional and off by default:

```json
"consolidation": {
  "tombstones": {
    "enabled": true,
    "maxRows": 100000,
    "maxAgeInDays": 30
  }
}
```

```bash
hippo forgotten list                                     # the most recent page
hippo forgotten list --memory-id abc123                  # did this memory exist, and when did it go
hippo forgotten list --rule eviction --group ingest      # only what went to meet the byte capacity
hippo forgotten clear --before 2026-07-01T00:00:00Z      # or --all
```

Each record carries the memory's id, group, event, significance, stored size, the `value` the decay
algorithm gave it and the `threshold` it was measured against **at that moment**, the `rule` that
took it, its creation and recall timestamps, and when it went. The threshold is recorded per record
rather than inferred, because it moves with capacity pressure: without it a value from last month
means nothing today.

**Bodies are never kept.** A tombstone records that a memory was forgotten, not what it said; this
is not an undelete, and building one on top of it is a different feature entirely.

Four things to know before turning it on.

- **It records forgetting, not deletion.** The two decay paths write records; `Clear` (which
  deletes memories an `Export`/`Transfer` has already moved elsewhere) and the client-initiated
  deletes (`DeleteMemories`, `DeleteEvent --memories`, summary replacement) do not. Nothing was
  lost in those cases, and a log claiming otherwise would be worse than no log.
- **It is bounded, and the bounds matter.** The log lives in the store it describes, so an
  unbounded one would slowly consume the headroom that drives forgetting. `maxRows` and
  `maxAgeInDays` are applied at the end of every cycle and a record past either bound is trimmed;
  setting both to 0 removes the bounds, which is supported and warned about at startup. The log is
  excluded from the store's measured size, so it never raises capacity pressure or triggers
  eviction — but it does still occupy disk.
- **Turning it off does not delete anything.** Disabling stops the writing _and_ the trimming, so
  what was already recorded stays readable. Emptying the log is always an explicit request
  (`hippo forgotten clear`, `POST /v1/memories/forgotten/delete`) — a configuration change must
  never destroy a record somebody kept. The one exception is `Purge`, which empties everything by
  definition.
- **Recording is part of the delete.** A record that cannot be written fails that batch of
  deletions rather than letting the memories go unrecorded; the cycle reports the failure and the
  memories are reconsidered next time.

**Reading the log is `reader`-tier; emptying it is `admin`.** Both honour group scoping as a
predicate — a tombstone carries its memory's group — so a group-scoped token sees, and can clear,
only its own partition's losses. That is what separates the log from the dry run beside it, which is
refused to a scoped caller outright: the log speaks only about records that were already the
caller's, it carries no bodies, and the `value`/`threshold` pair it reports is what
`ExplainConsolidation` already serves at `reader` for the memories still here. Emptying it stays
`admin` because that is destructive, and destructive on an audit record rather than on data the
caller put there.

One case argues for raising the read back to `admin`, and is worth checking against your deployment:
the log outlives what it describes, so a long `maxAgeInDays` leaves a trace of groups, sizes and
timing that the live store would by then have discarded. If `reader` tokens go to clients you would
not trust with that history, keep the log short or move the RPC up a tier.

Two metrics come with it: `hippocampus.tombstones` (records held, measured each cycle) and
`hippocampus.tombstones.deleted` (records removed, by whether it was a manual clear or the caps).
What was forgotten and by which rule is already reported by `hippocampus.memories.consolidated` and
`hippocampus.memories.evicted`.

The console's **Decay** tab shows the log beside the dry run — the log to any caller, the dry run
and the log's Clear button to an administrator — and the **Now** tab carries a short feed of the
most recent losses.

## Seeing the deployment

`hippo topology`, and the console's **Deployment** tab, report the deployment as one instance
understands it: itself, the components it dials, how they relate, and the last known health of each.
The configuration is under
[Deployment topology](configuration.md#deployment-topology); what matters operationally is what the
answer does and does not cover.

**It is one instance's view, not a survey.** An instance knows itself, whatever it dials outward,
and — on a shared `postgres`/`mysql` store — the peers it finds registered there. Everything else in
a deployment connects _to_ it: the event-source bridges, the ingestor, MCP servers, `hippo` itself,
none of which it holds an address for. Every component reported therefore carries a **source**, and
a short list means nothing has been declared rather than nothing is running. This is deliberate: the
alternative is a registry of clients, and that makes a memory store into a control plane. Nothing in
this view can act on another component, and nothing is planned to.

**Declare the components that dial in.** The inbound half becomes visible by listing it under
`topology.components` (see [Deployment topology](configuration.md#deployment-topology)) — and it
costs nothing on the other side, because the bridges and the ingestor already serve the health
surface it probes. Their `/readyz` reports a per-dependency breakdown, so a declared bridge that
cannot reach its broker says so on the diagram, and one that cannot reach _this service_ says that
instead. Those two look identical from outside and have entirely different owners, which is most of
the value of declaring them at all.

An edge to a declared component points **inward**, because that is the direction the connection is
opened in: the bridge holds an address for the service, not the reverse. Every other edge in the
view runs outward.

**Clients that call are shown as they call.** Where authentication is on, a component that presents
a verified token is drawn from the `client_id` it authenticated with, sourced `observed`. This needs
no configuration and names real clients — but it is the weakest of the five sources, and
deliberately so:

- It carries **no health**, permanently. A call proves the client was alive at that instant, which
  is not what a probe proves: an idle client is not broken, and a client that polls while its real
  work has stopped is not healthy. Observed components therefore report as unchecked, with the
  moment of their last call beside them.
- It reports **nothing when authentication is off**, since a caller is identified by its token and
  by nothing else — never by a source address, which names a proxy or a pod that has since been
  replaced. The `self` component says so, so an empty inbound column is not left to be read as
  "nothing is calling".
- The set is **capped** (32) and the least recently seen entry is dropped when it fills; it is held
  in memory, so a restart clears it. Entries are never expired on a timer: a bridge whose last call
  was six hours ago is more useful on the diagram than an absence would be, because the absence is
  indistinguishable from a component nobody ever configured.

Declaring a component and observing it are complementary, and they combine: where a declared
component's name matches an observed `client_id`, the two are shown as **one** component carrying
both its health and its last call. That pair separates the case nothing else here can — a bridge
that is up and writing from a bridge that is up and has written nothing.

What it does answer, and answers well:

- **Which of these two addresses is the consolidator.** The `self` component reports its role, and
  on the server drivers the store reports which side of the single-consolidator lock this instance
  is on — and names every instance sharing it, so the answer holds whichever one you asked.
- **Is anything consolidating at all.** A shared store whose instances all came up as replicas
  forgets nothing and grows without bound while every instance reports itself healthy. It is
  reported here as a **warning**, above the components, because the fault is the absent instance and
  there is no component to colour red. So is the reverse — two instances claiming the role — which is
  what a broken lock, or two tiers pointed at different databases, looks like from inside.
- **Which instances are running what.** Each peer reports its version and whether it has the search
  index, the summariser, the embedder and the gateway — so a replica that came up missing one is
  visible as such, which it otherwise is not: it behaves exactly as its own configuration says it
  should.
- **Is a dependency quietly broken.** A failed OpenSearch write is best-effort and logged; a missing
  Ollama model fails nothing until the first summarisation; an unreachable S3 bucket surfaces on the
  first `Export`. All three show here as a status with the reason beside it, before anyone runs the
  operation that would otherwise discover them.
- **Why is an optional feature doing nothing.** Components that are switched off are listed with the
  config key that would enable them (`hippo topology --all`, or the console's toggle).
- **Is a declared bridge writing, and if not, whose problem is it.** A degraded component names the
  end it cannot reach; an unreachable one is not answering at all; a `404` is a `healthUrl` that is
  wrong rather than a component that is down.
- **Who is actually calling this instance, and who is being refused.** An observed component lists
  the roles its token carried and whether it is group-scoped. A client calling constantly and being
  refused constantly looks exactly like a healthy one everywhere else; here its roles row says
  `none`, which is what a token resolving to no known tier gets.

Three cautions. Statuses come from a **background prober**, so every one is a snapshot and each
component reports when it was last checked — `unreachable` means "when last asked", not "now". Two
components are never probed at all, for reasons given in the configuration guide: the OTLP
collector, and the identity provider. They report as unchecked rather than as healthy. And a peer is
not probed either — this instance holds no address it could dial one on. What its row proves is that
it was writing to the shared store recently, which is the property that actually matters about a
peer: one that has stopped writing has stopped serving from here whether or not its port answers.

## Backup, restore, and migration

Two complementary approaches:

- **Standard backups.** For SQLite, the database file in `storage.directory` is the store — copy it
  (ideally with the service stopped, or via SQLite's online backup). For Postgres/MySQL, use the
  server's normal backup tooling (`pg_dump`, `mysqldump`, snapshots).
- **The transfer/archive RPCs** (see the RPC mapping in the README): `Export` writes a gzip
  length-delimited-proto archive to S3; `Import` reads one back; `Transfer` streams the whole store
  directly into another instance's `ImportBatch`; `Clear` deletes exactly what a prior
  `Export`/`Transfer` captured. These preserve full state (timestamps, recall history, groups,
  summary flags, links) and are idempotent by id.

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
[Observability](configuration.md#observability)).

### Request metrics (RED)

Two instruments describe the request surface itself, and are what an SLO is built on:

- **`hippocampus.rpc.requests`** — a counter of RPCs served. Rate and error rate both come from it.
- **`hippocampus.rpc.duration`** — a histogram of server-side duration in seconds.

Both carry the same four low-cardinality attributes, so a rate and a latency quantile for the same
slice of traffic are filtered identically:

| Attribute   | Values                                | Notes                                                                                                                                    |
| ----------- | ------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------- |
| `transport` | `grpc`, `http`                        | The gateway is instrumented separately, since it calls the service directly and never runs the gRPC interceptor chain.                   |
| `rpc`       | the bare RPC name, e.g. `StoreMemory` | Named identically on both transports, so one query sums a call across them. `unknown` for a request rejected before routing.             |
| `code`      | a gRPC code name, or an HTTP status   | The transport's own code. The gateway maps several gRPC codes onto one status, so it reports the status rather than a guessed-back code. |
| `outcome`   | `ok`, `client_error`, `server_error`  | The uniform classification across transports — see below.                                                                                |

`outcome` is three-valued rather than a success flag on purpose. A client sending malformed or
unauthorised requests is not the service failing, so **alert on `outcome="server_error"`**; a rising
`client_error` rate is worth looking at but is usually a caller to talk to, not a page.

Two scoping decisions worth knowing:

- **Only the RPC surface is counted.** The health and readiness probes, the console and its
  front-channel config, the login endpoints, and the static OpenAPI document are excluded — a
  probe's steady tick in the denominator would make every error-rate query meaningless.
- **A request rejected before it could be routed** — a bad token, an oversized body, an unmatched
  path — is counted with `rpc="unknown"`. It is deliberately not named from its path: a path carries
  memory and event ids, and so unbounded metric cardinality.

A recovered panic is _not_ counted here (the request unwinds with no status to classify); it appears
as `hippocampus.panics_recovered` instead, which is the metric to alert on for that case.

### Alert rules

The alert set is shipped, not just described: `deploy/observability/prometheus-alerts.yaml` is a
Prometheus rule file covering the failures below, ready to load with `rule_files:`, lift into a
prometheus-operator `PrometheusRule` `spec:`, or `mimirtool rules load`. Ten rules — server error
rate, request latency, sleep-cycle failures, no consolidator at all, capacity pressure, over
capacity, retention consuming the capacity target, rate-limit rejections, search-index drops, and
recovered panics — each with a `description` saying what to do about it and a `runbook_url` back
into this document.

Three things to know before deploying them, and the file repeats each at the rule it applies to:

- **The thresholds are starting points.** The 1% error ratio and the 1s p95 budget are whatever your
  SLO says they are; the capacity thresholds depend on `consolidation.capacityPressureExponent`.
- **A rule for a feature you have not configured stays silent rather than broken.** The capacity,
  retention, and search rules read metrics the service only publishes when the corresponding setting
  is on, and an expression over an absent metric returns nothing.
- **Every expression aggregates over the whole datasource**, which is right for the
  [one-instance-per-store model](#deployment-model-one-consolidating-instance-per-store). If one
  Prometheus holds several Hippocampus deployments, add `by (job)` to each.

The one rule that is _about_ absence is `HippocampusConsolidatorAbsent`, and it is how the
[instance-lock keepalive](#the-instance-lock-keepalive-server-drivers) exiting the consolidator
becomes visible: a process that has exited publishes nothing, so no counter of failures can catch
it, and the question has to be asked the other way round — has _any_ instance completed a cycle in
the last hour. Keep that window comfortably above `sleep.periodSeconds`. It asks in PromQL
(`absent_over_time`) rather than through an alerting engine's no-data policy, so Prometheus and
Grafana answer it identically.

The same ten rules are provisioned into the bundled Grafana below, so the demo stack alerts as well
as draws; see [deploy/observability/README.md](../deploy/observability/README.md). Neither file
provisions a contact point — where alerts should be delivered is deployment-specific.

### Domain metrics

Metrics worth alerting on in production:

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

### Client-side components

The [ingestor](ingestor.md#observability) and the [broker bridges](eventsource.md#observability) are
separate processes that dial a Hippocampus instance, and they are instrumented on the same model:
`--metrics` exports over OTLP/gRPC, and `--health-port` (**8090 by default**) serves `/healthz`
(liveness) and `/readyz` (whether the instance they write to can actually serve). Both surfaces are
documented in full on those pages; three things are worth knowing at the operations level.

- **`hippocampus.client.rpc.requests` / `.duration`** are the client-side RED metrics, emitted by
  every component that dials the service, with an `endpoint` attribute (`source`/`target` for the
  ingestor, `hippocampus` for a bridge). `outcome` is classified exactly as the service classifies
  its own, so "the error rate" means one thing on both sides of the connection.
- **`hippocampus.ingestor.seconds_since_last_pass` is the staleness signal.** Every other metric
  these components emit is a counter, and a counter that stops advancing is indistinguishable from a
  component with nothing to do; this one rises on its own whether the loop is stalled, deadlocked or
  dead. Alert on it rather than on the absence of promotions.
- **Tenancy is `--metrics-group`**, a per-process label stamped on both the resource and each metric
  (the duplication is what avoids a `target_info` join in every query). It is deliberately never read
  off the records: a bridge derives a memory's group from the message subject by default, so a
  per-record label would be unbounded.

Nothing here is wired into the shipped alert rules, which cover the service only.

For local viewing (evaluation, soak testing, dashboard work) every compose stack carries an
optional all-in-one `grafana/otel-lgtm` collector (Grafana + Prometheus + Tempo + Loki) behind a
compose `observability` profile — off by default, so a stack run without it never attempts an
export or logs a failure. Start it and tell the service to ship to it in one command:

```sh
OBSERVABILITY=true docker compose --profile observability up --build
```

Grafana is then at `http://localhost:3000`, opening on a pre-built **Hippocampus** dashboard
(provisioned from `deploy/compose/observability/`, set as the home page) that charts request rate,
error rate and latency (overall and per RPC), then ingest, forgetting (consolidation/eviction volume
and bytes reclaimed), capacity/used-bytes, and sleep-cycle duration from the metrics above. The alert
rules above are provisioned with it (as Grafana-managed rules, since Grafana cannot read a Prometheus
rule file), so firing rules appear under **Alerting → Alert rules**; nothing is notified, as the demo
stack has nowhere to send it. The demo soak harness has the same switch: `OBSERVABILITY=1 ./demo/run.sh`
launches the collector (via docker or podman) with the same dashboard and rules and points the
service at it — note that the demo deliberately runs its store at the capacity cap, so the capacity
alerts firing during a soak is them working. Metrics stay off unless the collector is present,
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

## Rate limiting

Off by default; [Rate limiting](configuration.md#rate-limiting) has the keys. Three levels of token
bucket — an instance-wide ceiling, one bucket per authorisation tier, one per caller — of which a
request must pass every level that has a rate. Both transports share the buckets, so a caller cannot
double its allowance by switching between gRPC and the gateway.

**What to set first.** The two that earn their keep on almost any exposed deployment are the global
ceiling and the per-client rule. The ceiling is a survivability bound: it is checked _before_ the
bearer token is verified, so it is the only level that can bound a flood of requests carrying junk
tokens (with an identity provider, that is an RS256 verify per request the service would otherwise
pay). The per-client rule is the fairness one, and is what stops a single misbehaving integration
consuming the ceiling and taking everyone else's service with it. Per-tier limits are the tuning
layer on top — worth setting once you know which class of caller is the noisy one, and best left
off `admin`, so an operator answering an incident is not queued behind a limit meant for
application traffic.

**Sizing.** Start from observed peak on `hippocampus.rpc.requests` and leave real headroom above it:
a global ceiling that bites refuses well-behaved callers along with the rest, which is the one
failure mode of this feature that looks like an outage. The per-client rule can be much tighter,
since it only ever affects the caller that exceeded it. Bursts matter more than they look: a client
that batches its writes needs a burst that covers a batch, or it will be throttled at a rate it is
nowhere near on average.

One consequence to hold in mind while sizing: a request the per-client rule refuses has already
spent a token from the global ceiling, because the ceiling is checked first and counts arrivals. A
single flooding client can therefore exhaust the instance's allowance even while every one of its
own requests is being refused — which is another way of saying the ceiling is a survivability bound
and not a fair-share allocator. (Between the tier and client levels, where both decisions are made
together, a refusal _is_ refunded.)

**Watching it.** `hippocampus.ratelimit.rejected` counts refusals by `transport` and by `scope`
(`global` / `tier` / `client`) — the scope is the whole diagnosis, since `client` is a caller to
talk to and `global` is a limit to raise or an instance to scale out. The shipped
`HippocampusRateLimitRejecting` alert fires on a sustained rejection rate.
`hippocampus.ratelimit.clients` reports how many callers currently hold a bucket; sitting at
`rateLimit.maxClients` means the table is churning — either the deployment has more callers than the
cap allows for, or something is minting identities — and a churning table hands each evicted caller
a fresh full bucket on its next request. Raise the cap.

**What is never limited**: `/healthz`, `/readyz`, the console and its front-channel config, the
login endpoints, and `/v1/openapi.json`. Throttling a readiness probe under load would get the
instance restarted rather than protected.

**Identity, and the proxy caveat.** A caller is its authenticated `client_id` (then the token's
`sub`, then its address). With authentication off, or for an unauthenticated request, the address is
all there is — per-host rather than per-application. Behind a reverse proxy every request arrives
from the proxy's address, which collapses every caller into one bucket; `rateLimit.trustForwardedFor`
is the fix, and is **off by default** because believing a caller-supplied header on a directly
reachable listener lets any caller mint itself an unlimited number of buckets. Set it only when a
proxy you control overwrites `X-Forwarded-For` rather than appending to it.
## Group scoping and the trust boundary

**A shared store is a shared trust domain by default.** `group` is a label, not an access control:
every token that can read can read everything until tokens are bound to groups. Binding them
([group scoping](configuration.md#group-scoping)) is a **soft** partition — records are scoped, but
the decay dynamics stay store-global, so one group's write volume still influences what another
forgets. Hard isolation is one instance per tenant.

Which of the two you need, and the four specific things soft partitioning does not give you, are in
**[Security · Group scoping and the trust boundary](security.md#group-scoping-and-the-trust-boundary)**.

## Security

Authentication, TLS and rate limiting are **off by default**, and nothing here turns itself on — so
any deployment reachable beyond localhost needs a deliberate pass over
**[the security guide](security.md)**. It covers authentication and key rotation, the role tiers,
group scoping, transport and gateway hardening, the console's boundary, where memory content can
leave the process, what the service does not do (no encryption at rest, no mutual TLS on the
listeners, no separate audit log), and a hardening checklist to work down.

Two operational notes belong here rather than there:

- [Rate limiting](#rate-limiting) is documented above, with its bucket hierarchy and tuning — the
  security guide only says to turn it on.
- [Seeing the deployment](#seeing-the-deployment) is where the topology view's redaction is
  described in context; the security guide records only that every address it reports is redacted.
