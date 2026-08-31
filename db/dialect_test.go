package db

import (
	"strings"
	"testing"
)

// TestUpsertRendersBothForms pins the two upsert shapes against one spec, which is the whole reason
// the builder exists: four call sites used to carry their own pair of arms, and the pair is easy to
// get subtly wrong (the row alias, the AS clause the newer MySQL syntax requires, which columns are
// refreshed and which keep their stored value).
func TestUpsertRendersBothForms(t *testing.T) {
	spec := upsertSpec{
		table:   "memory_links",
		columns: `from_id, to_id, significance, created`,
		key:     []string{"from_id", "to_id"},
		update:  []string{"significance"},
	}

	standard := (&DB{driver: driverPostgres}).upsert(spec)

	for _, want := range []string{
		`INSERT INTO memory_links (from_id, to_id, significance, created)`,
		`VALUES (?,?,?,?)`,
		`ON CONFLICT (from_id, to_id) DO UPDATE SET`,
		`significance = excluded.significance`,
	} {
		if !strings.Contains(standard, want) {
			t.Errorf("standard upsert %q does not contain %q", standard, want)
		}
	}

	// created is in the column list but not the update list, so a re-link re-weights the edge and
	// leaves "when this association was first made" alone.
	if strings.Contains(standard, "created = excluded.created") {
		t.Errorf("standard upsert must not refresh a column absent from the update list: %q", standard)
	}

	duplicate := (&DB{driver: driverMySQL}).upsert(spec)

	for _, want := range []string{
		`INSERT INTO memory_links (from_id, to_id, significance, created)`,
		` AS new`,
		`ON DUPLICATE KEY UPDATE`,
		`significance = new.significance`,
	} {
		if !strings.Contains(duplicate, want) {
			t.Errorf("duplicate-key upsert %q does not contain %q", duplicate, want)
		}
	}

	// It infers the conflicting key from the violated index rather than being told, so naming one
	// would be a syntax error.
	if strings.Contains(duplicate, "ON CONFLICT") {
		t.Errorf("duplicate-key upsert must not name the conflict target: %q", duplicate)
	}
}

// TestUpsertHonoursASuppliedValuesClause covers the bulk-import path, which builds its own tuple
// list rather than taking the generated single row.
func TestUpsertHonoursASuppliedValuesClause(t *testing.T) {
	got := (&DB{driver: driverSQLite}).upsert(upsertSpec{
		table:   "memories",
		columns: `id, body`,
		values:  `(?, ?), (?, ?)`,
		key:     []string{"id"},
		update:  []string{"body"},
	})

	if !strings.Contains(got, `VALUES (?, ?), (?, ?)`) {
		t.Errorf("a supplied VALUES clause must be used verbatim, got %q", got)
	}
}

// TestGreatestIsTheScalarMaximum pins the decay clock's spelling per dialect. The embedded dialect's
// two-argument MAX is a scalar function; on the server dialects MAX is aggregate-only and using it
// here would either be a syntax error or - worse - collapse the scan to one row.
func TestGreatestIsTheScalarMaximum(t *testing.T) {
	tests := []struct {
		name   string
		driver driver
		want   string
	}{
		{name: "sqlite", driver: driverSQLite, want: `MAX(timestamp, time_recalled)`},
		{name: "postgres", driver: driverPostgres, want: `GREATEST(timestamp, time_recalled)`},
		{name: "mysql", driver: driverMySQL, want: `GREATEST(timestamp, time_recalled)`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := (&DB{driver: tt.driver}).greatest("timestamp", "time_recalled"); got != tt.want {
				t.Errorf("greatest = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestCoreSchemaIsOneTemplatePerDialect verifies the shared CREATE TABLE renders each dialect's own
// types rather than one dialect's leaking into another - the risk a single template trades the
// three-copies-drifting risk for.
func TestCoreSchemaIsOneTemplatePerDialect(t *testing.T) {
	tests := []struct {
		name    string
		driver  driver
		present []string
		absent  []string
	}{
		{
			name:    "sqlite",
			driver:  driverSQLite,
			present: []string{`id                    TEXT PRIMARY KEY`, `body                  BLOB NOT NULL DEFAULT x''`, `timestamp             INTEGER NOT NULL DEFAULT 0`},
			absent:  []string{"BIGINT", "JSONB", "VARCHAR", "BYTEA"},
		},
		{
			name:    "postgres",
			driver:  driverPostgres,
			present: []string{`id                    TEXT PRIMARY KEY`, `body                  BYTEA NOT NULL DEFAULT ''::bytea`, `metadata              JSONB`},
			absent:  []string{"VARCHAR", "BLOB", "COLLATE"},
		},
		{
			name:    "mysql",
			driver:  driverMySQL,
			present: []string{`VARCHAR(255) COLLATE ` + mysqlBinaryCollation + ` PRIMARY KEY`, `body                  LONGBLOB NOT NULL,`, `metadata              JSON`},
			absent:  []string{"BYTEA", "JSONB", `BLOB NOT NULL DEFAULT`},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			schema := strings.Join((&DB{driver: tt.driver}).coreSchemaStatements(), "\n")

			for _, want := range tt.present {
				if !strings.Contains(schema, want) {
					t.Errorf("%s schema does not contain %q:\n%s", tt.name, want, schema)
				}
			}

			for _, unwanted := range tt.absent {
				if strings.Contains(schema, unwanted) {
					t.Errorf("%s schema contains %q, which belongs to another dialect:\n%s", tt.name, unwanted, schema)
				}
			}
		})
	}
}

// TestCoreSchemaCoversTheStoredColumns holds the shared template to the column list every INSERT is
// built from. A column added to one and not the other is the failure a single template was supposed
// to make impossible, and it would present as a store that creates cleanly and then fails its first
// write.
func TestCoreSchemaCoversTheStoredColumns(t *testing.T) {
	schema := strings.Join((&DB{driver: driverSQLite}).coreSchemaStatements(), "\n")

	// link_significance is a physical column but deliberately absent from memoryStoredColumns,
	// being maintained by the link graph rather than supplied by a write.
	for column := range namedColumns(memoryStoredColumns + ", link_significance") {
		if !strings.Contains(schema, column) {
			t.Errorf("memories.%s is written by every insert but is not in the shared CREATE TABLE", column)
		}
	}

	for column := range namedColumns(eventStoredColumns) {
		if !strings.Contains(schema, column) {
			t.Errorf("events.%s is written by every insert but is not in the shared CREATE TABLE", column)
		}
	}
}

// TestCoreColumnMigrationsRenderPerDialect verifies the shared migration list carries each dialect's
// own type for the same column - the half of the schema a single template would be useless without,
// since a column added to a fresh database by CREATE TABLE reaches an existing one only through here.
func TestCoreColumnMigrationsRenderPerDialect(t *testing.T) {
	definitionOf := func(d driver, table string, column string) string {
		t.Helper()

		for _, entry := range (&DB{driver: d}).coreColumnMigrations() {
			if entry.table == table && entry.column == column {
				return entry.definition
			}
		}

		t.Fatalf("coreColumnMigrations no longer carries %s.%s", table, column)

		return ""
	}

	tests := []struct {
		name   string
		driver driver
		bool   string
		json   string
	}{
		{name: "sqlite", driver: driverSQLite, bool: "INTEGER NOT NULL DEFAULT 0", json: "TEXT"},
		{name: "postgres", driver: driverPostgres, bool: "BOOLEAN NOT NULL DEFAULT FALSE", json: "JSONB"},
		{name: "mysql", driver: driverMySQL, bool: "BOOLEAN NOT NULL DEFAULT FALSE", json: "JSON"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := definitionOf(tt.driver, "memories", "is_summary"); got != tt.bool {
				t.Errorf("memories.is_summary = %q, want %q", got, tt.bool)
			}

			// Metadata must stay NULL-able with no DEFAULT on every dialect; see the column's own
			// comment for what an ''-defaulted column does to the first filtered query.
			if got := definitionOf(tt.driver, "memories", "metadata"); got != tt.json {
				t.Errorf("memories.metadata = %q, want exactly %q with no NOT NULL or DEFAULT", got, tt.json)
			}
		})
	}
}
