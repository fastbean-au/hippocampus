package hippocampus

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/fastbean-au/hippocampus/contract"
	"github.com/fastbean-au/hippocampus/db"
	"github.com/fastbean-au/hippocampus/types"
)

// newEventTestServer builds a Server over an in-memory database, without autoSleep, for exercising
// the event RPCs directly.
func newEventTestServer(t *testing.T) *Server {
	t.Helper()

	database, err := db.New("")
	if err != nil {
		t.Fatalf("db.New: %s", err)
	}

	t.Cleanup(func() { _ = database.Close() })

	return &Server{db: database}
}

// TestEndEvent_EmptyIdRejected is a regression test: EndEvent used to pass an
// empty id straight into db.UpdateEvent's upsert, inserting a poisonous id = ” event row that
// every event-less memory (event_id = ”) then LEFT JOINs to in eviction. The RPC must reject an
// empty id and create nothing.
func TestEndEvent_EmptyIdRejected(t *testing.T) {
	s := newEventTestServer(t)

	res, err := s.EndEvent(context.Background(), &contract.EndEventRequest{Id: ""})
	if err == nil {
		t.Fatal("EndEvent accepted an empty id; expected an error")
	}

	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("expected InvalidArgument, got %s", status.Code(err))
	}

	if res.GetOk() {
		t.Error("EndEvent reported success for an empty id")
	}

	if n := s.db.CountEvents(context.Background()); n != 0 {
		t.Fatalf("EndEvent with an empty id created %d event(s); expected none", n)
	}
}

// TestEndEvent_UnknownIdNotFound verifies EndEvent no longer upserts a phantom event for an unknown
// id: it must return NotFound and leave the store empty.
func TestEndEvent_UnknownIdNotFound(t *testing.T) {
	s := newEventTestServer(t)

	res, err := s.EndEvent(context.Background(), &contract.EndEventRequest{Id: "nope", TimeEnd: 500})
	if err == nil {
		t.Fatal("EndEvent accepted an unknown id; expected NotFound")
	}

	if status.Code(err) != codes.NotFound {
		t.Errorf("expected NotFound, got %s", status.Code(err))
	}

	if res.GetOk() {
		t.Error("EndEvent reported success for an unknown id")
	}

	if n := s.db.CountEvents(context.Background()); n != 0 {
		t.Fatalf("EndEvent with an unknown id created %d event(s); expected none", n)
	}
}

// TestEndEvent_Success confirms the happy path still ends an existing event and reports Ok.
func TestEndEvent_Success(t *testing.T) {
	s := newEventTestServer(t)

	if _, err := s.db.CreateEvent(context.Background(), types.Event{Id: "e1", Name: "one", TimeStart: 100, Significance: 5}); err != nil {
		t.Fatalf("CreateEvent: %s", err)
	}

	res, err := s.EndEvent(context.Background(), &contract.EndEventRequest{Id: "e1", TimeEnd: 900})
	if err != nil {
		t.Fatalf("EndEvent: %s", err)
	}

	if !res.GetOk() {
		t.Error("EndEvent did not report Ok for a successful end")
	}

	got, err := s.db.GetEvent(context.Background(), "e1")
	if err != nil {
		t.Fatalf("GetEvent: %s", err)
	}

	if got.TimeEnd != 900 {
		t.Errorf("expected time_end 900, got %d", got.TimeEnd)
	}
}

// TestUpdateEventSignificance_EmptyIdRejected mirrors TestEndEvent_EmptyIdRejected for the other
// RPC that fed db.UpdateEvent unvalidated.
func TestUpdateEventSignificance_EmptyIdRejected(t *testing.T) {
	s := newEventTestServer(t)

	res, err := s.UpdateEventSignificance(context.Background(), &contract.UpdateEventSignificanceRequest{Id: "", Significance: 7})
	if err == nil {
		t.Fatal("UpdateEventSignificance accepted an empty id; expected an error")
	}

	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("expected InvalidArgument, got %s", status.Code(err))
	}

	if res.GetOk() {
		t.Error("UpdateEventSignificance reported success for an empty id")
	}

	if n := s.db.CountEvents(context.Background()); n != 0 {
		t.Fatalf("UpdateEventSignificance with an empty id created %d event(s); expected none", n)
	}
}

// TestUpdateEventSignificance_UnknownIdNotFound verifies the unknown-id path returns NotFound and
// creates nothing.
func TestUpdateEventSignificance_UnknownIdNotFound(t *testing.T) {
	s := newEventTestServer(t)

	res, err := s.UpdateEventSignificance(context.Background(), &contract.UpdateEventSignificanceRequest{Id: "nope", Significance: 7})
	if err == nil {
		t.Fatal("UpdateEventSignificance accepted an unknown id; expected NotFound")
	}

	if status.Code(err) != codes.NotFound {
		t.Errorf("expected NotFound, got %s", status.Code(err))
	}

	if res.GetOk() {
		t.Error("UpdateEventSignificance reported success for an unknown id")
	}

	if n := s.db.CountEvents(context.Background()); n != 0 {
		t.Fatalf("UpdateEventSignificance with an unknown id created %d event(s); expected none", n)
	}
}

// TestUpdateEventSignificance_Success confirms the happy path updates the significance and reports
// Ok.
func TestUpdateEventSignificance_Success(t *testing.T) {
	s := newEventTestServer(t)

	if _, err := s.db.CreateEvent(context.Background(), types.Event{Id: "e1", Name: "one", TimeStart: 100, Significance: 5}); err != nil {
		t.Fatalf("CreateEvent: %s", err)
	}

	res, err := s.UpdateEventSignificance(context.Background(), &contract.UpdateEventSignificanceRequest{Id: "e1", Significance: 42})
	if err != nil {
		t.Fatalf("UpdateEventSignificance: %s", err)
	}

	if !res.GetOk() {
		t.Error("UpdateEventSignificance did not report Ok for a successful update")
	}

	got, err := s.db.GetEvent(context.Background(), "e1")
	if err != nil {
		t.Fatalf("GetEvent: %s", err)
	}

	if got.Significance != 42 {
		t.Errorf("expected significance 42, got %d", got.Significance)
	}
}

// failUnsetStore wraps a real db.Store but forces UnsetMemoriesEventId to fail, so DeleteEvent's
// detach branch can be driven down its error path without a full hand-written mock.
type failUnsetStore struct {
	db.Store
	err error
}

func (f failUnsetStore) UnsetMemoriesEventId(ctx context.Context, eventId string) (int, error) {
	return 0, f.err
}

// TestDeleteEvent_DetachSuccess is a regression test: a successful DeleteEvent
// must report Ok (every other GeneralResponse RPC does), and the detach arm (memories: false) must
// leave the event's memories in place with their event_id cleared rather than deleting them.
func TestDeleteEvent_DetachSuccess(t *testing.T) {
	s := newEventTestServer(t)

	if _, err := s.db.CreateEvent(context.Background(), types.Event{Id: "e1", Name: "one", TimeStart: 100, Significance: 5}); err != nil {
		t.Fatalf("CreateEvent: %s", err)
	}

	if _, err := s.db.CreateMemory(context.Background(), types.Memory{Id: "m1", TimeStamp: 100, Significance: 5, EventId: "e1", Body: "x"}); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	res, err := s.DeleteEvent(context.Background(), &contract.DeleteEventRequest{Id: "e1", Memories: false})
	if err != nil {
		t.Fatalf("DeleteEvent: %s", err)
	}

	if !res.GetOk() {
		t.Error("DeleteEvent did not report Ok for a successful detach")
	}

	if _, err := s.db.GetEvent(context.Background(), "e1"); err == nil {
		t.Error("expected event e1 to be deleted")
	}

	with, without := s.db.CountMemories(context.Background())
	if with != 0 || without != 1 {
		t.Errorf("expected the memory detached (0 with event, 1 without), got %d with / %d without", with, without)
	}
}

// TestDeleteEvent_WithMemoriesSuccess verifies the memories: true arm deletes the event's memories
// and still reports Ok.
func TestDeleteEvent_WithMemoriesSuccess(t *testing.T) {
	s := newEventTestServer(t)

	if _, err := s.db.CreateEvent(context.Background(), types.Event{Id: "e1", Name: "one", TimeStart: 100, Significance: 5}); err != nil {
		t.Fatalf("CreateEvent: %s", err)
	}

	if _, err := s.db.CreateMemory(context.Background(), types.Memory{Id: "m1", TimeStamp: 100, Significance: 5, EventId: "e1", Body: "x"}); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	res, err := s.DeleteEvent(context.Background(), &contract.DeleteEventRequest{Id: "e1", Memories: true})
	if err != nil {
		t.Fatalf("DeleteEvent: %s", err)
	}

	if !res.GetOk() {
		t.Error("DeleteEvent did not report Ok for a successful delete-with-memories")
	}

	if with, without := s.db.CountMemories(context.Background()); with != 0 || without != 0 {
		t.Errorf("expected the memory deleted, got %d with / %d without", with, without)
	}
}

// TestDeleteEvent_UnsetErrorSurfaces verifies that a failure clearing the memories' event_id in the
// detach arm surfaces to the caller instead of being swallowed as a nil error: the event is gone but
// the memories still point at it, which the client must be told about. The raw storage error is
// masked to codes.Internal by mapError (the detail is logged server-side, not leaked to the client),
// so the assertion is on the code and Ok, not on the underlying error text.
func TestDeleteEvent_UnsetErrorSurfaces(t *testing.T) {
	s := newEventTestServer(t)

	if _, err := s.db.CreateEvent(context.Background(), types.Event{Id: "e1", Name: "one", TimeStart: 100, Significance: 5}); err != nil {
		t.Fatalf("CreateEvent: %s", err)
	}

	wantErr := errors.New("unset failed")
	s.db = failUnsetStore{Store: s.db, err: wantErr}

	res, err := s.DeleteEvent(context.Background(), &contract.DeleteEventRequest{Id: "e1", Memories: false})
	if err == nil {
		t.Fatal("DeleteEvent swallowed the UnsetMemoriesEventId failure; expected an error")
	}

	if got := status.Code(err); got != codes.Internal {
		t.Errorf("expected the detach failure masked to codes.Internal, got %s (%v)", got, err)
	}

	if res.GetOk() {
		t.Error("DeleteEvent reported Ok despite the detach failing")
	}
}

// TestDeleteEvent_EmptyIdRejected is a regression test: DeleteEvent used to pass
// an unvalidated id into the store, so an empty id with memories: true ran
// DELETE FROM memories WHERE event_id = ” and wiped every memory not associated with any event.
// The RPC must reject an empty id with InvalidArgument and leave those memories untouched.
func TestDeleteEvent_EmptyIdRejected(t *testing.T) {
	s := newEventTestServer(t)

	// An event-less memory (event_id = '') - exactly what the empty-id delete would have swept away.
	if _, err := s.db.CreateMemory(context.Background(), types.Memory{Id: "m1", TimeStamp: 100, Significance: 5, Body: "keep me"}); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	res, err := s.DeleteEvent(context.Background(), &contract.DeleteEventRequest{Id: "", Memories: true})
	if err == nil {
		t.Fatal("DeleteEvent accepted an empty id; expected an error")
	}

	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("expected InvalidArgument, got %s", status.Code(err))
	}

	if res.GetOk() {
		t.Error("DeleteEvent reported success for an empty id")
	}

	if with, without := s.db.CountMemories(context.Background()); with != 0 || without != 1 {
		t.Fatalf("empty-id DeleteEvent deleted event-less memories: got %d with / %d without, expected 0 / 1", with, without)
	}
}

// TestDeleteEvent_UnknownIdNotFound verifies DeleteEvent returns NotFound for an id that matches no
// event, rather than reporting Ok unconditionally, matching EndEvent and
// UpdateEventSignificance.
func TestDeleteEvent_UnknownIdNotFound(t *testing.T) {
	s := newEventTestServer(t)

	res, err := s.DeleteEvent(context.Background(), &contract.DeleteEventRequest{Id: "nope", Memories: true})
	if err == nil {
		t.Fatal("DeleteEvent accepted an unknown id; expected NotFound")
	}

	if status.Code(err) != codes.NotFound {
		t.Errorf("expected NotFound, got %s", status.Code(err))
	}

	if res.GetOk() {
		t.Error("DeleteEvent reported success for an unknown id")
	}
}

// TestMergeEvents_NonexistentTargetRejected verifies the prevention half for merges: a
// merge into a nonexistent merge_to is rejected with FailedPrecondition and no memories are moved,
// so a typo cannot turn a whole event's memories into dangling references in one call.
func TestMergeEvents_NonexistentTargetRejected(t *testing.T) {
	s := newEventTestServer(t)

	if _, err := s.db.CreateEvent(context.Background(), types.Event{Id: "from", Name: "source", TimeStart: 100, Significance: 5}); err != nil {
		t.Fatalf("CreateEvent: %s", err)
	}

	if _, err := s.db.CreateMemory(context.Background(), types.Memory{Id: "m1", TimeStamp: 100, Significance: 5, EventId: "from", Body: "x"}); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	res, err := s.MergeEvents(context.Background(), &contract.MergeEventsRequest{MergeFrom: "from", MergeTo: "ghost"})
	if err == nil {
		t.Fatal("MergeEvents accepted a nonexistent merge_to; expected an error")
	}

	if status.Code(err) != codes.FailedPrecondition {
		t.Errorf("expected FailedPrecondition, got %s", status.Code(err))
	}

	if res.GetOk() {
		t.Error("MergeEvents reported Ok despite the nonexistent target")
	}

	// The memory must still belong to its original event, not the phantom target.
	got, err := s.db.GetMemoriesByIds(context.Background(), []string{"m1"})
	if err != nil {
		t.Fatalf("GetMemoriesByIds: %s", err)
	}

	if (*got)[0].EventId != "from" {
		t.Errorf("expected m1 still attached to 'from', got %q", (*got)[0].EventId)
	}
}

// TestMergeEvents_EmptyIdsRejected verifies both ids are required: an absent merge_from or
// merge_to is rejected before any store call.
func TestMergeEvents_EmptyIdsRejected(t *testing.T) {
	s := newEventTestServer(t)

	if _, err := s.MergeEvents(context.Background(), &contract.MergeEventsRequest{MergeFrom: "", MergeTo: "dst"}); err == nil {
		t.Error("expected an error for an empty merge_from")
	}

	if _, err := s.MergeEvents(context.Background(), &contract.MergeEventsRequest{MergeFrom: "src", MergeTo: ""}); err == nil {
		t.Error("expected an error for an empty merge_to")
	}
}

// TestMergeEvents_Success verifies the happy path re-points merge_from's memories onto an existing
// merge_to and reports Ok.
func TestMergeEvents_Success(t *testing.T) {
	s := newEventTestServer(t)

	if _, err := s.db.CreateEvent(context.Background(), types.Event{Id: "src", Name: "source", TimeStart: 100, Significance: 5}); err != nil {
		t.Fatalf("CreateEvent(src): %s", err)
	}

	if _, err := s.db.CreateEvent(context.Background(), types.Event{Id: "dst", Name: "dest", TimeStart: 100, Significance: 5}); err != nil {
		t.Fatalf("CreateEvent(dst): %s", err)
	}

	for _, id := range []string{"m1", "m2"} {
		if _, err := s.db.CreateMemory(context.Background(), types.Memory{Id: id, TimeStamp: 100, Significance: 5, EventId: "src", Body: "x"}); err != nil {
			t.Fatalf("CreateMemory(%s): %s", id, err)
		}
	}

	res, err := s.MergeEvents(context.Background(), &contract.MergeEventsRequest{MergeFrom: "src", MergeTo: "dst"})
	if err != nil {
		t.Fatalf("MergeEvents: %s", err)
	}

	if !res.GetOk() {
		t.Error("expected Ok for a successful merge")
	}

	moved, err := s.db.GetMemoriesByEventId(context.Background(), "dst")
	if err != nil {
		t.Fatalf("GetMemoriesByEventId(dst): %s", err)
	}

	if len(*moved) != 2 {
		t.Errorf("expected 2 memories re-pointed onto dst, got %d", len(*moved))
	}
}

// TestMergeEvents_SetsOkOnSuccess is a regression test: a successful merge used to leave the
// GeneralResponse Ok at its zero value (false) even though the memories moved, inconsistent with
// every other GeneralResponse RPC (EndEvent, DeleteEvent, UpdateEventSignificance). Ok must be
// true once the merge succeeds.
func TestMergeEvents_SetsOkOnSuccess(t *testing.T) {
	s := newEventTestServer(t)

	if _, err := s.db.CreateEvent(context.Background(), types.Event{Id: "src", Name: "source", TimeStart: 100, Significance: 5}); err != nil {
		t.Fatalf("CreateEvent(src): %s", err)
	}

	if _, err := s.db.CreateEvent(context.Background(), types.Event{Id: "dst", Name: "dest", TimeStart: 100, Significance: 5}); err != nil {
		t.Fatalf("CreateEvent(dst): %s", err)
	}

	res, err := s.MergeEvents(context.Background(), &contract.MergeEventsRequest{MergeFrom: "src", MergeTo: "dst"})
	if err != nil {
		t.Fatalf("MergeEvents: %s", err)
	}

	if !res.GetOk() {
		t.Error("expected Ok=true after a successful merge")
	}
}

// TestStoreEvent_StoresNestedMemories verifies the nested-memory path: memories carried on the
// event are stored, defaulted onto the new event id, and counted in the response.
func TestStoreEvent_StoresNestedMemories(t *testing.T) {
	s := newEventTestServer(t)

	res, err := s.StoreEvent(context.Background(), &contract.Event{
		Name:         "trip",
		TimeStart:    100,
		Significance: 5,
		Memories: []*contract.Memory{
			{Significance: 5, Body: "arrival"},
			{Significance: 5, Body: "departure"},
		},
	})
	if err != nil {
		t.Fatalf("StoreEvent: %s", err)
	}

	if res.GetMemoryCount() != 2 {
		t.Fatalf("expected 2 nested memories stored, got %d", res.GetMemoryCount())
	}

	attached, err := s.db.GetMemoriesByEventId(context.Background(), res.GetId())
	if err != nil {
		t.Fatalf("GetMemoriesByEventId: %s", err)
	}

	if len(*attached) != 2 {
		t.Errorf("expected 2 memories attached to the new event, got %d", len(*attached))
	}
}

// TestStoreEvent_NestedMemoryDroppedNotCounted is a regression test: a nested memory dropped for
// insignificance returns (rejected, no error), and the old code counted it towards memory_count
// because it only checked err. memory_count must reflect only the memories actually retained.
func TestStoreEvent_NestedMemoryDroppedNotCounted(t *testing.T) {
	s := newEventTestServer(t)
	s.minimumMemorySignificance = 10

	res, err := s.StoreEvent(context.Background(), &contract.Event{
		Name:         "trip",
		TimeStart:    100,
		Significance: 5,
		Memories: []*contract.Memory{
			{Significance: 20, Body: "kept"},
			{Significance: 5, Body: "dropped"},
		},
	})
	if err != nil {
		t.Fatalf("StoreEvent: %s", err)
	}

	if res.GetMemoryCount() != 1 {
		t.Fatalf("expected memory_count 1 (dropped memory excluded), got %d", res.GetMemoryCount())
	}

	attached, err := s.db.GetMemoriesByEventId(context.Background(), res.GetId())
	if err != nil {
		t.Fatalf("GetMemoriesByEventId: %s", err)
	}

	if len(*attached) != 1 {
		t.Errorf("expected 1 memory actually attached, got %d", len(*attached))
	}
}

// TestStoreEvent_InvalidRejected verifies a validation failure surfaces as an error and stores
// nothing (an event with no name fails Validate).
func TestStoreEvent_InvalidRejected(t *testing.T) {
	s := newEventTestServer(t)

	if _, err := s.StoreEvent(context.Background(), &contract.Event{Name: "", TimeStart: 100, Significance: 5}); err == nil {
		t.Fatal("expected a validation error for an event with no name")
	}

	if s.db.CountEvents(context.Background()) != 0 {
		t.Error("expected no event stored after a validation failure")
	}
}

// TestGetEvents_BatchesMemoriesCorrectly verifies the N+1 fix: GetEvents with memories
// requested attaches each event's own memories (fetched in one batched query) and never
// cross-attaches, and a loose memory is left off entirely.
func TestGetEvents_BatchesMemoriesCorrectly(t *testing.T) {
	s := newEventTestServer(t)

	for _, e := range []types.Event{
		{Id: "e1", Name: "one", TimeStart: 100, Significance: 5},
		{Id: "e2", Name: "two", TimeStart: 100, Significance: 5},
	} {
		if _, err := s.db.CreateEvent(context.Background(), e); err != nil {
			t.Fatalf("CreateEvent(%s): %s", e.Id, err)
		}
	}

	for _, m := range []types.Memory{
		{Id: "m1", TimeStamp: 100, Significance: 5, EventId: "e1", Body: "a"},
		{Id: "m2", TimeStamp: 100, Significance: 5, EventId: "e1", Body: "b"},
		{Id: "m3", TimeStamp: 100, Significance: 5, EventId: "e2", Body: "c"},
		{Id: "loose", TimeStamp: 100, Significance: 5, Body: "d"},
	} {
		if _, err := s.db.CreateMemory(context.Background(), m); err != nil {
			t.Fatalf("CreateMemory(%s): %s", m.Id, err)
		}
	}

	res, err := s.GetEvents(context.Background(), &contract.GetEventsRequest{Memories: true})
	if err != nil {
		t.Fatalf("GetEvents: %s", err)
	}

	counts := map[string]int{}

	for _, e := range res.GetEvents() {
		counts[e.GetId()] = len(e.GetMemories())

		for _, m := range e.GetMemories() {
			if m.GetEventId() != e.GetId() {
				t.Errorf("memory %s (event %s) was attached to the wrong event %s", m.GetId(), m.GetEventId(), e.GetId())
			}
		}
	}

	if counts["e1"] != 2 {
		t.Errorf("expected e1 to carry its 2 memories, got %d", counts["e1"])
	}

	if counts["e2"] != 1 {
		t.Errorf("expected e2 to carry its 1 memory, got %d", counts["e2"])
	}
}

// TestGetEvents_SignificanceExtremum verifies the RPC passes SignificanceExtremum through to the
// db filter and returns every event tied at the highest significance, not just one.
func TestGetEvents_SignificanceExtremum(t *testing.T) {
	s := newEventTestServer(t)

	for _, e := range []types.Event{
		{Id: "e1", Name: "one", TimeStart: 100, Significance: 3},
		{Id: "e2", Name: "two", TimeStart: 200, Significance: 8},
		{Id: "e3", Name: "three", TimeStart: 300, Significance: 8},
	} {
		if _, err := s.db.CreateEvent(context.Background(), e); err != nil {
			t.Fatalf("CreateEvent(%s): %s", e.Id, err)
		}
	}

	res, err := s.GetEvents(context.Background(), &contract.GetEventsRequest{
		SignificanceExtremum: contract.SignificanceExtremum_SIGNIFICANCE_EXTREMUM_HIGHEST,
	})
	if err != nil {
		t.Fatalf("GetEvents: %s", err)
	}

	if len(res.GetEvents()) != 2 {
		t.Fatalf("expected 2 events tied at the highest significance, got %d", len(res.GetEvents()))
	}

	for _, e := range res.GetEvents() {
		if e.GetSignificance() != 8 {
			t.Errorf("expected significance 8, got %d for %s", e.GetSignificance(), e.GetId())
		}
	}
}

// TestGetEvents_SignificanceExtremum_RejectsCombinationWithRange verifies significance_extremum
// and significance_min/significance_max are mutually exclusive, per the overload the field was
// deliberately kept separate to avoid.
func TestGetEvents_SignificanceExtremum_RejectsCombinationWithRange(t *testing.T) {
	s := newEventTestServer(t)

	if _, err := s.GetEvents(context.Background(), &contract.GetEventsRequest{
		SignificanceExtremum: contract.SignificanceExtremum_SIGNIFICANCE_EXTREMUM_HIGHEST,
		SignificanceMin:      1,
	}); err == nil {
		t.Fatal("expected an error combining SignificanceExtremum with SignificanceMin")
	}

	if _, err := s.GetEvents(context.Background(), &contract.GetEventsRequest{
		SignificanceExtremum: contract.SignificanceExtremum_SIGNIFICANCE_EXTREMUM_LOWEST,
		SignificanceMax:      5,
	}); err == nil {
		t.Fatal("expected an error combining SignificanceExtremum with SignificanceMax")
	}
}

// TestStoreEvent_InsignificantRejected verifies the "quietly forgotten" contract for
// events: an event below the minimum significance returns no error, no id, stores none of its
// nested memories, and sets rejected.
func TestStoreEvent_InsignificantRejected(t *testing.T) {
	s := newEventTestServer(t)
	s.minimumEventSignificance = 10

	res, err := s.StoreEvent(context.Background(), &contract.Event{
		Name:         "minor",
		TimeStart:    100,
		Significance: 1,
		Memories:     []*contract.Memory{{Significance: 50, Body: "would-be-kept"}},
	})
	if err != nil {
		t.Fatalf("StoreEvent: %s", err)
	}

	if !res.GetRejected() {
		t.Error("expected rejected=true for an event below the minimum significance")
	}

	if res.GetId() != "" {
		t.Errorf("expected no id for a rejected event, got %q", res.GetId())
	}

	if res.GetMemoryCount() != 0 {
		t.Errorf("expected no nested memories stored for a rejected event, got %d", res.GetMemoryCount())
	}

	if s.db.CountEvents(context.Background()) != 0 {
		t.Error("expected no event stored")
	}

	if with, without := s.db.CountMemories(context.Background()); with+without != 0 {
		t.Errorf("expected no memories stored, got %d", with+without)
	}
}

// TestStoreEvent_SignificantNotRejected verifies a stored event reports rejected=false with an id.
func TestStoreEvent_SignificantNotRejected(t *testing.T) {
	s := newEventTestServer(t)
	s.minimumEventSignificance = 10

	res, err := s.StoreEvent(context.Background(), &contract.Event{Name: "major", TimeStart: 100, Significance: 20})
	if err != nil {
		t.Fatalf("StoreEvent: %s", err)
	}

	if res.GetRejected() {
		t.Error("expected rejected=false for an event above the minimum significance")
	}

	if res.GetId() == "" {
		t.Error("expected an id for a stored event")
	}
}

// TestStoreEvent_DefaultsTimeStart verifies that StoreEvent accepts a zero time_start,
// defaulting it to now (SetDefaults runs before Validate), rather than rejecting it as invalid.
func TestStoreEvent_DefaultsTimeStart(t *testing.T) {
	s := newEventTestServer(t)

	before := time.Now().UnixNano()

	res, err := s.StoreEvent(context.Background(), &contract.Event{Name: "no start", Significance: 5})
	if err != nil {
		t.Fatalf("StoreEvent with a zero time_start should default it, got: %s", err)
	}

	if res.GetId() == "" {
		t.Fatal("expected an id for a stored event")
	}

	got, err := s.db.GetEvent(context.Background(), res.GetId())
	if err != nil {
		t.Fatalf("GetEvent: %s", err)
	}

	if got.TimeStart < before {
		t.Errorf("expected time_start defaulted to ~now (>= %d), got %d", before, got.TimeStart)
	}
}

// TestStoreEvent_PlacementAboveNumericAnchor exercises resolveEventSignificance's placement path
// (AnchorEvent) end to end via a numeric anchor: storing an event "above" an existing significance
// opens a gap and lands it between the neighbours, mirroring the memory placement behaviour but
// through the events table join.
func TestStoreEvent_PlacementAboveNumericAnchor(t *testing.T) {
	s := newEventTestServer(t)
	ctx := context.Background()

	if _, err := s.StoreEvent(ctx, &contract.Event{Id: "five", Name: "five", TimeStart: 100, Significance: 5}); err != nil {
		t.Fatalf("store five: %s", err)
	}

	if _, err := s.StoreEvent(ctx, &contract.Event{Id: "six", Name: "six", TimeStart: 100, Significance: 6}); err != nil {
		t.Fatalf("store six: %s", err)
	}

	res, err := s.StoreEvent(ctx, &contract.Event{
		Id:        "between",
		Name:      "between",
		TimeStart: 100,
		Placement: &contract.SignificancePlacement{
			Mode:   contract.SignificancePlacement_ABOVE,
			Anchor: 5,
		},
	})
	if err != nil {
		t.Fatalf("store between: %s", err)
	}

	if res.GetId() != "between" {
		t.Fatalf("expected id 'between', got %q", res.GetId())
	}

	between, err := s.db.GetEvent(ctx, "between")
	if err != nil {
		t.Fatalf("GetEvent(between): %s", err)
	}

	if between.Significance != 6 {
		t.Errorf("between significance = %d, want 6", between.Significance)
	}

	six, err := s.db.GetEvent(ctx, "six")
	if err != nil {
		t.Fatalf("GetEvent(six): %s", err)
	}

	if six.Significance != 7 {
		t.Errorf("six significance = %d, want 7 (shifted up)", six.Significance)
	}

	five, err := s.db.GetEvent(ctx, "five")
	if err != nil {
		t.Fatalf("GetEvent(five): %s", err)
	}

	if five.Significance != 5 {
		t.Errorf("five significance = %d, want 5", five.Significance)
	}
}

// TestStoreEvent_PlacementIdAnchor verifies an id-based anchor resolves against the anchor event's
// own current rank rather than a literal value.
func TestStoreEvent_PlacementIdAnchor(t *testing.T) {
	s := newEventTestServer(t)
	ctx := context.Background()

	if _, err := s.StoreEvent(ctx, &contract.Event{Id: "anchor", Name: "anchor", TimeStart: 100, Significance: 10}); err != nil {
		t.Fatalf("store anchor: %s", err)
	}

	res, err := s.StoreEvent(ctx, &contract.Event{
		Id:        "above",
		Name:      "above",
		TimeStart: 100,
		Placement: &contract.SignificancePlacement{
			Mode:     contract.SignificancePlacement_ABOVE,
			AnchorId: "anchor",
		},
	})
	if err != nil {
		t.Fatalf("store above: %s", err)
	}

	got, err := s.db.GetEvent(ctx, res.GetId())
	if err != nil {
		t.Fatalf("GetEvent: %s", err)
	}

	if got.Significance != 11 {
		t.Errorf("above significance = %d, want 11", got.Significance)
	}
}

// TestStoreEvent_PlacementUnknownIdAnchorRejected confirms a placement naming a missing event
// anchor is a client error (InvalidArgument) that creates nothing, matching the memory RPC's
// behaviour for the same case but exercising the events-table anchor lookup.
func TestStoreEvent_PlacementUnknownIdAnchorRejected(t *testing.T) {
	s := newEventTestServer(t)
	ctx := context.Background()

	_, err := s.StoreEvent(ctx, &contract.Event{
		Id:        "x",
		Name:      "x",
		TimeStart: 100,
		Placement: &contract.SignificancePlacement{
			Mode:     contract.SignificancePlacement_ABOVE,
			AnchorId: "does-not-exist",
		},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument, got %s (%v)", status.Code(err), err)
	}

	if s.db.CountEvents(ctx) != 0 {
		t.Error("expected no event stored")
	}
}

// TestUpdateEventSignificance_PlacementAbove exercises resolveEventSignificance's placement path
// through UpdateEventSignificance rather than StoreEvent.
func TestUpdateEventSignificance_PlacementAbove(t *testing.T) {
	s := newEventTestServer(t)
	ctx := context.Background()

	if _, err := s.db.CreateEvent(ctx, types.Event{Id: "anchor", Name: "anchor", TimeStart: 100, Significance: 10}); err != nil {
		t.Fatalf("CreateEvent(anchor): %s", err)
	}

	if _, err := s.db.CreateEvent(ctx, types.Event{Id: "e1", Name: "e1", TimeStart: 100, Significance: 1}); err != nil {
		t.Fatalf("CreateEvent(e1): %s", err)
	}

	res, err := s.UpdateEventSignificance(ctx, &contract.UpdateEventSignificanceRequest{
		Id: "e1",
		Placement: &contract.SignificancePlacement{
			Mode:     contract.SignificancePlacement_ABOVE,
			AnchorId: "anchor",
		},
	})
	if err != nil {
		t.Fatalf("UpdateEventSignificance: %s", err)
	}

	if !res.GetOk() {
		t.Error("UpdateEventSignificance did not report Ok")
	}

	got, err := s.db.GetEvent(ctx, "e1")
	if err != nil {
		t.Fatalf("GetEvent: %s", err)
	}

	if got.Significance != 11 {
		t.Errorf("e1 significance = %d, want 11", got.Significance)
	}
}

// TestUpdateEventSignificance_PlacementInvalidBetweenRejected confirms an inverted BETWEEN range
// (upper <= lower) surfaces as InvalidArgument rather than an internal error, and leaves the event
// unchanged.
func TestUpdateEventSignificance_PlacementInvalidBetweenRejected(t *testing.T) {
	s := newEventTestServer(t)
	ctx := context.Background()

	if _, err := s.db.CreateEvent(ctx, types.Event{Id: "e1", Name: "e1", TimeStart: 100, Significance: 5}); err != nil {
		t.Fatalf("CreateEvent: %s", err)
	}

	_, err := s.UpdateEventSignificance(ctx, &contract.UpdateEventSignificanceRequest{
		Id: "e1",
		Placement: &contract.SignificancePlacement{
			Mode:   contract.SignificancePlacement_BETWEEN,
			Anchor: 10,
			Upper:  5,
		},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument, got %s (%v)", status.Code(err), err)
	}

	got, err := s.db.GetEvent(ctx, "e1")
	if err != nil {
		t.Fatalf("GetEvent: %s", err)
	}

	if got.Significance != 5 {
		t.Errorf("e1 significance changed to %d, want unchanged 5", got.Significance)
	}
}
