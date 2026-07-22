package hippocampus

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/fastbean-au/hippocampus/contract"
	"github.com/fastbean-au/hippocampus/db"
	"github.com/fastbean-au/hippocampus/types"
)

// conflictStore wraps a real db.Store but forces CreateMemory to fail with a wrapped
// db.ErrWriteConflict, standing in for a MySQL deadlock that survived the driver's retries, so
// StoreMemory's error mapping can be exercised without a live MySQL server.
type conflictStore struct {
	db.Store
}

func (conflictStore) CreateMemory(ctx context.Context, memory types.Memory) (string, error) {
	return "", fmt.Errorf("db write: %w", db.ErrWriteConflict)
}

// TestStoreMemory_WriteConflictMapsToAborted is a regression test: a storage-level write conflict
// (a MySQL deadlock that outlived the retries) used to surface as a gRPC Unknown, which clients read
// as a lost write. It must now map to Aborted, which clients treat as retryable.
func TestStoreMemory_WriteConflictMapsToAborted(t *testing.T) {
	s := newTestServer(t)
	s.db = conflictStore{Store: s.db}

	_, err := s.StoreMemory(context.Background(), &contract.Memory{Significance: 5, Body: "x"})
	if err == nil {
		t.Fatal("expected StoreMemory to return the write conflict")
	}

	if got := status.Code(err); got != codes.Aborted {
		t.Errorf("expected codes.Aborted, got %s (%v)", got, err)
	}
}

// TestStoreMemory_DuplicateIdMapsToAlreadyExists verifies the finding's concrete example: creating
// a memory whose id already exists used to surface the raw driver text ("UNIQUE constraint failed:
// memories.id") as a gRPC Unknown, both leaking schema detail and miscoding the error. It must now
// map to codes.AlreadyExists with a generic message.
func TestStoreMemory_DuplicateIdMapsToAlreadyExists(t *testing.T) {
	s := newTestServer(t)

	first, err := s.StoreMemory(context.Background(), &contract.Memory{Id: "dup", Significance: 5, Body: "x"})
	if err != nil {
		t.Fatalf("first StoreMemory: %s", err)
	}

	if first.GetId() != "dup" {
		t.Fatalf("expected the first create to keep id 'dup', got %q", first.GetId())
	}

	_, err = s.StoreMemory(context.Background(), &contract.Memory{Id: "dup", Significance: 5, Body: "y"})
	if err == nil {
		t.Fatal("expected a duplicate id to be rejected")
	}

	if got := status.Code(err); got != codes.AlreadyExists {
		t.Errorf("expected codes.AlreadyExists, got %s (%v)", got, err)
	}

	if msg := status.Convert(err).Message(); strings.Contains(msg, "memories") || strings.Contains(msg, "UNIQUE") {
		t.Errorf("duplicate-id error leaked schema detail: %q", msg)
	}
}

// TestDeleteMemories_DuplicateIdsReportOk is a regression test: a request repeating an id used to
// report Ok: false because the store deletes each row once (cnt < len(ids)), even though every
// requested memory was in fact deleted. Deduplicating the ids first makes the count comparison
// honest.
func TestDeleteMemories_DuplicateIdsReportOk(t *testing.T) {
	s := newTestServer(t)

	for _, id := range []string{"m1", "m2"} {
		if _, err := s.db.CreateMemory(context.Background(), types.Memory{Id: id, TimeStamp: 100, Significance: 1, Body: "x"}); err != nil {
			t.Fatalf("CreateMemory(%s): %s", id, err)
		}
	}

	res, err := s.DeleteMemories(context.Background(), &contract.DeleteMemoriesRequest{Ids: []string{"m1", "m1", "m2"}})
	if err != nil {
		t.Fatalf("DeleteMemories: %s", err)
	}

	if !res.GetOk() {
		t.Error("expected Ok: true when every requested (deduplicated) memory was deleted")
	}

	memories, err := s.db.GetMemoriesByIds(context.Background(), []string{"m1", "m2"})
	if err != nil {
		t.Fatalf("GetMemoriesByIds: %s", err)
	}

	if len(*memories) != 0 {
		t.Errorf("expected all memories deleted, %d remain", len(*memories))
	}
}

// newTestServer builds a Server over an in-memory database, ready for RPC-level tests.
func newTestServer(t *testing.T) *Server {
	t.Helper()

	database, err := db.New("")
	if err != nil {
		t.Fatalf("failed to create in-memory DB: %s", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	return &Server{db: database}
}

// TestReplaceMemoriesWithSummary_RPC verifies the happy path end to end: the event's memories are
// deleted, the summary is stored in their place flagged is_summary, and the response reports how
// many memories were replaced.
func TestReplaceMemoriesWithSummary_RPC(t *testing.T) {
	s := newTestServer(t)

	if _, err := s.db.CreateEvent(context.Background(), types.Event{Id: "e1", Name: "trip", TimeStart: 100, Significance: 1}); err != nil {
		t.Fatalf("CreateEvent: %s", err)
	}

	for _, id := range []string{"m1", "m2"} {
		if _, err := s.db.CreateMemory(context.Background(), types.Memory{Id: id, TimeStamp: 100, Significance: 1, EventId: "e1", Body: "detail"}); err != nil {
			t.Fatalf("CreateMemory(%s): %s", id, err)
		}
	}

	req := &contract.ReplaceMemoriesWithSummaryRequest{
		EventId: "e1",
		Summary: &contract.Memory{Significance: 5, Body: "the gist of the trip"},
	}

	res, err := s.ReplaceMemoriesWithSummary(context.Background(), req)
	if err != nil {
		t.Fatalf("ReplaceMemoriesWithSummary: %s", err)
	}

	if res.GetMemoriesReplaced() != 2 {
		t.Errorf("expected 2 memories replaced, got %d", res.GetMemoriesReplaced())
	}

	if res.GetId() == "" {
		t.Error("expected a generated summary id")
	}

	memories, err := s.db.GetMemoriesByEventId(context.Background(), "e1")
	if err != nil {
		t.Fatalf("GetMemoriesByEventId: %s", err)
	}

	if len(*memories) != 1 {
		t.Fatalf("expected exactly 1 surviving memory, got %d", len(*memories))
	}

	summary := (*memories)[0]
	if summary.Id != res.GetId() || summary.Body != "the gist of the trip" || !summary.IsSummary {
		t.Errorf("unexpected summary memory: %+v", summary)
	}
}

// TestReplaceMemoriesWithSummary_UnknownEvent verifies that an event id with no matching event is
// rejected, and — critically — that rejection happens before any memory is deleted.
func TestReplaceMemoriesWithSummary_UnknownEvent(t *testing.T) {
	s := newTestServer(t)

	// A memory dangling on a non-existent event id, as can happen after DeleteEvent(memories:
	// false).
	if _, err := s.db.CreateMemory(context.Background(), types.Memory{Id: "m1", TimeStamp: 100, Significance: 1, EventId: "ghost", Body: "x"}); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	req := &contract.ReplaceMemoriesWithSummaryRequest{
		EventId: "ghost",
		Summary: &contract.Memory{Significance: 5, Body: "gist"},
	}

	if _, err := s.ReplaceMemoriesWithSummary(context.Background(), req); err == nil {
		t.Fatal("expected an error for an unknown event id")
	}

	memories, err := s.db.GetMemoriesByEventId(context.Background(), "ghost")
	if err != nil {
		t.Fatalf("GetMemoriesByEventId: %s", err)
	}

	if len(*memories) != 1 {
		t.Error("the dangling memory must not be deleted when the event does not exist")
	}
}

// TestReplaceMemoriesWithSummary_RejectsInsignificantSummary verifies that a summary below the
// configured minimum significance is rejected, and that the original memories survive the
// rejected call — a caller must not lose data to a summary that never made it into the store.
func TestReplaceMemoriesWithSummary_RejectsInsignificantSummary(t *testing.T) {
	s := newTestServer(t)
	s.minimumMemorySignificance = 10

	if _, err := s.db.CreateEvent(context.Background(), types.Event{Id: "e1", Name: "trip", TimeStart: 100, Significance: 1}); err != nil {
		t.Fatalf("CreateEvent: %s", err)
	}

	if _, err := s.db.CreateMemory(context.Background(), types.Memory{Id: "m1", TimeStamp: 100, Significance: 1, EventId: "e1", Body: "detail"}); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	req := &contract.ReplaceMemoriesWithSummaryRequest{
		EventId: "e1",
		Summary: &contract.Memory{Significance: 1, Body: "gist"},
	}

	if _, err := s.ReplaceMemoriesWithSummary(context.Background(), req); err == nil {
		t.Fatal("expected an error for a summary below the minimum significance")
	}

	memories, err := s.db.GetMemoriesByEventId(context.Background(), "e1")
	if err != nil {
		t.Fatalf("GetMemoriesByEventId: %s", err)
	}

	if len(*memories) != 1 || (*memories)[0].Id != "m1" {
		t.Error("the original memory must survive a rejected summary")
	}
}

// TestGetSummarizationCandidates_RPC verifies that the RPC returns whatever the most recent scan
// stored on the server, converted to contract.
func TestGetSummarizationCandidates_RPC(t *testing.T) {
	s := newTestServer(t)

	s.summarizationCandidates = []db.SummarizationCandidate{
		{EventId: "e1", EventName: "trip", MemoryCount: 12},
	}

	res, err := s.GetSummarizationCandidates(context.Background(), &contract.EmptyRequest{})
	if err != nil {
		t.Fatalf("GetSummarizationCandidates: %s", err)
	}

	if len(res.GetCandidates()) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(res.GetCandidates()))
	}

	c := res.GetCandidates()[0]
	if c.GetEventId() != "e1" || c.GetEventName() != "trip" || c.GetMemoryCount() != 12 {
		t.Errorf("unexpected candidate: %+v", c)
	}
}

// TestGetSummarizationCandidates_Empty verifies that an unpopulated candidate list (the default
// before the first sleep cycle, or with the scan disabled) returns an empty response rather than
// an error.
func TestGetSummarizationCandidates_Empty(t *testing.T) {
	s := newTestServer(t)

	res, err := s.GetSummarizationCandidates(context.Background(), &contract.EmptyRequest{})
	if err != nil {
		t.Fatalf("GetSummarizationCandidates: %s", err)
	}

	if len(res.GetCandidates()) != 0 {
		t.Errorf("expected no candidates, got %d", len(res.GetCandidates()))
	}
}

// TestStoreMemory_InsignificantRejected verifies the "quietly forgotten" contract: a
// memory below the minimum significance returns no error and no id, but sets rejected so the caller
// can tell it apart from a store that simply produced no id, and nothing is persisted.
func TestStoreMemory_InsignificantRejected(t *testing.T) {
	s := newTestServer(t)
	s.minimumMemorySignificance = 10

	res, err := s.StoreMemory(context.Background(), &contract.Memory{Significance: 1, Body: "trivial"})
	if err != nil {
		t.Fatalf("StoreMemory: %s", err)
	}

	if !res.GetRejected() {
		t.Error("expected rejected=true for a memory below the minimum significance")
	}

	if res.GetId() != "" {
		t.Errorf("expected no id for a rejected memory, got %q", res.GetId())
	}

	if with, without := s.db.CountMemories(context.Background()); with+without != 0 {
		t.Errorf("expected nothing stored, got %d memories", with+without)
	}
}

// TestStoreMemory_SignificantNotRejected verifies a stored memory reports rejected=false with an id.
func TestStoreMemory_SignificantNotRejected(t *testing.T) {
	s := newTestServer(t)
	s.minimumMemorySignificance = 10

	res, err := s.StoreMemory(context.Background(), &contract.Memory{Significance: 20, Body: "worth keeping"})
	if err != nil {
		t.Fatalf("StoreMemory: %s", err)
	}

	if res.GetRejected() {
		t.Error("expected rejected=false for a memory above the minimum significance")
	}

	if res.GetId() == "" {
		t.Error("expected an id for a stored memory")
	}
}

// TestStoreMemory_NonexistentEventRejected verifies the prevention half: a memory whose
// event_id names no existing event is rejected with FailedPrecondition and nothing is persisted,
// so no dangling reference is ever created.
func TestStoreMemory_NonexistentEventRejected(t *testing.T) {
	s := newTestServer(t)

	res, err := s.StoreMemory(context.Background(), &contract.Memory{Significance: 5, Body: "orphan", EventId: "ghost"})
	if err == nil {
		t.Fatal("StoreMemory accepted a nonexistent event_id; expected an error")
	}

	if status.Code(err) != codes.FailedPrecondition {
		t.Errorf("expected FailedPrecondition, got %s", status.Code(err))
	}

	if res.GetId() != "" {
		t.Errorf("expected no id for a rejected memory, got %q", res.GetId())
	}

	if with, without := s.db.CountMemories(context.Background()); with+without != 0 {
		t.Errorf("expected nothing stored, got %d memories", with+without)
	}
}

// TestStoreMemory_ExistingEventAccepted verifies the guard admits a memory whose event_id names a
// real event.
func TestStoreMemory_ExistingEventAccepted(t *testing.T) {
	s := newTestServer(t)

	if _, err := s.db.CreateEvent(context.Background(), types.Event{Id: "e1", Name: "trip", TimeStart: 100, Significance: 5}); err != nil {
		t.Fatalf("CreateEvent: %s", err)
	}

	res, err := s.StoreMemory(context.Background(), &contract.Memory{Significance: 5, Body: "kept", EventId: "e1"})
	if err != nil {
		t.Fatalf("StoreMemory: %s", err)
	}

	if res.GetId() == "" {
		t.Error("expected an id for a memory attached to an existing event")
	}
}

// TestStoreMemory_IgnoresClientRecallState is a regression test: a fresh store must
// never inherit client-supplied recall state, or the memory arrives already reinforced (its decay
// clock pre-set and its effective significance boosted), which would make it hard to ever forget.
func TestStoreMemory_IgnoresClientRecallState(t *testing.T) {
	s := newTestServer(t)

	res, err := s.StoreMemory(context.Background(), &contract.Memory{
		Id:           "m1",
		Significance: 5,
		Body:         "fresh",
		TimeRecalled: time.Now().UnixNano(),
		RecallCount:  2000000000,
	})
	if err != nil {
		t.Fatalf("StoreMemory: %s", err)
	}

	if res.GetId() == "" {
		t.Fatal("expected an id for a stored memory")
	}

	got, err := s.db.GetMemoriesByIds(context.Background(), []string{"m1"})
	if err != nil {
		t.Fatalf("GetMemoriesByIds: %s", err)
	}

	if m := (*got)[0]; m.TimeRecalled != 0 || m.RecallCount != 0 {
		t.Errorf("expected recall state zeroed on a fresh store, got time_recalled=%d recall_count=%d", m.TimeRecalled, m.RecallCount)
	}
}

// TestStoreMemory_FutureTimestampRejected is a regression test: a store with a
// timestamp beyond the clock-skew allowance is rejected (a negative-age memory is undeletable by
// decay and ranks last for eviction) and nothing is persisted.
func TestStoreMemory_FutureTimestampRejected(t *testing.T) {
	s := newTestServer(t)

	future := time.Now().Add(time.Hour).UnixNano()

	res, err := s.StoreMemory(context.Background(), &contract.Memory{Significance: 5, Body: "from the future", TimeStamp: future})
	if err == nil {
		t.Fatal("StoreMemory accepted a far-future timestamp; expected an error")
	}

	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("expected InvalidArgument, got %s", status.Code(err))
	}

	if res.GetId() != "" {
		t.Errorf("expected no id for a rejected memory, got %q", res.GetId())
	}

	if with, without := s.db.CountMemories(context.Background()); with+without != 0 {
		t.Errorf("expected nothing stored, got %d memories", with+without)
	}
}

// TestStoreMemory_NearFutureTimestampAccepted verifies a timestamp within the clock-skew allowance
// is still accepted, so ordinary client/server clock drift is not rejected.
func TestStoreMemory_NearFutureTimestampAccepted(t *testing.T) {
	s := newTestServer(t)

	nearFuture := time.Now().Add(time.Minute).UnixNano()

	res, err := s.StoreMemory(context.Background(), &contract.Memory{Significance: 5, Body: "slightly ahead", TimeStamp: nearFuture})
	if err != nil {
		t.Fatalf("StoreMemory rejected a timestamp within the skew allowance: %s", err)
	}

	if res.GetId() == "" {
		t.Error("expected an id for a memory with a within-skew timestamp")
	}
}

// TestUpdateMemory_FutureTimestampRejected verifies the future-timestamp guard also covers the
// update path: UpdateMemory can write time_stamp, so a far-future value would be exploitable there
// too.
func TestUpdateMemory_FutureTimestampRejected(t *testing.T) {
	s := newTestServer(t)

	if _, err := s.db.CreateMemory(context.Background(), types.Memory{Id: "m1", TimeStamp: 100, Significance: 5, Body: "x"}); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	future := time.Now().Add(time.Hour).UnixNano()

	res, err := s.UpdateMemory(context.Background(), &contract.Memory{Id: "m1", TimeStamp: future})
	if err == nil {
		t.Fatal("UpdateMemory accepted a far-future timestamp; expected an error")
	}

	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("expected InvalidArgument, got %s", status.Code(err))
	}

	if res.GetOk() {
		t.Error("UpdateMemory reported Ok despite the future timestamp")
	}

	got, err := s.db.GetMemoriesByIds(context.Background(), []string{"m1"})
	if err != nil {
		t.Fatalf("GetMemoriesByIds: %s", err)
	}

	if (*got)[0].TimeStamp != 100 {
		t.Errorf("expected the stored timestamp unchanged at 100, got %d", (*got)[0].TimeStamp)
	}
}

// TestUpdateMemory_PartialUpdate verifies the UpdateMemory RPC: only the provided
// content fields overwrite the stored memory, and a successful update reports Ok.
func TestUpdateMemory_PartialUpdate(t *testing.T) {
	s := newTestServer(t)

	if _, err := s.db.CreateMemory(context.Background(), types.Memory{Id: "m1", TimeStamp: 100, Significance: 5, Body: "original", Group: "billing"}); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	res, err := s.UpdateMemory(context.Background(), &contract.Memory{Id: "m1", Significance: 9})
	if err != nil {
		t.Fatalf("UpdateMemory: %s", err)
	}

	if !res.GetOk() {
		t.Error("expected Ok for a successful update")
	}

	got, err := s.db.GetMemoriesByIds(context.Background(), []string{"m1"})
	if err != nil {
		t.Fatalf("GetMemoriesByIds: %s", err)
	}

	if m := (*got)[0]; m.Significance != 9 || m.Body != "original" || m.Group != "billing" {
		t.Errorf("expected significance 9 with body/group preserved, got %+v", m)
	}
}

// TestUpdateMemory_NonexistentEventRejected verifies that re-pointing a memory at an event
// that does not exist is rejected with FailedPrecondition and the memory's stored event_id is left
// unchanged.
func TestUpdateMemory_NonexistentEventRejected(t *testing.T) {
	s := newTestServer(t)

	if _, err := s.db.CreateMemory(context.Background(), types.Memory{Id: "m1", TimeStamp: 100, Significance: 5, Body: "x"}); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	res, err := s.UpdateMemory(context.Background(), &contract.Memory{Id: "m1", EventId: "ghost"})
	if err == nil {
		t.Fatal("UpdateMemory accepted a nonexistent event_id; expected an error")
	}

	if status.Code(err) != codes.FailedPrecondition {
		t.Errorf("expected FailedPrecondition, got %s", status.Code(err))
	}

	if res.GetOk() {
		t.Error("UpdateMemory reported Ok despite the nonexistent event")
	}

	got, err := s.db.GetMemoriesByIds(context.Background(), []string{"m1"})
	if err != nil {
		t.Fatalf("GetMemoriesByIds: %s", err)
	}

	if (*got)[0].EventId != "" {
		t.Errorf("expected the memory's event_id left unchanged, got %q", (*got)[0].EventId)
	}
}

// TestUpdateMemory_EmptyIdRejected verifies an empty id is rejected with InvalidArgument and
// nothing is touched.
func TestUpdateMemory_EmptyIdRejected(t *testing.T) {
	s := newTestServer(t)

	res, err := s.UpdateMemory(context.Background(), &contract.Memory{Id: "", Significance: 5})
	if err == nil {
		t.Fatal("UpdateMemory accepted an empty id; expected an error")
	}

	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("expected InvalidArgument, got %s", status.Code(err))
	}

	if res.GetOk() {
		t.Error("UpdateMemory reported success for an empty id")
	}
}

// TestUpdateMemory_UnknownIdNotFound verifies the RPC returns NotFound and creates nothing for an
// unknown id, rather than inserting a phantom memory.
func TestUpdateMemory_UnknownIdNotFound(t *testing.T) {
	s := newTestServer(t)

	res, err := s.UpdateMemory(context.Background(), &contract.Memory{Id: "nope", Significance: 5})
	if err == nil {
		t.Fatal("UpdateMemory accepted an unknown id; expected NotFound")
	}

	if status.Code(err) != codes.NotFound {
		t.Errorf("expected NotFound, got %s", status.Code(err))
	}

	if res.GetOk() {
		t.Error("UpdateMemory reported success for an unknown id")
	}

	if with, without := s.db.CountMemories(context.Background()); with+without != 0 {
		t.Fatalf("UpdateMemory created %d memories for an unknown id; expected none", with+without)
	}
}

// TestGetMemories_SignificanceExtremum verifies the RPC passes SignificanceExtremum through to the
// db filter and returns every memory tied at the highest significance, not just one.
func TestGetMemories_SignificanceExtremum(t *testing.T) {
	s := newTestServer(t)

	for _, m := range []types.Memory{
		{Id: "m1", TimeStamp: 100, Significance: 3, Body: "one"},
		{Id: "m2", TimeStamp: 200, Significance: 8, Body: "two"},
		{Id: "m3", TimeStamp: 300, Significance: 8, Body: "three"},
	} {
		if _, err := s.db.CreateMemory(context.Background(), m); err != nil {
			t.Fatalf("CreateMemory(%s): %s", m.Id, err)
		}
	}

	res, err := s.GetMemories(context.Background(), &contract.GetMemoriesRequest{
		SignificanceExtremum: contract.SignificanceExtremum_SIGNIFICANCE_EXTREMUM_HIGHEST,
	})
	if err != nil {
		t.Fatalf("GetMemories: %s", err)
	}

	if len(res.GetMemories()) != 2 {
		t.Fatalf("expected 2 memories tied at the highest significance, got %d", len(res.GetMemories()))
	}

	for _, m := range res.GetMemories() {
		if m.GetSignificance() != 8 {
			t.Errorf("expected significance 8, got %d for %s", m.GetSignificance(), m.GetId())
		}
	}
}

// TestGetMemories_SignificanceExtremum_RejectsCombinationWithRange verifies significance_extremum
// and significance_min/significance_max are mutually exclusive, matching GetEvents.
func TestGetMemories_SignificanceExtremum_RejectsCombinationWithRange(t *testing.T) {
	s := newTestServer(t)

	if _, err := s.GetMemories(context.Background(), &contract.GetMemoriesRequest{
		SignificanceExtremum: contract.SignificanceExtremum_SIGNIFICANCE_EXTREMUM_HIGHEST,
		SignificanceMin:      1,
	}); err == nil {
		t.Fatal("expected an error combining SignificanceExtremum with SignificanceMin")
	}

	if _, err := s.GetMemories(context.Background(), &contract.GetMemoriesRequest{
		SignificanceExtremum: contract.SignificanceExtremum_SIGNIFICANCE_EXTREMUM_LOWEST,
		SignificanceMax:      5,
	}); err == nil {
		t.Fatal("expected an error combining SignificanceExtremum with SignificanceMax")
	}
}

// memoryFaultStore wraps a real db.Store and lets a test force any one of several memory-RPC-
// adjacent methods to fail, so the many single-line mapError branches across memory.go can each be
// driven down their error path with a single reusable type. Every field defaults to nil, in which
// case the call passes through to the embedded Store.
type memoryFaultStore struct {
	db.Store

	eventExistsErr                error
	updateMemoryErr               error
	recallMemoriesErr             error
	getEventErr                   error
	replaceMemoriesWithSummaryErr error
	countMemoriesFilteredErr      error
	getMemoriesErr                error
	resolveSignificanceLevelErr   error
}

func (f memoryFaultStore) EventExists(ctx context.Context, id string) (bool, error) {
	if f.eventExistsErr != nil {
		return false, f.eventExistsErr
	}

	return f.Store.EventExists(ctx, id)
}

func (f memoryFaultStore) UpdateMemory(ctx context.Context, memory types.Memory) (bool, error) {
	if f.updateMemoryErr != nil {
		return false, f.updateMemoryErr
	}

	return f.Store.UpdateMemory(ctx, memory)
}

func (f memoryFaultStore) RecallMemories(ctx context.Context, ids []string) (*[]types.Memory, error) {
	if f.recallMemoriesErr != nil {
		return nil, f.recallMemoriesErr
	}

	return f.Store.RecallMemories(ctx, ids)
}

func (f memoryFaultStore) GetEvent(ctx context.Context, id string) (*types.Event, error) {
	if f.getEventErr != nil {
		return nil, f.getEventErr
	}

	return f.Store.GetEvent(ctx, id)
}

func (f memoryFaultStore) ReplaceMemoriesWithSummary(ctx context.Context, eventId string, summary types.Memory) (int, error) {
	if f.replaceMemoriesWithSummaryErr != nil {
		return 0, f.replaceMemoriesWithSummaryErr
	}

	return f.Store.ReplaceMemoriesWithSummary(ctx, eventId, summary)
}

func (f memoryFaultStore) CountMemoriesFiltered(ctx context.Context, filter db.MemoryFilter) (int, error) {
	if f.countMemoriesFilteredErr != nil {
		return 0, f.countMemoriesFilteredErr
	}

	return f.Store.CountMemoriesFiltered(ctx, filter)
}

func (f memoryFaultStore) GetMemories(ctx context.Context, filter db.MemoryFilter) (*[]types.Memory, error) {
	if f.getMemoriesErr != nil {
		return nil, f.getMemoriesErr
	}

	return f.Store.GetMemories(ctx, filter)
}

func (f memoryFaultStore) ResolveSignificanceLevel(ctx context.Context, spec db.SignificanceSpec) (sql.NullInt64, int32, error) {
	if f.resolveSignificanceLevelErr != nil {
		return sql.NullInt64{}, 0, f.resolveSignificanceLevelErr
	}

	return f.Store.ResolveSignificanceLevel(ctx, spec)
}

// TestStoreMemory_EventExistsErrorMapped verifies a generic EventExists failure (checking the
// memory's event_id) is mapped via mapError rather than returned raw.
func TestStoreMemory_EventExistsErrorMapped(t *testing.T) {
	s := newTestServer(t)

	wantErr := errors.New("exists boom")
	s.db = memoryFaultStore{Store: s.db, eventExistsErr: wantErr}

	_, err := s.StoreMemory(context.Background(), &contract.Memory{EventId: "e1", Significance: 5, Body: "x"})
	if status.Code(err) != codes.Internal {
		t.Fatalf("expected codes.Internal, got %s (%v)", status.Code(err), err)
	}
}

// TestStoreMemory_ResolveSignificanceGenericErrorMapped verifies a non-placement failure from
// resolveMemorySignificance is mapped via mapError rather than returned raw.
func TestStoreMemory_ResolveSignificanceGenericErrorMapped(t *testing.T) {
	s := newTestServer(t)

	wantErr := errors.New("resolve boom")
	s.db = memoryFaultStore{Store: s.db, resolveSignificanceLevelErr: wantErr}

	_, err := s.StoreMemory(context.Background(), &contract.Memory{
		Body:      "x",
		Placement: &contract.SignificancePlacement{Mode: contract.SignificancePlacement_ABOVE, Anchor: 5},
	})
	if status.Code(err) != codes.Internal {
		t.Fatalf("expected codes.Internal, got %s (%v)", status.Code(err), err)
	}
}

// TestUpdateMemory_EventExistsAndUpdateErrorsMapped verifies UpdateMemory's own EventExists,
// resolveMemorySignificance, and db.UpdateMemory failures are each mapped via mapError.
func TestUpdateMemory_EventExistsAndUpdateErrorsMapped(t *testing.T) {
	s := newTestServer(t)

	if _, err := s.db.CreateMemory(context.Background(), types.Memory{Id: "m1", TimeStamp: 100, Significance: 5, Body: "x"}); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	real := s.db

	existsErr := errors.New("exists boom")
	s.db = memoryFaultStore{Store: real, eventExistsErr: existsErr}

	if _, err := s.UpdateMemory(context.Background(), &contract.Memory{Id: "m1", EventId: "e1"}); status.Code(err) != codes.Internal {
		t.Fatalf("EventExists failure: expected codes.Internal, got %s (%v)", status.Code(err), err)
	}

	resolveErr := errors.New("resolve boom")
	s.db = memoryFaultStore{Store: real, resolveSignificanceLevelErr: resolveErr}

	if _, err := s.UpdateMemory(context.Background(), &contract.Memory{
		Id:        "m1",
		Placement: &contract.SignificancePlacement{Mode: contract.SignificancePlacement_ABOVE, Anchor: 5},
	}); status.Code(err) != codes.Internal {
		t.Fatalf("resolve failure: expected codes.Internal, got %s (%v)", status.Code(err), err)
	}

	updateErr := errors.New("update boom")
	s.db = memoryFaultStore{Store: real, updateMemoryErr: updateErr}

	if _, err := s.UpdateMemory(context.Background(), &contract.Memory{Id: "m1", Significance: 9}); status.Code(err) != codes.Internal {
		t.Fatalf("UpdateMemory failure: expected codes.Internal, got %s (%v)", status.Code(err), err)
	}
}

// TestDeleteMemories_EmptyIdsIsNoOp verifies an empty (post-dedupe) id list returns immediately
// without touching the store or the search index.
func TestDeleteMemories_EmptyIdsIsNoOp(t *testing.T) {
	s := newTestServer(t)

	res, err := s.DeleteMemories(context.Background(), &contract.DeleteMemoriesRequest{})
	if err != nil {
		t.Fatalf("DeleteMemories: %s", err)
	}

	if res.GetOk() {
		t.Error("expected Ok false for an empty request (nothing was asked for, nothing succeeded)")
	}
}

// TestRecallMemories_ErrorMapped verifies a generic RecallMemories failure is mapped via mapError.
func TestRecallMemories_ErrorMapped(t *testing.T) {
	s := newTestServer(t)

	wantErr := errors.New("recall boom")
	s.db = memoryFaultStore{Store: s.db, recallMemoriesErr: wantErr}

	if _, err := s.RecallMemories(context.Background(), &contract.RecallMemoriesRequest{Ids: []string{"m1"}}); status.Code(err) != codes.Internal {
		t.Fatalf("expected codes.Internal, got %s (%v)", status.Code(err), err)
	}
}

// TestReplaceMemoriesWithSummary_ErrorsMapped covers ReplaceMemoriesWithSummary's remaining
// error-mapping branches: an empty event_id, an invalid summary body, a generic GetEvent failure,
// a generic resolveMemorySignificance failure, and a generic db.ReplaceMemoriesWithSummary failure.
func TestReplaceMemoriesWithSummary_ErrorsMapped(t *testing.T) {
	s := newTestServer(t)

	if _, err := s.ReplaceMemoriesWithSummary(context.Background(), &contract.ReplaceMemoriesWithSummaryRequest{
		Summary: &contract.Memory{Body: "gist", Significance: 5},
	}); err == nil {
		t.Error("expected an error for a missing event_id")
	}

	if _, err := s.db.CreateEvent(context.Background(), types.Event{Id: "e1", Name: "trip", TimeStart: 100, Significance: 1}); err != nil {
		t.Fatalf("CreateEvent: %s", err)
	}

	if _, err := s.ReplaceMemoriesWithSummary(context.Background(), &contract.ReplaceMemoriesWithSummaryRequest{
		EventId: "e1",
		Summary: &contract.Memory{Significance: 5},
	}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("expected InvalidArgument for a summary with no body, got %s", status.Code(err))
	}

	real := s.db

	getEventErr := errors.New("get event boom")
	s.db = memoryFaultStore{Store: real, getEventErr: getEventErr}

	if _, err := s.ReplaceMemoriesWithSummary(context.Background(), &contract.ReplaceMemoriesWithSummaryRequest{
		EventId: "e1",
		Summary: &contract.Memory{Body: "gist", Significance: 5},
	}); status.Code(err) != codes.Internal {
		t.Errorf("GetEvent failure: expected codes.Internal, got %s (%v)", status.Code(err), err)
	}

	resolveErr := errors.New("resolve boom")
	s.db = memoryFaultStore{Store: real, resolveSignificanceLevelErr: resolveErr}

	if _, err := s.ReplaceMemoriesWithSummary(context.Background(), &contract.ReplaceMemoriesWithSummaryRequest{
		EventId: "e1",
		Summary: &contract.Memory{
			Body:      "gist",
			Placement: &contract.SignificancePlacement{Mode: contract.SignificancePlacement_ABOVE, Anchor: 5},
		},
	}); status.Code(err) != codes.Internal {
		t.Errorf("resolve failure: expected codes.Internal, got %s (%v)", status.Code(err), err)
	}

	replaceErr := errors.New("replace boom")
	s.db = memoryFaultStore{Store: real, replaceMemoriesWithSummaryErr: replaceErr}

	if _, err := s.ReplaceMemoriesWithSummary(context.Background(), &contract.ReplaceMemoriesWithSummaryRequest{
		EventId: "e1",
		Summary: &contract.Memory{Body: "gist", Significance: 5},
	}); status.Code(err) != codes.Internal {
		t.Errorf("db failure: expected codes.Internal, got %s (%v)", status.Code(err), err)
	}
}

// capturingMemoryStore wraps a real db.Store and records the last filter GetMemories was called
// with, so a test can assert on the RPC layer's limit/offset clamping without needing 200+ rows.
type capturingMemoryStore struct {
	db.Store
	gotFilter db.MemoryFilter
}

func (c *capturingMemoryStore) GetMemories(ctx context.Context, filter db.MemoryFilter) (*[]types.Memory, error) {
	c.gotFilter = filter

	return c.Store.GetMemories(ctx, filter)
}

// TestGetMemories_LimitAndOffsetClamped verifies an over-large limit is clamped to
// maxMemoryPageSize and a negative offset is clamped to 0.
func TestGetMemories_LimitAndOffsetClamped(t *testing.T) {
	s := newTestServer(t)

	captured := &capturingMemoryStore{Store: s.db}
	s.db = captured

	if _, err := s.GetMemories(context.Background(), &contract.GetMemoriesRequest{Limit: 10000, Offset: -5}); err != nil {
		t.Fatalf("GetMemories: %s", err)
	}

	if captured.gotFilter.Limit != maxMemoryPageSize {
		t.Errorf("expected limit clamped to %d, got %d", maxMemoryPageSize, captured.gotFilter.Limit)
	}

	if captured.gotFilter.Offset != 0 {
		t.Errorf("expected negative offset clamped to 0, got %d", captured.gotFilter.Offset)
	}
}

// TestGetMemories_CountAndListErrorsMapped verifies CountMemoriesFiltered's and GetMemories' own
// generic failures are both mapped via mapError.
func TestGetMemories_CountAndListErrorsMapped(t *testing.T) {
	countErr := errors.New("count boom")
	s := newTestServer(t)
	s.db = memoryFaultStore{Store: s.db, countMemoriesFilteredErr: countErr}

	if _, err := s.GetMemories(context.Background(), &contract.GetMemoriesRequest{}); status.Code(err) != codes.Internal {
		t.Fatalf("CountMemoriesFiltered failure: expected codes.Internal, got %s (%v)", status.Code(err), err)
	}

	listErr := errors.New("list boom")
	s2 := newTestServer(t)
	s2.db = memoryFaultStore{Store: s2.db, getMemoriesErr: listErr}

	if _, err := s2.GetMemories(context.Background(), &contract.GetMemoriesRequest{}); status.Code(err) != codes.Internal {
		t.Fatalf("GetMemories failure: expected codes.Internal, got %s (%v)", status.Code(err), err)
	}
}

// TestUpdateMemory_PlacementInvalidRejected verifies UpdateMemory's own db.ErrInvalidPlacement
// branch (naming a placement anchor that does not exist), distinct from a generic
// resolveMemorySignificance failure - it must map to InvalidArgument, not codes.Internal.
func TestUpdateMemory_PlacementInvalidRejected(t *testing.T) {
	s := newTestServer(t)

	if _, err := s.db.CreateMemory(context.Background(), types.Memory{Id: "m1", TimeStamp: 100, Significance: 5, Body: "x"}); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	_, err := s.UpdateMemory(context.Background(), &contract.Memory{
		Id:        "m1",
		Placement: &contract.SignificancePlacement{Mode: contract.SignificancePlacement_ABOVE, AnchorId: "does-not-exist"},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument, got %s (%v)", status.Code(err), err)
	}
}

// TestReplaceMemoriesWithSummary_PlacementInvalidRejected verifies
// ReplaceMemoriesWithSummary's own db.ErrInvalidPlacement branch for the summary's placement.
func TestReplaceMemoriesWithSummary_PlacementInvalidRejected(t *testing.T) {
	s := newTestServer(t)

	if _, err := s.db.CreateEvent(context.Background(), types.Event{Id: "e1", Name: "trip", TimeStart: 100, Significance: 1}); err != nil {
		t.Fatalf("CreateEvent: %s", err)
	}

	_, err := s.ReplaceMemoriesWithSummary(context.Background(), &contract.ReplaceMemoriesWithSummaryRequest{
		EventId: "e1",
		Summary: &contract.Memory{
			Body:      "gist",
			Placement: &contract.SignificancePlacement{Mode: contract.SignificancePlacement_ABOVE, AnchorId: "does-not-exist"},
		},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument, got %s (%v)", status.Code(err), err)
	}
}
