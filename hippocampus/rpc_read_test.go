package hippocampus

import (
	"context"
	"testing"

	"github.com/fastbean-au/hippocampus/contract"
	"github.com/fastbean-au/hippocampus/types"
)

// TestGetEventById_RPC covers the read handler: fetching an event, optionally with its memories,
// and surfacing a not-found error for an unknown id.
func TestGetEventById_RPC(t *testing.T) {
	s := newTestServer(t)

	if _, err := s.db.CreateEvent(context.Background(), types.Event{Id: "e1", Name: "trip", TimeStart: 100, Significance: 3}); err != nil {
		t.Fatalf("CreateEvent: %s", err)
	}

	for _, id := range []string{"m1", "m2"} {
		if _, err := s.db.CreateMemory(context.Background(), types.Memory{Id: id, TimeStamp: 100, Significance: 1, EventId: "e1", Body: "detail"}); err != nil {
			t.Fatalf("CreateMemory(%s): %s", id, err)
		}
	}

	// Without memories requested, the event comes back bare.
	res, err := s.GetEventById(context.Background(), &contract.GetEventByIdRequest{Id: "e1"})
	if err != nil {
		t.Fatalf("GetEventById: %s", err)
	}

	if res.GetEvent().GetName() != "trip" {
		t.Errorf("expected event 'trip', got %q", res.GetEvent().GetName())
	}

	if len(res.GetEvent().GetMemories()) != 0 {
		t.Errorf("expected no memories when not requested, got %d", len(res.GetEvent().GetMemories()))
	}

	// With memories requested, both attach.
	withMems, err := s.GetEventById(context.Background(), &contract.GetEventByIdRequest{Id: "e1", Memories: true})
	if err != nil {
		t.Fatalf("GetEventById(memories): %s", err)
	}

	if len(withMems.GetEvent().GetMemories()) != 2 {
		t.Errorf("expected 2 memories attached, got %d", len(withMems.GetEvent().GetMemories()))
	}

	// An unknown id surfaces an error.
	if _, err := s.GetEventById(context.Background(), &contract.GetEventByIdRequest{Id: "missing"}); err == nil {
		t.Error("expected an error for an unknown event id")
	}
}

// TestRecallMemories_RPC covers the recall handler: an empty id list is a no-op, and recalling a
// memory returns it while reinforcing the stored recall count.
func TestRecallMemories_RPC(t *testing.T) {
	s := newTestServer(t)

	if _, err := s.db.CreateMemory(context.Background(), types.Memory{Id: "m1", TimeStamp: 100, Significance: 5, Body: "recall me"}); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	// Empty id list short-circuits to an empty response.
	empty, err := s.RecallMemories(context.Background(), &contract.RecallMemoriesRequest{})
	if err != nil {
		t.Fatalf("RecallMemories(empty): %s", err)
	}

	if len(empty.GetMemories()) != 0 {
		t.Errorf("expected no memories for an empty recall, got %d", len(empty.GetMemories()))
	}

	res, err := s.RecallMemories(context.Background(), &contract.RecallMemoriesRequest{Ids: []string{"m1"}})
	if err != nil {
		t.Fatalf("RecallMemories: %s", err)
	}

	if len(res.GetMemories()) != 1 || res.GetMemories()[0].GetId() != "m1" {
		t.Fatalf("expected m1 returned, got %+v", res.GetMemories())
	}

	if res.GetMemories()[0].GetRecallCount() != 1 {
		t.Errorf("expected recall count 1 after a recall, got %d", res.GetMemories()[0].GetRecallCount())
	}
}

// TestGetMemories_RPC covers the list handler: filtering, the total count, and request validation.
func TestGetMemories_RPC(t *testing.T) {
	s := newTestServer(t)

	memories := []types.Memory{
		{Id: "m1", TimeStamp: 100, Significance: 1, Body: "a"},
		{Id: "m2", TimeStamp: 200, Significance: 5, Body: "b"},
		{Id: "m3", TimeStamp: 300, Significance: 9, Body: "c"},
	}

	for _, m := range memories {
		if _, err := s.db.CreateMemory(context.Background(), m); err != nil {
			t.Fatalf("CreateMemory(%s): %s", m.Id, err)
		}
	}

	// significance >= 5 matches m2 and m3.
	res, err := s.GetMemories(context.Background(), &contract.GetMemoriesRequest{SignificanceMin: 5})
	if err != nil {
		t.Fatalf("GetMemories: %s", err)
	}

	if res.GetTotalCount() != 2 || len(res.GetMemories()) != 2 {
		t.Errorf("expected 2 memories with significance >= 5, got total=%d len=%d", res.GetTotalCount(), len(res.GetMemories()))
	}

	// An inverted significance range is rejected.
	if _, err := s.GetMemories(context.Background(), &contract.GetMemoriesRequest{SignificanceMin: 9, SignificanceMax: 1}); err == nil {
		t.Error("expected an error for SignificanceMax < SignificanceMin")
	}

	// An inverted timestamp range is rejected.
	if _, err := s.GetMemories(context.Background(), &contract.GetMemoriesRequest{TimestampMin: 300, TimestampMax: 100}); err == nil {
		t.Error("expected an error for TimestampMax < TimestampMin")
	}

	// An unsupported order_by is rejected.
	if _, err := s.GetMemories(context.Background(), &contract.GetMemoriesRequest{OrderBy: "bogus"}); err == nil {
		t.Error("expected an error for an unsupported order_by")
	}
}
