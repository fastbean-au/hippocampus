package db

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/fastbean-au/hippocampus/types"
)

// createMemoryWithSignificance stores a memory at an absolute significance (resolved through the
// registry by CreateMemory) and returns its id.
func createMemoryWithSignificance(t *testing.T, d *DB, id string, significance int32) {
	t.Helper()

	if _, err := d.CreateMemory(context.Background(), types.Memory{
		Id:           id,
		Body:         "b",
		Significance: significance,
		TimeStamp:    time.Now().UnixNano(),
	}); err != nil {
		t.Fatalf("create memory %s: %s", id, err)
	}
}

// significanceOf reads a memory's current significance (its level's rank) back through the store.
func significanceOf(t *testing.T, d *DB, id string) int32 {
	t.Helper()

	ms, err := d.GetMemoriesByIds(context.Background(), []string{id})
	if err != nil {
		t.Fatalf("get memory %s: %s", id, err)
	}

	if len(*ms) != 1 {
		t.Fatalf("expected 1 memory for %s, got %d", id, len(*ms))
	}

	return (*ms)[0].Significance
}

// assertRegistryPlacement exercises the registry's shared behaviour - absolute find-or-create, a
// placement gap-open across consecutive ranks (the dialect's openGapAt two-phase shift and registry
// lock), and unranked (NULL level) - against any driver's *DB, so the Postgres/MySQL integration
// tests can reuse it. It assumes an empty store.
func assertRegistryPlacement(t *testing.T, d *DB) {
	t.Helper()

	ctx := context.Background()

	for i, id := range []string{"m5", "m6", "m7", "m8"} {
		createMemoryWithSignificance(t, d, id, int32(5+i))
	}

	// "above 5" with 6,7,8 all present must shift them to 7,8,9 and land the new memory at 6.
	levelID, rank, err := d.ResolveSignificanceLevel(ctx, SignificanceSpec{
		Placement:  PlacementAbove,
		Anchor:     5,
		AnchorKind: AnchorMemory,
	})
	if err != nil {
		t.Fatalf("resolve above: %s", err)
	}

	if rank != 6 {
		t.Fatalf("resolved rank = %d, want 6", rank)
	}

	id := levelID.Int64
	if _, err := d.CreateMemory(ctx, types.Memory{Id: "new", Body: "b", SignificanceLevelID: &id, TimeStamp: time.Now().UnixNano()}); err != nil {
		t.Fatalf("create new: %s", err)
	}

	for mid, want := range map[string]int32{"m5": 5, "new": 6, "m6": 7, "m7": 8, "m8": 9} {
		if got := significanceOf(t, d, mid); got != want {
			t.Fatalf("%s significance = %d, want %d", mid, got, want)
		}
	}

	createMemoryWithSignificance(t, d, "unranked", 0)

	if got := significanceOf(t, d, "unranked"); got != 0 {
		t.Fatalf("unranked significance = %d, want 0", got)
	}
}

// assertMigratedRegistry checks the state a migration from the pre-registry schema must produce
// (see the seed rows m1/m2/m3 and event e1 in the migration tests): the old column is gone and the
// ranks are preserved, including unranked (m3). Shared by the SQLite/Postgres/MySQL migration tests.
func assertMigratedRegistry(t *testing.T, d *DB) {
	t.Helper()

	if exists, err := d.columnExists("memories", "significance"); err != nil || exists {
		t.Fatalf("old significance column still present (exists=%v, err=%v)", exists, err)
	}

	for id, want := range map[string]int32{"m1": 7, "m2": 3, "m3": 0} {
		if got := significanceOf(t, d, id); got != want {
			t.Fatalf("%s significance after migration = %d, want %d", id, got, want)
		}
	}

	ev, err := d.GetEvent(context.Background(), "e1")
	if err != nil {
		t.Fatalf("get migrated event: %s", err)
	}

	if ev.Significance != 5 {
		t.Fatalf("event significance after migration = %d, want 5", ev.Significance)
	}
}

func TestPurgeClearsRegistry(t *testing.T) {
	d := newTestDB(t)
	defer func() { _ = d.Close() }()

	createMemoryWithSignificance(t, d, "a", 5)

	if err := d.Purge(context.Background()); err != nil {
		t.Fatalf("purge: %s", err)
	}

	var count int
	if err := d.sql.QueryRow(`SELECT COUNT(*) FROM significance_levels`).Scan(&count); err != nil {
		t.Fatal(err)
	}

	if count != 0 {
		t.Fatalf("registry not cleared by Purge: %d levels remain", count)
	}
}

func TestResolveSignificanceLevel_AbsoluteSharesLevel(t *testing.T) {
	d := newTestDB(t)
	defer func() { _ = d.Close() }()

	createMemoryWithSignificance(t, d, "a", 5)
	createMemoryWithSignificance(t, d, "b", 5)

	// Both memories at significance 5 share the one level, and read back as 5.
	if got := significanceOf(t, d, "a"); got != 5 {
		t.Fatalf("memory a significance = %d, want 5", got)
	}

	if got := significanceOf(t, d, "b"); got != 5 {
		t.Fatalf("memory b significance = %d, want 5", got)
	}

	var count int
	if err := d.sql.QueryRow(`SELECT COUNT(*) FROM significance_levels WHERE level_rank = 5`).Scan(&count); err != nil {
		t.Fatal(err)
	}

	if count != 1 {
		t.Fatalf("expected exactly one level at rank 5, got %d", count)
	}
}

func TestResolveSignificanceLevel_UnrankedIsNull(t *testing.T) {
	d := newTestDB(t)
	defer func() { _ = d.Close() }()

	createMemoryWithSignificance(t, d, "u", 0)

	if got := significanceOf(t, d, "u"); got != 0 {
		t.Fatalf("unranked memory significance = %d, want 0", got)
	}

	id, rank, err := d.ResolveSignificanceLevel(context.Background(), SignificanceSpec{Value: 0})
	if err != nil {
		t.Fatal(err)
	}

	if id.Valid || rank != 0 {
		t.Fatalf("resolve of value 0 = (%v, %d), want (invalid, 0)", id, rank)
	}
}

func TestPlacement_AboveOpensGapUpward(t *testing.T) {
	d := newTestDB(t)
	defer func() { _ = d.Close() }()

	createMemoryWithSignificance(t, d, "five", 5)
	createMemoryWithSignificance(t, d, "six", 6)

	// Place a new memory just above the 5s. Ranks 5 and 6 are adjacent, so the gap is opened
	// upward: the old rank-6 shifts to 7, the new memory takes 6.
	levelID, rank, err := d.ResolveSignificanceLevel(context.Background(), SignificanceSpec{
		Placement:  PlacementAbove,
		Anchor:     5,
		AnchorKind: AnchorMemory,
	})
	if err != nil {
		t.Fatalf("resolve above: %s", err)
	}

	if rank != 6 {
		t.Fatalf("placement above 5 resolved rank = %d, want 6", rank)
	}

	id := levelID.Int64
	if _, err := d.CreateMemory(context.Background(), types.Memory{
		Id:                  "between",
		Body:                "b",
		SignificanceLevelID: &id,
		TimeStamp:           time.Now().UnixNano(),
	}); err != nil {
		t.Fatal(err)
	}

	// Order is preserved: six (now 7) > between (6) > five (5).
	if got := significanceOf(t, d, "six"); got != 7 {
		t.Fatalf("six significance = %d, want 7 (shifted up)", got)
	}

	if got := significanceOf(t, d, "between"); got != 6 {
		t.Fatalf("between significance = %d, want 6", got)
	}

	if got := significanceOf(t, d, "five"); got != 5 {
		t.Fatalf("five significance = %d, want 5 (unchanged)", got)
	}

	// Ordering by significance surfaces them highest-first.
	ms, err := d.GetMemories(context.Background(), MemoryFilter{})
	if err != nil {
		t.Fatal(err)
	}

	order := make([]string, 0, len(*ms))
	for _, m := range *ms {
		order = append(order, m.Id)
	}

	want := []string{"six", "between", "five"}
	for i := range want {
		if i >= len(order) || order[i] != want[i] {
			t.Fatalf("order = %v, want %v", order, want)
		}
	}
}

// TestPlacement_ShiftAcrossConsecutiveRanks pins the fix for a UNIQUE-collision bug: opening a gap
// when the ranks at/above the target are consecutive must not transiently violate the rank UNIQUE
// constraint (a naive single UPDATE ... rank+1 does on SQLite).
func TestPlacement_ShiftAcrossConsecutiveRanks(t *testing.T) {
	d := newTestDB(t)
	defer func() { _ = d.Close() }()

	// Consecutive ranks 5,6,7,8, all in use.
	for i, id := range []string{"m5", "m6", "m7", "m8"} {
		createMemoryWithSignificance(t, d, id, int32(5+i))
	}

	// "above 5" must shift 6,7,8 up to 7,8,9 and land the new memory at 6.
	levelID, rank, err := d.ResolveSignificanceLevel(context.Background(), SignificanceSpec{
		Placement:  PlacementAbove,
		Anchor:     5,
		AnchorKind: AnchorMemory,
	})
	if err != nil {
		t.Fatalf("resolve above across consecutive ranks: %s", err)
	}

	if rank != 6 {
		t.Fatalf("resolved rank = %d, want 6", rank)
	}

	id := levelID.Int64
	if _, err := d.CreateMemory(context.Background(), types.Memory{Id: "new", Body: "b", SignificanceLevelID: &id, TimeStamp: time.Now().UnixNano()}); err != nil {
		t.Fatal(err)
	}

	want := map[string]int32{"m5": 5, "new": 6, "m6": 7, "m7": 8, "m8": 9}
	for mid, wantSig := range want {
		if got := significanceOf(t, d, mid); got != wantSig {
			t.Fatalf("%s significance = %d, want %d", mid, got, wantSig)
		}
	}
}

func TestPlacement_BetweenUsesFreeSlot(t *testing.T) {
	d := newTestDB(t)
	defer func() { _ = d.Close() }()

	createMemoryWithSignificance(t, d, "low", 5)
	createMemoryWithSignificance(t, d, "high", 8)

	// A free integer (6) exists strictly between 5 and 8, so no shift is needed.
	_, rank, err := d.ResolveSignificanceLevel(context.Background(), SignificanceSpec{
		Placement:  PlacementBetween,
		Anchor:     5,
		Upper:      8,
		AnchorKind: AnchorMemory,
		UpperKind:  AnchorMemory,
	})
	if err != nil {
		t.Fatalf("resolve between: %s", err)
	}

	if rank != 6 {
		t.Fatalf("between 5 and 8 resolved rank = %d, want 6", rank)
	}

	if got := significanceOf(t, d, "high"); got != 8 {
		t.Fatalf("high significance = %d, want 8 (no shift needed)", got)
	}
}

func TestPlacement_AnchorById(t *testing.T) {
	d := newTestDB(t)
	defer func() { _ = d.Close() }()

	createMemoryWithSignificance(t, d, "anchor", 10)

	_, rank, err := d.ResolveSignificanceLevel(context.Background(), SignificanceSpec{
		Placement:  PlacementAbove,
		AnchorID:   "anchor",
		AnchorKind: AnchorMemory,
	})
	if err != nil {
		t.Fatalf("resolve above id: %s", err)
	}

	if rank != 11 {
		t.Fatalf("above anchor (rank 10) resolved rank = %d, want 11", rank)
	}
}

func TestPlacement_UnknownAnchorErrors(t *testing.T) {
	d := newTestDB(t)
	defer func() { _ = d.Close() }()

	_, _, err := d.ResolveSignificanceLevel(context.Background(), SignificanceSpec{
		Placement:  PlacementAbove,
		AnchorID:   "nonexistent",
		AnchorKind: AnchorMemory,
	})
	if !errors.Is(err, ErrInvalidPlacement) {
		t.Fatalf("resolve with unknown anchor err = %v, want ErrInvalidPlacement", err)
	}
}

func TestCompactSignificanceLevels(t *testing.T) {
	d := newTestDB(t)
	defer func() { _ = d.Close() }()

	// Seed a level well above the compaction threshold and a couple of ordinary ones, then a memory
	// referencing the inflated level so its ordering must be preserved across compaction.
	inflated := registryCompactionThreshold + 100

	if _, err := d.sql.Exec(`INSERT INTO significance_levels (level_rank) VALUES (2), (5), (?)`, inflated); err != nil {
		t.Fatal(err)
	}

	var topID int64
	if err := d.sql.QueryRow(`SELECT id FROM significance_levels WHERE level_rank = ?`, inflated).Scan(&topID); err != nil {
		t.Fatal(err)
	}

	if _, err := d.CreateMemory(context.Background(), types.Memory{
		Id:                  "top",
		Body:                "b",
		SignificanceLevelID: &topID,
		TimeStamp:           time.Now().UnixNano(),
	}); err != nil {
		t.Fatal(err)
	}

	if err := d.CompactSignificanceLevels(context.Background()); err != nil {
		t.Fatalf("compact: %s", err)
	}

	var maxRank int32
	if err := d.sql.QueryRow(`SELECT MAX(level_rank) FROM significance_levels`).Scan(&maxRank); err != nil {
		t.Fatal(err)
	}

	// Three levels renumbered to consecutive 1..3; the previously-inflated one is now the top (3).
	if maxRank != 3 {
		t.Fatalf("max rank after compaction = %d, want 3", maxRank)
	}

	if got := significanceOf(t, d, "top"); got != 3 {
		t.Fatalf("top memory significance after compaction = %d, want 3", got)
	}
}

// TestMigrateSignificanceToLevels drives initSchema against a database still carrying the old
// per-item significance column, asserting the ranks are preserved through the migration and the old
// column is dropped.
func TestMigrateSignificanceToLevels(t *testing.T) {
	sqlDB, err := sql.Open("sqlite", "file::memory:?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}

	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)
	defer func() { _ = sqlDB.Close() }()

	// The pre-registry schema: a per-item significance column and the old covering index over it.
	old := []string{
		`CREATE TABLE events (id TEXT PRIMARY KEY, time_start INTEGER NOT NULL DEFAULT 0, time_end INTEGER NOT NULL DEFAULT 0,
			significance INTEGER NOT NULL DEFAULT 0, name TEXT NOT NULL DEFAULT '', description TEXT NOT NULL DEFAULT '',
			memories_consolidated INTEGER NOT NULL DEFAULT 0, relationship_significance INTEGER NOT NULL DEFAULT 0,
			relationships TEXT NOT NULL DEFAULT '[]', group_name TEXT NOT NULL DEFAULT '')`,
		`CREATE TABLE memories (id TEXT PRIMARY KEY, timestamp INTEGER NOT NULL DEFAULT 0, significance INTEGER NOT NULL DEFAULT 0,
			event_id TEXT NOT NULL DEFAULT '', is_binary INTEGER NOT NULL DEFAULT 0, time_recalled INTEGER NOT NULL DEFAULT 0,
			recall_count INTEGER NOT NULL DEFAULT 0, is_summary INTEGER NOT NULL DEFAULT 0, group_name TEXT NOT NULL DEFAULT '',
			body BLOB NOT NULL DEFAULT x'')`,
		`CREATE INDEX idx_memories_consolidation ON memories (event_id, timestamp, significance, time_recalled, recall_count)`,
		`INSERT INTO memories (id, timestamp, significance, body) VALUES ('m1', 1, 7, x'')`,
		`INSERT INTO memories (id, timestamp, significance, body) VALUES ('m2', 1, 3, x'')`,
		`INSERT INTO memories (id, timestamp, significance, event_id, body) VALUES ('m3', 1, 0, '', x'')`,
		`INSERT INTO events (id, significance, name) VALUES ('e1', 5, 'evt')`,
	}

	for _, stmt := range old {
		if _, err := sqlDB.Exec(stmt); err != nil {
			t.Fatalf("seed old schema: %s", err)
		}
	}

	d := &DB{sql: sqlDB, driver: driverSQLite}

	if err := d.initSchema(); err != nil {
		t.Fatalf("initSchema (migration): %s", err)
	}

	assertMigratedRegistry(t, d)
}
