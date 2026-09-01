# Stored-schema upgrade fixtures

One fixture per distinct **released** schema, per driver, used by `db/schema_upgrade_test.go` and
`db/schema_upgrade_server_test.go` to prove that a database written by an older version of the
service opens on HEAD with its rows still readable, filterable and consolidatable.

**There are three schema-init functions, not one** — `initSchema` (SQLite), `initPostgresSchema` and
`initMySQLSchema` — each with its own migration list. A fixture set built for SQLite alone leaves
two of the three unguarded, which is how `initInstances` (server-only) and the whole of Postgres's
native `ADD COLUMN IF NOT EXISTS` path came to have nothing behind them. So each tag carries three
artefacts:

| Artefact         | Driver     | Form                                  |
| ---------------- | ---------- | ------------------------------------- |
| `hippocampus.db` | SQLite     | the database file itself              |
| `postgres.sql`   | PostgreSQL | a `pg_dump`, one statement per line   |
| `mysql.sql`      | MySQL      | a `mysqldump`, one statement per line |

That is the fourth of the four promises in [`CHANGELOG.md`](../../../CHANGELOG.md)'s Compatibility
section, and it was the only one with no mechanical guard behind it: the contract has `buf
breaking`, the config keys have `configkeys_test.go`, the archive format has its own versioned
header, and this had a sentence.

## Which tags, and why these

Every tag between two entries below writes a byte-identical schema, so one fixture covers the band.
Each is named by the **last** tag that wrote it — the version somebody would actually be upgrading
from.

| Fixture   | What `initSchema` must do to it that it need not do to the next one                                                                                        |
| --------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `v0.4.0`  | `migrateSignificanceToLevels` — the one migration that moves **data** — plus `significance_level_id` on both tables and the covering index rebuilt onto it |
| `v0.22.0` | `memories.is_compressed`                                                                                                                                   |
| `v0.23.0` | `initContentSearch` — the FTS index created **and backfilled** over a non-empty store                                                                      |
| `v0.25.0` | `initLinkTables`, `dropLegacyRelationshipColumns`, `link_significance` and `metadata` on both tables                                                       |
| `v0.31.0` | `initTombstones`                                                                                                                                           |
| `v0.34.0` | nothing of its own — it shares `v0.37.0`'s schema, and is kept as the older end of that band                                                               |
| `v0.37.0` | `search_outbox`                                                                                                                                            |
| `v0.38.3` | `schema_migrations` — the schema ledger itself                                                                                                             |

`v0.38.3` is the most valuable fixture in the set, and the reason is worth stating: it is the last
release that recorded no schema version at all, so migrating it is the upgrade **every deployment in
the field will actually perform**. The others each prove one historical migration; this one proves
the transition every real store is about to make.

There is also no "control" fixture whose migration is a no-op, and there cannot be a durable one:
the newest fixture is by construction the release before the one being cut, so as soon as a release
changes the schema the previous control stops being one. What replaces it is the guard in
`db/schema_upgrade_test.go`, which requires every migration to name a fixture predating it.

## How they are made

By [`scripts/schema-fixtures.sh`](../../../scripts/schema-fixtures.sh), which checks out the tag,
**builds the released binary, runs it, and seeds it over its own HTTP gateway** — once per driver,
against a scratch database for the server ones.

The dump tools live in the test containers rather than on the host, so the script shells into them
with `podman exec`. **The script's default container names are one machine's and will not match
yours** — run `podman ps` and override what differs via the `FIXTURE_*` environment variables. **That container
dependency is deliberately in the generator and not in the test**: because the dumps are normalised
to one statement per line, replaying one needs no client binary and no SQL parser — just a DSN and a
loop — so the tests run unchanged against CI's existing service containers.

They are never hand-written. A hand-written `CREATE TABLE` of an old schema is a second copy of the
schema, and it would drift exactly as every other copy in this repo has — at which point the test
would be asserting that HEAD can migrate a schema no release ever produced.

Each directory carries a `SOURCE` file recording the tag, the commit and when it was generated.

## Regenerating

Only when a new release changes the schema, and then only for the new fixture:

```sh
scripts/schema-fixtures.sh --driver all v0.35.0
```

The server drivers need `HIPPOCAMPUS_TEST_POSTGRES_DSN` / `HIPPOCAMPUS_TEST_MYSQL_DSN` to run their
half of the tests, and a credential that may `CREATE DATABASE` — the tests build a scratch database
per subtest. Where the configured user cannot (CI's MySQL user is scoped to one database),
`HIPPOCAMPUS_TEST_MYSQL_ADMIN_DSN` supplies one that can. A configured server that cannot create the
scratch database **fails** rather than skipping, so this guard cannot quietly stop running.

`TestEverySchemaMigrationHasAFixture` reads the migrations out of `initSchema`'s source and fails
when one is undeclared, so a migration added without a fixture cannot ship quietly.

## What the tests assert, and what they deliberately do not

They assert **behaviour** — that the seeded rows read back with the values they were written with,
that a metadata-filtered query returns an empty page rather than an error, that content search finds
a backfilled body, and that all three consolidation passes still walk every row.

They deliberately do not assert that a column exists. That proves nothing the migration's own `ALTER
TABLE` does not already say, and the one migration defect this repo has actually found — `metadata`
defaulting to `''` rather than NULL, where SQLite's `json_extract` raises "malformed JSON" on the
empty string — would have passed a column check. Reintroducing that defect fails four of these six
fixtures.

Opening a SQLite fixture **migrates it in place**, so the test copies each into a temp directory
first. Never point a service at one of these files. The server fixtures have the same hazard in a
different shape: each is replayed into a scratch database that is dropped when the subtest ends.

Two dialect-specific notes. `v0.23.0` and `v0.25.0` are identical on the server drivers, because
what separates them is the FTS content index, which is SQLite-only; both are kept so the tag set is
the same across all three. And no released MySQL schema predates
`setMySQLColumnCollationIfNeeded` — the binary collation was pinned before `v0.1.0` — so no fixture
can drive that migration. What the fixtures pin instead is the property it exists to guarantee, on
every released schema: `TestSchemaUpgradeMySQLIdsStayCaseSensitive` stores two ids differing only in
case and requires two rows, which under the server's case-insensitive default reports as
`Duplicate entry`.
