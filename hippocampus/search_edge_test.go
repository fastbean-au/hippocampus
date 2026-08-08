package hippocampus

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/fastbean-au/hippocampus/contract"
	"github.com/fastbean-au/hippocampus/db"
	"github.com/fastbean-au/hippocampus/embed"
	"github.com/fastbean-au/hippocampus/types"
)

// shortEmbedder is an enabled embedder that returns fewer vectors than it was given texts, standing
// in for a model server that answered without actually embedding anything. Both embedQuery and
// embedBody guard against it, and they treat it very differently - one refuses, one shrugs.
type shortEmbedder struct{}

func (shortEmbedder) Embed(ctx context.Context, texts []string) ([]embed.Vector, error) {
	return nil, nil
}

func (shortEmbedder) Model() string { return "short" }

func (shortEmbedder) Enabled() bool { return true }

// recallErrStore fails the recall a reinforcing search runs over the memories it is about to
// return.
type recallErrStore struct {
	db.Store
}

func (recallErrStore) RecallMemories(ctx context.Context, ids []string) (*[]types.Memory, error) {
	return nil, errLink
}

// vanishingRecallStore returns fewer memories than were asked for, standing in for a memory deleted
// between the ranking read and the recall.
type vanishingRecallStore struct {
	db.Store
}

func (vanishingRecallStore) RecallMemories(ctx context.Context, ids []string) (*[]types.Memory, error) {
	return &[]types.Memory{}, nil
}

// TestSearchMemories_ReinforceFailureIsReported pins the one place a search's reinforcement is not
// best-effort: reinforceRanked rebuilds the response from the recalled rows, so if the recall fails
// there is nothing honest to return.
func TestSearchMemories_ReinforceFailureIsReported(t *testing.T) {
	idx := &fakeIndex{enabled: true, searchIds: []string{"m1"}}
	s := newSearchTestServer(t, idx)

	if _, err := s.db.CreateMemory(context.Background(), testMemory("m1", 5)); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	s.db = recallErrStore{Store: s.db}

	req := &contract.SearchMemoriesRequest{Query: "hello", Reinforce: true}

	_, err := s.SearchMemories(context.Background(), req)
	if err == nil {
		t.Fatal("expected the recall failure to reach the caller")
	}

	if got := status.Code(err); got != codes.Internal {
		t.Errorf("expected codes.Internal, got %s (%v)", got, err)
	}
}

// TestSearchMemories_VanishedMemoryIsDropped pins reinforceRanked's other branch: a memory that
// disappeared between the ranking read and the recall is dropped rather than returned with a stale
// body, because RecallMemories only returns what it actually reinforced.
func TestSearchMemories_VanishedMemoryIsDropped(t *testing.T) {
	idx := &fakeIndex{enabled: true, searchIds: []string{"m1"}}
	s := newSearchTestServer(t, idx)

	if _, err := s.db.CreateMemory(context.Background(), testMemory("m1", 5)); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	s.db = vanishingRecallStore{Store: s.db}

	req := &contract.SearchMemoriesRequest{Query: "hello", Reinforce: true}

	res, err := s.SearchMemories(context.Background(), req)
	if err != nil {
		t.Fatalf("SearchMemories: %s", err)
	}

	if len(res.GetMemories()) != 0 {
		t.Errorf("expected the vanished memory to be dropped, got %+v", res.GetMemories())
	}
}

// TestSearchMemories_IncludeLinked covers associative retrieval on the search path: neighbours are
// appended after the ranked matches, are not counted as matches, and are not reinforced.
func TestSearchMemories_IncludeLinked(t *testing.T) {
	idx := &fakeIndex{enabled: true, searchIds: []string{"m1"}}
	s := newSearchTestServer(t, idx)

	for _, id := range []string{"m1", "m2"} {
		if _, err := s.db.CreateMemory(context.Background(), testMemory(id, 5)); err != nil {
			t.Fatalf("CreateMemory(%s): %s", id, err)
		}
	}

	link := &contract.LinkMemoriesRequest{Id: "m1", Links: []*contract.Link{{Id: "m2", Significance: 1}}}
	if _, err := s.LinkMemories(context.Background(), link); err != nil {
		t.Fatalf("LinkMemories: %s", err)
	}

	req := &contract.SearchMemoriesRequest{Query: "hello", IncludeLinked: true}

	res, err := s.SearchMemories(context.Background(), req)
	if err != nil {
		t.Fatalf("SearchMemories: %s", err)
	}

	if len(res.GetMemories()) != 2 {
		t.Fatalf("expected the match plus its neighbour, got %d", len(res.GetMemories()))
	}

	if res.GetMemories()[0].GetId() != "m1" || res.GetMemories()[1].GetId() != "m2" {
		t.Errorf("expected the neighbour appended after the match, got %+v", res.GetMemories())
	}

	if got := res.GetMemories()[1].GetRecallCount(); got != 0 {
		t.Errorf("expected the linked memory not to be reinforced, recall count %d", got)
	}
}

// TestSearchMemories_EmbedFailuresPropagate walks the embedQuery call in each mode that makes one.
// A semantic or hybrid search cannot fall back to keyword: the caller asked for a different kind of
// matching, and silently giving them another would be worse than refusing.
func TestSearchMemories_EmbedFailuresPropagate(t *testing.T) {
	modes := []struct {
		name string
		mode contract.SearchMode
	}{
		{name: "semantic", mode: contract.SearchMode_SEARCH_MODE_SEMANTIC},
		{name: "hybrid", mode: contract.SearchMode_SEARCH_MODE_HYBRID},
	}

	for _, m := range modes {
		t.Run(m.name+" no embedder", func(t *testing.T) {
			idx := &fakeIndex{enabled: true, supportsVectors: true}
			s := newSemanticTestServer(t, idx, &fakeEmbedder{enabled: false})

			req := &contract.SearchMemoriesRequest{Query: "hello", Mode: m.mode}

			_, err := s.SearchMemories(context.Background(), req)
			if err == nil {
				t.Fatal("expected the search to be refused without an embedder")
			}

			if got := status.Code(err); got != codes.FailedPrecondition {
				t.Errorf("expected codes.FailedPrecondition, got %s (%v)", got, err)
			}
		})

		t.Run(m.name+" embedder returns no vector", func(t *testing.T) {
			idx := &fakeIndex{enabled: true, supportsVectors: true}
			s := newSemanticTestServer(t, idx, nil)
			s.embed = shortEmbedder{}

			req := &contract.SearchMemoriesRequest{Query: "hello", Mode: m.mode}

			_, err := s.SearchMemories(context.Background(), req)
			if err == nil {
				t.Fatal("expected a vectorless answer to be refused")
			}

			if got := status.Code(err); got != codes.Internal {
				t.Errorf("expected codes.Internal, got %s (%v)", got, err)
			}
		})
	}
}

// TestSearchMemories_HybridIndexFailuresPropagate covers the two index calls hybrid makes: a
// failure in either half must surface rather than silently degrading to the half that worked.
func TestSearchMemories_HybridIndexFailuresPropagate(t *testing.T) {
	idx := &fakeIndex{enabled: true, supportsVectors: true, searchErr: errLink}
	s := newSemanticTestServer(t, idx, &fakeEmbedder{enabled: true})

	req := &contract.SearchMemoriesRequest{Query: "hello", Mode: contract.SearchMode_SEARCH_MODE_HYBRID}

	_, err := s.SearchMemories(context.Background(), req)
	if err == nil {
		t.Fatal("expected the index failure to reach the caller")
	}

	if got := status.Code(err); got != codes.Internal {
		t.Errorf("expected codes.Internal, got %s (%v)", got, err)
	}
}

// TestEmbedBody_SkipsWhenUnusable pins embedBody's deliberate asymmetry with embedQuery: it reports
// failure rather than returning it, because no caller can act on it - the memory is already stored,
// the index is best-effort, and a rebuild can supply the vector later.
func TestEmbedBody_SkipsWhenUnusable(t *testing.T) {
	memory := types.Memory{Id: "m1", Body: "some text"}

	t.Run("no vector index", func(t *testing.T) {
		idx := &fakeIndex{enabled: true, supportsVectors: false}
		s := newSemanticTestServer(t, idx, &fakeEmbedder{enabled: true})

		if _, ok := s.embedBody(context.Background(), memory); ok {
			t.Error("expected no vector when the backend has no vector index")
		}
	})

	t.Run("embedder returns no vector", func(t *testing.T) {
		idx := &fakeIndex{enabled: true, supportsVectors: true}
		s := newSemanticTestServer(t, idx, nil)
		s.embed = shortEmbedder{}

		if _, ok := s.embedBody(context.Background(), memory); ok {
			t.Error("expected no vector when the model answered without one")
		}
	})
}

// TestCheckLinkTargets_InvalidLinkSet covers checkLinkTargets' own validation branch directly. The
// create RPCs reach it only after types.ValidateMemory/ValidateEvent have already rejected a bad
// link set, so this guard is the one that holds if a future caller arrives by another route.
func TestCheckLinkTargets_InvalidLinkSet(t *testing.T) {
	s := newTestServer(t)

	links := []types.Link{
		{Id: "target", Significance: 1},
		{Id: "target", Significance: 2},
	}

	err := s.checkLinkTargets(context.Background(), s.memoryLinks(), "fresh", links)
	if err == nil {
		t.Fatal("expected a duplicated link target to be rejected")
	}

	if got := status.Code(err); got != codes.InvalidArgument {
		t.Errorf("expected codes.InvalidArgument, got %s (%v)", got, err)
	}
}
