package db

import (
	"bufio"
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fastbean-au/hippocampus/types"

	mysqldriver "github.com/go-sql-driver/mysql"
)

// The server drivers' half of the stored-schema upgrade guard (TODO item 78). See
// schema_upgrade_test.go for why this exists at all; what is different here is the fixture format
// and, more importantly, that THERE ARE THREE SCHEMA-INIT FUNCTIONS, not one. `initSchema`,
// `initPostgresSchema` and `initMySQLSchema` each carry their own migration list, so a fixture set
// for SQLite alone leaves two of the three unguarded — including `initInstances`, which exists only
// on the server drivers, and Postgres's native ADD COLUMN IF NOT EXISTS, which shares no code with
// the probe the other two use.
//
// A server fixture cannot be a database file, so it is a SQL dump taken from the server itself with
// pg_dump/mysqldump and normalised by scripts/schema-fixtures.sh to ONE STATEMENT PER LINE. That
// normalisation is what keeps the container dependency in the generator: replaying a fixture needs
// no client binary and no SQL parser, just a DSN and a loop.
//
// Both skip unless HIPPOCAMPUS_TEST_POSTGRES_DSN / HIPPOCAMPUS_TEST_MYSQL_DSN name a disposable
// server, exactly as postgres_test.go and mysql_test.go already do. CI sets both.

const (
	postgresTestAdminDSNEnv = "HIPPOCAMPUS_TEST_POSTGRES_ADMIN_DSN"
	mysqlTestAdminDSNEnv    = "HIPPOCAMPUS_TEST_MYSQL_ADMIN_DSN"
)

// TestSchemaUpgradePostgres replays each released Postgres schema into a scratch database and runs
// the same assertions the SQLite fixtures do.
func TestSchemaUpgradePostgres(t *testing.T) {
	for _, fixture := range schemaFixtures {
		t.Run(fixture.tag, func(t *testing.T) {
			database := restoreServerFixture(t, driverPostgres, fixture.tag)

			assertSeededRowsReadBack(t, database, fixture.tag, fixture.migrations)
			assertMetadataFilterIsSafe(t, database, fixture.tag, fixture.migrations)
			assertStoreIsConsolidatable(t, database, fixture.tag, fixture.migrations)
		})
	}
}

// TestSchemaUpgradeMySQL is TestSchemaUpgradePostgres's counterpart. MySQL is the driver with the
// most to migrate — it is the only one that probes information_schema AND rewrites column
// collations in place — so it is also the one where a fixture predating a migration is worth most.
func TestSchemaUpgradeMySQL(t *testing.T) {
	for _, fixture := range schemaFixtures {
		t.Run(fixture.tag, func(t *testing.T) {
			database := restoreServerFixture(t, driverMySQL, fixture.tag)

			assertSeededRowsReadBack(t, database, fixture.tag, fixture.migrations)
			assertMetadataFilterIsSafe(t, database, fixture.tag, fixture.migrations)
			assertStoreIsConsolidatable(t, database, fixture.tag, fixture.migrations)
		})
	}
}

// TestSchemaUpgradeMySQLCollation is MySQL's own, because the collation is not a column the other
// drivers also have — it is the property that makes ids compare byte-for-byte on MySQL as they
// already do on SQLite and Postgres. Without it `MEM-1` and `mem-1` collide under the server's
// case-insensitive default, which is a data-corrupting difference rather than a cosmetic one.
//
// setMySQLColumnCollationIfNeeded migrates a database created before it was pinned. Every RELEASED
// schema already carries the binary collation (it landed before v0.1.0), so no fixture here can
// exercise the migration itself — what these fixtures pin is the property it exists to guarantee,
// on every released schema, which is the part that must never regress.
func TestSchemaUpgradeMySQLCollation(t *testing.T) {
	for _, fixture := range schemaFixtures {
		t.Run(fixture.tag, func(t *testing.T) {
			database := restoreServerFixture(t, driverMySQL, fixture.tag)

			for _, column := range []struct{ table, column string }{
				{"memories", "id"},
				{"memories", "event_id"},
				{"memories", "group_name"},
				{"events", "id"},
				{"events", "group_name"},
			} {
				var collation string

				err := database.sql.QueryRow(
					`SELECT COLLATION_NAME FROM information_schema.columns
					 WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? AND COLUMN_NAME = ?`,
					column.table, column.column,
				).Scan(&collation)
				if err != nil {
					t.Fatalf("failed to read %s.%s's collation on a migrated %s store: %s",
						column.table, column.column, fixture.tag, err)
				}

				// The LITERAL name, not mysqlBinaryCollation. Comparing against the constant the
				// schema is built from is self-referential — it would pass just as happily if the
				// constant were changed to the case-insensitive server default, which is exactly
				// the regression worth catching.
				if collation != "utf8mb4_bin" {
					t.Errorf("%s.%s collates as %s after migrating %s, want utf8mb4_bin — ids would "+
						"compare case-insensitively here and byte-for-byte on the other two drivers",
						column.table, column.column, collation, fixture.tag)
				}
			}
		})
	}
}

// TestSchemaUpgradeMySQLIdsStayCaseSensitive asserts the PROPERTY the collation exists for, rather
// than the collation's name: two ids differing only in case must be two memories on a migrated
// store, as they already are on SQLite and Postgres. This is the half of the collation check that
// survives the constant being renamed, and it is what a user would actually notice — under MySQL's
// case-insensitive default the second write silently updates the first.
func TestSchemaUpgradeMySQLIdsStayCaseSensitive(t *testing.T) {
	ctx := context.Background()

	for _, fixture := range schemaFixtures {
		t.Run(fixture.tag, func(t *testing.T) {
			database := restoreServerFixture(t, driverMySQL, fixture.tag)

			// mem-loose-2 is seeded; its upper-case twin is not, so storing it must create a row
			// rather than collide with the existing one.
			twin := types.Memory{
				Id:           "MEM-LOOSE-2",
				TimeStamp:    seededBaseTimestamp,
				Significance: 10,
				Body:         "an id differing from a seeded one only in case",
			}

			if _, err := database.CreateMemory(ctx, twin); err != nil {
				t.Fatalf("failed to store a case-variant id on a migrated %s store: %s", fixture.tag, err)
			}

			memories, err := database.GetMemoriesByIds(ctx, []string{"mem-loose-2", "MEM-LOOSE-2"})
			if err != nil {
				t.Fatalf("failed to read back both cases on a migrated %s store: %s", fixture.tag, err)
			}

			if len(*memories) != 2 {
				t.Errorf("a migrated %s store holds %d of the two ids differing only in case, want 2 "+
					"— they are colliding, so one write silently overwrote the other",
					fixture.tag, len(*memories))
			}
		})
	}
}

// restoreServerFixture creates a scratch database, replays the fixture into it, and opens it with
// the driver under test. The scratch database is dropped when the test ends.
//
// A scratch database per subtest rather than reusing the configured one is not tidiness: the
// fixture's own CREATE TABLEs would collide with whatever the configured database already holds,
// and dropping that database is not this test's business.
func restoreServerFixture(t *testing.T, driver driver, tag string) *DB {
	t.Helper()

	var (
		environment      string
		adminEnvironment string
		fixtureFile      string
		label            string
	)

	switch driver {

	case driverPostgres:
		environment, adminEnvironment = postgresTestDSNEnv, postgresTestAdminDSNEnv
		fixtureFile, label = "postgres.sql", "postgres"

	case driverMySQL:
		environment, adminEnvironment = mysqlTestDSNEnv, mysqlTestAdminDSNEnv
		fixtureFile, label = "mysql.sql", "mysql"

	default:
		t.Fatalf("restoreServerFixture called for driver %d, which is not a server driver", driver)
	}

	dsn := os.Getenv(environment)
	if dsn == "" {
		t.Skipf("set %s to run the %s schema-upgrade tests", environment, label)
	}

	// Creating a scratch database needs a privilege the configured user may not have — CI's MySQL
	// user is scoped to one database, deliberately. The admin variable supplies a credential that
	// can, falling back to the configured one where it is already enough (a Postgres POSTGRES_USER
	// is a superuser, so that fallback is the normal case there).
	//
	// A missing privilege FAILS rather than skips: an unset base DSN means "not configured for
	// server tests", but a configured server that cannot create the scratch database means this
	// guard would silently stop running, which is the failure it exists to prevent elsewhere.
	adminCredential := os.Getenv(adminEnvironment)
	if adminCredential == "" {
		adminCredential = dsn
	}

	// Scratch names must be unique per subtest and valid unquoted identifiers on both servers, so
	// the tag's dots become underscores.
	scratch := "hippocampus_upgrade_" + strings.NewReplacer(".", "_", "-", "_").Replace(tag)

	// The scratch database is then used end to end under that same credential — the replay and the
	// driver open both. Creating a database grants nobody else anything on it, so a scoped user
	// pointed at a root-created scratch database is denied its first CREATE TABLE; nothing here is
	// testing privileges, so the credential that made the database is the one that works in it.
	adminDSN, scratchDSN := scratchDSNs(t, driver, adminCredential, scratch)

	admin, err := sql.Open(sqlDriverName(driver), adminDSN)
	if err != nil {
		t.Fatalf("failed to connect to the %s server: %s", label, err)
	}

	defer func() { _ = admin.Close() }()

	// DROP first: a previous run killed mid-test leaves its scratch database behind, and a stale
	// one would be silently reused and would still hold the PREVIOUS fixture's rows.
	if _, err := admin.Exec("DROP DATABASE IF EXISTS " + scratch); err != nil {
		t.Fatalf("failed to drop a leftover scratch database %s: %s\n"+
			"If this is a privilege failure, set %s to a credential that may create and drop "+
			"databases.", scratch, err, adminEnvironment)
	}

	if _, err := admin.Exec("CREATE DATABASE " + scratch); err != nil {
		t.Fatalf("failed to create the scratch database %s: %s\n"+
			"If this is a privilege failure, set %s to a credential that may create databases.",
			scratch, err, adminEnvironment)
	}

	t.Cleanup(func() {
		cleanup, err := sql.Open(sqlDriverName(driver), adminDSN)
		if err != nil {
			t.Logf("failed to reconnect to drop %s: %s", scratch, err)

			return
		}

		defer func() { _ = cleanup.Close() }()

		if _, err := cleanup.Exec("DROP DATABASE IF EXISTS " + scratch); err != nil {
			t.Logf("failed to drop the scratch database %s: %s", scratch, err)
		}
	})

	replayFixture(t, driver, label, scratchDSN, filepath.Join("testdata", "schema", tag, fixtureFile))

	// Open through the real constructor, so this is the migration path a service takes on startup
	// and not some test-only shortcut around it. consolidate: true takes the instance lock, which
	// is scoped to the scratch database and released on Close.
	var database *DB

	switch driver {

	case driverPostgres:
		database, err = NewPostgres(scratchDSN, true)

	case driverMySQL:
		database, err = NewMySQL(scratchDSN, true)

	}

	if err != nil {
		t.Fatalf("failed to open a %s %s store on HEAD: %s", tag, label, err)
	}

	t.Cleanup(func() { _ = database.Close() })

	return database
}

// replayFixture executes the fixture one statement per line. The generator guarantees that shape,
// which is why nothing here parses SQL — a parser in the test would be a second implementation of
// the dialect, and wrong differently from the server.
func replayFixture(t *testing.T, driver driver, label string, dsn string, path string) {
	t.Helper()

	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("failed to read fixture %s (regenerate with scripts/schema-fixtures.sh --driver %s): %s",
			path, label, err)
	}

	defer func() { _ = file.Close() }()

	target, err := sql.Open(sqlDriverName(driver), dsn)
	if err != nil {
		t.Fatalf("failed to connect to the scratch database: %s", err)
	}

	defer func() { _ = target.Close() }()

	scanner := bufio.NewScanner(file)

	// A dumped CREATE TABLE with every column on one line is comfortably longer than the default
	// 64KiB token, and a truncated statement would fail as a syntax error naming nothing useful.
	scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)

	statements := 0

	for scanner.Scan() {
		statement := strings.TrimSpace(scanner.Text())
		if statement == "" || strings.HasPrefix(statement, "--") {
			continue
		}

		if _, err := target.Exec(statement); err != nil {
			t.Fatalf("replaying %s failed at statement %d: %s\n  %.200s",
				path, statements+1, err, statement)
		}

		statements++
	}

	if err := scanner.Err(); err != nil {
		t.Fatalf("failed to read %s: %s", path, err)
	}

	if statements == 0 {
		t.Fatalf("%s carried no statements", path)
	}
}

// scratchDSNs derives the admin DSN (pointing at a database that always exists, since a connection
// is needed before the scratch one is created) and the scratch DSN, from the configured one.
func scratchDSNs(t *testing.T, driver driver, dsn string, scratch string) (string, string) {
	t.Helper()

	switch driver {

	case driverPostgres:
		parsed, err := url.Parse(dsn)
		if err != nil {
			t.Fatalf("failed to parse %s: %s", postgresTestDSNEnv, err)
		}

		admin := *parsed
		admin.Path = "/postgres"

		target := *parsed
		target.Path = "/" + scratch

		return admin.String(), target.String()

	case driverMySQL:
		config, err := mysqldriver.ParseDSN(dsn)
		if err != nil {
			t.Fatalf("failed to parse %s: %s", mysqlTestDSNEnv, err)
		}

		admin := *config
		admin.DBName = ""

		target := *config
		target.DBName = scratch

		return admin.FormatDSN(), target.FormatDSN()

	}

	t.Fatalf("no scratch DSN rule for driver %d", driver)

	return "", ""
}

// sqlDriverName maps a db driver constant onto the database/sql driver the package opens it with,
// so this test connects exactly as postgres.go and mysql.go do.
func sqlDriverName(driver driver) string {
	switch driver {

	case driverPostgres:
		return "pgx"

	case driverMySQL:
		return "mysql"

	}

	return fmt.Sprintf("unknown driver %d", driver)
}
