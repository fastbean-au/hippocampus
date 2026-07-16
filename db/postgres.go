package db

import (
	"context"
	"database/sql"
	"fmt"

	_ "github.com/jackc/pgx/v5/stdlib"
	log "github.com/sirupsen/logrus"
)

// advisoryLockKey is the pg_advisory_lock key identifying "a hippocampus instance owns this
// database". The value is arbitrary but must be stable across versions; it is the ASCII bytes of
// "hipo" so a collision with another application's key is unlikely and it is recognisable in
// pg_locks output.
const advisoryLockKey = int64(0x6869706f)

// NewPostgres opens the Postgres database at the given DSN and prepares it for use. When
// consolidate is true this instance is the one that runs sleep cycles, so it takes the
// session-scoped advisory lock — the single-consolidator lock, held for the life of the process —
// and a second consolidating instance pointed at the same database fails fast here instead of
// silently running concurrent sleep cycles against shared data. When consolidate is false the lock
// is skipped: the instance opens read/write but never consolidates, so it can run alongside the one
// consolidating instance as a horizontally scaled replica. Exactly one instance in a
// deployment must be started with consolidate true.
func NewPostgres(dsn string, consolidate bool) (*DB, error) {
	log.Trace("func() NewPostgres")

	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		log.Errorf("failed to open postgres database: %s", err.Error())

		return nil, err
	}

	// Recycle pooled connections before a server-side idle timeout can close them under the pool
	// (see serverConnMaxLifetime). The pinned lock connection is exempt (never returned while held)
	// and is instead kept alive by the keepalive started below.
	sqlDB.SetConnMaxLifetime(serverConnMaxLifetime)

	d := &DB{sql: sqlDB, driver: driverPostgres}

	if consolidate {
		if err := d.acquireInstanceLock(); err != nil {
			_ = sqlDB.Close()

			return nil, err
		}
	}

	if err := d.initPostgresSchema(); err != nil {
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

// NewPostgresReadOnly opens the Postgres database at the given DSN without taking the instance
// advisory lock, for tooling (the --backfill-search CLI mode) that only reads memories and events
// and so may run alongside a live service instance without violating the single-instance design. It
// runs no schema initialisation: initPostgresSchema's ALTER TABLE ... ADD COLUMN IF NOT EXISTS takes
// a brief ACCESS EXCLUSIVE lock even when it no-ops, which would contend with the very service the
// tool is meant to run beside. Instead it probes that the tables exist, failing fast
// like NewSQLiteReadOnly does for a missing file.
func NewPostgresReadOnly(dsn string) (*DB, error) {
	log.Trace("func() NewPostgresReadOnly")

	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		log.Errorf("failed to open postgres database: %s", err.Error())

		return nil, err
	}

	d := &DB{sql: sqlDB, driver: driverPostgres}

	if err := d.checkReadOnlyTables(); err != nil {
		_ = sqlDB.Close()
		log.Errorf("failed to open postgres database read-only: %s", err.Error())

		return nil, err
	}

	return d, nil
}

// acquireInstanceLock takes the instance advisory lock on a dedicated connection pinned for the
// life of the process. Advisory locks are session-scoped, so the lock must live on a connection
// that is never returned to the pool; Close releases it by closing that connection.
func (d *DB) acquireInstanceLock() error {
	log.Trace("func() db.acquireInstanceLock")

	conn, err := d.sql.Conn(context.Background())
	if err != nil {
		log.Errorf("failed to open advisory lock connection: %s", err.Error())

		return err
	}

	var locked bool

	if err := conn.QueryRowContext(
		context.Background(),
		`SELECT pg_try_advisory_lock($1)`,
		advisoryLockKey,
	).Scan(&locked); err != nil {
		_ = conn.Close()
		log.Errorf("failed to acquire instance advisory lock: %s", err.Error())

		return err
	}

	if !locked {
		_ = conn.Close()

		return fmt.Errorf("another hippocampus instance already holds the advisory lock on this database - the service is single-instance only")
	}

	d.lockConn = conn

	return nil
}

// usedBytesLiveRows estimates the store's live logical size for the server drivers: every row's
// payload bytes plus the same fixed per-row allowance eviction uses when estimating the bytes a
// deletion will free (evictionRowOverheadBytes, covering the remaining columns and index
// entries), so the two measures converge — evicting rows estimated to free N bytes lowers this
// figure by exactly N.
//
// A file-size measure (pg_database_size, information_schema table sizes) would be cheaper to
// read but is wrong here: neither server returns space freed by DELETE to the filesystem —
// Postgres's autovacuum and InnoDB's purge only make it internally reusable — so the reading
// would plateau at its high-water mark, and once past the capacity target, eviction would fire
// every cycle draining live memories without the figure ever dropping. Live-row accounting
// shrinks the moment rows are deleted, matching the SQLite measure's freelist exclusion.
// octet_length (a LENGTH synonym on MySQL) reads a stored value's byte size without loading the
// content, so the cost is one heap scan of the two tables per reading — and UsedBytes is only
// consulted when a byte capacity is configured.
func (d *DB) usedBytesLiveRows(ctx context.Context) (int64, error) {
	log.Trace("func() db.usedBytesLiveRows")

	var used int64

	ctx, cancel := d.opContext(ctx)
	defer cancel()

	if err := d.queryRow(
		ctx,
		`SELECT
			(SELECT COUNT(*) * ? + COALESCE(SUM(octet_length(body)), 0) FROM memories)
			+ (SELECT COUNT(*) * ? + COALESCE(SUM(
				octet_length(name) + octet_length(description) + octet_length(relationships)
			), 0) FROM events)`,
		evictionRowOverheadBytes,
		evictionRowOverheadBytes,
	).Scan(&used); err != nil {
		log.Errorf("failed to estimate used bytes: %s", err.Error())

		return 0, err
	}

	return used, nil
}

func (d *DB) initPostgresSchema() error {
	log.Trace("func() db.initPostgresSchema")

	schema := `
	CREATE TABLE IF NOT EXISTS events (
		id                        TEXT PRIMARY KEY,
		time_start                BIGINT NOT NULL DEFAULT 0,
		time_end                  BIGINT NOT NULL DEFAULT 0,
		significance              INTEGER NOT NULL DEFAULT 0,
		name                      TEXT NOT NULL DEFAULT '',
		description               TEXT NOT NULL DEFAULT '',
		memories_consolidated     BOOLEAN NOT NULL DEFAULT FALSE,
		relationship_significance BIGINT NOT NULL DEFAULT 0,
		relationships             TEXT NOT NULL DEFAULT '[]',
		group_name                TEXT NOT NULL DEFAULT ''
	);

	CREATE TABLE IF NOT EXISTS memories (
		id            TEXT PRIMARY KEY,
		timestamp     BIGINT NOT NULL DEFAULT 0,
		significance  INTEGER NOT NULL DEFAULT 0,
		event_id      TEXT NOT NULL DEFAULT '',
		is_binary     BOOLEAN NOT NULL DEFAULT FALSE,
		time_recalled BIGINT NOT NULL DEFAULT 0,
		recall_count  INTEGER NOT NULL DEFAULT 0,
		is_summary    BOOLEAN NOT NULL DEFAULT FALSE,
		group_name    TEXT NOT NULL DEFAULT '',
		body          BYTEA NOT NULL DEFAULT ''::bytea
	);

	-- Covering index for the consolidation scans: the sleep cycle reads only these columns, so
	-- the scan never touches the pages holding memory bodies.
	CREATE INDEX IF NOT EXISTS idx_memories_consolidation
		ON memories (event_id, timestamp, significance, time_recalled, recall_count);

	-- Postgres supports ADD COLUMN IF NOT EXISTS natively, so columns added after a table's
	-- original CREATE TABLE are migrated in place without SQLite's pragma_table_info probe.
	ALTER TABLE memories ADD COLUMN IF NOT EXISTS is_summary BOOLEAN NOT NULL DEFAULT FALSE;

	-- The column is named group_name rather than group because GROUP is a reserved word.
	ALTER TABLE memories ADD COLUMN IF NOT EXISTS group_name TEXT NOT NULL DEFAULT '';
	ALTER TABLE events ADD COLUMN IF NOT EXISTS group_name TEXT NOT NULL DEFAULT '';
	`

	if _, err := d.sql.Exec(schema); err != nil {
		log.Errorf("failed to initialise postgres database schema: %s", err.Error())

		return err
	}

	return nil
}
