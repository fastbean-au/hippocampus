package db

import (
	"context"
	"testing"
	"time"

	"github.com/fastbean-au/hippocampus/types"
)

func newTestDB(t *testing.T) *DB {
	t.Helper()

	db, err := New("")
	if err != nil {
		t.Fatalf("failed to create test DB: %s", err)
	}

	return db
}

// stubServer implements the Server interface with fixed answers, so DB-level consolidation scans
// can be tested without the hippocampus package.
type stubServer struct {
	consolidateMemories bool
	consolidateEvents   bool
}

func (s *stubServer) ShouldConsolidateMemory(candidate MemoryConsolidationCandidate) bool {
	return s.consolidateMemories
}

func (s *stubServer) ShouldConsolidateEvent(candidate EventConsolidationCandidate) bool {
	return s.consolidateEvents
}

func (s *stubServer) MemoryValue(candidate MemoryConsolidationCandidate) float64 {
	return 0
}

func (s *stubServer) MemoryRetained(candidate MemoryConsolidationCandidate) bool {
	return false
}

// TestConsolidateEvents verifies that the bare-event pass deletes only events without memories:
// an event with an associated memory must be left for the evented pass regardless of what the
// decision function says.
func TestConsolidateEvents(t *testing.T) {
	db := newTestDB(t)

	events := []types.Event{
		{Id: "bare", Name: "no memories", TimeStart: 100, Significance: 1},
		{Id: "evented", Name: "has a memory", TimeStart: 100, Significance: 1},
	}

	for _, e := range events {
		if _, err := db.CreateEvent(context.Background(), e); err != nil {
			t.Fatalf("CreateEvent(%s): %s", e.Id, err)
		}
	}

	if _, err := db.CreateMemory(context.Background(), types.Memory{Id: "m1", TimeStamp: 100, Significance: 1, EventId: "evented", Body: "x"}); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	deleted, err := db.ConsolidateEvents(context.Background(), &stubServer{consolidateEvents: true})
	if err != nil {
		t.Fatalf("ConsolidateEvents: %s", err)
	}

	if deleted != 1 {
		t.Errorf("expected 1 event deleted, got %d", deleted)
	}

	if _, err := db.GetEvent(context.Background(), "evented"); err != nil {
		t.Errorf("event with memories should survive the bare-event pass: %s", err)
	}

	if db.CountEvents(context.Background()) != 1 {
		t.Errorf("expected 1 remaining event, got %d", db.CountEvents(context.Background()))
	}
}

// TestCreateEvent_CalculatesRelationshipSignificance verifies that the stored
// RelationshipSignificance is computed from the event's relationships rather than taken from the
// caller. Before the fix, CalculateRelationshipSignificance was never invoked, so the stored
// value was always whatever the caller passed (typically zero) and relationships had no effect
// on consolidation.
func TestCreateEvent_CalculatesRelationshipSignificance(t *testing.T) {
	db := newTestDB(t)

	event := types.Event{
		Id:           "e1",
		Name:         "connected",
		TimeStart:    100,
		Significance: 1,
		Relationships: []types.Relationship{
			{EventId: "e2", Significance: 3},
			{EventId: "e3", Significance: 4},
		},
	}

	if _, err := db.CreateEvent(context.Background(), event); err != nil {
		t.Fatalf("CreateEvent: %s", err)
	}

	got, err := db.GetEvent(context.Background(), "e1")
	if err != nil {
		t.Fatalf("GetEvent: %s", err)
	}

	if got.RelationshipSignificance != 7 {
		t.Errorf("expected RelationshipSignificance 7, got %d", got.RelationshipSignificance)
	}
}

// TestGetEvents_TimeEndFilter verifies that filtering by TimeEnd range returns only events whose
// TimeEnd falls within the requested bounds. This was previously broken: the TimeEnd filter branch
// compared x against timeStartMin/timeStartMax instead of timeEndMin/timeEndMax.
func TestGetEvents_TimeEndFilter(t *testing.T) {
	db := newTestDB(t)

	events := []types.Event{
		{Id: "e1", Name: "early", TimeStart: 100, TimeEnd: 200, Significance: 1},
		{Id: "e2", Name: "mid", TimeStart: 300, TimeEnd: 500, Significance: 1},
		{Id: "e3", Name: "late", TimeStart: 600, TimeEnd: 900, Significance: 1},
	}

	for _, e := range events {
		if _, err := db.CreateEvent(context.Background(), e); err != nil {
			t.Fatalf("CreateEvent(%s): %s", e.Id, err)
		}
	}

	// Filter: timeEndMin=400, timeEndMax=800 — should match e2 (500) and e3 (900 is outside), so
	// only e2.
	got, err := db.GetEvents(context.Background(), EventFilter{TimeEndMin: 400, TimeEndMax: 800})
	if err != nil {
		t.Fatalf("GetEvents: %s", err)
	}

	if len(*got) != 1 {
		t.Errorf("expected 1 event, got %d", len(*got))
		for _, e := range *got {
			t.Logf("  id=%s timeEnd=%d", e.Id, e.TimeEnd)
		}

		return
	}

	if (*got)[0].Id != "e2" {
		t.Errorf("expected event e2, got %s", (*got)[0].Id)
	}
}

// TestGetEvents_TimeEndMaxOnly verifies the single-bound (max only) TimeEnd filter path, which
// previously had a wrong condition variable (timeStartMax instead of timeEndMax).
func TestGetEvents_TimeEndMaxOnly(t *testing.T) {
	db := newTestDB(t)

	events := []types.Event{
		{Id: "e1", Name: "early", TimeStart: 100, TimeEnd: 200, Significance: 1},
		{Id: "e2", Name: "late", TimeStart: 600, TimeEnd: 900, Significance: 1},
	}

	for _, e := range events {
		if _, err := db.CreateEvent(context.Background(), e); err != nil {
			t.Fatalf("CreateEvent(%s): %s", e.Id, err)
		}
	}

	// Filter: timeEndMax=500 — should match only e1 (TimeEnd=200).
	got, err := db.GetEvents(context.Background(), EventFilter{TimeEndMax: 500})
	if err != nil {
		t.Fatalf("GetEvents: %s", err)
	}

	if len(*got) != 1 {
		t.Errorf("expected 1 event, got %d", len(*got))
		for _, e := range *got {
			t.Logf("  id=%s timeEnd=%d", e.Id, e.TimeEnd)
		}

		return
	}

	if (*got)[0].Id != "e1" {
		t.Errorf("expected event e1, got %s", (*got)[0].Id)
	}
}

// TestUpdateEvent_PartialUpdate verifies the conditional-overwrite semantics of UpdateEvent: only
// fields carrying a non-zero value replace the stored ones, and the stored relationships (and
// their calculated significance) are only replaced when new relationships are provided.
func TestUpdateEvent_PartialUpdate(t *testing.T) {
	db := newTestDB(t)

	event := types.Event{
		Id:           "e1",
		Name:         "original",
		Description:  "a description",
		TimeStart:    100,
		TimeEnd:      200,
		Significance: 5,
		Relationships: []types.Relationship{
			{EventId: "e2", Significance: 3},
			{EventId: "e3", Significance: 4},
		},
	}

	if _, err := db.CreateEvent(context.Background(), event); err != nil {
		t.Fatalf("CreateEvent: %s", err)
	}

	// Update only the description: every other field, including the relationships, must be
	// preserved.
	if ok, err := db.UpdateEvent(context.Background(), types.Event{Id: "e1", Description: "updated"}); err != nil {
		t.Fatalf("UpdateEvent: %s", err)
	} else if !ok {
		t.Fatal("UpdateEvent reported the existing event as missing")
	}

	got, err := db.GetEvent(context.Background(), "e1")
	if err != nil {
		t.Fatalf("GetEvent: %s", err)
	}

	if got.Description != "updated" {
		t.Errorf("expected description 'updated', got '%s'", got.Description)
	}

	if got.Name != "original" || got.TimeStart != 100 || got.TimeEnd != 200 || got.Significance != 5 {
		t.Errorf("zero-valued fields must not clobber stored values, got %+v", got)
	}

	if len(got.Relationships) != 2 || got.RelationshipSignificance != 7 {
		t.Errorf("relationships must be preserved when none are provided, got %d relationships with significance %d", len(got.Relationships), got.RelationshipSignificance)
	}

	// Update with new relationships: the relationship significance must be recalculated.
	if ok, err := db.UpdateEvent(context.Background(), types.Event{Id: "e1", Relationships: []types.Relationship{{EventId: "e4", Significance: 10}}}); err != nil {
		t.Fatalf("UpdateEvent: %s", err)
	} else if !ok {
		t.Fatal("UpdateEvent reported the existing event as missing")
	}

	got, err = db.GetEvent(context.Background(), "e1")
	if err != nil {
		t.Fatalf("GetEvent: %s", err)
	}

	if len(got.Relationships) != 1 || got.RelationshipSignificance != 10 {
		t.Errorf("expected 1 relationship with significance 10, got %d with %d", len(got.Relationships), got.RelationshipSignificance)
	}
}

// TestUpdateEvent_DoesNotInsertMissing verifies that updating an event that does not exist neither
// creates a row nor errors: it reports (false, nil) so the RPC layer can surface NotFound. The
// previous upsert semantics silently inserted a phantom event, which poisoned eviction's LEFT JOIN
// and the event-consolidation scans.
func TestUpdateEvent_DoesNotInsertMissing(t *testing.T) {
	db := newTestDB(t)

	ok, err := db.UpdateEvent(context.Background(), types.Event{Id: "new", Name: "created by update", TimeStart: 100, Significance: 2})
	if err != nil {
		t.Fatalf("UpdateEvent: %s", err)
	}

	if ok {
		t.Fatal("UpdateEvent reported an unknown event as existing")
	}

	if _, err := db.GetEvent(context.Background(), "new"); err == nil {
		t.Fatal("UpdateEvent created a phantom event for an unknown id; expected no row")
	}
}

// TestUpdateEvent_EmptyIdCreatesNoRow guards specifically against the poisonous id = ” row: an
// empty-id update must not create one, because every event-less memory has event_id = ” and would
// otherwise LEFT JOIN to it in eviction.
func TestUpdateEvent_EmptyIdCreatesNoRow(t *testing.T) {
	db := newTestDB(t)

	ok, err := db.UpdateEvent(context.Background(), types.Event{Id: "", Significance: 9})
	if err != nil {
		t.Fatalf("UpdateEvent: %s", err)
	}

	if ok {
		t.Fatal("UpdateEvent reported an empty-id event as existing")
	}

	if n := db.CountEvents(context.Background()); n != 0 {
		t.Fatalf("expected no events after an empty-id update, got %d", n)
	}
}

// TestGetEvents_CombinedFilters verifies that filters on different columns compose: a time_start
// lower bound combined with a significance upper bound.
func TestGetEvents_CombinedFilters(t *testing.T) {
	db := newTestDB(t)

	events := []types.Event{
		{Id: "e1", Name: "early minor", TimeStart: 100, Significance: 1},
		{Id: "e2", Name: "mid moderate", TimeStart: 300, Significance: 3},
		{Id: "e3", Name: "late major", TimeStart: 500, Significance: 5},
	}

	for _, e := range events {
		if _, err := db.CreateEvent(context.Background(), e); err != nil {
			t.Fatalf("CreateEvent(%s): %s", e.Id, err)
		}
	}

	// time_start >= 200 and significance <= 4 — only e2 matches both.
	got, err := db.GetEvents(context.Background(), EventFilter{TimeStartMin: 200, SignificanceMax: 4})
	if err != nil {
		t.Fatalf("GetEvents: %s", err)
	}

	if len(*got) != 1 {
		t.Fatalf("expected 1 event, got %d", len(*got))
	}

	if (*got)[0].Id != "e2" {
		t.Errorf("expected event e2, got %s", (*got)[0].Id)
	}
}

// TestGetEvents_SignificanceExtremum verifies HIGHEST/LOWEST return every event tied at that
// significance value (not just one), computed dynamically rather than against a caller-supplied
// bound.
func TestGetEvents_SignificanceExtremum(t *testing.T) {
	db := newTestDB(t)

	events := []types.Event{
		{Id: "e1", Name: "low a", TimeStart: 100, Significance: 2},
		{Id: "e2", Name: "low b", TimeStart: 200, Significance: 2},
		{Id: "e3", Name: "mid", TimeStart: 300, Significance: 5},
		{Id: "e4", Name: "high a", TimeStart: 400, Significance: 9},
		{Id: "e5", Name: "high b", TimeStart: 500, Significance: 9},
	}

	for _, e := range events {
		if _, err := db.CreateEvent(context.Background(), e); err != nil {
			t.Fatalf("CreateEvent(%s): %s", e.Id, err)
		}
	}

	highest, err := db.GetEvents(context.Background(), EventFilter{SignificanceExtremum: SignificanceExtremumHighest})
	if err != nil {
		t.Fatalf("GetEvents(highest): %s", err)
	}

	if len(*highest) != 2 {
		t.Fatalf("expected 2 events tied at the highest significance, got %d: %+v", len(*highest), *highest)
	}

	for _, e := range *highest {
		if e.Significance != 9 {
			t.Errorf("expected significance 9, got %d for %s", e.Significance, e.Id)
		}
	}

	lowest, err := db.GetEvents(context.Background(), EventFilter{SignificanceExtremum: SignificanceExtremumLowest})
	if err != nil {
		t.Fatalf("GetEvents(lowest): %s", err)
	}

	if len(*lowest) != 2 {
		t.Fatalf("expected 2 events tied at the lowest significance, got %d: %+v", len(*lowest), *lowest)
	}

	for _, e := range *lowest {
		if e.Significance != 2 {
			t.Errorf("expected significance 2, got %d for %s", e.Significance, e.Id)
		}
	}

	total, err := db.CountEventsFiltered(context.Background(), EventFilter{SignificanceExtremum: SignificanceExtremumHighest})
	if err != nil {
		t.Fatalf("CountEventsFiltered(highest): %s", err)
	}

	if total != 2 {
		t.Errorf("expected CountEventsFiltered(highest) = 2, got %d", total)
	}
}

// TestGetEvents_SignificanceExtremum_ComposesWithOtherFilters verifies the extremum is computed
// only over events matching the other filters (group here), not the whole store.
func TestGetEvents_SignificanceExtremum_ComposesWithOtherFilters(t *testing.T) {
	db := newTestDB(t)

	events := []types.Event{
		{Id: "e1", Name: "billing low", TimeStart: 100, Significance: 3, Group: "billing"},
		{Id: "e2", Name: "billing high", TimeStart: 200, Significance: 7, Group: "billing"},
		{Id: "e3", Name: "ingest highest", TimeStart: 300, Significance: 20, Group: "ingest"},
	}

	for _, e := range events {
		if _, err := db.CreateEvent(context.Background(), e); err != nil {
			t.Fatalf("CreateEvent(%s): %s", e.Id, err)
		}
	}

	got, err := db.GetEvents(context.Background(), EventFilter{Group: "billing", SignificanceExtremum: SignificanceExtremumHighest})
	if err != nil {
		t.Fatalf("GetEvents: %s", err)
	}

	if len(*got) != 1 || (*got)[0].Id != "e2" {
		t.Fatalf("expected only e2 (highest within group 'billing'), got %+v", *got)
	}
}

// TestEventGroup verifies the group label round-trips through create/read, filters GetEvents,
// and follows the upsert's only-non-zero-values-overwrite rule on update.
func TestEventGroup(t *testing.T) {
	db := newTestDB(t)

	events := []types.Event{
		{Id: "e1", Name: "one", TimeStart: 100, Significance: 1, Group: "billing"},
		{Id: "e2", Name: "two", TimeStart: 100, Significance: 1, Group: "ingest"},
		{Id: "e3", Name: "three", TimeStart: 100, Significance: 1},
	}

	for _, e := range events {
		if _, err := db.CreateEvent(context.Background(), e); err != nil {
			t.Fatalf("CreateEvent(%s): %s", e.Id, err)
		}
	}

	if e, err := db.GetEvent(context.Background(), "e1"); err != nil || e.Group != "billing" {
		t.Errorf("expected e1 to carry group 'billing', got %+v (%v)", e, err)
	}

	if e, err := db.GetEvent(context.Background(), "e3"); err != nil || e.Group != "" {
		t.Errorf("expected e3 to carry no group, got %+v (%v)", e, err)
	}

	got, err := db.GetEvents(context.Background(), EventFilter{Group: "billing"})
	if err != nil {
		t.Fatalf("GetEvents: %s", err)
	}

	if len(*got) != 1 || (*got)[0].Id != "e1" {
		t.Errorf("expected only e1 in group 'billing', got %v", *got)
	}

	// An update without a group must leave the stored group untouched.
	if ok, err := db.UpdateEvent(context.Background(), types.Event{Id: "e1", Significance: 2}); err != nil {
		t.Fatalf("UpdateEvent: %s", err)
	} else if !ok {
		t.Fatal("UpdateEvent reported the existing event as missing")
	}

	if e, err := db.GetEvent(context.Background(), "e1"); err != nil || e.Group != "billing" {
		t.Errorf("expected e1 to keep group 'billing' after an update without one, got %+v (%v)", e, err)
	}

	// An update carrying a group overwrites it.
	if ok, err := db.UpdateEvent(context.Background(), types.Event{Id: "e1", Group: "ops"}); err != nil {
		t.Fatalf("UpdateEvent: %s", err)
	} else if !ok {
		t.Fatal("UpdateEvent reported the existing event as missing")
	}

	if e, err := db.GetEvent(context.Background(), "e1"); err != nil || e.Group != "ops" {
		t.Errorf("expected e1 to carry group 'ops' after the update, got %+v (%v)", e, err)
	}
}

// TestCreateEvent_DefaultsTimeStart verifies that time_start is optional on create - a zero
// value defaults to the current time (SetDefaults runs before Validate) rather than being rejected.
func TestCreateEvent_DefaultsTimeStart(t *testing.T) {
	db := newTestDB(t)

	before := time.Now().UnixNano()

	id, err := db.CreateEvent(context.Background(), types.Event{Name: "no start", Significance: 1})
	if err != nil {
		t.Fatalf("CreateEvent with a zero time_start should default it, got: %s", err)
	}

	got, err := db.GetEvent(context.Background(), id)
	if err != nil {
		t.Fatalf("GetEvent: %s", err)
	}

	if got.TimeStart < before {
		t.Errorf("expected time_start defaulted to ~now (>= %d), got %d", before, got.TimeStart)
	}
}

// ids maps a slice of events to their ids, in order, for concise ordering assertions.
func ids(events *[]types.Event) []string {
	out := make([]string, len(*events))

	for i, e := range *events {
		out[i] = e.Id
	}

	return out
}

func equalStrings(a []string, b []string) bool {
	if len(a) != len(b) {
		return false
	}

	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}

	return true
}

// TestGetEventsSortingAndPagination pins the two supported sort orders and the LIMIT/OFFSET paging
// against a fixed set of events, plus CountEventsFiltered ignoring the page window.
func TestGetEventsSortingAndPagination(t *testing.T) {
	db := newTestDB(t)

	events := []types.Event{
		{Id: "a", Name: "a", TimeStart: 100, Significance: 30, Group: "g1"},
		{Id: "b", Name: "b", TimeStart: 200, Significance: 50, Group: "g1"},
		{Id: "c", Name: "c", TimeStart: 300, Significance: 50, Group: "g2"},
		{Id: "d", Name: "d", TimeStart: 400, Significance: 10, Group: "g2"},
	}

	for _, e := range events {
		if _, err := db.CreateEvent(context.Background(), e); err != nil {
			t.Fatalf("CreateEvent(%s): %s", e.Id, err)
		}
	}

	// significance: sig desc, then time desc, then id asc — c and b tie on 50 so newer (c) wins.
	bySig, err := db.GetEvents(context.Background(), EventFilter{OrderBy: "significance"})
	if err != nil {
		t.Fatalf("GetEvents(significance): %s", err)
	}

	if want := []string{"c", "b", "a", "d"}; !equalStrings(ids(bySig), want) {
		t.Errorf("significance order = %v, want %v", ids(bySig), want)
	}

	// timestamp: time desc only.
	byTime, err := db.GetEvents(context.Background(), EventFilter{OrderBy: "timestamp"})
	if err != nil {
		t.Fatalf("GetEvents(timestamp): %s", err)
	}

	if want := []string{"d", "c", "b", "a"}; !equalStrings(ids(byTime), want) {
		t.Errorf("timestamp order = %v, want %v", ids(byTime), want)
	}

	// an empty/unknown order_by falls back to significance.
	byDefault, err := db.GetEvents(context.Background(), EventFilter{})
	if err != nil {
		t.Fatalf("GetEvents(default): %s", err)
	}

	if want := []string{"c", "b", "a", "d"}; !equalStrings(ids(byDefault), want) {
		t.Errorf("default order = %v, want %v", ids(byDefault), want)
	}

	// paging over the significance order: page 1 and page 2 partition the list with no overlap.
	page1, err := db.GetEvents(context.Background(), EventFilter{OrderBy: "significance", Limit: 2, Offset: 0})
	if err != nil {
		t.Fatalf("GetEvents(page1): %s", err)
	}

	if want := []string{"c", "b"}; !equalStrings(ids(page1), want) {
		t.Errorf("page1 = %v, want %v", ids(page1), want)
	}

	page2, err := db.GetEvents(context.Background(), EventFilter{OrderBy: "significance", Limit: 2, Offset: 2})
	if err != nil {
		t.Fatalf("GetEvents(page2): %s", err)
	}

	if want := []string{"a", "d"}; !equalStrings(ids(page2), want) {
		t.Errorf("page2 = %v, want %v", ids(page2), want)
	}

	// CountEventsFiltered ignores Limit/Offset and reflects the filter.
	total, err := db.CountEventsFiltered(context.Background(), EventFilter{Limit: 1, Offset: 3})
	if err != nil {
		t.Fatalf("CountEventsFiltered(all): %s", err)
	}

	if total != 4 {
		t.Errorf("total count = %d, want 4", total)
	}

	g2, err := db.CountEventsFiltered(context.Background(), EventFilter{Group: "g2"})
	if err != nil {
		t.Fatalf("CountEventsFiltered(g2): %s", err)
	}

	if g2 != 2 {
		t.Errorf("group g2 count = %d, want 2", g2)
	}

	sig, err := db.CountEventsFiltered(context.Background(), EventFilter{SignificanceMin: 50})
	if err != nil {
		t.Fatalf("CountEventsFiltered(sig>=50): %s", err)
	}

	if sig != 2 {
		t.Errorf("significance>=50 count = %d, want 2", sig)
	}
}
