package main

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/viper"
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
	reportSchemaVersion(schemaVersionConfig{StorageDriver: "sqlite", StorageDirectory: directory})
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
		reportSchemaVersion(schemaVersionConfig{StorageDriver: "sqlite", StorageDirectory: directory})
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
		reportSchemaVersion(schemaVersionConfig{StorageDriver: "sqlite", StorageDirectory: t.TempDir()})
	})
}

// TestReportSchemaVersion_UnknownDriverIsFatal covers a misconfigured storage.driver.
func TestReportSchemaVersion_UnknownDriverIsFatal(t *testing.T) {
	withFatalPanic(t, func() {
		reportSchemaVersion(schemaVersionConfig{StorageDriver: "bogus", StorageDirectory: t.TempDir()})
	})
}

// TestSchemaStatus pins the three status values both renderings derive from. They are what a script
// branches on, so their spellings are part of this command's output contract, not an internal
// detail.
func TestSchemaStatus(t *testing.T) {
	tests := []struct {
		name   string
		report db.SchemaReport
		want   string
	}{
		{name: "current", report: db.SchemaReport{Version: 12, Supported: 12}, want: "current"},

		// A migration gated to another dialect is never recorded here, so a current store
		// legitimately sits below this build's newest version. Deriving the status from the two
		// numbers rather than from what is pending would call this one behind.
		{name: "current below the newest declared version", report: db.SchemaReport{Version: 11, Supported: 12}, want: "current"},

		{name: "behind", report: db.SchemaReport{Version: 9, Supported: 12, Pending: []string{"x"}}, want: "behind"},
		{name: "pre-ledger is behind", report: db.SchemaReport{Supported: 12, Pending: []string{"x", "y"}}, want: "behind"},
		{name: "ahead", report: db.SchemaReport{Version: 13, Supported: 12}, want: "ahead"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := schemaStatus(&tt.report); got != tt.want {
				t.Errorf("schemaStatus = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestPrintSchemaReportJSON covers the machine-readable rendering, which is a contract with whatever
// parses it rather than prose: the field names, the empty-not-null collections, and the absent
// timestamp are each something a consumer would otherwise have to guard for.
func TestPrintSchemaReportJSON(t *testing.T) {
	report := db.SchemaReport{
		Dialect:   "postgres",
		Version:   11,
		Supported: 12,
		HasLedger: true,
		Applied: []db.AppliedMigration{
			{Version: 1, Name: "core_tables", AppliedAt: time.Unix(0, 1767225600000000000)},
			{Version: 2, Name: "core_columns"},
		},
	}

	var out strings.Builder

	if err := printSchemaReportJSON(&out, &report); err != nil {
		t.Fatalf("printSchemaReportJSON: %s", err)
	}

	var decoded struct {
		Dialect   string   `json:"dialect"`
		Status    string   `json:"status"`
		Version   int      `json:"version"`
		Supported int      `json:"supported"`
		HasLedger bool     `json:"has_ledger"`
		Pending   []string `json:"pending"`
		Applied   []struct {
			Version   int     `json:"version"`
			Name      string  `json:"name"`
			AppliedAt *string `json:"applied_at"`
		} `json:"applied"`
	}

	if err := json.Unmarshal([]byte(out.String()), &decoded); err != nil {
		t.Fatalf("the output must parse as JSON: %s\n%s", err, out.String())
	}

	if decoded.Dialect != "postgres" || decoded.Version != 11 || decoded.Supported != 12 || !decoded.HasLedger {
		t.Errorf("scalars did not survive the projection: %+v", decoded)
	}

	if decoded.Status != "current" {
		t.Errorf("status = %q, want current", decoded.Status)
	}

	// Empty rather than null, so a consumer need not guard for the difference.
	if decoded.Pending == nil {
		t.Error("pending must be an empty array, not null")
	}

	if !strings.Contains(out.String(), `"pending": []`) {
		t.Errorf("pending should render as [], got:\n%s", out.String())
	}

	if len(decoded.Applied) != 2 {
		t.Fatalf("applied has %d entries, want 2", len(decoded.Applied))
	}

	if decoded.Applied[0].AppliedAt == nil || *decoded.Applied[0].AppliedAt != "2026-01-01T00:00:00Z" {
		t.Errorf("applied_at = %v, want RFC3339 in UTC", decoded.Applied[0].AppliedAt)
	}

	// A row with no timestamp is null, not the zero time - which would render as a date in year one
	// and read as a real value.
	if decoded.Applied[1].AppliedAt != nil {
		t.Errorf("an unrecorded applied_at must be null, got %q", *decoded.Applied[1].AppliedAt)
	}
}

// TestPrintSchemaReportJSON_PreLedger covers the store every deployment is holding when this ships:
// no ledger, so version 0 and everything pending. has_ledger is what stops a consumer reading that
// zero as a real version, and there is no other signal for it.
func TestPrintSchemaReportJSON_PreLedger(t *testing.T) {
	var out strings.Builder

	report := db.SchemaReport{Dialect: "sqlite", Supported: 12, Pending: []string{"core_tables", "core_columns"}}

	if err := printSchemaReportJSON(&out, &report); err != nil {
		t.Fatalf("printSchemaReportJSON: %s", err)
	}

	for _, want := range []string{`"has_ledger": false`, `"version": 0`, `"status": "behind"`, `"core_tables"`, `"applied": []`} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output does not contain %s:\n%s", want, out.String())
		}
	}
}

// TestReportSchemaVersion_JSONStdoutStaysParseable is the property the whole stdout/stderr split
// exists for, and the one a test is worth having for: on the AHEAD path the mode renders the report
// and then fatals, and if that log line went to stdout - which is where this binary's logging
// normally goes - the document a deployment script is parsing would be corrupt precisely when it
// matters most.
func TestReportSchemaVersion_JSONStdoutStaysParseable(t *testing.T) {
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

	// The mode points logging at stderr and does not put it back, which is right for a terminal CLI
	// mode and would leak into every test after this one.
	defer log.StandardLogger().SetOutput(log.StandardLogger().Out)

	// withFatalPanic recovers inside itself, so the capture sees a normal return and still reads
	// everything written before the fatal.
	out := captureStdout(t, func() {
		withFatalPanic(t, func() {
			reportSchemaVersion(schemaVersionConfig{StorageDriver: "sqlite", StorageDirectory: directory, JSON: true})
		})
	})

	var decoded map[string]any

	if err := json.Unmarshal([]byte(out), &decoded); err != nil {
		t.Fatalf("stdout must be parseable JSON even on the fatal path: %s\n%s", err, out)
	}

	if decoded["status"] != "ahead" {
		t.Errorf("status = %v, want ahead", decoded["status"])
	}
}

// TestExecute_SchemaVersion drives the flag wiring: --schema-version reads the configured store and
// returns without starting the server, and --output json changes the rendering rather than being
// silently ignored.
func TestExecute_SchemaVersion(t *testing.T) {
	directory := t.TempDir()

	database, err := db.New(directory)
	if err != nil {
		t.Fatalf("db.New: %s", err)
	}

	if err := database.Close(); err != nil {
		t.Fatalf("Close: %s", err)
	}

	config := writeConfigFile(t, `{"storage": {"driver": "sqlite", "directory": "`+directory+`"}}`)

	for _, tt := range []struct {
		name string
		args []string
		want string
	}{
		{name: "text is the default", args: []string{"--schema-version", "-c", config}, want: "supported: "},
		{name: "json", args: []string{"--schema-version", "--output", "json", "-c", config}, want: `"status": "current"`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			viper.Reset()
			defer viper.Reset()
			defer log.StandardLogger().SetOutput(log.StandardLogger().Out)

			out := captureStdout(t, func() {
				execute(tt.args)
			})

			if !strings.Contains(out, tt.want) {
				t.Errorf("execute(%v) output does not contain %q:\n%s", tt.args, tt.want, out)
			}
		})
	}
}

// TestExecute_SchemaVersionRejectsAnUnknownFormat fails fast rather than falling back to text, which
// would hand a script that asked for JSON something it cannot parse and no indication why.
func TestExecute_SchemaVersionRejectsAnUnknownFormat(t *testing.T) {
	viper.Reset()
	defer viper.Reset()

	config := writeConfigFile(t, `{"storage": {"driver": "sqlite", "directory": "`+t.TempDir()+`"}}`)

	withFatalPanic(t, func() {
		execute([]string{"--schema-version", "--output", "yaml", "-c", config})
	})
}
