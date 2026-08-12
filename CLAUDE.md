# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

- Build: `go build ./...`
- Run: `go run ./cmd/hippocampus -c config.json` (the `-c`/`--config_file` flag defaults to `./config.json`)
- Run the MCP server (separate module — run from its directory):
  `cd integrations/mcp && go run . --address localhost:50051` (a standalone MCP
  bridge that dials a running service; stdio by default, `--transport http` for streamable HTTP;
  see `docs/mcp.md`)
- Run the `hippo` CLI (separate module — run from its directory):
  `cd integrations/cli && go run . whoami` (a stateless command-line client exposing the full RPC
  surface; gRPC by default, `--transport http` for the `/v1` gateway; `go test ./...` in that dir;
  see `docs/cli.md`)
- Run an event-sourcing bridge (separate module — run from its directory):
  `cd integrations/eventsource && go run ./cmd/nats --subject 'events.>' --address localhost:50051`
  (one `cmd/<broker>` each for `nats`/`mqtt`/`rabbitmq`/`kafka`/`bluesky`; consumes from the broker
  and stores each message as a memory; `go test ./...` in that dir, with `HIPPOCAMPUS_TEST_MQTT_BROKER`/
  `HIPPOCAMPUS_TEST_RABBITMQ_URL`/`HIPPOCAMPUS_TEST_JETSTREAM` set to run the integration tests; see
  `docs/eventsource.md`)
- Run the ingestor (separate module — run from its directory):
  `cd integrations/ingestor && go run ./cmd/ingestor --source-address localhost:50051 --target-address central:50051 --rules rules.json`
  (promotes completed events from an edge instance into a central one under a CEL rules file;
  `--check-rules` compiles the rules and exits, `--dry-run` judges without moving anything;
  `--health-port` (8090) serves `/healthz`+`/readyz` and `--metrics` exports OTLP;
  `go test ./...` in that dir needs no service; see `docs/ingestor.md`)
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
- Check the contract for breaking changes: `cd contract && buf breaking --against
'../.git#tag=<previous tag>,subdir=contract' --path hippocampus.proto` (config and rationale in
  `contract/buf.yaml`; CI runs it as the `proto-breaking` job against the last release tag, which
  always reports but only **fails** the build where semver does not already permit the break — it
  stands down against a pre-1.0 baseline, and for a major increment declared as
  `## [Unreleased] (v2.0.0)` in `CHANGELOG.md`; see `RELEASE.md`). The
  proto package is `hippocampus.v1`, so every gRPC method is `/hippocampus.v1.Hippocampus/<Method>`
  — three packages keep a hand-written copy of that prefix (`auth/grpc.go`,
  `cmd/hippocampus/rpcmetrics.go`, `hippocampus/server.go`), each held to the generated descriptor
  by a `TestServicePrefixMatchesDescriptor`, because a stale copy fails **open** in the auth
  interceptor and the purge gate. `buf lint` is deliberately not wired up — see `contract/buf.yaml`
- Demo/soak test: `./demo/run.sh` (builds and launches the service plus a load generator; see
  `demo/README.md`). By default it also launches a `grafana/otel-lgtm` collector (docker or
  podman) with the provisioned dashboard and ships metrics/traces to it (Grafana on `:3000`); set
  `OBSERVABILITY=0` to skip it. The env overrides are exported by `run.sh`, not baked into
  `demo/config.json`
- Docker: `docker compose up --build` (SQLite), `docker compose -f deploy/compose/docker-compose.postgres.yaml
up --build` (PostgreSQL), `docker compose -f deploy/compose/docker-compose.mysql.yaml up --build` (MySQL), or
  `docker compose -f deploy/compose/docker-compose.opensearch.yaml up --build` (SQLite + OpenSearch content
  search, security disabled — demo only) or `docker compose -f
deploy/compose/docker-compose.opensearch-secured.yaml up --build` (the same with the OpenSearch security
  plugin enabled: HTTPS + basic auth, Hippocampus connecting over TLS via the `opensearch.tls`
  config block, credentials injected as `OPENSEARCH_ADMIN_PASSWORD`); container configs in
  `deploy/compose/`, image config baked from `deploy/compose/config.sqlite.json`. The `Dockerfile` is multi-stage:
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
  collector is up. A Hippocampus overview dashboard (`deploy/compose/observability/`) is bind-mounted into
  Grafana's provisioning tree and set as the home page (`GF_DASHBOARDS_DEFAULT_HOME_DASHBOARD_PATH`),
  alongside `alerting-rules.yaml` (the shipped alerts as Grafana-managed rules) — see the alerting
  note under Architecture
- Kubernetes: `kubectl apply -k deploy/k8s/overlays/sqlite` (embedded SQLite: one `StatefulSet` +
  a PVC) or `kubectl apply -k deploy/k8s/overlays/postgres` (centralised: one consolidator
  `Deployment` + N replica `Deployment`s over a shared Postgres, mirroring the horizontal-scaling
  model). Kustomize base+overlays under `deploy/k8s/` — no Helm; a shared `base/` (namespace,
  token-less ServiceAccount, Service) plus per-overlay `config.json` wired through a
  `configMapGenerator` (content-hashed → auto-rolls on edit). Secrets (DSN, signing key) and the
  consolidator/replica split are injected as `HIPPOCAMPUS_*` env overrides, not baked into the
  ConfigMap; probes hit `/healthz`/`/readyz`; pods run non-root/read-only-rootfs. See
  `deploy/k8s/README.md`
- CI: `.github/workflows/ci.yaml` — build/vet/gofmt/tests (with postgres and mysql service
  containers so the `db/postgres_test.go` and `db/mysql_test.go` integration tests run instead
  of skipping) plus compose-stack smoke tests. Postgres/MySQL integration tests run locally with
  `HIPPOCAMPUS_TEST_POSTGRES_DSN=<dsn>`/`HIPPOCAMPUS_TEST_MYSQL_DSN=<dsn>` `go test ./db`
  against any disposable database. The `proto-breaking` job gates the contract (above)
- Release compatibility: `CHANGELOG.md` is the curated record (the GitHub release notes are a commit
  list); its **Compatibility** section states what a version number covers — contract, config keys,
  stored schema — and what is exempt. `RELEASE.md` carries the process, including the changelog step
  in the pre-flight and what a deliberate break requires. Pre-1.0, a breaking change goes in a minor
  release
- Run the configuration wizard (root module, second binary):
  `go run ./cmd/config-wizard` (serves the browser-based config/deployment builder on `:8091`;
  static assets only, no service connection; `--port`/`--bind-address`/`--log-level`, all
  `HIPPOCAMPUS_WIZARD_*` overridable; see `docs/config-wizard.md`)
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
  console — one self-contained HTML/CSS/JS page, no build step, whose **Decay** tab is the client
  side of `ExplainConsolidation`: a per-row value column in the memory/search tables, the current
  capacity pressure and threshold, an inline-SVG decay curve, and an `admin`-gated dry-run panel
  over `PreviewConsolidation`. It computes **no** decay maths of its own — every number and every
  curve point is served — which is the whole reason those RPCs report what they do). `main.go` —
  bootstrap only: reads the JSON config file into viper (**optional on the default path** — an
  absent `./config.json` starts the service on `setStartupDefaults`' built-in defaults with a Warn
  line naming them, while a `--config_file` given explicitly must exist; `setStartupDefaults` is a
  function rather than inline statements so a test can assert the defaults alone form a valid
  configuration, and it defaults the four keys `validateConfig` refuses at zero —
  `consolidation.method`/`aggressiveness`/`unitsOfAgeInDays` and `storage.directory` — without
  relaxing item 19.1, since viper falls back to a default only for an _unset_ key and a configured
  0 still fails validation), initialises logging
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
  the interface — e.g. `127.0.0.1` behind a TLS-terminating sidecar/mesh. A zero `gateway.port` is
  logged at Info naming what goes with it (console, OpenAPI doc, HTTP probes) and how to enable it,
  since binding nothing was previously indistinguishable from binding something that failed. When
  `gateway.port`
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
  dashboard (`deploy/compose/observability/`) exists for local viewing. The **RED metrics** —
  request rate, errors, duration — are separate, in `rpcmetrics.go`, because they belong to the
  transport boundary rather than the domain: `hippocampus.rpc.requests` and
  `hippocampus.rpc.duration` share one attribute set (`transport`, `rpc`, `code`, `outcome`) and are
  recorded by `InterceptorMetrics` on gRPC and `httpMetricsMiddleware` on the gateway, both installed
  inside panic recovery but **outside** authentication so a rejected request still appears in the
  error rate. Four things carry the design. (1) `outcome` is three-valued
  (`ok`/`client_error`/`server_error`, gRPC classifying via the existing `isClientFaultCode`) rather
  than a success bool, so an SLO can alert on the service failing without also firing on clients
  sending bad requests. (2) `rpc` names the same thing on both transports — the gateway resolves it
  via `auth.RouteRPC`, an inversion of `auth/authz.go`'s already-drift-guarded `policies` table —
  and **never** from the request path, which carries ids. (3) The gateway therefore needs _two_
  middlewares: only a post-routing `runtime.Middleware` (`gatewayRouteMiddleware`, registered first
  so it runs ahead of the authoriser) knows the matched route, but only the outer handler sees
  requests rejected before routing, so the former fills in a `routeCapture` the latter placed on the
  context, and an unrouted request is counted as `rpc="unknown"`. (4) Neither recording is deferred:
  panic recovery sits outside both, so a deferred record would count a panicking call as a success —
  panics stay `hippocampus.panics_recovered`'s to report. Both are scoped to the RPC surface (the
  `/hippocampus.v1.Hippocampus/` prefix; `/v1` minus the OpenAPI doc), keeping probe, console and login
  traffic out of the error-rate denominator. **The alert rules those metrics exist for are shipped
  too**, and deliberately twice: `deploy/observability/prometheus-alerts.yaml` (a portable
  Prometheus rule file — the artefact a real deployment loads) and
  `deploy/compose/observability/alerting-rules.yaml` (the same nine rules as Grafana-managed rules,
  provisioned into every compose file's `observability` profile and `demo/run.sh`, because Grafana
  provisions its own format and cannot read a Prometheus rule file). Two copies of a PromQL
  expression that nothing in the repo executes is exactly what drifts, so the drift guard
  (`cmd/hippocampus/alerts_test.go`) fails if the two disagree on any expression, `for:`, label or
  annotation, if either
  names a metric no instrument declares (matching the queried series name back to the instrument
  through the OTLP `_total`/`_bucket`/`_seconds` suffixes), or if a Grafana rule's wiring is wrong in
  a way that provisions cleanly and then fails every evaluation (dangling `condition`, wrong
  datasource uid, a query window narrower than its own range selector). Two decisions carry the
  pair: the comparison lives in the **PromQL** on both sides (Grafana adds only a `gt 0` threshold
  over an instant query, hence `noDataState: OK` on every rule), so the two engines behave
  identically; and absence — a consolidator that has exited publishes no counter to alert on — is
  asked with `absent_over_time` inside the expression rather than through a no-data policy, for the
  same reason. Neither file provisions a contact point.
- `cmd/config-wizard/` — the configuration and deployment wizard: a second `package main` in the
  root module (`main.go` plus the embedded `wizard/` assets). The Go side is a static file server
  and nothing else — embedded `index.html`/`app.js`/`styles.css` behind a strict CSP (no
  `unsafe-inline`, `connect-src 'none'`), plus `/healthz`. All the work is in `wizard/app.js`: a
  schema (`STEPS` → cards → fields, indexed into `FIELDS`) that drives the form, the validation, and
  the generated artefacts from one source. It builds a `config.json` plus an `HIPPOCAMPUS_*`
  environment file for the secret-typed keys, and a Compose file / Kubernetes manifests / systemd
  unit / launchd plist / `DEPLOY.md` runbook per deployment target, all in the browser (nothing is
  transmitted, and secrets are kept out of `localStorage`). Validation mirrors `validateConfig` and
  the driver switch in `cmd/hippocampus/main.go`; the decay preview mirrors `calculateValue` in
  `hippocampus/sleep.go`. Each field records `def` (what the wizard suggests) and, where the service
  has one of its own, `svc` (the `viper.SetDefault` value) — that distinction is what makes the
  "minimal" config safe, since a key the service does not default reads as zero and several of those
  are fatal; `defaults_test.go` cross-checks the two files so they cannot drift. Ships as its own
  image (`Dockerfile` `target: config-wizard` → `ghcr.io/fastbean-au/hippocampus-config-wizard`) and
  a per-OS/arch release binary; the hosted copy is `config-builder.hippocampus-demo.com`, a service
  in the separate demo-site repo's combined showcase stack. See `docs/config-wizard.md`.
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
    with value tables in `docs/consolidation.md`. The value combines memory/event significance, the
    **damped link contributions** (`linkSignificanceWeight` × `log1p(sum)`, applied separately to the
    memory's own links and its event's - see `db/link.go`), and a per-recall boost
    (`recallSignificanceWeight`); age is measured from the most recent recall. The deletion threshold is scaled each cycle by capacity
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
  - `PreviewConsolidation` (`hippocampus/preview.go` + `db/preview.go`) is the dry run: what a cycle
    would forget, deleting nothing. It is a **separate RPC, not a `dry_run` flag on `Sleep`** —
    `Sleep` takes `EmptyRequest`/`GeneralResponse`, and more to the point authorisation is per-RPC,
    so a flag could never be tiered apart from the destructive cycle it rode on (it is `admin`
    today, but separability is what leaves that free to change). Three things carry the design.
    (1) It does **not** go through `sleepOnce`'s singleflight: joining an in-flight cycle would
    describe a run that is at that moment deleting. (2) Standing outside that group means it cannot
    read the two fields the sleep goroutine mutates (`capacityPressure`,
    `defaultEventSignificanceValue`) — that would be a data race, and would also let one scan
    evaluate its first rows against different numbers from its last. So `previewDecider` carries a
    snapshot, and `shouldConsolidateUnder`/`memorySignificanceUnder`/`memoryValueUnder`/
    `shouldConsolidateEventUnder` are the parameterised forms the existing methods now delegate to.
    Every actual decision still goes through the server's own methods. (3) `db.PreviewConsolidation`
    scans **once** and reimplements only the per-event bookkeeping rather than sharing the four real
    passes' code — deliberately, to keep the most delicate code in the repo untouched — so
    `TestPreviewMatchesASleepCycle` (preview, then run the real passes, then compare) is what stops
    the two drifting. It applies the cycle's ordering (consolidation first, its memories excluded
    from the eviction pool and its bytes already reclaimed), reads `group_name`/`length(body)` and
    so leaves the covering index the real scans stay on, and never returns bodies. Concurrent
    previews collapse onto one scan via `Server.previewGroup` — a **separate** singleflight from
    `sleepGroup` (sharing one would defeat (1)), keyed on the `db.PreviewLimit`-normalised sample
    size so a caller asking for more rows is never handed a shorter list. What the group shares is
    `previewResult`, a plain struct, **not** the proto response: a proto message is not safe to
    marshal concurrently (marshalling writes its internal size cache), so each caller builds its
    own via `previewResponse`.
  - `ExplainConsolidation` (`hippocampus/explain.go` + `db/explain.go`) is the per-memory half of the
    same transparency: given ids (at most `explainMaxMemoryIds`, 200) it reports each memory's
    computed value, the pressure-scaled threshold, its effective significance, the `retained` /
    `below_minimum_age` overrides, and `days_until_forgotten`; with a `curve` it also returns the
    decay curve of the current configuration. Four things carry the design. (1) It is **`reader`**
    tier while the preview is `admin` — it enumerates nothing, answering only about ids the caller
    supplies and could already read in full via `GetMemories`. (2) It reuses the preview's snapshot
    machinery (`decisionSnapshot`, which `previewDecisionState` became — now returning a
    `decisionState` carrying the memory count as well), so both evaluate against one consistent set
    of inputs and neither reads the sleep goroutine's live fields. (3) That snapshot is **cached**
    for `explainStateTTL` behind `Server.explainGroup`/`explainStateMu`, because it costs a
    `UsedBytes` plus a `CountMemories` — both full scans on the server drivers — and this RPC is
    called once per console page rather than once per operator decision (item 25.9's lesson).
    Capacity pressure moves over a cycle, not over seconds. (4) The curve and the
    `days_until_forgotten` projection are found by **bisecting `calculateValue`** over
    `curveHorizonUnits` rather than by inverting six curves, so a seventh method needs no new maths
    here; a configuration that never crosses reports `-1` rather than a number that looks like an
    answer. The projection includes the `minimumAgeInDays`/`minimumRetentionInDays` floors and
    assumes no further recall. Not on the MCP surface, like the preview.
  - `recordRetention` (`sleep.go`, called from `evict`) publishes the
    `hippocampus.memories.retained`/`hippocampus.retained_bytes` gauges from `db.RetainedStats` —
    one aggregate query (`MAX`/`GREATEST(timestamp, time_recalled) >= cutoff`, the same decay clock
    consolidation ages from, plus the shared `evictionRowOverheadBytes` allowance so the figure is
    comparable with `UsedBytes`). Gated on **both** `minimumRetentionInDays` and `capacityBytes`
    being set: it costs an extra scan per cycle and means nothing without a capacity target, since
    the pair exists to expose the one failure mode where retention (which overrides the capacity
    target) holds so much that eviction can never bring the store back under it — also logged at
    Warn for deployments without a metrics stack. Best-effort: a failure leaves the gauges stale and
    never fails the cycle. `hippocampus.capacity_bytes` is exported beside `used_bytes` so a
    dashboard need not hard-code the limit.
  - `Purge` deletes everything; while it runs, `InterceptorBlockWhenPurgeInProgress` (registered in
    main.go, `codes.Unavailable`) rejects all Hippocampus RPCs on gRPC, and its HTTP counterpart
    `HTTPMiddlewareBlockWhenPurgeInProgress` (503) rejects them on the gateway.
  - `scope.go` is the RPC half of **group scoping** (`auth/groups.go` is the other; the decision
    record is TODO 60.1). It cannot be one chokepoint, because the RPCs do not reach the store the
    same way: a listing carries the scope as a predicate (`MemoryFilter.Groups`), an id-addressing
    RPC has no predicate and checks the ids instead (`scopeMemoryIds`/`scopeEventIds`), a store walk
    threads it into pagination, and `Purge`/`Sleep`/`PreviewConsolidation` cannot be scoped at all
    and are refused (`requireUnbound`). Four mechanisms means four places to forget one, so the
    `scopes` table declares which each RPC uses and `TestScopesCoverEveryRPC` requires every
    descriptor method to appear — **a new RPC must add an entry**, and a subtest in
    `scope_isolation_test.go`, whose own descriptor check is the reminder. The table is
    documentation and a checklist, never consulted at request time; what verifies the handlers is
    `TestGroupScopeIsolation*`, which drives every RPC as a caller bound to one group (disabling
    `scopedGroups` fails 34 of its subtests). Three rules the code holds to: an out-of-scope id the
    caller **named** reports `NotFound`, never `PermissionDenied`, which would confirm it exists;
    an id the caller did **not** name (a link's far end, an unlink target) is dropped silently, since
    refusing would reveal the crossing; and `writeGroup` stamps a scoped caller's sole group on a
    write naming none, so a bound writer never creates a record it cannot read back. Two things
    deliberately cross the boundary, both consequences of the partition being _soft_:
    `link_significance` is scope-blind (it is the denormalised aggregate in the covering index, and
    recomputing per-scope would mean joining the link tables in the consolidation scans), and the
    decay dynamics stay store-global.
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
  own (tracing-span) context and stays server-owned, not tied to the `Sleep` RPC's deadline.
  `scope.go` is the storage half of group scoping: `appendGroupScope` builds the one `group_name IN
(...)` predicate every scoped query uses (applied in `memoryFilterConditions`/
  `eventFilterConditions` **above the significance-extremum early return**, for the reason that
  function's own comment gives, plus `SearchMemoryHits` and the two `Get*Page` walks), and
  `MemoryIdsOutsideGroups`/`EventIdsOutsideGroups` are the id-check counterpart, chunked exactly as
  `MissingIds` is. An **empty groups slice means unrestricted**, which is what lets every
  server-owned scan (the sleep cycle, the reconcile sweep, the search backfill) pass nil and keep
  seeing the whole store — a consolidation pass that skipped a group would simply never forget it.
  There is deliberately **no tenant column**: the scope is the existing `group_name`, already in the
  covering index and both search backends, which is why the feature needed no migration. Memory
  bodies are stored compressed (`compress.go`, `storage.compression.enabled`, **on** by default;
  gzip, deliberately not configurable so an old body always stays readable — though the compression
  _level_ is encoder-side only and so carries no such commitment, which is why it is `BestSpeed`):
  the write helpers call `compressBody` and the row scanners `decompressBody`, so compression lives
  entirely at the storage boundary and everything above the package — RPCs, search index, summariser,
  archive — sees plain bodies. The `gzip.Writer`/`gzip.Reader` are pooled (`sync.Pool` + `Reset`)
  because constructing one allocates flate's window and hash tables regardless of body size and
  dominated everything else; pooling plus `BestSpeed` is what takes a store-and-read round trip from
  ~60% overhead to ~2.4% (benchmarks in `db/bench_test.go`, written up in `docs/performance.md`). The decision is recorded per row (`memories.is_compressed`) and reads follow
  that flag, never the current configuration, so the setting is safe to change on a live store and a
  mixed store reads correctly. `compressBody` skips binary memories and bodies under
  `storage.compression.minBytes`, and keeps a compressed body only when it actually came out smaller
  — so the feature can cost CPU but never storage. `UsedBytes` therefore counts compressed bodies,
  which is what makes compression translate into capacity-target headroom. SQLite
  (`modernc.org/sqlite`, pure Go): one database file (`hippocampus.db` in `storage.directory`)
  holding the `events` and `memories` tables; an empty directory (used by tests) selects an
  in-memory database. WAL mode makes every write durable as it happens — there is no snapshot
  cycle. The pool is capped at one connection, so queries must not be nested (collect rows,
  close, then act — the consolidation scans already work this way); that cap is a _per-process_
  pool limit and excludes nothing outside the process, which is what `lock.go` is for: a
  file-backed open takes an exclusive OS lock (`flock`/`LockFileEx`, via `golang.org/x/sys` in the
  two `lock_unix.go`/`lock_windows.go` files) on `hippocampus.lock` in `storage.directory` and
  refuses to start when another process holds it — WAL mode permits multi-process writers, so
  nothing in SQLite itself would stop a second instance running its own decay/eviction schedule
  over one store. The lock is the kernel's, not the file's existence, so a crashed holder leaves
  nothing to clear; the file's contents (pid/host/since) are diagnostics for the loser's error
  message only. Deliberately on a separate file rather than the database, so the read-only opens
  documented as safe beside a live service (`NewSQLiteReadOnly`, an operator's `sqlite3`) keep
  working — they take no lock. Postgres (`jackc/pgx` via
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
  table's original `CREATE TABLE` (currently `memories.is_summary`, the `group_name` column
  on both tables — named `group_name` because `GROUP` is reserved in every dialect, surfaced as
  `group` in the API — and the `metadata` column on both), so a database file written by an older
  version of the service is migrated in place on next startup (Postgres uses native
  `ADD COLUMN IF NOT EXISTS`; MySQL shares the probe with SQLite via `information_schema`).
  **`metadata` is NULL-able with no DEFAULT on all three dialects, unlike `group_name` beside it,
  and must stay that way**: SQLite's `json_extract` raises "malformed JSON" on an empty string but
  returns NULL for NULL, so an `''`-defaulted column would make the _first_ metadata-filtered query
  fail against every row written before the migration — a failure invisible to any fresh-database
  test, which is why `TestMetadataFilterAgainstAPreMigrationDatabase` builds an old-schema store and
  migrates it. The dialect-specific halves live in `db/metadata.go`: `metadataBytesExpr` (the byte
  length for the store's accounting, per-dialect because SQLite's `length()` counts characters on a
  text value and Postgres' `octet_length` has no definition for `jsonb`) and `metadataConditions`
  (the filter predicate, which binds the key as a parameter — safe only because the key charset in
  `types/metadata.go` excludes the characters that would let it escape the JSON path, and which
  needs an explicit `COLLATE utf8mb4_bin` on MySQL or values would match case-insensitively there
  and byte-for-byte everywhere else). Metadata bytes are counted at **four** sites that must agree
  or eviction chases a figure it cannot reach: `EvictMemories`, `usedBytesLiveRows`,
  `PreviewConsolidation`, and `RetainedStats`.
- `contract/` — the gRPC contract (`hippocampus.proto`) and generated code. RPCs cover
  event/memory CRUD plus `Sleep`, `Purge`, `MergeEvents`, `RecallMemories`,
  `ReplaceMemoriesWithSummary`, `GetSummarisationCandidates`, `SummariseMemories` (the embedded-LLM
  generate-and-replace), `PreviewConsolidation`/`ExplainConsolidation` (the forgetting-transparency
  pair: what a cycle would forget, and where an individual memory stands), `WhoAmI` (reports the
  caller's
  effective authorisation tier, so the web console can adapt), and the transfer/archive surface
  (`Export`, `Import`, `ImportBatch`, `Transfer`, `Clear`). Each RPC carries a
  `google.api.http` annotation mapping it onto a REST-ish `/v1/...` path (see
  [Configurability](docs/configuration.md#configurability) for the full mapping); `go generate
./contract` (directive in `generate.go`) turns those into `hippocampus.pb.gw.go` (the gateway)
  and `hippocampus.swagger.json` (the OpenAPI description, embedded via `swagger.go`).
  `contract/google/api/{annotations,http}.proto`
  are vendored copies of the googleapis definitions the annotations depend on.
- `search/` — the secondary content-search index (`search.Index` interface with three
  implementations: no-op, `SQL`, and `opensearch-go/v4`). **Two backends, selected in main.go:**
  OpenSearch when `opensearch.enabled`, otherwise `search.NewSQL` over the primary store — so
  `SearchMemories` works out of the box on the default (SQLite) deployment instead of failing
  closed, which is what it did while OpenSearch was the only backend. Only a driver with neither
  (`postgres`/`mysql` without OpenSearch) leaves the no-op in place, logged at startup and
  surfaced as `FailedPrecondition`.
  - `search/sql.go` — the store-backed backend. A thin adapter: `Search` delegates to
    `db.SearchMemoryIds`, and **every mutator is deliberately a no-op**, because the index is
    maintained inside the primary write rather than propagated to afterwards (wiring them up would
    double-index). It reaches the store through the `ContentStore` interface declared in the
    package, so `db` is not imported for its concrete type and the adapter is fakeable. `Rebuild`
    is a concrete method, not part of `Index`, like OpenSearch's `RecreateIndex`/`IndexMemorySync`.
    The `Index` doc comment was amended when this landed: async best-effort propagation is the
    OpenSearch implementation's _strategy_, not the contract, which promises only that mutators
    return without error.
  - The actual index lives in `db/search.go` (SQLite only — `ContentSearchAvailable` gates it):
    an **FTS5 virtual table**, available with no cgo and no new dependency because
    `modernc.org/sqlite` is built with `SQLITE_ENABLE_FTS5`. It is `content=''` +
    `contentless_delete=1`, i.e. **contentless** — it holds the inverted index and not a second
    copy of the bodies. That is load-bearing twice over: storing the text again would give back
    much of what body compression exists to save, and the obvious alternative (an external-content
    table over `memories.body`) is impossible since that column can hold a gzip stream. Every
    write therefore feeds it the **plain** body from inside the storage boundary, before
    `compressBody`. The FTS rowid is `memories.rowid`, which is what lets **deletes be handled by
    a single `AFTER DELETE` trigger** on `memories` keyed on `OLD.rowid` — covering consolidation,
    eviction, `DeleteMemories`, `DeleteEventMemories`, `Purge`, `Clear`, and import-replace with
    no call-site hooks and no possibility of drift. Inserts/updates are hooked explicitly in
    `CreateMemory`, `UpdateMemory`, `ReplaceMemoriesWithSummary`, and `ImportMemories` (the last
    via `reindexMemoryContent`, since an import is an upsert and a plain insert would leave a row
    matching both its old and new body); those are synchronous but **not** transactional, so a
    failure is logged and never fails the write. `initContentSearch` populates the index at
    startup when it is empty on a non-empty store, which is what makes an upgrade need no manual
    step. Query text is sanitised by `ftsMatchExpression` into quoted tokens joined by `OR` —
    never passed through, since FTS5's MATCH is a query language whose operators would otherwise
    be a syntax error or a way to reach past the caller's intent; `OR` is chosen to match the
    OpenSearch backend's `match` semantics so the two agree on which memories match, with bm25
    ranking favouring those matching more tokens.
  - `--backfill-search` without OpenSearch routes to `rebuildContentSearch` in `backfill.go`, a
    separate entry point because it **writes to the service's own database** (so it must not run
    beside a live instance, unlike the read-only OpenSearch backfill).
  - **Ranking** (`hippocampus/ranking.go`, `search.significanceWeight`/`search.recallWeight`,
    defaulting to 0.3/0.2) blends the store's own view of a memory — significance and recall count
    — into `SearchMemories`' result order, so the differentiator actually shapes retrieval instead
    of relevance alone deciding. `search.Index.Search` therefore returns `[]search.Hit` (id +
    score) rather than ids; **each backend normalises the score's direction** so higher is always
    better (the SQL backend flips FTS5's negative bm25 rank in `SearchMemoryHits`, OpenSearch's
    `_score` already is), while magnitudes stay incomparable between backends. The blend runs at
    the RPC layer, above both backends, deliberately: pushing it down would need significance and
    recall mirrored into the OpenSearch index (breaking one-way propagation, and ranking on a stale
    copy of a number that changes on every recall) and would let the backends drift on ordering.
    `normalise` divides by the set maximum rather than min-max rescaling — that is load-bearing,
    not stylistic: min-max maps the weakest candidate to 0 whatever the real gap, so two matches
    differing by one percent would look maximally different and significance would decide
    everything. Recall counts are `log1p`-damped before normalising (they are heavily skewed).
    Two consequences to preserve: the RPC **over-fetches** (`rankingOverFetch`) only when ranking
    is active, so the weights-zero path is exactly the pre-ranking one; and a reinforcing search
    recalls **only the returned page**, never the wider candidate set — hence `reinforceRanked`
    running after truncation rather than the old single `RecallMemories` over everything fetched,
    since recalling unseen candidates would reset decay clocks on the caller's behalf.
  - **Semantic search** (`SearchMemories`' `mode`: `keyword`/`semantic`/`hybrid`, default keyword so
    existing callers are unchanged) is **OpenSearch-only on every driver** — a deliberate trade, not
    an oversight: the embedded deployment gives up feature parity for having nothing to run
    alongside. `sqlite-vec` was rejected because it is cgo (costing the six-target single-runner
    cross-compile and the pure-Go static binaries) and, as of its still-open ANN issue, is _also_
    brute-force — so it would buy a constant factor, not scale. The two halves are independent and
    neither implies the other: the `embed` package produces vectors, OpenSearch's k-NN index stores
    and searches them; `main.go` **fails startup** if `ollama.embedding.enabled` is set without
    `opensearch.enabled`. `search.Query` carries either `Text` or `Vector` (the RPC layer embeds the
    query and passes it down, so `search/` never learns about an embedder), and `search.Doc` carries
    the vector. Vectors live **only in the index, never the primary store** — ~3 KiB per memory
    would compete for the capacity compression exists to save — so a rebuild re-embeds rather than
    re-reads. `hippocampus/fusion.go` fuses hybrid by **Reciprocal Rank Fusion**, deliberately a
    different technique from `ranking.go`'s max-normalised blend: a bm25 score and a cosine
    similarity share no scale, so only the orderings can be combined. Three traps the code guards
    and that must stay guarded: re-indexing without a vector **replaces** a document that had one
    (so every write-through goes through `Server.indexMemory`, including the reconcile sweep, which
    is why that sweep now re-embeds and is much more expensive); `index.knn` is a **static** setting,
    so an index predating semantic search cannot gain the field in place (`checkVectorField` detects
    it at startup and names `--backfill-search --reindex` as the fix); and the k-NN dimension is
    fixed at index creation, so `ollama.embedding.dimensions` is validated in `embed/ollama.go`
    against what the model actually returns. `WhoAmI` reports `search_modes` so clients feature-detect
    rather than probe-and-fail.
  - The OpenSearch backend (`opensearch.enabled`, off by default) is unchanged by any of the above.
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
    `deploy/compose/docker-compose.opensearch.yaml` runs the full stack.
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
- `db/link.go` — the **link graph**: `memory_links` and `event_links`, one directed row per edge
  (composite primary key, so a re-link re-weights rather than duplicating) plus a reverse index on
  `to_id`. Three things carry the design. (1) **Storage is directed, value is symmetric**: both ends
  of a link gain its significance, so every aggregate sums `from_id = ? OR to_id = ?` and the
  reverse index is what keeps the second half off a scan; direction survives only because a client
  may want to read it back. (2) The **denormalised aggregate** (`memories.link_significance`,
  `events.link_significance`, both in the covering index) is what the consolidation scans read, so
  they never join to this table and stay off the memory bodies. It is maintained by _recomputing_
  it for exactly the ids whose links changed, never by applying deltas — one statement, self-
  correcting, and the single point every mutation funnels through. (3) Links **must not dangle**,
  because a dangling edge would count significance for one end forever: the RPC layer checks both
  ends exist (`MissingIds` → NotFound), and `pruneLinks` runs inside every path that deletes a
  memory or event — the chokepoints are `deleteMemoriesIfUnrecalled` (covering consolidation,
  eviction and `Clear`), `deleteMemoriesByIds`, `DeleteEventMemories`/`ReplaceMemoriesWithSummary`
  (which read the ids first, since they delete by `event_id`), `DeleteEvent`/`DeleteEventIfEmpty`,
  and `Purge`. The RPC half is `hippocampus/link.go`; bounds are shared with events in
  `types/link.go` (128 links, 1,000,000 each, no self-links, no duplicates), and the damping in
  `linkContribution` is what makes those bounds safe rather than merely large. `Memory.links` and
  `Event.links` are **outbound only** — that is what keeps an export/import round trip from doubling
  every edge; `GetMemoryLinks`/`GetEventLinks` take a direction and default to both. Import applies
  links in a **second pass** after every row in the batch exists, because an archive routinely
  carries a link whose target appears later.
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
  (`grpc.health.v1.Health`, `/healthz`) never do — the gRPC side by a `/hippocampus.v1.Hippocampus/` prefix
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
  **Group scoping** (`groups.go`) is the orthogonal axis: a tier says what a caller may _do_, a scope
  says which records they may do it _to_. `Claims.Groups` (from the `groups` claim, or
  `auth.groupsClaim` for an IdP naming it differently) names the `group` labels a token may reach;
  `GroupsFromContext` returns `(groups, bound)` and **callers must branch on the bool, never on the
  slice's length** — an empty scope means the _whole store_, so reading it as "scoped to nothing"
  empties every read on an unauthenticated instance and reading the reverse hands a bound token
  everything. `GroupInScope` is the membership test, exact and byte-for-byte so it agrees with how
  the store compares `group_name` (MySQL needs its explicit `COLLATE utf8mb4_bin` for that).
  `NewGroupScopedVerifier` (`auth.requireGroupScope`) is a decorator in the `NewRevokingVerifier`
  mould, refusing a token that carries no scope — an unscoped token being the _most_ privileged
  shape there is, not the least. Enforcement is not here; see `hippocampus/scope.go`.
- `demo/` — a long-running load generator (`demo/generator`, its own `main` package) plus a
  launch script (`run.sh`) and a demo-tuned config. Bursty/slow/event-less writers, query and
  recall workers, and a mutator exercise every RPC; a watcher pauses writes while the database
  is at its size cap (default 1 GiB, `MAX_BYTES` env var overrides). The demo config compresses
  the decay clock (`unitsOfAgeInDays` 0.002 ≈ one age unit per 3 minutes) so forgetting,
  recall reinforcement, and the byte capacity target all play out within a session instead of
  over real days.
- `integrations/mcp/` — a standalone Model Context Protocol server (its own Go module, module path
  `github.com/fastbean-au/hippocampus/integrations/mcp`; `replace
github.com/fastbean-au/hippocampus => ../..`, so the modelcontextprotocol/go-sdk dependency tree
  stays out of the root build — **the root module does not import it**) that bridges an LLM host
  (Claude Desktop/Code, any MCP client) to a running
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
  `github.com/modelcontextprotocol/go-sdk/mcp`. Built/vetted/tested by its own `mcp-bridge` CI job
  (like `otel-exporter`), since the root module's `go build/test ./...` no longer descends into it.
  Ships as its own image (`Dockerfile` `target: mcp`, built from within the module directory),
  reachable over HTTP via the opt-in `mcp` compose profile; the release workflow cross-compiles the
  binary for every OS/arch onto the GitHub release and publishes the image to
  `ghcr.io/fastbean-au/hippocampus-mcp`. See `docs/mcp.md`.
- `integrations/` — self-contained client/edge subprojects, each a thin bridge rather than part of
  the core service. Each Go integration is a separate module whose dependency tree stays out of the
  root build (`mcp`, `cli`, `otel/hippocampusexporter`, `eventsource`), and one is a TypeScript
  project (`obsidian`).
  - `integrations/cli/` — the `hippo` command-line client (its own Go module, module path
    `github.com/fastbean-au/hippocampus/integrations/cli`; `replace
github.com/fastbean-au/hippocampus => ../..`, so its client dependency tree stays out of the
    root build — **the root module does not import it**). A thin, stateless client exposing the
    **full** RPC surface as noun-verb subcommands (`memory`/`event`/`summary` plus the admin
    `whoami`/`sleep`/`purge` and the data-movement `export`/`import`/`import-batch`/`transfer`/`clear`
    — unlike the MCP bridge it deliberately includes the destructive/bulk RPCs, since it is an
    operator tool and the service's auth tiers gate what a token may actually do). It talks to the
    service over **either** transport, selected by `--transport`: native gRPC (default) via
    `contract.NewHippocampusClient`, or the JSON/HTTP `/v1` gateway (`--transport http`) via
    `httpClient`, a hand-rolled implementation of the same generated `contract.HippocampusClient`
    interface (each method maps its RPC onto the gateway's method/path/body binding exactly as the
    `google.api.http` annotations declare, with protojson (un)marshalling and a generic
    protojson→query-param helper for the GET/DELETE routes; a non-2xx gateway body is turned back
    into a gRPC `status` error so codes/messages match across transports). Because both transports
    satisfy one interface, every command handler is written once. `main.go` holds the subcommand
    dispatch (a probe flag set with interspersing disabled locates the command so global flags may
    appear on either side of it) and the single viper read of the global connection flags
    (`HIPPOCAMPUS_*` env overridable — token, TLS trust options mirroring the MCP bridge, timeout,
    `--output text|json`); `commands.go` is the command registry + handlers, `output.go` the
    text/protojson renderer. Shell completion (`completion.go`) is driven off the same `commands()`
    registry so it never drifts: `hippo completion <bash|zsh|fish>` emits a script that calls a
    hidden `hippo __complete` at completion time (special-cased in `run()`, needs no service
    connection), computing subcommand/flag/enum-value candidates from the registry. Built/vetted/
    tested by its own `cli` CI job (self-contained: fake gRPC client plus an httptest gateway, no
    service container); the release cross-compiles the `hippo` binary for every OS/arch onto the
    GitHub release. See `docs/cli.md` and the module README.
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
    (`nats.go`, `paho.mqtt.golang`, `amqp091-go`, `segmentio/kafka-go`, `gorilla/websocket`) stay out
    of the root build —
    **the root module does not import it**. A shared `bridge/` core carries the reusable pieces: a
    broker-agnostic `Message`, the `Transformer` callback seam (`Transform(Message) ([]*contract.Memory,
error)`) with a `TransformerFunc` adapter and a configurable `DefaultTransformer` (payload→body,
    subject→group, fixed/header significance, optional base64/binary + truncation, future-timestamp
    clamping), `Store.Handle` (transform then `StoreMemory` each memory; a `Rejected` below-threshold
    memory is a success, a transform/transport failure is the adapter's cue to nack/redeliver), the
    gRPC `Dial` (bearer-token + TLS trust options, mirroring the MCP bridge; auth is either a static
    `--token` or the OIDC **client-credentials** grant in `bridge/oidc.go`, selected by a set
    `--oidc-client-id`, which mints and refreshes its own access tokens — a static token expires and
    then fails every write *silently* for as long as the daemon runs, which is why anything against
    an IdP-backed service wants the grant; config is validated eagerly but discovery is LAZY so an
    IdP blip does not stop a supervised bridge starting, and it deliberately matches the generators'
    implementation in the `hippocampus-gen` repo, down to the Auth0 audience quirk, so one Keycloak
    realm configures both), and `RegisterCommonFlags`
    (pflag only — each `cmd/*` main owns its viper reads, per the convention). The client seam
    (`hippocampusClient`, `bridge/store.go`) names **four** RPCs and no more — `StoreMemory`,
    `StoreEvent`, `RecallMemories`, `DeleteMemories` — and that unexported interface IS the module's
    statement of what a bridge may do to a store: `Dial` hands back the whole generated client, so
    this declaration is the only thing standing between an adapter and `Purge`. Beyond `Handle`,
    `bridge/recall.go` adds `Recall`/`Forget`/`EnsureEvent`/`HandleEvent`, which share one rule — **an
    id the store does not have is never an error** (recall is an `UPDATE ... WHERE id IN (...)` that
    matches nothing, a duplicate create is `AlreadyExists`, a delete reports `Ok false`) — and that
    rule is exactly what lets a reinforcing bridge hold no state. Two traps live there: `HandleEvent`
    absorbing `AlreadyExists` must still store the memories (an event is routinely opened by something
    other than the record that owns it, and returning early dropped that record's memory silently),
    and `DefaultTransformer`'s `MaxBodyBytes` must back up to a rune boundary (a proto3 string must be
    valid UTF-8, and a split rune fails to MARSHAL, which redelivery can never fix — a poison message
    retried forever). Both were found by running the Bluesky bridge against the live firehose, not by
    a test. Five adapters (`nats/`, `mqtt/`, `rabbitmq/`, `kafka/`, `bluesky/`), each a library
    `Bridge` (`New(Config, *bridge.Store)` + `Run(ctx)`) plus a `cmd/<broker>` runnable, with a
    connection seam injected so `Run` is unit-testable with fakes; delivery semantics match each
    broker (NATS at-most-once; MQTT/RabbitMQ/Kafka at-least-once via manual ack/commit; Bluesky
    at-least-once, cursor-gated). **`bluesky/` is the one that is not a message broker**: it consumes
    Jetstream (Bluesky's JSON projection of the atproto firehose, via `gorilla/websocket` — already an
    indirect dep, so it cost no new module) and is the only adapter that REINFORCES as well as writes.
    A post becomes a memory whose id is its `at://` URI; a like/repost/reply becomes a `RecallMemories`
    against that URI, so the mapping needs no map and no lookup, and every post arrives equally
    significant with only engagement differentiating what survives. **`--feed at://…`** swaps the post
    source from the firehose to a curated atproto FEED GENERATOR (HTTP `getFeed`, polled) while
    engagement keeps arriving on Jetstream — the feed decides what is stored, the firehose reports what
    was done with it, and they meet by URI with no correlation state; it trades volume for legibility
    (tens of posts/hour, all readable) so it suits a hosted demo where the firehose suits a local one.
    `--feed-backfill` seeds from the whole feed at startup and `--feed-seed-recalls` carries observed
    engagement across as `round(log1p(likes+reposts))` — the damping is load-bearing, since effective
    significance is LINEAR in recall count and a raw count would make one post unforgettable. Seeding
    is the only `ImportBatch` write (the one RPC carrying recall history) and happens once; polling
    uses `StoreMemories`, treating `AlreadyExists` as "already have it", which needs no bookmark and
    never rolls back live reinforcement — an upsert per poll would. The feed shares the Store's own
    Transformer (`Store.Transformer()`) so both sources filter identically by construction.
    **`--topic-links`** (`bluesky/topics.go`) relates posts with NO NLP: a news post's link card
    carries the article URL and a news URL's path is a hand-written SLUG, already tokenised on
    hyphens and chosen editorially, so terms come from splitting it (falling back to the body when
    there is no card). Two posts relate on >= `--topic-min-shared` terms, ignoring any term carried by
    more than `--topic-max-frequency-percent` of the index — the cheap stand-in for IDF, without
    which a section name relates everything. It relates ~a quarter of a live news feed, cross-outlet.
    This is what makes the service's `linkRecallPropagation` and `linkSignificanceWeight` do
    anything, since nothing else in the bridge creates links. Two constraints: the term index is the
    one genuinely STATEFUL thing here (bounded; unlike `rootCache` it is not just an optimisation, so
    losing it stops links being made — accepted, it is best-effort enrichment), and links are issued
    AFTER the write via `Store.Link` rather than attached to it, because a target must exist and in a
    forgetting store attaching them would let a just-consolidated neighbour fail the write; the
    backfill is the exception and attaches them to its `ImportBatch`, whose second pass resolves
    intra-batch targets. Its token
    must be **unscoped and
    writer-tier** (a group-scoped token makes an unknown id `NotFound` for the whole batch; a reader
    token does not reinforce). Recalls are batched (`--recall-batch-size/-window-ms`, best-effort by
    design — a lost like decays a memory slightly sooner, it does not make it wrong), `--events thread`
    opens an event per thread root (sparse on the open firehose; `--dids` is where threading gets
    interesting), and `--honour-deletes` defaults **on** because decay is about significance while
    deletion is about consent. Tests are network-free by default (NATS uses an embedded in-process
    server; bluesky's `consume`/`serve` run off a canned frame slice and its real dial is covered by a
    local `httptest` websocket server; MQTT/RabbitMQ/Jetstream real-connect paths are env-gated
    integration tests — `HIPPOCAMPUS_TEST_MQTT_BROKER`/`HIPPOCAMPUS_TEST_RABBITMQ_URL`/
    `HIPPOCAMPUS_TEST_JETSTREAM`, the last needing no container since Jetstream is public, and set in
    CI only on pushes to the default branch so a fork's PR never reaches Bluesky), every package
    ≥95% covered (`bridge` itself sits at ~91%, held down by `StartRuntime`). Built/vetted/tested by the `eventsource-bridges`
    CI job (like `otel-exporter`; the `docker` CI job also smoke-builds the five images). The release
    workflow cross-compiles all five `cmd` binaries (`hippocampus-<broker>-bridge`) onto the GitHub
    release and publishes one multi-arch image per broker to GHCR
    (`ghcr.io/fastbean-au/hippocampus-<broker>-bridge`) via a matrix over the one parameterised
    `integrations/eventsource/Dockerfile` (the `BROKER` build-arg selects `cmd/<broker>`; built with
    the repo root as context since the module's `replace` reaches the root contract). A bridge is an
    outbound client (dials broker + service), and since the instrumentation landed it also serves
    `/healthz`+`/readyz` on `--health-port` (8090, 0 disables) and exports `hippocampus.bridge.*`
    metrics — so the image's lack of an EXPOSE is now a default rather than a property of the design.
    It has no default CMD — each broker's required flags are passed after the image name. Readiness
    deliberately covers the SERVICE and not the broker: a broker unreachable at startup exits the
    process (visible as a restart) and a mid-run disconnect is the adapter's own to retry, whereas a
    bridge that cannot write looks exactly like a bridge with no traffic. See `docs/eventsource.md`
    and the module README.
  - `integrations/ingestor/` — the **ingestor** (TODO 67): stage data in an edge instance, and when
    an event **completes**, judge it against a CEL rules file and either promote it to a central
    instance, promote it after reducing it, or drop it — draining the edge either way. Its own Go
    module (module path `github.com/fastbean-au/hippocampus/integrations/ingestor`; `replace
github.com/fastbean-au/hippocampus => ../..`), which is what makes `github.com/google/cel-go`
    affordable — **the root module does not import it**. A client of two instances, not a service
    feature: the edge is a stock `hippocampus` binary, and the core needed only one additive field
    (`GetMemoriesRequest.event_id`/`has_event`). Five things carry the design. (1) **It holds no
    state.** `ImportBatch` is a full-state upsert by id, so promote-then-drain is at-least-once
    against an idempotent receiver and a crash between the two re-promotes identical rows; there is
    no cursor and no bookmark, because _the edge store is the queue_ and what it contains is exactly
    what has not been judged yet. (2) **Judgement happens at completion, not at ingest**, which is
    the whole answer to "how do rule changes reach events in flight" — an open event is judged by
    whatever rules are in force when it completes, and the only mechanism needed is one immutable
    ruleset snapshot per pass (`rules.Watcher`, an `atomic.Pointer` swap on an mtime poll, modelled
    on `auth/revocation.go` including bad-initial-load-fails-startup and
    bad-reload-keeps-the-last-good). (3) **Rules are compiled at load** against a declared CEL
    environment (`rules/env.go`'s `Event`/`Memory` structs, pinned by `TestDeclaredEnvironment`
    because the field set is a contract with every deployed rules file), bounded by a cost limit and
    a timeout, and an expression that _errors_ (the classic: an unguarded `event.metadata['k']`) does
    not match, is logged naming the rule, and does not stop the rules after it. (4) **The drain
    re-checks** the event's memory count before deleting, so a memory landing against an
    already-ended event is never deleted unjudged — `--settle-seconds` makes that rare and the
    re-check makes it correct; note the two reduction kinds disagree about that count
    (`keepTopN`/`minSignificance` choose what _crosses_ and leave the source untouched, `summarise`
    replaces memories on the source), which is why `reduce` returns both. (5) **An event over
    `--max-event-memories` is left unjudged** rather than judged on a truncated view of itself.
    (6) A `promote` rule may also carry a **`set` block** (`rules/set.go`) — CEL expressions for
    `significance`/`group`/`metadata` (plus `name`/`description` on the event), evaluated per event
    and per memory, which is what turns the gate from admit-or-not into admit-and-rank; significance
    is the number the central store's decay runs on, so this decides how long what crosses is kept.
    Four constraints carry it: only the **promoted copy** is written (the edge is drained anyway);
    the mutation runs **before** the reduction, so `keepTopN` ranks by the score the rule just set
    (and a `summarise` reduction scores the summary, being what crosses); every result is
    bounds-checked here against what the target would accept, because a value the target rejects
    fails the whole `ImportBatch` and the event then sits on the edge being re-refused every pass;
    and a failure **fails the event loudly** rather than falling back to the stored significance —
    unlike a *match* expression erroring, which merely does not match, there is no safe fallback for
    a rank the operator asked for. `metadata` merges rather than replaces (CEL has no map union).
    `promoter/` is driven in tests against two in-memory fake instances, so no service is needed.
    Built/vetted/tested by the `ingestor` CI job; the release cross-compiles `hippocampus-ingestor`
    and publishes `ghcr.io/fastbean-au/hippocampus-ingestor`. See `docs/ingestor.md` — in particular
    that **an edge must set `consolidation.minimumRetentionInDays` above the longest an event stays
    open**, or its own decay will forget in-flight events before the rules ever see them.
    Instrumented like the bridges (see `observability/` below): `hippocampus.ingestor.*` plus the
    shared client RED metrics, and `/healthz`+`/readyz` naming which of its two ends is unreachable.
- `observability/` — the shared OTEL bootstrap and probe endpoints, in the root module so the
  service, the ingestor and the four broker bridges use one implementation (it began as
  `cmd/hippocampus/observability.go` and was promoted, not copied; the integration modules already
  depend on the root module for the contract, and the root already carried the OTEL dependencies, so
  sharing costs neither side anything). Three pieces. `Init` installs the global tracer/meter
  providers and returns a flush func, unchanged from the service's version apart from a
  configurable `service.name`. `HealthServer` serves `/healthz` (liveness) and `/readyz` (a named
  map of dependency checks, cached so a probe cannot become its own load), with `GRPCHealthCheck`
  probing `grpc.health.v1.Health` — chosen because it is exempt from the auth interceptor, touches
  no data, and is driven on the service side by its own database readiness, so "ready" means the far
  end can serve rather than that a socket opened. `UnaryClientMetricsInterceptor` records the
  client-side RED metrics (`hippocampus.client.rpc.requests`/`.duration`) for everything that dials
  the service, classifying `outcome` exactly as `rpcmetrics.go` does so the error rate means one
  thing on both sides. **Tenancy** (`WithGroup`, `GroupAttribute`) is a per-process label set once at
  `Init` and stamped on both the resource and each metric: the duplication exists because the
  OTLP→Prometheus translation puts resource attributes in `target_info`, and it is affordable only
  because the value is static — reading a group off each record would be unbounded (a bridge's group
  defaults to the message subject), which is why that shape is refused. No **service** metric carries
  a group; this is the client side only, and item 60.1 records why the service side stayed out.

## Conventions in this repo

- Logging is **logrus** (not zerolog), typically with a `log.Trace("func() ...")` entry line at the
  top of functions — match this existing style rather than global preferences.
- Errors are logged where they occur and returned unwrapped with `fmt.Errorf`.
- Exactly one instance may consolidate a given store. SQLite is single-instance (embedded DB),
  enforced by the `hippocampus.lock` file lock described above; on
  the `postgres`/`mysql` drivers a shared database can have one consolidating instance
  (`consolidation.enabled: true`, holds the lock) plus read/write replicas
  (`consolidation.enabled: false`, skip the lock, reject the `Sleep` RPC) — horizontal scaling.
  Authentication (JWT bearer tokens) and TLS
  are both optional and disabled by default; see [Authentication](docs/configuration.md#authentication)
  and [TLS](docs/configuration.md#tls).
- **A shared store is a shared trust domain unless tokens are group-scoped.** `group` is a label
  with no access-control meaning of its own; binding it to a token's `groups` claim
  ([Group scoping](docs/configuration.md#group-scoping)) makes it a **soft** partition — records are
  scoped, but the decay dynamics stay store-global, so a busy group still influences what another
  forgets. Hard isolation is still one instance per tenant, which is what item 9 and item 60.1 both
  concluded. Anything new that reads or writes stored records must therefore decide how it honours a
  caller's scope, and say so in `hippocampus/scope.go` — the drift guard will not let a new RPC
  through without it.
