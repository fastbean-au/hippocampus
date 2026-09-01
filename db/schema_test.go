package db

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// TestMigrationVersionsAreStable pins the version-to-name mapping.
//
// Versions are what the ledger records, so renumbering one silently re-points every stored row at a
// different step: a store that ran "link_tables" as version 4 would, after an insertion shifted it
// to 5, read as never having run 4 and as having run whatever now occupies it. This is the guard
// that makes "append, never insert" a rule the build enforces rather than a comment.
//
// A new migration adds a line here. Changing an existing one is the thing this refuses.
func TestMigrationVersionsAreStable(t *testing.T) {
	want := map[int]string{
		1:  "core_tables",
		2:  "core_columns",
		3:  "id_collation",
		4:  "link_tables",
		5:  "drop_legacy_relationships",
		6:  "significance_levels",
		7:  "covering_index",
		8:  "listing_index",
		9:  "search_outbox",
		10: "forgotten_log",
		11: "instance_registry",
		12: "content_search",
	}

	migrations := (&DB{driver: driverSQLite}).migrations()

	if len(migrations) != len(want) {
		t.Errorf("the schema declares %d migrations and this guard pins %d - a new one needs a line "+
			"here, and an existing one must never be renumbered", len(migrations), len(want))
	}

	seen := map[int]bool{}
	previous := 0

	for _, migration := range migrations {
		if name, ok := want[migration.version]; ok && name != migration.name {
			t.Errorf("version %d is %q but was released as %q - versions are never reused",
				migration.version, migration.name, name)
		}

		if _, ok := want[migration.version]; !ok {
			t.Errorf("version %d (%q) is not pinned here", migration.version, migration.name)
		}

		if seen[migration.version] {
			t.Errorf("version %d is declared twice", migration.version)
		}

		if migration.version <= previous {
			t.Errorf("migration %q is version %d, which does not follow %d - the list is ordered",
				migration.name, migration.version, previous)
		}

		seen[migration.version] = true
		previous = migration.version
	}
}

// TestEveryMigrationHasAnApply catches a declaration that would panic at startup rather than at
// build time, since apply is a field rather than a method.
func TestEveryMigrationHasAnApply(t *testing.T) {
	for _, dialect := range []driver{driverSQLite, driverPostgres, driverMySQL} {
		d := &DB{driver: dialect}

		for _, migration := range d.migrations() {
			if migration.apply == nil {
				t.Errorf("%s migration %d (%q) declares no apply", d.dialect().name, migration.version, migration.name)
			}

			if migration.name == "" {
				t.Errorf("%s migration %d declares no name", d.dialect().name, migration.version)
			}
		}
	}
}

// TestSchemaVersionIsTheNewestMigration pins what the version gate compares against.
func TestSchemaVersionIsTheNewestMigration(t *testing.T) {
	d := &DB{driver: driverSQLite}

	migrations := d.migrations()

	if got, want := d.schemaVersion(), migrations[len(migrations)-1].version; got != want {
		t.Errorf("schemaVersion = %d, want the newest migration's %d", got, want)
	}
}

// TestCheckSchemaVersionRefusesTheFuture is the downgrade guard, and the reason the ledger exists.
//
// "Downgrading is not supported" was a sentence in CHANGELOG.md with nothing enforcing it: an older
// binary opened a store written by a newer one, found every table it expected, and served.
func TestCheckSchemaVersionRefusesTheFuture(t *testing.T) {
	d := &DB{driver: driverSQLite}

	tests := []struct {
		name    string
		applied map[int]bool
		wantErr bool
	}{
		{name: "empty ledger", applied: map[int]bool{}},
		{name: "current", applied: map[int]bool{d.schemaVersion(): true}},

		// A capability-gated migration is never recorded on a dialect it does not apply to, so a
		// legitimate ledger has gaps. Reading a gap as "not migrated" would refuse a current store.
		{name: "gapped", applied: map[int]bool{1: true, 2: true, 4: true, 12: true}},

		{name: "one ahead", applied: map[int]bool{d.schemaVersion() + 1: true}, wantErr: true},
		{name: "far ahead", applied: map[int]bool{d.schemaVersion() + 40: true}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := d.checkSchemaVersion(tt.applied)

			if tt.wantErr && err == nil {
				t.Fatal("a store recorded ahead of this build must be refused")
			}

			if !tt.wantErr {
				if err != nil {
					t.Fatalf("checkSchemaVersion: %s", err)
				}

				return
			}

			if !errors.Is(err, ErrSchemaTooNew) {
				t.Errorf("error should wrap ErrSchemaTooNew so a caller can distinguish it: %v", err)
			}

			// The message is what an operator acts on, so it has to say which way round the problem
			// is and what to do about it.
			for _, want := range []string{"newer Hippocampus", "downgrading is not supported"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %q", err, want)
				}
			}
		})
	}
}

// TestInitSchemaRefusesAFutureStore drives the gate end to end against a real store: open one,
// stamp a version this build does not have, and reopen it.
func TestInitSchemaRefusesAFutureStore(t *testing.T) {
	requireSQLite(t)

	directory := t.TempDir()

	database, err := New(directory)
	if err != nil {
		t.Fatalf("New: %s", err)
	}

	future := database.schemaVersion() + 1

	if _, err := database.sql.Exec(
		`INSERT INTO `+schemaMigrationsTable+` (version, name, applied_at) VALUES (?, ?, ?)`,
		future, "from_the_future", 1,
	); err != nil {
		t.Fatalf("stamping a future version: %s", err)
	}

	if err := database.Close(); err != nil {
		t.Fatalf("Close: %s", err)
	}

	reopened, err := New(directory)
	if err == nil {
		_ = reopened.Close()

		t.Fatal("a store recorded ahead of this build must not open")
	}

	if !errors.Is(err, ErrSchemaTooNew) {
		t.Errorf("New should fail with ErrSchemaTooNew, got %v", err)
	}
}

// TestReadOnlyOpenRefusesAFutureStore covers the same gate on the path that runs no DDL. A
// read-only tool cannot corrupt a store it does not understand, but it can produce a wrong answer
// from one - a search index rebuilt against a schema whose meaning has moved is worse than no
// rebuild, because nothing about it looks wrong afterwards.
func TestReadOnlyOpenRefusesAFutureStore(t *testing.T) {
	requireSQLite(t)

	directory := t.TempDir()

	database, err := New(directory)
	if err != nil {
		t.Fatalf("New: %s", err)
	}

	if _, err := database.sql.Exec(
		`INSERT INTO `+schemaMigrationsTable+` (version, name, applied_at) VALUES (?, ?, ?)`,
		database.schemaVersion()+1, "from_the_future", 1,
	); err != nil {
		t.Fatalf("stamping a future version: %s", err)
	}

	if err := database.Close(); err != nil {
		t.Fatalf("Close: %s", err)
	}

	if _, err := NewSQLiteReadOnly(directory); !errors.Is(err, ErrSchemaTooNew) {
		t.Errorf("a read-only open of a future store should fail with ErrSchemaTooNew, got %v", err)
	}
}

// TestReadOnlyOpenToleratesAnAbsentLedger is the other half of that: a store written before the
// ledger existed has no table to read, and refusing to back-fill a search index because of it would
// be a regression for no safety gained.
func TestReadOnlyOpenToleratesAnAbsentLedger(t *testing.T) {
	requireSQLite(t)

	directory := t.TempDir()

	database, err := New(directory)
	if err != nil {
		t.Fatalf("New: %s", err)
	}

	if _, err := database.sql.Exec(`DROP TABLE ` + schemaMigrationsTable); err != nil {
		t.Fatalf("dropping the ledger: %s", err)
	}

	if err := database.Close(); err != nil {
		t.Fatalf("Close: %s", err)
	}

	readOnly, err := NewSQLiteReadOnly(directory)
	if err != nil {
		t.Fatalf("a store with no ledger must still open read-only: %s", err)
	}

	_ = readOnly.Close()
}

// TestInitSchemaIsIdempotentAndRecordsItself opens a store twice and checks the ledger both times.
// Re-running every self-detecting step is what makes the ledger safe to add to an existing store -
// there is no baseline detection to get wrong - so the second open must change nothing.
func TestInitSchemaIsIdempotentAndRecordsItself(t *testing.T) {
	requireSQLite(t)

	directory := t.TempDir()

	first := ledgerAfterOpen(t, directory)
	second := ledgerAfterOpen(t, directory)

	if len(first) == 0 {
		t.Fatal("the first open recorded nothing")
	}

	if len(first) != len(second) {
		t.Errorf("reopening changed the ledger: %v then %v", first, second)
	}

	for version, name := range first {
		if second[version] != name {
			t.Errorf("version %d was %q and is now %q", version, name, second[version])
		}
	}

	// Every migration that applies to this dialect is recorded, and nothing else is.
	d := &DB{driver: driverSQLite}

	for _, migration := range d.migrations() {
		applies := migration.when == nil || migration.when(d.dialect())

		if applies && first[migration.version] != migration.name {
			t.Errorf("migration %d (%q) applies here but is not recorded", migration.version, migration.name)
		}

		if !applies && first[migration.version] != "" {
			t.Errorf("migration %d (%q) does not apply here but is recorded", migration.version, migration.name)
		}
	}
}

// ledgerAfterOpen opens a store at the given directory, reads its ledger, and closes it.
func ledgerAfterOpen(t *testing.T, directory string) map[int]string {
	t.Helper()

	database, err := New(directory)
	if err != nil {
		t.Fatalf("New(%s): %s", filepath.Base(directory), err)
	}

	defer func() { _ = database.Close() }()

	rows, err := database.sql.Query(`SELECT version, name FROM ` + schemaMigrationsTable)
	if err != nil {
		t.Fatalf("reading the ledger: %s", err)
	}

	defer func() { _ = rows.Close() }()

	ledger := map[int]string{}

	for rows.Next() {
		var (
			version int
			name    string
		)

		if err := rows.Scan(&version, &name); err != nil {
			t.Fatalf("scanning the ledger: %s", err)
		}

		ledger[version] = name
	}

	return ledger
}

// TestSchemaHealsARevertedMigration is the property the ledger deliberately does NOT gate on: every
// migration runs on every startup, so a store whose schema has been reverted - a dropped index, a
// restored partial backup - is repaired on the next open, even though the ledger says the migration
// already ran.
//
// The obvious design skips a recorded migration. It was tried, and this is what caught it: with the
// skip in place, a store rebuilt into its pre-registry shape opened cleanly and kept the old column
// forever, because the ledger said that step was done. Every step here detects its own completion,
// so skipping saves a handful of round trips and gives up self-healing entirely.
func TestSchemaHealsARevertedMigration(t *testing.T) {
	requireSQLite(t)

	directory := t.TempDir()

	database, err := New(directory)
	if err != nil {
		t.Fatalf("New: %s", err)
	}

	// The ledger records the index migration, and the index is then dropped behind its back.
	if _, err := database.sql.Exec(`DROP INDEX ` + coveringIndexName); err != nil {
		t.Fatalf("dropping the covering index: %s", err)
	}

	if err := database.Close(); err != nil {
		t.Fatalf("Close: %s", err)
	}

	reopened, err := New(directory)
	if err != nil {
		t.Fatalf("reopening: %s", err)
	}

	defer func() { _ = reopened.Close() }()

	var name string

	err = reopened.sql.QueryRow(
		reopened.rebind(`SELECT name FROM sqlite_master WHERE type = 'index' AND name = ?`),
		coveringIndexName,
	).Scan(&name)
	if err != nil {
		t.Fatalf("the covering index was not restored on reopening: %s", err)
	}
}

// TestInspectSchemaReportsACurrentStore covers the ordinary answer: a store this build wrote, read
// back without opening it for service.
func TestInspectSchemaReportsACurrentStore(t *testing.T) {
	requireSQLite(t)

	directory := t.TempDir()

	database, err := New(directory)
	if err != nil {
		t.Fatalf("New: %s", err)
	}

	supported := database.schemaVersion()

	if err := database.Close(); err != nil {
		t.Fatalf("Close: %s", err)
	}

	report, err := InspectSchema("sqlite", directory)
	if err != nil {
		t.Fatalf("InspectSchema: %s", err)
	}

	if report.Dialect != "sqlite" {
		t.Errorf("dialect = %q, want sqlite", report.Dialect)
	}

	if !report.HasLedger {
		t.Error("a store this build wrote must have a ledger")
	}

	if report.Supported != supported {
		t.Errorf("supported = %d, want %d", report.Supported, supported)
	}

	if report.Ahead() {
		t.Error("a store this build wrote is not ahead of it")
	}

	if len(report.Pending) != 0 {
		t.Errorf("a store this build wrote has nothing pending, got %v", report.Pending)
	}

	// Every migration that applies to this dialect is reported, with its name and a real timestamp.
	d := &DB{driver: driverSQLite}

	wanted := 0

	for _, migration := range d.migrations() {
		if migration.when == nil || migration.when(d.dialect()) {
			wanted++
		}
	}

	if len(report.Applied) != wanted {
		t.Errorf("reported %d applied migrations, want %d", len(report.Applied), wanted)
	}

	for _, migration := range report.Applied {
		if migration.Name == "" {
			t.Errorf("migration %d reported no name", migration.Version)
		}

		if migration.AppliedAt.IsZero() {
			t.Errorf("migration %d (%s) reported no timestamp", migration.Version, migration.Name)
		}
	}
}

// TestInspectSchemaReportsAPreLedgerStore is the answer for every store in the field at the moment
// the ledger ships: no migrations table, so nothing recorded and everything pending.
//
// HasLedger is what separates it from a store whose table was dropped, and the distinction is the
// reason that field exists: both read as version 0, and only one of them is normal.
func TestInspectSchemaReportsAPreLedgerStore(t *testing.T) {
	requireSQLite(t)

	directory := t.TempDir()

	database, err := New(directory)
	if err != nil {
		t.Fatalf("New: %s", err)
	}

	if _, err := database.sql.Exec(`DROP TABLE ` + schemaMigrationsTable); err != nil {
		t.Fatalf("dropping the ledger: %s", err)
	}

	if err := database.Close(); err != nil {
		t.Fatalf("Close: %s", err)
	}

	report, err := InspectSchema("sqlite", directory)
	if err != nil {
		t.Fatalf("a store with no ledger must be inspectable, not an error: %s", err)
	}

	if report.HasLedger {
		t.Error("HasLedger must be false when there is no migrations table")
	}

	if report.Version != 0 {
		t.Errorf("version = %d, want 0", report.Version)
	}

	// Everything applicable is pending, which is what tells an operator what an upgrade would do.
	// Reporting none - which an early return would - would say the opposite.
	if len(report.Pending) == 0 {
		t.Error("a store with no ledger has every applicable migration pending, and must say so")
	}

	if report.Ahead() {
		t.Error("a store with no recorded version is not ahead of anything")
	}
}

// TestInspectSchemaReportsAFutureStore is the case this whole entry point exists for: the store the
// read-only constructors refuse, so there is nothing left for them to report.
func TestInspectSchemaReportsAFutureStore(t *testing.T) {
	requireSQLite(t)

	directory := t.TempDir()

	database, err := New(directory)
	if err != nil {
		t.Fatalf("New: %s", err)
	}

	future := database.schemaVersion() + 1

	if _, err := database.sql.Exec(
		`INSERT INTO `+schemaMigrationsTable+` (version, name, applied_at) VALUES (?, ?, ?)`,
		future, "from_the_future", 1,
	); err != nil {
		t.Fatalf("stamping a future version: %s", err)
	}

	if err := database.Close(); err != nil {
		t.Fatalf("Close: %s", err)
	}

	// The gated open refuses it - which is correct, and is exactly why inspection cannot go through
	// that path.
	if _, err := NewSQLiteReadOnly(directory); !errors.Is(err, ErrSchemaTooNew) {
		t.Fatalf("the read-only open should refuse a future store, got %v", err)
	}

	report, err := InspectSchema("sqlite", directory)
	if err != nil {
		t.Fatalf("InspectSchema must read a store the gate refuses: %s", err)
	}

	if !report.Ahead() {
		t.Errorf("version %d against supported %d must report as ahead", report.Version, report.Supported)
	}

	if report.Version != future {
		t.Errorf("version = %d, want %d", report.Version, future)
	}
}

// TestInspectSchemaFailsCleanly covers the two ways it cannot answer: a store that is not there, and
// a driver it does not know. Both must be errors rather than an empty report, which would read as
// "version 0" - indistinguishable from a real pre-ledger store.
func TestInspectSchemaFailsCleanly(t *testing.T) {
	requireSQLite(t)

	if _, err := InspectSchema("sqlite", filepath.Join(t.TempDir(), "nothing-here")); err == nil {
		t.Error("inspecting a directory with no database must fail")
	}

	if _, err := InspectSchema("sqlite", ""); err == nil {
		t.Error("inspecting with no directory must fail")
	}

	if _, err := InspectSchema("cassandra", "whatever"); err == nil {
		t.Error("an unknown driver must fail")
	}
}

// TestInspectSchemaTakesNoLock verifies the tool is safe beside a live instance: inspecting a store
// while the service holds it must neither block nor be refused.
func TestInspectSchemaTakesNoLock(t *testing.T) {
	requireSQLite(t)

	directory := t.TempDir()

	database, err := New(directory)
	if err != nil {
		t.Fatalf("New: %s", err)
	}

	// Still open, still holding the storage lock.
	defer func() { _ = database.Close() }()

	report, err := InspectSchema("sqlite", directory)
	if err != nil {
		t.Fatalf("inspecting a store a live instance holds must work: %s", err)
	}

	if report.Version != database.schemaVersion() {
		t.Errorf("version = %d, want %d", report.Version, database.schemaVersion())
	}
}
