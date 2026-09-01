package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path"
	"time"

	log "github.com/sirupsen/logrus"
)

// The schema, as one ordered list of migrations shared by every dialect, and the ledger recording
// which of them a store has seen.
//
// Three things this file is for, in order of how much they matter.
//
// FIRST, the version gate. "Downgrading is not supported" was a sentence in CHANGELOG.md with
// nothing enforcing it: an older binary opened a store written by a newer one, found every table it
// expected, and served. It would go on serving right up until it met a migration whose meaning had
// changed underneath it - the significance registry's move off the per-item column being exactly
// that shape of change - and by then the damage is in the data. A store now states its schema
// version, and a build that does not understand that version refuses to open it and says so.
//
// SECOND, one declared list instead of three functions. After the dialect table landed, the three
// initialisers differed in exactly three places, all of which are dialect CAPABILITIES rather than
// dialect procedures: the embedded dialect configures incremental vacuum and carries the content
// index, one server dialect migrates an id collation, and both server dialects keep a peer
// registry. Expressed as gates on the shared list, the three functions become one. The
// schema-fixture drift guard then reads a declared list rather than parsing the Go AST of whatever
// the init functions happened to call, which is what it did before.
//
// What this deliberately is NOT is a gate on whether each step runs. EVERY migration runs on EVERY
// startup, exactly as it did before the ledger existed, and the ledger records rather than decides.
//
// That is worth stating plainly because the other design is the obvious one, and it is wrong here.
// Every step below detects its own completion - it probes for the column, checks the collation,
// looks for the old table - so re-running one costs a round trip and does nothing. Skipping a
// recorded step would save those round trips and give up the property that makes them worth having:
// a store whose index was dropped, or whose schema was restored from a partial backup, is repaired
// on the next startup. TestSchemaHealsARevertedMigration is that property written down.
//
// It also removes a whole class of mistake from adding the ledger to stores that already exist.
// There is no baselining, no "assume everything up to version N already ran" heuristic to get
// wrong: a store that has never seen the ledger runs every step once more, each finding its work
// already done, and is recorded.
//
// A future migration that CANNOT detect its own completion - a data backfill that leaves no trace of
// having run - would need a skip-when-recorded flag, and this is where it would go. No step today
// may take one, and the reason is above.

// schemaMigrationsTable records which migrations a store has seen. Deliberately its own table
// rather than a row in an existing one: it is read before any other table is known to exist.
const schemaMigrationsTable = "schema_migrations"

// ErrSchemaTooNew is returned when a store's recorded schema version is higher than the newest this
// build declares. It is a refusal to open, not a warning: the alternative is serving a store whose
// shape this binary only partly understands.
var ErrSchemaTooNew = errors.New("database schema is newer than this build supports")

// schemaMigration is one step in the schema's history.
//
// Versions are assigned once and never renumbered or reused. A new migration takes the next number
// and appends; inserting one in the middle would silently re-point every ledger row after it at a
// different step. TestMigrationVersionsAreStable pins the mapping so that cannot happen quietly.
type schemaMigration struct {
	// version orders the list and is what the ledger records.
	version int

	// name identifies the migration in the ledger, in logs, and in the schema-fixture guard's
	// declarations. Stable, like the version.
	name string

	// when reports whether this migration applies to a dialect at all. Nil means every dialect.
	// A migration that does not apply is neither run nor recorded, so enabling a capability later
	// (which cannot happen - a store does not change dialect - but the ledger should not lie about
	// it either) would still run it.
	when func(*dialect) bool

	// covers names the units the schema-fixture guard should account for separately, when one
	// migration is really a list. Empty means the migration answers for itself under its own name.
	covers func(*DB) []string

	apply func(*DB) error
}

// migrations is the schema's history, oldest first.
//
// The numbering starts at 1 with the whole pre-ledger schema as a single step rather than being
// reconstructed into the sequence it was actually built in. That reconstruction would be fiction:
// these steps arrived across seventeen releases, several of them are already the accumulated result
// of earlier ones, and no store anywhere is at an intermediate point between them - every store in
// existence has either seen all of them or is about to. What the ledger needs to be honest about is
// the future, and it is: version 12 is where this list ends today, and a store recording 13 was
// written by something this build has not met.
func (d *DB) migrations() []schemaMigration {
	return []schemaMigration{
		{
			version: 1,
			name:    "core_tables",
			apply:   (*DB).createCoreTables,
		},
		{
			version: 2,
			name:    "core_columns",
			covers:  coreColumnNames,
			apply:   (*DB).migrateCoreColumns,
		},
		{
			// One server dialect's ids predate their collation being pinned to a binary one. The
			// CREATE TABLE above collates a new database correctly, so this only ever has work to do
			// on a database created before that; the five probes it costs otherwise are the price of
			// it being able to correct one that has drifted.
			version: 3,
			name:    "id_collation",
			when:    func(dialect *dialect) bool { return dialect.idCollationMigration },
			apply:   (*DB).migrateIdCollation,
		},
		{
			version: 4,
			name:    "link_tables",
			apply:   (*DB).initLinkTables,
		},
		{
			// Event relationships became event links; the old JSON column and its denormalised sum
			// are dropped. Guarded on the columns still existing, so it is a no-op on a fresh
			// database and on every startup after the first.
			version: 5,
			name:    "drop_legacy_relationships",
			apply:   (*DB).dropLegacyRelationshipColumns,
		},
		{
			// The move from a per-item significance column to the shared registry. Guarded on that
			// column still existing, which the current schema never creates.
			version: 6,
			name:    "significance_levels",
			apply:   (*DB).migrateSignificanceToLevels,
		},
		{
			// The indexes follow the significance migration, so a database migrated in place gets
			// the covering index rebuilt on significance_level_id rather than on the column that
			// step just dropped.
			version: 7,
			name:    "covering_index",
			apply:   (*DB).ensureCoveringIndex,
		},
		{
			version: 8,
			name:    "listing_index",
			apply:   (*DB).ensureListingIndex,
		},
		{
			version: 9,
			name:    "search_outbox",
			apply:   (*DB).initSearchOutbox,
		},
		{
			version: 10,
			name:    "forgotten_log",
			apply:   (*DB).initTombstones,
		},
		{
			// Server dialects only: the embedded one is single-instance by construction, so there
			// are no peers for it to hold.
			version: 11,
			name:    "instance_registry",
			when:    func(dialect *dialect) bool { return dialect.instanceRegistry },
			apply:   (*DB).initInstances,
		},
		{
			// Last, because it reads memory bodies to populate itself on a store that predates it,
			// and so wants every column and index above it already in place.
			version: 12,
			name:    "content_search",
			when:    func(dialect *dialect) bool { return dialect.contentSearch },
			apply:   (*DB).initContentSearch,
		},
	}
}

// schemaVersion is the newest migration this build declares - the version a store is left at, and
// the ceiling the version gate compares against.
func (d *DB) schemaVersion() int {
	migrations := d.migrations()

	return migrations[len(migrations)-1].version
}

// initSchema brings a store's schema up to date and leaves its version recorded. The single
// initialiser for every dialect: what used to be three functions differed only in the capability
// gates now declared on the list above.
func (d *DB) initSchema() error {
	log.Trace("func() db.initSchema")

	if err := d.configureStorage(); err != nil {
		return err
	}

	if err := d.initSchemaLedger(); err != nil {
		return err
	}

	// Taken before the ledger is read, not after. Two replicas starting together would otherwise
	// race one another's ALTER TABLE, which fails one of them rather than being merely wasteful -
	// and, worse, the loser could read a version, wait, and then migrate against a store the winner
	// had meanwhile moved past. Reading under the lock makes both impossible. A single-writer
	// dialect needs no lock and takes no connection to say so.
	release, err := d.acquireSchemaLock()
	if err != nil {
		return err
	}

	defer release()

	applied, err := d.appliedMigrations()
	if err != nil {
		return err
	}

	if err := d.checkSchemaVersion(applied); err != nil {
		return err
	}

	// The version an operator needs is the one the store ARRIVES at - it is what decides whether a
	// rollback is possible - so the settled figure is logged after the run. An upgrade is announced
	// before it, because the run is the part that can fail and the line before it is the one that
	// says what was being attempted.
	from := newestVersion(applied)

	if from > 0 && from < d.schemaVersion() {
		log.Infof("upgrading the database schema from version %d to %d", from, d.schemaVersion())
	}

	for _, migration := range d.migrations() {
		if migration.when != nil && !migration.when(d.dialect()) {
			continue
		}

		if err := migration.apply(d); err != nil {
			log.Errorf("failed to apply schema migration %d (%s): %s", migration.version, migration.name, err.Error())

			return err
		}

		// Recorded per step rather than in one write at the end, so a run that stops half way leaves
		// a ledger describing what actually happened rather than nothing at all.
		if err := d.recordMigration(migration); err != nil {
			return err
		}
	}

	log.Infof("database schema at version %d (%s)", d.schemaVersion(), d.dialect().name)

	return nil
}

// createCoreTables is migration 1: the two tables holding everything the service stores, plus the
// significance registry both of them reference.
func (d *DB) createCoreTables() error {
	log.Trace("func() db.createCoreTables")

	// One statement at a time: one of the dialects rejects a multi-statement string unless its DSN
	// opts in, and requiring that of every deployment for startup DDL alone is not worth it.
	for _, statement := range append(d.coreSchemaStatements(), d.significanceLevelsDDL()) {
		if _, err := d.sql.Exec(statement); err != nil {
			log.Errorf("failed to initialise database schema: %s", err.Error())

			return err
		}
	}

	return nil
}

// initSchemaLedger creates the migrations table. It is the one piece of schema that cannot be a
// migration itself, and it carries no columns beyond what the gate and an operator need: which
// version, under what name, and when this store first saw it.
func (d *DB) initSchemaLedger() error {
	log.Trace("func() db.initSchemaLedger")

	dialect := d.dialect()

	ddl := `CREATE TABLE IF NOT EXISTS ` + schemaMigrationsTable + ` (
		version    INTEGER PRIMARY KEY,
		name       ` + dialect.idType + ` NOT NULL,
		applied_at ` + dialect.bigintType + ` NOT NULL DEFAULT 0
	)`

	if _, err := d.sql.Exec(ddl); err != nil {
		log.Errorf("failed to create the schema migrations table: %s", err.Error())

		return err
	}

	return nil
}

// appliedMigrations reads the ledger, which the version gate is the only consumer of - no migration
// is skipped on account of being recorded (see the file comment). A store that has never seen the
// ledger reads as empty, which is exactly right: it has no recorded version, so there is no future
// version to refuse.
func (d *DB) appliedMigrations() (map[int]bool, error) {
	rows, err := d.sql.Query(`SELECT version FROM ` + schemaMigrationsTable)
	if err != nil {
		log.Errorf("failed to read the schema migrations table: %s", err.Error())

		return nil, err
	}

	defer func() { _ = rows.Close() }()

	applied := map[int]bool{}

	for rows.Next() {
		var version int

		if err := rows.Scan(&version); err != nil {
			return nil, err
		}

		applied[version] = true
	}

	return applied, rows.Err()
}

// checkSchemaVersion refuses a store recorded at a version this build does not declare.
//
// The comparison is against the HIGHEST version recorded, not against a count or a contiguous run:
// a capability-gated migration is never recorded on a dialect it does not apply to, so a legitimate
// ledger has gaps, and treating a gap as "not yet migrated" would refuse a perfectly current store.
func (d *DB) checkSchemaVersion(applied map[int]bool) error {
	newest := newestVersion(applied)

	if newest <= d.schemaVersion() {
		return nil
	}

	return fmt.Errorf(
		"%w: the store records schema version %d, and this build understands up to %d. It was "+
			"written by a newer Hippocampus; downgrading is not supported (see CHANGELOG.md). Run "+
			"the newer version against it, or restore a backup taken before the upgrade",
		ErrSchemaTooNew, newest, d.schemaVersion(),
	)
}

// verifySchemaVersion is the version gate for the read-only tool opens, which run no DDL at all.
//
// It tolerates the ledger being absent, and that is the difference from the read/write path: a
// store written before the ledger existed has no table to read, and refusing to back-fill a search
// index because of it would be a regression for no safety gained. A store recorded at a FUTURE
// version is still refused - that is the case this exists for.
func (d *DB) verifySchemaVersion() error {
	log.Trace("func() db.verifySchemaVersion")

	applied, err := d.appliedMigrations()
	if err != nil {
		log.Debugf("no schema migrations table to read (a store written before the ledger existed): %s", err.Error())

		return nil
	}

	return d.checkSchemaVersion(applied)
}

// recordMigration writes one ledger row, leaving an existing row's applied_at alone so it keeps
// meaning "when this store first ran this migration" rather than "when it last started".
func (d *DB) recordMigration(migration schemaMigration) error {
	query := d.upsert(upsertSpec{
		table:   schemaMigrationsTable,
		columns: `version, name, applied_at`,
		key:     []string{"version"},
	})

	if _, err := d.sql.Exec(d.rebind(query), migration.version, migration.name, time.Now().UnixNano()); err != nil {
		log.Errorf("failed to record schema migration %d (%s): %s", migration.version, migration.name, err.Error())

		return err
	}

	return nil
}

// acquireSchemaLock serialises the migration run across instances sharing a store, and returns the
// release. See acquireRegistryLock, whose shape this follows: a single-writer dialect needs no lock
// and takes no connection to say so.
func (d *DB) acquireSchemaLock() (func(), error) {
	if d.dialect().singleWriter {
		return func() {}, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), schemaLockTimeout)
	defer cancel()

	conn, err := d.sql.Conn(ctx)
	if err != nil {
		return nil, err
	}

	release, err := d.namedLock(ctx, conn, schemaAdvisoryLockKey, schemaLockName)
	if err != nil {
		_ = conn.Close()

		return nil, err
	}

	return func() {
		release()
		_ = conn.Close()
	}, nil
}

// schemaLockTimeout bounds waiting for another instance's migration run. Generous, because the run
// it waits on may include a table rebuild, and failing early would only restart the loser into the
// same wait.
const schemaLockTimeout = 5 * time.Minute

// schemaAdvisoryLockKey and schemaLockName identify the migration lock, distinct from both the
// single-consolidator instance lock and the significance registry's so none of the three contend.
const (
	schemaAdvisoryLockKey = advisoryLockKey + 2
	schemaLockName        = "hippocampus:schema"
)

// coreColumnNames is the schema-fixture guard's view of migration 2: it adds ten columns, and the
// guard accounts for each separately so that adding an eleventh is not silently covered by the
// declaration the tenth already has.
func coreColumnNames(d *DB) []string {
	migrations := d.coreColumnMigrations()

	names := make([]string, 0, len(migrations))

	for _, migration := range migrations {
		names = append(names, migration.table+"."+migration.column)
	}

	return names
}

// newestVersion is the highest version a ledger records, or 0 for a store that has never seen one.
func newestVersion(applied map[int]bool) int {
	newest := 0

	for version := range applied {
		if version > newest {
			newest = version
		}
	}

	return newest
}

// SchemaReport is what a store says about its own schema, read without opening it for service.
type SchemaReport struct {
	// Dialect names the SQL dialect the store was read as.
	Dialect string

	// Version is the highest migration version the store records, or 0 for one written before
	// versions were recorded at all.
	Version int

	// Supported is the newest version the reading build declares.
	Supported int

	// HasLedger distinguishes a store with no migrations table from one whose table is empty. The
	// first is a store written before the ledger existed - every deployment upgrading to it - and
	// reads as version 0; the second should not occur, and if it does it means something dropped
	// the table.
	HasLedger bool

	// Applied is every recorded migration, oldest first.
	Applied []AppliedMigration

	// Pending names the migrations this build declares for this dialect that the store has not
	// recorded - what an upgrade would do. Empty on a current store.
	Pending []string
}

// AppliedMigration is one recorded ledger row.
type AppliedMigration struct {
	Version   int
	Name      string
	AppliedAt time.Time
}

// Ahead reports whether the store was written by a build newer than the one reading it - the
// condition that makes it unopenable.
func (r *SchemaReport) Ahead() bool {
	return r.Version > r.Supported
}

// InspectSchema reads a store's schema version without opening it for service, so an operator can
// answer "what is this store at" for an instance that is stopped - or one that will not start.
//
// target is the storage directory for the embedded dialect and the DSN for the server ones.
//
// It opens the store itself rather than going through the read-only constructors, and that is the
// point rather than an oversight: those apply the version gate, so against a store written by a
// newer build - the case an operator most needs this for - they refuse to open and there is nothing
// left to report. This reads and never refuses. It takes no lock, runs no DDL, and closes before
// returning, so it is safe beside a live instance.
func InspectSchema(driverName string, target string) (*SchemaReport, error) {
	log.Trace("func() db.InspectSchema")

	d, err := openForInspection(driverName, target)
	if err != nil {
		return nil, err
	}

	defer func() { _ = d.sql.Close() }()

	report := &SchemaReport{
		Dialect:   d.dialect().name,
		Supported: d.schemaVersion(),
	}

	recorded := map[int]bool{}

	// A store with no migrations table is not an error here, it is an answer: it was written before
	// versions were recorded, so nothing is recorded and everything is pending. Anything else - an
	// unreachable server, a permission failure - would be reported the same way, which is why the
	// reason is logged.
	if applied, err := d.readLedger(); err != nil {
		log.Debugf("no schema migrations table to read: %s", err.Error())
	} else {
		report.HasLedger = true
		report.Applied = applied

		for _, migration := range applied {
			recorded[migration.Version] = true

			if migration.Version > report.Version {
				report.Version = migration.Version
			}
		}
	}

	for _, migration := range d.migrations() {
		if migration.when != nil && !migration.when(d.dialect()) {
			continue
		}

		if !recorded[migration.version] {
			report.Pending = append(report.Pending, migration.name)
		}
	}

	return report, nil
}

// openForInspection opens a store read-only with no schema work of any kind - no DDL, no instance
// lock, no version gate. Deliberately returns a bare handle rather than going through the read-only
// constructors: everything they add is something this must not do.
func openForInspection(driverName string, target string) (*DB, error) {
	switch driverName {

	case "sqlite":
		if target == "" {
			return nil, fmt.Errorf("a storage directory is required to inspect a sqlite store")
		}

		// The same read-only DSN NewSQLiteReadOnly uses: mode=ro rejects writes at the SQLite level
		// and no journal-mode pragma is set, since that would write page 1.
		sqlDB, err := sql.Open("sqlite", "file:"+path.Join(target, DataFile)+"?mode=ro&_pragma=busy_timeout(5000)")
		if err != nil {
			return nil, err
		}

		if err := sqlDB.Ping(); err != nil {
			_ = sqlDB.Close()

			return nil, fmt.Errorf("no database at '%s': %w", path.Join(target, DataFile), err)
		}

		return &DB{sql: sqlDB, driver: driverSQLite, readOnly: true}, nil

	case "postgres":
		sqlDB, err := sql.Open("pgx", target)
		if err != nil {
			return nil, err
		}

		return &DB{sql: sqlDB, driver: driverPostgres, readOnly: true}, nil

	case "mysql":
		sqlDB, err := sql.Open("mysql", target)
		if err != nil {
			return nil, err
		}

		return &DB{sql: sqlDB, driver: driverMySQL, readOnly: true}, nil

	}

	return nil, fmt.Errorf("unknown storage driver '%s' (expected 'sqlite', 'postgres', or 'mysql')", driverName)
}

// readLedger reads every recorded migration, oldest first. Errors when the table does not exist,
// which InspectSchema treats as an answer rather than a failure.
func (d *DB) readLedger() ([]AppliedMigration, error) {
	rows, err := d.sql.Query(`SELECT version, name, applied_at FROM ` + schemaMigrationsTable + ` ORDER BY version`)
	if err != nil {
		return nil, err
	}

	defer func() { _ = rows.Close() }()

	var applied []AppliedMigration

	for rows.Next() {
		var (
			migration AppliedMigration
			appliedAt int64
		)

		if err := rows.Scan(&migration.Version, &migration.Name, &appliedAt); err != nil {
			return nil, err
		}

		if appliedAt > 0 {
			migration.AppliedAt = time.Unix(0, appliedAt)
		}

		applied = append(applied, migration)
	}

	return applied, rows.Err()
}
