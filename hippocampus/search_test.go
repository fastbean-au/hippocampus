package hippocampus

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/fastbean-au/hippocampus/contract"
	"github.com/fastbean-au/hippocampus/db"
	"github.com/fastbean-au/hippocampus/search"
	"github.com/fastbean-au/hippocampus/types"
)

func testMemory(id string, significance int32) types.Memory {
	return types.Memory{Id: id, TimeStamp: 100, Significance: significance, Body: "hello world"}
}

func testEvent(id string) types.Event {
	return types.Event{Id: id, Name: "event " + id, TimeStart: 100, Significance: 5}
}

// fakeIndex implements search.Index, recording every call so tests can assert the write- and
// delete-through hooks fire (and in what order).
type fakeIndex struct {
	enabled   bool
	searchIds []string
	searchErr error

	calls []string
	docs  []search.Doc
}

func (f *fakeIndex) IndexMemory(doc search.Doc) {
	f.calls = append(f.calls, "index:"+doc.Id)
	f.docs = append(f.docs, doc)
}

func (f *fakeIndex) DeleteMemories(ids []string) {
	call := "delete"
	for _, id := range ids {
		call += ":" + id
	}

	f.calls = append(f.calls, call)
}

func (f *fakeIndex) DeleteByEventId(eventId string) {
	f.calls = append(f.calls, "delete_event:"+eventId)
}

func (f *fakeIndex) SetEventId(fromEventId string, toEventId string) {
	f.calls = append(f.calls, "set_event:"+fromEventId+">"+toEventId)
}

func (f *fakeIndex) Purge() {
	f.calls = append(f.calls, "purge")
}

func (f *fakeIndex) Search(ctx context.Context, query search.Query) ([]string, error) {
	f.calls = append(f.calls, "search:"+query.Text)

	return f.searchIds, f.searchErr
}

func (f *fakeIndex) Enabled() bool {
	return f.enabled
}

func (f *fakeIndex) Close() error {
	return nil
}

func newSearchTestServer(t *testing.T, idx search.Index) *Server {
	t.Helper()

	database, err := db.New("")
	if err != nil {
		t.Fatalf("failed to create test DB: %s", err)
	}

	return &Server{db: database, search: idx}
}

// TestSearchMemories_DisabledReturnsFailedPrecondition verifies both the no-op index and a nil
// index (a Server constructed without one) reject the RPC with FailedPrecondition.
func TestSearchMemories_DisabledReturnsFailedPrecondition(t *testing.T) {
	for _, s := range []*Server{
		newSearchTestServer(t, nil),
		newSearchTestServer(t, search.NewNoop()),
	} {
		_, err := s.SearchMemories(context.Background(), &contract.SearchMemoriesRequest{Query: "anything"})

		if status.Code(err) != codes.FailedPrecondition {
			t.Errorf("expected FailedPrecondition, got %v", err)
		}
	}
}

// TestSearchMemories_DropsStaleIdsAndKeepsRelevanceOrder verifies ids the index returns that the
// primary store no longer holds are silently dropped, and surviving results come back in the
// index's relevance order rather than the fetch order.
func TestSearchMemories_DropsStaleIdsAndKeepsRelevanceOrder(t *testing.T) {
	idx := &fakeIndex{enabled: true, searchIds: []string{"m2", "stale", "m1"}}
	s := newSearchTestServer(t, idx)

	for _, id := range []string{"m1", "m2"} {
		if _, err := s.db.CreateMemory(context.Background(), testMemory(id, 5)); err != nil {
			t.Fatalf("CreateMemory(%s): %s", id, err)
		}
	}

	res, err := s.SearchMemories(context.Background(), &contract.SearchMemoriesRequest{Query: "hello"})
	if err != nil {
		t.Fatalf("SearchMemories: %s", err)
	}

	if len(res.Memories) != 2 || res.Memories[0].Id != "m2" || res.Memories[1].Id != "m1" {
		t.Errorf("expected [m2 m1] (stale dropped, relevance order kept), got %v", res.Memories)
	}

	if res.Memories[0].RecallCount != 0 {
		t.Errorf("reinforce=false must not touch recall state, got recall_count %d", res.Memories[0].RecallCount)
	}
}

// TestSearchMemories_ReinforceRecalls verifies the reinforce flag routes matches through recall,
// bumping the recall count.
func TestSearchMemories_ReinforceRecalls(t *testing.T) {
	idx := &fakeIndex{enabled: true, searchIds: []string{"m1"}}
	s := newSearchTestServer(t, idx)

	if _, err := s.db.CreateMemory(context.Background(), testMemory("m1", 5)); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	res, err := s.SearchMemories(context.Background(), &contract.SearchMemoriesRequest{Query: "hello", Reinforce: true})
	if err != nil {
		t.Fatalf("SearchMemories: %s", err)
	}

	if len(res.Memories) != 1 || res.Memories[0].RecallCount != 1 {
		t.Errorf("reinforce=true should recall the match (recall_count 1), got %v", res.Memories)
	}
}

// TestSearchMemories_EmptyQueryRejected verifies a missing query is an error even when enabled.
func TestSearchMemories_EmptyQueryRejected(t *testing.T) {
	s := newSearchTestServer(t, &fakeIndex{enabled: true})

	if _, err := s.SearchMemories(context.Background(), &contract.SearchMemoriesRequest{}); err == nil {
		t.Error("expected an error for an empty query")
	}
}

// TestSearchMemories_IndexErrorMapped verifies a failing index search is mapped through mapError
// rather than returned raw (which would leak driver detail and mis-code the RPC).
func TestSearchMemories_IndexErrorMapped(t *testing.T) {
	idx := &fakeIndex{enabled: true, searchErr: errors.New("cluster unreachable")}
	s := newSearchTestServer(t, idx)

	_, err := s.SearchMemories(context.Background(), &contract.SearchMemoriesRequest{Query: "hello"})
	if err == nil {
		t.Fatal("expected the index search failure to surface")
	}

	if status.Code(err) != codes.Internal {
		t.Errorf("expected the failure masked to codes.Internal, got %s (%v)", status.Code(err), err)
	}
}

// TestSearchMemories_NoMatchesReturnsEmpty verifies a successful index search returning no ids
// short-circuits to an empty result without touching the primary store.
func TestSearchMemories_NoMatchesReturnsEmpty(t *testing.T) {
	idx := &fakeIndex{enabled: true}
	s := newSearchTestServer(t, idx)

	res, err := s.SearchMemories(context.Background(), &contract.SearchMemoriesRequest{Query: "nothing matches"})
	if err != nil {
		t.Fatalf("SearchMemories: %s", err)
	}

	if len(res.GetMemories()) != 0 {
		t.Errorf("expected no memories for a search with no matching ids, got %v", res.GetMemories())
	}
}

// failFetchStore wraps a real db.Store but forces both post-search fetch paths (RecallMemories,
// used when reinforce is set, and GetMemoriesByIds otherwise) to fail, so SearchMemories' second
// error-mapping branch - reached only after the index search itself succeeds - can be exercised.
type failFetchStore struct {
	db.Store
	err error
}

func (f failFetchStore) RecallMemories(ctx context.Context, ids []string) (*[]types.Memory, error) {
	return nil, f.err
}

func (f failFetchStore) GetMemoriesByIds(ctx context.Context, ids []string) (*[]types.Memory, error) {
	return nil, f.err
}

// TestSearchMemories_FetchErrorMapped verifies a failure re-reading the matched ids from the
// primary store - after a successful index search - is also mapped rather than returned raw, for
// both the reinforcing and non-reinforcing fetch paths.
func TestSearchMemories_FetchErrorMapped(t *testing.T) {
	wantErr := errors.New("store unavailable")

	for _, reinforce := range []bool{false, true} {
		idx := &fakeIndex{enabled: true, searchIds: []string{"m1"}}
		s := newSearchTestServer(t, idx)
		s.db = failFetchStore{Store: s.db, err: wantErr}

		_, err := s.SearchMemories(context.Background(), &contract.SearchMemoriesRequest{Query: "hello", Reinforce: reinforce})
		if err == nil {
			t.Fatalf("reinforce=%v: expected the fetch failure to surface", reinforce)
		}

		if status.Code(err) != codes.Internal {
			t.Errorf("reinforce=%v: expected codes.Internal, got %s (%v)", reinforce, status.Code(err), err)
		}
	}
}

// TestSearchHooks_WriteAndDeleteThrough verifies each mutating RPC fires the matching index
// operation - and that binary memories are never indexed.
func TestSearchHooks_WriteAndDeleteThrough(t *testing.T) {
	idx := &fakeIndex{enabled: true}
	s := newSearchTestServer(t, idx)

	if _, err := s.StoreMemory(context.Background(), &contract.Memory{Id: "m1", Body: "hello world", Significance: 5}); err != nil {
		t.Fatalf("StoreMemory: %s", err)
	}

	if _, err := s.StoreMemory(context.Background(), &contract.Memory{Id: "bin", Body: "AAEC", Significance: 5, IsBinary: contract.Bool_TRUE}); err != nil {
		t.Fatalf("StoreMemory(binary): %s", err)
	}

	if _, err := s.DeleteMemories(context.Background(), &contract.DeleteMemoriesRequest{Ids: []string{"m1", "bin"}}); err != nil {
		t.Fatalf("DeleteMemories: %s", err)
	}

	if _, err := s.Purge(context.Background(), &contract.EmptyRequest{}); err != nil {
		t.Fatalf("Purge: %s", err)
	}

	want := []string{"index:m1", "delete:m1:bin", "purge"}

	if len(idx.calls) != len(want) {
		t.Fatalf("expected calls %v, got %v", want, idx.calls)
	}

	for i, call := range want {
		if idx.calls[i] != call {
			t.Errorf("call %d: expected %q, got %q", i, call, idx.calls[i])
		}
	}
}

// TestSearchHooks_SummaryDeleteThenIndex verifies ReplaceMemoriesWithSummary enqueues the
// event-scoped delete before the summary's index write - the order the FIFO worker preserves.
func TestSearchHooks_SummaryDeleteThenIndex(t *testing.T) {
	idx := &fakeIndex{enabled: true}
	s := newSearchTestServer(t, idx)

	if _, err := s.db.CreateEvent(context.Background(), testEvent("e1")); err != nil {
		t.Fatalf("CreateEvent: %s", err)
	}

	m := testMemory("m1", 5)
	m.EventId = "e1"

	if _, err := s.db.CreateMemory(context.Background(), m); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	res, err := s.ReplaceMemoriesWithSummary(context.Background(), &contract.ReplaceMemoriesWithSummaryRequest{
		EventId: "e1",
		Summary: &contract.Memory{Body: "the summary", Significance: 5},
	})
	if err != nil {
		t.Fatalf("ReplaceMemoriesWithSummary: %s", err)
	}

	want := []string{"delete_event:e1", "index:" + res.Id}

	if len(idx.calls) != 2 || idx.calls[0] != want[0] || idx.calls[1] != want[1] {
		t.Errorf("expected %v, got %v", want, idx.calls)
	}

	if !idx.docs[0].IsSummary {
		t.Error("the indexed summary document should carry is_summary")
	}
}

// TestUpdateMemory_ReindexesNonBinary verifies the UpdateMemory RPC re-propagates the updated
// memory to the search index for a non-binary memory, keyed off the memory's stored is_binary flag
// (which the RPC does not change), and never indexes a binary one.
func TestUpdateMemory_ReindexesNonBinary(t *testing.T) {
	idx := &fakeIndex{enabled: true}
	s := newSearchTestServer(t, idx)

	if _, err := s.db.CreateMemory(context.Background(), types.Memory{Id: "m1", TimeStamp: 100, Significance: 5, Body: "text"}); err != nil {
		t.Fatalf("CreateMemory(m1): %s", err)
	}

	if _, err := s.db.CreateMemory(context.Background(), types.Memory{Id: "b1", TimeStamp: 100, Significance: 5, Body: "raw", IsBinary: true}); err != nil {
		t.Fatalf("CreateMemory(b1): %s", err)
	}

	idx.calls = nil
	idx.docs = nil

	if _, err := s.UpdateMemory(context.Background(), &contract.Memory{Id: "m1", Body: "updated text"}); err != nil {
		t.Fatalf("UpdateMemory(m1): %s", err)
	}

	// The binary memory's stored is_binary flag keeps it out of the index even though the request
	// omits is_binary (the RPC reads the stored flag, not the request).
	if _, err := s.UpdateMemory(context.Background(), &contract.Memory{Id: "b1", Significance: 9}); err != nil {
		t.Fatalf("UpdateMemory(b1): %s", err)
	}

	if len(idx.calls) != 1 || idx.calls[0] != "index:m1" {
		t.Fatalf("expected exactly [index:m1], got %v", idx.calls)
	}

	if len(idx.docs) != 1 || idx.docs[0].Body != "updated text" {
		t.Errorf("expected the re-indexed doc to carry the updated body, got %+v", idx.docs)
	}
}
