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

	if n := s.db.CountEvents(); n != 0 {
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

	if n := s.db.CountEvents(); n != 0 {
		t.Fatalf("EndEvent with an unknown id created %d event(s); expected none", n)
	}
}

// TestEndEvent_Success confirms the happy path still ends an existing event and reports Ok.
func TestEndEvent_Success(t *testing.T) {
	s := newEventTestServer(t)

	if _, err := s.db.CreateEvent(types.Event{Id: "e1", Name: "one", TimeStart: 100, Significance: 5}); err != nil {
		t.Fatalf("CreateEvent: %s", err)
	}

	res, err := s.EndEvent(context.Background(), &contract.EndEventRequest{Id: "e1", TimeEnd: 900})
	if err != nil {
		t.Fatalf("EndEvent: %s", err)
	}

	if !res.GetOk() {
		t.Error("EndEvent did not report Ok for a successful end")
	}

	got, err := s.db.GetEvent("e1")
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

	if n := s.db.CountEvents(); n != 0 {
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

	if n := s.db.CountEvents(); n != 0 {
		t.Fatalf("UpdateEventSignificance with an unknown id created %d event(s); expected none", n)
	}
}

// TestUpdateEventSignificance_Success confirms the happy path updates the significance and reports
// Ok.
func TestUpdateEventSignificance_Success(t *testing.T) {
	s := newEventTestServer(t)

	if _, err := s.db.CreateEvent(types.Event{Id: "e1", Name: "one", TimeStart: 100, Significance: 5}); err != nil {
		t.Fatalf("CreateEvent: %s", err)
	}

	res, err := s.UpdateEventSignificance(context.Background(), &contract.UpdateEventSignificanceRequest{Id: "e1", Significance: 42})
	if err != nil {
		t.Fatalf("UpdateEventSignificance: %s", err)
	}

	if !res.GetOk() {
		t.Error("UpdateEventSignificance did not report Ok for a successful update")
	}

	got, err := s.db.GetEvent("e1")
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

func (f failUnsetStore) UnsetMemoriesEventId(eventId string) (int, error) {
	return 0, f.err
}

// TestDeleteEvent_DetachSuccess is a regression test: a successful DeleteEvent
// must report Ok (every other GeneralResponse RPC does), and the detach arm (memories: false) must
// leave the event's memories in place with their event_id cleared rather than deleting them.
func TestDeleteEvent_DetachSuccess(t *testing.T) {
	s := newEventTestServer(t)

	if _, err := s.db.CreateEvent(types.Event{Id: "e1", Name: "one", TimeStart: 100, Significance: 5}); err != nil {
		t.Fatalf("CreateEvent: %s", err)
	}

	if _, err := s.db.CreateMemory(types.Memory{Id: "m1", TimeStamp: 100, Significance: 5, EventId: "e1", Body: "x"}); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	res, err := s.DeleteEvent(context.Background(), &contract.DeleteEventRequest{Id: "e1", Memories: false})
	if err != nil {
		t.Fatalf("DeleteEvent: %s", err)
	}

	if !res.GetOk() {
		t.Error("DeleteEvent did not report Ok for a successful detach")
	}

	if _, err := s.db.GetEvent("e1"); err == nil {
		t.Error("expected event e1 to be deleted")
	}

	with, without := s.db.CountMemories()
	if with != 0 || without != 1 {
		t.Errorf("expected the memory detached (0 with event, 1 without), got %d with / %d without", with, without)
	}
}

// TestDeleteEvent_WithMemoriesSuccess verifies the memories: true arm deletes the event's memories
// and still reports Ok.
func TestDeleteEvent_WithMemoriesSuccess(t *testing.T) {
	s := newEventTestServer(t)

	if _, err := s.db.CreateEvent(types.Event{Id: "e1", Name: "one", TimeStart: 100, Significance: 5}); err != nil {
		t.Fatalf("CreateEvent: %s", err)
	}

	if _, err := s.db.CreateMemory(types.Memory{Id: "m1", TimeStamp: 100, Significance: 5, EventId: "e1", Body: "x"}); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	res, err := s.DeleteEvent(context.Background(), &contract.DeleteEventRequest{Id: "e1", Memories: true})
	if err != nil {
		t.Fatalf("DeleteEvent: %s", err)
	}

	if !res.GetOk() {
		t.Error("DeleteEvent did not report Ok for a successful delete-with-memories")
	}

	if with, without := s.db.CountMemories(); with != 0 || without != 0 {
		t.Errorf("expected the memory deleted, got %d with / %d without", with, without)
	}
}

// TestDeleteEvent_UnsetErrorPropagates verifies that a failure clearing the memories' event_id in
// the detach arm surfaces to the caller instead of being swallowed as a nil error: the
// event is gone but the memories still point at it, which the client must be told about.
func TestDeleteEvent_UnsetErrorPropagates(t *testing.T) {
	s := newEventTestServer(t)

	if _, err := s.db.CreateEvent(types.Event{Id: "e1", Name: "one", TimeStart: 100, Significance: 5}); err != nil {
		t.Fatalf("CreateEvent: %s", err)
	}

	wantErr := errors.New("unset failed")
	s.db = failUnsetStore{Store: s.db, err: wantErr}

	res, err := s.DeleteEvent(context.Background(), &contract.DeleteEventRequest{Id: "e1", Memories: false})
	if err == nil {
		t.Fatal("DeleteEvent swallowed the UnsetMemoriesEventId failure; expected an error")
	}

	if !errors.Is(err, wantErr) {
		t.Errorf("expected the UnsetMemoriesEventId error to propagate, got %v", err)
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
	if _, err := s.db.CreateMemory(types.Memory{Id: "m1", TimeStamp: 100, Significance: 5, Body: "keep me"}); err != nil {
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

	if with, without := s.db.CountMemories(); with != 0 || without != 1 {
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

	if _, err := s.db.CreateEvent(types.Event{Id: "from", Name: "source", TimeStart: 100, Significance: 5}); err != nil {
		t.Fatalf("CreateEvent: %s", err)
	}

	if _, err := s.db.CreateMemory(types.Memory{Id: "m1", TimeStamp: 100, Significance: 5, EventId: "from", Body: "x"}); err != nil {
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
	got, err := s.db.GetMemoriesByIds([]string{"m1"})
	if err != nil {
		t.Fatalf("GetMemoriesByIds: %s", err)
	}

	if (*got)[0].EventId != "from" {
		t.Errorf("expected m1 still attached to 'from', got %q", (*got)[0].EventId)
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
		if _, err := s.db.CreateEvent(e); err != nil {
			t.Fatalf("CreateEvent(%s): %s", e.Id, err)
		}
	}

	for _, m := range []types.Memory{
		{Id: "m1", TimeStamp: 100, Significance: 5, EventId: "e1", Body: "a"},
		{Id: "m2", TimeStamp: 100, Significance: 5, EventId: "e1", Body: "b"},
		{Id: "m3", TimeStamp: 100, Significance: 5, EventId: "e2", Body: "c"},
		{Id: "loose", TimeStamp: 100, Significance: 5, Body: "d"},
	} {
		if _, err := s.db.CreateMemory(m); err != nil {
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

	if s.db.CountEvents() != 0 {
		t.Error("expected no event stored")
	}

	if with, without := s.db.CountMemories(); with+without != 0 {
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

	got, err := s.db.GetEvent(res.GetId())
	if err != nil {
		t.Fatalf("GetEvent: %s", err)
	}

	if got.TimeStart < before {
		t.Errorf("expected time_start defaulted to ~now (>= %d), got %d", before, got.TimeStart)
	}
}
