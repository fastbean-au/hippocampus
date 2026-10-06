package hippocampus

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/fastbean-au/hippocampus/contract"
	"github.com/fastbean-au/hippocampus/types"
)

// DeleteEventRequest.if_empty (TODO-3 item 165): what a caller that has already deleted the memories
// it judged asks for, so a memory that arrived since is never deleted unjudged - the ingestor's drain
// compared a count and then deleted with memories: true, and a memory landing in between went with it.

func seedEventWithMemory(t *testing.T, s *Server) {
	t.Helper()

	ctx := context.Background()

	if _, err := s.db.CreateEvent(ctx, types.Event{Id: "e1", Name: "one", TimeStart: 100, Significance: 5, Group: "a"}); err != nil {
		t.Fatalf("CreateEvent: %s", err)
	}

	if _, err := s.db.CreateMemory(ctx, types.Memory{Id: "late", Body: "x", TimeStamp: 100, Significance: 5, EventId: "e1", Group: "b"}); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}
}

func TestDeleteEvent_IfEmptyRefusesAnEventHoldingAMemory(t *testing.T) {
	s := newTestServer(t)
	seedEventWithMemory(t, s)

	_, err := s.DeleteEvent(context.Background(), &contract.DeleteEventRequest{Id: "e1", IfEmpty: true})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("DeleteEvent(if_empty) on an event holding a memory = %v, want FailedPrecondition", err)
	}

	stored, err := s.db.GetMemoriesByIds(context.Background(), []string{"late"})
	if err != nil || len(*stored) != 1 || (*stored)[0].EventId != "e1" {
		t.Errorf("the refused delete changed the memory: %+v, %v", stored, err)
	}
}

func TestDeleteEvent_IfEmptyDeletesAnEmptyEvent(t *testing.T) {
	s := newTestServer(t)
	seedEventWithMemory(t, s)

	if _, err := s.db.DeleteMemories(context.Background(), []string{"late"}); err != nil {
		t.Fatalf("DeleteMemories: %s", err)
	}

	res, err := s.DeleteEvent(context.Background(), &contract.DeleteEventRequest{Id: "e1", IfEmpty: true})
	if err != nil || !res.GetOk() {
		t.Fatalf("DeleteEvent(if_empty) on an empty event = %v, %v", res, err)
	}
}

// TestDeleteEvent_IfEmptyIsJudgedWithinTheCallersScope: another group's memory neither blocks the
// delete nor is revealed by a refusal; it is detached, as any delete of the event detaches it.
func TestDeleteEvent_IfEmptyIsJudgedWithinTheCallersScope(t *testing.T) {
	s := newTestServer(t)
	seedEventWithMemory(t, s)

	res, err := s.DeleteEvent(scopedContext("a"), &contract.DeleteEventRequest{Id: "e1", IfEmpty: true})
	if err != nil || !res.GetOk() {
		t.Fatalf("a scoped if_empty delete blocked by another group's memory: %v, %v", res, err)
	}

	stored, err := s.db.GetMemoriesByIds(context.Background(), []string{"late"})
	if err != nil || len(*stored) != 1 || (*stored)[0].EventId != "" {
		t.Errorf("the other group's memory should survive, detached: %+v, %v", stored, err)
	}
}

func TestDeleteEvent_IfEmptyWithMemoriesIsRefused(t *testing.T) {
	s := newTestServer(t)
	seedEventWithMemory(t, s)

	_, err := s.DeleteEvent(context.Background(), &contract.DeleteEventRequest{Id: "e1", IfEmpty: true, Memories: true})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("if_empty with memories = %v, want InvalidArgument: one asks for nothing to be deleted, the other for everything", err)
	}
}
