package db

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/fastbean-au/hippocampus/types"
)

// markOf reads a level's unused mark straight out of the registry, which is the state the two
// phases of the reap are actually about and which nothing above this package can see.
func markOf(t *testing.T, d *DB, rank int32) sql.NullInt64 {
	t.Helper()

	var since sql.NullInt64

	query := d.rebind(
		`SELECT ` + significanceLevelUnusedColumn + ` FROM significance_levels WHERE level_rank = ?`,
	)

	if err := d.sql.QueryRow(query, rank).Scan(&since); err != nil {
		t.Fatalf("reading the mark on rank %d: %s", rank, err)
	}

	return since
}

// rankExists reports whether the registry still carries a level at a rank.
func rankExists(t *testing.T, d *DB, rank int32) bool {
	t.Helper()

	var count int

	query := d.rebind(`SELECT COUNT(*) FROM significance_levels WHERE level_rank = ?`)

	if err := d.sql.QueryRow(query, rank).Scan(&count); err != nil {
		t.Fatalf("counting levels at rank %d: %s", rank, err)
	}

	return count > 0
}

// ageMark backdates a level's unused mark, standing in for the days a real store would take to
// reach the same state. The reap's cutoff is the only thing that reads it.
func ageMark(t *testing.T, d *DB, rank int32, age time.Duration) {
	t.Helper()

	query := d.rebind(
		`UPDATE significance_levels SET ` + significanceLevelUnusedColumn + ` = ? WHERE level_rank = ?`,
	)

	if _, err := d.sql.Exec(query, time.Now().Add(-age).UnixNano(), rank); err != nil {
		t.Fatalf("backdating the mark on rank %d: %s", rank, err)
	}
}

// TestReapSignificanceLevels_MarksBeforeReaping is the two-phase contract: the cycle that first
// finds a level carrying nothing marks it and deletes nothing, and only a later cycle - one that
// finds it still unused and marked for longer than the retention - removes it.
//
// The delay is not tidiness. A level is handed out BEFORE anything references it (see the file
// comment in significance_reap.go), so a reap that deleted on first sight would eventually delete
// one mid-hand-out and leave a memory pointing at a level that is not there.
func TestReapSignificanceLevels_MarksBeforeReaping(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()

	createMemoryWithSignificance(t, d, "m1", 5)

	if _, err := d.DeleteMemories(ctx, []string{"m1"}); err != nil {
		t.Fatalf("delete memory: %s", err)
	}

	registry, err := d.ReapSignificanceLevels(ctx, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("first reap: %s", err)
	}

	if registry.Reaped != 0 {
		t.Errorf("the first pass reaped %d levels; it must only mark them", registry.Reaped)
	}

	if !markOf(t, d, 5).Valid {
		t.Fatal("the first pass left rank 5 unmarked, so no later pass can ever remove it")
	}

	// Still inside the retention window: marked, but not yet old enough to act on.
	registry, err = d.ReapSignificanceLevels(ctx, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("second reap: %s", err)
	}

	if registry.Reaped != 0 || !rankExists(t, d, 5) {
		t.Errorf("rank 5 was removed while still inside its retention window (reaped %d)", registry.Reaped)
	}

	ageMark(t, d, 5, 8*24*time.Hour)

	registry, err = d.ReapSignificanceLevels(ctx, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("third reap: %s", err)
	}

	if registry.Reaped != 1 {
		t.Errorf("reaped %d levels, want 1", registry.Reaped)
	}

	if rankExists(t, d, 5) {
		t.Error("rank 5 survived a reap although nothing carried it and its window had passed")
	}
}

// TestReapSignificanceLevels_LeavesLevelsInUse is the other half: a level something still carries is
// never marked and never removed, whatever the retention says.
func TestReapSignificanceLevels_LeavesLevelsInUse(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()

	createMemoryWithSignificance(t, d, "kept", 5)

	if _, err := d.CreateEvent(ctx, types.Event{
		Id:           "e1",
		Name:         "an event",
		Significance: 9,
		TimeStart:    time.Now().UnixNano(),
	}); err != nil {
		t.Fatalf("create event: %s", err)
	}

	// A retention of a nanosecond would reap anything reapable on the very next pass.
	for range 2 {
		if _, err := d.ReapSignificanceLevels(ctx, time.Nanosecond); err != nil {
			t.Fatalf("reap: %s", err)
		}
	}

	for _, rank := range []int32{5, 9} {
		if !rankExists(t, d, rank) {
			t.Errorf("rank %d was reaped although an item still carries it", rank)
		}

		if markOf(t, d, rank).Valid {
			t.Errorf("rank %d was marked unused although an item still carries it", rank)
		}
	}

	if significanceOf(t, d, "kept") != 5 {
		t.Error("the memory lost its significance across a reap")
	}
}

// TestReapSignificanceLevels_HandOutClearsTheMark pins the writer's half of the race the mark
// exists for: resolving a marked level un-marks it, so the reap that comes next cannot act on the
// clock it had already started.
func TestReapSignificanceLevels_HandOutClearsTheMark(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()

	createMemoryWithSignificance(t, d, "m1", 5)

	if _, err := d.DeleteMemories(ctx, []string{"m1"}); err != nil {
		t.Fatalf("delete memory: %s", err)
	}

	if _, err := d.ReapSignificanceLevels(ctx, 7*24*time.Hour); err != nil {
		t.Fatalf("marking reap: %s", err)
	}

	ageMark(t, d, 5, 8*24*time.Hour)

	// A write at the same significance arrives before the reap that would have removed the level.
	createMemoryWithSignificance(t, d, "m2", 5)

	if markOf(t, d, 5).Valid {
		t.Fatal("resolving rank 5 left its unused mark in place, so the next reap would remove a level in use")
	}

	registry, err := d.ReapSignificanceLevels(ctx, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("reap: %s", err)
	}

	if registry.Reaped != 0 {
		t.Errorf("reaped %d levels; the level was claimed back before the window passed", registry.Reaped)
	}

	if significanceOf(t, d, "m2") != 5 {
		t.Error("the memory written against the reclaimed level did not read back at its significance")
	}
}

// TestReapSignificanceLevels_ClaimLosingTheRaceRebuildsTheLevel is the failure this whole design
// exists to prevent, driven directly: the level is deleted in the window between a writer reading
// it and that writer claiming it.
//
// The write must not store the id it read. It must find the rank gone, create the level again under
// the registry lock, and store a memory that reads back at its own significance - because the
// alternative is a row pointing at nothing, which every read resolves to unranked and which the
// next cycle then forgets first.
func TestReapSignificanceLevels_ClaimLosingTheRaceRebuildsTheLevel(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()

	createMemoryWithSignificance(t, d, "m1", 5)

	if _, err := d.DeleteMemories(ctx, []string{"m1"}); err != nil {
		t.Fatalf("delete memory: %s", err)
	}

	if _, err := d.ReapSignificanceLevels(ctx, 7*24*time.Hour); err != nil {
		t.Fatalf("marking reap: %s", err)
	}

	// Stand in for the reap landing after the read and before the claim: findLevel's UPDATE now
	// matches no row, which is exactly what the race produces.
	if _, err := d.sql.Exec(
		d.rebind(`DELETE FROM significance_levels WHERE level_rank = ?`),
		int32(5),
	); err != nil {
		t.Fatalf("removing the level: %s", err)
	}

	levelID, rank, err := d.ResolveSignificanceLevel(ctx, SignificanceSpec{Value: 5})
	if err != nil {
		t.Fatalf("resolve after the level was reaped: %s", err)
	}

	if !levelID.Valid || rank != 5 {
		t.Fatalf("resolve returned (%v, %d), want a valid level at rank 5", levelID, rank)
	}

	if _, err := d.CreateMemory(ctx, types.Memory{
		Id:                  "m2",
		Body:                "b",
		Significance:        5,
		SignificanceLevelID: &levelID.Int64,
		TimeStamp:           time.Now().UnixNano(),
	}); err != nil {
		t.Fatalf("create memory: %s", err)
	}

	if got := significanceOf(t, d, "m2"); got != 5 {
		t.Errorf("significance = %d, want 5 - the write stored a level id that no longer existed", got)
	}
}

// TestReapSignificanceLevels_RetentionZeroOnlyCounts covers the opt-out: nothing is marked, nothing
// is removed, and the registry's size is still reported - which is the figure that makes an
// unbounded registry visible, and so the one a deployment that has turned the reap off most needs.
func TestReapSignificanceLevels_RetentionZeroOnlyCounts(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()

	for i, id := range []string{"m1", "m2", "m3"} {
		createMemoryWithSignificance(t, d, id, int32(5+i))
	}

	if _, err := d.DeleteMemories(ctx, []string{"m1", "m2", "m3"}); err != nil {
		t.Fatalf("delete memories: %s", err)
	}

	registry, err := d.ReapSignificanceLevels(ctx, 0)
	if err != nil {
		t.Fatalf("reap: %s", err)
	}

	if registry.Levels != 3 {
		t.Errorf("Levels = %d, want 3", registry.Levels)
	}

	if registry.Reaped != 0 {
		t.Errorf("Reaped = %d, want 0 - a retention of 0 keeps every value the store has seen", registry.Reaped)
	}

	if markOf(t, d, 5).Valid {
		t.Error("a disabled reap marked a level, which would let a later enabled pass skip its window")
	}
}

// TestReapSignificanceLevels_ReportsWhatIsLeft pins the reported size against the reap that produced
// it: a gauge published from a figure taken before the delete would keep reporting a registry that
// is no longer there.
func TestReapSignificanceLevels_ReportsWhatIsLeft(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()

	createMemoryWithSignificance(t, d, "kept", 5)
	createMemoryWithSignificance(t, d, "gone", 6)

	if _, err := d.DeleteMemories(ctx, []string{"gone"}); err != nil {
		t.Fatalf("delete memory: %s", err)
	}

	if _, err := d.ReapSignificanceLevels(ctx, time.Hour); err != nil {
		t.Fatalf("marking reap: %s", err)
	}

	ageMark(t, d, 6, 2*time.Hour)

	registry, err := d.ReapSignificanceLevels(ctx, time.Hour)
	if err != nil {
		t.Fatalf("reap: %s", err)
	}

	if registry.Reaped != 1 || registry.Levels != 1 {
		t.Errorf("registry = %+v, want 1 reaped and 1 left", registry)
	}
}

// TestReapSignificanceLevels_UnmarksALevelCarriedAgain covers the self-correcting pass. A hand-out
// clears its own mark, so a marked level that is nonetheless carried by something can only arise
// from a write landing between the reap's two reads - and it must have its clock restarted rather
// than spend the window it is not entitled to.
func TestReapSignificanceLevels_UnmarksALevelCarriedAgain(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()

	createMemoryWithSignificance(t, d, "m1", 5)

	// The state that race leaves behind, written directly: the level is carried and marked.
	if _, err := d.sql.Exec(
		d.rebind(
			`UPDATE significance_levels SET `+significanceLevelUnusedColumn+` = ? WHERE level_rank = ?`,
		),
		time.Now().Add(-8*24*time.Hour).UnixNano(),
		int32(5),
	); err != nil {
		t.Fatalf("marking the level: %s", err)
	}

	registry, err := d.ReapSignificanceLevels(ctx, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("reap: %s", err)
	}

	if registry.Reaped != 0 || !rankExists(t, d, 5) {
		t.Fatalf("a level still carried by a memory was reaped (reaped %d)", registry.Reaped)
	}

	if markOf(t, d, 5).Valid {
		t.Error("the level is carried again but kept its mark, so it would be reaped the instant it next fell idle")
	}
}
