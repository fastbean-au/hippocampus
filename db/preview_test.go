package db

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/fastbean-au/hippocampus/types"
)

// seedPreviewStore creates count memories against the given event id (empty for none), with ids
// prefixed so a test can name them.
func seedPreviewStore(t *testing.T, db *DB, prefix string, eventId string, group string, count int) {
	t.Helper()

	for i := range count {
		memory := types.Memory{
			Id:           fmt.Sprintf("%s%d", prefix, i),
			TimeStamp:    100,
			Significance: 1,
			EventId:      eventId,
			Group:        group,
			Body:         strings.Repeat("x", 64),
		}

		if _, err := db.CreateMemory(context.Background(), memory); err != nil {
			t.Fatalf("CreateMemory(%s): %s", memory.Id, err)
		}
	}
}

// TestPreviewMatchesASleepCycle is the guarantee the preview exists on: whatever it predicts, an
// actual cycle over the same store must then do. The preview reimplements the per-event
// bookkeeping rather than sharing it with the four real passes, so this test - not shared code -
// is what stops the two drifting apart.
//
// It runs the preview, then the real passes in the order sleep() runs them, and compares.
func TestPreviewMatchesASleepCycle(t *testing.T) {
	tests := []struct {
		name     string
		seed     func(*testing.T, *DB)
		server   *decisionServer
		capacity int64
	}{
		{
			// The plain case: some memories go, some stay, no events involved.
			name: "eventless memories, some consolidated",
			seed: func(t *testing.T, db *DB) {
				seedPreviewStore(t, db, "doomed", "", "logs", 4)
				seedPreviewStore(t, db, "safe", "", "logs", 3)
			},
			server: &decisionServer{
				memory: func(c MemoryConsolidationCandidate) bool { return c.MemorySignificance == 1 },
			},
		},
		{
			// An event losing every memory must be deleted with them - the bookkeeping the preview
			// mirrors rather than shares.
			name: "event emptied by consolidation is deleted",
			seed: func(t *testing.T, db *DB) {
				if _, err := db.CreateEvent(context.Background(), types.Event{Id: "e1", Name: "e1", TimeStart: 100, Significance: 1}); err != nil {
					t.Fatalf("CreateEvent: %s", err)
				}

				seedPreviewStore(t, db, "m", "e1", "", 3)
			},
			server: &decisionServer{
				memory: func(MemoryConsolidationCandidate) bool { return true },
			},
		},
		{
			// One survivor must keep the event alive: the count of deletions no longer reaches the
			// count of memories it holds.
			name: "event keeping one memory survives",
			seed: func(t *testing.T, db *DB) {
				if _, err := db.CreateEvent(context.Background(), types.Event{Id: "e1", Name: "e1", TimeStart: 100, Significance: 5}); err != nil {
					t.Fatalf("CreateEvent: %s", err)
				}

				seedPreviewStore(t, db, "doomed", "e1", "", 2)
				seedPreviewStore(t, db, "keep", "e1", "", 1)
			},
			server: &decisionServer{
				memory: func(c MemoryConsolidationCandidate) bool { return c.MemorySignificance < 5 },
			},
		},
		{
			// A memory pointing at an event that no longer exists is scored as event-less and must
			// not enter the event bookkeeping in either path.
			name: "dangling event reference",
			seed: func(t *testing.T, db *DB) {
				seedPreviewStore(t, db, "ghost", "gone", "", 3)
			},
			server: &decisionServer{
				memory: func(MemoryConsolidationCandidate) bool { return true },
			},
		},
		{
			// An event with no memories at all, decayed past the threshold: the third pass.
			name: "empty event consolidated",
			seed: func(t *testing.T, db *DB) {
				if _, err := db.CreateEvent(context.Background(), types.Event{Id: "empty", Name: "empty", TimeStart: 100, Significance: 1}); err != nil {
					t.Fatalf("CreateEvent: %s", err)
				}
			},
			server: &decisionServer{
				event: func(EventConsolidationCandidate) bool { return true },
			},
		},
		{
			// Nothing to do at all - the preview must report zeroes rather than anything spurious.
			name: "nothing forgotten",
			seed: func(t *testing.T, db *DB) {
				seedPreviewStore(t, db, "safe", "", "", 3)
			},
			server: &decisionServer{},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			db := newTestDB(t)
			test.seed(t, db)

			used, err := db.UsedBytes(context.Background())
			if err != nil {
				t.Fatalf("UsedBytes: %s", err)
			}

			preview, err := db.PreviewConsolidation(context.Background(), test.server, PreviewOptions{
				UsedBytes:     used,
				CapacityBytes: test.capacity,
				EvictionFloor: test.capacity,
			})
			if err != nil {
				t.Fatalf("PreviewConsolidation: %s", err)
			}

			// Now actually run the cycle, in sleep()'s order.
			eventless, err := db.ConsolidateMemories(context.Background(), test.server)
			if err != nil {
				t.Fatalf("ConsolidateMemories: %s", err)
			}

			withEvents, _, eventsFromMemories, err := db.ConsolidateEventMemories(context.Background(), test.server)
			if err != nil {
				t.Fatalf("ConsolidateEventMemories: %s", err)
			}

			emptyEvents, err := db.ConsolidateEvents(context.Background(), test.server)
			if err != nil {
				t.Fatalf("ConsolidateEvents: %s", err)
			}

			if got, want := preview.MemoriesConsolidated, eventless+withEvents; got != want {
				t.Errorf("preview said %d memories consolidated, the cycle consolidated %d", got, want)
			}

			if got, want := preview.EventsDeleted, eventsFromMemories+emptyEvents; got != want {
				t.Errorf("preview said %d events deleted, the cycle deleted %d", got, want)
			}
		})
	}
}

// TestPreviewEvictionMatchesACycle covers the capacity half of the same guarantee. Eviction is
// checked separately because it only runs when a byte capacity is both configured and exceeded,
// and because the real EvictMemories is driven by a byte budget the preview has to reproduce.
func TestPreviewEvictionMatchesACycle(t *testing.T) {
	db := newTestDB(t)

	// Distinct significances so the value ordering (and therefore which memories are selected) is
	// unambiguous rather than dependent on scan order.
	for i := range 10 {
		memory := types.Memory{
			Id:           fmt.Sprintf("m%d", i),
			TimeStamp:    100,
			Significance: int32(i + 1),
			Body:         strings.Repeat("x", 512),
		}

		if _, err := db.CreateMemory(context.Background(), memory); err != nil {
			t.Fatalf("CreateMemory: %s", err)
		}
	}

	// Value ascending by significance, so the lowest-significance memories evict first. No
	// consolidation, so the eviction half is isolated.
	server := &decisionServer{
		value: func(c MemoryConsolidationCandidate) float64 { return float64(c.MemorySignificance) },
	}

	used, err := db.UsedBytes(context.Background())
	if err != nil {
		t.Fatalf("UsedBytes: %s", err)
	}

	// A capacity comfortably below current usage, so eviction has real work to do.
	capacity := used / 2

	preview, err := db.PreviewConsolidation(context.Background(), server, PreviewOptions{
		UsedBytes:     used,
		CapacityBytes: capacity,
		EvictionFloor: capacity,
	})
	if err != nil {
		t.Fatalf("PreviewConsolidation: %s", err)
	}

	evicted, _, _, err := db.EvictMemories(context.Background(), server, used-capacity)
	if err != nil {
		t.Fatalf("EvictMemories: %s", err)
	}

	if preview.MemoriesEvicted != evicted {
		t.Errorf("preview said %d memories evicted, the cycle evicted %d", preview.MemoriesEvicted, evicted)
	}

	if preview.MemoriesConsolidated != 0 {
		t.Errorf("expected no consolidation, got %d", preview.MemoriesConsolidated)
	}
}

// TestPreviewExcludesConsolidatedFromEviction pins the ordering the real cycle imposes:
// consolidation runs first, so a memory it would delete must not also be counted as evicted, and
// the bytes it frees must count against what eviction still has to reclaim.
func TestPreviewExcludesConsolidatedFromEviction(t *testing.T) {
	db := newTestDB(t)

	seedPreviewStore(t, db, "m", "", "", 10)

	used, err := db.UsedBytes(context.Background())
	if err != nil {
		t.Fatalf("UsedBytes: %s", err)
	}

	// Everything consolidates, and the capacity target is far below usage - so a preview that
	// forgot to exclude the consolidated set would go on to "evict" the same memories again.
	server := &decisionServer{
		memory: func(MemoryConsolidationCandidate) bool { return true },
		value:  func(MemoryConsolidationCandidate) float64 { return 1 },
	}

	preview, err := db.PreviewConsolidation(context.Background(), server, PreviewOptions{
		UsedBytes:     used,
		CapacityBytes: 1,
		EvictionFloor: 1,
	})
	if err != nil {
		t.Fatalf("PreviewConsolidation: %s", err)
	}

	if preview.MemoriesConsolidated != 10 {
		t.Fatalf("expected 10 consolidated, got %d", preview.MemoriesConsolidated)
	}

	if preview.MemoriesEvicted != 0 {
		t.Errorf("consolidated memories were counted again as evictions: %d evicted", preview.MemoriesEvicted)
	}

	// And each memory appears once, under one rule.
	seen := make(map[string]bool)

	for _, candidate := range preview.Candidates {
		if seen[candidate.Id] {
			t.Errorf("memory %s reported twice", candidate.Id)
		}

		seen[candidate.Id] = true
	}
}

// TestPreviewCountsRetainedSeparately covers the retention floor: a retained memory is exempt from
// both paths, and is counted rather than listed.
func TestPreviewCountsRetainedSeparately(t *testing.T) {
	db := newTestDB(t)

	seedPreviewStore(t, db, "keep", "", "", 4)

	used, err := db.UsedBytes(context.Background())
	if err != nil {
		t.Fatalf("UsedBytes: %s", err)
	}

	// Everything is inside the retention window, and the store is far over its capacity: nothing
	// may be forgotten by either path regardless.
	server := &decisionServer{
		retained: func(MemoryConsolidationCandidate) bool { return true },
		value:    func(MemoryConsolidationCandidate) float64 { return 1 },
	}

	preview, err := db.PreviewConsolidation(context.Background(), server, PreviewOptions{
		UsedBytes:     used,
		CapacityBytes: 1,
		EvictionFloor: 1,
	})
	if err != nil {
		t.Fatalf("PreviewConsolidation: %s", err)
	}

	if preview.MemoriesRetained != 4 {
		t.Errorf("expected 4 retained, got %d", preview.MemoriesRetained)
	}

	if preview.RetainedBytes <= 0 {
		t.Errorf("expected retained bytes to be reported, got %d", preview.RetainedBytes)
	}

	if preview.MemoriesEvicted != 0 {
		t.Errorf("retained memories were evicted: %d", preview.MemoriesEvicted)
	}

	if len(preview.Candidates) != 0 {
		t.Errorf("retained memories were listed as candidates: %d", len(preview.Candidates))
	}
}

// TestPreviewSampleIsBoundedButCountsAreNot is the property that makes a preview safe to run
// against a large store: the sample truncates, the numbers do not.
func TestPreviewSampleIsBoundedButCountsAreNot(t *testing.T) {
	db := newTestDB(t)

	seedPreviewStore(t, db, "m", "", "", 25)

	server := &decisionServer{
		memory: func(MemoryConsolidationCandidate) bool { return true },
	}

	preview, err := db.PreviewConsolidation(context.Background(), server, PreviewOptions{Limit: 10})
	if err != nil {
		t.Fatalf("PreviewConsolidation: %s", err)
	}

	if preview.MemoriesConsolidated != 25 {
		t.Errorf("expected the count to be complete at 25, got %d", preview.MemoriesConsolidated)
	}

	if len(preview.Candidates) != 10 {
		t.Errorf("expected 10 sampled candidates, got %d", len(preview.Candidates))
	}

	if !preview.Truncated {
		t.Error("expected truncated to be set")
	}
}

// TestPreviewLimitBounds covers the limit's defaulting and clamping.
func TestPreviewLimitBounds(t *testing.T) {
	db := newTestDB(t)

	seedPreviewStore(t, db, "m", "", "", 3)

	server := &decisionServer{
		memory: func(MemoryConsolidationCandidate) bool { return true },
	}

	tests := []struct {
		name  string
		limit int
	}{
		{name: "zero selects the default", limit: 0},
		{name: "negative selects the default", limit: -5},
		{name: "above the cap is clamped", limit: previewMaxLimit * 10},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			preview, err := db.PreviewConsolidation(context.Background(), server, PreviewOptions{Limit: test.limit})
			if err != nil {
				t.Fatalf("PreviewConsolidation: %s", err)
			}

			// Three candidates is under every bound, so all three come back whichever bound applied.
			if len(preview.Candidates) != 3 {
				t.Errorf("expected 3 candidates, got %d", len(preview.Candidates))
			}

			if preview.Truncated {
				t.Error("expected truncated to be false")
			}
		})
	}
}

// TestPreviewOrdersByValueAscending pins the sample's order: least valuable first, so the memories
// furthest past the threshold are the ones a truncated sample shows.
func TestPreviewOrdersByValueAscending(t *testing.T) {
	db := newTestDB(t)

	for i := range 5 {
		memory := types.Memory{
			Id:           fmt.Sprintf("m%d", i),
			TimeStamp:    100,
			Significance: int32(i + 1),
			Body:         "x",
		}

		if _, err := db.CreateMemory(context.Background(), memory); err != nil {
			t.Fatalf("CreateMemory: %s", err)
		}
	}

	server := &decisionServer{
		memory: func(MemoryConsolidationCandidate) bool { return true },
		value:  func(c MemoryConsolidationCandidate) float64 { return float64(c.MemorySignificance) },
	}

	preview, err := db.PreviewConsolidation(context.Background(), server, PreviewOptions{})
	if err != nil {
		t.Fatalf("PreviewConsolidation: %s", err)
	}

	if len(preview.Candidates) != 5 {
		t.Fatalf("expected 5 candidates, got %d", len(preview.Candidates))
	}

	for i := 1; i < len(preview.Candidates); i++ {
		if preview.Candidates[i-1].Value > preview.Candidates[i].Value {
			t.Fatalf("candidates are not ordered by value ascending: %v", preview.Candidates)
		}
	}
}

// TestPreviewReportsDetail covers the per-candidate reporting: the fields an operator reads to
// understand why a memory is on the list, and the absence of the one that must never appear.
func TestPreviewReportsDetail(t *testing.T) {
	db := newTestDB(t)

	if _, err := db.CreateEvent(context.Background(), types.Event{Id: "e1", Name: "e1", TimeStart: 100, Significance: 3}); err != nil {
		t.Fatalf("CreateEvent: %s", err)
	}

	memory := types.Memory{
		Id:           "m1",
		TimeStamp:    100,
		Significance: 7,
		EventId:      "e1",
		Group:        "slack",
		Body:         strings.Repeat("x", 300),
	}

	if _, err := db.CreateMemory(context.Background(), memory); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	server := &decisionServer{
		memory: func(MemoryConsolidationCandidate) bool { return true },
		value:  func(MemoryConsolidationCandidate) float64 { return 0.5 },
	}

	preview, err := db.PreviewConsolidation(context.Background(), server, PreviewOptions{})
	if err != nil {
		t.Fatalf("PreviewConsolidation: %s", err)
	}

	if len(preview.Candidates) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(preview.Candidates))
	}

	got := preview.Candidates[0]

	if got.Id != "m1" {
		t.Errorf("id: got %s, want m1", got.Id)
	}

	if got.EventId != "e1" {
		t.Errorf("event id: got %s, want e1", got.EventId)
	}

	if got.Group != "slack" {
		t.Errorf("group: got %s, want slack", got.Group)
	}

	if got.Significance != 7 {
		t.Errorf("significance: got %d, want 7", got.Significance)
	}

	if got.Rule != ForgetRuleConsolidation {
		t.Errorf("rule: got %d, want consolidation", got.Rule)
	}

	// Reported net of the row-overhead allowance, so it reads as the body's stored size. The body
	// is compressible, so the stored size is smaller than the 300 characters written - what
	// matters is that it is a real positive size and not the accounting figure.
	if got.Bytes <= 0 {
		t.Errorf("expected a positive body size, got %d", got.Bytes)
	}
}

// TestPreviewDeletesNothing is the whole point of a dry run.
func TestPreviewDeletesNothing(t *testing.T) {
	db := newTestDB(t)

	if _, err := db.CreateEvent(context.Background(), types.Event{Id: "e1", Name: "e1", TimeStart: 100, Significance: 1}); err != nil {
		t.Fatalf("CreateEvent: %s", err)
	}

	seedPreviewStore(t, db, "m", "e1", "", 5)
	seedPreviewStore(t, db, "n", "", "", 5)

	used, err := db.UsedBytes(context.Background())
	if err != nil {
		t.Fatalf("UsedBytes: %s", err)
	}

	// Everything consolidates, everything evicts, every event goes - the most destructive preview
	// available.
	server := &decisionServer{
		memory: func(MemoryConsolidationCandidate) bool { return true },
		event:  func(EventConsolidationCandidate) bool { return true },
		value:  func(MemoryConsolidationCandidate) float64 { return 1 },
	}

	if _, err := db.PreviewConsolidation(context.Background(), server, PreviewOptions{
		UsedBytes:     used,
		CapacityBytes: 1,
		EvictionFloor: 1,
	}); err != nil {
		t.Fatalf("PreviewConsolidation: %s", err)
	}

	memories, err := db.GetMemories(context.Background(), MemoryFilter{})
	if err != nil {
		t.Fatalf("GetMemories: %s", err)
	}

	if len(*memories) != 10 {
		t.Errorf("the preview deleted memories: %d of 10 remain", len(*memories))
	}

	events, err := db.GetEvents(context.Background(), EventFilter{})
	if err != nil {
		t.Fatalf("GetEvents: %s", err)
	}

	if len(*events) != 1 {
		t.Errorf("the preview deleted events: %d of 1 remain", len(*events))
	}
}

// TestPreviewWithoutCapacityNeverEvicts covers the disabled case: with no byte capacity
// configured, the eviction path does not run at all however full the store is.
func TestPreviewWithoutCapacityNeverEvicts(t *testing.T) {
	db := newTestDB(t)

	seedPreviewStore(t, db, "m", "", "", 5)

	server := &decisionServer{
		value: func(MemoryConsolidationCandidate) float64 { return 1 },
	}

	preview, err := db.PreviewConsolidation(context.Background(), server, PreviewOptions{
		UsedBytes:     1 << 30,
		CapacityBytes: 0,
	})
	if err != nil {
		t.Fatalf("PreviewConsolidation: %s", err)
	}

	if preview.MemoriesEvicted != 0 {
		t.Errorf("expected no evictions without a byte capacity, got %d", preview.MemoriesEvicted)
	}
}

// --- error paths, following the package's sqlmock gap-test convention ---

// previewColumns is the projection PreviewConsolidation scans, so a mocked row matches it.
func previewColumns() []string {
	return []string{
		"id", "timestamp", "significance_level_id", "time_recalled", "recall_count", "event_id",
		"group_name", "significance_level_id", "relationship_significance", "e_id", "body_bytes",
	}
}

func TestPreviewConsolidation_QueryError(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)
	emptyRanksQuery(mock)

	mock.ExpectQuery(`FROM memories m LEFT JOIN events e`).WillReturnError(errors.New("boom"))

	if _, err := d.PreviewConsolidation(context.Background(), &stubServer{}, PreviewOptions{}); err == nil {
		t.Fatal("expected an error")
	}

	expectationsMet(t, mock)
}

func TestPreviewConsolidation_RanksError(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	mock.ExpectQuery(`SELECT id, level_rank FROM significance_levels`).WillReturnError(errors.New("boom"))

	if _, err := d.PreviewConsolidation(context.Background(), &stubServer{}, PreviewOptions{}); err == nil {
		t.Fatal("expected an error")
	}

	expectationsMet(t, mock)
}

func TestPreviewConsolidation_ScanError(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)
	emptyRanksQuery(mock)

	mock.ExpectQuery(`FROM memories m LEFT JOIN events e`).
		WillReturnRows(sqlmock.NewRows(previewColumns()).
			AddRow("m1", "not-an-int", nil, int64(0), int32(0), "", "", nil, int64(0), nil, int64(0)))

	if _, err := d.PreviewConsolidation(context.Background(), &stubServer{}, PreviewOptions{}); err == nil {
		t.Fatal("expected a scan error")
	}

	expectationsMet(t, mock)
}

func TestPreviewConsolidation_RowsIterationError(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)
	emptyRanksQuery(mock)

	mock.ExpectQuery(`FROM memories m LEFT JOIN events e`).
		WillReturnRows(sqlmock.NewRows(previewColumns()).
			AddRow("m1", int64(1), nil, int64(0), int32(0), "", "", nil, int64(0), nil, int64(0)).
			RowError(0, errors.New("boom")))

	if _, err := d.PreviewConsolidation(context.Background(), &stubServer{}, PreviewOptions{}); err == nil {
		t.Fatal("expected an iteration error")
	}

	expectationsMet(t, mock)
}

// TestPreviewConsolidation_EmptyEventQueryError covers the third pass failing after the memory
// scan has already succeeded.
func TestPreviewConsolidation_EmptyEventQueryError(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)
	emptyRanksQuery(mock)

	mock.ExpectQuery(`FROM memories m LEFT JOIN events e`).
		WillReturnRows(sqlmock.NewRows(previewColumns()))
	mock.ExpectQuery(`FROM events e LEFT JOIN significance_levels`).WillReturnError(errors.New("boom"))

	if _, err := d.PreviewConsolidation(context.Background(), &stubServer{}, PreviewOptions{}); err == nil {
		t.Fatal("expected an error")
	}

	expectationsMet(t, mock)
}

func TestPreviewEmptyEventDeletions_ScanError(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)
	emptyRanksQuery(mock)

	mock.ExpectQuery(`FROM memories m LEFT JOIN events e`).
		WillReturnRows(sqlmock.NewRows(previewColumns()))
	mock.ExpectQuery(`FROM events e LEFT JOIN significance_levels`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "time_start", "time_end", "level_rank", "relationship_significance"}).
			AddRow("e1", "not-an-int", int64(0), int32(0), int64(0)))

	if _, err := d.PreviewConsolidation(context.Background(), &stubServer{}, PreviewOptions{}); err == nil {
		t.Fatal("expected a scan error")
	}

	expectationsMet(t, mock)
}

func TestPreviewEmptyEventDeletions_RowsIterationError(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)
	emptyRanksQuery(mock)

	mock.ExpectQuery(`FROM memories m LEFT JOIN events e`).
		WillReturnRows(sqlmock.NewRows(previewColumns()))
	mock.ExpectQuery(`FROM events e LEFT JOIN significance_levels`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "time_start", "time_end", "level_rank", "relationship_significance"}).
			AddRow("e1", int64(1), int64(0), int32(0), int64(0)).
			RowError(0, errors.New("boom")))

	if _, err := d.PreviewConsolidation(context.Background(), &stubServer{}, PreviewOptions{}); err == nil {
		t.Fatal("expected an iteration error")
	}

	expectationsMet(t, mock)
}

// TestPreviewEvictionFloorFallback covers the floor guard: a missing or nonsensical floor falls
// back to the capacity itself, exactly as Server.evictionFloor does.
func TestPreviewEvictionFloorFallback(t *testing.T) {
	tests := []struct {
		name  string
		floor int64
	}{
		{name: "unset falls back to the capacity", floor: 0},
		{name: "negative falls back to the capacity", floor: -1},
		{name: "above the capacity falls back to it", floor: 1 << 30},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			db := newTestDB(t)
			seedPreviewStore(t, db, "m", "", "", 6)

			used, err := db.UsedBytes(context.Background())
			if err != nil {
				t.Fatalf("UsedBytes: %s", err)
			}

			server := &decisionServer{
				value: func(c MemoryConsolidationCandidate) float64 { return float64(c.MemorySignificance) },
			}

			preview, err := db.PreviewConsolidation(context.Background(), server, PreviewOptions{
				UsedBytes:     used,
				CapacityBytes: used / 2,
				EvictionFloor: test.floor,
			})
			if err != nil {
				t.Fatalf("PreviewConsolidation: %s", err)
			}

			if preview.MemoriesEvicted == 0 {
				t.Error("expected eviction to reclaim down to the capacity when the floor is unusable")
			}
		})
	}
}

// TestRetainedStats covers the aggregate behind the retained gauges: which memories count as
// inside the window, and that the byte figure is on the same basis as the capacity target.
func TestRetainedStats(t *testing.T) {
	db := newTestDB(t)

	now := time.Now().UnixNano()
	day := int64(24 * time.Hour)

	// Two inside a 7-day window, two outside it.
	for _, seed := range []struct {
		id  string
		age int64
	}{
		{id: "fresh1", age: 1},
		{id: "fresh2", age: 3},
		{id: "old1", age: 30},
		{id: "old2", age: 90},
	} {
		memory := types.Memory{
			Id:           seed.id,
			TimeStamp:    now - seed.age*day,
			Significance: 1,
			Body:         strings.Repeat("x", 128),
		}

		if _, err := db.CreateMemory(context.Background(), memory); err != nil {
			t.Fatalf("CreateMemory(%s): %s", seed.id, err)
		}
	}

	cutoff := now - 7*day

	count, bytes, err := db.RetainedStats(context.Background(), cutoff)
	if err != nil {
		t.Fatalf("RetainedStats: %s", err)
	}

	if count != 2 {
		t.Errorf("expected 2 retained, got %d", count)
	}

	// Bodies plus the same per-row allowance eviction and the preview use, so the figure is
	// comparable with the capacity target rather than a bare sum of body lengths.
	if bytes <= 2*evictionRowOverheadBytes {
		t.Errorf("expected the byte figure to include bodies and row overhead, got %d", bytes)
	}

	// Recall renews retention with the decay clock, so a recalled old memory joins the window.
	if _, err := db.RecallMemories(context.Background(), []string{"old1"}); err != nil {
		t.Fatalf("RecallMemories: %s", err)
	}

	count, _, err = db.RetainedStats(context.Background(), cutoff)
	if err != nil {
		t.Fatalf("RetainedStats: %s", err)
	}

	if count != 3 {
		t.Errorf("expected the recalled memory to be retained, got %d", count)
	}
}

// TestRetainedStatsEmptyStore covers the SUM-over-no-rows case, which is NULL rather than 0.
func TestRetainedStatsEmptyStore(t *testing.T) {
	db := newTestDB(t)

	count, bytes, err := db.RetainedStats(context.Background(), time.Now().UnixNano())
	if err != nil {
		t.Fatalf("RetainedStats: %s", err)
	}

	if count != 0 || bytes != 0 {
		t.Errorf("expected 0/0 on an empty store, got %d/%d", count, bytes)
	}
}

func TestRetainedStats_QueryError(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	mock.ExpectQuery(`SELECT COUNT\(\*\), COALESCE\(SUM`).WillReturnError(errors.New("boom"))

	if _, _, err := d.RetainedStats(context.Background(), 0); err == nil {
		t.Fatal("expected an error")
	}

	expectationsMet(t, mock)
}

// TestRetainedStatsUsesTheDialectExpression pins the MAX/GREATEST branch, which is the one thing
// in this query that differs across the three drivers.
func TestRetainedStatsUsesTheDialectExpression(t *testing.T) {
	tests := []struct {
		name   string
		driver driver
		want   string
	}{
		{name: "sqlite", driver: driverSQLite, want: `MAX\(timestamp, time_recalled\)`},
		{name: "postgres", driver: driverPostgres, want: `GREATEST\(timestamp, time_recalled\)`},
		{name: "mysql", driver: driverMySQL, want: `GREATEST\(timestamp, time_recalled\)`},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			d, mock := newMockDB(t, test.driver)

			mock.ExpectQuery(test.want).
				WillReturnRows(sqlmock.NewRows([]string{"count", "bytes"}).AddRow(1, int64(10)))

			if _, _, err := d.RetainedStats(context.Background(), 0); err != nil {
				t.Fatalf("RetainedStats: %s", err)
			}

			expectationsMet(t, mock)
		})
	}
}
