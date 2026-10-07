# Consolidation and forgetting transparency

This is the design record moved out of CLAUDE.md verbatim (TODO-3 item 176): why the code is the way it
is, the alternatives that were rejected, and the incidents behind the guards. CLAUDE.md keeps the short
version and links here.

## `hippocampus/`

`hippocampus/` — the gRPC service implementation (`Server` in `server.go`). Reads its config
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
  snapshot (whose one live input, the computed default event significance, is an atomic read
  through `defaultEventSignificance()`, after the snapshot was found reading the plain field under
  `-race` — TODO-3 item 164), and `shouldConsolidateUnder`/`memorySignificanceUnder`/`memoryValueUnder`/
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
- `recordAncillaryStorage` (`hippocampus/ancillary.go` + `db/ancillary.go`, called from `sleep`)
  publishes `hippocampus.ancillary_bytes` and caches the reading for `GetConsolidationStatus`'s
  `ancillary` block, which is what the console's Deployment tab and the shipped
  `HippocampusAncillaryStorageHigh` rule read. It is the **reporting** half of TODO-2 item 112.3 and
  decides nothing. Five things carry it. (1) It reports exactly the figure `UsedBytes` **subtracts**
  — the three helpers (`tombstoneBytes`/`searchOutboxBytes`/`callbackQueueBytes`) and this method
  now go through one `ancillaryTable`, so what the console shows and what the controller ignores
  cannot become two numbers. The exclusion itself is unchanged and correct; what was missing was any
  way to see how large the excluded part had grown, and on the embedded deployment those three
  tables share the store's own file, so a receiver that is down while a large cycle runs grows the
  disk while capacity pressure sits exactly where it was. (2) The **error policy is deliberately
  opposite** at the two callers: `UsedBytes` swallows a failed count and subtracts nothing (which
  over-counts the store, so it errs toward forgetting slightly harder), while the report returns the
  error and the server leaves the PREVIOUS measurement standing — a stale figure carries its own
  `measured_at` and can be read, whereas a fresh zero says the queues are empty when nobody knows.
  (3) It is measured **once per cycle and cached**, never per request: a count of the callback queue
  is a scan of up to `callbacks.maxRows` rows on the server dialects and this is an RPC a console
  polls, which is item 25.9's lesson and `ExplainConsolidation`'s snapshot cache by another route.
  (4) `enabled` means "is recording", while the rows are counted whenever the **table exists** —
  disabling any of the three stops the writing _and_ the trimming and leaves everything already
  written in place, so a table nothing records into can still be holding megabytes, and that is the
  state the report most has to avoid hiding. (5) The gauge is per **table** (`component`) and
  publishes nothing for a table the deployment never enabled, on the external axis's reasoning: a
  flat zero reads as a queue that is keeping up. Only the callback queue's rows have no fixed size,
  which is why its row cap bounds its bytes loosely and why the byte-cap half of 112.3 is still
  open.
- `recordStorageFootprint` (`hippocampus/footprint.go` + `db/footprint.go`, called from `sleep`)
  is the same move in the opposite direction: what the tables `UsedBytes` **does** count really
  occupy on disk, per table and per index. `UsedBytes` is a live-row estimate on the server
  drivers and **stays one** — eviction driven by a file-size measure would chase a reading that
  never drops after a delete — but nothing compared the estimate with the disk, and a measured
  instance reported 153 MB against a 160 MB `capacityBytes` while holding an 892 MB database, of
  which 533 MB was the covering index alone at 5% leaf density. A **store that forgets is a store
  whose indexes bloat**: `VACUUM` marks a B-tree page a delete emptied as reusable and never
  repacks it, and a store keyed on a UUID never refills one. Six things carry it (TODO-2 item 124).
  (1) It is **reported and never acted on** — `REINDEX CONCURRENTLY` is online but is still a
  maintenance decision, and there is no seam for it anyway, `Preserve()` being a correct no-op on
  both server dialects. `docs/operations.md` carries the runbook and the **daily** cadence the
  regrowth rate justifies. (2) Everything is a **catalogue lookup** (`pg_total_relation_size`,
  `pg_relation_size`, `pg_class.reltuples`), two statements per table, reading no rows.
  `pgstatindex`'s `avg_leaf_density` is the authoritative figure and is deliberately **not** taken:
  it reads every page of the index, which is item 25.9's cost on the very index this is about, and
  it needs an extension a managed instance may not have — bytes against entries says the same thing
  for free. (3) It is **per index**, because only that says what to act on, and because it is what
  makes the small-store case visible: bloat tracks churn rather than row count, and a store of
  1,011 memories was measured carrying a listing index at 3,581 bytes an entry. (4) The **estimate
  travels with the measurement** — the cycle's own cached `UsedBytes`, never a second scan — since
  `used_bytes` is published only under a byte capacity target and the **ratio** is the finding.
  (5) A driver that cannot answer publishes **nothing**, not a `measured:false` block: a zeroed
  footprint reads as a store occupying no disk, and that is the ordinary state on SQLite, which has
  nothing to report, its page accounting already counting every index inside the target. MySQL
  answers **per table only** (per-index sizes need a grant an application user lacks), and only
  because `dialect.catalogueSession` pins one connection per measurement with
  `information_schema_stats_expiry = 0` and resets it after. Left at the default, that cache answers
  with a size up to a day old. It is a session statement rather than a DSN parameter so that a server
  refusing the variable fails the reading, not every connection.
  (6) The set of tables is exactly `usedBytesLiveRows`' plus the content index, because a footprint
  covering tables the estimate excludes would report a gap that was never the estimate's to close.
  Published as `hippocampus.disk_bytes` plus `hippocampus.index_bytes`/`.index_entries`
  (`table`, `index` — a bounded set, and the storage layer caps what any one table reports so an
  operator's own indexes cannot grow the series count), served as `GetConsolidationStatus.footprint`,
  alerted on by `HippocampusStoreDiskFarAboveEstimate`/`HippocampusIndexBloated`, and logged when
  the ratio crosses — naming the index costing the most per entry, which is not always the largest.
- The **forgotten log** (`db/tombstone.go` + `hippocampus/forgotten.go`,
  `consolidation.tombstones.*`, off by default) is the third leg of the transparency trio and the
  only one that can speak about a memory that no longer exists: one row per memory the two decay
  paths delete, carrying id/group/event/significance/stored size, the `value` the pass computed
  and the `threshold` then in force, the rule, and when. Six things carry it. (1) The write is
  **inside `deleteMemoriesIfUnrecalled`**, not on `SetMemoryDeleteObserver` — that seam fires
  post-commit with ids only, by which point the row is gone and the fields a tombstone needs are
  unreadable. The rule travels in as a `forgetReason` parameter because the chokepoint also serves
  `Clear`, which is data movement rather than forgetting and passes the zero reason; the
  client-initiated deletes never reach it at all. (2) The capture is a **primary-key lookup over
  the rows a chunk is about to delete**, joined to `significance_levels` for the frozen rank —
  which is what keeps the consolidation scans on the covering index, since they deliberately never
  read `group_name` or `length(body)`. Only `value` comes from the pass, computed only when the log
  is on. (3) Capture before, write after, **filtered by the ids that actually went**, so the
  recall-race guard can never leave a record claiming a surviving memory was forgotten. (4) A write
  failure **fails the batch** — it is in the delete's transaction, so on Postgres best-effort is not
  available, and the honest reading is right anyway. (5) It must not eat the store it lives in:
  `maxRows`/`maxAgeInDays` are defaulted even though the feature is off, trimmed by
  `PruneTombstones` from the cycle (before `preserve()`), and the log is **excluded from
  `UsedBytes`** — the server drivers count live rows and exclude it already, but SQLite's page
  accounting would otherwise let the record of what was evicted raise capacity pressure and evict
  live memories to make room for itself. That exclusion is `COUNT(*)` × a flat allowance, not a
  scan of the log (item 25.9). The row cap needs the surrogate `seq`, since a whole batch shares one
  `forgotten_at`; `seq` also gives the reader keyset pagination and lets an id be forgotten twice.
  (6) **Disabling deletes nothing**: `PruneTombstones` is gated on `enabled`, not only on the caps,
  so turning the feature off stops the writing _and_ the trimming; emptying the log is always
  `DeleteForgottenMemories`, which refuses an empty request. `Purge` is the one exception, and is
  itself the explicit request to leave nothing behind. Both RPCs are `admin` but **scoped by
  predicate**, not refused like the preview — a tombstone carries its memory's group. Bodies are
  never recorded; not on the MCP surface. See TODO 57.2.
- **`callbacks.backlogPolicy`** (`hippocampus/callbacks.go` + the prune half in `db/callbacks.go`)
  decides whether a `memory_forgotten` delivery is a **notification** the queue's caps may discard
  or an **instruction** they may not. The queue was built for the former and its caps are right for
  one; the latter is what it becomes whenever the receiver is the system holding what the memory
  pointed at (`Memory.external_bytes`), and discarding it orphans that payload permanently — a leak
  that is monotonic, silent, and grows precisely with how well the decay cycle is working. Six
  things carry it. (1) **Three values, differing in who absorbs the failure**: `abandon` (the
  default, and exactly the previous behaviour) costs the far system orphans, `retain` costs this
  disk a queue nothing trims, `stall` costs the workload a store that stops forgetting. Nothing here
  makes the failure cost nothing, which is why it is configuration rather than a fix. (2) **`stall`
  needs no limit of its own**: the same three caps it exempts the deletions from stop _trimming_ and
  start _gating_, so there is one bound rather than two that can disagree — and that is also why
  `stall` with no cap is **refused** at startup, being a policy where nothing is trimmed and nothing
  ever stalls. (3) The other two kinds stay capped under every policy: a sleep-completed summary and
  a pre-reap warning are worthless once stale. (4) **The pull path is the forgotten log**, not a
  reverse sweep — this service cannot enumerate the far system, but the log already _is_ the
  ordered, durable, keyset-paginated record of these instructions, so a rebuilt receiver pages
  `GetForgottenMemories` newest-first back to its own cursor and a repeated delete is a no-op.
  Startup warns when a retaining policy runs with the log off. (5) **A stalled cycle is reported,
  not inferred**: it publishes zero consolidated and zero evicted, identical to a quiet store, so
  `CycleReport.stalled`/`stalled_reason` exist, `cycleSummary` in the console reads the flag before
  the counts, `hippocampus.forgetting.stalls` counts it, `HippocampusForgettingStalled` alerts on
  it, and the flag rides the `sleep_completed` delivery itself — the receiver being the one party
  that can end the stall. (6) Only the two **decay** passes are held; a client's `DeleteMemories`
  still deletes, and a failure to _measure_ the backlog is not a stall. The storage half is one SQL
  fragment on the three prune statements, empty unless the policy retains, with the byte cap's
  running total still summing every row and only its DELETE narrowed. See TODO 107.2/119.
