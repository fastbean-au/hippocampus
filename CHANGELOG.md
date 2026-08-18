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

### Added

- **A deployment topology view: `GetTopology`, the console's Deployment tab, and `hippo topology`.**
  An instance now reports what it is attached to — itself, its store, the search index, the
  summariser and embedder, the object store, the transfer target, the identity provider and the OTLP
  endpoint — with the last known health of each and the configuration behind it. A dependency that
  is quietly broken (an OpenSearch cluster refusing writes, an Ollama server that never pulled its
  model, an S3 bucket the credentials cannot see) previously surfaced only when the operation that
  needed it failed; all three now show as a status with the reason beside it.

  What it reports is deliberately bounded by what an instance can honestly know: itself, and
  whatever it dials. Everything else in a deployment connects _to_ it and it holds no address for
  any of them, so every component carries a **source** and a sparse view reads as "nothing has been
  declared" rather than "nothing is running". It is read-only, and there is no counterpart that acts
  on another component — a registry would make a memory store into a control plane.

  Health comes from a background prober on `topology.probeIntervalSeconds` rather than from the
  request, and each component reports when it was last checked, so one console page never opens a
  connection to every dependency at once and a hung dependency cannot hang the RPC. Two components
  are never probed and say so: the OTLP collector (export is fire-and-forget) and the identity
  provider (a console poll must not become load on somebody's IdP). The transfer target is opt-in.

  Every address is redacted server-side — DSN credentials in all three forms, cluster passwords,
  signing material — which is what makes the default `reader` tier defensible.
  `topology.minimumTier`
  is the one per-RPC tier a deployment may configure; a group-scoped caller is refused regardless,
  since there is no per-group topology to answer with. New `topology.*` config block, all of it
  defaulted. See [Deployment topology](docs/configuration.md#deployment-topology).

- **Declared components in the topology view (`topology.components`).** The half of a deployment an
  instance cannot discover — the broker bridges, the ingestor, MCP servers, any other client —
  can now be listed, and each is probed and drawn as part of the deployment, with its edge
  pointing **inward**, because that is the direction the connection is opened in.

  It needs no change to the components themselves: the bridges and the ingestor already serve
  `/readyz`, and that endpoint already reports a per-dependency breakdown, so a declared bridge that
  cannot reach its broker now says so where before it would have been an unexplained red box. A
  `503` is reported as degraded (it answered, and it named the reason), a refused connection as
  unreachable, and a `404` — a `healthUrl` that is simply wrong — with its status code, so all
  three are told apart. The list is capped at 32 and every entry is validated at startup; the
  probe round now runs a few probes at a time so a full list still completes inside one interval.

- **Peer discovery on a shared store, and the warnings it makes possible
  (`topology.heartbeatSeconds`).** A horizontally scaled deployment — one consolidator plus N
  replicas over a shared `postgres`/`mysql` database — could not name its own peers: the
  single-consolidator advisory lock proves only that *somebody* holds it, not who, and nothing at
  all about how many replicas are attached. Each instance now registers one row in an `instances`
  table every `heartbeatSeconds` (default 30, `0` disables it), so every instance can report its
  peers with their version, their role, their capability flags (search index, summariser, embedder,
  gateway) and how long ago each last checked in. The id is `hostname:port` and therefore
  deterministic, so a restart replaces its own row rather than leaving a ghost to age out; a clean
  shutdown removes the row immediately, and an instance that stops without saying so is reported
  unreachable for a window before it disappears.

  That registry is what makes two deployment-wide faults visible, both reported as **warnings** on
  the response — shown above the diagram in the console, printed first by `hippo topology`, and
  logged at `WARN` when they appear and at `INFO` when they clear. **No instance is consolidating**:
  every one came up with `consolidation.enabled: false`, so nothing forgets, nothing evicts and the
  store simply grows, while each instance individually reports itself perfectly healthy — because it
  is. There was no component to show as red for that, since the fault *is* the absent one. And the
  reverse, **more than one instance consolidating**, which is what a circumvented lock or two tiers
  pointed at different databases looks like from inside. A stale row is never counted toward either,
  so a replacement correctly taking over is not reported as a duplicate.

  SQLite keeps no registry and creates no table: that store is single-instance by construction, and
  its page-based capacity accounting would let the record of the deployment raise capacity pressure
  and evict live memories to make room for itself. `GetTopologyResponse` gains a `warnings` field
  (additive).

- **Observed callers in the topology view.** Where authentication is on, a client that presents a
  verified token is now drawn from the `client_id` it authenticated with, reporting the roles that
  token carried, whether it is group-scoped, how many calls it has made and when it last made one.
  There is nothing to configure: it is on wherever the view and authentication both are. It closes
  the last gap in the inbound half of a deployment — until now a component that dialled in was
  invisible unless an operator remembered to declare it.

  Declaring and observing combine rather than compete. Where a declared component's `name` matches
  an observed `client_id`, the two are reported as **one** component carrying both its health and
  its last call — the pair that separates a bridge which is up and writing from a bridge which is up
  and has written nothing, which nothing in the view could distinguish before.

  Four limits are deliberate. An observed component carries **no health** and reports `not checked`
  permanently, because a call proves the client was alive at that instant and not that it is
  working. It reports **nothing when authentication is off**, since a caller is identified by its
  token and never by a source address or a user agent — and the `self` component says so, so the
  empty inbound half is explained rather than merely empty. The set is **bounded at 32**, least
  recently seen evicted first, because the key arrives in a token and an unbounded one would be
  memory a caller controls; for the same reason a `client_id` is never used as a metric attribute.
  And entries are **never expired on a timer** — a component whose last call was hours ago stays on
  the diagram saying so, which is the report of the fault, where one that had quietly vanished would
  be indistinguishable from one nobody ever ran. No contract change.

## [0.33.2] - 2026-08-16

### Changed

- **A much shorter README.** It had grown into a second copy of the documentation — every
  integration, every deployment target, and the security surface each explained in full — so the
  things a reader arrives for were buried. It is now a landing page: what the service is, the live
  demo, one quick start, a table of the components around the service, and the documentation index.
  Everything it used to explain in place is in `docs/`, and what was only ever in the README moved
  there rather than being dropped — the **Docker Compose stacks and the Kubernetes overlays** (a new
  _Containers and Kubernetes_ section in [Operations](docs/operations.md), which had no Kubernetes
  coverage at all), the **hosted demo instances** and what each shows
  ([Demonstrations](docs/demonstrations.md)), the **Homebrew formulae** for the two client binaries
  ([CLI](docs/cli.md), [MCP](docs/mcp.md)), and the note that storage errors are masked behind gRPC
  status codes ([Operations · Security](docs/operations.md#security)).
- **The hosted Bluesky demo is listed, and led with.** Neither the README nor
  [Demonstrations](docs/demonstrations.md) mentioned it, though it is the first console on the demo
  site and the only one running on live data nobody staged — headlines arriving from a curated news
  feed, all at one significance, with real engagement the sole differentiator between what survives
  and what decays.

- **Opening an event in the console is now a drill-down rather than a card stacked on top of the
  list.** An event and its memories used to appear above the events list, the filter and the create
  form — so the thing you had asked for arrived with a page of controls beneath it that no longer
  applied to what was on screen, and on a long list it was easy to miss that anything had opened at
  all. The Events tab now shows one thing at a time: the list, or one event. A **Back** button is the
  way out, and it names where it goes, because an event id is a link in four tables across three
  tabs and returning to the events list is wrong for the three-quarters of them that were not opened
  from it — following an event from a memory row lands you back on Memories, with the results you
  had. Clicking a tab in the nav leaves the drill-down as well, so arriving at Events by hand always
  finds the list. Each row also carries an explicit **Open ›** button beside its other actions: every
  other thing a row can do to an event was already a button, and the one that navigates was the one
  that did not look like a control.

### Fixed

- **The Bluesky bridge no longer asks the service to link a post to itself.** A feed generator hands
  back the same page on every poll, and the topic-linking path was reached again for every post it
  had already related — by which point that post was in the term index and so came back as its own
  strongest match. Each poll therefore produced one rejected `LinkMemories` call per already-seen
  post, filling the bridge's log with `InvalidArgument … link N links memory to itself` while the
  posts themselves stored fine. The lookup and the index insert are now one locked operation that
  reports nothing for a post already indexed, which closes the ordering race behind it as well: the
  poll loop and the firehose consumer both reach that code, and either could index a post between
  the other's lookup and insert. A post already related is simply not related again — nothing is
  lost, since a link's significance counts for both of its ends, so a related post arriving later
  builds the edge from its own end.

- **A results table's column headings now stay put while its rows scroll.** Listing a page of
  memories, or a wide set of search hits, scrolled the headings off the top after the first few
  rows, and from there a column of bare numbers said nothing about which column it was. Each table
  now scrolls within its own bounded height with its heading row pinned above it — including the
  pinned actions column's corner, which holds both edges at once — so the headings are in view for
  every row.

## [0.33.1] - 2026-08-16

### Fixed

- **Opening an event no longer throws you onto the Search tab.** The event drill-down — an event's
  details and every memory attached to it — was rendered on the Search tab, so clicking an event's
  name in the events list navigated away from the Events tab and showed those memories above a query
  form that had nothing to do with them. An event's memories are not a search result. The card now
  lives on the **Events** tab, at the top, and opening an event from anywhere (the events list, a
  memory's event id, a link, the candidates list) lands there. A search no longer closes an open
  event either: the two are on different tabs now, so doing so would reach across and discard
  something the user had opened.
- **A memory body no longer runs off the side of the card.** The free-text row spanned every column
  including the pinned actions column, and was as wide as the _table_ rather than as wide as the
  visible scroll area — so on any table wide enough to scroll (two monospace id columns and four
  buttons, which is most window sizes) the right-hand end of every body was outside the card, with
  the pinned buttons painted over what was left. Reading a memory meant scrolling a table sideways.
  The row now stops one column short, with the pinned column continuing beside it, and the text is
  pinned to the left of the scroll viewport and sized against it, so it stays whole and in place
  while the columns scroll past.

### Changed

- **The Events tab has List and Summarise modes.** Summarisation candidates were a second card
  stacked permanently below the events list, under a filter that did not apply to them, with a
  Refresh button in its header that overlapped the text explaining what the list was. They are now
  the second mode of the tab's control card: **List** is the filter you write, **Summarise** is the
  list the sleep cycle wrote for you, and each shows its own results card. Selecting Summarise does
  not load anything — the candidates are a snapshot the cycle refreshes, so loading them stays a
  deliberate act with its own button.

- **Reading the forgotten log is now `reader`-tier, not `admin`.** `GetForgottenMemories` was
  assigned `admin` on the grounds that it enumerates ids, groups and significances across the store
  — but `GetMemories` does that at `reader`, with the bodies as well, so enumeration was never what
  separated the two tiers. What separates `PreviewConsolidation`, which the log was placed beside,
  is that it is _unscoped_: it describes the whole store and is refused outright to a group-scoped
  caller. The log is not. A tombstone carries its memory's group, so both its RPCs are scope
  filtered, and a scoped caller reads exactly their own partition's losses — the same records they
  could have read in full, with bodies, while those memories were alive. It reports the `value` and
  `threshold` pair that `ExplainConsolidation` already serves at `reader`, and it carries no bodies.
  This puts all three forgetting-transparency reads (`ExplainConsolidation`,
  `GetConsolidationStatus`, `GetForgottenMemories`) at one tier.

  `DeleteForgottenMemories` is unchanged and stays `admin`: it is destructive, and destructive on an
  audit record rather than on data the caller put there.

  This widens access, so no client breaks — but it is security-relevant, and a token that is refused
  today will be allowed after upgrading. The case for keeping the read at `admin`, if it applies to
  your deployment: the log outlives what it describes, so a long
  `consolidation.tombstones.maxAgeInDays` leaves a trace of groups, sizes and timing that the live
  store would by then have discarded. Where `reader` tokens go to clients you would not trust with
  that history, keep the log short.

  In the console this moves the **Now** tab's forgetting feed and the **Decay** tab's log panel into
  view for every caller — a writer could not previously see that their own memories had been
  forgotten — while the panel's _Clear the log_ button remains administrator-only.

## [0.33.0] - 2026-08-16

_Covers every change since v0.23.0. The releases between it and v0.33.0
shipped without their entries being rolled out of `[Unreleased]`, so the changes below are
recorded together rather than attributed to the release each shipped in._

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

- **A "Now" tab: the console's landing view, and the store's premise made live.** The console opened
  on an empty Search tab and said nothing about forgetting, so a store quietly consolidating every
  two minutes looked exactly like a store doing nothing. Now leads with how many memories are held,
  what the last cycle forgot, a countdown to the next, a capacity meter showing whichever axis is
  actually binding, and a feed of what has just gone. It computes no decay maths of its own — every
  figure is served — and it degrades in place rather than vanishing: a replica explains that its
  store is consolidated elsewhere, a reader sees everything but the forgetting feed and the Sleep
  control, and a store not recording tombstones says which config key would start it.
- **`GetConsolidationStatus`** (`GET /v1/consolidation/status`, `hippo status`) — when the next
  consolidation cycle is due, whether one is running, and what the last one did (counts for each of
  the two decay paths, bytes reclaimed, duration, and what triggered it). It completes the
  forgetting-transparency set: the dry run says what would go, `ExplainConsolidation` where a memory
  stands, the forgotten log what went, and none of the three said **when**.
  - **Reader tier**, because it names no stored record and returns counts rather than ids — the same
    grounds on which `ExplainConsolidation` already serves a reader the store's used bytes and
    memory count.
  - **It does not refuse on a read/write replica**, unlike `Sleep`, `PreviewConsolidation` and
    `ExplainConsolidation`. Reporting `consolidation_enabled: false` _is_ the answer there; refusing
    would leave a client unable to tell a replica from a consolidator whose cycle had died, which is
    the distinction the RPC exists to make.
  - It reports `snapshot_ttl_seconds` so a polling client paces its `ExplainConsolidation` calls
    from the server's own cache rather than from a guess.
- **Event links are reachable from the console.** `LinkEvents`/`UnlinkEvents`/`GetEventLinks` had no
  UI at all while memories had a full links card, even though the two are one mechanism. One card
  now serves both.
- **Event metadata is settable, filterable and visible in the console** — a metadata field on the
  create form, a metadata filter on the listing, and the same key=value pills the memories table
  already rendered. `Event.metadata` and `GetEventsRequest.metadata` had existed server-side
  throughout.
- **The forgotten log pages**, by keyset (`after_seq`/`next_seq`, which the RPC already returned),
  so a log longer than one screen is readable rather than truncated at the limit.
- **`scripts/release.sh`** — cuts a release: runs the pre-flight, rolls `[Unreleased]` into a dated
  version section, rewrites both link references, commits and tags. It deliberately does not push,
  and it refuses to tag when the changelog is not in the state a release needs — including when its
  newest version heading is not the current tag, which is how seventeen releases came to ship with
  their entries still under `[Unreleased]`. See [RELEASE.md](RELEASE.md#cut-the-release).

- **`WhoAmI` reports `consolidation_enabled` and `tombstones_enabled`**, so a client can tell a
  consolidator from a replica and a recording store from a silent one without probing. The first is
  the deployment property behind a family of refusals — `Sleep`, `PreviewConsolidation` and
  `ExplainConsolidation` are all `FailedPrecondition` where `consolidation.enabled` is false, because
  a replica's store is consolidated by another instance under _that_ instance's configuration. The
  second exists for the opposite reason: `GetForgottenMemories` does not refuse when the log is off,
  it answers with an empty page, and "nothing was written down" is indistinguishable from "nothing
  has been forgotten" without being told which. Both are reported on the authenticated and the
  unauthenticated path, like `search_modes` and `summariser_enabled`; `hippo whoami` prints them.
- **The forgotten log: an optional record of what each cycle deleted, and why.** Off by default
  (`consolidation.tombstones.enabled`). When on, every memory the two decay paths delete leaves one
  record carrying its id, group, event, significance, stored size, the computed `value` and the
  `threshold` it was measured against, which rule took it, and when. `GetForgottenMemories`
  (`GET /v1/memories/forgotten`, `hippo forgotten list`) reads it; `DeleteForgottenMemories`
  (`POST /v1/memories/forgotten/delete`, `hippo forgotten clear`) empties it. Both are `admin`, and
  both honour group scoping as a predicate, so a scoped token reads and clears only its own
  partition's losses. This completes the forgetting-transparency trio: the dry run says what would
  go, `ExplainConsolidation` says where a memory stands, and this is the only one that can speak
  about a memory that is no longer there. See
  [the forgotten log](docs/operations.md#what-was-forgotten--the-forgotten-log).
  - **Bodies are never kept.** A record says a memory was forgotten, not what it said; this is
    deliberately not an undelete.
  - **It records forgetting, not deletion.** `Clear` (which deletes memories an `Export`/`Transfer`
    has already moved) and the client-initiated deletes write nothing — nothing was lost in those
    cases, and a log claiming otherwise would be worse than none.
  - **Turning it off deletes nothing.** Disabling stops the writing _and_ the automatic trimming, so
    what was recorded stays readable; emptying the log is always an explicit request. The threshold
    is recorded per record rather than inferred because it moves with capacity pressure — a value
    from last month means nothing measured against today's threshold.
  - **Bounded by default so the feature cannot eat the store it describes.**
    `consolidation.tombstones.maxRows` (100,000) and `maxAgeInDays` (30) are applied at the end of
    each cycle; either can be set to 0 to remove that bound, which is warned about at startup. The
    log is excluded from the store's measured size, so it never raises capacity pressure or
    triggers eviction. Two metrics come with it: `hippocampus.tombstones` and
    `hippocampus.tombstones.deleted`.
  - The console's **Decay** tab shows the log beside the dry run, for an administrator. It is
    deliberately not on the MCP surface, alongside the other admin RPCs.

- **The memory and event listings sort on more than two columns, in either direction.** `order_by`
  previously accepted `significance` or `timestamp` and always sorted descending. It now names any
  of seven columns per listing — memories add `time_recalled`, `recall_count`, `link_significance`,
  `group` and `id`; events add `time_end`, `name`, `link_significance`, `group` and `id` — and a new
  `order_dir` (`SORT_DIRECTION_ASC`/`SORT_DIRECTION_DESC`) reverses any of them. Existing callers are
  unchanged: both fields default to what these listings did before.
  - An omitted `order_dir` means each column's **natural** direction rather than ascending —
    descending for the magnitude and time columns, ascending for the lexical ones (`id`, `group`,
    an event's `name`) — since alphabetical is the only reading of a name anyone wants first.
  - Reversing applies to the tiebreakers too, so `ASC` is the exact reverse of `DESC`; every
    ordering still ends in an id tiebreaker, so offset paging stays stable.
  - A never-recalled memory (`time_recalled` 0) and an unended event (`time_end` 0) sort as the
    least recent rather than dropping out of the page: an ordering that silently filtered would be
    the more surprising behaviour, and `recalled`/`time_end_min` ask that question directly.
  - An unaccepted `order_by` is now rejected with `InvalidArgument` listing the accepted values,
    where it previously surfaced as `Unknown` (a 500 over the gateway). The other range validations
    on those two RPCs are unchanged.
  - Reachable as `hippo memory list --order-by … --order-dir …` (and `hippo event list`), with
    per-command shell completion, and as `order_by`/`order_dir` on the MCP bridge's `list_memories`
    and `list_events`. The embedded console gains a Sort by/Direction pair on both filter forms and
    clickable column headers on both tables.
- **`GetEvents` can report each event's memory count without transferring the memories.** A new
  opt-in `GetEventsRequest.memory_counts` populates a read-only `Event.memory_count` from one
  aggregate query that reads no bodies at all. Previously the only way to say how much an event held
  was `memories: true`, which fetches every body on the page to count them. It is opt-in because it
  is a second query per page, separate from `memories` rather than derived from it, and counted
  within the caller's group scope exactly as the nested memories are filtered. Reachable as
  `hippo event list --memory-counts`; the embedded console's events table now always asks for it.
- **`GetSummarisationCandidates` reports `scan_enabled`.** An empty candidate list means one of two
  opposite things — nothing is due yet, or this instance does not scan at all (the threshold is 0, or
  it is a replica and runs no sleep cycle) — and only one of them is worth waiting on. A client
  presenting an empty list should branch on the flag, not on the list. The `hippo` CLI prints the
  configuration it would need; the MCP bridge projects it; the console renders a different empty
  state for each.
- **`GetEventById` can report the memory count too** (`memory_counts`), mirroring `GetEvents`.
  Reachable as `hippo event get --memory-counts`. The MCP bridge's `list_events` now always asks for
  counts (and still never for bodies).
- **`WhoAmI` reports `summariser_enabled`.** Whether the embedded LLM (`ollama.enabled`) is
  configured, so a client can offer service-authored summarisation only where `SummariseMemories`
  will serve rather than discovering its absence through a `FAILED_PRECONDITION` — the same
  deployment-property reporting as `search_modes`, and on the same terms.
- **A Bluesky firehose bridge, and with it engagement-as-recall.** `integrations/eventsource` gains a
  fifth adapter (`cmd/bluesky`) that consumes
  [Jetstream](https://github.com/bluesky-social/jetstream), the public JSON projection of the atproto
  firehose. It is the first bridge that _reinforces_ as well as writes: a post becomes a memory, and
  the likes, reposts and replies that follow it become `RecallMemories` calls against that post — so
  every post arrives with the same significance and only what people came back to survives.
  - **It needs no state.** A memory's id _is_ the post's `at://` URI, and a like names its target by
    that same URI, so reinforcing is a call rather than a lookup. A like for a post the bridge never
    ingested, or one the store has already forgotten, costs one `UPDATE` matching no rows. The same
    identity makes replay idempotent, which is what the cursor-gated at-least-once loop relies on.
  - **The bridge's token must be unscoped and writer-tier.** A group-scoped token makes the service
    scope-check ids before recalling them, turning an id it does not hold into `NotFound` for the
    whole batch; a reader-tier token gets a non-reinforcing read unless `auth.readerRecallReinforces`
    is set. Both are absorbed, so a misconfiguration degrades to "reinforcement stops working"
    rather than "the bridge stops consuming".
  - `--events thread` opens an event per thread root, `--honour-deletes` (**on by default**) maps an
    upstream deletion onto a memory deletion — decay is about significance, deletion is about
    consent — and `--dids`/`--langs` narrow the stream. Recalls are batched and deliberately
    best-effort: a lost like decays a memory slightly sooner, it does not make it wrong.
  - Ships as `ghcr.io/fastbean-au/hippocampus-bluesky-bridge` and a per-OS/arch release binary, like
    the other four. A demo is in `demo/bluesky.sh`. See
    [docs/eventsource.md](docs/eventsource.md#bluesky-the-firehose-bridge).
- **The Bluesky bridge can relate posts to each other, with no NLP.** `--topic-links` extracts topic
  terms from a post's link-card URL — a news URL's path is a slug someone wrote by hand, already
  tokenised on hyphens and chosen editorially — and relates posts sharing at least two of them,
  ignoring terms common enough to be section names rather than topics. Measured against a live news
  feed it relates about a quarter of posts, and the matches are cross-outlet. This is what makes
  `consolidation.linkRecallPropagation` do anything: with links, a like on one outlet's coverage
  pulls the others back from the threshold too, and `linkSignificanceWeight` makes a cluster of
  coverage more durable than a lone post. Links are issued **after** the write rather than attached
  to it, because a link target must exist and in a store whose job is forgetting, attaching them to
  the create would let a neighbour consolidated a minute ago fail the write itself; a backfill is the
  exception, attaching them to its `ImportBatch` whose second pass resolves intra-batch targets.

- **The Bluesky bridge can take its posts from a curated feed.** `--feed at://…` reads an atproto
  feed generator over HTTP instead of the firehose, while engagement keeps arriving on Jetstream —
  the feed decides what is worth storing, the firehose reports what people did with it, and the two
  meet by `at://` URI with no correlation state. It trades volume for legibility (tens of posts an
  hour rather than tens a second, every one of them readable), which suits a hosted demo where the
  firehose suits a local one. `--feed-backfill` seeds the store from the whole feed at startup, and
  `--feed-seed-recalls` carries each backfilled post's observed engagement across as a **damped**
  recall count — `round(log1p(likes + reposts))` — because those likes happened before the bridge was
  watching and the firehose will never report them. The damping matters: effective significance rises
  linearly with recall count, so a raw five-thousand-like count would make one post unforgettable
  forever. Seeding is the only write that uses `ImportBatch`, which is the one RPC that carries
  recall history, and it happens once; polling uses `StoreMemory` and treats `AlreadyExists` as
  "already have it", so re-reading the feed needs no bookmark and never rolls back live
  reinforcement.

- **The Bluesky bridge can capture the conversation around a feed's posts, not just the posts.** In
  feed mode a firehose post was reinforcement and nothing else, which left `--events thread` holding
  events of one memory. Two new flags widen that, independently:
  - `--capture-replies` stores a post that **replies to a thread the bridge holds**, matching on the
    thread root first (so it matches however deep the reply sits) and its parent after. With
    `--events thread` the feed's post opens the event and the public's replies become memories in it
    — the post-as-event, responses-as-memories shape. A captured reply still reinforces its parent;
    capture adds a memory rather than replacing the engagement.
  - `--feed-authors` stores a post by **any account the feed has surfaced**, so the accounts a feed
    is made of are followed rather than only the posts that feed picked. The DIDs are derived from
    the feed itself on each read, so there is no account list to maintain.

  `--capture-significance` ranks a captured post below the feed's own (a reply is worth keeping
  without being worth as much as the post it answers; without it they compete for the same capacity),
  and captured posts are deliberately **not** topic-linked, since a reply carries no link card and its
  body is conversation rather than an editorially written slug.

  Both indexes are bounded, in-memory and best-effort (`--capture-index-size`, `--feed-authors-max`);
  the capture index holds only what the feed produced, never the replies captured through it, so one
  busy thread cannot evict the posts every other thread is matched on. Neither works under `--dids`,
  because Jetstream's `wantedDids` selects on the repository a record was written **in** and a reply
  lives in the replier's — the same reason `--dids` beside `--feed` receives no engagement at all.
  All three combinations are now warned about at startup, having previously presented as a feed
  nobody appeared to be interacting with rather than as anything failing.

- **The event-sourcing bridges can authenticate with OIDC client-credentials.** `--oidc-issuer`
  (or `--oidc-token-url`) plus `--oidc-client-id`/`--oidc-client-secret` make a bridge mint and
  refresh its own access tokens instead of carrying a `--token` that eventually expires — at which
  point the bridge does not stop, it keeps consuming and fails every write with `Unauthenticated`,
  silently, for as long as it is left running. Setting a client id selects the grant over a static
  token. Configuration is validated at startup but discovery is deferred to the first RPC, so a
  typo fails immediately while an IdP that happens to be unreachable does not stop a supervised
  bridge coming up; a token that cannot be obtained fails the RPC rather than sending none, so on an
  at-least-once broker the message is redelivered. Flag names match the showcase generators', so one
  realm configures both.

- **The event-sourcing bridge core gained `Recall`, `Forget`, `EnsureEvent` and `HandleEvent`.** The
  client seam widened from one RPC to four named ones (`StoreMemory`, `StoreEvent`,
  `RecallMemories`, `DeleteMemories`), which is still a guard rather than the whole generated
  client: `Dial` hands back everything, so that unexported interface is the only thing standing
  between an adapter and `Purge`. `Handle`'s behaviour is unchanged, so the existing four adapters
  are untouched. Three new metrics come with it — `hippocampus.bridge.recalls`,
  `hippocampus.bridge.events` and `hippocampus.bridge.recall.batch_size` — where the `missing`
  recall outcome is the informative one: an id the store no longer holds is the decay model having
  already done its job, and its ratio to `reinforced` is the most useful number a reinforcing bridge
  produces.

- **The ingestor and the four broker bridges are instrumented.** Both were long-running daemons with
  no metrics and nothing to probe, which made their worst failure mode — stalling silently — look
  exactly like being idle. Each now serves `/healthz` and `/readyz` and exports OTEL metrics over
  OTLP/gRPC, sharing one implementation with the service via a new root-module `observability`
  package (`cmd/hippocampus/observability.go` promoted, not copied).
  - **Health surfaces.** `--health-port` (**8090 by default**, 0 disables) serves `/healthz`
    (liveness, never fails while the process runs) and `/readyz` (whether the Hippocampus instance
    can actually serve, checked via the token-free gRPC health service and named per dependency, so
    a failing probe says _which_ end is down).
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
    rows. There is no cursor and no bookmark: the edge store _is_ the queue, and what it holds is
    exactly what has not been judged yet.
  - **Judgement happens at event completion, not at ingest**, which is what makes a rule change
    reach in-flight data: an event still open when the rules reload is judged by the new ones. The
    file is re-read on an mtime change, a bad initial load fails startup, and a bad reload keeps the
    last good ruleset — the same contract as `auth.revocationFile`.
  - **A `promote` rule may reduce first**: `keepTopN`/`minSignificance` choose what crosses (the
    rest are still drained), or `summarise` calls `SummariseMemories` on the edge, which needs
    `ollama.enabled` there and fails the event loudly if it is absent.
  - **A `promote` rule may also rewrite what crosses.** A `set` block carries CEL expressions for
    `significance`, `group`, `metadata` (and `name`/`description` on the event), evaluated per event
    and per memory, so the edge can **re-rank** what it admits rather than only admit it —
    significance being the number the central store's whole decay model runs on. Only the promoted
    copy is touched, never the edge, which is drained regardless. The mutation runs **before** any
    reduction, so `keepTopN` ranks by the score the rule just set; values are bounds-checked against
    what the target would accept (a bad one fails the event loudly and leaves it on the edge rather
    than promoting it at a weight the file rejected); and `metadata` is **merged** over what the
    record carries, since CEL has no map union and stamping one provenance label is the common case.
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
  - **An empty scope means the whole store**, so an unscoped token is the _most_ privileged shape a
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

- **The embedded console is now four files rather than one, and is served under a strict
  Content-Security-Policy.** `index.html`, `styles.css`, `app.js` and `lib.js` — still no bundler
  and no build step, still embedded in the binary. The split is what makes
  `script-src 'self'; style-src 'self'` without `unsafe-inline` possible: every handler is a
  `data-act` attribute routed through one table, and every dynamic style goes through the CSSOM.
  An injected inline `<script>` no longer executes, which is the shape the console's one stored
  XSS took.
  - It is also what makes the JavaScript testable. `cmd/hippocampus/webuitest` runs `lib.js` under
    node's built-in test runner with **no dependencies** — no lockfile, no `node_modules` in a Go
    source tree — alongside drift guards pairing every control with a handler, every lib function
    with its import, and every embedded asset with a route and both middleware allow-lists.
  - The console's assets are revalidated by ETag rather than never cached, so a navigation costs
    two 304s instead of re-transferring them; the entry document stays `no-store`.
- **Controls disable themselves while their own request is in flight**, and a progress bar reports
  the requests no control started. Previously only sign-in did.
- **Search results gained "Show more" rather than pagination**, deliberately. `SearchMemories`
  over-fetches and then truncates, so a second page is a separately ranked query whose candidate set
  can differ — and a _reinforcing_ search recalls exactly the page it returns, so paging would
  reinforce a second set of memories, resetting their decay clocks, as a side effect of navigating a
  list. Re-running with a larger limit keeps it one query, one ranking, one reinforcement set.

- First run after a clone no longer needs a config file: an absent `./config.json` starts the
  service on built-in defaults (a `--config_file` given explicitly must still exist).
- **A memory's body (and an event's description) now gets a row of its own** in the web console's
  tables, spanning the width beneath the record's metadata, rather than competing for width as a
  column — where a single long unbreakable neighbour left it wrapping one word per line. Ids in the
  tables are middle-truncated with the full value on the tooltip and behind a copy button.
- The web console presents a **sign-in card in place of the console** when authentication is on and
  no session has resolved — on first load, after **Sign out**, and when a token is refused. The
  header's always-present bearer-token box is gone; the token is entered on the card instead. Purely
  a console change: the server enforced (and still enforces) authorisation on every RPC regardless of
  what the page shows.
- **The web console shows compact ages instead of absolute datetimes**, with the exact timestamp on
  the tooltip. Age since the last recall is what every consolidation method actually runs on, so it
  is the number that says whether a row is near being forgotten; it is also the narrowest form of the
  widest column in those tables.
- **Summarise buttons in the console's event view** — one calling `ReplaceMemoriesWithSummary` with
  text you write, one calling `SummariseMemories` and shown only where the embedded LLM is
  configured. Both confirm first, naming how many memories go. They live in the event view rather
  than on an events-list row because the operation replaces every memory of the event, and that is
  the one place the console shows what is about to go.
- Tidied the console's memory filter: the timestamp from/to controls are a matched pair on a row of
  their own.
- **The console hides controls this deployment cannot serve**, extending what it already did for the
  search-mode picker and the LLM summarise button. On a replica the whole Decay tab is absent — nav
  button included — along with the tables' per-row Value column and the summarisation-candidates
  card, since every one of them is served by an RPC a replica refuses; the forgotten-log panel
  appears only where `consolidation.tombstones.enabled` is set. An operator seeing no Decay tab is
  looking at a replica.
- **The console's create and filter cards are full width** on the Memories and Events tabs, matching
  the search card, instead of sharing a half-width two-column grid.
- **Enter runs the card it is typed in** on the Search, Memories and Events tabs — search, create,
  filter, and add-link. None of those cards is a `<form>`, so Enter previously did nothing and every
  field had to be left by hand to reach its button. A memory body and an event description still
  take Enter as a newline, and the two destructive forms (the summary that replaces an event's
  memories, and clearing the forgotten log) are deliberately left out.
- **The console's cards no longer name the endpoint they call.** Each blurb says what the card does
  and stops there; a reader after the API has `/v1/openapi.json`. What stays in `<code>` is what an
  operator would have to change — the config keys, and the `--mint-token` invocation.
- **An opened event now stands first on the console's Search tab** and is scrolled to, rather than
  appearing between the search form and the results — it arrives by navigation from another tab, so
  it has nothing to do with the query it was sitting under. The links panel is scrolled to for the
  same reason.
- **A table row's action buttons stay inside the card at any width**, pinned to the right edge of the
  scroll container while the rest of the row scrolls beneath them. These tables have a min-content
  width that no styling removes, so a narrow-enough viewport scrolls them — and what went out of view
  was always the buttons, being last.
- **A summarisation-candidates card on the console's Events tab** — the scan's list, refreshed on
  demand, with each event opening into the view the summarise buttons are in.

### Fixed

- **A group-scoped administrator was shown no forgotten log**, though the service serves them one.
  `GetForgottenMemories` is scope-filtered rather than refused to a scoped caller — a tombstone
  carries its memory's group, so a scoped admin sees exactly their own partition's losses — but the
  console gated it on a capability that folded "unscoped" into "is an admin", the test that
  `PreviewConsolidation` genuinely needs. The two are now separate, and the console hides only what
  the service will actually refuse.
- **`formatBytes` reported an unknown figure as `0 B`.** `Number(value || 0)` folded `NaN` to zero
  before the guard that existed to catch it, so a missing byte count read as a fact about the store
  rather than as a missing number. Absent (an unset field, genuinely zero) and unknown are now
  distinguished.
- **A status poll arriving during startup could report "no timed cycle" on an instance that has
  one** — the sleep timer is created inside its own goroutine, leaving a window before it was
  scheduled. The next-cycle time is now recorded before the goroutine launches.

- **A Bluesky bridge wedged permanently on a record the store already held.** Every adapter acks
  after the write, so a redelivery re-presents a memory that was already stored — and with the id
  derived from the upstream record, that is a duplicate rather than a second copy. `Store.Handle`
  returned the resulting `AlreadyExists` as an error, so the adapter was told a frame it could never
  store had not been handled: it retried, dropped the connection, resumed from the same cursor, and
  was handed the same record again. The hosted demo sat in that loop for hours, reading nothing after
  the poisonous frame and so reinforcing nothing at all. `Handle` now counts it as handled, reported
  as `hippocampus.bridge.messages{outcome="exists"}` — success, but distinct from `stored`, since a
  bridge whose whole stream is duplicates is doing nothing. The Bluesky consume loop additionally
  **skips** a frame the service can never accept (`InvalidArgument`, `AlreadyExists`,
  `Unimplemented`, `OutOfRange`) instead of replaying it; every other code, including the
  `FailedPrecondition` of an event consolidated mid-write, keeps its replay.
- **A thread's opening post was missing from the thread**, in two places. In the bridge core, a
  nested memory carries no `event_id` of its own (the service stamps the event's on it), so
  `HandleEvent`'s already-exists fallback — which stores them individually — wrote them with no
  event at all; the very case that fallback exists for, a post arriving after a reply has opened its
  event, produced a thread without its own opening post. In the Bluesky feed path, a post that was
  not a reply opened no thread at all under `--events thread` (only the firehose path did), so its
  event appeared later, opened by a captured reply, holding every reply except the post they
  answered. Both now stamp the event they belong to; a feed post's own thread is named from its
  text.
- **The console's scroll-to-top did nothing under `prefers-reduced-motion: reduce`.** Chrome does
  not shorten a `behavior: "smooth"` scroll when reduced motion is requested — it drops it — so
  editing a memory or an event from a row left the page exactly where it was, for precisely the
  reader who most needs it to have moved. The behaviour is now chosen from the media query.
- **Summarising through either RPC left the event in the candidate cache**, so
  `GetSummarisationCandidates` went on offering an event the service had just condensed until the
  next sleep cycle refreshed the scan. Only the auto-summarisation path pruned; the pruning now lives
  at the chokepoint both paths share.
- **Opening an event in the web console destroyed the search results.** The event view was written
  into the search results panel, and an event id is clickable in all three tables — so opening one
  from the Events or Memories tab silently replaced a search that had been run, and returning to the
  Search tab found that single event in place of the results. The event now has a card of its own.

- **A Bluesky feed poll stalled permanently on the first reply in a page.** Under `--events thread`
  the Transformer puts a reply's memory in its thread root's event, and the service refuses a memory
  naming an event it does not hold — but only the firehose path ever opened that event, never the
  feed path. The refusal then aborted the write of the whole page, so every post after the reply went
  unwritten; since a poll returns the same page each time, the next tick stopped at the same post and
  the store never grew past it. Two fixes: the feed path now opens a reply's thread before writing
  it, as the firehose path always has, and a batch write **skips** a memory whose event is missing
  rather than aborting on it (counted as `hippocampus.bridge.memories{outcome="orphaned"}`).

- **A Bluesky feed backfill failed permanently if the IdP was still booting.** The startup seed ran
  once, with no retry, so a bridge starting alongside its identity provider could not mint a token,
  logged one warning, and ran unseeded until somebody restarted it — which any stack bounce could
  cause. The seed is now retried past a cold start (eight attempts over about four minutes) before
  giving up and leaving the store to fill from live polling.

- **The `/v1` gateway returned `404` for any id containing a `/`.** Every by-id path — `GET`/`PATCH`
  a memory or event, the link endpoints, `EndEvent`, `UpdateEventSignificance`, the two summary
  routes — failed for an id the caller had percent-encoded correctly, because the gateway matched
  routes against the already-unescaped path: a `%2F` inside an id had by then become a real
  separator and split the id across path segments. Ids are caller-chosen and full of slashes in
  practice (an `at://` URI is what the event-source bridges write), so this took out most of the web
  console's row actions on any Bluesky-sourced store, and the same calls from the `hippo` CLI's HTTP
  transport and the Obsidian plugin. The gRPC surface was never affected. No client change is
  needed: an id must be encoded as one path segment, as those clients already did.

- **The OpenSearch index dropped every memory whose id contains a `/`.** The document id went into
  the `/<index>/_doc/<id>` request path unescaped, so an `at://` URI became six path segments and
  the cluster answered `no handler found for uri ... and method [PUT]`. Indexing and deleting missed
  alike: nothing failed visibly on the write path, the index simply stayed empty of those memories
  and `SearchMemories` returned nothing for them — which on a Bluesky-sourced store is nearly
  everything, since the bridge ids each memory by the post's `at://` URI. The id is now
  path-escaped; it is still stored and returned by the cluster in its original form, so hits still
  name a memory the primary store can be asked for and no reindex is needed beyond re-indexing the
  memories that never landed (`--backfill-search`). The one shape escaping cannot survive — a
  cluster address carrying a path prefix, where the client library rebuilds the path from its
  decoded form — is now warned about at startup. This is the same class of fault as the gateway
  `404` above, in a different layer.

- **A bridge truncating a body with `--max-body-bytes` could produce a message that never delivered.**
  The truncation sliced raw bytes, so a multi-byte character straddling the budget was cut in half
  and the memory failed to _marshal_ (`string field contains invalid UTF-8`) before it ever reached
  the service. Because the fault was in the message rather than the service, redelivery could not
  clear it: on an at-least-once broker it was a poison message nacked and retried forever. The cut
  now backs up to a rune boundary. Affected all bridges, on any non-ASCII payload; found by running
  the new Bluesky bridge, whose posts are full of emoji, against the live firehose.

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

[Unreleased]: https://github.com/fastbean-au/hippocampus/compare/v0.33.2...HEAD
[0.33.2]: https://github.com/fastbean-au/hippocampus/compare/v0.33.1...v0.33.2
[0.33.1]: https://github.com/fastbean-au/hippocampus/compare/v0.33.0...v0.33.1
[0.33.0]: https://github.com/fastbean-au/hippocampus/compare/v0.23.0...v0.33.0
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
