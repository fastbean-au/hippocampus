# Stored-schema upgrade fixtures

One SQLite store per distinct **released** schema, used by `db/schema_upgrade_test.go` to prove that
a database written by an older version of the service opens on HEAD with its rows still readable,
filterable and consolidatable.

That is the fourth of the four promises in [`CHANGELOG.md`](../../../CHANGELOG.md)'s Compatibility
section, and it was the only one with no mechanical guard behind it: the contract has `buf
breaking`, the config keys have `configkeys_test.go`, the archive format has its own versioned
header, and this had a sentence.

## Which tags, and why these

Every tag between two entries below writes a byte-identical schema, so one fixture covers the band.
Each is named by the **last** tag that wrote it — the version somebody would actually be upgrading
from.

| Fixture   | What `initSchema` must do to it that it need not do to the next one                                                    |
| --------- | ---------------------------------------------------------------------------------------------------------------------- |
| `v0.4.0`  | `migrateSignificanceToLevels` — the one migration that moves **data** — plus `significance_level_id` on both tables and the covering index rebuilt onto it |
| `v0.22.0` | `memories.is_compressed`                                                                                                 |
| `v0.23.0` | `initContentSearch` — the FTS index created **and backfilled** over a non-empty store                                    |
| `v0.25.0` | `initLinkTables`, `dropLegacyRelationshipColumns`, `link_significance` and `metadata` on both tables                      |
| `v0.31.0` | `initTombstones`                                                                                                         |
| `v0.34.0` | nothing — the control, and the only fixture whose migration should be a no-op                                            |

## How they are made

By [`scripts/schema-fixtures.sh`](../../../scripts/schema-fixtures.sh), which checks out the tag,
**builds the released binary, runs it, and seeds it over its own HTTP gateway**.

They are never hand-written. A hand-written `CREATE TABLE` of an old schema is a second copy of the
schema, and it would drift exactly as every other copy in this repo has — at which point the test
would be asserting that HEAD can migrate a schema no release ever produced.

Each directory carries a `SOURCE` file recording the tag, the commit and when it was generated.

## Regenerating

Only when a new release changes the schema, and then only for the new fixture:

```sh
scripts/schema-fixtures.sh v0.35.0
```

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

Opening a fixture **migrates it in place**, so the test copies each into a temp directory first.
Never point a service at one of these files.
