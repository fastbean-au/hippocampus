package db

import (
	"context"
	"os"
	"regexp"
	"strings"
	"testing"
)

// dialectEnv names the environment variable that selects which SQL dialect the package's shared
// test suite runs against. Unset (the default) means SQLite, so an ordinary `go test ./db` is
// exactly what it always was: an in-memory database, no server, no skips.
//
// Setting it to `postgres` or `mysql` re-points newTestDB at the corresponding server and reruns
// the SAME ~190 tests there. That is the point of the variable. Before it existed, the two server
// drivers were covered only by the hand-picked tests in postgres_test.go and mysql_test.go - 18 of
// the 74 db.Store methods - which left dialect-specific code in the link graph, the forgotten log,
// the search outbox, PreviewConsolidation and RetainedStats with no test that had ever executed the
// branch. The dialect divergences are exactly the places a shared test is most valuable, and the
// shared suite already describes the behaviour all three must agree on.
//
// The driver-named suites remain, and are not redundant: they cover what is genuinely per-dialect
// (instance locking, the collation migration, UsedBytes accounting) rather than what must agree.
const dialectEnv = "HIPPOCAMPUS_TEST_DIALECT"

// testDialect resolves dialectEnv to a driver. An unrecognised value fails rather than silently
// falling back to SQLite, which would report a green run against a dialect that was never touched.
func testDialect(t testing.TB) driver {
	t.Helper()

	switch strings.ToLower(strings.TrimSpace(os.Getenv(dialectEnv))) {

	case "", "sqlite", "sqlite3":
		return driverSQLite

	case "postgres", "postgresql", "pgx":
		return driverPostgres

	case "mysql":
		return driverMySQL

	}

	t.Fatalf("%s=%q is not a known dialect (want sqlite, postgres or mysql)", dialectEnv, os.Getenv(dialectEnv))

	return driverSQLite
}

// newTestDB opens a store on the dialect selected by dialectEnv, empty, closed when the test ends.
//
// The three arms differ in how "empty" is reached, and that difference is the harness's one real
// constraint. SQLite gets a brand-new in-memory database per call, so isolation is free and two
// calls in one test are two independent stores. The server drivers share one database, so the
// store is emptied on entry instead (resetTestStore, not Purge - Purge compacts, and on the server
// drivers it also leaves the instance registry standing). A test that needs two INDEPENDENT stores
// therefore cannot run against a server dialect and must say so with requireSQLite.
//
// Server opens are replicas (consolidate false) and so take no instance lock: the lock coordinates
// consolidating instances, every Store method works without it, and holding it for the length of a
// suite would starve any other package testing against the same server.
func newTestDB(t *testing.T) *DB {
	t.Helper()

	switch testDialect(t) {

	case driverPostgres:
		return newSharedServerTestDB(t, postgresTestDSNEnv, func(dsn string) (*DB, error) {
			return NewPostgres(dsn, false)
		})

	case driverMySQL:
		return newSharedServerTestDB(t, mysqlTestDSNEnv, func(dsn string) (*DB, error) {
			return NewMySQL(dsn, false)
		})

	}

	database, err := New("")
	if err != nil {
		t.Fatalf("failed to create test DB: %s", err)
	}

	t.Cleanup(func() {
		_ = database.Close()
	})

	return database
}

// newSharedServerTestDB is the common body of the two server arms: read the DSN, open, empty, and
// close on cleanup. It skips rather than fails when the DSN is unset, so `HIPPOCAMPUS_TEST_DIALECT=
// postgres go test ./db` without a server reports skips rather than a wall of failures.
func newSharedServerTestDB(t *testing.T, dsnEnv string, open func(string) (*DB, error)) *DB {
	t.Helper()

	dsn := os.Getenv(dsnEnv)
	if dsn == "" {
		t.Skipf("set %s to run the shared suite against this dialect", dsnEnv)
	}

	database, err := open(dsn)
	if err != nil {
		t.Fatalf("opening the test database: %s", err)
	}

	t.Cleanup(func() {
		_ = database.Close()
	})

	resetTestStore(t, database)

	return database
}

// resetTestStore empties every table the schema creates, in an order that respects nothing but
// tidiness (there are no foreign keys; the link tables are emptied first only because that is the
// order Purge uses).
//
// It is deliberately not Purge. Purge compacts afterwards, which on SQLite is a VACUUM per test,
// and it leaves the instance registry populated - a row from a previous run would make
// ListInstances non-deterministic. It also resets the surrogate sequences on the two tables that
// have one, so a test asserting on seq ordering starts from a known point rather than wherever the
// previous test left off.
func resetTestStore(t *testing.T, d *DB) {
	t.Helper()

	ctx := context.Background()

	tables := []string{
		memoryLinksTable,
		eventLinksTable,
		"memories",
		"events",
		significanceLevelsTable,
		tombstonesTable,
		searchOutboxTable,
		callbackQueueTable,
	}

	if d.instanceTable {
		tables = append(tables, instancesTable)
	}

	for _, table := range tables {
		if _, err := d.exec(ctx, `DELETE FROM `+table); err != nil {
			t.Fatalf("emptying %s: %s", table, err)
		}
	}

	resetTestSequences(t, d)
}

// resetTestSequences returns the AUTO_INCREMENT / SERIAL counters on the tables carrying a
// surrogate key to their starting value, so seq-ordering assertions do not depend on how many rows
// earlier tests inserted. SQLite's in-memory database is new each time and needs nothing.
func resetTestSequences(t *testing.T, d *DB) {
	t.Helper()

	ctx := context.Background()

	var statements []string

	switch d.driver {

	case driverPostgres:
		// A no-op when the table is empty on the next insert anyway, but pg_get_serial_sequence
		// returns NULL for a table without one, and setval(NULL, ...) is an error - so both are
		// guarded by the column actually being serial, which both of these are.
		statements = []string{
			`SELECT setval(pg_get_serial_sequence('` + tombstonesTable + `', 'seq'), 1, false)`,
			`SELECT setval(pg_get_serial_sequence('` + searchOutboxTable + `', 'seq'), 1, false)`,
			`SELECT setval(pg_get_serial_sequence('` + callbackQueueTable + `', 'seq'), 1, false)`,
		}

	case driverMySQL:
		statements = []string{
			`ALTER TABLE ` + tombstonesTable + ` AUTO_INCREMENT = 1`,
			`ALTER TABLE ` + searchOutboxTable + ` AUTO_INCREMENT = 1`,
			`ALTER TABLE ` + callbackQueueTable + ` AUTO_INCREMENT = 1`,
		}

	default:
		return

	}

	for _, statement := range statements {
		if _, err := d.exec(ctx, statement); err != nil {
			t.Fatalf("resetting sequences: %s", err)
		}
	}
}

// requireSQLite skips a test that is legitimately SQLite-only rather than dialect-agnostic: the
// FTS5 content search, the WAL file, the storage-directory lock, Preserve's compaction, and
// UsedBytes' page accounting all describe things the server drivers deliberately do differently or
// not at all. Anything else failing under a server dialect is a bug, not a candidate for this.
func requireSQLite(t *testing.T) {
	t.Helper()

	if testDialect(t) != driverSQLite {
		t.Skipf("SQLite-only: not meaningful under %s", os.Getenv(dialectEnv))
	}
}

// sqliteOnlyTestFiles names the test files allowed to open a store directly rather than through
// newTestDB. Every one of them is about SQLite's own construction and has no server-driver
// counterpart to compare against:
//
//   - bench_test.go pins the cost of the consolidation scans against one storage engine; a
//     benchmark whose number moved with the dialect would measure nothing.
//   - conformance_test.go is newTestDB itself.
//   - instances_test.go asserts SQLite does NOT keep the instance registry, which is the point.
//   - listing_index_test.go reads EXPLAIN QUERY PLAN, a SQLite statement.
//   - lock_test.go is the storage-directory file lock, which only the embedded driver takes.
//   - schema_upgrade_test.go builds old-schema SQLite files; the server dialects' equivalent is
//     schema_upgrade_server_test.go, which builds its own scratch databases.
//
// Anything else opening its own store is opting out of two thirds of the coverage without saying
// so, which is the state this whole harness exists to end.
var sqliteOnlyTestFiles = map[string]bool{
	"bench_test.go":          true,
	"conformance_test.go":    true,
	"instances_test.go":      true,
	"listing_index_test.go":  true,
	"lock_test.go":           true,
	"schema_upgrade_test.go": true,
}

// TestSharedSuiteOpensThroughNewTestDB is the drift guard on the harness: a test opening an
// in-memory SQLite store directly gets no Postgres or MySQL coverage, and nothing about writing it
// that way looks wrong at the call site. The allow-list above is the place to record a deliberate
// exception, with the reason.
func TestSharedSuiteOpensThroughNewTestDB(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading the package directory: %s", err)
	}

	for _, entry := range entries {
		name := entry.Name()

		if !strings.HasSuffix(name, "_test.go") || sqliteOnlyTestFiles[name] {
			continue
		}

		source, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("reading %s: %s", name, err)
		}

		// New("") is the in-memory open; New(dir) is a file-backed one, which is equally SQLite-only
		// but is how the schema-upgrade and read-only fixtures legitimately build a store from
		// scratch, so only the unambiguous form is refused.
		if strings.Contains(string(source), `New("")`) {
			t.Errorf(
				`%s opens an in-memory SQLite store directly; use newTestDB so the test runs under `+
					`%s too, or add the file to sqliteOnlyTestFiles with a reason`,
				name, dialectEnv,
			)
		}
	}
}

// TestDialectKnowledgeIsConfined is the drift guard on dialect.go's boundary: no file outside the
// declared dialect files may ask which dialect is active.
//
// It is the property that makes a fourth dialect tractable. Before the dialect table existed the
// same question was asked at fifty-odd sites across thirteen files, so adding one meant finding all
// fifty - and missing one produced not a compile error but a store that ran and was subtly wrong on
// exactly one backend. A shared fragment or capability read from the table cannot be forgotten,
// because there is only one of it.
//
// A new branch that genuinely cannot be expressed as a fragment or a capability belongs in
// dialect.go beside the others, not at its call site.
func TestDialectKnowledgeIsConfined(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading the package directory: %s", err)
	}

	// A comparison of the driver field, in either direction, and the switch over it. Assignment
	// (driver: driverPostgres, in the constructors) is deliberately not matched: setting the field
	// is not knowing what it is.
	comparison := regexp.MustCompile(`(?:\w+\.driver\s*[!=]=|switch\s+\w+\.driver\b)`)

	for _, entry := range entries {
		name := entry.Name()

		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || dialectFiles[name] {
			continue
		}

		source, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("reading %s: %s", name, err)
		}

		for i, line := range strings.Split(string(source), "\n") {
			trimmed := strings.TrimSpace(line)

			if strings.HasPrefix(trimmed, "//") || !comparison.MatchString(line) {
				continue
			}

			t.Errorf(
				"%s:%d asks which dialect is active: %q\n"+
					"Read a fragment or a capability from the dialect table instead, or - if the "+
					"difference is genuinely structural - put the branch in dialect.go beside the "+
					"upsert builder and the index helpers.",
				name, i+1, trimmed,
			)
		}
	}
}
