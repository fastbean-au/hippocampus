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
estimate); `walTriggerBytes` stays rejected in `initStore` (no client-visible WAL file) and
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
IF NOT EXISTS`). MariaDB (10.11+) is not a fourth driver but a derived row of the MySQL dialect
(`mariaDBDialect`), picked by `detectMariaDB` from `VERSION()` at every open, because the same DSN
reaches either server and a configured choice could only be wrong. The row differs in three
respects and no others: upserts spell the proposed row `VALUES(column)` (MariaDB never took up
the `AS new` alias), the catalogue reads need no `information_schema_stats_expiry` session (MariaDB
has no statistics cache to expire), and its own measured row allowances (MySQL's over-read
MariaDB's storage by up to 2.1x). `TestMariaDBDialectDiffersOnlyWhereItMust` holds it to that
list. Its JSON column is a `LONGTEXT` holding the client's bytes, so metadata measures as sent
rather than in a canonical re-rendering. Postgres/MySQL integration tests in
`postgres_test.go`/`mysql_test.go` skip unless
`HIPPOCAMPUS_TEST_POSTGRES_DSN`/`HIPPOCAMPUS_TEST_MYSQL_DSN` point at a disposable database. A
covering index over the memories consolidation columns lets the sleep-cycle scans
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

**Encryption at rest** (`encrypt.go`, the `seal` package, `storage.encryption.*`, off by default;
TODO-3 item 195) sits at the compression boundary and in the same order: compress, then seal on the
way in; open, then decompress on the way out, so ciphertext, which does not compress, never reaches
gzip, and nothing above the package sees anything but plain bodies. The decision is recorded per
row, as compression's is: `memories.body_key_id` (empty = plaintext) names the key, and reads
follow the row, so enabling, disabling and rotating are all safe on a live store and a mixed store
reads uniformly. The key id is **derived from the key** (an HMAC of it, 8 hex characters) rather
than configured beside it, so it cannot be wrong: a ring given different material for a key reports
the row's key as not configured, which says what is the matter, rather than as an authentication
failure, which does not. Each value is sealed under its own key, derived by HKDF from the configured
key and a 16-byte random salt, which removes the AES-GCM random-nonce budget from the question at 33
bytes a row. That is deliberately not envelope encryption in the KMS sense (a random data key
wrapped by the configured one): wrapping buys nothing while the key-encryption key lives in the
process. Its real value, a key-encryption key that never leaves a KMS, is `storage.encryption.kms.*`
(TODO-3 item 205; below). A body's additional data is its memory id, so a sealed body moved onto another row by
someone who can write the table but lacks the key fails to open. The column is a **core column**
(migration 2), not migration 18's, for a reason that broke the first attempt: migrations 12 and 14
populate the content index by reading bodies through the scanners, which now select
`body_key_id`, so on an upgraded store the column has to exist before they run. Migration 18 adds
`callback_queue.payload_key_id` (queued payloads carry bodies under `callbacks.includeBodies`) and,
more importantly, bumps the version, because an older build would otherwise read a sealed body as
the body itself and hand ciphertext to every caller with no error anywhere. A payload sealed under a
key the ring does not hold stays queued rather than being abandoned like an undecodable one, because
the remedy is configuration. What is **not** sealed is everything a query reads; metadata was the
close call, and stays plaintext because every dialect filters on it inside the database. The store's
own content index is a plaintext copy of every body, so `contentIndexSetting` drops it under
encryption and `encryptionProblems` refuses an explicit `search.contentIndex.enabled: true` beside
it. Export archives are sealed as a stream (`seal/stream.go`: 64 KiB GCM segments whose nonce is the
segment's position plus a final flag, so truncation at a segment boundary is detected) beneath the
archive's own gzip, and `Import` tells a sealed archive from a gzip one by its first bytes. Rows
written before encryption, or under a retired key, are moved by `--reencrypt` (`ReencryptBodies`),
which rewrites a row only if it still holds exactly the bytes it read, so it is safe beside a live
instance on a server dialect.

**The key ledger and the KMS** (`db/keyledger.go`, `seal/kms`, `openKeyring` in
`cmd/hippocampus/encryption.go`; TODO-3 item 205). A per-row key id says which key a row needs only
when that row is read, so a lost retired key used to surface weeks after the deploy that lost it.
`encryption_keys` (migration 19) holds one row per key ever sealed under, not per memory. Every
instance records its active key before sealing with it, and every open checks its ring against the
ledger, refusing under `storage.encryption.ledgerCheck: refuse` (the default) or warning under
`warn`. `--reencrypt` ends with `DrainKeys`, which marks drained every key no row is under. It finds
those keys with a scan of both sealed columns rather than trusting the run's own count, because a
live replica on an older configuration may have written under the old key meanwhile. Recording an
active key undrains it. The migration seeds the ledger from the same scan, which has no index to
stay on and so must run once. It therefore needs a completion trace (schema.go requires one). The
ledger is built as `encryption_keys_build` and renamed into place, so an interrupted seeding leaves
no ledger and is redone. A ledger that existed but had missed a key would pass a ring that cannot
read the store, which is worse than having none. The table probe is a no-row SELECT, the same probe
`checkReadOnlyTables` uses, so no new dialect knowledge is needed. With a KMS, the same table holds
the store's data keys: one random 32-byte key, wrapped under `kms.keyId` and bound to its own id as
additional data, is the active key, and the KMS unwraps it at every open. The ring is otherwise
unchanged: a data key is just another key, and `seal` never learns where it came from. Two rules
carry the design. (1) A data key under the configured key-encryption key that will not unwrap stops
a sealing start. Generating a fresh key instead would mint one more data key per KMS outage, each
needed forever after. (2) Two replicas that both find no data key each generate one, then re-read
the ledger and take the oldest, so they converge on one key; the loser's key stays in the ledger,
held and unused. The providers are AWS KMS through the SDK the S3 store already brings, and Google
Cloud KMS and Vault transit over their REST APIs. Cloud KMS's client library would bring a gRPC
dependency tree larger than the rest of the service's, all for two calls.

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
