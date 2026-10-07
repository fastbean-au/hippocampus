# The storage layer

This is the design record moved out of CLAUDE.md verbatim (TODO-3 item 176): why the code is the way it
is, the alternatives that were rejected, and the incidents behind the guards. CLAUDE.md keeps the short
version and links here.

## `db/`

`db/` — storage layer. One `DB` struct speaks three SQL dialects, selected by `storage.driver`
(`sqlite`, the default, `postgres`, or `mysql`); nearly all query and consolidation logic is
shared. **Everything the package knows about how the dialects differ lives in `db/dialect.go`**
(plus `metadata.go`, the JSON accessors, and `search_dialect.go`, the three content indexes) — a `dialect` table with one row per dialect carrying the
column types, expression fragments and capability flags the shared code splices in, alongside the
handful of helpers whose difference is structural rather than lexical (`upsert`, `ensureIndex`/
`dropIndexIfExists`, `columnProbe`, `registryLock`, `rebind`). `TestDialectKnowledgeIsConfined`
fails the build if any other non-test file compares `d.driver`, which is what makes a fourth
dialect a new row rather than a hunt through thirteen files. In particular `coreSchemaStatements`
and `coreColumnMigrations` are the events/memories schema written **once**: the three copies they
replaced differed only in column types, so keeping them apart bought nothing and risked the one
failure this schema cannot afford — a column added to one dialect's copy and forgotten in another,
which yields a store that opens, serves, and is missing a field on exactly one backend.
`db/schema.go` is the other half: **one ordered, versioned migration list for every dialect**
(`migrations()`), run by a single `initSchema` — what used to be three per-driver initialisers,
which after the dialect table differed only in three dialect _capabilities_ (the embedded dialect
configures incremental vacuum and carries the content index, one server dialect migrates an id
collation, both keep a peer registry). Four things carry it. (1) The point is the **version gate**:
"downgrading is not supported" was a sentence in `CHANGELOG.md` with nothing enforcing it, and an
older binary opened a newer store, found every table it expected, and served — right up until it
met a migration whose meaning had moved. A store now records its version in `schema_migrations`
and a build that does not understand it refuses to open with `ErrSchemaTooNew`; the read-only tool
opens apply the same gate in a form that tolerates the ledger being **absent** (a pre-ledger store
must still be backfillable). (2) The ledger **records, it does not decide**: every migration runs
on every startup exactly as before, because each detects its own completion. Skipping recorded
steps was tried and reverted — it saves eight round trips and gives up self-healing, which
`TestSchemaHealsARevertedMigration` now pins. It is also what makes the ledger safe to add to
existing stores: no baselining, no "assume everything up to N already ran" heuristic. (3)
**Versions are never renumbered or reused** — a new migration appends — because a renumber
silently re-points every stored row at a different step; `TestMigrationVersionsAreStable` pins the
mapping, and `schemaFixtureTags` (`db/schema_upgrade_test.go`) requires each migration to name the
released fixture that exercises it. (4) The run is serialised across instances by a third named
lock (`namedLock`, beside the single-consolidator and significance-registry locks), so two
replicas starting together cannot race one another's `ALTER TABLE`; that lock pins a connection,
which is why the pool must not be capped at one before `initSchema` runs. The
`db.Store` interface (in `db.go`) is what
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

## `db/link.go`

`db/link.go` — the **link graph**: `memory_links` and `event_links`, one directed row per edge
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
