package hippocampus

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/fastbean-au/hippocampus/contract"
	"github.com/fastbean-au/hippocampus/db"
)

// callbackQueueServer builds a Server over a recording in-memory store, with the queue reporting
// itself enabled so the response's flag can be asserted.
func callbackQueueServer(t *testing.T) (*Server, *db.DB) {
	t.Helper()

	database, err := db.New("")
	if err != nil {
		t.Fatalf("db.New: %s", err)
	}

	t.Cleanup(func() { _ = database.Close() })

	database.SetCallbackPolicy(db.CallbackPolicy{Enabled: true, MemoryEvents: true, EventEvents: true})

	return &Server{db: database, callbacksEnabled: true}, database
}

// seedQueue records n memory-forgotten deliveries plus one sleep-completed, returning nothing: the
// tests below assert on what the RPC reports rather than on the rows.
func seedQueue(t *testing.T, database *db.DB, n int) {
	t.Helper()

	ctx := context.Background()

	for i := range n {
		if err := database.QueueCallbacks(ctx, []db.CallbackDelivery{{
			Kind:      db.CallbackKindMemoryForgotten,
			Cause:     db.CauseConsolidation,
			CycleId:   int64(100 + i),
			ItemCount: 2,
			Payload:   db.CallbackPayload{Items: []db.CallbackItem{{Id: "m", Body: "a body nobody should see here"}}},
		}}); err != nil {
			t.Fatalf("QueueCallbacks: %s", err)
		}
	}

	if err := database.QueueCallbacks(ctx, []db.CallbackDelivery{{
		Kind:      db.CallbackKindSleepCompleted,
		CycleId:   999,
		Chunk:     1,
		Chunks:    1,
		ItemCount: 3,
		Payload:   db.CallbackPayload{Cycle: &db.CallbackCycle{Trigger: "timer", Success: true}},
	}}); err != nil {
		t.Fatalf("QueueCallbacks: %s", err)
	}
}

// TestGetCallbackQueueReportsTheBacklog is the happy path: the depth, the oldest instant, and a page
// of deliveries with the attempt state an operator reads to tell draining from stuck.
func TestGetCallbackQueueReportsTheBacklog(t *testing.T) {
	s, database := callbackQueueServer(t)
	seedQueue(t, database, 3)

	res, err := s.GetCallbackQueue(context.Background(), &contract.GetCallbackQueueRequest{})
	if err != nil {
		t.Fatalf("GetCallbackQueue: %s", err)
	}

	if !res.GetEnabled() {
		t.Error("a recording store reported enabled false")
	}

	if res.GetDepth() != 4 {
		t.Errorf("depth = %d, want 4", res.GetDepth())
	}

	if res.GetOldestQueuedAt() == 0 {
		t.Error("a non-empty queue reported no oldest instant")
	}

	if len(res.GetDeliveries()) != 4 {
		t.Fatalf("the page holds %d deliveries, want 4", len(res.GetDeliveries()))
	}

	first := res.GetDeliveries()[0]

	if first.GetKind() != contract.CallbackKind_CALLBACK_KIND_MEMORY_FORGOTTEN {
		t.Errorf("the first delivery's kind is %v", first.GetKind())
	}

	if first.GetCause() != contract.DeleteCause_DELETE_CAUSE_CONSOLIDATION {
		t.Errorf("the first delivery's cause is %v", first.GetCause())
	}

	if first.GetCycleId() != 100 || first.GetItemCount() != 2 || first.GetQueuedAt() == 0 {
		t.Errorf("the first delivery is %+v", first)
	}

	// A fresh delivery is due immediately and has not been tried.
	if first.GetAttempts() != 0 || first.GetNextAttemptAt() == 0 {
		t.Errorf("a fresh delivery reports attempts=%d next=%d", first.GetAttempts(), first.GetNextAttemptAt())
	}

	// The page is short of the limit, so there is no cursor: offering one would make a client fetch
	// an empty page to discover it had finished.
	if res.GetNextSeq() != 0 {
		t.Errorf("a short page offered a cursor (%d)", res.GetNextSeq())
	}

	// And the payload never travels - a queued delivery may carry memory bodies.
	if body := res.String(); len(body) > 0 && contains(body, "nobody should see") {
		t.Error("the listing carried a delivery payload")
	}
}

// contains is a tiny helper so the assertion above reads as one line.
func contains(haystack string, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}

	return false
}

// TestGetCallbackQueuePaginates covers the cursor, which is offered only on a full page.
func TestGetCallbackQueuePaginates(t *testing.T) {
	s, database := callbackQueueServer(t)
	seedQueue(t, database, 3)

	first, err := s.GetCallbackQueue(context.Background(), &contract.GetCallbackQueueRequest{Limit: 2})
	if err != nil {
		t.Fatalf("GetCallbackQueue: %s", err)
	}

	if len(first.GetDeliveries()) != 2 || first.GetNextSeq() == 0 {
		t.Fatalf("a full page returned %d deliveries and cursor %d", len(first.GetDeliveries()), first.GetNextSeq())
	}

	second, err := s.GetCallbackQueue(context.Background(), &contract.GetCallbackQueueRequest{
		Limit:    2,
		AfterSeq: first.GetNextSeq(),
	})
	if err != nil {
		t.Fatalf("GetCallbackQueue (page 2): %s", err)
	}

	if len(second.GetDeliveries()) != 2 {
		t.Fatalf("the second page holds %d", len(second.GetDeliveries()))
	}

	if second.GetDeliveries()[0].GetSeq() <= first.GetDeliveries()[1].GetSeq() {
		t.Error("the second page overlapped the first")
	}

	// Depth is the whole queue, not the page: a client showing "2 of 4" needs both numbers.
	if second.GetDepth() != 4 {
		t.Errorf("depth on page two is %d, want the whole queue's 4", second.GetDepth())
	}
}

// TestGetCallbackQueueFiltersByKind covers the filter and the wire-to-storage kind mapping behind it.
func TestGetCallbackQueueFiltersByKind(t *testing.T) {
	s, database := callbackQueueServer(t)
	seedQueue(t, database, 3)

	res, err := s.GetCallbackQueue(context.Background(), &contract.GetCallbackQueueRequest{
		Kind: contract.CallbackKind_CALLBACK_KIND_SLEEP_COMPLETED,
	})
	if err != nil {
		t.Fatalf("GetCallbackQueue: %s", err)
	}

	if len(res.GetDeliveries()) != 1 {
		t.Fatalf("the sleep filter returned %d deliveries, want 1", len(res.GetDeliveries()))
	}

	only := res.GetDeliveries()[0]

	if only.GetKind() != contract.CallbackKind_CALLBACK_KIND_SLEEP_COMPLETED || only.GetCycleId() != 999 {
		t.Errorf("the filtered delivery is %+v", only)
	}

	if only.GetChunk() != 1 || only.GetChunks() != 1 {
		t.Errorf("the chunk numbering did not survive the projection: %+v", only)
	}

	// Depth stays the whole queue even under a filter, which is what stops a filtered view reading
	// as an empty backlog.
	if res.GetDepth() != 4 {
		t.Errorf("depth under a filter is %d, want 4", res.GetDepth())
	}
}

// TestGetCallbackQueueOnAStoreThatIsNotRecording pins the ambiguity the enabled flag resolves: an
// empty queue means everything was delivered, or nothing is being queued at all.
func TestGetCallbackQueueOnAStoreThatIsNotRecording(t *testing.T) {
	s, _ := callbackQueueServer(t)
	s.callbacksEnabled = false

	res, err := s.GetCallbackQueue(context.Background(), &contract.GetCallbackQueueRequest{})
	if err != nil {
		t.Fatalf("GetCallbackQueue: %s", err)
	}

	if res.GetEnabled() {
		t.Error("a store that is not recording reported enabled true")
	}

	if len(res.GetDeliveries()) != 0 || res.GetDepth() != 0 {
		t.Errorf("an empty queue reported %d deliveries at depth %d", len(res.GetDeliveries()), res.GetDepth())
	}
}

// TestGetCallbackQueueSurfacesStoreFailures covers the three reads, each of which must fail the RPC
// rather than report a half-answer an operator would act on.
func TestGetCallbackQueueSurfacesStoreFailures(t *testing.T) {
	cases := map[string]failingCallbackStore{
		"listing": {failList: true},
		"depth":   {failDepth: true},
		"oldest":  {failOldest: true},
	}

	for name, fake := range cases {
		t.Run(name, func(t *testing.T) {
			s, database := callbackQueueServer(t)
			seedQueue(t, database, 1)

			fake.Store = database
			s.db = fake

			if _, err := s.GetCallbackQueue(context.Background(), &contract.GetCallbackQueueRequest{}); err == nil {
				t.Errorf("a failed %s read returned a response", name)
			}
		})
	}
}

// TestDeleteCallbackQueueRequiresABound is the refusal, and the sharper sibling of the forgotten
// log's: what this discards is not the record of a notification but the notification itself.
func TestDeleteCallbackQueueRequiresABound(t *testing.T) {
	s, database := callbackQueueServer(t)
	seedQueue(t, database, 2)

	_, err := s.DeleteCallbackQueue(context.Background(), &contract.DeleteCallbackQueueRequest{})

	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("a bare request = %v, want InvalidArgument", err)
	}

	if depth, _ := database.CallbackQueueDepth(context.Background()); depth != 3 {
		t.Errorf("the refused request still removed deliveries (%d left of 3)", depth)
	}
}

func TestDeleteCallbackQueueAll(t *testing.T) {
	s, database := callbackQueueServer(t)
	seedQueue(t, database, 2)

	res, err := s.DeleteCallbackQueue(context.Background(), &contract.DeleteCallbackQueueRequest{All: true})
	if err != nil {
		t.Fatalf("DeleteCallbackQueue: %s", err)
	}

	if res.GetDeleted() != 3 {
		t.Errorf("deleted = %d, want 3", res.GetDeleted())
	}

	if depth, _ := database.CallbackQueueDepth(context.Background()); depth != 0 {
		t.Errorf("%d deliveries survived", depth)
	}
}

func TestDeleteCallbackQueueBeforeATime(t *testing.T) {
	s, database := callbackQueueServer(t)
	ctx := context.Background()

	cutoff := time.Now().UnixNano()

	for _, at := range []int64{cutoff - int64(time.Hour), cutoff + int64(time.Hour)} {
		if err := database.QueueCallbacks(ctx, []db.CallbackDelivery{{
			Kind:     db.CallbackKindMemoryForgotten,
			Cause:    db.CauseConsolidation,
			QueuedAt: at,
			Payload:  db.CallbackPayload{Items: []db.CallbackItem{{Id: "m"}}},
		}}); err != nil {
			t.Fatalf("QueueCallbacks: %s", err)
		}
	}

	res, err := s.DeleteCallbackQueue(ctx, &contract.DeleteCallbackQueueRequest{BeforeTime: cutoff})
	if err != nil {
		t.Fatalf("DeleteCallbackQueue: %s", err)
	}

	if res.GetDeleted() != 1 {
		t.Errorf("deleted = %d, want just the older delivery", res.GetDeleted())
	}

	if depth, _ := database.CallbackQueueDepth(ctx); depth != 1 {
		t.Errorf("%d deliveries left, want 1", depth)
	}
}

func TestDeleteCallbackQueueSurfacesTheStoreFailure(t *testing.T) {
	s, database := callbackQueueServer(t)

	s.db = failingCallbackStore{Store: database, failDelete: true}

	if _, err := s.DeleteCallbackQueue(context.Background(), &contract.DeleteCallbackQueueRequest{All: true}); err == nil {
		t.Error("a failed delete returned a response")
	}
}

// TestCallbackKindOf covers the wire-to-storage filter mapping, where UNSPECIFIED means "every kind"
// rather than "no kind" - the inverse of what the same zero value means on a stored row.
func TestCallbackKindOf(t *testing.T) {
	cases := map[contract.CallbackKind]db.CallbackKind{
		contract.CallbackKind_CALLBACK_KIND_UNSPECIFIED:      db.CallbackKindNone,
		contract.CallbackKind_CALLBACK_KIND_MEMORY_FORGOTTEN: db.CallbackKindMemoryForgotten,
		contract.CallbackKind_CALLBACK_KIND_EVENT_FORGOTTEN:  db.CallbackKindEventForgotten,
		contract.CallbackKind_CALLBACK_KIND_SLEEP_COMPLETED:  db.CallbackKindSleepCompleted,
		contract.CallbackKind(99):                            db.CallbackKindNone,
	}

	for in, want := range cases {
		if got := callbackKindOf(in); got != want {
			t.Errorf("callbackKindOf(%v) = %d, want %d", in, got, want)
		}
	}
}
