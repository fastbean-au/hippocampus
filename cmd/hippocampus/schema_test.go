package main

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/fastbean-au/hippocampus/db"
)

// TestPrintSchemaReport covers what the four report shapes render as. The rendering is the whole
// deliverable of this mode - an operator reads it off a store that will not start - so each shape's
// distinguishing line is asserted rather than the output as a whole.
func TestPrintSchemaReport(t *testing.T) {
	applied := []db.AppliedMigration{
		{Version: 1, Name: "core_tables", AppliedAt: time.Unix(0, 1767225600000000000)},
		{Version: 2, Name: "core_columns"},
	}

	tests := []struct {
		name    string
		report  db.SchemaReport
		want    []string
		notWant []string
	}{
		{
			name:   "current",
			report: db.SchemaReport{Dialect: "sqlite", Version: 12, Supported: 12, HasLedger: true, Applied: applied},
			want: []string{
				"dialect:   sqlite",
				"version:   12",
				"supported: 12",
				"status:    current",
				// tabwriter pads the name column to the widest entry, so the exact run of spaces
				// is the alignment rather than an accident; asserting the parts separately would
				// stop this catching a row rendered with no timestamp column at all.
				"1  core_tables   2026-01-01 00:00:00Z",
				// A row with no timestamp says so rather than rendering the epoch, which would read
				// as a real date fifty-six years ago.
				"2  core_columns  unrecorded",
			},
		},
		{
			// The store every deployment is holding when this ships.
			name:   "pre-ledger",
			report: db.SchemaReport{Dialect: "sqlite", Supported: 12, Pending: []string{"a", "b", "c"}},
			want: []string{
				"written before schema versions existed",
				"will apply 3 migrations",
			},
			notWant: []string{"version:   0", "applied migrations"},
		},
		{
			name:   "behind",
			report: db.SchemaReport{Dialect: "postgres", Version: 9, Supported: 12, HasLedger: true, Pending: []string{"forgotten_log"}},
			want:   []string{"status:    behind", "1 migration(s) pending", "forgotten_log"},
		},
		{
			name:   "ahead",
			report: db.SchemaReport{Dialect: "mysql", Version: 14, Supported: 12, HasLedger: true},
			want:   []string{"status:    AHEAD", "cannot open it"},
		},
		{
			// A capability-gated migration is never recorded on a dialect it does not apply to, so a
			// current store legitimately sits below this build's newest version. The two numbers
			// disagreeing beside a bare "current" would look like a contradiction.
			name:   "current below the newest declared version",
			report: db.SchemaReport{Dialect: "postgres", Version: 11, Supported: 12, HasLedger: true},
			want:   []string{"status:    current -", "do not apply to this dialect"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out strings.Builder

			printSchemaReport(&out, &tt.report)

			for _, want := range tt.want {
				if !strings.Contains(out.String(), want) {
					t.Errorf("output does not contain %q:\n%s", want, out.String())
				}
			}

			for _, unwanted := range tt.notWant {
				if strings.Contains(out.String(), unwanted) {
					t.Errorf("output should not contain %q:\n%s", unwanted, out.String())
				}
			}
		})
	}
}

// TestReportSchemaVersion drives the mode end to end against a real store.
func TestReportSchemaVersion(t *testing.T) {
	directory := t.TempDir()

	database, err := db.New(directory)
	if err != nil {
		t.Fatalf("db.New: %s", err)
	}

	if err := database.Close(); err != nil {
		t.Fatalf("Close: %s", err)
	}

	// No fatal: a current store reports and returns.
	reportSchemaVersion("sqlite", directory, "", "")
}

// TestReportSchemaVersion_AheadExitsNonZero pins the exit status a deployment script gates on: a
// store the running build cannot open must not read as success.
func TestReportSchemaVersion_AheadExitsNonZero(t *testing.T) {
	directory := t.TempDir()

	database, err := db.New(directory)
	if err != nil {
		t.Fatalf("db.New: %s", err)
	}

	report, err := db.InspectSchema("sqlite", directory)
	if err != nil {
		t.Fatalf("InspectSchema: %s", err)
	}

	if err := database.Close(); err != nil {
		t.Fatalf("Close: %s", err)
	}

	stampFutureSchemaVersion(t, directory, report.Supported+1)

	withFatalPanic(t, func() {
		reportSchemaVersion("sqlite", directory, "", "")
	})
}

// stampFutureSchemaVersion records a migration version no build declares, producing the store a
// refused downgrade leaves behind.
//
// It writes the row directly rather than through the db package, which deliberately exposes no way
// to forge a version. The table name is part of the on-disk shape now, so naming it here is a
// statement about that rather than a reach into a private detail.
func stampFutureSchemaVersion(t *testing.T, directory string, version int) {
	t.Helper()

	sqlDB, err := sql.Open("sqlite", "file:"+filepath.Join(directory, db.DataFile))
	if err != nil {
		t.Fatalf("opening the store to stamp it: %s", err)
	}

	defer func() { _ = sqlDB.Close() }()

	if _, err := sqlDB.Exec(
		`INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, ?)`,
		version, "from_the_future", time.Now().UnixNano(),
	); err != nil {
		t.Fatalf("stamping a future version: %s", err)
	}
}

// TestReportSchemaVersion_UnreadableStoreIsFatal covers the store that is not there.
func TestReportSchemaVersion_UnreadableStoreIsFatal(t *testing.T) {
	withFatalPanic(t, func() {
		reportSchemaVersion("sqlite", t.TempDir(), "", "")
	})
}

// TestReportSchemaVersion_UnknownDriverIsFatal covers a misconfigured storage.driver.
func TestReportSchemaVersion_UnknownDriverIsFatal(t *testing.T) {
	withFatalPanic(t, func() {
		reportSchemaVersion("bogus", t.TempDir(), "", "")
	})
}
