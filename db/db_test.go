package db

import (
	"testing"

	"github.com/fastbean-au/hippocampus/types"
)

// TestReopenDurability verifies that written data survives closing and reopening a file-backed
// database without any explicit snapshot or preserve step: WAL mode makes every acknowledged
// write durable.
func TestReopenDurability(t *testing.T) {
	dir := t.TempDir()

	db, err := New(dir)
	if err != nil {
		t.Fatalf("failed to create file-backed DB: %s", err)
	}

	if _, err := db.CreateEvent(types.Event{Id: "e1", Name: "an event", TimeStart: 100, Significance: 1}); err != nil {
		t.Fatalf("CreateEvent: %s", err)
	}

	if _, err := db.CreateMemory(types.Memory{Id: "m1", TimeStamp: 100, Significance: 1, EventId: "e1", Body: "remember me"}); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	if err := db.Close(); err != nil {
		t.Fatalf("Close: %s", err)
	}

	reopened, err := New(dir)
	if err != nil {
		t.Fatalf("failed to reopen DB: %s", err)
	}
	defer func() { _ = reopened.Close() }()

	event, err := reopened.GetEvent("e1")
	if err != nil {
		t.Fatalf("GetEvent after reopen: %s", err)
	}

	if event.Name != "an event" {
		t.Errorf("expected event name 'an event', got '%s'", event.Name)
	}

	memories, err := reopened.GetMemoriesByEventId("e1")
	if err != nil {
		t.Fatalf("GetMemoriesByEventId after reopen: %s", err)
	}

	if len(*memories) != 1 || (*memories)[0].Body != "remember me" {
		t.Errorf("expected memory m1 with body 'remember me', got %+v", *memories)
	}
}

// TestVerifyInstanceLock_NoLockConnIsNoOp confirms the instance-lock keepalive check is inert for
// backends that hold no lock connection - SQLite and the read-only opens - so it never fatally
// mishandles a nil lockConn. The server-driver recovery behaviour is covered by the
// Postgres/MySQL integration tests.
func TestVerifyInstanceLock_NoLockConnIsNoOp(t *testing.T) {
	for _, driver := range []driver{driverSQLite, driverPostgres, driverMySQL} {
		d := &DB{driver: driver}

		if err := d.verifyInstanceLock(); err != nil {
			t.Errorf("verifyInstanceLock with no lock connection (driver %d) returned %s, want nil", driver, err)
		}
	}
}

// TestAutoVacuumEnabled exposes a bug where auto_vacuum never took effect: the journal-mode
// pragma in the DSN initialises the database before initSchema runs, and auto_vacuum can only
// be changed on a completely empty database (or by a subsequent VACUUM). With auto_vacuum off,
// every incremental_vacuum in Preserve is a silent no-op and the file never shrinks.
func TestAutoVacuumEnabled(t *testing.T) {
	db, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("failed to create file-backed DB: %s", err)
	}
	defer func() { _ = db.Close() }()

	var av int
	if err := db.sql.QueryRow(`PRAGMA auto_vacuum`).Scan(&av); err != nil {
		t.Fatalf("PRAGMA auto_vacuum: %s", err)
	}

	// 2 = INCREMENTAL
	if av != 2 {
		t.Errorf("expected auto_vacuum INCREMENTAL (2), got %d — incremental_vacuum is a no-op", av)
	}
}

// TestPreserveReleasesFreeSpace verifies that Preserve returns the space freed by deletions to
// the filesystem: after deleting a multi-page memory and preserving, the freelist must be empty.
func TestPreserveReleasesFreeSpace(t *testing.T) {
	db, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("failed to create file-backed DB: %s", err)
	}
	defer func() { _ = db.Close() }()

	body := make([]byte, 256*1024)
	if _, err := db.CreateMemory(types.Memory{Id: "big", TimeStamp: 100, Significance: 1, Body: string(body)}); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	if err := db.DeleteMemory("big"); err != nil {
		t.Fatalf("DeleteMemory: %s", err)
	}

	if err := db.Preserve(); err != nil {
		t.Fatalf("Preserve: %s", err)
	}

	var freelist int64
	if err := db.sql.QueryRow(`PRAGMA freelist_count`).Scan(&freelist); err != nil {
		t.Fatalf("PRAGMA freelist_count: %s", err)
	}

	if freelist != 0 {
		t.Errorf("expected an empty freelist after Preserve, got %d pages still unreleased", freelist)
	}
}

// TestWALBytes_GrowsThenShrinksOnPreserve verifies that WALBytes reads the on-disk WAL file
// directly: it grows as writes land and drops back down once Preserve checkpoints and truncates
// it, without needing a database connection to observe either.
func TestWALBytes_GrowsThenShrinksOnPreserve(t *testing.T) {
	db, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("failed to create file-backed DB: %s", err)
	}
	defer func() { _ = db.Close() }()

	before, err := db.WALBytes()
	if err != nil {
		t.Fatalf("WALBytes (before writes): %s", err)
	}

	body := make([]byte, 256*1024)
	if _, err := db.CreateMemory(types.Memory{Id: "big", TimeStamp: 100, Significance: 1, Body: string(body)}); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	grown, err := db.WALBytes()
	if err != nil {
		t.Fatalf("WALBytes (after write): %s", err)
	}

	if grown <= before {
		t.Fatalf("expected WALBytes to grow after a write, got %d (was %d)", grown, before)
	}

	if err := db.Preserve(); err != nil {
		t.Fatalf("Preserve: %s", err)
	}

	after, err := db.WALBytes()
	if err != nil {
		t.Fatalf("WALBytes (after Preserve): %s", err)
	}

	if after >= grown {
		t.Errorf("expected WALBytes to shrink after Preserve's checkpoint, got %d (was %d)", after, grown)
	}
}

// TestWALBytes_InMemoryDatabase verifies that WALBytes is a harmless no-op for the in-memory
// database used by tests, which has no WAL file on disk.
func TestWALBytes_InMemoryDatabase(t *testing.T) {
	db := newTestDB(t)

	n, err := db.WALBytes()
	if err != nil {
		t.Fatalf("WALBytes: %s", err)
	}

	if n != 0 {
		t.Errorf("expected 0 for an in-memory database, got %d", n)
	}
}

// TestPurge verifies that Purge removes all events and memories and that the store remains
// usable afterwards.
func TestPurge(t *testing.T) {
	db := newTestDB(t)

	if _, err := db.CreateEvent(types.Event{Id: "e1", Name: "an event", TimeStart: 100, Significance: 1}); err != nil {
		t.Fatalf("CreateEvent: %s", err)
	}

	if _, err := db.CreateMemory(types.Memory{Id: "m1", TimeStamp: 100, Significance: 1, Body: "x"}); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	if err := db.Purge(); err != nil {
		t.Fatalf("Purge: %s", err)
	}

	if db.CountEvents() != 0 {
		t.Errorf("expected 0 events after purge, got %d", db.CountEvents())
	}

	with, without := db.CountMemories()
	if with != 0 || without != 0 {
		t.Errorf("expected 0 memories after purge, got %d with events, %d without", with, without)
	}

	if _, err := db.CreateMemory(types.Memory{Id: "m2", TimeStamp: 100, Significance: 1, Body: "y"}); err != nil {
		t.Errorf("store should be usable after purge: %s", err)
	}
}

// TestNewSQLiteReadOnly verifies that the backfill tool's read-only SQLite open reads an
// existing database, rejects writes, and skips Preserve (no checkpoint/vacuum on close), so it can
// run beside a live service without contending for writes.
func TestNewSQLiteReadOnly(t *testing.T) {
	dir := t.TempDir()

	// Seed a database the normal (writable) way, then close it.
	writable, err := New(dir)
	if err != nil {
		t.Fatalf("New: %s", err)
	}

	if _, err := writable.CreateMemory(types.Memory{Id: "m1", TimeStamp: 100, Significance: 5, Body: "hello"}); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	if err := writable.Close(); err != nil {
		t.Fatalf("Close: %s", err)
	}

	ro, err := NewSQLiteReadOnly(dir)
	if err != nil {
		t.Fatalf("NewSQLiteReadOnly: %s", err)
	}
	defer func() { _ = ro.Close() }()

	// Reads work.
	got, err := ro.GetMemoriesByIds([]string{"m1"})
	if err != nil {
		t.Fatalf("GetMemoriesByIds: %s", err)
	}

	if len(*got) != 1 || (*got)[0].Body != "hello" {
		t.Errorf("expected to read m1 with body 'hello', got %+v", *got)
	}

	// Writes are rejected by the mode=ro open.
	if _, err := ro.CreateMemory(types.Memory{Id: "m2", TimeStamp: 200, Significance: 5, Body: "nope"}); err == nil {
		t.Error("expected a write to a read-only database to fail")
	}

	// Preserve must be a no-op - no checkpoint/incremental-vacuum against a possibly-live database.
	if err := ro.Preserve(); err != nil {
		t.Errorf("Preserve on a read-only database should be a no-op, got: %s", err)
	}
}

// TestNewSQLiteReadOnly_MissingDatabase verifies a read-only open of a database that does not exist
// fails fast rather than creating an empty one.
func TestNewSQLiteReadOnly_MissingDatabase(t *testing.T) {
	if _, err := NewSQLiteReadOnly(t.TempDir()); err == nil {
		t.Error("expected a read-only open of a nonexistent database to fail")
	}
}
