# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

- Build: `go build ./...`
- Run: `go run ./cmd/hippocampus -c config.json` (the `-c`/`--config_file` flag defaults to `./config.json`)
- Run the MCP server: `go run ./integrations/mcp --address localhost:50051` (a standalone MCP
  bridge that dials a running service; stdio by default, `--transport http` for streamable HTTP;
  see `docs/mcp.md`)
- Run an event-sourcing bridge (separate module — run from its directory):
  `cd integrations/eventsource && go run ./cmd/nats --subject 'events.>' --address localhost:50051`
  (one `cmd/<broker>` each for `nats`/`mqtt`/`rabbitmq`/`kafka`; consumes from the broker and stores
  each message as a memory; `go test ./...` in that dir, with `HIPPOCAMPUS_TEST_MQTT_BROKER`/
  `HIPPOCAMPUS_TEST_RABBITMQ_URL` set to run the broker integration tests; see `docs/eventsource.md`)
- Test: `go test ./...` (single test: `go test ./hippocampus -run TestName`)
- Benchmarks: `go test ./db -bench . -run XXX` (`db/bench_test.go`; run on demand — deliberately
  not CI-gated — and compare with benchstat when touching `hippocampus/sleep.go`, the db scans,
  or the schema; they pin that the consolidation scans never read memory bodies, eviction's
  scan+sort cost, and `UsedBytes` on all three drivers — the Postgres/MySQL ones need
  `HIPPOCAMPUS_TEST_POSTGRES_DSN`/`HIPPOCAMPUS_TEST_MYSQL_DSN`)
- Lint: `trunk check` (config in `.trunk/trunk.yaml`: golangci-lint, gofmt, markdownlint, etc.)
- Regenerate protobuf/gRPC/gateway code after editing `contract/hippocampus.proto`:
  `go generate ./contract` (the `//go:generate` directive lives in `contract/generate.go`)
  (requires `protoc` plus the `protoc-gen-go`, `protoc-gen-go-grpc`, `protoc-gen-grpc-gateway`,
  and `protoc-gen-openapiv2` plugins, all `go install`-able; the `google/api` proto dependencies
  the gateway needs are vendored under `contract/google/api/`)
- Demo/soak test: `./demo/run.sh` (builds and launches the service plus a load generator; see
  `demo/README.md`). By default it also launches a `grafana/otel-lgtm` collector (docker or
  podman) with the provisioned dashboard and ships metrics/traces to it (Grafana on `:3000`); set
  `OBSERVABILITY=0` to skip it. The env overrides are exported by `run.sh`, not baked into
  `demo/config.json`
- Docker: `docker compose up --build` (SQLite), `docker compose -f docker/docker-compose.postgres.yaml
up --build` (PostgreSQL), `docker compose -f docker/docker-compose.mysql.yaml up --build` (MySQL), or
  `docker compose -f docker/docker-compose.opensearch.yaml up --build` (SQLite + OpenSearch content
  search, security disabled — demo only) or `docker compose -f
  docker/docker-compose.opensearch-secured.yaml up --build` (the same with the OpenSearch security
  plugin enabled: HTTPS + basic auth, Hippocampus connecting over TLS via the `opensearch.tls`
  config block, credentials injected as `OPENSEARCH_ADMIN_PASSWORD`); container configs in
  `docker/`, image config baked from `docker/config.sqlite.json`. The `Dockerfile` is multi-stage:
  one build stage compiles both binaries, then an `mcp` stage (the `hippocampus-mcp` image) precedes
  the default `hippocampus` stage — the mcp stage is placed first so a no-`target` build still selects
  hippocampus, keeping every existing compose file unchanged. The event-sourcing broker bridges have
  their own `integrations/eventsource/Dockerfile` (parameterised by a `BROKER` build-arg, built from
  the repo root): `docker build -f integrations/eventsource/Dockerfile --build-arg BROKER=nats -t
  hippocampus-nats-bridge .` — the release publishes one image per broker to
  `ghcr.io/fastbean-au/hippocampus-<broker>-bridge`
- MCP-over-HTTP endpoint (SQLite compose only): `docker compose --profile mcp up --build` adds an
  opt-in `mcp` service (streamable-HTTP transport, `Dockerfile` `target: mcp`) that dials the
  `hippocampus` service over the compose network and publishes the MCP endpoint on `:8090`; off by
  default (behind the `mcp` profile), unauthenticated like the rest of that demo stack. The common
  local pattern is instead the stdio transport, spawned by the MCP host against the published
  `:50051` — no container. See `docs/mcp.md`
- Observability stack (any compose file): `OBSERVABILITY=true docker compose --profile observability
up --build` adds an all-in-one `grafana/otel-lgtm` service (Grafana `:3000`, OTLP `:4317`) behind a
  compose `observability` profile — off by default. The `hippocampus` service sets
  `HIPPOCAMPUS_OBSERVABILITY_*` env overrides (metrics/traces on from `${OBSERVABILITY:-false}`,
  endpoint `otel-lgtm:4317`), so metrics stay off (and never log an export failure) unless the
  collector is up. A Hippocampus overview dashboard (`docker/observability/`) is bind-mounted into
  Grafana's provisioning tree and set as the home page (`GF_DASHBOARDS_DEFAULT_HOME_DASHBOARD_PATH`)
- CI: `.github/workflows/ci.yaml` — build/vet/gofmt/tests (with postgres and mysql service
  containers so the `db/postgres_test.go` and `db/mysql_test.go` integration tests run instead
  of skipping) plus compose-stack smoke tests. Postgres/MySQL integration tests run locally with
  `HIPPOCAMPUS_TEST_POSTGRES_DSN=<dsn>`/`HIPPOCAMPUS_TEST_MYSQL_DSN=<dsn>` `go test ./db`
  against any disposable database
- Print the build version: `go run ./cmd/hippocampus --version` (module + VCS revision/time from
  `runtime/debug.ReadBuildInfo`; prints and exits before the config is read — see `version.go`)
- Mint an auth token: `go run ./cmd/hippocampus --mint-token --client-id <id> --ttl 24h -c config.json` (prints the token and exits; see [Authentication](docs/configuration.md#authentication))
- Backfill/rebuild the OpenSearch index: `go run ./cmd/hippocampus --backfill-search [--reindex] -c config.json`
  (CLI mode in `backfill.go`, exits when done; requires `opensearch.enabled`; safe beside a live
  instance; see [Backfill and reindex](docs/configuration.md#backfill-and-reindex))

## What this is

Hippocampus is a gRPC service that emulates human memory: finite storage where
less-significant data is forgotten over time. It stores **memories** (blobs with a significance and
timestamp) optionally linked to **events** (named time spans with their own significance). A
recurring **sleep** cycle consolidates (deletes) memories and events whose computed value falls
below a threshold, then persists the survivors to disk. Recalling a memory (`RecallMemories` RPC)
reinforces it: the decay clock resets and each recall raises its effective significance. The sleep
cycle can also identify events worth condensing into a single **summary** memory
(`GetSummarisationCandidates`); by default the service has no visibility into memory content, so a
client performs the actual replacement (`ReplaceMemoriesWithSummary`). An optional embedded LLM
(Ollama, `ollama.enabled`, off by default — the `summarise` package) lets the service author the
summary itself: the `SummariseMemories` RPC generates and replaces in one call, and
`ollama.autoSummarise` does it automatically for the scan's candidates during sleep. Every RPC is
also reachable as a
JSON/HTTP endpoint under `/v1` via an in-process grpc-gateway (`gateway.port`, 0 disables). Both
transports can require a signed JWT bearer token (`auth.method`: `none`/`hmac`/`idp`) and TLS
(`tls.enabled`); both are off by default.

## Architecture

- `cmd/hippocampus/` — the `package main` entrypoint (`main.go` plus `backfill.go`,
  `interceptors.go`, `logging.go`, `observability.go`, and the `webui.go`/`webui/` embedded
  console). `main.go` — bootstrap only: reads the JSON config file into viper, initialises logging
  (logrus, `logging.go`; `logging.level` selects severity — default `info` — and `logging.json`
  toggles JSON-vs-text output to stdout) and observability (`observability.go`: optional OTEL
  tracing/metrics over OTLP/gRPC,
  no-op when disabled), opens the DB, wires the gRPC server with interceptors (plus the
  `otelgrpc` stats handler when observability is enabled), starts stats, and on SIGINT/SIGTERM
  flushes observability then closes the DB. The build version (`version.go`,
  `runtime/debug.ReadBuildInfo`) is logged in the startup lines, returned in the `/healthz` body,
  and set as the OTEL `service.version` resource attribute; `--version` prints it and exits before
  the config file is read. `--mint-token` (with `--client-id`, `--ttl`,
  `--signing-secret`) is a separate CLI mode: it prints a signed `auth.MintToken` token to stdout
  and exits before the database, observability, or server are touched at all; it refuses under
  `auth.method: idp` (the IdP issues tokens there). `auth.method` selects the auth scheme —
  `none` (default), `hmac` (`auth.NewHMACVerifier` from `auth.signingSecret`), or `idp`
  (`auth.NewJWKSVerifier`: RS256 against an IdP's JWKS, from `auth.jwksUrl` or OIDC discovery
  via `auth.issuer`); the legacy boolean `auth.enabled` is a deprecated alias consulted only
  when `auth.method` is unset (`true` → `hmac` plus a warning). Whichever verifier is built,
  `auth.UnaryServerInterceptor` is prepended to the gRPC interceptor chain (ahead of
  `InterceptorBlockWhenPurgeInProgress`/`InterceptorLogger`, so unauthenticated requests are
  rejected before any other interceptor runs; on success it stashes the verified `auth.Claims` in
  the request context (`auth.ContextWithClaims`) so downstream interceptors can attribute the call.
  `InterceptorLogger` keeps its Trace entry/exit lines
  but also logs a failing RPC at Warn — Info for client-fault codes — so failures are visible at the
  default log level, adding a `client_id` field from the stashed claims when present; when `tls.enabled`,
  `credentials.NewServerTLSFromFile` is added via `grpc.Creds`. Auth without
  `tls.enabled` only logs a warning — TLS may be terminated upstream instead. Optional gRPC
  hardening server options are appended when their keys are positive: `maxRecvMsgBytes`
  (`grpc.MaxRecvMsgSize`), `maxConcurrentStreams` (`grpc.MaxConcurrentStreams`), and a keepalive
  enforcement policy from `keepalive.minTimeSeconds`/`keepalive.permitWithoutStream`
  (`grpc.KeepaliveEnforcementPolicy`); each defaults to grpc-go's own default when unset. Both
  listeners bind all interfaces unless `bindAddress` (gRPC) / `gateway.bindAddress` (HTTP) restrict
  the interface — e.g. `127.0.0.1` behind a TLS-terminating sidecar/mesh. When `gateway.port`
  is positive it also registers `contract.RegisterHippocampusHandlerServer` (the generated
  `hippocampus.pb.gw.go` reverse proxy) on a `runtime.NewServeMux()` and serves it over HTTP (TLS
  via `ListenAndServeTLS` when `tls.enabled`) — calling straight into the same `hipo` server
  instance, not dialing back over gRPC — alongside a static `/v1/openapi.json` (the embedded
  `contract.SwaggerJSON`) and an unauthenticated `/healthz`. Because the gateway calls `hipo`
  directly and never runs the gRPC interceptor chain, the mux is always wrapped in
  `hipo.HTTPMiddlewareBlockWhenPurgeInProgress` (the HTTP counterpart to
  `InterceptorBlockWhenPurgeInProgress`; open paths `/healthz` and `/v1/openapi.json`, else 503
  while a purge runs), which is in turn wrapped in `httpLoggingMiddleware` (the gateway's counterpart
  to `InterceptorLogger`, since the gateway never runs the gRPC chain — logs 5xx at Warn, else at
  Debug, via an intercepting status recorder, and adds the `client_id` from the request context
  when present); when `auth.enabled`, that is in turn wrapped in
  `auth.HTTPMiddleware` (outermost, so unauthenticated requests are rejected first — and, like the
  gRPC interceptor, stashing the verified claims on the request context on success) except
  `/healthz`. The gateway is shut down before the gRPC
  server on SIGINT/SIGTERM; `shutdown.timeoutSeconds` (default 10) bounds each phase (gateway drain,
  gRPC graceful stop, observability flush). All configuration flows through viper keys matching `config.json`
  structure. Instrumentation elsewhere uses the global OTEL providers
  (`hippocampus/telemetry.go`, `stats/stats.go`), so it stays no-op-safe whether or not
  observability is enabled. The domain metrics defined in `hippocampus/telemetry.go` (counters,
  the `sleep.duration` and `memory.body_bytes` histograms, and the `capacity_pressure`/`used_bytes`
  gauges) keep every attribute low-cardinality (bool or small enum), so it is safe to add attributes
  only within that constraint; an optional `grafana/otel-lgtm` compose profile with a provisioned
  dashboard (`docker/observability/`) exists for local viewing.
- `hippocampus/` — the gRPC service implementation (`Server` in `server.go`). Reads its config
  from viper once in `New()`. `sleep.go` holds the core consolidation logic:
  - `autoSleep` runs `sleep()` every `sleep.periodSeconds`; a manual `Sleep` RPC resets the timer
    via the `sleepReset` channel. A non-positive `sleep.periodSeconds` disables the timed cycle
    entirely (`sleepTimer` returns a nil channel, dropping that select case) — a supported mode for
    an instance driven only by the manual `Sleep` RPC or the WAL trigger; the manual RPC and WAL
    trigger keep working. When `consolidation.walTriggerBytes` is positive, `autoSleep` also polls
    the on-disk WAL file's size (`db.WALBytes`, a filesystem stat — no database connection needed)
    every `walCheckInterval` and runs an out-of-cycle sleep as soon as it's exceeded, so a
    checkpoint runs sooner than the next timed cycle under sustained high write rates. All three
    routes call `sleep()` through `sleepOnce`, which wraps it in a `singleflight.Group`
    (`Server.sleepGroup`) keyed on a constant, so a caller landing while a cycle is already running
    joins that in-flight call instead of starting a second, overlapping one.
  - `sleep()` = `consolidate()` (delete memories/events below threshold) +
    `scanSummarisationCandidates()` (when `consolidation.summarisationMinMemories` is positive, find
    events with at least that many memories that have all gone quiet — no creation or recall —
    for `summarisationMinAgeInDays`, cache up to `summarisationMaxCandidates` of them for
    `GetSummarisationCandidates` to serve; best-effort, never fails the cycle) + `evict()` (when
    `consolidation.capacityBytes` is positive and the store's used bytes still exceed it, delete
    memories in ascending value order until back at the eviction floor —
    `consolidation.capacityBytesFloor`, hysteresis headroom below the target; ignores
    `minimumAgeInDays` but honours `minimumRetentionInDays`, the hard retention floor that
    overrides the capacity target — retained memories are excluded from the eviction pool, and
    are still counted toward their event's total so a retained memory keeps its event alive) +
    `preserve()` (compact the database: incremental vacuum + WAL
    checkpoint). `consolidate()` runs three passes: memories without events, memories with
    events (deleting an event when its last memory goes), and events without memories.
  - `ReplaceMemoriesWithSummary` (in `memory.go`) deletes every memory for an event and inserts a
    single caller-supplied summary memory in their place, in one transaction; the summary is
    validated before anything is deleted. The new memory is flagged `is_summary` so it doesn't
    recount towards a future candidate scan until fresh, unsummarised memories accumulate again.
  - `ShouldConsolidateMemory` / `ShouldConsolidateEvent` (taking candidate structs defined in
    `db/db.go`) share `shouldConsolidate` / `calculateValue`, which implement the six
    configurable deletion algorithms (`consolidation.method` 1–6: power law, two linear variants,
    exponential half-life, logarithmic long-tail, and sigmoid consolidation-window) documented
    with value tables in `docs/consolidation.md`. The value combines memory/event significance, weighted relationship significance
    (`relationshipSignificanceWeight`), and a per-recall boost (`recallSignificanceWeight`); age is
    measured from the most recent recall. The deletion threshold is scaled each cycle by capacity
    pressure (the greater of row-count utilisation against `capacityMemories` and byte
    utilisation against `capacityBytes`, raised to `capacityPressureExponent`) so forgetting
    becomes more aggressive as the store fills. Both `shouldConsolidate` and eviction first
    short-circuit on `consolidation.minimumRetentionInDays` (via `retained()`): an item inside
    the retention window is never reaped by either path, whatever its value or the store's
    pressure — a hard floor distinct from `minimumAgeInDays`, which only defers value-based
    consolidation and is ignored by eviction. Memories without an event get a default event significance,
    either a fixed value or computed each sleep cycle from a percentile of existing event
    significances (`consolidation.defaultEventSignificancePercentile`, which overrides the fixed
    value when non-zero).
  - `Purge` deletes everything; while it runs, `InterceptorBlockWhenPurgeInProgress` (registered in
    main.go, `codes.Unavailable`) rejects all Hippocampus RPCs on gRPC, and its HTTP counterpart
    `HTTPMiddlewareBlockWhenPurgeInProgress` (503) rejects them on the gateway.
- `db/` — storage layer. One `DB` struct speaks three SQL dialects, selected by `storage.driver`
  (`sqlite`, the default, `postgres`, or `mysql`); nearly all query and consolidation logic is
  shared, with a `driver` field branching the genuinely divergent pieces (DDL, `?`-vs-`$N`
  placeholders via `rebind()`, `MAX(a,b)` vs `GREATEST`, upserts, and the
  compaction/size-accounting methods). The `db.Store` interface (in `db.go`) is what
  `hippocampus.Server` and `stats` depend on — the seam for future non-SQL backends. Every
  `db.Store` method that issues a query takes a leading `ctx context.Context` (all but `WALBytes`,
  a filesystem stat, and `Close`), so an RPC's deadline/cancellation reaches the driver; the db
  layer wraps that ctx with the server-owned `storage.queryTimeoutSeconds` bound (default 60; 0
  disables) in `opContext`, so whichever fires first ends the operation. The sleep cycle passes its
  own (tracing-span) context and stays server-owned, not tied to the `Sleep` RPC's deadline. SQLite
  (`modernc.org/sqlite`, pure Go): one database file (`hippocampus.db` in `storage.directory`)
  holding the `events` and `memories` tables; an empty directory (used by tests) selects an
  in-memory database. WAL mode makes every write durable as it happens — there is no snapshot
  cycle. The pool is capped at one connection, so queries must not be nested (collect rows,
  close, then act — the consolidation scans already work this way). Postgres (`jackc/pgx` via
  database/sql, `postgres.go`): when opened to consolidate (`NewPostgres(dsn, true)`) it takes a
  session-scoped advisory lock — the single-consolidator lock — on a dedicated pinned connection at
  startup so a second consolidating instance against the same database fails fast; opened with
  `consolidate` false it skips the lock and runs as a read/write replica (horizontal scaling).
  `UsedBytes`
  estimates live rows (payload `octet_length` + `evictionRowOverheadBytes` per row — deliberately
  NOT a file-size measure, which never shrinks after deletes on Postgres and would make eviction
  chase a figure that cannot drop; keep it the exact complement of `EvictMemories`' freed-bytes
  estimate); `walTriggerBytes` stays rejected in main.go (no client-visible WAL file) and
  `Preserve` is a no-op (autovacuum). MySQL (`go-sql-driver/mysql`, `mysql.go`, requires MySQL
  8.0.20+): same shape as Postgres — the instance lock is a schema-scoped `GET_LOCK` on a pinned
  connection, `UsedBytes` shares the live-row estimate (`usedBytesLiveRows`), `Preserve` is a
  no-op (InnoDB purge), `walTriggerBytes` rejected — plus its own genuinely divergent branches:
  upserts are `ON DUPLICATE KEY UPDATE` with the `AS new` row alias (no `ON CONFLICT`), recall
  reinforcement runs UPDATE-then-SELECT in one transaction (`recallMemoriesMySQL` — no
  `UPDATE ... RETURNING`), `CountMemories` uses the portable `COUNT(CASE ...)` (no `FILTER`),
  ids are `VARCHAR(255)` (MySQL can't index unbounded TEXT) `COLLATE utf8mb4_bin` (so `id`,
  `event_id`, and `group_name` compare byte-for-byte like SQLite/Postgres instead of under MySQL's
  case-/accent-insensitive server default, which would collide ids differing only in case;
  `setMySQLColumnCollationIfNeeded` migrates a pre-existing database in place via an
  `information_schema.columns` `COLLATION_NAME` probe), and the schema init probes
  `information_schema` for index/column existence (no `CREATE INDEX IF NOT EXISTS`/`ADD COLUMN
IF NOT EXISTS`). Postgres/MySQL integration tests in `postgres_test.go`/`mysql_test.go` skip
  unless `HIPPOCAMPUS_TEST_POSTGRES_DSN`/`HIPPOCAMPUS_TEST_MYSQL_DSN` point at a disposable
  database. A covering index over the memories consolidation columns lets the sleep-cycle scans
  avoid ever reading memory bodies. The `db.Server` interface (implemented by
  `hippocampus.Server`) inverts the dependency so the DB's consolidation scans can ask the server
  whether to delete a row. `initSchema` also runs `addColumnIfMissing` for columns added after a
  table's original `CREATE TABLE` (currently `memories.is_summary` and the `group_name` column
  on both tables — named `group_name` because `GROUP` is reserved in every dialect, surfaced as
  `group` in the API), so a database file written by an older version of the service is migrated
  in place on next startup (Postgres uses native `ADD COLUMN IF NOT EXISTS`; MySQL shares the
  probe with SQLite via `information_schema`).
- `contract/` — the gRPC contract (`hippocampus.proto`) and generated code. RPCs cover
  event/memory CRUD plus `Sleep`, `Purge`, `MergeEvents`, `RecallMemories`,
  `ReplaceMemoriesWithSummary`, `GetSummarisationCandidates`, `SummariseMemories` (the embedded-LLM
  generate-and-replace), `WhoAmI` (reports the caller's
  effective authorisation tier, so the web console can adapt), and the transfer/archive surface
  (`Export`, `Import`, `ImportBatch`, `Transfer`, `Clear`). Each RPC carries a
  `google.api.http` annotation mapping it onto a REST-ish `/v1/...` path (see
  [Configurability](docs/configuration.md#configurability) for the full mapping); `go generate
./contract` (directive in `generate.go`) turns those into `hippocampus.pb.gw.go` (the gateway)
  and `hippocampus.swagger.json` (the OpenAPI description, embedded via `swagger.go`).
  `contract/google/api/{annotations,http}.proto`
  are vendored copies of the googleapis definitions the annotations depend on.
- `search/` — the optional OpenSearch secondary content-search index (`opensearch.enabled`,
  off by default; `search.Index` interface with no-op and `opensearch-go/v4` implementations).
  Connection security: basic auth (`opensearch.username`/`password`, the password injectable via
  `HIPPOCAMPUS_OPENSEARCH_PASSWORD`) plus an optional `opensearch.tls` block for HTTPS clusters
  (`caCertFile` to trust a private/self-signed CA, `certFile`/`keyFile` for mutual TLS,
  `insecureSkipVerify` as a dev-only escape hatch) — `TLSConfig.build`/`buildTransport` in
  `search/opensearch.go` turn it into a cloned default transport with a `*tls.Config`, and a
  malformed block fails startup. Strictly secondary: all mutations propagate primary→index
  asynchronously (bounded queue, one
  FIFO worker — ordering matters for summarisation's delete-then-index; overflow drops, never
  blocks), and `SearchMemories` results are always re-read from the primary store so stale index
  entries drop out. The worker retries a transient cluster failure (bounded attempts with jittered
  backoff in `applyWithRetry`) before dropping an operation; its four timing constants
  (`applyTimeout`, `applyMaxAttempts`, `applyRetryBaseBackoff`, `closeDrainTimeout`) are package
  defaults, each overridable per instance via the matching `opensearch.apply*`/`closeDrain*`
  `search.Config` field (0 → the default), resolved onto the `OpenSearch` struct at construction.
  Consolidation/eviction deletes reach it
  via `db.SetMemoryDeleteObserver` (on the concrete `*db.DB`, not `db.Store`); RPC-layer hooks cover
  the rest. Binary memories are never indexed. Because propagation is best-effort, the index can
  still go sparse, so two recovery paths exist. The self-healing one is automatic: the consolidating
  instance runs a periodic reconciliation sweep (`hippocampus/reconcile.go`, gated on
  `consolidation.enabled` + a positive `opensearch.reconcileIntervalSeconds`, started/stopped alongside
  `autoSleep`) that pages the primary store via `db.GetMemoriesPage` and re-indexes non-binary
  memories through the normal async `IndexMemory`, healing missing documents (idempotent; heals
  missing docs only — stale-doc removal stays a `--reindex` job). The manual one is the
  `--backfill-search` CLI mode (`backfill.go`), which rebuilds it from the primary store via synchronous
  `IndexMemorySync`/`RecreateIndex` calls that bypass the queue (safe: the tool has no worker or
  live writes of its own) and `db.GetIndexableMemoriesPage` keyset pagination — with `--reindex`
  it recreates the index first to clear stale documents. Each driver opens read-only so the tool
  can run beside a live service: SQLite via `db.NewSQLiteReadOnly` (`mode=ro`, no `initSchema`,
  `Preserve` a no-op — so it never writes DDL or checkpoints the database the service owns),
  Postgres/MySQL via `db.NewPostgresReadOnly`/`NewMySQLReadOnly` (skipping the instance lock). Integration tests skip unless `HIPPOCAMPUS_TEST_OPENSEARCH_URL` is set;
  `docker/docker-compose.opensearch.yaml` runs the full stack.
- `summarise/` — the optional embedded-LLM summariser (`ollama.enabled`, off by default;
  `summarise.Summariser` interface with a no-op and an `Ollama` implementation). The `Ollama`
  impl is a small hand-rolled HTTP client to Ollama's `POST /api/generate` (`stream:false`, no new
  module dependency), bounding the prompt by body count and total characters and never sending
  binary bodies. It is the one component with visibility into memory content. Wired into
  `hippocampus.Server` via the `summarise.Summariser` field (nil-safe through `summariser()`, like
  `searchIdx()`): the `SummariseMemories` RPC reads an event's memories, generates a summary, and
  replaces them through the same `insertSummary` path `ReplaceMemoriesWithSummary` uses; the sleep
  cycle's `autoSummariseCandidates` (gated on `ollama.autoSummarise`, off by default) does the same
  for the scan's candidates, best-effort. All viper reads stay in main.go, which builds the no-op or
  `Ollama` from the `ollama.*` keys. An optional `ollama` compose profile ships it alongside the
  service. Deliberately off the MCP tool surface (it deletes memories, like the omitted
  `ReplaceMemoriesWithSummary`).
- `archive/` — the export/import wire format and object storage:
  protodelim+gzip codec over `ArchiveRecord` protos (versioned header first) and the
  `ObjectStore` interface (Put/Get) with an aws-sdk-go-v2 S3 implementation
  (`s3.endpoint`/`s3.usePathStyle` for MinIO; credentials from the standard AWS chain). The
  transfer RPCs live in `hippocampus/transfer.go`: `Transfer` dials `transfer.targetAddress` with
  credentials from `Transfer.clientCredentials`, which honours the same TLS trust-option block as
  `opensearch.tls` (`transfer.tls.{caCertFile,certFile,keyFile,insecureSkipVerify}`); TLS is
  toggled by `transferTLSEnabled` (`hippocampus/server.go`), accepting both the block form
  `transfer.tls.enabled` and the legacy scalar `transfer.tls: true`. Export/Transfer walk the store via
  `db.GetMemoriesPage`/`db.GetEventsPage` keyset pagination, record an in-memory manifest (ids +
  recall-state snapshots, last 8 kept — `transfer.maxManifestRows`, 0/default unlimited, bounds one
  run's capture: `walkStore` pre-flights the count and re-checks during the walk, refusing over-cap
  with `FailedPrecondition` before any upload), and `Clear` (or the RPCs' `clear` flag) deletes exactly
  the captured records via `db.ClearMemories` (the exported wrapper over the race-safe
  `deleteMemoriesIfUnrecalled`, so recalls landing mid-run protect their memory) and
  `DeleteEventIfEmpty`. The one-shot `clear` flag clears the manifest in-place (never via a
  store-then-take round trip, which could return a nil manifest under concurrent runs and panic);
  on a successful clear the manifest is not cached, and on a _failed_ clear it is cached so the
  returned `manifest_id` can retry via `Clear` (the error message says so). Import/ImportBatch upsert full rows by id (`db.ImportMemories`/
  `db.ImportEvents` — no defaulting, no minimum-significance gate, idempotent) and index
  non-binary memories into the optional search index. Bodies are proto3 strings and therefore
  UTF-8 everywhere — "binary" memory bodies are client-encoded — so the archive needs no special
  binary handling.
- `types/` — request/response validation and conversion between proto messages and DB rows.
- `stats/` — logs event/memory counts every `stats.intervalSeconds` (default 300; 0 disables the
  log line) and registers the count gauges. The log ticker and the gauge callback share one
  `countCache`, so the full-table `CountEvents`/`CountMemories` run at most once per interval
  regardless of the metric export frequency, rather than once per ticker tick plus once per export.
- `auth/` — JWT bearer-token support, self-contained (no `*hippocampus.Server`, no DB). `Verifier`
  is an interface (`Verify(token string) (*Claims, error)`) with two implementations, both
  restricted to a single algorithm via `jwt.WithValidMethods` (so a token can never select its own)
  and both requiring an `exp` claim via `jwt.WithExpirationRequired` (golang-jwt only validates `exp`
  when present, so this stops an expiry-less token verifying forever): `HMACVerifier` (HS256; built from an `HMACConfig` of a legacy single `signingSecret` plus
  any number of `kid`-tagged `signingKeys` — every key verifies, so a new secret rotates in while
  old tokens still verify; a kid-less token uses the legacy secret, an unknown kid is rejected) and
  `JWKSVerifier` (`jwks.go`; RS256 against an
  identity provider's JWKS — endpoint from `auth.jwksUrl` or OIDC discovery via `auth.issuer`,
  keys cached by kid, re-fetched lazily on `auth.jwksRefreshIntervalSeconds` plus one
  cooldown-limited forced re-fetch on an unknown kid so IdP key rotation verifies on first
  sight; `iss`/`aud` enforced when configured; the initial fetch failing fails construction,
  later outages leave cached keys serving). `Claims` embeds `jwt.RegisteredClaims` (including the
  `jti`) plus `ClientID` and `Roles`. `MintToken` (taking a `MintRequest`) is a plain function, not part of
  `Verifier`, used by both the `--mint-token` CLI mode and tests; it is HMAC-only (an IdP mints
  its own tokens), always stamps a random `jti`, and sets a `kid` header when minting under a
  keyed secret. `revocation.go` adds `RevocationList` (a JSON file of revoked `jti`s and
  `client_id`s — the latter optionally only before an `issuedBefore` cutoff — reloaded on the
  file's mtime every `auth.revocationRefreshSeconds`, failing startup on a bad initial load but
  keeping the last good set on a bad reload) and `NewRevokingVerifier`, a decorator that checks
  the list after any inner `Verifier` succeeds, so revocation composes with `idp` as well as
  `hmac`. All viper reads stay in main.go: `hmacConfigFromViper`/`resolveMintKey` there build the
  `HMACConfig` and pick the minting key.
  `UnaryServerInterceptor` and `HTTPMiddleware` are the two enforcement adapters — both are
  needed because the HTTP gateway calls `hipo` directly and never passes through the gRPC
  interceptor chain. Both scope themselves so Hippocampus RPCs require a token but health surfaces
  (`grpc.health.v1.Health`, `/healthz`) never do — the gRPC side by a `/proto.Hippocampus/` prefix
  check (mirroring `InterceptorBlockWhenPurgeInProgress`), the HTTP side by an explicit open-path
  allow-list (closed by default, so newly added endpoints are protected without remembering to
  update anything). On a successful verify both adapters stash the `*Claims` in the request context
  (`context.go`: `ContextWithClaims`/`ClaimsFromContext`/`ClientIDFromContext`), which the two
  loggers read to attach a `client_id` to request logs (a per-client audit trail).
  Authorisation (`authz.go`) layers roles on top of that authentication: a `Tier` hierarchy
  (`reader` ⊂ `writer` ⊂ `admin`) and a single `policies` table assigning every RPC a minimum
  tier, from which `NewAuthorizer` derives both a gRPC method map and a gateway verb+path map — so
  the two transports enforce one policy from one source (a drift-guard test asserts every RPC in
  the service descriptor has a policy). `Authorizer.UnaryServerInterceptor` (chained right after
  the auth interceptor) and `Authorizer.GatewayMiddleware` (a grpc-gateway `runtime.WithMiddlewares`
  middleware keyed on the matched `runtime.HTTPPattern`, normalised — `RPCMethod` is not yet set
  pre-handler) are the two enforcement adapters; both resolve the highest tier the verified
  `Claims.Roles` grant (default-closed: a token resolving to no known tier is denied every RPC) and
  stash it via `ContextWithTier`/`TierFromContext`. Roles come from the `roles` claim (or
  `auth.roleClaim` for an IdP that names it differently), mapped to tiers by `auth.roleMapping`;
  `--mint-token --role` stamps them. The authorizer is built (in main.go) only when auth is enabled.
  The stashed tier drives two things: `hippocampus.Server.mayReinforce` (the reader-recall gate —
  `auth.readerRecallReinforces` decides whether a reader's `RecallMemories`/reinforcing
  `SearchMemories` actually reinforces or is downgraded to a plain read) and the `WhoAmI` RPC, which
  reports the caller's effective tier so the web console can hide the write controls it may not use.
- `demo/` — a long-running load generator (`demo/generator`, its own `main` package) plus a
  launch script (`run.sh`) and a demo-tuned config. Bursty/slow/event-less writers, query and
  recall workers, and a mutator exercise every RPC; a watcher pauses writes while the database
  is at its size cap (default 1 GiB, `MAX_BYTES` env var overrides). The demo config compresses
  the decay clock (`unitsOfAgeInDays` 0.002 ≈ one age unit per 3 minutes) so forgetting,
  recall reinforcement, and the byte capacity target all play out within a session instead of
  over real days.
- `integrations/mcp/` — a standalone Model Context Protocol server (its own `package main`, like
  `demo/generator`) that bridges an LLM host (Claude Desktop/Code, any MCP client) to a running
  Hippocampus instance. A thin gRPC-client bridge, not an in-service transport: it holds no state,
  dials the service at `--address`, and turns each MCP tool call into an RPC (`main.go` wires the
  dial/transport, `tools.go` registers the tools and handlers). Serves stdio by default (logging
  forced to stderr so stdout carries only the MCP JSON-RPC stream) or streamable HTTP
  (`--transport http`). The tool surface is the per-item memory/event operations — `store_memory`,
  `update_memory`, `delete_memories` (a by-id scalpel), `recall_memories`, `search_memories`,
  `list_memories`, `create_event`, `list_events`, `get_summarisation_candidates` — deliberately
  excluding the admin/destructive and bulk data-movement RPCs (Purge, Sleep,
  Export/Import/Transfer/Clear, event delete/merge) so a model can't wipe or exfiltrate a store. The
  mutating tools are all writer-tier, so what a token may actually do is enforced by the service's
  role tiers (a reader-scoped token is refused every mutation regardless of the registered tools),
  not by tool omission.
  Proto messages are projected to plain view structs for clean inferred JSON schemas. Bearer-token
  auth (`--token`/`HIPPOCAMPUS_MCP_TOKEN`, injected as an `authorization: Bearer` client
  interceptor) and the TLS trust-option block mirror the service's Transfer client; a per-call
  timeout (`--call-timeout-seconds`) bounds each RPC. Handlers depend on a narrow `hippoClient`
  interface so `tools_test.go` drives them with a fake, plus an end-to-end test over the SDK's
  in-memory transport (`main_test.go` covers the flag/transport/credential wiring — the package
  sits ~94%, only the thin `main` shell uncovered). Built on
  `github.com/modelcontextprotocol/go-sdk/mcp`. Ships as its own image (`Dockerfile` `target: mcp`),
  reachable over HTTP via the opt-in `mcp` compose profile; the release workflow cross-compiles the
  binary for every OS/arch onto the GitHub release and publishes the image to
  `ghcr.io/fastbean-au/hippocampus-mcp`. See `docs/mcp.md`.
- `integrations/` — self-contained client/edge subprojects, each a thin bridge rather than part of
  the core service. Some are separate Go modules whose heavy dependency trees stay out of the root
  build (`otel/hippocampusexporter`, `eventsource`), one is a TypeScript project (`obsidian`), and
  one is a thin `package main` in the root module (`hippocampus-mcp`).
  - `integrations/otel/` — the OpenTelemetry Collector logs pipeline (moved here from the old
    top-level `otel/`): `hippocampusexporter/` is its own Go module (module path
    `github.com/fastbean-au/hippocampus/integrations/otel/hippocampusexporter`; `replace
    github.com/fastbean-au/hippocampus => ../../..`) — a collector logs exporter turning each log
    record into a `StoreMemory` call (severity→significance, `service.name`→`group`); `collector/`
    is the OCB builder manifest (`builder-config.yaml`) that links it into a runnable collector. See
    the two READMEs and `otel/collector`'s walkthrough. **NB the root module does not import this
    module**, so the main build is unaffected by it.
  - `integrations/obsidian/` — a TypeScript Obsidian community plugin (its own npm project, not part
    of the Go module) that uses Hippocampus as a memory layer for a vault. It talks to the **HTTP
    `/v1` gateway** via Obsidian's `requestUrl` (not gRPC, not the MCP bridge) — store notes/
    selections as memories, search/recall, and optional idempotent folder auto-sync (a persisted
    note-path→memory-id map, update-or-recreate on 404). Pure logic (`parse.ts` wire normalisation,
    `mapping.ts` note→memory mapping) is split out from the Obsidian-dependent modules so it is
    unit-testable without a running app. Requires Node.js to build (`npm install && npm run build`);
    there is no JS runtime in the default dev image. See `docs/obsidian.md` and the plugin README.
  - `integrations/eventsource/` — event-sourcing broker bridges: consume from a message broker and
    store each message as a memory. Its own Go module (module path
    `github.com/fastbean-au/hippocampus/integrations/eventsource`; `replace
    github.com/fastbean-au/hippocampus => ../..`) so the four broker-client dependency trees
    (`nats.go`, `paho.mqtt.golang`, `amqp091-go`, `segmentio/kafka-go`) stay out of the root build —
    **the root module does not import it**. A shared `bridge/` core carries the reusable pieces: a
    broker-agnostic `Message`, the `Transformer` callback seam (`Transform(Message) ([]*contract.Memory,
    error)`) with a `TransformerFunc` adapter and a configurable `DefaultTransformer` (payload→body,
    subject→group, fixed/header significance, optional base64/binary + truncation, future-timestamp
    clamping), `Store.Handle` (transform then `StoreMemory` each memory; a `Rejected` below-threshold
    memory is a success, a transform/transport failure is the adapter's cue to nack/redeliver), the
    gRPC `Dial` (bearer-token + TLS trust options, mirroring the MCP bridge), and `RegisterCommonFlags`
    (pflag only — each `cmd/*` main owns its viper reads, per the convention). Four adapters
    (`nats/`, `mqtt/`, `rabbitmq/`, `kafka/`), each a library `Bridge` (`New(Config, *bridge.Store)` +
    `Run(ctx)`) plus a `cmd/<broker>` runnable, with a broker-connection seam injected so `Run` is
    unit-testable with fakes; delivery semantics match each broker (NATS at-most-once; MQTT/RabbitMQ/
    Kafka at-least-once via manual ack/commit). Tests are broker-free by default (NATS uses an
    embedded in-process server; MQTT/RabbitMQ real-connect paths are env-gated integration tests —
    `HIPPOCAMPUS_TEST_MQTT_BROKER`/`HIPPOCAMPUS_TEST_RABBITMQ_URL` — that CI runs against mosquitto/
    rabbitmq containers), every package ≥95% covered. Built/vetted/tested by the `eventsource-bridges`
    CI job (like `otel-exporter`; the `docker` CI job also smoke-builds the four images). The release
    workflow cross-compiles all four `cmd` binaries (`hippocampus-<broker>-bridge`) onto the GitHub
    release and publishes one multi-arch image per broker to GHCR
    (`ghcr.io/fastbean-au/hippocampus-<broker>-bridge`) via a matrix over the one parameterised
    `integrations/eventsource/Dockerfile` (the `BROKER` build-arg selects `cmd/<broker>`; built with
    the repo root as context since the module's `replace` reaches the root contract). A bridge is an
    outbound client (dials broker + service, listens on no port), so the image exposes nothing and has
    no default CMD — each broker's required flags are passed after the image name. See
    `docs/eventsource.md` and the module README.

## Conventions in this repo

- Logging is **logrus** (not zerolog), typically with a `log.Trace("func() ...")` entry line at the
  top of functions — match this existing style rather than global preferences.
- Errors are logged where they occur and returned unwrapped with `fmt.Errorf`.
- Exactly one instance may consolidate a given store. SQLite is single-instance (embedded DB); on
  the `postgres`/`mysql` drivers a shared database can have one consolidating instance
  (`consolidation.enabled: true`, holds the lock) plus read/write replicas
  (`consolidation.enabled: false`, skip the lock, reject the `Sleep` RPC) — horizontal scaling.
  Authentication (JWT bearer tokens) and TLS
  are both optional and disabled by default; see [Authentication](docs/configuration.md#authentication)
  and [TLS](docs/configuration.md#tls).
