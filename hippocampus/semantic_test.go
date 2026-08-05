package hippocampus

import (
	"context"
	"errors"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/fastbean-au/hippocampus/contract"
	"github.com/fastbean-au/hippocampus/db"
	"github.com/fastbean-au/hippocampus/embed"
)

// fakeEmbedder is an embed.Embedder returning fixed vectors, so the semantic paths can be driven
// without a model server.
type fakeEmbedder struct {
	enabled bool
	err     error
	calls   [][]string
}

func (f *fakeEmbedder) Embed(ctx context.Context, texts []string) ([]embed.Vector, error) {
	f.calls = append(f.calls, texts)

	if f.err != nil {
		return nil, f.err
	}

	vectors := make([]embed.Vector, 0, len(texts))
	for range texts {
		vectors = append(vectors, embed.Vector{0.1, 0.2, 0.3})
	}

	return vectors, nil
}

func (f *fakeEmbedder) Model() string { return "fake-embed" }

func (f *fakeEmbedder) Enabled() bool { return f.enabled }

// newSemanticTestServer builds a Server with a fake index and embedder wired for semantic search.
func newSemanticTestServer(t *testing.T, idx *fakeIndex, embedder *fakeEmbedder) *Server {
	t.Helper()

	database, err := db.New("")
	if err != nil {
		t.Fatalf("db.New: %s", err)
	}

	t.Cleanup(func() { _ = database.Close() })

	return &Server{db: database, search: idx, embed: embedder}
}

// A semantic search must send the embedded query as a vector, not as text.
func TestSearchMemories_SemanticSendsAVector(t *testing.T) {
	ctx := context.Background()

	idx := &fakeIndex{enabled: true, supportsVectors: true, searchIds: []string{"m1"}}
	embedder := &fakeEmbedder{enabled: true}

	s := newSemanticTestServer(t, idx, embedder)

	if _, err := s.db.CreateMemory(ctx, testMemory("m1", 5)); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	res, err := s.SearchMemories(ctx, &contract.SearchMemoriesRequest{
		Query: "the rollout broke",
		Mode:  contract.SearchMode_SEARCH_MODE_SEMANTIC,
	})
	if err != nil {
		t.Fatalf("SearchMemories: %s", err)
	}

	if len(res.Memories) != 1 {
		t.Fatalf("got %d memories, want 1", len(res.Memories))
	}

	if len(idx.queries) != 1 {
		t.Fatalf("the index saw %d queries, want 1", len(idx.queries))
	}

	if len(idx.queries[0].Vector) == 0 {
		t.Error("the semantic query carried no vector")
	}

	if len(embedder.calls) != 1 || embedder.calls[0][0] != "the rollout broke" {
		t.Errorf("the embedder was asked for %v, want the query text", embedder.calls)
	}
}

// Hybrid must run both searches - one with a vector, one without - and fuse them.
func TestSearchMemories_HybridRunsBothSearches(t *testing.T) {
	ctx := context.Background()

	idx := &fakeIndex{enabled: true, supportsVectors: true, searchIds: []string{"m1"}}
	embedder := &fakeEmbedder{enabled: true}

	s := newSemanticTestServer(t, idx, embedder)

	if _, err := s.db.CreateMemory(ctx, testMemory("m1", 5)); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	if _, err := s.SearchMemories(ctx, &contract.SearchMemoriesRequest{
		Query: "hello",
		Mode:  contract.SearchMode_SEARCH_MODE_HYBRID,
	}); err != nil {
		t.Fatalf("SearchMemories: %s", err)
	}

	if len(idx.queries) != 2 {
		t.Fatalf("the index saw %d queries, want 2 (keyword and semantic)", len(idx.queries))
	}

	if len(idx.queries[0].Vector) != 0 {
		t.Error("the first hybrid query carried a vector; it should be the keyword half")
	}

	if len(idx.queries[1].Vector) == 0 {
		t.Error("the second hybrid query carried no vector; it should be the semantic half")
	}
}

// Keyword must remain vector-free, and an unset mode must behave exactly as keyword does - an
// existing caller sees no change.
func TestSearchMemories_KeywordAndUnspecifiedNeverEmbed(t *testing.T) {
	ctx := context.Background()

	for _, mode := range []contract.SearchMode{
		contract.SearchMode_SEARCH_MODE_UNSPECIFIED,
		contract.SearchMode_SEARCH_MODE_KEYWORD,
	} {
		idx := &fakeIndex{enabled: true, supportsVectors: true, searchIds: []string{"m1"}}
		embedder := &fakeEmbedder{enabled: true}

		s := newSemanticTestServer(t, idx, embedder)

		if _, err := s.db.CreateMemory(ctx, testMemory("m1", 5)); err != nil {
			t.Fatalf("CreateMemory: %s", err)
		}

		if _, err := s.SearchMemories(ctx, &contract.SearchMemoriesRequest{Query: "hello", Mode: mode}); err != nil {
			t.Fatalf("SearchMemories(%v): %s", mode, err)
		}

		if len(embedder.calls) != 0 {
			t.Errorf("mode %v embedded the query; it should not", mode)
		}

		if len(idx.queries) != 1 || len(idx.queries[0].Vector) != 0 {
			t.Errorf("mode %v sent a vector query", mode)
		}
	}
}

// The two ways a deployment can lack semantic search are separate misconfigurations with separate
// fixes, so they must be reported separately rather than as one vague message.
func TestSearchMemories_SemanticRefusalsAreSpecific(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name            string
		embedderEnabled bool
		supportsVectors bool
		wantSubstring   string
	}{
		{
			name:            "no embedder",
			embedderEnabled: false,
			supportsVectors: true,
			wantSubstring:   "no embedding model is configured",
		},
		{
			name:            "no vector index",
			embedderEnabled: true,
			supportsVectors: false,
			wantSubstring:   "no vector index",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			idx := &fakeIndex{enabled: true, supportsVectors: test.supportsVectors}
			embedder := &fakeEmbedder{enabled: test.embedderEnabled}

			s := newSemanticTestServer(t, idx, embedder)

			_, err := s.SearchMemories(ctx, &contract.SearchMemoriesRequest{
				Query: "hello",
				Mode:  contract.SearchMode_SEARCH_MODE_SEMANTIC,
			})

			if status.Code(err) != codes.FailedPrecondition {
				t.Fatalf("got %v, want FailedPrecondition", err)
			}

			if !strings.Contains(err.Error(), test.wantSubstring) {
				t.Errorf("error %q does not mention %q", err, test.wantSubstring)
			}
		})
	}
}

// An unreachable model server is a transient failure, not a misconfiguration, so it must be
// Unavailable rather than FailedPrecondition - the distinction a client uses to decide whether
// retrying is worthwhile.
func TestSearchMemories_EmbedderFailureIsUnavailable(t *testing.T) {
	idx := &fakeIndex{enabled: true, supportsVectors: true}
	embedder := &fakeEmbedder{enabled: true, err: errors.New("connection refused")}

	s := newSemanticTestServer(t, idx, embedder)

	_, err := s.SearchMemories(context.Background(), &contract.SearchMemoriesRequest{
		Query: "hello",
		Mode:  contract.SearchMode_SEARCH_MODE_SEMANTIC,
	})

	if status.Code(err) != codes.Unavailable {
		t.Errorf("got %v, want Unavailable", err)
	}
}

// WhoAmI reports the modes the deployment can serve, so a client can choose without probing.
func TestWhoAmI_ReportsSearchModes(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name            string
		indexEnabled    bool
		supportsVectors bool
		embedderEnabled bool
		want            []contract.SearchMode
	}{
		{
			name: "no search at all",
			want: nil,
		},
		{
			name:         "keyword only",
			indexEnabled: true,
			want:         []contract.SearchMode{contract.SearchMode_SEARCH_MODE_KEYWORD},
		},
		{
			// An embedder with nowhere to put the vectors is not semantic search.
			name:            "embedder but no vector index",
			indexEnabled:    true,
			embedderEnabled: true,
			want:            []contract.SearchMode{contract.SearchMode_SEARCH_MODE_KEYWORD},
		},
		{
			// A vector index with nothing to fill it is not semantic search either.
			name:            "vector index but no embedder",
			indexEnabled:    true,
			supportsVectors: true,
			want:            []contract.SearchMode{contract.SearchMode_SEARCH_MODE_KEYWORD},
		},
		{
			name:            "both halves",
			indexEnabled:    true,
			supportsVectors: true,
			embedderEnabled: true,
			want: []contract.SearchMode{
				contract.SearchMode_SEARCH_MODE_KEYWORD,
				contract.SearchMode_SEARCH_MODE_SEMANTIC,
				contract.SearchMode_SEARCH_MODE_HYBRID,
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			idx := &fakeIndex{enabled: test.indexEnabled, supportsVectors: test.supportsVectors}
			embedder := &fakeEmbedder{enabled: test.embedderEnabled}

			s := newSemanticTestServer(t, idx, embedder)

			res, err := s.WhoAmI(ctx, &contract.EmptyRequest{})
			if err != nil {
				t.Fatalf("WhoAmI: %s", err)
			}

			if len(res.SearchModes) != len(test.want) {
				t.Fatalf("got %v, want %v", res.SearchModes, test.want)
			}

			for i := range test.want {
				if res.SearchModes[i] != test.want[i] {
					t.Errorf("got %v, want %v", res.SearchModes, test.want)

					break
				}
			}
		})
	}
}

// Storing a memory must attach its embedding to the indexed document, or it is stored but not
// findable by meaning.
func TestStoreMemory_IndexesWithAVector(t *testing.T) {
	ctx := context.Background()

	idx := &fakeIndex{enabled: true, supportsVectors: true}
	embedder := &fakeEmbedder{enabled: true}

	s := newSemanticTestServer(t, idx, embedder)

	if _, err := s.StoreMemory(ctx, &contract.Memory{Body: "something worth remembering", Significance: 5, TimeStamp: 100}); err != nil {
		t.Fatalf("StoreMemory: %s", err)
	}

	if len(idx.docs) != 1 {
		t.Fatalf("indexed %d documents, want 1", len(idx.docs))
	}

	if len(idx.docs[0].Vector) == 0 {
		t.Error("the indexed document carried no vector")
	}
}

// A binary body is opaque, so embedding it would describe its encoding rather than its content.
func TestStoreMemory_DoesNotEmbedBinaryBodies(t *testing.T) {
	ctx := context.Background()

	idx := &fakeIndex{enabled: true, supportsVectors: true}
	embedder := &fakeEmbedder{enabled: true}

	s := newSemanticTestServer(t, idx, embedder)

	if _, err := s.StoreMemory(ctx, &contract.Memory{
		Body:         "AAAA",
		Significance: 5,
		TimeStamp:    100,
		IsBinary:     contract.Bool_TRUE,
	}); err != nil {
		t.Fatalf("StoreMemory: %s", err)
	}

	if len(embedder.calls) != 0 {
		t.Errorf("a binary body was embedded: %v", embedder.calls)
	}
}

// An unreachable model server must cost the vector, never the memory.
func TestStoreMemory_SurvivesAnEmbedderFailure(t *testing.T) {
	ctx := context.Background()

	idx := &fakeIndex{enabled: true, supportsVectors: true}
	embedder := &fakeEmbedder{enabled: true, err: errors.New("connection refused")}

	s := newSemanticTestServer(t, idx, embedder)

	res, err := s.StoreMemory(ctx, &contract.Memory{Body: "still worth keeping", Significance: 5, TimeStamp: 100})
	if err != nil {
		t.Fatalf("StoreMemory failed because the embedder did: %s", err)
	}

	if res.Id == "" {
		t.Fatal("StoreMemory returned no id")
	}

	// The memory is stored...
	stored, err := s.db.GetMemoriesByIds(ctx, []string{res.Id})
	if err != nil || len(*stored) != 1 {
		t.Fatalf("the memory was not stored: %v", err)
	}

	// ...and indexed, just without a vector.
	if len(idx.docs) != 1 {
		t.Fatalf("indexed %d documents, want 1", len(idx.docs))
	}

	if len(idx.docs[0].Vector) != 0 {
		t.Error("a vector was indexed despite the embedder failing")
	}
}

// Re-indexing without a vector would replace a document that had one, silently removing that
// memory from semantic search. The reconcile sweep is the path most likely to do it.
func TestReconcile_KeepsVectorsOnReindexedMemories(t *testing.T) {
	ctx := context.Background()

	idx := &fakeIndex{enabled: true, supportsVectors: true}
	embedder := &fakeEmbedder{enabled: true}

	s := newSemanticTestServer(t, idx, embedder)
	s.reconcileBatchSize = 10
	s.stopReconcile = make(chan struct{})

	if _, err := s.db.CreateMemory(ctx, testMemory("m1", 5)); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	s.reconcileOnce()

	if len(idx.docs) != 1 {
		t.Fatalf("the sweep indexed %d documents, want 1", len(idx.docs))
	}

	if len(idx.docs[0].Vector) == 0 {
		t.Error("the sweep re-indexed a memory without its vector, which would strip it from semantic search")
	}
}
