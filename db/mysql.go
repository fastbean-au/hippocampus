package db

import (
	"context"
	"database/sql"
	"fmt"

	_ "github.com/go-sql-driver/mysql"
	log "github.com/sirupsen/logrus"
)

// NewMySQL opens the MySQL database at the given DSN and prepares it for use. When consolidate is
// true this instance is the one that runs sleep cycles, so it takes the session-scoped GET_LOCK
// lock — the single-consolidator lock, held for the life of the process — and a second
// consolidating instance pointed at the same database fails fast here instead of silently running
// concurrent sleep cycles against shared data. When consolidate is false the lock is skipped: the
// instance opens read/write but never consolidates, so it can run alongside the one consolidating
// instance as a horizontally scaled replica. Exactly one instance in a deployment must
// be started with consolidate true. Requires MySQL 8.0.20+ (the upserts use the ON DUPLICATE KEY
// UPDATE row alias) and a DSN that names a database schema.
func NewMySQL(dsn string, consolidate bool) (*DB, error) {
	log.Trace("func() NewMySQL")

	sqlDB, err := sql.Open("mysql", dsn)
	if err != nil {
		log.Errorf("failed to open mysql database: %s", err.Error())

		return nil, err
	}

	// Recycle pooled connections before MySQL's wait_timeout can close them under the pool (see
	// serverConnMaxLifetime; go-sql-driver's README recommends exactly this). The pinned lock
	// connection is exempt (never returned while held) and is kept alive by the keepalive below.
	sqlDB.SetConnMaxLifetime(serverConnMaxLifetime)

	d := &DB{sql: sqlDB, driver: driverMySQL}

	if consolidate {
		if err := d.acquireMySQLInstanceLock(); err != nil {
			_ = sqlDB.Close()

			return nil, err
		}
	}

	if err := d.initMySQLSchema(); err != nil {
		if d.lockConn != nil {
			_ = d.lockConn.Close()
		}

		_ = sqlDB.Close()

		return nil, err
	}

	// A no-op when the lock was not taken (consolidate false), so a replica starts no keepalive.
	d.startLockKeepalive()

	return d, nil
}

// NewMySQLReadOnly opens the MySQL database at the given DSN without taking the instance lock,
// for tooling (the --backfill-search CLI mode) that only reads memories and events and so may run
// alongside a live service instance without violating the single-instance design. It runs no schema
// initialisation: against a database created by an older service version, initMySQLSchema's
// setMySQLColumnCollationIfNeeded would run an ALTER TABLE ... MODIFY - a potentially long table
// rebuild - from a tool documented as safe to run beside a live instance. Instead it
// probes that the tables exist, failing fast like NewSQLiteReadOnly does for a missing file.
func NewMySQLReadOnly(dsn string) (*DB, error) {
	log.Trace("func() NewMySQLReadOnly")

	sqlDB, err := sql.Open("mysql", dsn)
	if err != nil {
		log.Errorf("failed to open mysql database: %s", err.Error())

		return nil, err
	}

	d := &DB{sql: sqlDB, driver: driverMySQL}

	if err := d.checkReadOnlyTables(); err != nil {
		_ = sqlDB.Close()
		log.Errorf("failed to open mysql database read-only: %s", err.Error())

		return nil, err
	}

	return d, nil
}

// acquireMySQLInstanceLock takes the instance lock on a dedicated connection pinned for the life
// of the process, mirroring the Postgres advisory lock. GET_LOCK locks are session-scoped, so the
// lock must live on a connection that is never returned to the pool; Close releases it by closing
// that connection. Named locks are server-wide rather than per-schema, so the current database's
// name is baked into the lock name — two instances against different schemas on the same server
// must not exclude each other.
func (d *DB) acquireMySQLInstanceLock() error {
	log.Trace("func() db.acquireMySQLInstanceLock")

	conn, err := d.sql.Conn(context.Background())
	if err != nil {
		log.Errorf("failed to open instance lock connection: %s", err.Error())

		return err
	}

	// GET_LOCK returns 1 when acquired, 0 when the zero-second timeout expires because another
	// session holds it, and NULL on error (e.g. no database selected in the DSN).
	var locked sql.NullInt64

	if err := conn.QueryRowContext(
		context.Background(),
		`SELECT GET_LOCK(CONCAT('hippocampus:', DATABASE()), 0)`,
	).Scan(&locked); err != nil {
		_ = conn.Close()
		log.Errorf("failed to acquire instance lock: %s", err.Error())

		return err
	}

	if !locked.Valid {
		_ = conn.Close()

		return fmt.Errorf("failed to acquire instance lock - the DSN must name a database schema")
	}

	if locked.Int64 != 1 {
		_ = conn.Close()

		return fmt.Errorf("another hippocampus instance already holds the instance lock on this database - the service is single-instance only")
	}

	d.lockConn = conn

	return nil
}

func (d *DB) initMySQLSchema() error {
	log.Trace("func() db.initMySQLSchema")

	// Statements run one at a time: go-sql-driver rejects multi-statement strings unless the DSN
	// opts in, and requiring that of every deployment for startup DDL alone isn't worth it.
	//
	// Ids are VARCHAR rather than TEXT because MySQL cannot use an unbounded TEXT column as a
	// primary key or in a full-column index; 255 characters comfortably holds the generated UUIDs
	// and stays inside InnoDB's utf8mb4 index-width limit. TEXT and LONGBLOB columns carry no
	// DEFAULT (MySQL only allows expression defaults on them) — every insert in the package
	// supplies those columns explicitly.
	//
	// id, event_id, and group_name are COLLATE utf8mb4_bin so they compare byte-for-byte, matching
	// SQLite and Postgres. Under MySQL's default collation (utf8mb4_0900_ai_ci) ids differing only
	// in case or accent would be the same key — client ids "abc"/"ABC" would collide (a duplicate
	// key on create, a silent merge on import) and keyset pagination would walk a different order —
	// so the same archive would change record identity across drivers. See mysqlBinaryCollation.
	statements := []string{
		`CREATE TABLE IF NOT EXISTS events (
			id                        VARCHAR(255) COLLATE utf8mb4_bin PRIMARY KEY,
			time_start                BIGINT NOT NULL DEFAULT 0,
			time_end                  BIGINT NOT NULL DEFAULT 0,
			significance              INTEGER NOT NULL DEFAULT 0,
			name                      TEXT NOT NULL,
			description               TEXT NOT NULL,
			memories_consolidated     BOOLEAN NOT NULL DEFAULT FALSE,
			relationship_significance BIGINT NOT NULL DEFAULT 0,
			relationships             TEXT NOT NULL,
			group_name                VARCHAR(255) COLLATE utf8mb4_bin NOT NULL DEFAULT ''
		)`,

		`CREATE TABLE IF NOT EXISTS memories (
			id            VARCHAR(255) COLLATE utf8mb4_bin PRIMARY KEY,
			timestamp     BIGINT NOT NULL DEFAULT 0,
			significance  INTEGER NOT NULL DEFAULT 0,
			event_id      VARCHAR(255) COLLATE utf8mb4_bin NOT NULL DEFAULT '',
			is_binary     BOOLEAN NOT NULL DEFAULT FALSE,
			time_recalled BIGINT NOT NULL DEFAULT 0,
			recall_count  INTEGER NOT NULL DEFAULT 0,
			is_summary    BOOLEAN NOT NULL DEFAULT FALSE,
			group_name    VARCHAR(255) COLLATE utf8mb4_bin NOT NULL DEFAULT '',
			body          LONGBLOB NOT NULL
		)`,
	}

	for _, statement := range statements {
		if _, err := d.sql.Exec(statement); err != nil {
			log.Errorf("failed to initialise mysql database schema: %s", err.Error())

			return err
		}
	}

	// Covering index for the consolidation scans: the sleep cycle reads only these columns, so
	// the scan never touches the pages holding memory bodies. MySQL has no CREATE INDEX IF NOT
	// EXISTS, so existence is probed first.
	if err := d.createMySQLIndexIfMissing(
		"memories",
		"idx_memories_consolidation",
		`CREATE INDEX idx_memories_consolidation
			ON memories (event_id, timestamp, significance, time_recalled, recall_count)`,
	); err != nil {
		return err
	}

	if err := d.addColumnIfMissing("memories", "is_summary", "BOOLEAN NOT NULL DEFAULT FALSE"); err != nil {
		return err
	}

	// The column is named group_name rather than group because GROUP is a reserved word; VARCHAR
	// rather than TEXT so the column stays indexable, matching the ids.
	if err := d.addColumnIfMissing("memories", "group_name", "VARCHAR(255) NOT NULL DEFAULT ''"); err != nil {
		return err
	}

	if err := d.addColumnIfMissing("events", "group_name", "VARCHAR(255) NOT NULL DEFAULT ''"); err != nil {
		return err
	}

	// Migrate the id/event_id/group_name collation on databases created before it was pinned to
	// utf8mb4_bin. The CREATE TABLE above already collates new databases correctly, so these are
	// no-ops there. definition is the full post-MODIFY column spec (COLLATE immediately after the
	// type, ahead of any NOT NULL/DEFAULT), matching the CREATE TABLE definitions.
	collations := []struct {
		table      string
		column     string
		definition string
	}{
		{"events", "id", "VARCHAR(255) COLLATE " + mysqlBinaryCollation},
		{"events", "group_name", "VARCHAR(255) COLLATE " + mysqlBinaryCollation + " NOT NULL DEFAULT ''"},
		{"memories", "id", "VARCHAR(255) COLLATE " + mysqlBinaryCollation},
		{"memories", "event_id", "VARCHAR(255) COLLATE " + mysqlBinaryCollation + " NOT NULL DEFAULT ''"},
		{"memories", "group_name", "VARCHAR(255) COLLATE " + mysqlBinaryCollation + " NOT NULL DEFAULT ''"},
	}

	for _, c := range collations {
		if err := d.setMySQLColumnCollationIfNeeded(c.table, c.column, c.definition); err != nil {
			return err
		}
	}

	return nil
}

// mysqlBinaryCollation is the collation pinned on the id/event_id/group_name columns so they
// compare byte-for-byte like SQLite and Postgres, rather than under MySQL's case- and
// accent-insensitive server default. See initMySQLSchema.
const mysqlBinaryCollation = "utf8mb4_bin"

// setMySQLColumnCollationIfNeeded rewrites a column to mysqlBinaryCollation when it is not already
// set to it, standing in for the ADD COLUMN IF NOT EXISTS-style in-place migration the other
// dialects would express natively — here probing information_schema.columns for the current
// COLLATION_NAME, mirroring addColumnIfMissing. definition must carry the target COLLATE clause so
// the MODIFY sets it in one statement.
func (d *DB) setMySQLColumnCollationIfNeeded(table string, column string, definition string) error {
	log.Trace("func() db.setMySQLColumnCollationIfNeeded")

	var current sql.NullString

	if err := d.sql.QueryRow(
		`SELECT collation_name FROM information_schema.columns
		WHERE table_schema = DATABASE() AND table_name = ? AND column_name = ?`,
		table,
		column,
	).Scan(&current); err != nil {
		log.Errorf("failed to read collation for column '%s' on table '%s': %s", column, table, err.Error())

		return err
	}

	if current.Valid && current.String == mysqlBinaryCollation {
		return nil
	}

	log.Infof("migrating column '%s' on table '%s' to collation '%s'", column, table, mysqlBinaryCollation)

	if _, err := d.sql.Exec(`ALTER TABLE ` + table + ` MODIFY ` + column + ` ` + definition); err != nil {
		log.Errorf("failed to migrate collation for column '%s' on table '%s': %s", column, table, err.Error())

		return err
	}

	return nil
}

// createMySQLIndexIfMissing creates the index when it is not already present, standing in for the
// CREATE INDEX IF NOT EXISTS the other dialects support natively.
func (d *DB) createMySQLIndexIfMissing(table string, index string, definition string) error {
	log.Trace("func() db.createMySQLIndexIfMissing")

	var count int

	if err := d.sql.QueryRow(
		`SELECT COUNT(*) FROM information_schema.statistics
		WHERE table_schema = DATABASE() AND table_name = ? AND index_name = ?`,
		table,
		index,
	).Scan(&count); err != nil {
		log.Errorf("failed to check for index '%s' on table '%s': %s", index, table, err.Error())

		return err
	}

	if count > 0 {
		return nil
	}

	if _, err := d.sql.Exec(definition); err != nil {
		log.Errorf("failed to create index '%s' on table '%s': %s", index, table, err.Error())

		return err
	}

	return nil
}
