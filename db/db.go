package db

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
	_ "modernc.org/sqlite"

	"github.com/fastbean-au/hippocampus/types"
)

// DataFile is the name of the SQLite database file within the storage directory.
const DataFile = "hippocampus.db"

// instanceLockCheckInterval is how often the server drivers ping the pinned lock connection. The
// ping doubles as activity keeping the session from being reaped by a server-side idle timeout
// (e.g. MySQL's wait_timeout), and detects a session that died anyway so the lock can be retaken
// or the process can fail-stop before a second instance runs concurrently against the same data.
const instanceLockCheckInterval = 60 * time.Second

// instanceLockCheckTimeout bounds a single keepalive ping and reacquisition attempt.
const instanceLockCheckTimeout = 10 * time.Second

// serverConnMaxLifetime caps how long a pooled connection (server drivers) is reused before being
// recycled, kept well under common server idle timeouts so the pool never hands out a connection
// the server has already closed. It does not reap the pinned lock connection, which is never
// returned to the pool while held (go-sql-driver's README recommends this be under wait_timeout).
const serverConnMaxLifetime = 5 * time.Minute

// driver identifies which SQL dialect the DB speaks. Both backends run through database/sql, and
// nearly all of the query and consolidation logic is identical, so the dialect is a field on one
// shared implementation rather than a second copy of it. The genuinely divergent pieces — DDL,
// placeholder style, and the compaction/size-accounting methods — branch on this.
type driver int

const (
	driverSQLite driver = iota
	driverPostgres
	driverMySQL
)

type DB struct {
	sql    *sql.DB
	driver driver

	// walFilePath is the on-disk WAL file's path, empty for the server drivers and for the
	// in-memory database used by tests (neither has one). Set once in New and never changed.
	walFilePath string

	// lockConn pins the session holding the instance lock (a Postgres advisory lock or a MySQL
	// GET_LOCK lock) for the lifetime of the process; both lock kinds are session-scoped, so the
	// lock would silently vanish if its connection were returned to the pool. Nil for the SQLite
	// driver.
	lockConn *sql.Conn

	// memoryDeleteObserver, when set, is invoked after a consolidation/eviction transaction
	// commits with the ids of the memory rows actually deleted, so the optional secondary search
	// index can be told about deletions the RPC layer never sees. Nil means no propagation. Set
	// once at startup, before serving, and never changed.
	memoryDeleteObserver func(ids []string)

	// readOnly marks a database opened for read-only tooling (NewSQLiteReadOnly, for
	// --backfill-search). Preserve becomes a no-op so Close does not attempt a WAL checkpoint or
	// incremental vacuum against a database a live service instance may own. SQLite only.
	readOnly bool

	// stopKeepalive / keepaliveStopped coordinate the instance-lock keepalive goroutine (server
	// drivers only; nil otherwise). Close signals stopKeepalive and waits for keepaliveStopped
	// before releasing lockConn, so the keepalive never races Close over lockConn.
	stopKeepalive    chan struct{}
	keepaliveStopped chan struct{}
}

// startLockKeepalive launches the goroutine that keeps the instance lock alive and healthy for a
// server driver. It is a no-op when there is no lock connection (SQLite, and the read-only opens).
// The goroutine pings the pinned lock connection on a fixed interval and, if the lock is confirmed
// lost and cannot be retaken, fail-stops the process rather than let a second instance run
// concurrently against the same database.
func (d *DB) startLockKeepalive() {
	if d.lockConn == nil {
		return
	}

	d.stopKeepalive = make(chan struct{})
	d.keepaliveStopped = make(chan struct{})

	go func() {
		defer close(d.keepaliveStopped)

		ticker := time.NewTicker(instanceLockCheckInterval)
		defer ticker.Stop()

		for {
			select {

			case <-d.stopKeepalive:
				return

			case <-ticker.C:
				if err := d.verifyInstanceLock(); err != nil {
					log.Fatalf("lost the single-instance lock and could not reacquire it, exiting to avoid running concurrently with another instance: %s", err.Error())
				}
			}
		}
	}()
}

// verifyInstanceLock pings the pinned lock connection; if it has died - taking the session-scoped
// lock with it - it attempts exactly one reacquisition on a fresh pinned connection. It returns an
// error only when the lock is confirmed unheld and cannot be retaken (another instance holds it,
// or the database is unreachable), which the keepalive treats as fatal: continuing would risk two
// instances mutating the same database. A healthy connection, or a successful reacquisition,
// returns nil. Only ever called from the keepalive goroutine (and directly by tests), so its
// mutation of lockConn does not race Close, which stops the goroutine before touching lockConn.
func (d *DB) verifyInstanceLock() error {
	if d.lockConn == nil {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), instanceLockCheckTimeout)
	defer cancel()

	if err := d.lockConn.PingContext(ctx); err == nil {
		return nil
	}

	log.Warn("instance lock connection is unhealthy - attempting to reacquire the lock")

	// The old session (and its lock) are gone; drop the dead connection and try to retake the lock
	// on a fresh pinned one via the driver's own acquisition path.
	_ = d.lockConn.Close()
	d.lockConn = nil

	switch d.driver {

	case driverPostgres:
		return d.acquireInstanceLock()

	case driverMySQL:
		return d.acquireMySQLInstanceLock()
	}

	return nil
}

// SetMemoryDeleteObserver registers the function called with the ids of memories deleted by the
// consolidation and eviction scans. It is deliberately on the concrete DB rather than the Store
// interface: it exists solely for the optional search index, and other backends are free to
// provide the same propagation differently.
func (d *DB) SetMemoryDeleteObserver(fn func(ids []string)) {
	d.memoryDeleteObserver = fn
}

// rebind converts ?-style placeholders to the $N style Postgres requires. Queries throughout the
// package are written with ?, the shared style; SQLite consumes them as-is. None of the package's
// SQL carries a literal '?' inside a string, so a plain character scan is sufficient.
func (d *DB) rebind(query string) string {
	if d.driver != driverPostgres {
		return query
	}

	var b strings.Builder
	n := 0

	for i := 0; i < len(query); i++ {
		if query[i] != '?' {
			b.WriteByte(query[i])

			continue
		}

		n++
		b.WriteByte('$')
		b.WriteString(strconv.Itoa(n))
	}

	return b.String()
}

// exec, query, and queryRow wrap the underlying database handle, rebinding placeholders for the
// active dialect. All SQL in the package goes through these (or rebinds explicitly when running
// inside a transaction).
func (d *DB) exec(query string, args ...any) (sql.Result, error) {
	return d.sql.Exec(d.rebind(query), args...)
}

func (d *DB) query(query string, args ...any) (*sql.Rows, error) {
	return d.sql.Query(d.rebind(query), args...)
}

func (d *DB) queryRow(query string, args ...any) *sql.Row {
	return d.sql.QueryRow(d.rebind(query), args...)
}

// MemoryConsolidationCandidate carries everything the consolidation decision needs to know about
// a memory and its associated event.
type MemoryConsolidationCandidate struct {
	EventSignificance        int32
	MemorySignificance       int32
	RelationshipSignificance int64
	Timestamp                int64
	TimeRecalled             int64
	RecallCount              int32
}

// EventConsolidationCandidate carries everything the consolidation decision needs to know about
// an event that has no associated memories.
type EventConsolidationCandidate struct {
	Significance             int32
	RelationshipSignificance int64
	TimeStart                int64
	TimeEnd                  int64
}

// MemoryFilter narrows a GetMemories query. A zero value on any field leaves that dimension
// unconstrained; Group matches the memory's group label exactly.
type MemoryFilter struct {
	TimeStampMin    int64
	TimeStampMax    int64
	SignificanceMin int32
	SignificanceMax int32
	Group           string
	OrderBy         string
	Limit           int
	Offset          int
}

// EventFilter narrows a GetEvents query. A zero value on any field leaves that dimension
// unconstrained; Group matches the event's group label exactly.
type EventFilter struct {
	TimeStartMin    int64
	TimeStartMax    int64
	TimeEndMin      int64
	TimeEndMax      int64
	SignificanceMin int32
	SignificanceMax int32
	Group           string
	OrderBy         string
	Limit           int
	Offset          int
}

// SummarizationCandidate identifies an event whose memories have accumulated enough, and gone
// quiet for long enough, to be worth condensing into a single summary memory via
// ReplaceMemoriesWithSummary.
type SummarizationCandidate struct {
	EventId     string
	EventName   string
	MemoryCount int
}

type Server interface {
	ShouldConsolidateMemory(MemoryConsolidationCandidate) bool
	ShouldConsolidateEvent(EventConsolidationCandidate) bool

	// MemoryValue returns the memory's current decayed value, used by capacity eviction to rank
	// memories from least to most valuable.
	MemoryValue(MemoryConsolidationCandidate) float64
}

// Store is the storage-backend contract hippocampus.Server and stats.Start depend on, satisfied
// today by *DB. It covers exactly the methods those callers currently use, so a second backend
// (e.g. a client/server SQL database) can be swapped in without touching call sites.
//
// UsedBytes, WALBytes, and Preserve carry SQLite-specific semantics today (PRAGMA page accounting,
// an on-disk WAL file, incremental vacuum). A non-SQLite implementation is free to give them
// different mechanics as long as UsedBytes/Preserve keep meaning "logical bytes used"/"compact",
// and WALBytes returns 0 where there is no comparable on-disk WAL to measure (as it already does
// for the in-memory database used by tests).
type Store interface {
	CreateMemory(memory types.Memory) (string, error)
	UpdateMemory(memory types.Memory) (bool, error)
	DeleteMemories(ids []string) (int, error)
	RecallMemories(ids []string) (*[]types.Memory, error)
	ReplaceMemoriesWithSummary(eventId string, summary types.Memory) (int, error)
	GetMemories(filter MemoryFilter) (*[]types.Memory, error)
	GetMemoriesByEventId(eventId string) (*[]types.Memory, error)
	GetMemoriesByEventIds(eventIds []string) (*[]types.Memory, error)
	GetMemoriesByIds(ids []string) (*[]types.Memory, error)
	CountMemories() (int, int)
	CountMemoriesFiltered(filter MemoryFilter) (int, error)

	CreateEvent(event types.Event) (string, error)
	UpdateEvent(event types.Event) (bool, error)
	DeleteEvent(id string) (bool, error)
	EventExists(id string) (bool, error)
	GetEvent(id string) (*types.Event, error)
	GetEvents(filter EventFilter) (*[]types.Event, error)
	CountEvents() int
	CountEventsFiltered(filter EventFilter) (int, error)
	MergeEventMemories(toEventId string, fromEventId string) error
	DeleteEventMemories(eventId string) (int, error)
	UnsetMemoriesEventId(eventId string) (int, error)
	CalculateSignificancePercentile(percent float64) (float64, error)

	ConsolidateMemories(s Server) (int, error)
	ConsolidateEventMemories(s Server) (int, int, int, error)
	ConsolidateEvents(s Server) (int, error)
	EvictMemories(s Server, freeBytes int64) (int, int, int64, error)
	FindSummarizationCandidates(minMemories int, maxTimestamp int64, limit int) ([]SummarizationCandidate, error)

	// Export/transfer surface (see transfer.go): keyset pagination over the whole store,
	// full-state import upserts, and the manifest-scoped clear primitives.
	GetMemoriesPage(afterId string, limit int) ([]types.Memory, error)
	GetEventsPage(afterId string, limit int) ([]types.Event, error)
	ImportMemories(memories []types.Memory) (int, error)
	ImportEvents(events []types.Event) (int, error)
	ClearMemories(snapshots []MemoryRecallSnapshot) (int, error)
	DeleteEventIfEmpty(id string) (bool, error)

	UsedBytes() (int64, error)
	WALBytes() (int64, error)
	Preserve() error
	Purge() error
	Close() error
}

// Compile-time check that *DB satisfies Store.
var _ Store = (*DB)(nil)

// New opens (creating if necessary) the SQLite database in the given directory. An empty
// directory selects an in-memory database, used by tests. The database runs in WAL mode, so
// every acknowledged write is durable; there is no snapshot cycle.
func New(directory string) (*DB, error) {
	log.Trace("func() NewDB")

	dsn := "file::memory:"

	var walFilePath string

	if directory != "" {
		if _, err := os.Stat(directory); os.IsNotExist(err) {
			log.Tracef("creating database directory '%s'", directory)

			if err := os.MkdirAll(directory, 0740); err != nil {
				log.Errorf("failed to create database data directory '%s': %s", directory, err)

				return nil, err
			}
		}

		dataFilePath := path.Join(directory, DataFile)
		dsn = "file:" + dataFilePath
		walFilePath = dataFilePath + "-wal"
	}

	dsn += "?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)"

	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		log.Errorf("failed to open database: %s", err.Error())

		return nil, err
	}

	// A single connection keeps the in-memory database alive for its whole lifetime and, for the
	// file-backed database, sidesteps writer contention entirely (the service is single-instance
	// by design).
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)

	d := &DB{sql: sqlDB, walFilePath: walFilePath}

	if err := d.initSchema(); err != nil {
		_ = sqlDB.Close()

		return nil, err
	}

	return d, nil
}

// NewSQLiteReadOnly opens the SQLite database in the given directory read-only, for tooling (the
// --backfill-search CLI mode) that only reads and so may run alongside a live service instance
// without contending for writes. Unlike New it opens with mode=ro (writes are rejected by SQLite),
// runs no initSchema (no DDL or VACUUM), and skips Preserve on Close (no WAL checkpoint or
// incremental vacuum) - all three of which would otherwise write to a database the live service
// owns. Mirrors NewPostgresReadOnly/NewMySQLReadOnly. The database must already exist: a read-only
// open cannot create it, and there would be nothing to index from anyway.
func NewSQLiteReadOnly(directory string) (*DB, error) {
	log.Trace("func() NewSQLiteReadOnly")

	if directory == "" {
		return nil, fmt.Errorf("a storage directory is required for a read-only sqlite open")
	}

	// mode=ro rejects writes at the SQLite level; busy_timeout lets a read briefly wait out a live
	// writer's lock rather than failing immediately. No journal_mode pragma - it would try to write
	// page 1; a mode=ro connection reads a WAL database through the existing -wal/-shm files.
	dsn := "file:" + path.Join(directory, DataFile) + "?mode=ro&_pragma=busy_timeout(5000)"

	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		log.Errorf("failed to open database read-only: %s", err.Error())

		return nil, err
	}

	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)

	// sql.Open is lazy; Ping forces the file open so a missing database fails here rather than on
	// the first query.
	if err := sqlDB.Ping(); err != nil {
		_ = sqlDB.Close()
		log.Errorf("failed to open database read-only: %s", err.Error())

		return nil, err
	}

	return &DB{sql: sqlDB, driver: driverSQLite, readOnly: true}, nil
}

func (d *DB) initSchema() error {
	log.Trace("func() db.initSchema")

	// auto_vacuum can only be changed while the database is completely empty, and the
	// journal-mode pragma in the DSN has already initialised page 1 by the time this runs, so
	// setting the pragma alone never takes effect. Setting it and then running VACUUM rebuilds
	// the file with the pending mode; without it every incremental_vacuum in Preserve is a
	// silent no-op and the file never shrinks.
	if _, err := d.sql.Exec(`PRAGMA auto_vacuum = INCREMENTAL`); err != nil {
		log.Errorf("failed to set auto_vacuum: %s", err.Error())

		return err
	}

	var autoVacuum int
	if err := d.sql.QueryRow(`PRAGMA auto_vacuum`).Scan(&autoVacuum); err != nil {
		log.Errorf("failed to read auto_vacuum: %s", err.Error())

		return err
	}

	if autoVacuum != 2 {
		log.Info("rebuilding database to enable incremental auto_vacuum")

		if _, err := d.sql.Exec(`VACUUM`); err != nil {
			log.Errorf("failed to vacuum database to enable auto_vacuum: %s", err.Error())

			return err
		}
	}

	schema := `
	CREATE TABLE IF NOT EXISTS events (
		id                        TEXT PRIMARY KEY,
		time_start                INTEGER NOT NULL DEFAULT 0,
		time_end                  INTEGER NOT NULL DEFAULT 0,
		significance              INTEGER NOT NULL DEFAULT 0,
		name                      TEXT NOT NULL DEFAULT '',
		description               TEXT NOT NULL DEFAULT '',
		memories_consolidated     INTEGER NOT NULL DEFAULT 0,
		relationship_significance INTEGER NOT NULL DEFAULT 0,
		relationships             TEXT NOT NULL DEFAULT '[]',
		group_name                TEXT NOT NULL DEFAULT ''
	);

	CREATE TABLE IF NOT EXISTS memories (
		id            TEXT PRIMARY KEY,
		timestamp     INTEGER NOT NULL DEFAULT 0,
		significance  INTEGER NOT NULL DEFAULT 0,
		event_id      TEXT NOT NULL DEFAULT '',
		is_binary     INTEGER NOT NULL DEFAULT 0,
		time_recalled INTEGER NOT NULL DEFAULT 0,
		recall_count  INTEGER NOT NULL DEFAULT 0,
		is_summary    INTEGER NOT NULL DEFAULT 0,
		group_name    TEXT NOT NULL DEFAULT '',
		body          BLOB NOT NULL DEFAULT x''
	);

	-- Covering index for the consolidation scans: the sleep cycle reads only these columns, so
	-- the scan never touches the pages holding memory bodies.
	CREATE INDEX IF NOT EXISTS idx_memories_consolidation
		ON memories (event_id, timestamp, significance, time_recalled, recall_count);
	`

	if _, err := d.sql.Exec(schema); err != nil {
		log.Errorf("failed to initialise database schema: %s", err.Error())

		return err
	}

	if err := d.addColumnIfMissing("memories", "is_summary", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}

	// The column is named group_name rather than group because GROUP is a reserved word in every
	// dialect the service speaks.
	if err := d.addColumnIfMissing("memories", "group_name", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}

	if err := d.addColumnIfMissing("events", "group_name", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}

	return nil
}

// checkReadOnlyTables verifies the events and memories tables are present without running any DDL,
// so a read-only tool open (NewPostgresReadOnly/NewMySQLReadOnly) fails fast against a database the
// service has never initialised - mirroring NewSQLiteReadOnly's fail-fast on a missing file -
// instead of running the schema initialiser's ALTER TABLE probes, which can take locks (Postgres'
// brief ACCESS EXCLUSIVE) or trigger a long rebuild (MySQL's collation MODIFY) against a live
// service the tool is meant to run beside. A trivial no-row SELECT errors on a
// missing table on both server dialects, and doubles as a connectivity check.
func (d *DB) checkReadOnlyTables() error {
	log.Trace("func() db.checkReadOnlyTables")

	for _, table := range []string{"events", "memories"} {
		rows, err := d.sql.Query(`SELECT 1 FROM ` + table + ` WHERE 1 = 0`)
		if err != nil {
			return fmt.Errorf("read-only open: table '%s' is not available (has the service initialised this database?): %w", table, err)
		}

		_ = rows.Close()
	}

	return nil
}

// addColumnIfMissing adds a column to an existing table when it is not already present, so a
// schema change introduced after the table was first created still applies to a database
// written by an older version of the service. CREATE TABLE IF NOT EXISTS alone would silently
// skip the change for any table that already exists. Used by the SQLite and MySQL schema
// initialisers; Postgres supports ADD COLUMN IF NOT EXISTS natively.
func (d *DB) addColumnIfMissing(table string, column string, definition string) error {
	log.Trace("func() db.addColumnIfMissing")

	probe := `SELECT name FROM pragma_table_info(?) WHERE name = ?`
	if d.driver == driverMySQL {
		probe = `SELECT column_name FROM information_schema.columns
			WHERE table_schema = DATABASE() AND table_name = ? AND column_name = ?`
	}

	rows, err := d.sql.Query(probe, table, column)
	if err != nil {
		log.Errorf("failed to check for column '%s' on table '%s': %s", column, table, err.Error())

		return err
	}

	exists := rows.Next()

	if err := rows.Err(); err != nil {
		_ = rows.Close()
		log.Errorf("failed to check for column '%s' on table '%s': %s", column, table, err.Error())

		return err
	}

	_ = rows.Close()

	if exists {
		return nil
	}

	log.Infof("adding column '%s' to table '%s'", column, table)

	if _, err := d.sql.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + column + ` ` + definition); err != nil {
		log.Errorf("failed to add column '%s' to table '%s': %s", column, table, err.Error())

		return err
	}

	return nil
}

// UsedBytes returns the store's logical live size — the figure compared against the byte
// capacity target, so space already freed by consolidation but not yet reclaimed must not count
// against it. For SQLite that is the database's pages excluding the freelist (the size the file
// would have after a full compaction); for the server drivers it is estimated from the live rows
// themselves (see usedBytesLiveRows), since no file-size measure on either server ever shrinks
// after deletes.
func (d *DB) UsedBytes() (int64, error) {
	log.Trace("func() db.UsedBytes")

	if d.driver != driverSQLite {
		return d.usedBytesLiveRows()
	}

	var pageCount, freelistCount, pageSize int64

	if err := d.sql.QueryRow(`PRAGMA page_count`).Scan(&pageCount); err != nil {
		log.Errorf("failed to read page_count: %s", err.Error())

		return 0, err
	}

	if err := d.sql.QueryRow(`PRAGMA freelist_count`).Scan(&freelistCount); err != nil {
		log.Errorf("failed to read freelist_count: %s", err.Error())

		return 0, err
	}

	if err := d.sql.QueryRow(`PRAGMA page_size`).Scan(&pageSize); err != nil {
		log.Errorf("failed to read page_size: %s", err.Error())

		return 0, err
	}

	return (pageCount - freelistCount) * pageSize, nil
}

// WALBytes returns the current size in bytes of the on-disk WAL file, or 0 for the server
// drivers and the in-memory database used by tests (neither has a client-visible WAL file).
// Unlike UsedBytes this reads the filesystem directly rather than querying the database, so
// checking it does not contend with the single connection.
func (d *DB) WALBytes() (int64, error) {
	log.Trace("func() db.WALBytes")

	if d.walFilePath == "" {
		return 0, nil
	}

	info, err := os.Stat(d.walFilePath)
	if os.IsNotExist(err) {
		return 0, nil
	}

	if err != nil {
		log.Errorf("failed to stat WAL file '%s': %s", d.walFilePath, err.Error())

		return 0, err
	}

	return info.Size(), nil
}

// Preserve is called at the end of each sleep cycle. For SQLite, WAL mode makes every write
// durable as it happens, so this only compacts: the WAL is checkpointed and truncated, and pages
// freed by consolidation are returned to the filesystem. For the server drivers it is a no-op —
// Postgres's autovacuum and InnoDB's background purge already reclaim dead rows continuously,
// and imitating SQLite's app-driven compaction (VACUUM FULL, OPTIMIZE TABLE) would take an
// exclusive table lock for no benefit.
func (d *DB) Preserve() error {
	log.Trace("func() db.Preserve")

	if d.driver != driverSQLite || d.readOnly {
		return nil
	}

	if _, err := d.sql.Exec(`PRAGMA incremental_vacuum`); err != nil {
		log.Errorf("failed to run incremental vacuum: %s", err.Error())

		return err
	}

	if _, err := d.sql.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		log.Errorf("failed to checkpoint WAL: %s", err.Error())

		return err
	}

	return nil
}

// Close checkpoints and closes the database. For the server drivers it also releases the
// instance lock by closing the session that holds it.
func (d *DB) Close() error {
	log.Trace("func() db.Close")

	if err := d.Preserve(); err != nil {
		log.Errorf("failed to preserve database before closing: %s", err.Error())
	}

	// Stop the instance-lock keepalive and wait for it to exit before releasing the lock
	// connection, so it never races Close over lockConn nor tries to reacquire during shutdown.
	if d.stopKeepalive != nil {
		close(d.stopKeepalive)
		<-d.keepaliveStopped
		d.stopKeepalive = nil
	}

	if d.lockConn != nil {
		if err := d.lockConn.Close(); err != nil {
			log.Errorf("failed to close instance lock connection: %s", err.Error())
		}

		// Cleared so a second Close (e.g. a test's deferred cleanup after an explicit close)
		// doesn't try to close the connection again.
		d.lockConn = nil
	}

	return d.sql.Close()
}

// Purge deletes all events and memories in a single transaction, then compacts the database.
func (d *DB) Purge() error {
	log.Info("func() db.Purge()")

	tx, err := d.sql.Begin()
	if err != nil {
		log.Errorf("failed to purge - beginning transaction: %s", err.Error())

		return err
	}

	if _, err := tx.Exec(`DELETE FROM memories`); err != nil {
		log.Errorf("failed to purge - deleting memories: %s", err.Error())
		_ = tx.Rollback()

		return err
	}

	if _, err := tx.Exec(`DELETE FROM events`); err != nil {
		log.Errorf("failed to purge - deleting events: %s", err.Error())
		_ = tx.Rollback()

		return err
	}

	if err := tx.Commit(); err != nil {
		log.Errorf("failed to purge - committing transaction: %s", err.Error())

		return err
	}

	if err := d.Preserve(); err != nil {
		log.Errorf("failed to purge - compacting database: %s", err.Error())

		return err
	}

	return nil
}
