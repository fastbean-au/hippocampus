package db

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/fastbean-au/hippocampus/types"
)

// recordingDB is a test store with the forgotten log switched on.
func recordingDB(t *testing.T, policy TombstonePolicy) *DB {
	t.Helper()

	d := newTestDB(t)
	d.SetTombstonePolicy(policy)

	return d
}

// seedForgettableMemory creates one memory (optionally under an event) with everything a tombstone
// needs to carry, so the assertions can check each field came from the row rather than from a
// default.
func seedForgettableMemory(t *testing.T, d *DB, id string, group string, eventId string) {
	t.Helper()

	if eventId != "" {
		if _, err := d.CreateEvent(context.Background(), types.Event{
			Id:           eventId,
			Name:         "event",
			TimeStart:    100,
			Significance: 3,
			Group:        group,
		}); err != nil {
			t.Fatalf("CreateEvent(%s): %s", eventId, err)
		}
	}

	if _, err := d.CreateMemory(context.Background(), types.Memory{
		Id:           id,
		TimeStamp:    100,
		Significance: 7,
		Body:         "a body of exactly some length",
		EventId:      eventId,
		Group:        group,
	}); err != nil {
		t.Fatalf("CreateMemory(%s): %s", id, err)
	}
}

// forgetAll is a Server that consolidates everything it is shown, valuing each memory at a fixed
// number so the tombstone's value can be asserted against a known quantity.
type forgetAll struct{}

func (forgetAll) ShouldConsolidateMemory(MemoryConsolidationCandidate) bool { return true }

func (forgetAll) ShouldConsolidateEvent(EventConsolidationCandidate) bool { return true }

func (forgetAll) MemoryValue(MemoryConsolidationCandidate) float64 { return 0.5 }

func (forgetAll) MemoryRetained(MemoryConsolidationCandidate) bool { return false }

func (forgetAll) DeletionThreshold() float64 { return 10 }

// TestConsolidationWritesTombstones is the central case: a consolidated memory leaves a record
// carrying what it was, what decided it, and when - and the fields come from the memory row, not
// from the pass that deleted it.
func TestConsolidationWritesTombstones(t *testing.T) {
	d := recordingDB(t, TombstonePolicy{Enabled: true})

	seedForgettableMemory(t, d, "m1", "notes", "")

	before := time.Now().UnixNano()

	if _, err := d.ConsolidateMemories(context.Background(), forgetAll{}); err != nil {
		t.Fatalf("ConsolidateMemories: %s", err)
	}

	forgotten, err := d.GetForgottenMemories(context.Background(), ForgottenFilter{})
	if err != nil {
		t.Fatalf("GetForgottenMemories: %s", err)
	}

	if len(forgotten) != 1 {
		t.Fatalf("expected 1 tombstone, got %d", len(forgotten))
	}

	record := forgotten[0]

	if record.Id != "m1" {
		t.Errorf("tombstone id = %q, want m1", record.Id)
	}

	// Group and significance are read from the memory row inside the delete transaction; the
	// consolidation scan itself never reads either, which is the whole reason the capture exists.
	if record.Group != "notes" {
		t.Errorf("tombstone group = %q, want notes (it must come from the row, not the pass)", record.Group)
	}

	if record.Significance != 7 {
		t.Errorf("tombstone significance = %d, want 7", record.Significance)
	}

	if record.Value != 0.5 {
		t.Errorf("tombstone value = %v, want 0.5 (the value the pass computed)", record.Value)
	}

	if record.Threshold != 10 {
		t.Errorf("tombstone threshold = %v, want 10 (the threshold then in force)", record.Threshold)
	}

	if record.Rule != ForgetRuleConsolidation {
		t.Errorf("tombstone rule = %v, want consolidation", record.Rule)
	}

	if record.TimeStamp != 100 {
		t.Errorf("tombstone timestamp = %d, want 100", record.TimeStamp)
	}

	if record.Bytes <= 0 {
		t.Errorf("tombstone body_bytes = %d, want the stored size of the body", record.Bytes)
	}

	if record.ForgottenAt < before {
		t.Errorf("tombstone forgotten_at = %d, want at or after %d", record.ForgottenAt, before)
	}
}

// TestEvictionWritesTombstonesUnderItsOwnRule pins the distinction the log exists to record: a
// memory that went to meet the capacity target is not one that decayed, and tuning the two is
// answered by different settings.
func TestEvictionWritesTombstonesUnderItsOwnRule(t *testing.T) {
	d := recordingDB(t, TombstonePolicy{Enabled: true})

	seedForgettableMemory(t, d, "m1", "", "")

	if _, _, _, err := d.EvictMemories(context.Background(), forgetAll{}, 1<<20); err != nil {
		t.Fatalf("EvictMemories: %s", err)
	}

	forgotten, err := d.GetForgottenMemories(context.Background(), ForgottenFilter{})
	if err != nil {
		t.Fatalf("GetForgottenMemories: %s", err)
	}

	if len(forgotten) != 1 || forgotten[0].Rule != ForgetRuleEviction {
		t.Fatalf("expected one eviction tombstone, got %+v", forgotten)
	}
}

// TestClearWritesNoTombstones is the distinction the chokepoint cannot infer for itself: Clear
// deletes memories that were exported or transferred first, so nothing was forgotten and the log
// must stay silent about it.
func TestClearWritesNoTombstones(t *testing.T) {
	d := recordingDB(t, TombstonePolicy{Enabled: true})

	seedForgettableMemory(t, d, "m1", "", "")

	cleared, err := d.ClearMemories(context.Background(), []MemoryRecallSnapshot{{Id: "m1"}})
	if err != nil {
		t.Fatalf("ClearMemories: %s", err)
	}

	if cleared != 1 {
		t.Fatalf("ClearMemories cleared %d memories, want 1", cleared)
	}

	forgotten, err := d.GetForgottenMemories(context.Background(), ForgottenFilter{})
	if err != nil {
		t.Fatalf("GetForgottenMemories: %s", err)
	}

	if len(forgotten) != 0 {
		t.Errorf("Clear wrote %d tombstone(s); moving data is not forgetting it", len(forgotten))
	}
}

// TestDisabledPolicyRecordsNothing pins that the feature is genuinely opt-in.
func TestDisabledPolicyRecordsNothing(t *testing.T) {
	d := newTestDB(t)

	seedForgettableMemory(t, d, "m1", "", "")

	if _, err := d.ConsolidateMemories(context.Background(), forgetAll{}); err != nil {
		t.Fatalf("ConsolidateMemories: %s", err)
	}

	forgotten, err := d.GetForgottenMemories(context.Background(), ForgottenFilter{})
	if err != nil {
		t.Fatalf("GetForgottenMemories: %s", err)
	}

	if len(forgotten) != 0 {
		t.Errorf("the forgotten log recorded %d memories while disabled", len(forgotten))
	}
}

// TestTombstoneRecallRaceLeavesNoRecord is the correctness case the capture-then-filter shape
// exists for: a memory recalled between the scan and the delete survives, and a log claiming it was
// forgotten would be worse than no log at all.
func TestTombstoneRecallRaceLeavesNoRecord(t *testing.T) {
	d := recordingDB(t, TombstonePolicy{Enabled: true})

	seedForgettableMemory(t, d, "spared", "", "")
	seedForgettableMemory(t, d, "taken", "", "")

	// The snapshot for "spared" is stale: it was recalled since, so the guard leaves it in place.
	if _, err := d.RecallMemories(context.Background(), []string{"spared"}); err != nil {
		t.Fatalf("RecallMemories: %s", err)
	}

	deleted, err := d.deleteMemoriesIfUnrecalled(
		context.Background(),
		[]memoryRecallSnapshot{
			{id: "spared", timeRecalled: 0, recallCount: 0, value: 1},
			{id: "taken", timeRecalled: 0, recallCount: 0, value: 2},
		},
		forgetReason{rule: ForgetRuleConsolidation, threshold: 10},
	)
	if err != nil {
		t.Fatalf("deleteMemoriesIfUnrecalled: %s", err)
	}

	if len(deleted) != 1 || deleted[0] != "taken" {
		t.Fatalf("expected only 'taken' deleted, got %v", deleted)
	}

	forgotten, err := d.GetForgottenMemories(context.Background(), ForgottenFilter{})
	if err != nil {
		t.Fatalf("GetForgottenMemories: %s", err)
	}

	if len(forgotten) != 1 || forgotten[0].Id != "taken" {
		t.Fatalf("the log must name exactly the memories that went, got %+v", forgotten)
	}
}

// TestForgottenFilters covers each predicate, including that an out-of-scope group returns nothing
// rather than everything.
func TestForgottenFilters(t *testing.T) {
	d := recordingDB(t, TombstonePolicy{Enabled: true})

	seedForgettableMemory(t, d, "m-a", "a", "e-a")
	seedForgettableMemory(t, d, "m-b", "b", "")

	if _, err := d.ConsolidateMemories(context.Background(), forgetAll{}); err != nil {
		t.Fatalf("ConsolidateMemories: %s", err)
	}

	if _, _, _, err := d.ConsolidateEventMemories(context.Background(), forgetAll{}); err != nil {
		t.Fatalf("ConsolidateEventMemories: %s", err)
	}

	cases := []struct {
		name   string
		filter ForgottenFilter
		want   []string
	}{
		{"everything", ForgottenFilter{}, []string{"m-a", "m-b"}},
		{"by memory id", ForgottenFilter{MemoryId: "m-a"}, []string{"m-a"}},
		{"by event id", ForgottenFilter{EventId: "e-a"}, []string{"m-a"}},
		{"by group", ForgottenFilter{Group: "b"}, []string{"m-b"}},
		{"by rule", ForgottenFilter{Rule: ForgetRuleConsolidation}, []string{"m-a", "m-b"}},
		{"by another rule", ForgottenFilter{Rule: ForgetRuleEviction}, nil},
		{"within the scope", ForgottenFilter{Groups: []string{"a"}}, []string{"m-a"}},
		{"outside the scope", ForgottenFilter{Group: "a", Groups: []string{"b"}}, nil},
		{"since the future", ForgottenFilter{Since: time.Now().Add(time.Hour).UnixNano()}, nil},
		{"until the past", ForgottenFilter{Until: 1}, nil},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			forgotten, err := d.GetForgottenMemories(context.Background(), c.filter)
			if err != nil {
				t.Fatalf("GetForgottenMemories: %s", err)
			}

			got := make(map[string]bool, len(forgotten))
			for _, record := range forgotten {
				got[record.Id] = true
			}

			if len(got) != len(c.want) {
				t.Fatalf("got %d records %v, want %v", len(got), got, c.want)
			}

			for _, id := range c.want {
				if !got[id] {
					t.Errorf("expected %s in the result, got %v", id, got)
				}
			}
		})
	}
}

// TestForgottenPagination pins the keyset walk: newest first, each page picking up below the last
// seq, and no record seen twice.
func TestForgottenPagination(t *testing.T) {
	d := recordingDB(t, TombstonePolicy{Enabled: true})

	for i := range 5 {
		seedForgettableMemory(t, d, fmt.Sprintf("m%d", i), "", "")

		// One pass per memory so each gets its own seq, mirroring separate cycles.
		if _, err := d.ConsolidateMemories(context.Background(), forgetAll{}); err != nil {
			t.Fatalf("ConsolidateMemories: %s", err)
		}
	}

	seen := make(map[string]bool)
	after := int64(0)

	for range 5 {
		page, err := d.GetForgottenMemories(context.Background(), ForgottenFilter{Limit: 2, AfterSeq: after})
		if err != nil {
			t.Fatalf("GetForgottenMemories: %s", err)
		}

		if len(page) == 0 {
			break
		}

		for _, record := range page {
			if seen[record.Id] {
				t.Errorf("record %s returned twice by the keyset walk", record.Id)
			}

			seen[record.Id] = true
		}

		after = page[len(page)-1].Seq
	}

	if len(seen) != 5 {
		t.Errorf("the walk returned %d of 5 records", len(seen))
	}
}

// TestPruneTombstonesRowCap and its age counterpart pin the bounds that stop the log eating the
// store. The row cap is exact - which is why the log carries a surrogate key rather than pruning on
// forgotten_at, which a whole batch shares.
func TestPruneTombstonesRowCap(t *testing.T) {
	d := recordingDB(t, TombstonePolicy{Enabled: true, MaxRows: 2})

	for i := range 5 {
		seedForgettableMemory(t, d, fmt.Sprintf("m%d", i), "", "")
	}

	if _, err := d.ConsolidateMemories(context.Background(), forgetAll{}); err != nil {
		t.Fatalf("ConsolidateMemories: %s", err)
	}

	pruned, err := d.PruneTombstones(context.Background())
	if err != nil {
		t.Fatalf("PruneTombstones: %s", err)
	}

	if pruned != 3 {
		t.Errorf("pruned %d records, want 3 (5 recorded, cap 2)", pruned)
	}

	count, err := d.CountForgottenMemories(context.Background(), nil)
	if err != nil {
		t.Fatalf("CountForgottenMemories: %s", err)
	}

	if count != 2 {
		t.Errorf("the log holds %d records, want the cap of 2", count)
	}
}

func TestPruneTombstonesAgeCap(t *testing.T) {
	d := recordingDB(t, TombstonePolicy{Enabled: true, MaxAgeInDays: 1})

	seedForgettableMemory(t, d, "m1", "", "")

	if _, err := d.ConsolidateMemories(context.Background(), forgetAll{}); err != nil {
		t.Fatalf("ConsolidateMemories: %s", err)
	}

	// Nothing is old enough yet.
	if pruned, err := d.PruneTombstones(context.Background()); err != nil || pruned != 0 {
		t.Fatalf("PruneTombstones on a fresh log = (%d, %v), want (0, nil)", pruned, err)
	}

	// Age the record past the bound.
	if _, err := d.exec(
		context.Background(),
		`UPDATE `+tombstonesTable+` SET forgotten_at = ?`,
		time.Now().Add(-48*time.Hour).UnixNano(),
	); err != nil {
		t.Fatalf("ageing the tombstone: %s", err)
	}

	pruned, err := d.PruneTombstones(context.Background())
	if err != nil {
		t.Fatalf("PruneTombstones: %s", err)
	}

	if pruned != 1 {
		t.Errorf("pruned %d records, want 1", pruned)
	}
}

// TestPruneTombstonesLeavesADisabledLogAlone is the requirement that a configuration change never
// destroys a record: turning the feature off stops the writing AND the trimming, leaving what was
// recorded for somebody to read and then remove deliberately.
func TestPruneTombstonesLeavesADisabledLogAlone(t *testing.T) {
	d := recordingDB(t, TombstonePolicy{Enabled: true, MaxRows: 1})

	for i := range 3 {
		seedForgettableMemory(t, d, fmt.Sprintf("m%d", i), "", "")
	}

	if _, err := d.ConsolidateMemories(context.Background(), forgetAll{}); err != nil {
		t.Fatalf("ConsolidateMemories: %s", err)
	}

	d.SetTombstonePolicy(TombstonePolicy{Enabled: false, MaxRows: 1})

	pruned, err := d.PruneTombstones(context.Background())
	if err != nil {
		t.Fatalf("PruneTombstones: %s", err)
	}

	if pruned != 0 {
		t.Errorf("pruning a disabled log removed %d records; disabling must not delete", pruned)
	}

	count, err := d.CountForgottenMemories(context.Background(), nil)
	if err != nil {
		t.Fatalf("CountForgottenMemories: %s", err)
	}

	if count != 3 {
		t.Errorf("the disabled log holds %d records, want all 3 still readable", count)
	}
}

// TestDeleteForgottenMemories covers the manual cleanup: a cutoff, the whole log, and a scoped
// clear leaving another group's records in place.
func TestDeleteForgottenMemories(t *testing.T) {
	d := recordingDB(t, TombstonePolicy{Enabled: true})

	seedForgettableMemory(t, d, "old", "a", "")

	if _, err := d.ConsolidateMemories(context.Background(), forgetAll{}); err != nil {
		t.Fatalf("ConsolidateMemories: %s", err)
	}

	cutoff := time.Now().UnixNano()

	seedForgettableMemory(t, d, "new", "b", "")

	if _, err := d.ConsolidateMemories(context.Background(), forgetAll{}); err != nil {
		t.Fatalf("ConsolidateMemories: %s", err)
	}

	deleted, err := d.DeleteForgottenMemories(context.Background(), cutoff, nil)
	if err != nil {
		t.Fatalf("DeleteForgottenMemories: %s", err)
	}

	if deleted != 1 {
		t.Fatalf("deleted %d records before the cutoff, want 1", deleted)
	}

	// A scoped clear must not reach another group's records.
	if deleted, err := d.DeleteForgottenMemories(context.Background(), 0, []string{"a"}); err != nil || deleted != 0 {
		t.Fatalf("a clear scoped to group a = (%d, %v), want (0, nil): only group b has a record left", deleted, err)
	}

	if deleted, err := d.DeleteForgottenMemories(context.Background(), 0, nil); err != nil || deleted != 1 {
		t.Fatalf("an unscoped clear = (%d, %v), want (1, nil)", deleted, err)
	}
}

// TestPurgeEmptiesTheForgottenLog: Purge is the explicit "leave nothing behind", and a log of what
// a now-empty store used to hold is not a record anybody asked to keep.
func TestPurgeEmptiesTheForgottenLog(t *testing.T) {
	d := recordingDB(t, TombstonePolicy{Enabled: true})

	seedForgettableMemory(t, d, "m1", "", "")

	if _, err := d.ConsolidateMemories(context.Background(), forgetAll{}); err != nil {
		t.Fatalf("ConsolidateMemories: %s", err)
	}

	if err := d.Purge(context.Background()); err != nil {
		t.Fatalf("Purge: %s", err)
	}

	count, err := d.CountForgottenMemories(context.Background(), nil)
	if err != nil {
		t.Fatalf("CountForgottenMemories: %s", err)
	}

	if count != 0 {
		t.Errorf("Purge left %d tombstones", count)
	}
}

// TestUsedBytesExcludesTheForgottenLog is the "feature must not eat itself" guarantee: SQLite's
// UsedBytes is page accounting over the whole file, so without the exclusion the record of what was
// evicted would raise capacity pressure and evict live memories to make room for itself.
func TestUsedBytesExcludesTheForgottenLog(t *testing.T) {
	d := recordingDB(t, TombstonePolicy{Enabled: true})

	for i := range 200 {
		seedForgettableMemory(t, d, fmt.Sprintf("m%d", i), "", "")
	}

	if _, err := d.ConsolidateMemories(context.Background(), forgetAll{}); err != nil {
		t.Fatalf("ConsolidateMemories: %s", err)
	}

	count, err := d.CountForgottenMemories(context.Background(), nil)
	if err != nil {
		t.Fatalf("CountForgottenMemories: %s", err)
	}

	if count != 200 {
		t.Fatalf("expected 200 tombstones to measure against, got %d", count)
	}

	withLog, err := d.UsedBytes(context.Background())
	if err != nil {
		t.Fatalf("UsedBytes: %s", err)
	}

	// The same store with the log emptied: the reading must not have counted it.
	if _, err := d.DeleteForgottenMemories(context.Background(), 0, nil); err != nil {
		t.Fatalf("DeleteForgottenMemories: %s", err)
	}

	withoutLog, err := d.UsedBytes(context.Background())
	if err != nil {
		t.Fatalf("UsedBytes: %s", err)
	}

	// Page accounting is coarse, so the two need not be equal - but the log's allowance must have
	// been subtracted, which is what stops the figure growing with it.
	if withLog > withoutLog {
		t.Errorf(
			"UsedBytes with a 200-record log (%d) exceeds the same store with none (%d): the log is being counted toward the capacity target",
			withLog,
			withoutLog,
		)
	}
}

// TestForgottenLimit pins the page bounds the RPC layer shares.
func TestForgottenLimit(t *testing.T) {
	cases := map[int]int{0: forgottenDefaultLimit, -5: forgottenDefaultLimit, 10: 10, 99999: forgottenMaxLimit}

	for requested, want := range cases {
		if got := ForgottenLimit(requested); got != want {
			t.Errorf("ForgottenLimit(%d) = %d, want %d", requested, got, want)
		}
	}
}

// --- error paths, in the package's sqlmock style ---

// TestRecordTombstonesQueryError: a capture that fails must fail the delete rather than let the
// memories go unrecorded, which is the whole point of having asked for a record.
func TestRecordTombstonesQueryError(t *testing.T) {
	d, mock := newMockDB(t, driverPostgres)
	d.SetTombstonePolicy(TombstonePolicy{Enabled: true})
	d.tombstoneTable = true

	mock.ExpectBegin()
	mock.ExpectQuery(`FROM memories m LEFT JOIN significance_levels`).WillReturnError(errors.New("boom"))
	mock.ExpectRollback()

	_, err := d.deleteMemoriesIfUnrecalled(
		context.Background(),
		[]memoryRecallSnapshot{{id: "m1"}},
		forgetReason{rule: ForgetRuleConsolidation},
	)
	if err == nil {
		t.Fatal("expected the delete to fail when its tombstones could not be captured")
	}

	expectationsMet(t, mock)
}

func TestWriteTombstonesInsertError(t *testing.T) {
	d, mock := newMockDB(t, driverPostgres)
	d.SetTombstonePolicy(TombstonePolicy{Enabled: true})
	d.tombstoneTable = true

	mock.ExpectBegin()
	mock.ExpectQuery(`FROM memories m LEFT JOIN significance_levels`).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "event_id", "group_name", "level_rank", "bytes", "timestamp", "time_recalled", "recall_count",
		}).AddRow("m1", "", "", int32(1), int64(10), int64(1), int64(0), int32(0)))
	mock.ExpectQuery(`DELETE FROM memories`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("m1"))
	mock.ExpectExec(`INSERT INTO memory_tombstones`).WillReturnError(errors.New("boom"))
	mock.ExpectRollback()

	_, err := d.deleteMemoriesIfUnrecalled(
		context.Background(),
		[]memoryRecallSnapshot{{id: "m1"}},
		forgetReason{rule: ForgetRuleConsolidation},
	)
	if err == nil {
		t.Fatal("expected the delete to fail when its tombstones could not be written")
	}

	expectationsMet(t, mock)
}

// TestPruneTombstonesCutoffError covers the row-cap cutoff read failing, which must surface rather
// than be read as "nothing to prune".
func TestPruneTombstonesCutoffError(t *testing.T) {
	d, mock := newMockDB(t, driverPostgres)
	d.SetTombstonePolicy(TombstonePolicy{Enabled: true, MaxRows: 10})
	d.tombstoneTable = true

	mock.ExpectQuery(`SELECT seq FROM memory_tombstones`).WillReturnError(errors.New("boom"))

	if _, err := d.PruneTombstones(context.Background()); err == nil {
		t.Fatal("expected an error from the cutoff read")
	}

	expectationsMet(t, mock)
}

// TestInitTombstonesErrors covers the two DDL failures, per driver arm: MySQL probes
// information_schema for each index while the others use CREATE INDEX IF NOT EXISTS.
func TestInitTombstonesErrors(t *testing.T) {
	t.Run("create table", func(t *testing.T) {
		d, mock := newMockDB(t, driverPostgres)

		mock.ExpectExec(`CREATE TABLE IF NOT EXISTS memory_tombstones`).WillReturnError(errors.New("boom"))

		if err := d.initTombstones(); err == nil {
			t.Fatal("expected an error")
		}

		expectationsMet(t, mock)
	})

	t.Run("create index", func(t *testing.T) {
		d, mock := newMockDB(t, driverPostgres)

		mock.ExpectExec(`CREATE TABLE IF NOT EXISTS memory_tombstones`).WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectExec(`CREATE INDEX IF NOT EXISTS idx_memory_tombstones`).WillReturnError(errors.New("boom"))

		if err := d.initTombstones(); err == nil {
			t.Fatal("expected an error")
		}

		expectationsMet(t, mock)
	})

	t.Run("mysql index probe", func(t *testing.T) {
		d, mock := newMockDB(t, driverMySQL)

		mock.ExpectExec(`CREATE TABLE IF NOT EXISTS memory_tombstones`).WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectQuery(`information_schema.statistics`).WillReturnError(errors.New("boom"))

		if err := d.initTombstones(); err == nil {
			t.Fatal("expected an error")
		}

		expectationsMet(t, mock)
	})
}

// TestRecordTombstonesScanError covers a row that cannot be scanned - a column type the driver
// disagrees about - which must fail the delete like any other capture failure.
func TestRecordTombstonesScanError(t *testing.T) {
	d, mock := newMockDB(t, driverPostgres)
	d.SetTombstonePolicy(TombstonePolicy{Enabled: true})
	d.tombstoneTable = true

	mock.ExpectBegin()
	mock.ExpectQuery(`FROM memories m LEFT JOIN significance_levels`).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "event_id", "group_name", "level_rank", "bytes", "timestamp", "time_recalled", "recall_count",
		}).AddRow("m1", "", "", "not a number", int64(10), int64(1), int64(0), int32(0)))
	mock.ExpectRollback()

	_, err := d.deleteMemoriesIfUnrecalled(
		context.Background(),
		[]memoryRecallSnapshot{{id: "m1"}},
		forgetReason{rule: ForgetRuleConsolidation},
	)
	if err == nil {
		t.Fatal("expected the delete to fail on an unscannable capture row")
	}

	expectationsMet(t, mock)
}

// TestForgottenReadErrors covers the read paths' failures, none of which may be reported as an
// empty log - "nothing was forgotten" is a claim, not a fallback.
func TestForgottenReadErrors(t *testing.T) {
	t.Run("query", func(t *testing.T) {
		d, mock := newMockDB(t, driverPostgres)

		mock.ExpectQuery(`FROM memory_tombstones`).WillReturnError(errors.New("boom"))

		if _, err := d.GetForgottenMemories(context.Background(), ForgottenFilter{}); err == nil {
			t.Fatal("expected an error")
		}

		expectationsMet(t, mock)
	})

	t.Run("scan", func(t *testing.T) {
		d, mock := newMockDB(t, driverPostgres)

		mock.ExpectQuery(`FROM memory_tombstones`).
			WillReturnRows(sqlmock.NewRows([]string{"seq"}).AddRow(int64(1)))

		if _, err := d.GetForgottenMemories(context.Background(), ForgottenFilter{}); err == nil {
			t.Fatal("expected an error")
		}

		expectationsMet(t, mock)
	})

	t.Run("count", func(t *testing.T) {
		d, mock := newMockDB(t, driverPostgres)

		mock.ExpectQuery(`COUNT\(\*\) FROM memory_tombstones`).WillReturnError(errors.New("boom"))

		if _, err := d.CountForgottenMemories(context.Background(), nil); err == nil {
			t.Fatal("expected an error")
		}

		expectationsMet(t, mock)
	})

	t.Run("delete", func(t *testing.T) {
		d, mock := newMockDB(t, driverPostgres)

		mock.ExpectExec(`DELETE FROM memory_tombstones`).WillReturnError(errors.New("boom"))

		if _, err := d.DeleteForgottenMemories(context.Background(), 0, nil); err == nil {
			t.Fatal("expected an error")
		}

		expectationsMet(t, mock)
	})

	t.Run("delete rows affected", func(t *testing.T) {
		d, mock := newMockDB(t, driverPostgres)

		mock.ExpectExec(`DELETE FROM memory_tombstones`).
			WillReturnResult(sqlmock.NewErrorResult(errors.New("boom")))

		if _, err := d.DeleteForgottenMemories(context.Background(), 0, nil); err == nil {
			t.Fatal("expected an error")
		}

		expectationsMet(t, mock)
	})
}

// TestPruneTombstonesErrors covers the prune's remaining failures: the age cap's delete, the row
// cap's delete, and its RowsAffected.
func TestPruneTombstonesErrors(t *testing.T) {
	t.Run("age cap delete", func(t *testing.T) {
		d, mock := newMockDB(t, driverPostgres)
		d.SetTombstonePolicy(TombstonePolicy{Enabled: true, MaxAgeInDays: 1})
		d.tombstoneTable = true

		mock.ExpectExec(`DELETE FROM memory_tombstones`).WillReturnError(errors.New("boom"))

		if _, err := d.PruneTombstones(context.Background()); err == nil {
			t.Fatal("expected an error")
		}

		expectationsMet(t, mock)
	})

	t.Run("row cap delete", func(t *testing.T) {
		d, mock := newMockDB(t, driverPostgres)
		d.SetTombstonePolicy(TombstonePolicy{Enabled: true, MaxRows: 2})
		d.tombstoneTable = true

		mock.ExpectQuery(`SELECT seq FROM memory_tombstones`).
			WillReturnRows(sqlmock.NewRows([]string{"seq"}).AddRow(int64(5)))
		mock.ExpectExec(`DELETE FROM memory_tombstones WHERE seq`).WillReturnError(errors.New("boom"))

		if _, err := d.PruneTombstones(context.Background()); err == nil {
			t.Fatal("expected an error")
		}

		expectationsMet(t, mock)
	})

	t.Run("row cap rows affected", func(t *testing.T) {
		d, mock := newMockDB(t, driverPostgres)
		d.SetTombstonePolicy(TombstonePolicy{Enabled: true, MaxRows: 2})
		d.tombstoneTable = true

		mock.ExpectQuery(`SELECT seq FROM memory_tombstones`).
			WillReturnRows(sqlmock.NewRows([]string{"seq"}).AddRow(int64(5)))
		mock.ExpectExec(`DELETE FROM memory_tombstones WHERE seq`).
			WillReturnResult(sqlmock.NewErrorResult(errors.New("boom")))

		if _, err := d.PruneTombstones(context.Background()); err == nil {
			t.Fatal("expected an error")
		}

		expectationsMet(t, mock)
	})
}

// TestTombstoneBytesUnavailable: a measurement that fails counts the log as nothing rather than
// failing UsedBytes, since a capacity reading is worth more than a perfect one.
func TestTombstoneBytesUnavailable(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)
	d.tombstoneTable = true

	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM memory_tombstones`).WillReturnError(errors.New("boom"))

	if got := d.tombstoneBytes(context.Background()); got != 0 {
		t.Errorf("tombstoneBytes on a failed measurement = %d, want 0", got)
	}

	expectationsMet(t, mock)
}
