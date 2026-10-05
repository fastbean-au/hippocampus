// Package dbtest opens a store for ANOTHER package's tests, on the dialect HIPPOCAMPUS_TEST_DIALECT
// selects - the same variable that reruns the db package's own suite on a server dialect.
//
// It exists because the db suite is not the only place dialect-specific behaviour can break. The
// service layer composes storage calls - scope predicates, predicate deletion in batches, the event
// cascades, the import collision check - and a composition can be wrong on one dialect while every
// storage method it calls is right on all three. Before this, every service-level test ran on
// SQLite alone (TODO-3 item 153).
//
// Every Open on a server dialect gets a store of its own: a fresh schema on PostgreSQL, a fresh
// database on MySQL, dropped when the test ends. The db suite instead shares one database and empties
// it between tests, but that needs a "delete everything" operation, and exporting one from the db
// package to make it reachable from here would put it on the production type. Isolation also means a
// test that opens two stores - a transfer's source and target - works on every dialect, which the
// shared-database approach cannot offer.
//
// The cost is a schema initialisation per Open, which is why only the service layer's main
// constructor uses this rather than every test that opens a store.
package dbtest

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"

	"github.com/fastbean-au/hippocampus/db"
)

const (
	// DialectEnv selects the dialect, exactly as for the db package's own suite.
	DialectEnv = "HIPPOCAMPUS_TEST_DIALECT"

	// PostgresDSNEnv names a PostgreSQL database the test user may create schemas in.
	PostgresDSNEnv = "HIPPOCAMPUS_TEST_POSTGRES_DSN"

	// MySQLAdminDSNEnv names a MySQL credential that may create and drop databases. The scoped test
	// user CI gives the db suite cannot, which is why this reads the admin DSN rather than that one.
	MySQLAdminDSNEnv = "HIPPOCAMPUS_TEST_MYSQL_ADMIN_DSN"
)

// Open returns an empty store on the selected dialect, closed and discarded when the test ends. A
// server dialect whose DSN is unset is a skip rather than a failure, so selecting one without a
// server reports skips instead of a wall of failures.
func Open(t testing.TB) *db.DB {
	t.Helper()

	switch dialect(t) {

	case "postgres":
		return openPostgres(t)

	case "mysql":
		return openMySQL(t)

	}

	store, err := db.New("")
	if err != nil {
		t.Fatalf("opening an in-memory store: %s", err)
	}

	t.Cleanup(func() { _ = store.Close() })

	return store
}

// dialect resolves DialectEnv. An unrecognised value fails rather than falling back to SQLite, which
// would report a green run against a dialect that was never touched.
func dialect(t testing.TB) string {
	t.Helper()

	switch strings.ToLower(strings.TrimSpace(os.Getenv(DialectEnv))) {

	case "", "sqlite", "sqlite3":
		return "sqlite"

	case "postgres", "postgresql", "pgx":
		return "postgres"

	case "mysql":
		return "mysql"

	}

	t.Fatalf("%s=%q is not a known dialect (want sqlite, postgres or mysql)", DialectEnv, os.Getenv(DialectEnv))

	return ""
}

// scratchName is a schema or database name no other test will choose.
func scratchName(t testing.TB) string {
	t.Helper()

	suffix := make([]byte, 6)

	if _, err := rand.Read(suffix); err != nil {
		t.Fatalf("naming a scratch store: %s", err)
	}

	return "hippo_t_" + hex.EncodeToString(suffix)
}

func openPostgres(t testing.TB) *db.DB {
	t.Helper()

	dsn := os.Getenv(PostgresDSNEnv)
	if dsn == "" {
		t.Skipf("set %s to run this test against PostgreSQL", PostgresDSNEnv)
	}

	parsed, err := url.Parse(dsn)
	if err != nil || parsed.Scheme == "" {
		t.Fatalf("%s must be a URL (postgres://...) so a search_path can be added to it", PostgresDSNEnv)
	}

	schema := scratchName(t)

	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("connecting to PostgreSQL: %s", err)
	}

	t.Cleanup(func() { _ = admin.Close() })

	if _, err := admin.ExecContext(context.Background(), `CREATE SCHEMA `+schema); err != nil {
		t.Fatalf("creating scratch schema %s: %s", schema, err)
	}

	// Registered before the store's own cleanup, so it runs after it: cleanups run last-in first-out,
	// and the schema can only be dropped once the store has let go of it.
	t.Cleanup(func() {
		_, _ = admin.ExecContext(context.Background(), `DROP SCHEMA `+schema+` CASCADE`)
	})

	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()

	store, err := db.NewPostgres(parsed.String(), false)
	if err != nil {
		t.Fatalf("opening a store in schema %s: %s", schema, err)
	}

	t.Cleanup(func() { _ = store.Close() })

	return store
}

func openMySQL(t testing.TB) *db.DB {
	t.Helper()

	dsn := os.Getenv(MySQLAdminDSNEnv)
	if dsn == "" {
		t.Skipf("set %s to run this test against MySQL", MySQLAdminDSNEnv)
	}

	config, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatalf("parsing %s: %s", MySQLAdminDSNEnv, err)
	}

	database := scratchName(t)

	admin, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("connecting to MySQL: %s", err)
	}

	t.Cleanup(func() { _ = admin.Close() })

	if _, err := admin.ExecContext(context.Background(), `CREATE DATABASE `+database); err != nil {
		t.Fatalf("creating scratch database %s: %s", database, err)
	}

	// Before the store's cleanup, so it runs after it (see openPostgres).
	t.Cleanup(func() {
		_, _ = admin.ExecContext(context.Background(), `DROP DATABASE `+database)
	})

	config.DBName = database

	store, err := db.NewMySQL(config.FormatDSN(), false)
	if err != nil {
		t.Fatalf("opening a store in database %s: %s", database, err)
	}

	t.Cleanup(func() { _ = store.Close() })

	return store
}
