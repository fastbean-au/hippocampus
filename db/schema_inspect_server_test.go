package db

import (
	"os"
	"testing"
)

// TestInspectSchemaOnServerDialects runs openForInspection's PostgreSQL and MySQL branches (TODO-3
// item 177), which the SQLite-only inspection tests never reach. It opens each store the normal way
// first, so it has been migrated, then reads it back through the inspection path the way
// --schema-version does beside a live instance: the report must name the dialect, record the
// version this build supports, and list nothing as pending.
func TestInspectSchemaOnServerDialects(t *testing.T) {
	for _, tc := range []struct {
		driver string
		env    string
		open   func(t *testing.T) *DB
	}{
		{"postgres", postgresTestDSNEnv, newPostgresTestDB},
		{"mysql", mysqlTestDSNEnv, newMySQLTestDB},
	} {
		t.Run(tc.driver, func(t *testing.T) {
			dsn := os.Getenv(tc.env)
			if dsn == "" {
				t.Skipf("set %s to inspect a %s store", tc.env, tc.driver)
			}

			migrated := tc.open(t)

			report, err := InspectSchema(tc.driver, dsn)
			if err != nil {
				t.Fatalf("InspectSchema: %s", err)
			}

			if report.Dialect != migrated.dialect().name {
				t.Errorf("report names dialect %q, the store is %q", report.Dialect, migrated.dialect().name)
			}

			if report.Version != report.Supported {
				t.Errorf("a store this build migrated records version %d, the build supports %d", report.Version, report.Supported)
			}

			if len(report.Pending) != 0 {
				t.Errorf("a store this build migrated has nothing pending, got %v", report.Pending)
			}
		})
	}
}
