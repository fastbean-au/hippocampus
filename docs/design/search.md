# Content search

This is the design record moved out of CLAUDE.md verbatim (TODO-3 item 176): why the code is the way it
is, the alternatives that were rejected, and the incidents behind the guards. CLAUDE.md keeps the short
version and links here.

## `search/`

`search/` — the secondary content-search index (`search.Index` interface with three
implementations: no-op, `SQL`, and `opensearch-go/v4`). **Two backends, selected in main.go:**
OpenSearch when `opensearch.enabled`, otherwise `search.NewSQL` over the primary store — so
`SearchMemories` works out of the box on **every** driver instead of failing closed, which is what
it did while OpenSearch was the only backend and, until item 96, what it still did on
`postgres`/`mysql`. Only a store that can carry no index at all (a read-only tool open) leaves the
no-op in place, logged at startup and surfaced as `FailedPrecondition`.
- **The store's own index is optional** (`search.contentIndex.enabled`, TODO-2 item 112.1),
  **derived when unset** on the `reflection.enabled` precedent: on with `opensearch.enabled` false,
  off with it true, overridable in either direction and logged with its reason. It exists because
  only one backend is ever selected, so on an OpenSearch deployment the SQL index was written for
  every memory - inside the storage boundary, before compression - and read by nothing for the life
  of the store, while being the largest non-body cost the store carries on every dialect (measured:
  43-59% of payload on SQLite, and on MySQL a second UNCOMPRESSED copy of every body, since a
  `FULLTEXT` index is an index on a column). Four things carry it. (1) It **drops** the index rather
  than ceasing to write it: an unmaintained index does not become empty, it becomes WRONG, answering
  from a subset that shrinks with every cycle - and dropped, `ContentSearchAvailable` is already
  false and `SearchMemories` already answers `FailedPrecondition`, the behaviour item 96 built for
  the read-only opens reached by a second route. (2) It is a **constructor `db.Option`, not a
  setter**, because `initContentSearch` runs inside `initSchema`; a decision arriving after the
  constructor would have created and populated the index before anything could say not to.
  (3) It does **not** gate the migration, which is recorded either way - gating it would move a
  store's schema version up and down as the key changed, and a build meeting the higher of the two
  would refuse `ErrSchemaTooNew` on a store it understands perfectly. (4) The SQLite **trigger goes
  first** in the drop: a trigger's body is resolved when it fires, so one left pointing at a dropped
  table turns every DELETE from `memories` into an error - consolidation, eviction, `Clear` and
  `Purge` failing at once. `--backfill-search` passes the same setting through, since it opens
  read-WRITE and would otherwise recreate the index the service was configured to drop.
- `search/sql.go` — the store-backed backend. A thin adapter: `Search` delegates to
  `db.SearchMemoryHits`, and **every mutator is deliberately a no-op**, because the index is
  maintained inside the primary write rather than propagated to afterwards (wiring them up would
  double-index). It reaches the store through the `ContentStore` interface declared in the
  package, so `db` is not imported for its concrete type and the adapter is fakeable. `Rebuild`
  is a concrete method, not part of `Index`, like OpenSearch's `RecreateIndex`/`IndexMemorySync`.
  The `Index` doc comment was amended when this landed: async best-effort propagation is the
  OpenSearch implementation's _strategy_, not the contract, which promises only that mutators
  return without error.
- The actual index lives in `db/search.go` (the shared flow) and `db/search_dialect.go` (the
  three implementations, and the **third** file allowed to know which dialect is active — see the
  `dialectFiles` allow-list). What is shared is when to index, what to index, the backfill, the
  rebuild, the filters and the scope; what is not is the index shape and the query language, which
  do not reduce to a fragment. **SQLite** keeps an **FTS5 virtual table**, available with no cgo
  and no new dependency because `modernc.org/sqlite` is built with `SQLITE_ENABLE_FTS5`; it is
  contentless (`content=''` + `contentless_delete=1`) — it holds the inverted index and not a
  second copy of the bodies. **Postgres** keeps a `tsvector` table under a **GIN** index, matched
  with `@@ to_tsquery('simple', …)` and ranked by `ts_rank`; `simple` rather than `english` because
  it neither stems nor drops stopwords, which is what FTS5's tokeniser and OpenSearch's standard
  analyser both do. **MySQL** keeps a **`FULLTEXT`** index over a text column, matched with
  `MATCH … AGAINST (… IN BOOLEAN MODE)` — which is both the predicate and the score — and is
  therefore the one dialect that **does** hold a second, uncompressed copy of every indexed body,
  because a `FULLTEXT` index is an index on a column. (The other two hold an inverted index rather
  than the text, which is a privacy property more than a size one — for short bodies a `tsvector`
  is about as large as the text it came from.) The index sits outside `UsedBytes` on the server
  dialects and inside it on SQLite, whose page accounting cannot exclude a table in its own file. The contentless-ness of the other two is
  load-bearing twice over: storing the text again would give back much of what body compression
  exists to save, and the obvious alternative (an external-content table over `memories.body`) is
  impossible since that column can hold a gzip stream. Every write therefore feeds the index the
  **plain** body from inside the storage boundary, before `compressBody`, truncated on a rune
  boundary at `contentIndexMaxBytes` (512 KiB, applied on all three because Postgres's `tsvector`
  has a hard 1 MB ceiling that is an ERROR rather than a truncation — so an unbounded body would
  fail to index on exactly one backend). **Deletes are the storage engine's**, never a call
  site's: an `AFTER DELETE` trigger keyed on `OLD.rowid` on SQLite, a foreign key with
  `ON DELETE CASCADE` on the server dialects — covering consolidation, eviction, `DeleteMemories`,
  `DeleteEventMemories`, `Purge`, `Clear`, and import-replace with no hooks and no possibility of
  drift, in the same transaction. (A consequence worth knowing: a test that drops the `memories`
  table must drop `memories_fts` first, which `dropMemoriesTable` in `conformance_test.go` does.)
  Inserts/updates are hooked explicitly in `CreateMemory`, `UpdateMemory`,
  `ReplaceMemoriesWithSummary`, and `ImportMemories` (the last via `reindexMemoryContent`, since an
  import is an upsert and a plain insert would leave a row matching both its old and new body);
  those are synchronous but **not** transactional, so a failure is logged and never fails the
  write. `initContentSearch` populates the index at startup when it is empty on a non-empty store,
  which is what makes an upgrade need no manual step — the server dialects' half of that is
  migration 14 (`content_search_sql`, gated on `dialect.contentIndexCascades`; SQLite's is
  migration 12, and the two are separate versions because they shipped in different releases and a
  store must record which index it actually has). Query text is never passed through: `contentTokens`
  splits it into bare alphanumerics and each dialect's `contentMatchExpression` quotes them and
  joins them with its own OR, since all three MATCH arguments are query languages whose operators
  would otherwise be a syntax error or a way to reach past the caller's intent; OR is chosen to
  match the OpenSearch backend's `match` semantics so all of them agree on which memories match,
  with relevance favouring those matching more tokens.
- `--backfill-search` without OpenSearch routes to `rebuildContentSearch` in `backfill.go`, a
  separate entry point because it **writes to the service's own database** (so it must not run
  beside a live instance, unlike the read-only OpenSearch backfill). It covers all three drivers;
  the server opens are replicas, since taking the single-consolidator lock would make the tool
  fail against a healthy deployment for a reason unrelated to what it writes.
- **Ranking** (`hippocampus/ranking.go`, `search.significanceWeight`/`search.recallWeight`,
  defaulting to 0.3/0.2) blends the store's own view of a memory — significance and recall count
  — into `SearchMemories`' result order, so the differentiator actually shapes retrieval instead
  of relevance alone deciding. `search.Index.Search` therefore returns `[]search.Hit` (id +
  score) rather than ids; **each backend normalises the score's direction** so higher is always
  better (the SQL backend settles it per dialect in `contentSearchTerms` — FTS5's bm25 runs
  backwards and has its sign flipped, `ts_rank` and InnoDB's relevance already run the right way;
  OpenSearch's `_score` already is), while magnitudes stay incomparable between backends and, for
  this one, between dialects. The blend runs at
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
  brute-force — so it would buy a constant factor, not scale. `pgvector` is the open question item
  96 left deliberately open: it is a server-side extension, so it costs the pure-Go build nothing
  and does real ANN, but the vectors would have to live in the primary store, which reverses item
  56's capacity decision — a separate argument, not part of the keyword work. The two halves are independent and
  neither implies the other: the `embed` package produces vectors, OpenSearch's k-NN index stores
  and searches them; `main.go` **fails startup** if `llm.embedding.enabled` is set without
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
  fixed at index creation, so `llm.embedding.dimensions` is validated in `embed/ollama.go`
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
  memories through the normal async `IndexMemory`, healing missing documents (idempotent). Since
  item 84 that sweep runs in **both directions**: `staleSweep` (`hippocampus/outbox.go`,
  `opensearch.staleSweep`, on by default) enumerates the index and removes documents the primary
  store no longer holds. Three things carry the reverse pass. (1) It exists because the forward
  direction alone made the system **self-heal one way and drop both**: a dropped index operation is
  recovered by the sweep, a dropped delete was permanent, and the divergence only ever grew — 4.38M
  documents against 211,657 rows on a live deployment. (2) It enumerates on the mapped `timestamp`,
  **not `_id`**, because `_id` has no doc values and sorting on it loads the field as fielddata:
  measured at 26 bytes per document, which is heap cost proportional to the very thing the sweep
  exists to bound. (3) A timestamp is not unique, and the cursor cannot be a plain `search_after`
  key, because **the caller deletes what it enumerates** and every removal shifts what follows. So a
  page normally ends on a timestamp **boundary** (the trailing group sharing the final instant is
  held back), which nothing the caller deletes can move; only a page wholly inside one instant is
  `Partial`, and there the caller subtracts its own deletions from the offset. The timestamp is read
  from a **doc-value field, never the sort value** — the SDK decodes sort values into `[]any`, and a
  UnixNano through float64 rounds by up to 256ns, upward as often as not, which steps the cursor
  past documents nothing comes back for.
  The **delete outbox** is the other half, and the mechanism to the sweep's backstop: `db/outbox.go`
  records one `search_outbox` row per deleted memory **inside the delete's own transaction** (the
  four chokepoints `pruneLinks` already funnels through), and a drain worker on the consolidating
  instance claims → `DeleteMemoriesSync` (synchronous, bypassing the lossy queue) → confirms →
  prunes. Claim does not consume, so a crash mid-pass replays rather than loses; re-deleting an
  absent document is a no-op, which is what makes at-least-once right here. Four things to preserve:
  `SetSearchOutbox` gates the **recording and the drain together** (a store queueing deletions
  nothing drains is worse than one queueing none — a sleep cycle deleting 10k memories would leave
  10k rows forever); the outbox is **excluded from SQLite's `UsedBytes`**, the tombstone lesson in
  sharper form, since it grows precisely when deletions are backing up and would otherwise evict
  live memories to make room for itself; it is **deletes only**, because a lost index operation
  self-heals and a row per write is real cost on the write path; and it exists only for OpenSearch —
  the SQLite FTS backend's deletes are an `AFTER DELETE` trigger and so were always transactional,
  which makes this parity rather than a new guarantee. The manual one is the
  `--backfill-search` CLI mode (`backfill.go`), which rebuilds it from the primary store via synchronous
  `IndexMemorySync`/`RecreateIndex` calls that bypass the queue (safe: the tool has no worker or
  live writes of its own) and `db.GetIndexableMemoriesPage` keyset pagination — with `--reindex`
  it recreates the index first to clear stale documents. Each driver opens read-only so the tool
  can run beside a live service: SQLite via `db.NewSQLiteReadOnly` (`mode=ro`, no `initSchema`,
  `Preserve` a no-op — so it never writes DDL or checkpoints the database the service owns),
  Postgres/MySQL via `db.NewPostgresReadOnly`/`NewMySQLReadOnly` (skipping the instance lock). Integration tests skip unless `HIPPOCAMPUS_TEST_OPENSEARCH_URL` is set;
  `deploy/compose/docker-compose.opensearch.yaml` runs the full stack.
