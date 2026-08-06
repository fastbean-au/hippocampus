# Changelog

All notable changes to Hippocampus are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[semantic versioning](https://semver.org/spec/v2.0.0.html).

## Compatibility

**Hippocampus is pre-1.0.** Under semver that permits a breaking change in any minor release, and
several have shipped. Until 1.0, read the **Breaking** section of every release you skip over.

What each version number covers:

- **The gRPC contract** (`contract/hippocampus.proto`) and the `/v1` JSON gateway it generates.
  Compatibility here is enforced mechanically: the `proto-breaking` CI job runs `buf breaking`
  against the previous release tag, so an accidental field renumbering or a removed RPC fails the
  build rather than shipping. A deliberate break is listed under **Breaking** below.
- **Configuration keys.** A removed or renamed key is a breaking change. A new key always carries a
  default that preserves the previous behaviour, so an existing `config.json` keeps working.
- **The stored database.** Schema additions are migrated in place on startup, so a store written by
  an older version opens on a newer one. Downgrading is not supported.
- **The archive format** (`Export`/`Import`) is versioned in its own header, independently of the
  release version.

Not covered: the embedded web console, the demo stack, the Grafana dashboard, and anything under
`docs/`. Each integration under `integrations/` (the MCP bridge, the `hippo` CLI, the event-source
bridges, the OTEL collector exporter) is released from this same tag and tracks the service; the
Obsidian plugin has its own `obsidian-v*` tags and its own version line.

## [Unreleased]

### Breaking

- **The proto package is now `hippocampus.v1`, was `proto`.** Taken deliberately, and now rather
  than after 1.0, so that a future v2 of the contract can be served beside v1 instead of replacing
  it — a namespace with no version in it leaves no room to do that, and the rename only gets more
  expensive as more clients are generated.
  - gRPC method paths change from `/proto.Hippocampus/<Method>` to
    `/hippocampus.v1.Hippocampus/<Method>`. Anything naming a method in full — a grpcurl
    invocation, a service-mesh route, an authorisation policy outside Hippocampus, a metrics
    relabelling rule — must be updated.
  - Generated clients must be regenerated. The Go package is unaffected (`option go_package` is
    unchanged, so it remains `contract`), as are the Go type names.
  - `/v1` gateway paths, JSON field names, and request/response bodies are **unchanged**. An HTTP
    client — including the Obsidian plugin — needs no change.
  - The OpenAPI document's schema names change (`protoMemory` → `v1Memory`, and so on). Generated
    OpenAPI clients must be regenerated; the paths and payloads they call are the same.
  - Stored data is unaffected: neither the database nor the archive format records the proto
    package.

### Added

- `CHANGELOG.md` (this file), backfilled over every release, and the compatibility policy above.
- A `proto-breaking` CI job running `buf breaking` on `contract/hippocampus.proto` against the
  previous release tag (`contract/buf.yaml`), so an accidental contract break fails CI.
- `PreviewConsolidation` — a read-only dry run of a sleep cycle (`GET /v1/sleep/preview`,
  `hippo sleep --dry-run`) reporting what would be forgotten and deleting nothing.
- `ExplainConsolidation` — per-memory decay transparency: computed value, the pressure-scaled
  threshold, the retention/minimum-age overrides, days until forgotten, and the current decay
  curve. The web console gained a **Decay** tab that renders it and computes no decay maths of its
  own.
- Built-in content search on the SQLite driver, via an FTS5 index maintained inside the primary
  write. `SearchMemories` now works out of the box on the default deployment instead of failing
  closed when OpenSearch is not configured.
- Semantic and hybrid search (`SearchMemories`' `mode`) over an OpenSearch k-NN index, with a text
  embedder (`ollama.embedding.*`). OpenSearch-only; keyword remains the default, so existing
  callers are unchanged.
- Search ranking that blends a memory's significance and recall count with relevance
  (`search.significanceWeight`, `search.recallWeight`).
- RPC-level RED metrics — `hippocampus.rpc.requests` and `hippocampus.rpc.duration`, on both the
  gRPC and gateway transports — plus a **Requests (RED)** row on the dashboard.
- Shipped alert rules: `deploy/observability/prometheus-alerts.yaml` (portable Prometheus) and the
  same nine rules as Grafana-managed rules provisioned into the compose observability profile.

### Changed

- First run after a clone no longer needs a config file: an absent `./config.json` starts the
  service on built-in defaults (a `--config_file` given explicitly must still exist).

## [0.23.0] - 2026-08-05

### Added

- Optional compression of memory bodies (`storage.compression`, on by default). Compression lives
  entirely at the storage boundary, the decision is recorded per row so the setting is safe to
  change on a live store, and a body is kept compressed only when it actually came out smaller.

## [0.22.0] - 2026-08-05

### Added

- A browser-based configuration and deployment wizard (`cmd/config-wizard`) that builds a
  `config.json`, an environment file for the secret-typed keys, and Compose/Kubernetes/systemd/
  launchd artefacts entirely in the browser.

## [0.21.0] - 2026-08-05

### Added

- Homebrew installation via the `fastbean-au/homebrew-tap` formulae, auto-bumped on release.
- Native systemd deployment with `.deb`/`.rpm` packaging.
- A macOS launchd LaunchAgent.

### Changed

- `docker/` moved to `deploy/compose/`.

## [0.20.0] - 2026-08-04

### Added

- The `hippo` command-line client (`integrations/cli`), covering the full RPC surface over either
  transport.
- Kubernetes kick-start manifests (`deploy/k8s`, Kustomize base plus SQLite and Postgres overlays).

### Changed

- The MCP bridge moved to `integrations/mcp` and became its own Go module, keeping its dependency
  tree out of the root build.

### Fixed

- Broker-readiness gating in the event-source CI job.

## [0.19.0] - 2026-08-03

### Added

- Event-sourcing broker bridges for NATS, MQTT, RabbitMQ, and Kafka
  (`integrations/eventsource`), each consuming from a broker and storing every message as a memory.

### Fixed

- Integration docs: port alignment and container networking.

## [0.18.0] - 2026-08-01

### Changed

- Australian English throughout the repo, in documentation _and_ code — identifiers, config keys,
  proto fields, and metric names. Protocol and standard-library terms (the `Authorization` header,
  `codes.Canceled`, and so on) are untouched.
- Multi-arch container builds, and OCI support alongside Docker.
- The hosted showcase moved to the separate `hippocampus-demo-site` repository.

## [0.17.0] - 2026-07-28

### Added

- Optional embedded-LLM summarisation (`ollama.*`, off by default): the `SummariseMemories` RPC
  generates and replaces in one call, and `ollama.autoSummarise` does the same for the sleep
  cycle's candidates.

## [0.16.0] - 2026-07-28

### Added

- Server-side OIDC login (`auth.oauth2`) for SSO, as a confidential client.
- A lite showcase stack sized for a GCP e2-micro, and a phased walkthrough for it.

## [0.15.0] - 2026-07-27

### Added

- OIDC Authorization Code + PKCE login in the web console, with front-channel configuration served
  at `/ui/config`.
- Showcase compose stacks with Keycloak realm provisioning, and a GCP deployment runbook.

### Fixed

- IdP role claims are resolved literally before being resolved as a nested path.

## [0.14.3] - 2026-07-26

### Added

- `update_memory` and `delete_memories` on the MCP bridge.

## [0.14.2] - 2026-07-26

### Fixed

- A minor bug in the search response.

## [0.14.1] - 2026-07-26

### Added

- The `WhoAmI` RPC, reporting the caller's effective authorisation tier, and a role-aware web
  console that hides the controls the caller may not use.

## [0.14.0] - 2026-07-26

### Added

- Role-based authorisation: a `reader` ⊂ `writer` ⊂ `admin` tier hierarchy with one policy table
  enforced identically on both transports.
- A release workflow for the Obsidian plugin, and OTEL collector image plus plugin assets published
  on release.

## [0.13.0] - 2026-07-25

### Changed

- The integrations became first-class components, each with its own build, tests, and release
  artefacts.

## [0.12.0] - 2026-07-25

### Added

- An Obsidian plugin (`integrations/obsidian`) using Hippocampus as a vault memory layer over the
  `/v1` gateway.

### Changed

- `otel/` moved under `integrations/`.

## [0.11.0] - 2026-07-24

### Added

- An OpenTelemetry Collector logs exporter that turns each log record into a `StoreMemory` call.

## [0.10.1] - 2026-07-23

### Fixed

- The MCP bridge's release packaging.

## [0.10.0] - 2026-07-23

### Added

- `hippocampus-mcp`, a Model Context Protocol server bridging an LLM host to a running instance,
  with its own image and release binaries.

## [0.9.2] - 2026-07-22

### Changed

- Test coverage, and a README layout fix.

## [0.9.1] - 2026-07-22

### Changed

- An overhauled README, and observability on by default in the demo script.

## [0.9.0] - 2026-07-21

### Added

- An extremum option on `GetMemories`, mirroring `GetEvents`.

## [0.8.1] - 2026-07-20

### Changed

- `run()` extracted from `main()` so the serve lifecycle is covered by tests.

## [0.8.0] - 2026-07-20

### Added

- A default query timeout at the storage layer (`storage.queryTimeoutSeconds`).

### Changed

- A hardened security surface and a broadened set of configurable keys, from the production
  readiness review.

### Fixed

- Correctness fixes across the sleep cycle and the RPC surface.

## [0.7.0] - 2026-07-19

### Added

- An optional extremum on `GetEvents`, returning the events at the highest significance.
- TLS configuration for the OpenSearch connection, plus a secured reference deployment.

## [0.6.1] - 2026-07-18

### Fixed

- A failing MySQL test.

## [0.6.0] - 2026-07-18

### Added

- `consolidation.minimumRetentionInDays` — a hard retention floor that neither value-based
  consolidation nor capacity eviction may take a memory inside.

## [0.5.0] - 2026-07-18

### Added

- Relative significance placement: rank a memory or event between two existing values rather than
  choosing a number.

## [0.4.0] - 2026-07-18

### Added

- Self-healing content search: the index worker retries transient cluster failures, and the
  consolidating instance runs a periodic reconciliation sweep.

### Fixed

- MySQL write deadlocks are retried, and exhausted conflicts map to `Aborted` so clients re-issue
  rather than read a lost write.

## [0.3.0] - 2026-07-17

### Added

- Throughput-sweep tooling and a high-throughput performance write-up.

### Fixed

- `autoSleep` livelock: with `consolidation.walTriggerBytes` set, the timed sleep cycle never fired.
- The demo generator's burst clock-skew overflow, and an event-not-found mapping to `NotFound`.

## [0.2.1] - 2026-07-17

### Added

- A demonstrations guide.

### Fixed

- The pre-commit hook, hardened.

## [0.2.0] - 2026-07-16

### Added

- HMAC signing-key rotation, and token/client revocation via a reloadable revocation list.

### Changed

- `context.Context` threaded through `db.Store`, so an RPC's deadline and cancellation reach the
  driver.

## [0.1.0] - 2026-07-16

The first tagged release. Hippocampus was already feature-complete at this point: memories and
events with significance-based decay, the six consolidation algorithms, the sleep cycle with
capacity eviction and summarisation candidates, SQLite/PostgreSQL/MySQL storage, the gRPC service
with its `/v1` gateway, optional JWT auth and TLS, the OpenSearch secondary index, the archive and
transfer RPCs, the embedded web console, and OTEL tracing and metrics.

This release added the delivery and production-readiness layer around it:

### Added

- A tag-driven release workflow publishing to GHCR, and `RELEASE.md`.
- Version identification (`--version`, the `/healthz` body, the OTEL `service.version` attribute).
- Per-request logging, stats caching, and bounded transfer manifests.
- Connection-pool limits, byte-aware `Transfer` batching, and `HIPPOCAMPUS_*` environment overrides
  for secrets.
- An otel-lgtm observability profile with a provisioned dashboard, and bytes-evicted and body-size
  metrics.

### Fixed

- A stored XSS in the embedded web console, plus auth, TLS, and gateway hardening.

[Unreleased]: https://github.com/fastbean-au/hippocampus/compare/v0.23.0...HEAD
[0.23.0]: https://github.com/fastbean-au/hippocampus/compare/v0.22.0...v0.23.0
[0.22.0]: https://github.com/fastbean-au/hippocampus/compare/v0.21.0...v0.22.0
[0.21.0]: https://github.com/fastbean-au/hippocampus/compare/v0.20.0...v0.21.0
[0.20.0]: https://github.com/fastbean-au/hippocampus/compare/v0.19.0...v0.20.0
[0.19.0]: https://github.com/fastbean-au/hippocampus/compare/v0.18.0...v0.19.0
[0.18.0]: https://github.com/fastbean-au/hippocampus/compare/v0.17.0...v0.18.0
[0.17.0]: https://github.com/fastbean-au/hippocampus/compare/v0.16.0...v0.17.0
[0.16.0]: https://github.com/fastbean-au/hippocampus/compare/v0.15.0...v0.16.0
[0.15.0]: https://github.com/fastbean-au/hippocampus/compare/v0.14.3...v0.15.0
[0.14.3]: https://github.com/fastbean-au/hippocampus/compare/v0.14.2...v0.14.3
[0.14.2]: https://github.com/fastbean-au/hippocampus/compare/v0.14.1...v0.14.2
[0.14.1]: https://github.com/fastbean-au/hippocampus/compare/v0.14.0...v0.14.1
[0.14.0]: https://github.com/fastbean-au/hippocampus/compare/v0.13.0...v0.14.0
[0.13.0]: https://github.com/fastbean-au/hippocampus/compare/v0.12.0...v0.13.0
[0.12.0]: https://github.com/fastbean-au/hippocampus/compare/v0.11.0...v0.12.0
[0.11.0]: https://github.com/fastbean-au/hippocampus/compare/v0.10.1...v0.11.0
[0.10.1]: https://github.com/fastbean-au/hippocampus/compare/v0.10.0...v0.10.1
[0.10.0]: https://github.com/fastbean-au/hippocampus/compare/v0.9.2...v0.10.0
[0.9.2]: https://github.com/fastbean-au/hippocampus/compare/v0.9.1...v0.9.2
[0.9.1]: https://github.com/fastbean-au/hippocampus/compare/v0.9.0...v0.9.1
[0.9.0]: https://github.com/fastbean-au/hippocampus/compare/v0.8.1...v0.9.0
[0.8.1]: https://github.com/fastbean-au/hippocampus/compare/v0.8.0...v0.8.1
[0.8.0]: https://github.com/fastbean-au/hippocampus/compare/v0.7.0...v0.8.0
[0.7.0]: https://github.com/fastbean-au/hippocampus/compare/v0.6.1...v0.7.0
[0.6.1]: https://github.com/fastbean-au/hippocampus/compare/v0.6.0...v0.6.1
[0.6.0]: https://github.com/fastbean-au/hippocampus/compare/v0.5.0...v0.6.0
[0.5.0]: https://github.com/fastbean-au/hippocampus/compare/v0.4.0...v0.5.0
[0.4.0]: https://github.com/fastbean-au/hippocampus/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/fastbean-au/hippocampus/compare/v0.2.1...v0.3.0
[0.2.1]: https://github.com/fastbean-au/hippocampus/compare/v0.2.0...v0.2.1
[0.2.0]: https://github.com/fastbean-au/hippocampus/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/fastbean-au/hippocampus/releases/tag/v0.1.0
