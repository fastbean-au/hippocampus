package db

import (
	"context"
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
