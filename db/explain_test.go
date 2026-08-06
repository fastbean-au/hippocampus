package db

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/fastbean-au/hippocampus/types"
)

// TestGetMemoryConsolidationCandidates covers the lookup end to end: what it finds, what it leaves
// out, and the decision inputs it must carry for the caller to be able to value a memory at all.
func TestGetMemoryConsolidationCandidates(t *testing.T) {
	database, err := New("")
	if err != nil {
		t.Fatalf("New: %s", err)
	}

	t.Cleanup(func() { _ = database.Close() })

	ctx := context.Background()

	// The relationship significance is summed from the relationships by the store, not taken from
	// the field, so it is seeded the way a client would produce it.
	if _, err := database.CreateEvent(ctx, types.Event{
		Id:            "event-1",
		Name:          "an event",
		Significance:  7,
		TimeStart:     100,
		Relationships: []types.Relationship{{EventId: "another", Significance: 3}},
	}); err != nil {
		t.Fatalf("CreateEvent: %s", err)
	}

	memories := []types.Memory{
		{Id: "with-event", TimeStamp: 100, Significance: 2, EventId: "event-1", Body: "one"},
		{Id: "eventless", TimeStamp: 200, Significance: 4, Body: "two"},
		{Id: "dangling", TimeStamp: 300, Significance: 5, EventId: "gone", Body: "three"},
	}

	for _, memory := range memories {
		if _, err := database.CreateMemory(ctx, memory); err != nil {
			t.Fatalf("CreateMemory(%s): %s", memory.Id, err)
		}
	}

	candidates, err := database.GetMemoryConsolidationCandidates(ctx,
		[]string{"with-event", "eventless", "dangling", "never-existed"},
	)
	if err != nil {
		t.Fatalf("GetMemoryConsolidationCandidates: %s", err)
	}

	if len(candidates) != 3 {
		t.Fatalf("expected 3 candidates (the fourth id does not exist), got %d", len(candidates))
	}

	found := make(map[string]IdentifiedMemoryCandidate, len(candidates))
	for _, v := range candidates {
		found[v.Id] = v
	}

	withEvent := found["with-event"]

	if withEvent.EventId != "event-1" {
		t.Errorf("expected the event id reported, got %q", withEvent.EventId)
	}

	if withEvent.Candidate.MemorySignificance != 2 || withEvent.Candidate.EventSignificance != 7 {
		t.Errorf("expected significances 2/7, got %d/%d",
			withEvent.Candidate.MemorySignificance,
			withEvent.Candidate.EventSignificance,
		)
	}

	if withEvent.Candidate.RelationshipSignificance != 3 {
		t.Errorf("expected the event's relationship significance, got %d", withEvent.Candidate.RelationshipSignificance)
	}

	if withEvent.Candidate.Timestamp != 100 {
		t.Errorf("expected the decay clock's timestamp, got %d", withEvent.Candidate.Timestamp)
	}

	if found["eventless"].EventId != "" || found["eventless"].Candidate.EventSignificance != 0 {
		t.Error("expected a memory with no event to carry neither an event id nor an event significance")
	}

	// A dangling reference is scored as event-less, and must not report an event id a client could
	// try to open - the same treatment the consolidation passes give it.
	if found["dangling"].EventId != "" {
		t.Errorf("expected a dangling event reference to be reported as event-less, got %q", found["dangling"].EventId)
	}

	if found["dangling"].Candidate.MemorySignificance != 5 {
		t.Errorf("expected the memory's own significance to survive a dangling event, got %d", found["dangling"].Candidate.MemorySignificance)
	}
}

// TestGetMemoryConsolidationCandidatesNoIds covers the empty request: no ids means no query.
func TestGetMemoryConsolidationCandidatesNoIds(t *testing.T) {
	database, err := New("")
	if err != nil {
		t.Fatalf("New: %s", err)
	}

	t.Cleanup(func() { _ = database.Close() })

	candidates, err := database.GetMemoryConsolidationCandidates(context.Background(), nil)
	if err != nil {
		t.Fatalf("GetMemoryConsolidationCandidates: %s", err)
	}

	if len(candidates) != 0 {
		t.Errorf("expected no candidates, got %d", len(candidates))
	}
}

// TestGetMemoryConsolidationCandidatesChunks covers the chunked IN (...) clauses: more ids than one
// chunk holds must still find every memory among them, and must not deadlock on the single SQLite
// connection by leaving a chunk's rows open across the next chunk's query.
func TestGetMemoryConsolidationCandidatesChunks(t *testing.T) {
	database, err := New("")
	if err != nil {
		t.Fatalf("New: %s", err)
	}

	t.Cleanup(func() { _ = database.Close() })

	ctx := context.Background()

	// Two ids that exist, at either end of a request spanning three chunks.
	if _, err := database.CreateMemory(ctx, types.Memory{Id: "first", TimeStamp: 1, Significance: 1, Body: "x"}); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	if _, err := database.CreateMemory(ctx, types.Memory{Id: "last", TimeStamp: 1, Significance: 1, Body: "x"}); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	ids := []string{"first"}
	for i := range deleteChunkSize * 2 {
		ids = append(ids, fmt.Sprintf("absent-%d", i))
	}

	ids = append(ids, "last")

	candidates, err := database.GetMemoryConsolidationCandidates(ctx, ids)
	if err != nil {
		t.Fatalf("GetMemoryConsolidationCandidates: %s", err)
	}

	if len(candidates) != 2 {
		t.Fatalf("expected the 2 existing memories across the chunks, got %d", len(candidates))
	}
}

// --- error paths, following the package's sqlmock gap-test convention ---

// explainColumns is the projection GetMemoryConsolidationCandidates scans, so a mocked row matches
// it.
func explainColumns() []string {
	return []string{
		"id", "timestamp", "significance_level_id", "time_recalled", "recall_count", "event_id",
		"e_significance_level_id", "relationship_significance", "e_id",
	}
}

func TestGetMemoryConsolidationCandidates_RanksError(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)

	mock.ExpectQuery(`SELECT id, level_rank FROM significance_levels`).WillReturnError(errors.New("boom"))

	if _, err := d.GetMemoryConsolidationCandidates(context.Background(), []string{"m1"}); err == nil {
		t.Fatal("expected an error")
	}

	expectationsMet(t, mock)
}

func TestGetMemoryConsolidationCandidates_QueryError(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)
	emptyRanksQuery(mock)

	mock.ExpectQuery(`FROM memories m LEFT JOIN events e`).WillReturnError(errors.New("boom"))

	if _, err := d.GetMemoryConsolidationCandidates(context.Background(), []string{"m1"}); err == nil {
		t.Fatal("expected an error")
	}

	expectationsMet(t, mock)
}

func TestGetMemoryConsolidationCandidates_ScanError(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)
	emptyRanksQuery(mock)

	mock.ExpectQuery(`FROM memories m LEFT JOIN events e`).
		WillReturnRows(sqlmock.NewRows(explainColumns()).
			AddRow("m1", "not-an-int", nil, int64(0), int32(0), "", nil, int64(0), nil))

	if _, err := d.GetMemoryConsolidationCandidates(context.Background(), []string{"m1"}); err == nil {
		t.Fatal("expected a scan error")
	}

	expectationsMet(t, mock)
}

func TestGetMemoryConsolidationCandidates_RowsIterationError(t *testing.T) {
	d, mock := newMockDB(t, driverSQLite)
	emptyRanksQuery(mock)

	mock.ExpectQuery(`FROM memories m LEFT JOIN events e`).
		WillReturnRows(sqlmock.NewRows(explainColumns()).
			AddRow("m1", int64(1), nil, int64(0), int32(0), "", nil, int64(0), nil).
			RowError(0, errors.New("boom")))

	if _, err := d.GetMemoryConsolidationCandidates(context.Background(), []string{"m1"}); err == nil {
		t.Fatal("expected an iteration error")
	}

	expectationsMet(t, mock)
}
