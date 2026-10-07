# The integrations

This is the design record moved out of CLAUDE.md verbatim (TODO-3 item 176): why the code is the way it
is, the alternatives that were rejected, and the incidents behind the guards. CLAUDE.md keeps the short
version and links here.

## `integrations/mcp/`

`integrations/mcp/` — a standalone Model Context Protocol server (its own Go module, module path
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
`list_memories`, `link_memories`, `unlink_memories`, `get_memory_links`, `create_event`,
`update_event`, `end_event`, `get_event`, `list_events`, `link_events`, `unlink_events`,
`get_event_links`, `get_summarisation_candidates` — plus four **introspection** tools
(`significance_levels`, `explain_consolidation`, `consolidation_status`, `whoami`), deliberately
excluding the admin/destructive and bulk data-movement RPCs (Purge, Sleep,
Export/Import/Transfer/Clear, event delete/merge) so a model can't wipe or exfiltrate a store.
The four are reader-tier and none of them **enumerates**, which is the line that keeps
`PreviewConsolidation` off the surface while `ExplainConsolidation` is on it: the preview lists
ids from across the store, whereas explain answers only about ids the caller supplied and could
already read in full through `list_memories`. They exist because without them a model using this
as its memory could not ask the one question the store exists to answer — is this memory about to
go, and how long has it got — and because `whoami` is the FEATURE-DETECTION RPC whose absence was
already costing the bridge: `search_memories` offers semantic and hybrid and could only discover
that a deployment serves neither by having a search rejected (TODO-2 item 100.3).
That set is now held exact in both directions by `TestServer_EndToEnd`, and to the
table in `docs/mcp.md` by `TestEveryToolIsDocumented` — the registered set is a security
statement, so a tool arriving unremarked is what wants noticing. The listing tools take RFC3339
time bounds while the views return UnixNano, deliberately: a model can write a date but not a
nanosecond epoch, and a bound wrong by three orders of magnitude returns a plausible empty page
rather than an error. The
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
(like `cli`), since the root module's `go build/test ./...` no longer descends into it.
Ships as its own image (`Dockerfile` `target: mcp`, built from within the module directory),
reachable over HTTP via the opt-in `mcp` compose profile; the release workflow cross-compiles the
binary for every OS/arch onto the GitHub release and publishes the image to
`ghcr.io/fastbean-au/hippocampus-mcp`. See `docs/mcp.md`.

## `integrations/`

`integrations/` — self-contained client/edge subprojects, each a thin bridge rather than part of
the core service. Each Go integration is a separate module whose dependency tree stays out of the
root build (`mcp`, `cli`, `eventsource`, `ingestor`, `objectstore`), and one is a Python project
(`python`).

**Three subprojects now live in their own repositories** (TODO-2 item 113), because each tracked a
release train that is not this one's: the Obsidian plugin
(`fastbean-au/hippocampus-obsidian` — Obsidian's community registry lists a repository whose
**root** holds `manifest.json`, which a monorepo cannot offer), the LlamaIndex adapter
(`fastbean-au/hippocampus-llamaindex` — it tracks `llama-index-core`, and depends on the
**published** client rather than on the contract) and the OpenTelemetry collector
(`fastbean-au/hippocampus-otel-collector` — twelve collector modules on a dual-series fortnightly
cadence). Each takes a **versioned** dependency on this module rather than a `replace`, and the
release workflow's `notify-satellites` job fires a `repository_dispatch` at each so it re-pins and
reports whether it still builds. The rule is no longer "one release train, plus Obsidian": it is
that a subproject stays here if it tracks **this** contract, and leaves if it tracks somebody
else's train.
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
- `integrations/python/` — the Python client, `hippocampus-client`, released for PyPI but not yet
  uploaded there (its own
  project, not a Go module and not imported by anything here). A thin wrapper over generated gRPC
  stubs covering the **full** RPC surface, unlike the MCP bridge's curated one - it is a client
  library, and what a token may actually do is the service's tiers to enforce;
  `tests/test_contract_coverage.py` holds the two in both directions, so a new RPC is a gap rather
  than a decision. Five things carry it. (1) **The stubs are generated at build time and are not
  committed**, which is item 64's own rule made mechanical: a committed copy is the fifth copy of
  the contract that drifts, and the release workflow stamps the tag into `_version.py` and
  publishes from the same tag, so `hippocampus-client==X.Y.Z` is by construction the client of
  `vX.Y.Z`'s contract. (2) **The contract is staged under `hippocampus/_proto/` before generation**
  rather than at the include root, because protoc derives a generated module's import path from
  the proto's PATH - staged bare it emits `import hippocampus_pb2`, a top-level import that
  resolves only outside a package; the usual alternative is rewriting the import in generated code
  afterwards. (3) **The openapiv2 option is stripped on the way past**, and this is the one thing
  `docs/clients.md` had wrong: protoc writes `from protoc_gen_openapiv2.options import
annotations_pb2` into the generated module and no index publishes that package, so a client
  generated by following that page installed cleanly and raised on first import. Both removals are
  ASSERTED, since a silent no-op ships a wheel nobody can import. (4) **`force-include` differs per
  target** - the stubs are gitignored and hatchling honours .gitignore, so without it the sdist
  ships without them and the wheel built FROM that sdist (the `pip install` path) has no contract
  to regenerate from; the sdist keeps the `src/` prefix so its own force-include still resolves,
  the wheel drops it. (5) **The wrapper's value is the four encodings**: UnixNano int64 timestamps
  become aware datetimes, a zero timestamp becomes None (never-recalled and not-yet-ended are
  absences, not 1970), tri-state `Bool` filters become `Optional[bool]`, and `"key=value"` metadata
  filters become a dict - plus pushing the three product behaviours into the types, so a
  rejected-for-insignificance write returns a falsey `Stored` rather than raising. The memory and
  event surface returns dataclasses; the operator surface returns its proto message unchanged, the
  same line the MCP bridge draws. Built/tested by the `python-client` CI job on the oldest and
  newest supported interpreters (a floor declared and not tested is a floor that is wrong), which
  also regenerates from the contract, builds both distributions and imports the wheel out of a
  clean environment - the one failure this package can ship is a wheel with no stubs in it, which
  every test passes against because they run on the source tree. See `docs/python.md`.
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
  gRPC `Dial` (now aliases over the root `dial` package - see below; auth is either a static
  `--token` or the OIDC **client-credentials** grant in `dial/oidc.go`, selected by a set
  `--oidc-client-id`, which mints and refreshes its own access tokens — a static token expires and
  then fails every write _silently_ for as long as the daemon runs, which is why anything against
  an IdP-backed service wants the grant; config is validated eagerly but discovery is LAZY so an
  IdP blip does not stop a supervised bridge starting, and it deliberately matches the generators'
  implementation in the `hippocampus-gen` repo, down to the Auth0 audience quirk, so one Keycloak
  realm configures both), and `RegisterCommonFlags`
  (pflag only — each `cmd/*` main owns its viper reads, per the convention). The client seam
  (`hippocampusClient`, `bridge/store.go`) names **seven** RPCs and no more — `StoreMemory`,
  `StoreMemories`, `StoreEvent`, `RecallMemories`, `DeleteMemories`, `ImportBatch`, `LinkMemories`
  — and that unexported interface IS the module's
  statement of what a bridge may do to a store: `Dial` hands back the whole generated client, so
  this declaration is the only thing standing between an adapter and `Purge`. `StoreMemories` is
  what the polled paths (`Store.StoreMemories`, and `HandleEvent`'s fallback) write through since
  item 106.3: one call per page in chunks of 100, reading each memory's own result exactly as
  `storeEach` read each call's — already held is a success, a memory naming an event the store does
  not hold is skipped, anything else fails the page — so the page-at-a-time write keeps the
  per-memory tolerance that stopped a polled source stalling on its own first orphan.
  `storeEach` remains as the latching fallback for a service answering `Unimplemented`, and one
  thing does change with the batch: `--call-timeout` bounds a CALL, so a chunk of a hundred shares
  the budget one memory used to have. Beyond `Handle`,
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
  one genuinely STATEFUL thing here (bounded; unlike the roots cache it is not just an optimisation, so
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
  deletion is about consent. **`--capture-replies`/`--feed-authors`** widen feed mode, where a
  firehose post is otherwise reinforcement alone: the first stores a post replying to a thread the
  bridge holds (matched on ROOT first, so it matches however deep the reply sits — this is what
  makes `--events thread` hold a conversation rather than one memory), the second stores a post by
  any DID the feed has surfaced (derived from `post.author.did` per read, so no account list is
  maintained). **`--capture-significance`** ranks a capture below the feed's own posts, delivered
  through the transformer's per-message override (`CaptureSignificanceHeader`) because the
  Transformer belongs to the Store — so it cannot be combined with `--significance-header`, which
  the command refuses; and a capture is deliberately NOT topic-linked, since terms fall back to the
  body and a reply's body is conversation rather than an editorial slug. Both indexes are
  `idCache`s, bounded and best-effort like the term index; the
  capture index holds only what the FEED produced, never the replies captured through it, or one
  busy thread would evict the posts every other thread is matched on. Neither works under `--dids`,
  because `wantedDids` selects on the repo a record was written IN and a reply lives in the
  replier's — the same reason `--dids` beside `--feed` receives NO engagement at all. All three
  combinations are warned about in `cmd/bluesky/main.go`, since each presents as a feed nobody is
  interacting with rather than as a failure. Tests are network-free by default (NATS uses an embedded in-process
  server; bluesky's `consume`/`serve` run off a canned frame slice and its real dial is covered by a
  local `httptest` websocket server; MQTT/RabbitMQ/Jetstream real-connect paths are env-gated
  integration tests — `HIPPOCAMPUS_TEST_MQTT_BROKER`/`HIPPOCAMPUS_TEST_RABBITMQ_URL`/
  `HIPPOCAMPUS_TEST_JETSTREAM`, the last needing no container since Jetstream is public, and set in
  CI only on pushes to the default branch so a fork's PR never reaches Bluesky), every package
  ≥95% covered (`bridge` itself sits at ~91%, held down by `StartRuntime`). Built/vetted/tested by the `eventsource-bridges`
  CI job (like `ingestor`; the `docker` CI job also smoke-builds the five images). The release
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
  deletes only what it judged**: the judged memories by id, then the event with
  `DeleteEvent.if_empty`, which the edge refuses while a late memory holds it - so a memory landing
  against an already-ended event is never deleted unjudged, and is judged on the next pass
  (TODO-3 item 165; a count re-check followed by `memories: true` left the window between the two
  calls open). The two reduction kinds disagree about what the source holds afterwards
  (`keepTopN`/`minSignificance` choose what _crosses_ and leave the source untouched, `summarise`
  replaces memories on the source), which is why `reduce` returns both lists. (5) **An event over
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
  unlike a _match_ expression erroring, which merely does not match, there is no safe fallback for
  a rank the operator asked for. `metadata` merges rather than replaces (CEL has no map union).
  `promoter/` is driven in tests against two in-memory fake instances, so no service is needed.
  Built/vetted/tested by the `ingestor` CI job; the release cross-compiles `hippocampus-ingestor`
  and publishes `ghcr.io/fastbean-au/hippocampus-ingestor`. See `docs/ingestor.md` — in particular
  that **an edge must set `consolidation.minimumRetentionInDays` above the longest an event stays
  open**, or its own decay will forget in-flight events before the rules ever see them.
  Instrumented like the bridges (see `observability/` below): `hippocampus.ingestor.*` plus the
  shared client RED metrics, and `/healthz`+`/readyz` naming which of its two ends is unreachable.
- `integrations/objectstore/` — the **object-storage agents** (TODO-2 item 107.3): the working half
  of the retention-controller mode, where the payload stays in a bucket and this store decides what
  survives. Its own Go module (module path
  `github.com/fastbean-au/hippocampus/integrations/objectstore`; `replace
github.com/fastbean-au/hippocampus => ../..`), which is what keeps the AWS SDK out of the root
  build — **the root module does not import it**. Two commands: `object-gateway` is the **tap**
  (fronts the bucket, and every object it serves reinforces the pointer-memory behind it) and
  `object-reaper` is the **actuator** (deletes the object behind a memory that has been forgotten).
  Seven things carry the design. (1) **The id IS the contract and it is reversible**:
  `keymap.MemoryId` is `<bucket>/<key>`, not a hash, and the reason is the catch-up path rather
  than anything about the tap — a `ForgottenMemory` carries an id and deliberately never a body, so
  an agent holding only a hash could read the log, learn that forty thousand memories went, and be
  unable to name one object. The cost is a length bound, and it must be the **service's** id
  limit (`types.MaxIdBytes`, 128 bytes) rather than MySQL's 255-character column: an id the keymap
  maps but the service refuses can never be registered, reads as absent, and is deleted by an armed
  sweep (TODO-3 item 142). A key over it is **unmappable**: skipped by the tap and never deleted by
  the sweep, which cannot tell an object it failed to map from one nobody asked it to manage. (2) **Reinforcement is stateless and
  its failure is silent**: recall is an `UPDATE ... WHERE id IN (...)` that matches nothing on a
  miss, which is what lets the tap hold no lookup table — and means a producer using any other id
  scheme produces no error and no reinforcement, forever. So the hit rate is a metric, a sustained
  zero is a Warn line naming that cause, and `HippocampusObjectTapNotReinforcing` alerts on it;
  that detector is the whole answer to the one failure this design can hide. (3) **Redirect, not
  proxy**: the gateway presigns and answers 302, so it is a chokepoint for the DECISION to read
  without being one for the bandwidth; `--mode proxy` streams instead and forwards `Range`, because
  answering a range request with a whole body is a wrong answer rather than a degraded one. A HEAD
  never reinforces. An inbound token is **required** unless `--allow-anonymous` — a presigned URL
  grants a read to whoever holds it, so an unauthenticated gateway is a public read endpoint for
  the whole bucket. (4) **Three deletion paths, because at-least-once against a process that can be
  down is still lossy**: the push path (the `memory_forgotten` callback), the pull path (the
  forgotten log read back over `--catch-up`, bounded by a WINDOW rather than a cursor so it stays
  stateless and idempotent), and the sweep (`ListObjectsV2`, then `ExplainConsolidation` as an
  existence oracle 200 ids at a time — it answers only about ids it is given and omits the ones it
  does not hold). The receiver's status codes are a contract with the drain worker: 5xx for
  anything it could not carry out (so the queue replays it), 2xx for anything it deliberately did
  not act on (or the queue replays that forever). (5) **Shadow mode is the default** — `--delete`
  arms it — and `outcome="shadow"` is a counted outcome rather than an absence, because comparing a
  shadow run against what the far end's flat expiry would have dropped is how this becomes
  authoritative over data the store cannot see. (6) **What it refuses to delete** is most of the
  safety: an id that is not an object reference, an id naming another bucket, an unmappable key, and
  any cause outside `--causes` (default `consolidation,eviction,expiry,cascade`) — `clear` is a MOVE,
  `purge` would empty the bucket on one administrative command, `summary_replace` is a judgement,
  and `client` only arrives when `callbacks.allDeletions` is set, which is a visibility key rather
  than consent. **Every path asks the store first**: the sweep of its own enumeration, and the push
  and catch-up paths through `Reaper.ReapForgotten`, because neither a delivery nor a log entry
  proves a memory is gone now - one can be forged, and either can be stale once an object is
  re-uploaded and re-registered under the same key (item 144; a held id counts as
  `outcome="held"`). An armed reaper also refuses to start with an unauthenticated listener unless
  `--allow-unauthenticated-callbacks`, mirroring the gateway's `--allow-anonymous`. The sweep
  additionally never judges an object younger than `--sweep-min-age` (24h, **not** disableable —
  without it the sweep races every producer write), and every path **stops** if the store cannot be
  asked what it holds, since reading "cannot ask" as "not held" empties the bucket on the first
  outage. (7) **Readiness is deliberately asymmetric**: the
  gateway's `/readyz` covers the bucket ONLY (its job is serving objects; pulling it from a load
  balancer because a memory store is down turns a lost decay-clock update into an outage for every
  reader), the reaper's covers both ends. Tests need no bucket and no service — the S3 driver runs
  against an `httptest` server speaking enough of the wire protocol to exercise the paginator and
  the presigner. Built by its own `objectstore-agents` CI job; the release cross-compiles both
  binaries and publishes `ghcr.io/fastbean-au/hippocampus-object-{gateway,reaper}` from one
  `COMMAND`-parameterised Dockerfile. Registration is **not** here: writing the pointer-memories is
  the producer's, being the only party that knows an object's significance and group. See
  `docs/objectstore.md`.

## `dial/`

`dial/` — the one connection to the service, used by every process that dials it: the broker
bridges, the ingestor, the object-storage agents, the MCP bridge and the `hippo` CLI (TODO-3 item
172; each had its own copy, and only one had learned the OIDC grant). `dial.Config` carries the
address, a static token or the OIDC client-credentials grant, the TLS trust block, the version
header, and caller-supplied `Interceptors`; `TLSClientConfig` serves the CLI's HTTPS transport the
same TLS. It deliberately does **not** import `observability`: the CLI and the MCP bridge are kept
off the OpenTelemetry dependency tree, so the processes that do export metrics pass
`observability.ClientMetrics(endpoint)` in. `examples/` holds a Go quickstart over it, an asyncio
agent loop over the Python client, and a curl walk-through of the gateway - all three executed in CI
as contract smoke tests (the docker job runs the Go and curl ones against the compose stack, the
python-client job runs the agent loop against its fake). The Python client has an
`AsyncHippocampus` beside `Hippocampus`, both built from one `_Surface` of method definitions that
differ only in `_invoke`, so the two cannot drift.
