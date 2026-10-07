# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.
It is the short version. Why things are the way they are - the rejected alternatives, the incidents
behind each guard - is in the [design record](docs/design/README.md), which an answer about a
subsystem should read before changing it.

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
(`llm.enabled`, off by default — the `summarise` package) lets the service author the
summary itself: the `SummariseMemories` RPC generates and replaces in one call, and
`llm.autoSummarise` does it automatically for the scan's candidates during sleep. Every RPC is
also reachable as a
JSON/HTTP endpoint under `/v1` via an in-process grpc-gateway (`gateway.port`, 0 disables). Both
transports can require a signed JWT bearer token (`auth.method`: `none`/`hmac`/`idp`) and TLS
(`tls.enabled`); both are off by default.

## Commands

- Build: `go build ./...`. Test: `go test ./...` (one test: `go test ./hippocampus -run TestName`).
- Run: `go run ./cmd/hippocampus -c config.json`. Without a config file it starts on built-in
  defaults; `--gateway-port 8080` turns on the JSON gateway and the console at `/ui`.
- Pre-commit: `hooks/pre-commit` (git runs it through `core.hooksPath hooks`). It runs tidy, gofmt,
  vet, golangci-lint (the same version CI's `lint` job installs) and the root module's tests with
  coverage. It does **not** test the integration
  modules - run `go test ./...` in each one you changed. Prepend `$(go env GOPATH)/bin` to `PATH` if
  `golangci-lint`, `buf` or the protoc plugins are "not found".
- Integration modules, each tested from its own directory: `integrations/cli` (`hippo`),
  `integrations/mcp`, `integrations/eventsource` (`cmd/<broker>`), `integrations/ingestor`,
  `integrations/objectstore`. Python client: `cd integrations/python && pip install -e '.[dev]' &&
  python -m pytest`.
- Server dialects: `HIPPOCAMPUS_TEST_DIALECT=postgres go test ./db ./hippocampus` (or `mysql`), with
  `HIPPOCAMPUS_TEST_POSTGRES_DSN`, or `HIPPOCAMPUS_TEST_MYSQL_DSN` plus
  `HIPPOCAMPUS_TEST_MYSQL_ADMIN_DSN`. Run it after any change under `db/`.
- Regenerate the contract: `go generate ./contract`. Breaking-change check: `cd contract && buf
  breaking --against '../.git#tag=<previous tag>,subdir=contract' --path hippocampus.proto`.
- JavaScript: `node --test` in `cmd/hippocampus/webuitest` (console) and `cmd/config-wizard/wizardtest`
  (wizard). No dependencies to install.
- Benchmarks (on demand, not CI-gated): `go test ./db -bench . -run XXX`.
- Fuzz one target: `go test ./hippocampus -run '^$' -fuzz '^FuzzRedactEndpoint$' -fuzztime 2m`.
- Vulnerability scan: `scripts/govulncheck.sh`. Grafana alert provisioning: `scripts/check-grafana-alerts.sh`.
- Demo soak: `./demo/run.sh`. Compose stacks: see `deploy/compose/README.md`. Kubernetes:
  `kubectl apply -k deploy/k8s/overlays/<sqlite|postgres|mysql>`.
- Offline modes of the service binary, each runs and exits: `--version`, `--check-config`,
  `--schema-version`, `--mint-token --client-id <id> --role <tier> --ttl 24h`, `--backup <path>`,
  `--backfill-search [--reindex]`.
- Release: `scripts/release.sh --minor` (it commits and tags, and does not push; see `RELEASE.md`).
- Reclaim disk: `scripts/cleanup.sh --dry-run`, then without the flag. It never touches
  `~/.hippocampus`.
- Family of repositories: `scripts/family-status.py --check` (read-only); `apply-rulesets.sh`,
  `setup-discussions.sh` and `social-preview.py` administer the satellites.

The long form of every command, with its design notes, is [docs/design/tooling.md](docs/design/tooling.md).

## Package map

| Path | What it is | Design record |
| :--- | :--------- | :------------ |
| `cmd/hippocampus/` | the service binary: bootstrap, interceptors, gateway, RED metrics, the embedded console, the offline modes. All config is read here; `serverconfig.go` builds the `hippocampus.Config` the service takes | [service](docs/design/service.md) |
| `cmd/config-wizard/` | the browser-based configuration and deployment wizard | [config-wizard](docs/design/config-wizard.md) |
| `hippocampus/` | the gRPC service: the sleep cycle, the transparency RPCs, callbacks, topology, scope, transfer | [consolidation](docs/design/consolidation.md), [topology](docs/design/topology.md), [scope-and-deletion](docs/design/scope-and-deletion.md) |
| `db/` | the storage layer: one `DB` over SQLite, PostgreSQL and MySQL; the schema ledger; the link graph | [storage](docs/design/storage.md) |
| `db/dbtest/` | opens a store on the dialect `HIPPOCAMPUS_TEST_DIALECT` selects, for other packages' tests | [storage](docs/design/storage.md) |
| `contract/` | `hippocampus.proto` and its generated code, the gateway and the OpenAPI document | [contract](docs/design/contract.md) |
| `types/` | request validation and proto/row conversion | [contract](docs/design/contract.md) |
| `search/` | the content index: no-op, the store's own (SQL) and OpenSearch | [search](docs/design/search.md) |
| `summarise/`, `embed/` | the optional LLM summariser and embedder (Ollama or OpenAI-compatible) | [llm](docs/design/llm.md) |
| `archive/` | the export archive format and the object stores (S3 or a directory) | [archive](docs/design/archive.md) |
| `auth/` | token verification, the tier policy table, group scope claims | [auth](docs/design/auth.md) |
| `notify/` | the outbound callback sink (HTTP, signed) behind `callbacks.*` | [consolidation](docs/design/consolidation.md) |
| `ratelimit/` | the token-bucket hierarchy behind `rateLimit.*` | - |
| `stats/` | the periodic count log line and count gauges | [service](docs/design/service.md) |
| `observability/` | OTEL bootstrap, the Prometheus reader, `/healthz`/`/readyz`, client RED metrics | [observability](docs/design/observability.md) |
| `dial/` | the one client connection every integration uses (token, OIDC, TLS) | [integrations](docs/design/integrations.md) |
| `internal/flagdocs/` | test helper holding integration flags to their doc pages | - |
| `integrations/` | CLI, MCP bridge, Python client, broker bridges, ingestor, object-storage agents; separate modules | [integrations](docs/design/integrations.md) |
| `demo/`, `examples/` | the load generator and soak stack; runnable client examples | [tooling](docs/design/tooling.md) |
| `deploy/` | compose, k8s, systemd, launchd, alert rules | each directory's README |
| `hooks/` | `pre-commit` | - |
| `scripts/` | release, cleanup, vulnerability scan, alert check, family administration | [tooling](docs/design/tooling.md) |

## Do not break

Each of these has failed before or would fail silently. The test named beside it is what catches it.

- **`UsedBytes` stays a live-row estimate on the server drivers**, the exact complement of
  `EvictMemories`' freed-bytes estimate. A file-size measure never drops after a delete, so eviction
  would chase it forever. `TestUsedBytesLiveRows`, `TestRowOverheadMatchesRealStorage`.
- **Metadata bytes are counted the same way at four sites**: `EvictMemories`, `usedBytesLiveRows`,
  `PreviewConsolidation` and `RetainedStats`. `TestMetadataCountsTowardEvictionBytes` covers the
  first pair; the other two are held by review.
- **`metadata` stays NULL-able with no DEFAULT** on every dialect, or the first metadata filter fails
  on every pre-migration row. `TestMetadataFilterAgainstAPreMigrationDatabase`.
- **Migration versions are never renumbered or reused**; a new migration appends.
  `TestMigrationVersionsAreStable`. Every migration detects its own completion and re-runs each
  start: `TestSchemaHealsARevertedMigration`.
- **Dialect knowledge stays in `db/dialect.go`** (plus `metadata.go`, `search_dialect.go`).
  `TestDialectKnowledgeIsConfined`. Shared `db` tests open through `newTestDB`:
  `TestSharedSuiteOpensThroughNewTestDB`.
- **The consolidation scans never read a memory body**; they stay on the covering index. No test
  gates this - `db/bench_test.go` shows it. Never call a DB method from inside a scan callback on
  SQLite: the pool has one connection and it deadlocks.
- **Tables the capacity target excludes stay excluded** (forgotten log, search outbox, callback
  queue). `TestAncillaryStorageReportsWhatUsedBytesExcludes`,
  `TestCallbackQueueIsExcludedFromUsedBytes`.
- **The preview agrees with a real cycle.** `TestPreviewMatchesASleepCycle`.
- **The `/hippocampus.v1.Hippocampus/` prefix** has hand-written copies in `auth/grpc.go`,
  `cmd/hippocampus/rpcmetrics.go` and `hippocampus/server.go`. A stale copy fails **open**.
  `TestServicePrefixMatchesDescriptor` (one per package).
- **Every RPC has a tier, a scope mode and an isolation subtest.** `TestPoliciesCoverEveryRPC`,
  `TestScopesCoverEveryRPC`, `TestEveryRPCIsCoveredByIsolationTest`.
- **An out-of-scope id the caller named reports `NotFound`**, never `PermissionDenied`, and an
  unnamed one is dropped silently. `TestGroupScopeIsolation*`.
- **The `hippocampus` package reads no configuration**: `New` takes a `Config` that
  `cmd/hippocampus/serverconfig.go` builds. `TestHippocampusReadsNoConfiguration`, and
  `TestServerConfigFillsEveryField` for an assignment forgotten there.
- **`hippocampus` tests run in parallel unless they say why not.** A test that touches the global
  logger, the OTEL provider or a package timing variable is sequential, with a `Not parallel:` line
  in its doc comment; every other test calls `t.Parallel()` first.
  `TestEveryTestDecidesWhetherItIsParallel`.
- **Every config key is read, defaulted, offered and documented.** `TestEveryConfigKeyIsDocumented`
  (a row in the generated index, `TestConfigIndex`), `TestEveryDocumentedConfigKeyIsRead`,
  `TestWizardOffersEveryConfigKey`, `defaults_test.go` in the wizard.
- **The built-in defaults form a valid, forgetting configuration**, and every shipped config is
  valid. `TestStartupDefaultsAreAValidConfiguration`, `TestShippedConfigsAreValid`,
  `TestShippedConfigsCoverEveryDriver`.
- **The contract is documented and routed.** `TestContractIsCommented`,
  `TestRouteTableMatchesTheContract` (against `docs/api.md`).
- **The alert rules exist twice and agree**, and name only instruments that exist.
  `TestAlertRulesMatchAcrossFiles`, `TestAlertRulesReferenceExportedMetrics`,
  `TestScrapeNamesMatchTheAlertRules`; the counts in prose are held by
  `TestDesignRecordRuleCountIsCurrent`.
- **The MCP tool set is a security statement**: no destructive, bulk or enumerating RPC.
  `TestServer_EndToEnd`, `TestEveryToolIsDocumented`.
- **Documented surfaces match the code.** `TestEveryCommandIsDocumented` (CLI),
  `TestEveryFlagIsDocumented` (each integration command), `TestEveryInstrumentIsDocumented`,
  `TestDocumentedMintCommandsCarryARole`, `TestEveryPackageHasADocComment`.
- **Compose ports publish on an address** (`PUBLISH_ADDRESS`, loopback by default).
  `TestComposePortsBindAnAddress`. Kubernetes overlays pin the released image:
  `TestKubernetesOverlaysPinTheImage`.

`TestClaudeMdNamesRealTests` checks that every test named in this file exists.

## Adding an RPC or a config key

Both have a checklist in [CONTRIBUTING.md](CONTRIBUTING.md) - [adding an
RPC](CONTRIBUTING.md#adding-an-rpc) and [adding a configuration
key](CONTRIBUTING.md#adding-a-configuration-key). Read the relevant one before starting either: each
step names the file and the guard that catches it being missed.

## Conventions

- **Australian English** everywhere, identifiers and config keys included. Protocol and stdlib names
  keep their spelling (`Authorization` header, `codes.Canceled`).
- **Logging is logrus** (not zerolog), usually with a `log.Trace("func() ...")` line at the top of
  a function. Match the surrounding code rather than global preferences.
- **Errors** are logged where they occur and returned with `fmt.Errorf`, using `%w` when they wrap a
  cause. An error reaching a caller goes through `mapError`, which passes a gRPC status through and
  masks everything else as `Internal`.
- **Metric, trace and log attributes** are snake_case and never high-cardinality: no ids, no group
  names, no client ids.
- **Exactly one instance consolidates a store.** SQLite is single-instance (the `hippocampus.lock`
  file lock). On PostgreSQL and MySQL one instance holds the consolidator lock and the rest are
  replicas, optionally `consolidation.standby` to take over. Read the role through
  `consolidating()`.
- **A shared store is one trust domain unless tokens are group-scoped**, and even then the partition
  is soft: the decay dynamics stay store-global. Anything new that reads or writes stored records
  declares how it honours a caller's scope in `hippocampus/scope.go`.
- **Bug fixes start with a failing test.** A drift guard's failure message names what to update;
  update the other copy rather than loosening the check.
- **CHANGELOG entries** are a few lines and a link. New work items go in `TODO-3.md`.
