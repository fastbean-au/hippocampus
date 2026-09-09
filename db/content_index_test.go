package db

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/fastbean-au/hippocampus/types"
)

// The content index is the largest non-body cost this store carries, and a deployment running
// OpenSearch reads none of it. WithoutContentIndex is how that cost is declined; what follows is
// what the option has to be true of on every dialect.
//
// This file opens its own stores rather than going through newTestDB, and is on
// sqliteOnlyTestFiles' allow-list for it, but only because it must CLOSE AND REOPEN one - the
// option is a constructor option, so both directions of the setting are only observable across a
// reopen. It is not SQLite-only in any other sense: reopenableStore below resolves the dialect the
// same way newTestDB does, and every test here runs under all three.

// reopenableStore returns an opener for a store that survives being closed - a directory-backed
// SQLite database, or the server dialect's own DSN - since the shared harness's in-memory SQLite
// store is a different database on every open.
//
// The caller closes what it opens: two open handles on one SQLite directory would meet the storage
// lock, which is the very thing that makes the reopen worth testing rather than a formality.
//
// The FIRST open empties the store and no later one does, which is the whole difference between
// this and newTestDB. The server dialects share one database, so a test that did not empty it would
// be searching a store carrying every other test's memories; but emptying on every open would
// delete the rows the reopen exists to look at.
func reopenableStore(t *testing.T) func(opts ...Option) *DB {
	t.Helper()

	open := dialectOpener(t)
	first := true

	return func(opts ...Option) *DB {
		d := open(opts...)

		if first {
			resetTestStore(t, d)

			first = false
		}

		return d
	}
}

// dialectOpener resolves the dialect exactly as newTestDB does and returns the raw open, skipping
// when a server dialect's DSN is unset.
func dialectOpener(t *testing.T) func(opts ...Option) *DB {
	t.Helper()

	switch testDialect(t) {

	case driverPostgres:
		dsn := serverTestDSN(t, postgresTestDSNEnv)

		return func(opts ...Option) *DB {
			d, err := NewPostgres(dsn, false, opts...)
			if err != nil {
				t.Fatalf("opening the test database: %s", err)
			}

			return d
		}

	case driverMySQL:
		dsn := serverTestDSN(t, mysqlTestDSNEnv)

		return func(opts ...Option) *DB {
			d, err := NewMySQL(dsn, false, opts...)
			if err != nil {
				t.Fatalf("opening the test database: %s", err)
			}

			return d
		}

	}

	directory := t.TempDir()

	return func(opts ...Option) *DB {
		d, err := New(directory, opts...)
		if err != nil {
			t.Fatalf("opening the test database: %s", err)
		}

		return d
	}
}

// serverTestDSN reads a server dialect's DSN, skipping when it is unset.
func serverTestDSN(t *testing.T, dsnEnv string) string {
	t.Helper()

	dsn := os.Getenv(dsnEnv)
	if dsn == "" {
		t.Skipf("set %s to run the shared suite against this dialect", dsnEnv)
	}

	return dsn
}

// contentIndexExists probes for the index table itself, rather than for the flag that says whether
// it should be there. Every dialect errors on a missing table, so one query answers for all three.
func contentIndexExists(t *testing.T, d *DB) bool {
	t.Helper()

	var n int

	err := d.sql.QueryRow(`SELECT count(*) FROM ` + contentSearchTable).Scan(&n)

	return err == nil
}

// TestWithoutContentIndexDropsAnExistingIndex is the option's whole point, and the reason it drops
// rather than merely stopping the writes: an index that stops being maintained does not become
// empty, it becomes wrong, and would go on answering searches from a subset that shrinks with every
// consolidation cycle. Dropped, the store refuses by name.
func TestWithoutContentIndexDropsAnExistingIndex(t *testing.T) {
	open := reopenableStore(t)

	d := open()
	storeMemory(t, d, "m1", "deployment rollout", "")

	if ids := searchIds(t, d, ContentQuery{Text: "deployment"}); len(ids) != 1 {
		t.Fatalf("the index did not answer before it was dropped: got %v", ids)
	}

	_ = d.Close()

	d = open(WithoutContentIndex())
	defer func() { _ = d.Close() }()

	if d.ContentSearchAvailable() {
		t.Error("ContentSearchAvailable is true on a store opened WithoutContentIndex")
	}

	if contentIndexExists(t, d) {
		t.Error("the content index table survived a WithoutContentIndex open")
	}

	// Refused by name rather than answered with nothing: an operator who expected search to work is
	// told it is unavailable instead of concluding their store is empty.
	if _, err := d.SearchMemoryHits(context.Background(), ContentQuery{Text: "deployment", Limit: 10}); !errors.Is(err, ErrContentSearchUnavailable) {
		t.Errorf("SearchMemoryHits: got %v, want ErrContentSearchUnavailable", err)
	}
}

// TestWithoutContentIndexKeepsDeletesWorking is the trap the drop has to avoid, and it is a SQLite
// trigger's: a trigger's body is resolved when it fires, not when it is created, so one left
// pointing at a dropped table turns every DELETE from memories into an error - which is
// consolidation, eviction, Clear and Purge all failing at once, on a store whose whole premise is
// deleting things. The server dialects have no trigger and pass this trivially; running it on all
// three is what makes the guard survive a fourth dialect.
func TestWithoutContentIndexKeepsDeletesWorking(t *testing.T) {
	open := reopenableStore(t)

	d := open()
	storeMemory(t, d, "m1", "deployment rollout", "")
	_ = d.Close()

	d = open(WithoutContentIndex())
	defer func() { _ = d.Close() }()

	storeMemory(t, d, "m2", "second memory", "")

	deleted, err := d.DeleteMemories(context.Background(), []string{"m1", "m2"})
	if err != nil {
		t.Fatalf("DeleteMemories with the content index dropped: %s", err)
	}

	if deleted != 2 {
		t.Errorf("deleted %d memories, want 2", deleted)
	}
}

// TestWithoutContentIndexStillAcceptsWrites: with the index gone, every write path that would have
// maintained it has to carry on as though it had never existed. A create, an update and a purge,
// since those are the three that hook the index explicitly.
func TestWithoutContentIndexStillAcceptsWrites(t *testing.T) {
	open := reopenableStore(t)

	d := open(WithoutContentIndex())
	defer func() { _ = d.Close() }()

	ctx := context.Background()

	storeMemory(t, d, "m1", "deployment rollout", "")

	memory := types.Memory{Id: "m1", TimeStamp: 1, Significance: 5, Body: "rewritten body"}

	updated, err := d.UpdateMemory(ctx, memory)
	if err != nil {
		t.Fatalf("UpdateMemory with the content index dropped: %s", err)
	}

	if !updated {
		t.Fatal("UpdateMemory reported no row updated")
	}

	got, err := d.GetMemoriesByIds(ctx, []string{"m1"})
	if err != nil {
		t.Fatalf("GetMemoriesByIds: %s", err)
	}

	if len(*got) != 1 || (*got)[0].Body != "rewritten body" {
		t.Errorf("read back %v, want the updated body", *got)
	}

	if err := d.Purge(ctx); err != nil {
		t.Fatalf("Purge with the content index dropped: %s", err)
	}
}

// TestContentIndexReturnsWhenReEnabled: the setting is reversible on any startup, and turning it
// back on has to repopulate rather than leave an empty index answering nothing. That path is
// already initContentSearch's - it is how a store written before content search existed gains it -
// so what this pins is that dropping and recreating reaches the same place.
func TestContentIndexReturnsWhenReEnabled(t *testing.T) {
	open := reopenableStore(t)

	d := open(WithoutContentIndex())
	storeMemory(t, d, "m1", "deployment rollout", "")
	_ = d.Close()

	d = open()
	defer func() { _ = d.Close() }()

	if !d.ContentSearchAvailable() {
		t.Fatal("ContentSearchAvailable is false after the index was re-enabled")
	}

	// Written while the index was gone, so it is findable only if the reopen backfilled it.
	if ids := searchIds(t, d, ContentQuery{Text: "deployment"}); len(ids) != 1 || ids[0] != "m1" {
		t.Errorf("the re-enabled index did not backfill: got %v, want [m1]", ids)
	}
}

// TestWithoutContentIndexRefusesARebuild: --backfill-search opens the store read-WRITE, and against
// a store carrying no index there is nothing to rebuild into. It has to refuse by name rather than
// fail on whichever "no such table" the dialect spells, since that message is what tells an operator
// which setting is in the way.
func TestWithoutContentIndexRefusesARebuild(t *testing.T) {
	open := reopenableStore(t)

	d := open(WithoutContentIndex())
	defer func() { _ = d.Close() }()

	if err := d.RebuildContentSearch(context.Background()); !errors.Is(err, ErrContentSearchUnavailable) {
		t.Errorf("RebuildContentSearch: got %v, want ErrContentSearchUnavailable", err)
	}
}

// TestWithoutContentIndexLeavesTheSchemaVersionAlone. The migration that carries the index is
// recorded either way, and that is deliberate: gating it on the setting would move the store's
// schema version up and down as the key changed, and a build meeting the higher of the two would
// refuse to open - with ErrSchemaTooNew - a store it understands perfectly.
func TestWithoutContentIndexLeavesTheSchemaVersionAlone(t *testing.T) {
	open := reopenableStore(t)

	d := open()
	want := d.schemaVersion()

	applied, err := d.appliedMigrations()
	if err != nil {
		t.Fatalf("appliedMigrations: %s", err)
	}

	wantApplied := newestVersion(applied)

	_ = d.Close()

	d = open(WithoutContentIndex())
	defer func() { _ = d.Close() }()

	if got := d.schemaVersion(); got != want {
		t.Errorf("schemaVersion is %d without the content index, want %d", got, want)
	}

	applied, err = d.appliedMigrations()
	if err != nil {
		t.Fatalf("appliedMigrations: %s", err)
	}

	if got := newestVersion(applied); got != wantApplied {
		t.Errorf("the recorded version is %d without the content index, want %d", got, wantApplied)
	}
}
