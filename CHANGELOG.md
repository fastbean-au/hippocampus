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
  build rather than shipping. A deliberate break is listed under **Breaking** below. The job always
  runs and always reports, but its verdict is advisory in the two cases where the version number
  already permits a break — a pre-1.0 baseline, or a major increment declared in the `[Unreleased]`
  heading (see [RELEASE.md](RELEASE.md#compatibility)) — so the report is worth reading even when
  the build is green.
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

- **Event relationships became links, and the contribution is now damped.** Memory-to-memory links
  and event-to-event relationships turned out to be the same mechanism, so they are now one:
  the `Link` message, the `link_significance` aggregate, and one weight. Four separate breaks:
  - **Contract.** `Relationship` → `Link` (its `event_id` field → `id`); `Event.relationships` →
    `Event.links`; `Event.relationship_significance` → `Event.link_significance`. Field numbers and
    types are unchanged, so the wire format is identical — only the names break. Generated clients
    must be regenerated. `buf breaking` reports the removal of the `Relationship` message (the field
    renames are not wire-breaking and it does not flag them); that report is expected.
  - **Configuration.** `consolidation.relationshipSignificanceWeight` →
    `consolidation.linkSignificanceWeight`. The old key is not read; there is no alias.
  - **Behaviour — the weight now means something different, and must be re-tuned.** The
    contribution changed from `weight × total` to `weight × ln(1 + total)`. A relationship total of
    1,280,000 at weight 0.1 used to contribute 128,000; it now contributes about 1.4. **Every
    configured weight is effectively a no-op until re-tuned.** The damping is what stops any number
    of links making an item unforgettable and defeating the capacity bound — see
    [Links](docs/consolidation.md#links). `ExplainConsolidation` reports each memory's link
    significance beside its damped contribution, which is the quickest way to pick a new value;
    the config wizard's decay preview mirrors the new maths.
  - **Stored schema.** Event relationships moved from the `events.relationships` JSON column to an
    `event_links` table, alongside the new `memory_links`. **No data is migrated**: the legacy
    columns are dropped on first startup and the graph starts empty. This is deliberate — a
    half-migration that silently reinterpreted significances under the new damped maths would be
    worse than starting clean. Re-create any event relationships you rely on via `LinkEvents`.
  - Also of note: an event's links no longer round-trip through `StoreEvent`/`UpdateEvent`. They
    are rows in their own table, edited through the link RPCs, so a create or partial update
    carrying links leaves the existing graph alone rather than replacing it. The `hippo` CLI's
    `--relationship` flag is renamed `--link` to match.

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

- **The ingestor and the four broker bridges are instrumented.** Both were long-running daemons with
  no metrics and nothing to probe, which made their worst failure mode — stalling silently — look
  exactly like being idle. Each now serves `/healthz` and `/readyz` and exports OTEL metrics over
  OTLP/gRPC, sharing one implementation with the service via a new root-module `observability`
  package (`cmd/hippocampus/observability.go` promoted, not copied).
  - **Health surfaces.** `--health-port` (**8090 by default**, 0 disables) serves `/healthz`
    (liveness, never fails while the process runs) and `/readyz` (whether the Hippocampus instance
    can actually serve, checked via the token-free gRPC health service and named per dependency, so
    a failing probe says *which* end is down).
    **This is a change of network surface**: these binaries previously listened on nothing at all.
    Set `--health-port 0` to keep that, and give each process its own port when several run on one
    host.
  - **Metrics.** `hippocampus.ingestor.*` (events by outcome and rule, memories, orphans, rule
    errors, pass duration, and `seconds_since_last_pass`), `hippocampus.bridge.*` (messages and
    memories by broker and outcome, message duration, body bytes), and `hippocampus.client.rpc.*`
    (client-side RED metrics emitted by everything that dials the service, tagged by endpoint).
    `outcome` is multi-valued rather than a success bool throughout, so an event a rule deliberately
    dropped, or a memory the service declined for insignificance, never shares a series with a
    failure.
  - **Tenancy is `--metrics-group`**, a per-process label stamped on both the OTEL resource and each
    metric — the duplication avoids a `target_info` join in every query, and costs nothing because
    the value is fixed for the process's lifetime. It is deliberately **not** read from each record:
    a bridge derives a memory's group from the message subject by default, so a per-record label
    would be unbounded. Note that no service metric carries a group; this is the client side only.
- **An ingestor: stage data at the edge, promote only what earns it.** A new
  `integrations/ingestor` module and `hippocampus-ingestor` binary that watches an edge instance for
  **completed** events, judges each against a [CEL](https://cel.dev) rules file, and either promotes
  it to a central instance, promotes it after reducing it, or drops it — draining the edge either
  way. It is a client of two instances, not a service feature: the edge is a stock, unmodified
  `hippocampus`. Documented in [Ingestor](docs/ingestor.md).
  - **It holds no state.** `ImportBatch` is a full-state upsert by id, so promote-then-drain is
    at-least-once against an idempotent receiver and a crash between the two re-promotes identical
    rows. There is no cursor and no bookmark: the edge store *is* the queue, and what it holds is
    exactly what has not been judged yet.
  - **Judgement happens at event completion, not at ingest**, which is what makes a rule change
    reach in-flight data: an event still open when the rules reload is judged by the new ones. The
    file is re-read on an mtime change, a bad initial load fails startup, and a bad reload keeps the
    last good ruleset — the same contract as `auth.revocationFile`.
  - **A `promote` rule may reduce first**: `keepTopN`/`minSignificance` choose what crosses (the
    rest are still drained), or `summarise` calls `SummariseMemories` on the edge, which needs
    `ollama.enabled` there and fails the event loudly if it is absent.
  - **An edge must be configured not to forget.** Default consolidation settings will evict the
    memories of an event before the rules ever see it; set `consolidation.minimumRetentionInDays`
    above the longest an event stays open. See the docs — this is the one thing to get right.
- **`GetMemories` can filter by event.** New `event_id` and `has_event` fields on
  `GetMemoriesRequest`, exposed as `hippo memory list --event/--has-event` and on the MCP bridge's
  `list_memories`. Previously the only way to read an event's memories was `GetEventById` with
  `memories: true`, which returns every one of them in a single message and overruns the receive
  frame on a large event; this composes with `limit`/`offset` and every other filter. `has_event`
  exists alongside it because an event-less memory stores an empty `event_id`, which is also that
  filter's "no bound" value — the same reason `recalled` exists alongside `recall_count_max`.
- **Group scoping — a token can be restricted to particular `group` labels.** Authorisation tiers say
  what a caller may do; nothing said which records they may do it to, so any writer in a shared store
  could read and delete every other team's memories. A token may now carry a `groups` claim naming
  the group labels whose records it can reach. New keys `auth.groupsClaim` (default `groups`, for an
  identity provider that names the claim differently) and `auth.requireGroupScope`; `--mint-token`
  gains `--group`; `WhoAmI` gains `groups` and `group_scoped`, which the console, the `hippo` CLI and
  the config wizard all surface. Documented under
  [Group scoping](docs/configuration.md#group-scoping).
  - **Inert until tokens carry the claim, so every existing deployment is unchanged.** There is no
    schema change, no migration, no index rebuild and no OpenSearch reindex: the scope is the
    existing `group_name` column, which is already in the covering index, both search backends and
    the archive. An unscoped token behaves exactly as every token did before.
  - **An empty scope means the whole store**, so an unscoped token is the *most* privileged shape a
    token has, not the least. `auth.requireGroupScope` turns its absence into a rejection — worth
    setting once a store is partitioned, since a provider that stops emitting the claim would
    otherwise hand every caller everything while every request succeeded.
  - For a scoped caller: listings, searches and their `total_count` are narrowed; records outside the
    scope report `NotFound` rather than `PermissionDenied` (which would confirm they exist); writes
    are stamped with the caller's group, and cannot be moved out of it; both ends of a link must be
    in scope, with edges reaching outside dropped from responses; `Export`/`Transfer`/`Clear` walk
    only that partition; and `Purge`, `Sleep` and `PreviewConsolidation` are refused, all three
    acting on the whole store.
  - **Pre-existing records carry no group and are invisible to a scoped token.** Stamp them with an
    `UPDATE` before issuing scoped tokens against an existing store — see the documentation.
  - **It is a soft partition, not isolation**, and the difference is documented at
    [the trust boundary](docs/operations.md#group-scoping-and-the-trust-boundary). The decay dynamics
    stay store-global, so a busy group still affects what another forgets and there is no per-group
    capacity target; `link_significance` is a scope-blind aggregate, so a cross-group link raises an
    item's significance regardless of who can read the other end. Hard isolation remains one instance
    per tenant. The reasoning, and what a full tenant model would have cost, is recorded as TODO 60.1.

- **Event-source bridges: `--subject-metadata-key`.** Records the broker subject/topic as a metadata
  label as well as (or instead of) using it as the group. Empty by default, so an existing bridge is
  unchanged. It exists because `--group-from-subject` is on by default and a group-scoped token may
  only write the groups it holds — the two are mutually exclusive, and without this the subject had
  nowhere else to go. Pair `--group <token's label> --group-from-subject=false
--subject-metadata-key subject` to keep the routing information as a filterable label. See
  [Event sourcing](docs/eventsource.md).

- **Memory and event metadata — tags and key-value attributes.** `group` was the only classification
  on a memory, one freeform 128-character string doing the work of several dimensions at once, so
  applications either packed a delimited string into it or buried the classification in the body.
  `Memory` and `Event` now carry a `map<string, string> metadata` alongside it, filterable on
  `GetMemories`, `GetEvents`, and `SearchMemories`. Documented under
  [Metadata](docs/configuration.md#metadata).
  - **Bounded, and the bounds are constants rather than configuration**: 32 keys, 64-byte keys
    matching `[A-Za-z0-9][A-Za-z0-9._:/-]{0,63}`, 512-byte values, 4096 bytes serialised in total.
    Unbounded metadata is a body by another name, so the serialised size **counts toward
    `memory.limit.sizeBytes`** and toward the store's byte accounting — it contributes to capacity
    pressure and to what eviction reclaims, rather than escaping both.
  - **Filters are repeated `key=value` strings, not a map**, because `GetMemories`/`GetEvents` are
    HTTP `GET` routes and a map cannot be bound from a query string. Every pair must match. On
    `SearchMemories` the filter is applied inside the index, like `group`, so it narrows the
    candidates ranking sees rather than trimming an already-truncated page. The predicates are
    **unindexed**, exactly as the `group` filter is.
  - **Stored as a nullable JSON column** on all three drivers, added in place on first startup.
    Nullable rather than defaulting to `''` deliberately: SQLite's `json_extract` raises "malformed
    JSON" on an empty string, so an `''`-defaulted column would make the first metadata-filtered
    query fail against every row written before the upgrade. Round-trips through
    `Export`/`Import`/`Transfer`.
  - Exposed on the `hippo` CLI (`--metadata k=v`, repeatable), the MCP bridge (`store_memory`,
    `update_memory`, `create_event`, and the three list/search tools), the web console, the
    event-source bridges (an explicit header allowlist or prefix, never copy-all), and the Obsidian
    plugin (named frontmatter keys plus fixed labels).
  - Metadata is **never** emitted as a metric, span, or log attribute — it is client-supplied and
    unbounded in cardinality.

- **`clear_metadata` and `clear_group` on `UpdateMemory`/`UpdateEvent`.** Every updatable field
  reads its zero value as "leave unchanged", and an absent map is indistinguishable from an empty
  one on the wire, so neither metadata nor a group label could be removed once set — only replaced.
  These two write-only flags close that gap. Metadata otherwise **replaces** the stored map
  wholesale on update; there is no per-key merge.

- **Recall-state filters on `GetMemories`** — `recalled`, `recall_count_min`/`_max`,
  `time_recalled_min`/`_max`, `is_summary`, and `is_binary`, over columns the store already
  maintained but never exposed. `recalled`, `is_summary` and `is_binary` are the tri-state `Bool`
  rather than plain booleans, since an unset proto3 `bool` and an explicit `false` are the same
  value on the wire. `recalled=FALSE` is what answers "what have I never recalled?" — the count
  range cannot, because `0` means "no bound" everywhere in this API. `time_recalled_min`/`_max` ask
  only about memories that have been recalled, so an upper bound does not sweep in every
  never-recalled memory (whose `time_recalled` is `0`).

- **Memory-to-memory links** — an associative graph over memories, the on-thesis capability the
  service had been missing. A link is directed, carries its own significance, and raises the
  effective significance of **both** ends with diminishing returns, so a well-connected memory
  decays more slowly. Three parts:
  - `LinkMemories` / `UnlinkMemories` / `GetMemoryLinks`, and the same three for events
    (`LinkEvents` / `UnlinkEvents` / `GetEventLinks`). Both ends must exist — links cannot dangle —
    and a link is removed automatically when either end is forgotten.
  - **Spreading activation**: `consolidation.linkRecallPropagation` (0–1, default 0 = off) advances
    a recalled memory's direct neighbours' decay clocks a fraction of the way to now, so recalling
    one thing keeps its associates alive. Their recall counts are never touched, so ranking and the
    recall term keep their existing meaning.
  - **Associative retrieval**: `include_linked` on `RecallMemories` and `SearchMemories`, and a
    `linked_to` filter on `GetMemories`, all one hop.

  Exposed on the `hippo` CLI (`memory link|unlink|links`, `event link|unlink|links`), the MCP bridge
  (`link_memories`, `unlink_memories`, `get_memory_links`), and the web console's memory table.
  Links round-trip through `Export`/`Import`/`Transfer`; the import applies them in a second pass
  once every row exists, so a link whose target appears later in the archive is not lost.

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
  same ten rules as Grafana-managed rules provisioned into the compose observability profile.
- Rate limiting (`rateLimit.*`, off by default): a hierarchy of token buckets — an instance-wide
  ceiling, one bucket per authorisation tier, one per caller — of which a request must pass every
  level that has a rate. Both transports share the buckets. The global ceiling is enforced ahead of
  token verification; the tier and client levels after it, since both need to know who is calling. A
  refused request gets `ResourceExhausted`/`429` with a retry-after, counted by
  `hippocampus.ratelimit.rejected` and classified as a client fault so the server-error alert does
  not fire on your own protection working.
- `docs/clients.md` — how to generate a client in a language other than Go, from either the proto or
  the OpenAPI document, with the API behaviour (quiet rejection, recall-is-a-write, int64-as-string)
  a generated stub does not convey. No packages are published for other languages yet.

- An inter-process storage lock on the `sqlite` driver: a `hippocampus.lock` file in
  `storage.directory`, held exclusively for the process lifetime. A second instance pointed at the
  same directory now refuses to start, naming the holder, instead of silently running a second
  forgetting schedule against one store. Read-only opens (`--backfill-search` against OpenSearch, an
  operator's `sqlite3` shell) take no lock and are unaffected.

### Changed

- First run after a clone no longer needs a config file: an absent `./config.json` starts the
  service on built-in defaults (a `--config_file` given explicitly must still exist).
- The web console presents a **sign-in card in place of the console** when authentication is on and
  no session has resolved — on first load, after **Sign out**, and when a token is refused. The
  header's always-present bearer-token box is gone; the token is entered on the card instead. Purely
  a console change: the server enforced (and still enforces) authorisation on every RPC regardless of
  what the page shows.

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
