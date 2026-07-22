package db

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// The tests in this file exercise the Postgres- and MySQL-only branches of the storage layer with
// a mocked database/sql handle (go-sqlmock), so the dialect-specific code paths are covered without
// a live Postgres or MySQL server. Every DB method reaches the database through the package-private
// d.sql handle, so a DB literal wrapping a mock connection plus the matching driver value drives
// exactly the branch under test.

// newMockDB returns a DB wired to a fresh sqlmock connection for the given driver.
func newMockDB(t *testing.T, drv driver) (*DB, sqlmock.Sqlmock) {
	t.Helper()

	sqlDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}

	t.Cleanup(func() { _ = sqlDB.Close() })

	return &DB{sql: sqlDB, driver: drv}, mock
}

func expectationsMet(t *testing.T, mock sqlmock.Sqlmock) {
	t.Helper()

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// stopConsolidatorKeepalive shuts down the lock keepalive goroutine a consolidator setup started
// and releases the pinned lock connection, mirroring Close's teardown without closing the mock
// handle (Close's own server-driver path is covered separately in db_extra_test.go). It leaves the
// underlying handle for newMockDB's t.Cleanup to close.
func stopConsolidatorKeepalive(d *DB) {
	if d.stopKeepalive != nil {
		close(d.stopKeepalive)
		<-d.keepaliveStopped
		d.stopKeepalive = nil
	}

	if d.lockConn != nil {
		_ = d.lockConn.Close()
		d.lockConn = nil
	}
}

// --- usedBytesLiveRows (postgres/mysql live-row size estimate) ---

func TestUsedBytesLiveRows(t *testing.T) {
	d, mock := newMockDB(t, driverPostgres)

	mock.ExpectQuery(`SELECT`).
		WillReturnRows(sqlmock.NewRows([]string{"used"}).AddRow(int64(9000)))

	used, err := d.usedBytesLiveRows(context.Background())
	if err != nil {
		t.Fatalf("usedBytesLiveRows: %v", err)
	}

	if used != 9000 {
		t.Fatalf("used = %d, want 9000", used)
	}

	expectationsMet(t, mock)
}

func TestUsedBytesLiveRowsError(t *testing.T) {
	d, mock := newMockDB(t, driverPostgres)

	mock.ExpectQuery(`SELECT`).WillReturnError(errors.New("boom"))

	if _, err := d.usedBytesLiveRows(context.Background()); err == nil {
		t.Fatal("expected an error")
	}

	expectationsMet(t, mock)
}

// --- acquireInstanceLock (postgres advisory lock) ---

func TestAcquireInstanceLockPostgres(t *testing.T) {
	d, mock := newMockDB(t, driverPostgres)

	mock.ExpectQuery(`pg_try_advisory_lock`).
		WillReturnRows(sqlmock.NewRows([]string{"locked"}).AddRow(true))

	if err := d.acquireInstanceLock(); err != nil {
		t.Fatalf("acquireInstanceLock: %v", err)
	}

	if d.lockConn == nil {
		t.Fatal("expected lockConn to be pinned")
	}

	_ = d.lockConn.Close()
	expectationsMet(t, mock)
}

func TestAcquireInstanceLockPostgresContended(t *testing.T) {
	d, mock := newMockDB(t, driverPostgres)

	mock.ExpectQuery(`pg_try_advisory_lock`).
		WillReturnRows(sqlmock.NewRows([]string{"locked"}).AddRow(false))

	if err := d.acquireInstanceLock(); err == nil {
		t.Fatal("expected an error when the lock is already held")
	}

	if d.lockConn != nil {
		t.Fatal("lockConn must stay nil when the lock is contended")
	}

	expectationsMet(t, mock)
}

func TestAcquireInstanceLockPostgresQueryError(t *testing.T) {
	d, mock := newMockDB(t, driverPostgres)

	mock.ExpectQuery(`pg_try_advisory_lock`).WillReturnError(errors.New("down"))

	if err := d.acquireInstanceLock(); err == nil {
		t.Fatal("expected an error")
	}

	expectationsMet(t, mock)
}

// --- acquireMySQLInstanceLock (GET_LOCK) ---

func TestAcquireMySQLInstanceLock(t *testing.T) {
	d, mock := newMockDB(t, driverMySQL)

	mock.ExpectQuery(`GET_LOCK`).
		WillReturnRows(sqlmock.NewRows([]string{"locked"}).AddRow(int64(1)))

	if err := d.acquireMySQLInstanceLock(); err != nil {
		t.Fatalf("acquireMySQLInstanceLock: %v", err)
	}

	if d.lockConn == nil {
		t.Fatal("expected lockConn to be pinned")
	}

	_ = d.lockConn.Close()
	expectationsMet(t, mock)
}

func TestAcquireMySQLInstanceLockContended(t *testing.T) {
	d, mock := newMockDB(t, driverMySQL)

	mock.ExpectQuery(`GET_LOCK`).
		WillReturnRows(sqlmock.NewRows([]string{"locked"}).AddRow(int64(0)))

	if err := d.acquireMySQLInstanceLock(); err == nil {
		t.Fatal("expected an error when another instance holds the lock")
	}

	expectationsMet(t, mock)
}

func TestAcquireMySQLInstanceLockNoDatabase(t *testing.T) {
	d, mock := newMockDB(t, driverMySQL)

	// GET_LOCK returns NULL (no database selected) -> NullInt64 invalid.
	mock.ExpectQuery(`GET_LOCK`).
		WillReturnRows(sqlmock.NewRows([]string{"locked"}).AddRow(nil))

	if err := d.acquireMySQLInstanceLock(); err == nil {
		t.Fatal("expected an error when GET_LOCK returns NULL")
	}

	expectationsMet(t, mock)
}

func TestAcquireMySQLInstanceLockQueryError(t *testing.T) {
	d, mock := newMockDB(t, driverMySQL)

	mock.ExpectQuery(`GET_LOCK`).WillReturnError(errors.New("down"))

	if err := d.acquireMySQLInstanceLock(); err == nil {
		t.Fatal("expected an error")
	}

	expectationsMet(t, mock)
}

// --- setMySQLColumnCollationIfNeeded ---

func TestSetMySQLColumnCollationAlreadyCorrect(t *testing.T) {
	d, mock := newMockDB(t, driverMySQL)

	mock.ExpectQuery(`collation_name`).
		WillReturnRows(sqlmock.NewRows([]string{"collation_name"}).AddRow(mysqlBinaryCollation))

	// No ALTER expected: the column already carries the target collation.
	if err := d.setMySQLColumnCollationIfNeeded("memories", "id", "VARCHAR(255) COLLATE "+mysqlBinaryCollation); err != nil {
		t.Fatalf("setMySQLColumnCollationIfNeeded: %v", err)
	}

	expectationsMet(t, mock)
}

func TestSetMySQLColumnCollationMigrates(t *testing.T) {
	d, mock := newMockDB(t, driverMySQL)

	mock.ExpectQuery(`collation_name`).
		WillReturnRows(sqlmock.NewRows([]string{"collation_name"}).AddRow("utf8mb4_0900_ai_ci"))
	mock.ExpectExec(`ALTER TABLE memories MODIFY id`).
		WillReturnResult(sqlmock.NewResult(0, 0))

	if err := d.setMySQLColumnCollationIfNeeded("memories", "id", "VARCHAR(255) COLLATE "+mysqlBinaryCollation); err != nil {
		t.Fatalf("setMySQLColumnCollationIfNeeded: %v", err)
	}

	expectationsMet(t, mock)
}

func TestSetMySQLColumnCollationProbeError(t *testing.T) {
	d, mock := newMockDB(t, driverMySQL)

	mock.ExpectQuery(`collation_name`).WillReturnError(errors.New("probe failed"))

	if err := d.setMySQLColumnCollationIfNeeded("memories", "id", "x"); err == nil {
		t.Fatal("expected an error")
	}

	expectationsMet(t, mock)
}

func TestSetMySQLColumnCollationModifyError(t *testing.T) {
	d, mock := newMockDB(t, driverMySQL)

	mock.ExpectQuery(`collation_name`).
		WillReturnRows(sqlmock.NewRows([]string{"collation_name"}).AddRow("utf8mb4_0900_ai_ci"))
	mock.ExpectExec(`ALTER TABLE memories MODIFY id`).WillReturnError(errors.New("modify failed"))

	if err := d.setMySQLColumnCollationIfNeeded("memories", "id", "VARCHAR(255)"); err == nil {
		t.Fatal("expected an error")
	}

	expectationsMet(t, mock)
}

// --- createMySQLIndexIfMissing ---

func TestCreateMySQLIndexAlreadyPresent(t *testing.T) {
	d, mock := newMockDB(t, driverMySQL)

	mock.ExpectQuery(`information_schema.statistics`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))

	if err := d.createMySQLIndexIfMissing("memories", "idx_x", "CREATE INDEX idx_x ON memories (x)"); err != nil {
		t.Fatalf("createMySQLIndexIfMissing: %v", err)
	}

	expectationsMet(t, mock)
}

func TestCreateMySQLIndexMissingCreates(t *testing.T) {
	d, mock := newMockDB(t, driverMySQL)

	mock.ExpectQuery(`information_schema.statistics`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectExec(`CREATE INDEX idx_x`).WillReturnResult(sqlmock.NewResult(0, 0))

	if err := d.createMySQLIndexIfMissing("memories", "idx_x", "CREATE INDEX idx_x ON memories (x)"); err != nil {
		t.Fatalf("createMySQLIndexIfMissing: %v", err)
	}

	expectationsMet(t, mock)
}

func TestCreateMySQLIndexProbeError(t *testing.T) {
	d, mock := newMockDB(t, driverMySQL)

	mock.ExpectQuery(`information_schema.statistics`).WillReturnError(errors.New("probe failed"))

	if err := d.createMySQLIndexIfMissing("memories", "idx_x", "CREATE INDEX idx_x ON memories (x)"); err == nil {
		t.Fatal("expected an error")
	}

	expectationsMet(t, mock)
}

// --- checkReadOnlyTables (used by NewPostgresReadOnly / NewMySQLReadOnly) ---

func TestCheckReadOnlyTablesOK(t *testing.T) {
	d, mock := newMockDB(t, driverPostgres)

	mock.ExpectQuery(`FROM events WHERE 1 = 0`).
		WillReturnRows(sqlmock.NewRows([]string{"1"}))
	mock.ExpectQuery(`FROM memories WHERE 1 = 0`).
		WillReturnRows(sqlmock.NewRows([]string{"1"}))

	if err := d.checkReadOnlyTables(); err != nil {
		t.Fatalf("checkReadOnlyTables: %v", err)
	}

	expectationsMet(t, mock)
}

func TestCheckReadOnlyTablesMissing(t *testing.T) {
	d, mock := newMockDB(t, driverPostgres)

	mock.ExpectQuery(`FROM events WHERE 1 = 0`).WillReturnError(errors.New("no such table"))

	if err := d.checkReadOnlyTables(); err == nil {
		t.Fatal("expected an error when a table is missing")
	}

	expectationsMet(t, mock)
}

// --- deleteChunkIfUnrecalled: postgres (DELETE ... RETURNING) and mysql (SELECT ... FOR UPDATE) ---

func TestDeleteChunkIfUnrecalledPostgres(t *testing.T) {
	d, mock := newMockDB(t, driverPostgres)

	mock.ExpectBegin()
	mock.ExpectQuery(`DELETE FROM memories WHERE`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("m1").AddRow("m2"))
	mock.ExpectCommit()

	tx, err := d.sql.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}

	chunk := []memoryRecallSnapshot{
		{id: "m1", timeRecalled: 0, recallCount: 0},
		{id: "m2", timeRecalled: 0, recallCount: 0},
	}

	ids, err := d.deleteChunkIfUnrecalled(tx, chunk)
	if err != nil {
		t.Fatalf("deleteChunkIfUnrecalled: %v", err)
	}

	if len(ids) != 2 || ids[0] != "m1" || ids[1] != "m2" {
		t.Fatalf("ids = %v, want [m1 m2]", ids)
	}

	_ = tx.Commit()
	expectationsMet(t, mock)
}

func TestDeleteChunkMySQL(t *testing.T) {
	d, mock := newMockDB(t, driverMySQL)

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT id FROM memories WHERE .* FOR UPDATE`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("m1").AddRow("m2"))
	mock.ExpectExec(`DELETE FROM memories WHERE id IN`).
		WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectCommit()

	tx, err := d.sql.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}

	chunk := []memoryRecallSnapshot{
		{id: "m1", timeRecalled: 0, recallCount: 0},
		{id: "m2", timeRecalled: 0, recallCount: 0},
	}

	ids, err := d.deleteChunkIfUnrecalled(tx, chunk)
	if err != nil {
		t.Fatalf("deleteChunkIfUnrecalled (mysql): %v", err)
	}

	if len(ids) != 2 {
		t.Fatalf("ids = %v, want 2 ids", ids)
	}

	_ = tx.Commit()
	expectationsMet(t, mock)
}

func TestDeleteChunkMySQLNoMatches(t *testing.T) {
	d, mock := newMockDB(t, driverMySQL)

	mock.ExpectBegin()
	// No rows locked -> the method returns early without a DELETE.
	mock.ExpectQuery(`SELECT id FROM memories WHERE .* FOR UPDATE`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))
	mock.ExpectCommit()

	tx, err := d.sql.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}

	ids, err := d.deleteChunkIfUnrecalled(tx, []memoryRecallSnapshot{{id: "gone"}})
	if err != nil {
		t.Fatalf("deleteChunkIfUnrecalled: %v", err)
	}

	if len(ids) != 0 {
		t.Fatalf("ids = %v, want none", ids)
	}

	_ = tx.Commit()
	expectationsMet(t, mock)
}

// --- recallMemoriesMySQL (UPDATE-then-SELECT, no RETURNING) ---

func TestRecallMemoriesMySQL(t *testing.T) {
	d, mock := newMockDB(t, driverMySQL)

	mock.ExpectBegin()
	mock.ExpectExec(`UPDATE memories SET time_recalled`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`FROM .* WHERE id IN`).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "timestamp", "significance", "event_id", "body",
			"is_binary", "time_recalled", "recall_count", "is_summary", "group_name",
		}).AddRow("m1", int64(10), int32(5), "e1", []byte("hi"), false, int64(99), int32(1), false, ""))
	mock.ExpectCommit()

	memories, err := d.recallMemoriesMySQL(context.Background(), []string{"m1"}, 99)
	if err != nil {
		t.Fatalf("recallMemoriesMySQL: %v", err)
	}

	if memories == nil || len(*memories) != 1 || (*memories)[0].Id != "m1" {
		t.Fatalf("unexpected memories: %+v", memories)
	}

	expectationsMet(t, mock)
}

func TestRecallMemoriesMySQLUpdateError(t *testing.T) {
	d, mock := newMockDB(t, driverMySQL)

	mock.ExpectBegin()
	mock.ExpectExec(`UPDATE memories SET time_recalled`).WillReturnError(errors.New("update failed"))
	mock.ExpectRollback()

	if _, err := d.recallMemoriesMySQL(context.Background(), []string{"m1"}, 99); err == nil {
		t.Fatal("expected an error")
	}

	expectationsMet(t, mock)
}

// --- initPostgresSchema (fresh database: significance migration is a no-op) ---

func TestInitPostgresSchemaFresh(t *testing.T) {
	d, mock := newMockDB(t, driverPostgres)

	mock.ExpectExec(`CREATE TABLE IF NOT EXISTS events`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`CREATE TABLE IF NOT EXISTS significance_levels`).WillReturnResult(sqlmock.NewResult(0, 0))
	// migrateSignificanceToLevels: the old significance column is absent -> no-op.
	mock.ExpectQuery(`information_schema.columns`).
		WillReturnRows(sqlmock.NewRows([]string{"column_name"}))
	mock.ExpectExec(`CREATE INDEX IF NOT EXISTS`).WillReturnResult(sqlmock.NewResult(0, 0))

	if err := d.initPostgresSchema(); err != nil {
		t.Fatalf("initPostgresSchema: %v", err)
	}

	expectationsMet(t, mock)
}

// --- initPostgresSchema against an old database that still has the significance column, exercising
// the full migrateSignificanceToLevels + dropCoveringIndex path. ---

func TestInitPostgresSchemaMigrates(t *testing.T) {
	d, mock := newMockDB(t, driverPostgres)

	mock.ExpectExec(`CREATE TABLE IF NOT EXISTS events`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`CREATE TABLE IF NOT EXISTS significance_levels`).WillReturnResult(sqlmock.NewResult(0, 0))
	// migrateSignificanceToLevels: the old significance column exists.
	mock.ExpectQuery(`information_schema.columns`).
		WillReturnRows(sqlmock.NewRows([]string{"column_name"}).AddRow("significance"))
	mock.ExpectExec(`INSERT INTO significance_levels`).WillReturnResult(sqlmock.NewResult(0, 3))
	mock.ExpectExec(`UPDATE memories SET significance_level_id`).WillReturnResult(sqlmock.NewResult(0, 5))
	mock.ExpectExec(`UPDATE events SET significance_level_id`).WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectExec(`DROP INDEX IF EXISTS`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`ALTER TABLE memories DROP COLUMN significance`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`ALTER TABLE events DROP COLUMN significance`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`CREATE INDEX IF NOT EXISTS`).WillReturnResult(sqlmock.NewResult(0, 0))

	if err := d.initPostgresSchema(); err != nil {
		t.Fatalf("initPostgresSchema (migrate): %v", err)
	}

	expectationsMet(t, mock)
}

// --- initMySQLSchema (fresh database). Expectations are ordered, mirroring the initialiser's call
// sequence: three CREATE TABLEs, then the addColumnIfMissing / collation / migration / covering-
// index probes, each reporting the desired state so no ALTER runs. The three probe kinds have
// distinct SELECT lists (column_name / collation_name / information_schema.statistics), so the
// regexes never overlap. ---

func TestInitMySQLSchemaFresh(t *testing.T) {
	d, mock := newMockDB(t, driverMySQL)

	columnPresent := func() {
		mock.ExpectQuery(`column_name FROM information_schema`).
			WillReturnRows(sqlmock.NewRows([]string{"column_name"}).AddRow("present"))
	}

	collationCorrect := func() {
		mock.ExpectQuery(`collation_name FROM information_schema`).
			WillReturnRows(sqlmock.NewRows([]string{"collation_name"}).AddRow(mysqlBinaryCollation))
	}

	mock.ExpectExec(`CREATE TABLE IF NOT EXISTS events`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`CREATE TABLE IF NOT EXISTS memories`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`CREATE TABLE IF NOT EXISTS significance_levels`).WillReturnResult(sqlmock.NewResult(0, 0))

	// addColumnIfMissing: is_summary, group_name (memories), group_name (events).
	columnPresent()
	columnPresent()
	columnPresent()

	// setMySQLColumnCollationIfNeeded x5, all already at the target collation.
	for range 5 {
		collationCorrect()
	}

	// addColumnIfMissing: significance_level_id on memories and events.
	columnPresent()
	columnPresent()

	// migrateSignificanceToLevels: the old significance column is absent -> no-op.
	mock.ExpectQuery(`column_name FROM information_schema`).
		WillReturnRows(sqlmock.NewRows([]string{"column_name"}))

	// ensureCoveringIndex -> createMySQLIndexIfMissing: index already present.
	mock.ExpectQuery(`information_schema.statistics`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))

	if err := d.initMySQLSchema(); err != nil {
		t.Fatalf("initMySQLSchema: %v", err)
	}

	expectationsMet(t, mock)
}

// --- schema-init expectation helpers, shared by the initSchema and setup* tests ---

// expectPostgresSchemaInitFresh queues the query expectations initPostgresSchema issues against a
// fresh database (no significance migration).
func expectPostgresSchemaInitFresh(mock sqlmock.Sqlmock) {
	mock.ExpectExec(`CREATE TABLE IF NOT EXISTS events`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`CREATE TABLE IF NOT EXISTS significance_levels`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`column_name FROM information_schema`).
		WillReturnRows(sqlmock.NewRows([]string{"column_name"}))
	mock.ExpectExec(`CREATE INDEX IF NOT EXISTS`).WillReturnResult(sqlmock.NewResult(0, 0))
}

// expectMySQLSchemaInitFresh queues the query expectations initMySQLSchema issues against a fresh
// database: three CREATE TABLEs, the addColumn/collation/migration probes (all reporting the
// desired state), and the covering-index probe.
func expectMySQLSchemaInitFresh(mock sqlmock.Sqlmock) {
	columnPresent := func() {
		mock.ExpectQuery(`column_name FROM information_schema`).
			WillReturnRows(sqlmock.NewRows([]string{"column_name"}).AddRow("present"))
	}

	mock.ExpectExec(`CREATE TABLE IF NOT EXISTS events`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`CREATE TABLE IF NOT EXISTS memories`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`CREATE TABLE IF NOT EXISTS significance_levels`).WillReturnResult(sqlmock.NewResult(0, 0))

	columnPresent()
	columnPresent()
	columnPresent()

	for range 5 {
		mock.ExpectQuery(`collation_name FROM information_schema`).
			WillReturnRows(sqlmock.NewRows([]string{"collation_name"}).AddRow(mysqlBinaryCollation))
	}

	columnPresent()
	columnPresent()

	mock.ExpectQuery(`column_name FROM information_schema`).
		WillReturnRows(sqlmock.NewRows([]string{"column_name"}))
	mock.ExpectQuery(`information_schema.statistics`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
}

// --- setupPostgres / setupPostgresReadOnly (the mockable core of NewPostgres*) ---

func TestSetupPostgresConsolidator(t *testing.T) {
	d, mock := newMockDB(t, driverPostgres)

	mock.ExpectQuery(`pg_try_advisory_lock`).
		WillReturnRows(sqlmock.NewRows([]string{"locked"}).AddRow(true))
	expectPostgresSchemaInitFresh(mock)

	got, err := setupPostgres(d.sql, true)
	if err != nil {
		t.Fatalf("setupPostgres: %v", err)
	}

	if got.lockConn == nil {
		t.Fatal("consolidator must pin a lock connection")
	}

	if got.stopKeepalive == nil {
		t.Fatal("consolidator must start the lock keepalive")
	}

	stopConsolidatorKeepalive(got)
	expectationsMet(t, mock)
}

func TestSetupPostgresReplica(t *testing.T) {
	d, mock := newMockDB(t, driverPostgres)

	// consolidate false -> no advisory lock, no keepalive.
	expectPostgresSchemaInitFresh(mock)
	mock.ExpectClose()

	got, err := setupPostgres(d.sql, false)
	if err != nil {
		t.Fatalf("setupPostgres (replica): %v", err)
	}

	if got.lockConn != nil {
		t.Fatal("replica must not pin a lock connection")
	}

	if err := got.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	expectationsMet(t, mock)
}

func TestSetupPostgresLockContended(t *testing.T) {
	d, mock := newMockDB(t, driverPostgres)

	mock.ExpectQuery(`pg_try_advisory_lock`).
		WillReturnRows(sqlmock.NewRows([]string{"locked"}).AddRow(false))
	mock.ExpectClose()

	if _, err := setupPostgres(d.sql, true); err == nil {
		t.Fatal("expected setupPostgres to fail when the lock is held")
	}

	expectationsMet(t, mock)
}

func TestSetupPostgresReadOnly(t *testing.T) {
	d, mock := newMockDB(t, driverPostgres)

	mock.ExpectQuery(`FROM events WHERE 1 = 0`).WillReturnRows(sqlmock.NewRows([]string{"1"}))
	mock.ExpectQuery(`FROM memories WHERE 1 = 0`).WillReturnRows(sqlmock.NewRows([]string{"1"}))

	got, err := setupPostgresReadOnly(d.sql)
	if err != nil {
		t.Fatalf("setupPostgresReadOnly: %v", err)
	}

	if got.lockConn != nil {
		t.Fatal("read-only open must not pin a lock connection")
	}

	expectationsMet(t, mock)
}

func TestSetupPostgresReadOnlyMissingTables(t *testing.T) {
	d, mock := newMockDB(t, driverPostgres)

	mock.ExpectQuery(`FROM events WHERE 1 = 0`).WillReturnError(errors.New("no such table"))
	mock.ExpectClose()

	if _, err := setupPostgresReadOnly(d.sql); err == nil {
		t.Fatal("expected an error when tables are missing")
	}

	expectationsMet(t, mock)
}

// --- setupMySQL / setupMySQLReadOnly ---

func TestSetupMySQLConsolidator(t *testing.T) {
	d, mock := newMockDB(t, driverMySQL)

	mock.ExpectQuery(`GET_LOCK`).
		WillReturnRows(sqlmock.NewRows([]string{"locked"}).AddRow(int64(1)))
	expectMySQLSchemaInitFresh(mock)

	got, err := setupMySQL(d.sql, true)
	if err != nil {
		t.Fatalf("setupMySQL: %v", err)
	}

	if got.lockConn == nil {
		t.Fatal("consolidator must pin a lock connection")
	}

	if got.stopKeepalive == nil {
		t.Fatal("consolidator must start the lock keepalive")
	}

	stopConsolidatorKeepalive(got)
	expectationsMet(t, mock)
}

func TestSetupMySQLReplica(t *testing.T) {
	d, mock := newMockDB(t, driverMySQL)

	expectMySQLSchemaInitFresh(mock)
	mock.ExpectClose()

	got, err := setupMySQL(d.sql, false)
	if err != nil {
		t.Fatalf("setupMySQL (replica): %v", err)
	}

	if got.lockConn != nil {
		t.Fatal("replica must not pin a lock connection")
	}

	if err := got.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	expectationsMet(t, mock)
}

func TestSetupMySQLLockContended(t *testing.T) {
	d, mock := newMockDB(t, driverMySQL)

	mock.ExpectQuery(`GET_LOCK`).
		WillReturnRows(sqlmock.NewRows([]string{"locked"}).AddRow(int64(0)))
	mock.ExpectClose()

	if _, err := setupMySQL(d.sql, true); err == nil {
		t.Fatal("expected setupMySQL to fail when the lock is held")
	}

	expectationsMet(t, mock)
}

func TestSetupMySQLReadOnly(t *testing.T) {
	d, mock := newMockDB(t, driverMySQL)

	mock.ExpectQuery(`FROM events WHERE 1 = 0`).WillReturnRows(sqlmock.NewRows([]string{"1"}))
	mock.ExpectQuery(`FROM memories WHERE 1 = 0`).WillReturnRows(sqlmock.NewRows([]string{"1"}))

	got, err := setupMySQLReadOnly(d.sql)
	if err != nil {
		t.Fatalf("setupMySQLReadOnly: %v", err)
	}

	if got.lockConn != nil {
		t.Fatal("read-only open must not pin a lock connection")
	}

	expectationsMet(t, mock)
}

// TestSetupPostgresSchemaInitFailsReleasesLock verifies setupPostgres releases the freshly taken
// lock connection (and closes the handle) when schema initialisation fails, rather than leaking the
// lock.
func TestSetupPostgresSchemaInitFailsReleasesLock(t *testing.T) {
	d, mock := newMockDB(t, driverPostgres)

	mock.ExpectQuery(`pg_try_advisory_lock`).
		WillReturnRows(sqlmock.NewRows([]string{"locked"}).AddRow(true))
	mock.ExpectExec(`CREATE TABLE IF NOT EXISTS events`).WillReturnError(errors.New("ddl failed"))
	mock.ExpectClose()

	if _, err := setupPostgres(d.sql, true); err == nil {
		t.Fatal("expected setupPostgres to fail when schema init fails")
	}

	expectationsMet(t, mock)
}

func TestSetupMySQLReadOnlyMissingTables(t *testing.T) {
	d, mock := newMockDB(t, driverMySQL)

	mock.ExpectQuery(`FROM events WHERE 1 = 0`).WillReturnError(errors.New("no such table"))
	mock.ExpectClose()

	if _, err := setupMySQLReadOnly(d.sql); err == nil {
		t.Fatal("expected an error when tables are missing")
	}

	expectationsMet(t, mock)
}

// --- columnExists error branches (dialect-generic; the mock reaches them without needing a real
// server-side probe failure) ---

func TestColumnExistsQueryError(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	mock.ExpectQuery(`pragma_table_info`).WillReturnError(errors.New("boom"))

	if _, err := d.columnExists("memories", "significance"); err == nil {
		t.Fatal("expected an error")
	}

	expectationsMet(t, mock)
}

// TestColumnExistsRowsIterationError drives the rows.Err() branch: go-sqlmock's RowError makes
// the underlying Next() call itself fail (rather than a Scan error), which is exactly what
// database/sql surfaces as rows.Err() after Next() returns false - see rows.Err() in the standard
// library and go-sqlmock's rowSets.Next.
func TestColumnExistsRowsIterationError(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	mock.ExpectQuery(`pragma_table_info`).
		WillReturnRows(sqlmock.NewRows([]string{"name"}).AddRow("significance").RowError(0, errors.New("iteration failed")))

	if _, err := d.columnExists("memories", "significance"); err == nil {
		t.Fatal("expected an error from rows.Err()")
	}

	expectationsMet(t, mock)
}

// --- dropCoveringIndex: MySQL arm (information_schema probe + conditional DROP INDEX) ---

func TestDropCoveringIndexMySQLProbeError(t *testing.T) {
	d, mock := newMockDB(t, driverMySQL)

	mock.ExpectQuery(`information_schema.statistics`).WillReturnError(errors.New("boom"))

	if err := d.dropCoveringIndex(); err == nil {
		t.Fatal("expected an error")
	}

	expectationsMet(t, mock)
}

func TestDropCoveringIndexMySQLAbsentIsNoOp(t *testing.T) {
	d, mock := newMockDB(t, driverMySQL)

	mock.ExpectQuery(`information_schema.statistics`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))

	if err := d.dropCoveringIndex(); err != nil {
		t.Fatalf("dropCoveringIndex: %v", err)
	}

	// No DROP INDEX expected: expectationsMet fails if one was queued but unmet, so the absence of
	// an ExpectExec here is itself the assertion that none ran.
	expectationsMet(t, mock)
}

func TestDropCoveringIndexMySQLDropsWhenPresent(t *testing.T) {
	d, mock := newMockDB(t, driverMySQL)

	mock.ExpectQuery(`information_schema.statistics`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectExec(`DROP INDEX idx_memories_consolidation ON memories`).WillReturnResult(sqlmock.NewResult(0, 0))

	if err := d.dropCoveringIndex(); err != nil {
		t.Fatalf("dropCoveringIndex: %v", err)
	}

	expectationsMet(t, mock)
}

func TestDropCoveringIndexMySQLDropExecError(t *testing.T) {
	d, mock := newMockDB(t, driverMySQL)

	mock.ExpectQuery(`information_schema.statistics`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectExec(`DROP INDEX idx_memories_consolidation ON memories`).WillReturnError(errors.New("boom"))

	if err := d.dropCoveringIndex(); err == nil {
		t.Fatal("expected an error")
	}

	expectationsMet(t, mock)
}

// --- ensureCoveringIndex: non-MySQL CREATE INDEX failure ---

func TestEnsureCoveringIndexExecError(t *testing.T) {
	d, mock := newMockDB(t, driverPostgres)

	mock.ExpectExec(`CREATE INDEX IF NOT EXISTS idx_memories_consolidation`).WillReturnError(errors.New("boom"))

	if err := d.ensureCoveringIndex(); err == nil {
		t.Fatal("expected an error")
	}

	expectationsMet(t, mock)
}

// --- migrateSignificanceToLevels error branches. The function only ever reaches the real schema
// via d.sql, so its every step is driven directly against the mock rather than by seeding an old
// SQLite schema for each failure mode. driverSQLite keeps every placeholder a plain '?', matching
// the literal query text below. ---

func TestMigrateSignificanceToLevels_ColumnExistsError(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	mock.ExpectQuery(`pragma_table_info`).WillReturnError(errors.New("boom"))

	if err := d.migrateSignificanceToLevels(); err == nil {
		t.Fatal("expected an error")
	}

	expectationsMet(t, mock)
}

func TestMigrateSignificanceToLevels_NoOpWhenColumnAbsent(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	mock.ExpectQuery(`pragma_table_info`).WillReturnRows(sqlmock.NewRows([]string{"name"}))

	if err := d.migrateSignificanceToLevels(); err != nil {
		t.Fatalf("migrateSignificanceToLevels: %v", err)
	}

	expectationsMet(t, mock)
}

func TestMigrateSignificanceToLevels_SeedError(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	mock.ExpectQuery(`pragma_table_info`).
		WillReturnRows(sqlmock.NewRows([]string{"name"}).AddRow("significance"))
	mock.ExpectExec(`INSERT INTO significance_levels`).WillReturnError(errors.New("boom"))

	if err := d.migrateSignificanceToLevels(); err == nil {
		t.Fatal("expected an error")
	}

	expectationsMet(t, mock)
}

func TestMigrateSignificanceToLevels_BackfillError(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	mock.ExpectQuery(`pragma_table_info`).
		WillReturnRows(sqlmock.NewRows([]string{"name"}).AddRow("significance"))
	mock.ExpectExec(`INSERT INTO significance_levels`).WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectExec(`UPDATE memories SET significance_level_id`).WillReturnError(errors.New("boom"))

	if err := d.migrateSignificanceToLevels(); err == nil {
		t.Fatal("expected an error")
	}

	expectationsMet(t, mock)
}

func TestMigrateSignificanceToLevels_DropIndexError(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	mock.ExpectQuery(`pragma_table_info`).
		WillReturnRows(sqlmock.NewRows([]string{"name"}).AddRow("significance"))
	mock.ExpectExec(`INSERT INTO significance_levels`).WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectExec(`UPDATE memories SET significance_level_id`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE events SET significance_level_id`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`DROP INDEX IF EXISTS idx_memories_consolidation`).WillReturnError(errors.New("boom"))

	if err := d.migrateSignificanceToLevels(); err == nil {
		t.Fatal("expected an error")
	}

	expectationsMet(t, mock)
}

func TestMigrateSignificanceToLevels_DropColumnError(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	mock.ExpectQuery(`pragma_table_info`).
		WillReturnRows(sqlmock.NewRows([]string{"name"}).AddRow("significance"))
	mock.ExpectExec(`INSERT INTO significance_levels`).WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectExec(`UPDATE memories SET significance_level_id`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE events SET significance_level_id`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`DROP INDEX IF EXISTS idx_memories_consolidation`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`ALTER TABLE memories DROP COLUMN significance`).WillReturnError(errors.New("boom"))

	if err := d.migrateSignificanceToLevels(); err == nil {
		t.Fatal("expected an error")
	}

	expectationsMet(t, mock)
}

// --- resolveLevelTx's helpers (anchorRank/rankTaken/openGapAt/findOrCreateLevelTx/createLevelTx),
// driven directly against a mocked transaction for error branches a real SQLite schema can't
// reach mid-transaction. ---

func beginMockTx(t *testing.T, d *DB, mock sqlmock.Sqlmock) *sql.Tx {
	t.Helper()

	mock.ExpectBegin()

	tx, err := d.sql.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}

	return tx
}

func TestAnchorRank_QueryErrorOtherThanNoRows(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)
	tx := beginMockTx(t, d, mock)

	mock.ExpectQuery(`FROM memories t`).WillReturnError(errors.New("connection reset"))

	if _, err := d.anchorRank(context.Background(), tx, 0, "m1", AnchorMemory); err == nil {
		t.Fatal("expected the raw error to surface")
	}

	_ = tx.Rollback()
	expectationsMet(t, mock)
}

func TestRankTaken_QueryError(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)
	tx := beginMockTx(t, d, mock)

	mock.ExpectQuery(`SELECT EXISTS`).WillReturnError(errors.New("boom"))

	if _, err := d.rankTaken(context.Background(), tx, 5); err == nil {
		t.Fatal("expected an error")
	}

	_ = tx.Rollback()
	expectationsMet(t, mock)
}

func TestOpenGapAt_FirstExecError(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)
	tx := beginMockTx(t, d, mock)

	mock.ExpectExec(`UPDATE significance_levels SET level_rank = -`).WillReturnError(errors.New("boom"))

	if err := d.openGapAt(context.Background(), tx, 5); err == nil {
		t.Fatal("expected an error")
	}

	_ = tx.Rollback()
	expectationsMet(t, mock)
}

func TestFindOrCreateLevelTx_QueryErrorOtherThanNoRows(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)
	tx := beginMockTx(t, d, mock)

	mock.ExpectQuery(`SELECT id FROM significance_levels WHERE level_rank`).WillReturnError(errors.New("boom"))

	if _, err := d.findOrCreateLevelTx(context.Background(), tx, 5); err == nil {
		t.Fatal("expected an error")
	}

	_ = tx.Rollback()
	expectationsMet(t, mock)
}

func TestCreateLevelTx_InsertError(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)
	tx := beginMockTx(t, d, mock)

	mock.ExpectExec(`INSERT INTO significance_levels`).WillReturnError(errors.New("boom"))

	if _, err := d.createLevelTx(context.Background(), tx, 5); err == nil {
		t.Fatal("expected an error")
	}

	_ = tx.Rollback()
	expectationsMet(t, mock)
}

func TestCreateLevelTx_SelectAfterInsertError(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)
	tx := beginMockTx(t, d, mock)

	mock.ExpectExec(`INSERT INTO significance_levels`).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectQuery(`SELECT id FROM significance_levels WHERE level_rank`).WillReturnError(errors.New("boom"))

	if _, err := d.createLevelTx(context.Background(), tx, 5); err == nil {
		t.Fatal("expected an error")
	}

	_ = tx.Rollback()
	expectationsMet(t, mock)
}

// --- acquireRegistryLock: server-driver branches (SQLite's no-op is covered by every SQLite
// registry test already) ---

func TestAcquireRegistryLock_ConnError(t *testing.T) {
	d, mock := newMockDB(t, driverPostgres)

	mock.ExpectClose()

	if err := d.sql.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if _, err := d.acquireRegistryLock(context.Background()); err == nil {
		t.Fatal("expected an error from a closed handle")
	}

	expectationsMet(t, mock)
}

func TestAcquireRegistryLock_Postgres(t *testing.T) {
	d, mock := newMockDB(t, driverPostgres)

	mock.ExpectExec(`pg_advisory_lock`).WillReturnResult(sqlmock.NewResult(0, 0))

	release, err := d.acquireRegistryLock(context.Background())
	if err != nil {
		t.Fatalf("acquireRegistryLock: %v", err)
	}

	mock.ExpectExec(`pg_advisory_unlock`).WillReturnResult(sqlmock.NewResult(0, 0))
	release()

	expectationsMet(t, mock)
}

func TestAcquireRegistryLock_PostgresExecError(t *testing.T) {
	d, mock := newMockDB(t, driverPostgres)

	mock.ExpectExec(`pg_advisory_lock`).WillReturnError(errors.New("boom"))

	if _, err := d.acquireRegistryLock(context.Background()); err == nil {
		t.Fatal("expected an error")
	}

	expectationsMet(t, mock)
}

func TestAcquireRegistryLock_MySQL(t *testing.T) {
	d, mock := newMockDB(t, driverMySQL)

	mock.ExpectExec(`GET_LOCK`).WillReturnResult(sqlmock.NewResult(0, 0))

	release, err := d.acquireRegistryLock(context.Background())
	if err != nil {
		t.Fatalf("acquireRegistryLock: %v", err)
	}

	mock.ExpectExec(`RELEASE_LOCK`).WillReturnResult(sqlmock.NewResult(0, 0))
	release()

	expectationsMet(t, mock)
}

func TestAcquireRegistryLock_MySQLExecError(t *testing.T) {
	d, mock := newMockDB(t, driverMySQL)

	mock.ExpectExec(`GET_LOCK`).WillReturnError(errors.New("boom"))

	if _, err := d.acquireRegistryLock(context.Background()); err == nil {
		t.Fatal("expected an error")
	}

	expectationsMet(t, mock)
}

// TestAcquireRegistryLock_UnknownDriverFallsThrough exercises the switch's fallthrough for a
// driver value that is neither SQLite (already returned above) nor Postgres/MySQL - defensive
// dead code today (only three driver values are ever constructed in production) but cheap to
// pin: it must release the connection and hand back a no-op release rather than panicking.
func TestAcquireRegistryLock_UnknownDriverFallsThrough(t *testing.T) {
	d, mock := newMockDB(t, driver(99))

	release, err := d.acquireRegistryLock(context.Background())
	if err != nil {
		t.Fatalf("acquireRegistryLock: %v", err)
	}

	if release == nil {
		t.Fatal("expected a non-nil no-op release")
	}

	release()
	expectationsMet(t, mock)
}

// --- initPostgresSchema / initMySQLSchema failure branches not already covered by the fresh/
// migrate success-path tests above. ---

func TestInitPostgresSchema_SignificanceLevelsDDLError(t *testing.T) {
	d, mock := newMockDB(t, driverPostgres)

	mock.ExpectExec(`CREATE TABLE IF NOT EXISTS events`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`CREATE TABLE IF NOT EXISTS significance_levels`).WillReturnError(errors.New("boom"))

	if err := d.initPostgresSchema(); err == nil {
		t.Fatal("expected an error")
	}

	expectationsMet(t, mock)
}

func TestInitPostgresSchema_MigrateError(t *testing.T) {
	d, mock := newMockDB(t, driverPostgres)

	mock.ExpectExec(`CREATE TABLE IF NOT EXISTS events`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`CREATE TABLE IF NOT EXISTS significance_levels`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`information_schema.columns`).WillReturnError(errors.New("boom"))

	if err := d.initPostgresSchema(); err == nil {
		t.Fatal("expected an error")
	}

	expectationsMet(t, mock)
}

func TestInitPostgresSchema_EnsureCoveringIndexError(t *testing.T) {
	d, mock := newMockDB(t, driverPostgres)

	mock.ExpectExec(`CREATE TABLE IF NOT EXISTS events`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`CREATE TABLE IF NOT EXISTS significance_levels`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`information_schema.columns`).
		WillReturnRows(sqlmock.NewRows([]string{"column_name"}))
	mock.ExpectExec(`CREATE INDEX IF NOT EXISTS`).WillReturnError(errors.New("boom"))

	if err := d.initPostgresSchema(); err == nil {
		t.Fatal("expected an error")
	}

	expectationsMet(t, mock)
}

func TestInitMySQLSchema_CreateTableError(t *testing.T) {
	d, mock := newMockDB(t, driverMySQL)

	mock.ExpectExec(`CREATE TABLE IF NOT EXISTS events`).WillReturnError(errors.New("boom"))

	if err := d.initMySQLSchema(); err == nil {
		t.Fatal("expected an error")
	}

	expectationsMet(t, mock)
}

func TestInitMySQLSchema_AddColumnError(t *testing.T) {
	d, mock := newMockDB(t, driverMySQL)

	mock.ExpectExec(`CREATE TABLE IF NOT EXISTS events`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`CREATE TABLE IF NOT EXISTS memories`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`CREATE TABLE IF NOT EXISTS significance_levels`).WillReturnResult(sqlmock.NewResult(0, 0))
	// addColumnIfMissing(memories, is_summary): probe reports missing, then the ALTER fails.
	mock.ExpectQuery(`column_name FROM information_schema`).
		WillReturnRows(sqlmock.NewRows([]string{"column_name"}))
	mock.ExpectExec(`ALTER TABLE memories ADD COLUMN is_summary`).WillReturnError(errors.New("boom"))

	if err := d.initMySQLSchema(); err == nil {
		t.Fatal("expected an error")
	}

	expectationsMet(t, mock)
}

func TestInitMySQLSchema_CollationError(t *testing.T) {
	d, mock := newMockDB(t, driverMySQL)

	columnPresent := func() {
		mock.ExpectQuery(`column_name FROM information_schema`).
			WillReturnRows(sqlmock.NewRows([]string{"column_name"}).AddRow("present"))
	}

	mock.ExpectExec(`CREATE TABLE IF NOT EXISTS events`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`CREATE TABLE IF NOT EXISTS memories`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`CREATE TABLE IF NOT EXISTS significance_levels`).WillReturnResult(sqlmock.NewResult(0, 0))

	columnPresent()
	columnPresent()
	columnPresent()

	// First collation probe (events.id) reports a mismatch, and the MODIFY fails.
	mock.ExpectQuery(`collation_name FROM information_schema`).
		WillReturnRows(sqlmock.NewRows([]string{"collation_name"}).AddRow("utf8mb4_0900_ai_ci"))
	mock.ExpectExec(`ALTER TABLE events MODIFY id`).WillReturnError(errors.New("boom"))

	if err := d.initMySQLSchema(); err == nil {
		t.Fatal("expected an error")
	}

	expectationsMet(t, mock)
}

func TestInitMySQLSchema_SignificanceLevelIDColumnError(t *testing.T) {
	d, mock := newMockDB(t, driverMySQL)

	columnPresent := func() {
		mock.ExpectQuery(`column_name FROM information_schema`).
			WillReturnRows(sqlmock.NewRows([]string{"column_name"}).AddRow("present"))
	}

	collationCorrect := func() {
		mock.ExpectQuery(`collation_name FROM information_schema`).
			WillReturnRows(sqlmock.NewRows([]string{"collation_name"}).AddRow(mysqlBinaryCollation))
	}

	mock.ExpectExec(`CREATE TABLE IF NOT EXISTS events`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`CREATE TABLE IF NOT EXISTS memories`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`CREATE TABLE IF NOT EXISTS significance_levels`).WillReturnResult(sqlmock.NewResult(0, 0))

	columnPresent()
	columnPresent()
	columnPresent()

	for range 5 {
		collationCorrect()
	}

	// addColumnIfMissing(memories, significance_level_id): reports missing, then the ALTER fails.
	mock.ExpectQuery(`column_name FROM information_schema`).
		WillReturnRows(sqlmock.NewRows([]string{"column_name"}))
	mock.ExpectExec(`ALTER TABLE memories ADD COLUMN significance_level_id`).WillReturnError(errors.New("boom"))

	if err := d.initMySQLSchema(); err == nil {
		t.Fatal("expected an error")
	}

	expectationsMet(t, mock)
}

func TestInitMySQLSchema_EnsureCoveringIndexError(t *testing.T) {
	d, mock := newMockDB(t, driverMySQL)

	columnPresent := func() {
		mock.ExpectQuery(`column_name FROM information_schema`).
			WillReturnRows(sqlmock.NewRows([]string{"column_name"}).AddRow("present"))
	}

	collationCorrect := func() {
		mock.ExpectQuery(`collation_name FROM information_schema`).
			WillReturnRows(sqlmock.NewRows([]string{"collation_name"}).AddRow(mysqlBinaryCollation))
	}

	mock.ExpectExec(`CREATE TABLE IF NOT EXISTS events`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`CREATE TABLE IF NOT EXISTS memories`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`CREATE TABLE IF NOT EXISTS significance_levels`).WillReturnResult(sqlmock.NewResult(0, 0))

	columnPresent()
	columnPresent()
	columnPresent()

	for range 5 {
		collationCorrect()
	}

	columnPresent()
	columnPresent()

	// migrateSignificanceToLevels: old column absent -> no-op.
	mock.ExpectQuery(`column_name FROM information_schema`).
		WillReturnRows(sqlmock.NewRows([]string{"column_name"}))

	// ensureCoveringIndex -> createMySQLIndexIfMissing: reported absent, and the CREATE INDEX fails.
	mock.ExpectQuery(`information_schema.statistics`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectExec(`CREATE INDEX`).WillReturnError(errors.New("boom"))

	if err := d.initMySQLSchema(); err == nil {
		t.Fatal("expected an error")
	}

	expectationsMet(t, mock)
}

func TestCreateMySQLIndexMissingCreateExecError(t *testing.T) {
	d, mock := newMockDB(t, driverMySQL)

	mock.ExpectQuery(`information_schema.statistics`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectExec(`CREATE INDEX idx_x`).WillReturnError(errors.New("boom"))

	if err := d.createMySQLIndexIfMissing("memories", "idx_x", "CREATE INDEX idx_x ON memories (x)"); err == nil {
		t.Fatal("expected an error")
	}

	expectationsMet(t, mock)
}

// --- loadSignificanceRanks error branches ---

func TestLoadSignificanceRanks_ScanError(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	mock.ExpectQuery(`SELECT id, level_rank FROM significance_levels`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "level_rank"}).AddRow("not-an-int", int32(5)))

	if _, err := d.loadSignificanceRanks(context.Background()); err == nil {
		t.Fatal("expected a scan error")
	}

	expectationsMet(t, mock)
}

func TestLoadSignificanceRanks_RowsIterationError(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	mock.ExpectQuery(`SELECT id, level_rank FROM significance_levels`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "level_rank"}).
			AddRow(int64(1), int32(5)).
			RowError(0, errors.New("boom")))

	if _, err := d.loadSignificanceRanks(context.Background()); err == nil {
		t.Fatal("expected an iteration error")
	}

	expectationsMet(t, mock)
}

// --- ResolveSignificanceLevel: findLevel error and the tx.Commit() failure ---

func TestResolveSignificanceLevel_FindLevelError(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	mock.ExpectQuery(`SELECT id FROM significance_levels WHERE level_rank`).WillReturnError(errors.New("boom"))

	if _, _, err := d.ResolveSignificanceLevel(context.Background(), SignificanceSpec{Value: 5}); err == nil {
		t.Fatal("expected an error")
	}

	expectationsMet(t, mock)
}

func TestResolveSignificanceLevel_CommitError(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	// The fast path (findLevel) reports no existing level, so ResolveSignificanceLevel takes the
	// lock+transaction path; findOrCreateLevelTx then also finds nothing and creates one.
	mock.ExpectQuery(`SELECT id FROM significance_levels WHERE level_rank`).WillReturnError(sql.ErrNoRows)
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT id FROM significance_levels WHERE level_rank`).WillReturnError(sql.ErrNoRows)
	mock.ExpectExec(`INSERT INTO significance_levels`).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectQuery(`SELECT id FROM significance_levels WHERE level_rank`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(1)))
	mock.ExpectCommit().WillReturnError(errors.New("commit failed"))

	if _, _, err := d.ResolveSignificanceLevel(context.Background(), SignificanceSpec{Value: 5}); err == nil {
		t.Fatal("expected a commit error")
	}

	expectationsMet(t, mock)
}

// --- CompactSignificanceLevels: mid-transaction failure branches, driven directly since a real
// SQLite schema can't be made to fail mid-transaction on demand. ---

func TestCompactSignificanceLevels_TxQueryError(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	mock.ExpectQuery(`SELECT MAX`).
		WillReturnRows(sqlmock.NewRows([]string{"max"}).AddRow(int64(registryCompactionThreshold)))
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT id FROM significance_levels ORDER BY level_rank`).WillReturnError(errors.New("boom"))
	mock.ExpectRollback()

	if err := d.CompactSignificanceLevels(context.Background()); err == nil {
		t.Fatal("expected an error")
	}

	expectationsMet(t, mock)
}

func TestCompactSignificanceLevels_ScanError(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	mock.ExpectQuery(`SELECT MAX`).
		WillReturnRows(sqlmock.NewRows([]string{"max"}).AddRow(int64(registryCompactionThreshold)))
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT id FROM significance_levels ORDER BY level_rank`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("not-an-int"))
	mock.ExpectRollback()

	if err := d.CompactSignificanceLevels(context.Background()); err == nil {
		t.Fatal("expected a scan error")
	}

	expectationsMet(t, mock)
}

func TestCompactSignificanceLevels_RowsIterationError(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	mock.ExpectQuery(`SELECT MAX`).
		WillReturnRows(sqlmock.NewRows([]string{"max"}).AddRow(int64(registryCompactionThreshold)))
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT id FROM significance_levels ORDER BY level_rank`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(1)).RowError(0, errors.New("boom")))
	mock.ExpectRollback()

	if err := d.CompactSignificanceLevels(context.Background()); err == nil {
		t.Fatal("expected an iteration error")
	}

	expectationsMet(t, mock)
}

func TestCompactSignificanceLevels_UpdateExecError(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	mock.ExpectQuery(`SELECT MAX`).
		WillReturnRows(sqlmock.NewRows([]string{"max"}).AddRow(int64(registryCompactionThreshold)))
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT id FROM significance_levels ORDER BY level_rank`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(1)).AddRow(int64(2)))
	mock.ExpectExec(`UPDATE significance_levels SET level_rank`).WillReturnError(errors.New("boom"))
	mock.ExpectRollback()

	if err := d.CompactSignificanceLevels(context.Background()); err == nil {
		t.Fatal("expected an error")
	}

	expectationsMet(t, mock)
}

func TestCompactSignificanceLevels_FinalFlipExecError(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	mock.ExpectQuery(`SELECT MAX`).
		WillReturnRows(sqlmock.NewRows([]string{"max"}).AddRow(int64(registryCompactionThreshold)))
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT id FROM significance_levels ORDER BY level_rank`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(1)))
	mock.ExpectExec(`UPDATE significance_levels SET level_rank = \?`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE significance_levels SET level_rank = -level_rank`).WillReturnError(errors.New("boom"))
	mock.ExpectRollback()

	if err := d.CompactSignificanceLevels(context.Background()); err == nil {
		t.Fatal("expected an error")
	}

	expectationsMet(t, mock)
}

// --- setupMySQL: releasing a freshly taken lock connection when schema init fails, mirroring
// TestSetupPostgresSchemaInitFailsReleasesLock. ---

func TestSetupMySQL_SchemaInitFailsReleasesLock(t *testing.T) {
	d, mock := newMockDB(t, driverMySQL)

	mock.ExpectQuery(`GET_LOCK`).
		WillReturnRows(sqlmock.NewRows([]string{"locked"}).AddRow(int64(1)))
	mock.ExpectExec(`CREATE TABLE IF NOT EXISTS events`).WillReturnError(errors.New("ddl failed"))
	mock.ExpectClose()

	if _, err := setupMySQL(d.sql, true); err == nil {
		t.Fatal("expected setupMySQL to fail when schema init fails")
	}

	expectationsMet(t, mock)
}

// --- initMySQLSchema: the remaining addColumnIfMissing propagation branches not already
// exercised by TestInitMySQLSchema_AddColumnError (is_summary) or
// TestInitMySQLSchema_SignificanceLevelIDColumnError (memories.significance_level_id). ---

func TestInitMySQLSchema_MemoriesGroupNameColumnError(t *testing.T) {
	d, mock := newMockDB(t, driverMySQL)

	columnPresent := func() {
		mock.ExpectQuery(`column_name FROM information_schema`).
			WillReturnRows(sqlmock.NewRows([]string{"column_name"}).AddRow("present"))
	}

	mock.ExpectExec(`CREATE TABLE IF NOT EXISTS events`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`CREATE TABLE IF NOT EXISTS memories`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`CREATE TABLE IF NOT EXISTS significance_levels`).WillReturnResult(sqlmock.NewResult(0, 0))

	// is_summary present, then group_name(memories) reported missing and its ALTER fails.
	columnPresent()
	mock.ExpectQuery(`column_name FROM information_schema`).
		WillReturnRows(sqlmock.NewRows([]string{"column_name"}))
	mock.ExpectExec(`ALTER TABLE memories ADD COLUMN group_name`).WillReturnError(errors.New("boom"))

	if err := d.initMySQLSchema(); err == nil {
		t.Fatal("expected an error")
	}

	expectationsMet(t, mock)
}

func TestInitMySQLSchema_EventsGroupNameColumnError(t *testing.T) {
	d, mock := newMockDB(t, driverMySQL)

	columnPresent := func() {
		mock.ExpectQuery(`column_name FROM information_schema`).
			WillReturnRows(sqlmock.NewRows([]string{"column_name"}).AddRow("present"))
	}

	mock.ExpectExec(`CREATE TABLE IF NOT EXISTS events`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`CREATE TABLE IF NOT EXISTS memories`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`CREATE TABLE IF NOT EXISTS significance_levels`).WillReturnResult(sqlmock.NewResult(0, 0))

	// is_summary and group_name(memories) present, group_name(events) missing and its ALTER fails.
	columnPresent()
	columnPresent()
	mock.ExpectQuery(`column_name FROM information_schema`).
		WillReturnRows(sqlmock.NewRows([]string{"column_name"}))
	mock.ExpectExec(`ALTER TABLE events ADD COLUMN group_name`).WillReturnError(errors.New("boom"))

	if err := d.initMySQLSchema(); err == nil {
		t.Fatal("expected an error")
	}

	expectationsMet(t, mock)
}

func TestInitMySQLSchema_EventsSignificanceLevelIDColumnError(t *testing.T) {
	d, mock := newMockDB(t, driverMySQL)

	columnPresent := func() {
		mock.ExpectQuery(`column_name FROM information_schema`).
			WillReturnRows(sqlmock.NewRows([]string{"column_name"}).AddRow("present"))
	}

	collationCorrect := func() {
		mock.ExpectQuery(`collation_name FROM information_schema`).
			WillReturnRows(sqlmock.NewRows([]string{"collation_name"}).AddRow(mysqlBinaryCollation))
	}

	mock.ExpectExec(`CREATE TABLE IF NOT EXISTS events`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`CREATE TABLE IF NOT EXISTS memories`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`CREATE TABLE IF NOT EXISTS significance_levels`).WillReturnResult(sqlmock.NewResult(0, 0))

	columnPresent()
	columnPresent()
	columnPresent()

	for range 5 {
		collationCorrect()
	}

	// memories.significance_level_id present, events.significance_level_id missing and its ALTER
	// fails.
	columnPresent()
	mock.ExpectQuery(`column_name FROM information_schema`).
		WillReturnRows(sqlmock.NewRows([]string{"column_name"}))
	mock.ExpectExec(`ALTER TABLE events ADD COLUMN significance_level_id`).WillReturnError(errors.New("boom"))

	if err := d.initMySQLSchema(); err == nil {
		t.Fatal("expected an error")
	}

	expectationsMet(t, mock)
}
