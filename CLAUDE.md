# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

- Build: `go build ./...`
- Run: `go run ./cmd/hippocampus -c config.json` (the `-c`/`--config_file` flag defaults to `./config.json`)
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
- Demo/soak test: `./demo/run.sh` (builds and launches the service plus a load generator; see `demo/README.md`)
- Docker: `docker compose up --build` (SQLite), `docker compose -f docker/docker-compose.postgres.yaml
  up --build` (PostgreSQL), `docker compose -f docker/docker-compose.mysql.yaml up --build` (MySQL), or
  `docker compose -f docker/docker-compose.opensearch.yaml up --build` (SQLite + OpenSearch content
  search); container configs in `docker/`, image config baked from `docker/config.sqlite.json`
- CI: `.github/workflows/ci.yaml` — build/vet/gofmt/tests (with postgres and mysql service
  containers so the `db/postgres_test.go` and `db/mysql_test.go` integration tests run instead
  of skipping) plus compose-stack smoke tests. Postgres/MySQL integration tests run locally with
  `HIPPOCAMPUS_TEST_POSTGRES_DSN=<dsn>`/`HIPPOCAMPUS_TEST_MYSQL_DSN=<dsn>` `go test ./db`
  against any disposable database
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
(`GetSummarizationCandidates`); the service has no visibility into memory content, so a client
performs the actual replacement (`ReplaceMemoriesWithSummary`). Every RPC is also reachable as a
JSON/HTTP endpoint under `/v1` via an in-process grpc-gateway (`gateway.port`, 0 disables). Both
transports can require a signed JWT bearer token (`auth.method`: `none`/`hmac`/`idp`) and TLS
(`tls.enabled`); both are off by default.

## Architecture

- `cmd/hippocampus/` — the `package main` entrypoint (`main.go` plus `backfill.go`,
  `interceptors.go`, `logging.go`, `observability.go`, and the `webui.go`/`webui/` embedded
  console). `main.go` — bootstrap only: reads the JSON config file into viper, initialises logging
  (logrus) and observability (`observability.go`: optional OTEL tracing/metrics over OTLP/gRPC,
  no-op when disabled), opens the DB, wires the gRPC server with interceptors (plus the
  `otelgrpc` stats handler when observability is enabled), starts stats, and on SIGINT/SIGTERM
  flushes observability then closes the DB. `--mint-token` (with `--client-id`, `--ttl`,
  `--signing-secret`) is a separate CLI mode: it prints a signed `auth.MintToken` token to stdout
  and exits before the database, observability, or server are touched at all; it refuses under
  `auth.method: idp` (the IdP issues tokens there). `auth.method` selects the auth scheme —
  `none` (default), `hmac` (`auth.NewHMACVerifier` from `auth.signingSecret`), or `idp`
  (`auth.NewJWKSVerifier`: RS256 against an IdP's JWKS, from `auth.jwksUrl` or OIDC discovery
  via `auth.issuer`); the legacy boolean `auth.enabled` is a deprecated alias consulted only
  when `auth.method` is unset (`true` → `hmac` plus a warning). Whichever verifier is built,
  `auth.UnaryServerInterceptor` is prepended to the gRPC interceptor chain (ahead of
  `InterceptorBlockWhenPurgeInProgress`/`InterceptorLogger`, so unauthenticated requests are
  rejected before any other interceptor runs); when `tls.enabled`,
  `credentials.NewServerTLSFromFile` is added via `grpc.Creds`. Auth without
  `tls.enabled` only logs a warning — TLS may be terminated upstream instead. When `gateway.port`
  > 0 it also registers `contract.RegisterHippocampusHandlerServer` (the generated
  `hippocampus.pb.gw.go` reverse proxy) on a `runtime.NewServeMux()` and serves it over HTTP (TLS
  via `ListenAndServeTLS` when `tls.enabled`) — calling straight into the same `hipo` server
  instance, not dialing back over gRPC — alongside a static `/v1/openapi.json` (the embedded
  `contract.SwaggerJSON`) and an unauthenticated `/healthz`. Because the gateway calls `hipo`
  directly and never runs the gRPC interceptor chain, the mux is always wrapped in
  `hipo.HTTPMiddlewareBlockWhenPurgeInProgress` (the HTTP counterpart to
  `InterceptorBlockWhenPurgeInProgress`; open paths `/healthz` and `/v1/openapi.json`, else 503
  while a purge runs); when `auth.enabled`, that is in turn wrapped in `auth.HTTPMiddleware`
  (outermost, so unauthenticated requests are rejected first) except `/healthz`. The gateway is shut down before the gRPC
  server on SIGINT/SIGTERM. All configuration flows through viper keys matching `config.json`
  structure. Instrumentation elsewhere uses the global OTEL providers
  (`hippocampus/telemetry.go`, `stats/stats.go`), so it stays no-op-safe whether or not
  observability is enabled.
- `hippocampus/` — the gRPC service implementation (`Server` in `server.go`). Reads its config
  from viper once in `New()`. `sleep.go` holds the core consolidation logic:
  - `autoSleep` runs `sleep()` every `sleep.periodSeconds`; a manual `Sleep` RPC resets the timer
    via the `sleepReset` channel. A non-positive `sleep.periodSeconds` disables the timed cycle
    entirely (`sleepTimer` returns a nil channel, dropping that select case) — a supported mode for
    an instance driven only by the manual `Sleep` RPC or the WAL trigger; the manual RPC and WAL
    trigger keep working. When `consolidation.walTriggerBytes` > 0, `autoSleep` also polls
    the on-disk WAL file's size (`db.WALBytes`, a filesystem stat — no database connection needed)
    every `walCheckInterval` and runs an out-of-cycle sleep as soon as it's exceeded, so a
    checkpoint runs sooner than the next timed cycle under sustained high write rates. All three
    routes call `sleep()` through `sleepOnce`, which wraps it in a `singleflight.Group`
    (`Server.sleepGroup`) keyed on a constant, so a caller landing while a cycle is already running
    joins that in-flight call instead of starting a second, overlapping one.
  - `sleep()` = `consolidate()` (delete memories/events below threshold) +
    `scanSummarizationCandidates()` (when `consolidation.summarizationMinMemories` > 0, find
    events with at least that many memories that have all gone quiet — no creation or recall —
    for `summarizationMinAgeInDays`, cache up to `summarizationMaxCandidates` of them for
    `GetSummarizationCandidates` to serve; best-effort, never fails the cycle) + `evict()` (when
    `consolidation.capacityBytes` > 0 and the store's used bytes still exceed it, delete
    memories in ascending value order until back at the eviction floor —
    `consolidation.capacityBytesFloor`, hysteresis headroom below the target; ignores
    `minimumAgeInDays`) + `preserve()` (compact the database: incremental vacuum + WAL
    checkpoint). `consolidate()` runs three passes: memories without events, memories with
    events (deleting an event when its last memory goes), and events without memories.
  - `ReplaceMemoriesWithSummary` (in `memory.go`) deletes every memory for an event and inserts a
    single caller-supplied summary memory in their place, in one transaction; the summary is
    validated before anything is deleted. The new memory is flagged `is_summary` so it doesn't
    recount towards a future candidate scan until fresh, unsummarized memories accumulate again.
  - `ShouldConsolidateMemory` / `ShouldConsolidateEvent` (taking candidate structs defined in
    `db/db.go`) share `shouldConsolidate` / `calculateValue`, which implement the six
    configurable deletion algorithms (`consolidation.method` 1–6: power law, two linear variants,
    exponential half-life, logarithmic long-tail, and sigmoid consolidation-window) documented
    with value tables in `docs/consolidation.md`. The value combines memory/event significance, weighted relationship significance
    (`relationshipSignificanceWeight`), and a per-recall boost (`recallSignificanceWeight`); age is
    measured from the most recent recall. The deletion threshold is scaled each cycle by capacity
    pressure (the greater of row-count utilisation against `capacityMemories` and byte
    utilisation against `capacityBytes`, raised to `capacityPressureExponent`) so forgetting
    becomes more aggressive as the store fills. Memories without an event get a default event significance,
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
  `hippocampus.Server` and `stats` depend on — the seam for future non-SQL backends. SQLite
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
  `ReplaceMemoriesWithSummary`, `GetSummarizationCandidates`, and the transfer/archive surface
  (`Export`, `Import`, `ImportBatch`, `Transfer`, `Clear`). Each RPC carries a
  `google.api.http` annotation mapping it onto a REST-ish `/v1/...` path (see
  [Configurability](docs/configuration.md#configurability) for the full mapping); `go generate
  ./contract` (directive in `generate.go`) turns those into `hippocampus.pb.gw.go` (the gateway)
  and `hippocampus.swagger.json` (the OpenAPI description, embedded via `swagger.go`).
  `contract/google/api/{annotations,http}.proto`
  are vendored copies of the googleapis definitions the annotations depend on.
- `search/` — the optional OpenSearch secondary content-search index (`opensearch.enabled`,
  off by default; `search.Index` interface with no-op and `opensearch-go/v4` implementations).
  Strictly secondary: all mutations propagate primary→index asynchronously (bounded queue, one
  FIFO worker — ordering matters for summarization's delete-then-index; overflow drops, never
  blocks), and `SearchMemories` results are always re-read from the primary store so stale index
  entries drop out. Consolidation/eviction deletes reach it via `db.SetMemoryDeleteObserver` (on
  the concrete `*db.DB`, not `db.Store`); RPC-layer hooks cover the rest. Binary memories are
  never indexed. Because propagation is best-effort, the index can go sparse; the
  `--backfill-search` CLI mode (`backfill.go`) rebuilds it from the primary store via synchronous
  `IndexMemorySync`/`RecreateIndex` calls that bypass the queue (safe: the tool has no worker or
  live writes of its own) and `db.GetIndexableMemoriesPage` keyset pagination — with `--reindex`
  it recreates the index first to clear stale documents. Each driver opens read-only so the tool
  can run beside a live service: SQLite via `db.NewSQLiteReadOnly` (`mode=ro`, no `initSchema`,
  `Preserve` a no-op — so it never writes DDL or checkpoints the database the service owns),
  Postgres/MySQL via `db.NewPostgresReadOnly`/`NewMySQLReadOnly` (skipping the instance lock). Integration tests skip unless `HIPPOCAMPUS_TEST_OPENSEARCH_URL` is set;
  `docker/docker-compose.opensearch.yaml` runs the full stack.
- `archive/` — the export/import wire format and object storage:
  protodelim+gzip codec over `ArchiveRecord` protos (versioned header first) and the
  `ObjectStore` interface (Put/Get) with an aws-sdk-go-v2 S3 implementation
  (`s3.endpoint`/`s3.usePathStyle` for MinIO; credentials from the standard AWS chain). The
  transfer RPCs live in `hippocampus/transfer.go`: Export/Transfer walk the store via
  `db.GetMemoriesPage`/`db.GetEventsPage` keyset pagination, record an in-memory manifest (ids +
  recall-state snapshots, last 8 kept), and `Clear` (or the RPCs' `clear` flag) deletes exactly
  the captured records via `db.ClearMemories` (the exported wrapper over the race-safe
  `deleteMemoriesIfUnrecalled`, so recalls landing mid-run protect their memory) and
  `DeleteEventIfEmpty`. The one-shot `clear` flag clears the manifest in-place (never via a
  store-then-take round trip, which could return a nil manifest under concurrent runs and panic);
  on a successful clear the manifest is not cached, and on a *failed* clear it is cached so the
  returned `manifest_id` can retry via `Clear` (the error message says so). Import/ImportBatch upsert full rows by id (`db.ImportMemories`/
  `db.ImportEvents` — no defaulting, no minimum-significance gate, idempotent) and index
  non-binary memories into the optional search index. Bodies are proto3 strings and therefore
  UTF-8 everywhere — "binary" memory bodies are client-encoded — so the archive needs no special
  binary handling.
- `types/` — request/response validation and conversion between proto messages and DB rows.
- `stats/` — logs event/memory counts every 5 minutes.
- `auth/` — JWT bearer-token support, self-contained (no `*hippocampus.Server`, no DB). `Verifier`
  is an interface (`Verify(token string) (*Claims, error)`) with two implementations, both
  restricted to a single algorithm via `jwt.WithValidMethods` so a token can never select its
  own: `HMACVerifier` (HS256, shared secret) and `JWKSVerifier` (`jwks.go`; RS256 against an
  identity provider's JWKS — endpoint from `auth.jwksUrl` or OIDC discovery via `auth.issuer`,
  keys cached by kid, re-fetched lazily on `auth.jwksRefreshIntervalSeconds` plus one
  cooldown-limited forced re-fetch on an unknown kid so IdP key rotation verifies on first
  sight; `iss`/`aud` enforced when configured; the initial fetch failing fails construction,
  later outages leave cached keys serving). `Claims` embeds `jwt.RegisteredClaims` plus
  `ClientID` (unused beyond logging today). `MintToken` is a plain function, not part of
  `Verifier`, used by both the `--mint-token` CLI mode and tests; it is HMAC-only — an IdP
  mints its own tokens.
  `UnaryServerInterceptor` and `HTTPMiddleware` are the two enforcement adapters — both are
  needed because the HTTP gateway calls `hipo` directly and never passes through the gRPC
  interceptor chain. Both scope themselves so Hippocampus RPCs require a token but health surfaces
  (`grpc.health.v1.Health`, `/healthz`) never do — the gRPC side by a `/proto.Hippocampus/` prefix
  check (mirroring `InterceptorBlockWhenPurgeInProgress`), the HTTP side by an explicit open-path
  allow-list (closed by default, so newly added endpoints are protected without remembering to
  update anything).
- `demo/` — a long-running load generator (`demo/generator`, its own `main` package) plus a
  launch script (`run.sh`) and a demo-tuned config. Bursty/slow/event-less writers, query and
  recall workers, and a mutator exercise every RPC; a watcher pauses writes while the database
  is at its size cap (default 1 GiB, `MAX_BYTES` env var overrides). The demo config compresses
  the decay clock (`unitsOfAgeInDays` 0.002 ≈ one age unit per 3 minutes) so forgetting,
  recall reinforcement, and the byte capacity target all play out within a session instead of
  over real days.

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
