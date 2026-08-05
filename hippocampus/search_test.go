package hippocampus

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/fastbean-au/hippocampus/auth"
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
	enabled    bool
	searchIds  []string
	searchHits []search.Hit
	searchErr  error

	// searchLimit records the limit the index was actually asked for, so a test can assert on the
	// over-fetching the re-ranking does.
	searchLimit int

	// supportsVectors makes the fake claim a vector index, and queries records every Query it was
	// given so a test can assert which of them carried a vector.
	supportsVectors bool
	queries         []search.Query

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

// Search returns searchHits when a test set explicit scores, and otherwise synthesises strictly
// decreasing scores from searchIds - so a test that only cares about ordering can keep expressing
// it as a list of ids, and gets the relevance order it wrote.
func (f *fakeIndex) Search(ctx context.Context, query search.Query) ([]search.Hit, error) {
	f.calls = append(f.calls, "search:"+query.Text)
	f.searchLimit = query.Limit
	f.queries = append(f.queries, query)

	if f.searchHits != nil {
		return f.searchHits, f.searchErr
	}

	hits := make([]search.Hit, 0, len(f.searchIds))
	for i, id := range f.searchIds {
		hits = append(hits, search.Hit{Id: id, Score: float64(len(f.searchIds) - i)})
	}

	return hits, f.searchErr
}

func (f *fakeIndex) Enabled() bool {
	return f.enabled
}

func (f *fakeIndex) SupportsVectors() bool {
	return f.supportsVectors
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

	if res.TotalCount != 2 {
		t.Errorf("expected total_count to match the returned rows (2, stale dropped), got %d", res.TotalCount)
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

// TestSearchMemories_ReinforceSuppressedForReader verifies the reinforcement gate applies to the
// search path too: a reader for whom reinforcement is disabled gets the match back but without the
// recall write, exactly as RecallMemories downgrades.
func TestSearchMemories_ReinforceSuppressedForReader(t *testing.T) {
	idx := &fakeIndex{enabled: true, searchIds: []string{"m1"}}
	s := newSearchTestServer(t, idx)
	s.readerRecallReinforces = false

	if _, err := s.db.CreateMemory(context.Background(), testMemory("m1", 5)); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	ctx := auth.ContextWithTier(context.Background(), auth.TierReader)

	res, err := s.SearchMemories(ctx, &contract.SearchMemoriesRequest{Query: "hello", Reinforce: true})
	if err != nil {
		t.Fatalf("SearchMemories: %s", err)
	}

	if len(res.Memories) != 1 || res.Memories[0].RecallCount != 0 {
		t.Errorf("a reader with reinforcement disabled should get the match without recall (recall_count 0), got %v", res.Memories)
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

// TestSearchMemories_WorksOnTheSQLBackend is the end-to-end check that the default deployment can
// search at all: a SQLite store with no OpenSearch anywhere, driven through the RPC exactly as a
// client would. Every other test in this file drives a fake index, so this is the one that would
// catch the wiring between the RPC, the search adapter, and the store's own index coming apart.
func TestSearchMemories_WorksOnTheSQLBackend(t *testing.T) {
	ctx := context.Background()

	database, err := db.New("")
	if err != nil {
		t.Fatalf("failed to create test DB: %s", err)
	}
	defer func() { _ = database.Close() }()

	idx, err := search.NewSQL(database)
	if err != nil {
		t.Fatalf("search.NewSQL: %s", err)
	}

	s := &Server{db: database, search: idx}

	bodies := map[string]string{
		"m1": "the deployment failed on the staging cluster",
		"m2": "lunch was quite good today",
	}

	for id, body := range bodies {
		memory := types.Memory{Id: id, TimeStamp: 100, Significance: 5, Body: body}

		if _, err := s.db.CreateMemory(ctx, memory); err != nil {
			t.Fatalf("CreateMemory(%s): %s", id, err)
		}
	}

	res, err := s.SearchMemories(ctx, &contract.SearchMemoriesRequest{Query: "deployment"})
	if err != nil {
		t.Fatalf("SearchMemories: %s", err)
	}

	if len(res.Memories) != 1 || res.Memories[0].Id != "m1" {
		t.Fatalf("SearchMemories returned %v, want just m1", res.Memories)
	}

	// The body must come back through the primary store's decompressing scanner, not from the
	// index (which holds no body at all).
	if res.Memories[0].Body != bodies["m1"] {
		t.Errorf("returned body %q, want %q", res.Memories[0].Body, bodies["m1"])
	}

	if res.TotalCount != 1 {
		t.Errorf("total_count %d, want 1", res.TotalCount)
	}
}

// A reinforcing search must still reinforce on this backend: the recall goes through the primary
// store, so nothing about it is index-specific, but it is the combination that a caller uses.
func TestSearchMemories_ReinforcesOnTheSQLBackend(t *testing.T) {
	ctx := context.Background()

	database, err := db.New("")
	if err != nil {
		t.Fatalf("failed to create test DB: %s", err)
	}
	defer func() { _ = database.Close() }()

	idx, err := search.NewSQL(database)
	if err != nil {
		t.Fatalf("search.NewSQL: %s", err)
	}

	s := &Server{db: database, search: idx}

	memory := types.Memory{Id: "m1", TimeStamp: 100, Significance: 5, Body: "the deployment failed"}
	if _, err := s.db.CreateMemory(ctx, memory); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	res, err := s.SearchMemories(ctx, &contract.SearchMemoriesRequest{Query: "deployment", Reinforce: true})
	if err != nil {
		t.Fatalf("SearchMemories: %s", err)
	}

	if len(res.Memories) != 1 {
		t.Fatalf("SearchMemories returned %d memories, want 1", len(res.Memories))
	}

	if res.Memories[0].RecallCount != 1 {
		t.Errorf("recall_count %d after a reinforcing search, want 1", res.Memories[0].RecallCount)
	}
}

// A memory deleted by consolidation must stop being findable, and on this backend that is the
// delete trigger's job rather than the delete observer's - so it holds even though nothing wires
// an observer here.
func TestSearchMemories_ConsolidationRemovesFromTheSQLBackend(t *testing.T) {
	ctx := context.Background()

	database, err := db.New("")
	if err != nil {
		t.Fatalf("failed to create test DB: %s", err)
	}
	defer func() { _ = database.Close() }()

	idx, err := search.NewSQL(database)
	if err != nil {
		t.Fatalf("search.NewSQL: %s", err)
	}

	s := &Server{db: database, search: idx}

	memory := types.Memory{Id: "m1", TimeStamp: 100, Significance: 5, Body: "the deployment failed"}
	if _, err := s.db.CreateMemory(ctx, memory); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	if _, err := s.db.DeleteMemories(ctx, []string{"m1"}); err != nil {
		t.Fatalf("DeleteMemories: %s", err)
	}

	res, err := s.SearchMemories(ctx, &contract.SearchMemoriesRequest{Query: "deployment"})
	if err != nil {
		t.Fatalf("SearchMemories: %s", err)
	}

	if len(res.Memories) != 0 {
		t.Errorf("a deleted memory is still findable: %v", res.Memories)
	}
}

// TestSearchMemories_RankingReordersResults drives the blend through the RPC: two near-equal
// textual matches, where the more significant memory should come out first.
func TestSearchMemories_RankingReordersResults(t *testing.T) {
	ctx := context.Background()

	idx := &fakeIndex{
		enabled: true,
		searchHits: []search.Hit{
			{Id: "ordinary", Score: 1.01},
			{Id: "important", Score: 1.0},
		},
	}

	s := newSearchTestServer(t, idx)
	s.ranking = rankingWeights{significance: 0.3, recall: 0.2}

	for id, significance := range map[string]int32{"ordinary": 1, "important": 20} {
		memory := types.Memory{Id: id, TimeStamp: 100, Significance: significance, Body: "hello world"}

		if _, err := s.db.CreateMemory(ctx, memory); err != nil {
			t.Fatalf("CreateMemory(%s): %s", id, err)
		}
	}

	res, err := s.SearchMemories(ctx, &contract.SearchMemoriesRequest{Query: "hello"})
	if err != nil {
		t.Fatalf("SearchMemories: %s", err)
	}

	if len(res.Memories) != 2 || res.Memories[0].Id != "important" {
		t.Errorf("got %v, want the significant memory first", memoryIds(res.Memories))
	}
}

// With ranking off the backend's order must reach the caller untouched, and the index must be
// asked for exactly the caller's limit rather than an over-fetched window.
func TestSearchMemories_RankingOffPreservesBackendOrder(t *testing.T) {
	ctx := context.Background()

	idx := &fakeIndex{
		enabled: true,
		searchHits: []search.Hit{
			{Id: "ordinary", Score: 1.01},
			{Id: "important", Score: 1.0},
		},
	}

	s := newSearchTestServer(t, idx)

	for id, significance := range map[string]int32{"ordinary": 1, "important": 20} {
		memory := types.Memory{Id: id, TimeStamp: 100, Significance: significance, Body: "hello world"}

		if _, err := s.db.CreateMemory(ctx, memory); err != nil {
			t.Fatalf("CreateMemory(%s): %s", id, err)
		}
	}

	res, err := s.SearchMemories(ctx, &contract.SearchMemoriesRequest{Query: "hello", Limit: 5})
	if err != nil {
		t.Fatalf("SearchMemories: %s", err)
	}

	if len(res.Memories) != 2 || res.Memories[0].Id != "ordinary" {
		t.Errorf("got %v, want the backend order", memoryIds(res.Memories))
	}

	if idx.searchLimit != 5 {
		t.Errorf("index was asked for %d candidates, want the caller's 5 with ranking off", idx.searchLimit)
	}
}

// Ranking widens the candidate window so a significant memory just outside the caller's page can
// still be promoted into it.
func TestSearchMemories_RankingOverFetchesCandidates(t *testing.T) {
	idx := &fakeIndex{enabled: true, searchIds: []string{"m1"}}

	s := newSearchTestServer(t, idx)
	s.ranking = rankingWeights{significance: 0.3}

	if _, err := s.SearchMemories(context.Background(), &contract.SearchMemoriesRequest{Query: "hello", Limit: 5}); err != nil {
		t.Fatalf("SearchMemories: %s", err)
	}

	if want := 5 * rankingOverFetch; idx.searchLimit != want {
		t.Errorf("index was asked for %d candidates, want %d", idx.searchLimit, want)
	}
}

// The result must still be truncated to the caller's limit, not to the over-fetched window.
func TestSearchMemories_RankingTruncatesToTheRequestedLimit(t *testing.T) {
	ctx := context.Background()

	ids := []string{"m1", "m2", "m3", "m4", "m5"}

	idx := &fakeIndex{enabled: true, searchIds: ids}

	s := newSearchTestServer(t, idx)
	s.ranking = rankingWeights{significance: 0.3}

	for i, id := range ids {
		memory := types.Memory{Id: id, TimeStamp: 100, Significance: int32(i + 1), Body: "hello world"}

		if _, err := s.db.CreateMemory(ctx, memory); err != nil {
			t.Fatalf("CreateMemory(%s): %s", id, err)
		}
	}

	res, err := s.SearchMemories(ctx, &contract.SearchMemoriesRequest{Query: "hello", Limit: 2})
	if err != nil {
		t.Fatalf("SearchMemories: %s", err)
	}

	if len(res.Memories) != 2 {
		t.Errorf("got %d memories, want the requested 2", len(res.Memories))
	}

	if res.TotalCount != 2 {
		t.Errorf("total_count %d, want 2", res.TotalCount)
	}
}

// A reinforcing search must reinforce exactly what it returns. Over-fetching means the candidate
// set is larger than the page, and recalling the candidates would reset the decay clock on
// memories the caller was never shown - silently changing what the store forgets.
func TestSearchMemories_ReinforceOnlyTouchesReturnedMemories(t *testing.T) {
	ctx := context.Background()

	ids := []string{"m1", "m2", "m3", "m4"}

	idx := &fakeIndex{enabled: true, searchIds: ids}

	s := newSearchTestServer(t, idx)
	s.ranking = rankingWeights{significance: 0.3}

	for _, id := range ids {
		memory := types.Memory{Id: id, TimeStamp: 100, Significance: 5, Body: "hello world"}

		if _, err := s.db.CreateMemory(ctx, memory); err != nil {
			t.Fatalf("CreateMemory(%s): %s", id, err)
		}
	}

	res, err := s.SearchMemories(ctx, &contract.SearchMemoriesRequest{Query: "hello", Limit: 1, Reinforce: true})
	if err != nil {
		t.Fatalf("SearchMemories: %s", err)
	}

	if len(res.Memories) != 1 {
		t.Fatalf("got %d memories, want 1", len(res.Memories))
	}

	returned := res.Memories[0].Id

	// The returned memory carries the recall its own call produced, not the pre-recall snapshot.
	if res.Memories[0].RecallCount != 1 {
		t.Errorf("returned memory has recall_count %d, want 1", res.Memories[0].RecallCount)
	}

	// Every other candidate must be untouched.
	stored, err := s.db.GetMemoriesByIds(ctx, ids)
	if err != nil {
		t.Fatalf("GetMemoriesByIds: %s", err)
	}

	for _, memory := range *stored {
		if memory.Id == returned {
			continue
		}

		if memory.RecallCount != 0 {
			t.Errorf("candidate %s was reinforced (recall_count %d) but never returned", memory.Id, memory.RecallCount)
		}

		if memory.TimeRecalled != 0 {
			t.Errorf("candidate %s had its decay clock reset but was never returned", memory.Id)
		}
	}
}

// memoryIds reduces a response to its ids, for readable ordering assertions.
func memoryIds(memories []*contract.Memory) []string {
	ids := make([]string, 0, len(memories))
	for _, memory := range memories {
		ids = append(ids, memory.Id)
	}

	return ids
}
