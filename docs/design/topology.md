# The deployment topology view

This is the design record moved out of CLAUDE.md verbatim (TODO-3 item 176): why the code is the way it
is, the alternatives that were rejected, and the incidents behind the guards. CLAUDE.md keeps the short
version and links here.

## `GetTopology`

- **The deployment topology view** (`hippocampus/topology.go` + `topology_probe.go`, `topology.*`,
  on by default) is `GetTopology`: what this instance is attached to, how the pieces relate, and
  the last known health of each, backing the console's Deployment tab and `hippo topology`. Six
  things carry it. (1) **It is bounded by what an instance can honestly know** - itself and
  whatever it dials - because everything else in a deployment (replicas on the shared store, the
  bridges, the ingestor, MCP servers, the CLI) dials IN and the service holds no address for any of
  them. Rather than imply otherwise, every node carries a `source` (SELF/CONFIGURED/DISCOVERED/
  DECLARED/OBSERVED) and the console renders it, so a sparse diagram reads as "nothing declared"
  rather than "nothing running". A registry would make a memory store a control plane; the response
  is read-only and there is no counterpart that acts on a component. (2) **Nothing secret is built
  into a node**: `redactEndpoint` strips credentials from all four address shapes (URL, libpq
  keyword/value, MySQL DSN, bare) at CONSTRUCTION, so the Server never holds a DSN password for
  this purpose, and the `self` node carries an explicit allow-list of settings rather than
  `viper.AllSettings()` - which would carry the signing secret, the DSN password, the OpenSearch
  password and the OAuth2 client secret. That redaction is what makes `topology.minimumTier`
  defaulting to **reader** defensible. (3) **The specs are plain structs converted to proto per
  call**, since a proto message is not safe to marshal concurrently - the same trap
  PreviewConsolidation documents. (4) **Statuses come from a background prober**, never from the
  RPC: probing N dependencies in the handler would open a connection to every dependency per page
  view, let one hung dependency hang the request, and multiply both by the number of viewers. The
  RPC serves a snapshot and reports `checked_at` plus `probe_interval_seconds` (which a client
  paces itself by, like `GetConsolidationStatus.snapshot_ttl_seconds`). (5) **Probes are optional
  interfaces on the concrete types** (`search.OpenSearch`, `summarise.Ollama`, `embed.Ollama`,
  `archive.S3Store` all gained a `Ping`), not new interface methods - the precedent `RecreateIndex`
  and `IndexMemorySync` already set - and three of them carry an `ErrDegraded` sentinel so
  "answered but unhappy" (a red index, a missing model) is a different status from "unreachable".
  Two nodes are never probed and say so: the OTLP collector (export is fire-and-forget) and the
  IdP (a console poll must not become load on someone's identity provider); the transfer target is
  opt-in because its probe dials a remote instance on a timer. (6) **`GetTopology` is the one RPC
  whose tier is configurable**, via a narrow `configurableTiers` allow-list in `auth/authz.go`
  (a general override would let a config file lower `Purge` to reader) - but its `scopeUnbound`
  refusal is NOT configurable, because that is not a sensitivity judgement: there is no per-group
  topology, so a scoped caller could only be shown infrastructure that is not theirs. (7) The
  inbound half is **declared, not discovered** (`topology.components`, phase 2): a name, a kind
  and a `healthUrl`, probed over the shared `/readyz` that `observability/health.go` already
  serves - so a bridge or the ingestor becomes a first-class node with its own per-dependency
  breakdown and **no change to those binaries at all**, and its edge points INWARD, the only edges
  in the graph that do. A 503 is degraded (it answered and named the reason), a refused connection
  unreachable, a 404 a wrong URL - three states an operator acts on differently. The list is
  capped (`MaxTopologyComponents`) because each entry is an outbound request per round and its
  name is a metric attribute, and that cap is also why the round went from sequential to
  `topologyProbeConcurrency`-at-a-time: with the count operator-controlled, a sequential round no
  longer fits inside its own interval. (8) **Peers on a shared store are DISCOVERED**
  (`hippocampus/peers.go` + `db/instances.go`, `topology.heartbeatSeconds`, phase 3): each instance
  upserts one row into an `instances` table on a timer and reads the rest, so a horizontally-scaled
  deployment can finally name its own peers - the advisory lock proves only that SOMEBODY holds it.
  Six things carry it. The id is **`hostname:port`, deterministic**, so a restart upserts its own
  row rather than leaving a ghost visible for four intervals, which in a rolling deployment is most
  of them. **Server drivers only**: SQLite is single-instance by construction, and its page-based
  `UsedBytes` would let the record of the deployment raise capacity pressure and evict live
  memories - the tombstone lesson. **A row prunes itself against its OWN interval**
  (`heartbeat_seconds`, deliberately BIGINT: Postgres types the whole pruning expression from that
  column, and as INTEGER both the nanosecond product and the bound overflow at runtime), so a slow
  peer is not deleted and resurrected by a faster one. **Counting is over FRESH rows only** - a
  dead consolidator's row outlives its process on purpose, and counting it would report the
  successful takeover as a duplicate. The payoff is `GetTopologyResponse.warnings`: **zero
  consolidators** (nothing is forgetting, every instance reports itself healthy, and the fault IS
  the absent node - so there is nothing to colour red) and **two or more**, logged on change rather
  than every round. And the write and the read **share one goroutine and one cadence**, because an
  instance that appeared in others' views while showing none of its own would be an asymmetry with
  no explanation on either side. (9) **Callers are OBSERVED, and that source is deliberately the
  weakest** (`hippocampus/observed.go`, phase 4): a client presenting a verified token is drawn
  from its `client_id`, with its roles, its scope (never its GROUPS - the view is reader-visible by
  default and a group name is frequently a customer's), its call count and its last call. Five
  things carry it. It carries **no health** and says `not checked` forever, because a call proves
  the client was alive at that instant, which is neither "healthy" (a client polling while its real
  work is broken) nor "unhealthy" (an idle one) - the collector/IdP reasoning again. It reports
  **nothing without auth**, since a caller is identified by its token and never by a source address
  (a proxy, or a replaced pod) or a user agent; the `self` node says so, so an empty inbound column
  is explained rather than read as "nothing is calling". It is **bounded at 32 with LRU eviction**,
  and that cap is a security property rather than tidiness - the map is keyed on a value that
  arrives in a token, so an unbounded one would be memory a caller controls; the same fact is why a
  `client_id` is never a metric attribute. Entries are **never expired on a timer**, only evicted:
  a bridge whose last call was six hours ago is worth more on the diagram than an absence, which is
  indistinguishable from a component nobody configured. And it **merges with the declared half** -
  where a declared component's name matches an observed `client_id` there is ONE node carrying both
  health and last call, which is the only thing in the view that separates a bridge that is up and
  writing from one that is up and silent. Recording sits **between authentication and
  authorisation** on both transports (hence roles rather than the resolved tier): a client whose
  token is valid and whose role is refused every call is exactly who an operator is looking for
  here, and behind the authoriser it would never appear. It is the one part of the view written on
  the request path, so the registry is an RWMutex taken only to insert plus atomics per entry,
  rather than a snapshot pointer like the prober's and the heartbeat's. (10) **Every node that can
  report a version does** (TODO-2 item 134): a probe is `func(ctx, wantVersion bool) (string,
  error)`, and a version that costs its own request (store engine via `dialect.versionQuery`,
  OpenSearch `GET /`, Ollama `/api/version`, the transfer target's `WhoAmI`) is asked for only on the
  first round, after a non-OK round, or every `topologyVersionRefresh`, cached in the previous
  published round. An unreadable version is never a failure, and a failed probe keeps the last one.
  Declared components report theirs on `/readyz` for free. Callers send
  `contract.ClientVersionHeader`, which lives in `contract` so the CLI and MCP bridge stay off the
  OTEL tree, deliberately is not the user agent, is sanitised by `sanitiseReportedVersion`, and is
  display-only, never identity. TODO 75, phases 1-4.
