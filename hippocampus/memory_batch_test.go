package hippocampus

import (
	"context"
	"fmt"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/fastbean-au/hippocampus/contract"
	"github.com/fastbean-au/hippocampus/types"
)

// TestStoreMemories_PartialSuccess is the property the whole RPC exists for: a batch carrying one
// unusable record still stores the rest. A producer holding records it did not author cannot
// re-author the bad one, so failing its neighbours only costs the store data it could have kept.
func TestStoreMemories_PartialSuccess(t *testing.T) {
	s := newTestServer(t)
	s.minimumMemorySignificance = 10

	res, err := s.StoreMemories(context.Background(), &contract.StoreMemoriesRequest{Memories: []*contract.Memory{
		{Id: "ok-1", Significance: 20, Body: "kept"},
		{Id: "bad-1", Significance: 20, Body: "orphan", EventId: "ghost"},
		{Id: "small-1", Significance: 1, Body: "trivial"},
		{Id: "ok-2", Significance: 20, Body: "kept too"},
	}})
	if err != nil {
		t.Fatalf("StoreMemories: %s", err)
	}

	results := res.GetResults()
	if len(results) != 4 {
		t.Fatalf("expected one result per memory, got %d", len(results))
	}

	if results[0].GetId() != "ok-1" || results[3].GetId() != "ok-2" {
		t.Errorf("expected the two well-formed memories stored, got ids %q and %q", results[0].GetId(), results[3].GetId())
	}

	if got := codes.Code(results[1].GetCode()); got != codes.FailedPrecondition {
		t.Errorf("memory naming a nonexistent event reported %s, want FailedPrecondition", got)
	}

	if results[1].GetError() == "" {
		t.Error("a failed result must carry the message saying why")
	}

	// Insignificance is not a failure: it is the store declining to keep something, exactly as on
	// StoreMemory, and it must not be reported with an error code.
	if !results[2].GetRejected() || results[2].GetCode() != 0 {
		t.Errorf("expected the insignificant memory rejected with code 0, got rejected=%v code=%d",
			results[2].GetRejected(), results[2].GetCode())
	}

	if res.GetStored() != 2 || res.GetRejected() != 1 || res.GetFailed() != 1 {
		t.Errorf("counts = stored %d, rejected %d, failed %d; want 2/1/1",
			res.GetStored(), res.GetRejected(), res.GetFailed())
	}

	if with, without := s.db.CountMemories(context.Background()); with+without != 2 {
		t.Errorf("expected 2 memories persisted, got %d", with+without)
	}
}

// TestStoreMemories_AppliesTheWritePath pins that a batch is held to the same rules a single write
// is - it is not ImportBatch. Client-supplied recall state is discarded, defaults are applied, and
// an id the store already holds fails with AlreadyExists rather than replacing a live row.
func TestStoreMemories_AppliesTheWritePath(t *testing.T) {
	s := newTestServer(t)

	if _, err := s.StoreMemory(context.Background(), &contract.Memory{Id: "held", Significance: 5, Body: "original"}); err != nil {
		t.Fatalf("StoreMemory: %s", err)
	}

	res, err := s.StoreMemories(context.Background(), &contract.StoreMemoriesRequest{Memories: []*contract.Memory{
		{Id: "fresh", Significance: 5, Body: "new", RecallCount: 99, TimeRecalled: 1},
		{Id: "held", Significance: 5, Body: "replacement"},
	}})
	if err != nil {
		t.Fatalf("StoreMemories: %s", err)
	}

	if got := codes.Code(res.GetResults()[1].GetCode()); got != codes.AlreadyExists {
		t.Errorf("re-storing a held id reported %s, want AlreadyExists - a batch write must not upsert", got)
	}

	stored, err := s.db.GetMemoriesByIds(context.Background(), []string{"fresh", "held"})
	if err != nil {
		t.Fatalf("GetMemoriesByIds: %s", err)
	}

	for _, m := range *stored {
		switch m.Id {

		case "fresh":
			if m.RecallCount != 0 || m.TimeRecalled != 0 {
				t.Errorf("a batch create arrived pre-reinforced: recall_count=%d time_recalled=%d", m.RecallCount, m.TimeRecalled)
			}

		case "held":
			if m.Body != "original" {
				t.Errorf("the held memory's body is %q, want %q - the batch overwrote a live row", m.Body, "original")
			}

		}
	}
}

// TestStoreMemories_BatchLevelFaults covers the three things that fail the call rather than one
// record: nothing to write, more than the cap, and a caller that has gone away.
func TestStoreMemories_BatchLevelFaults(t *testing.T) {
	s := newTestServer(t)

	if _, err := s.StoreMemories(context.Background(), &contract.StoreMemoriesRequest{}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("an empty batch returned %v, want InvalidArgument", err)
	}

	oversized := make([]*contract.Memory, maxStoreMemoriesBatch+1)
	for i := range oversized {
		oversized[i] = &contract.Memory{Significance: 5, Body: fmt.Sprintf("m%d", i)}
	}

	_, err := s.StoreMemories(context.Background(), &contract.StoreMemoriesRequest{Memories: oversized})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("an oversized batch returned %v, want InvalidArgument", err)
	}

	// Refused, not truncated: a caller that sent 501 records must not have to discover which 500
	// landed.
	if with, without := s.db.CountMemories(context.Background()); with+without != 0 {
		t.Errorf("an oversized batch stored %d memories; it must store none", with+without)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := s.StoreMemories(ctx, &contract.StoreMemoriesRequest{Memories: []*contract.Memory{
		{Significance: 5, Body: "x"},
	}}); status.Code(err) != codes.Canceled {
		t.Errorf("a cancelled batch returned %v, want Canceled", err)
	}
}

// TestStoreMemories_LinksAndEvents verifies the batch carries the whole write path's reach, not a
// reduced one: a memory may name an existing event and declare links, and both are applied.
func TestStoreMemories_LinksAndEvents(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()

	if _, err := s.db.CreateEvent(ctx, types.Event{Id: "e1", Name: "trip", TimeStart: 100, Significance: 5}); err != nil {
		t.Fatalf("CreateEvent: %s", err)
	}

	if _, err := s.StoreMemory(ctx, &contract.Memory{Id: "target", Significance: 5, Body: "target"}); err != nil {
		t.Fatalf("StoreMemory: %s", err)
	}

	res, err := s.StoreMemories(ctx, &contract.StoreMemoriesRequest{Memories: []*contract.Memory{
		{Id: "linked", Significance: 5, Body: "linked", EventId: "e1", Links: []*contract.Link{{Id: "target", Significance: 5}}},
		{Id: "dangling", Significance: 5, Body: "dangling", Links: []*contract.Link{{Id: "nobody", Significance: 5}}},
	}})
	if err != nil {
		t.Fatalf("StoreMemories: %s", err)
	}

	if res.GetResults()[0].GetId() != "linked" {
		t.Fatalf("expected the linked memory stored, got %+v", res.GetResults()[0])
	}

	if got := codes.Code(res.GetResults()[1].GetCode()); got != codes.NotFound {
		t.Errorf("a link to an absent target reported %s, want NotFound", got)
	}

	links, err := s.GetMemoryLinks(ctx, &contract.GetMemoryLinksRequest{Id: "linked", Direction: contract.LinkDirection_LINK_DIRECTION_OUTBOUND})
	if err != nil {
		t.Fatalf("GetMemoryLinks: %s", err)
	}

	if len(links.GetLinks()) != 1 || links.GetLinks()[0].GetId() != "target" {
		t.Errorf("expected the batch to write the declared link, got %+v", links.GetLinks())
	}
}
