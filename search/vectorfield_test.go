package search

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"
)

// newVectorIntegrationIndex is newIntegrationIndex with a vector dimension configured, so the index
// is created with the k-NN field.
func newVectorIntegrationIndex(t *testing.T, dimension int) *OpenSearch {
	t.Helper()

	url := os.Getenv(opensearchTestURLEnv)
	if url == "" {
		t.Skipf("set %s to run opensearch integration tests", opensearchTestURLEnv)
	}

	idx, err := NewOpenSearch(Config{
		Addresses:       []string{url},
		Index:           fmt.Sprintf("hippocampus-vec-test-%d", time.Now().UnixNano()),
		QueueSize:       16,
		VectorDimension: dimension,
	})
	if err != nil {
		t.Fatalf("NewOpenSearch: %s", err)
	}

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		_, _ = idx.client.Indices.Delete(ctx, opensearchapi.IndicesDeleteReq{Indices: []string{idx.index}})
		_ = idx.Close()
	})

	return idx
}

// TestCheckVectorField_PresentOnAFreshIndex pins the happy case: an index created while a dimension
// was configured carries the k-NN field, so semantic search is available.
func TestCheckVectorField_PresentOnAFreshIndex(t *testing.T) {
	idx := newVectorIntegrationIndex(t, 8)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	idx.checkVectorField(ctx)

	if !idx.vectorReady.Load() {
		t.Error("expected a freshly created index to carry the vector field")
	}

	if !idx.SupportsVectors() {
		t.Error("expected SupportsVectors to follow the mapping probe")
	}
}

// TestCheckVectorField_AbsentOnAPreSemanticIndex is the trap this probe exists for: index.knn is a
// static setting, so an index created before semantic search was configured cannot gain the field
// by any in-place update. Discovering that at query time would mean a cluster error per search;
// discovering it here means one log line at startup naming --backfill-search --reindex as the fix.
func TestCheckVectorField_AbsentOnAPreSemanticIndex(t *testing.T) {
	// An index created with no dimension, exactly as a deployment that predates semantic search has.
	existing := newIntegrationIndex(t)

	// Now point a vector-configured client at that same index, as enabling the embedder would.
	url := os.Getenv(opensearchTestURLEnv)

	idx, err := NewOpenSearch(Config{
		Addresses:       []string{url},
		Index:           existing.index,
		QueueSize:       16,
		VectorDimension: 8,
	})
	if err != nil {
		t.Fatalf("NewOpenSearch: %s", err)
	}
	t.Cleanup(func() { _ = idx.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	idx.checkVectorField(ctx)

	if idx.vectorReady.Load() {
		t.Error("expected an index predating semantic search to report no vector field")
	}

	if idx.SupportsVectors() {
		t.Error("expected SupportsVectors to be false so the RPC layer refuses clearly")
	}
}

// TestCheckVectorField_UnreadableMappingNeverFailsStartup pins the other half of the contract: a
// cluster that is unreachable or slow at boot must not stop the service, and the same check runs
// again on the next ensureIndex.
func TestCheckVectorField_UnreadableMappingNeverFailsStartup(t *testing.T) {
	idx := newVectorIntegrationIndex(t, 8)

	// A cancelled context stands in for the cluster being unreachable.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	idx.checkVectorField(ctx)

	// No panic and no failure: the probe simply leaves the previous answer in place.
}

// TestCheckVectorField_DeletedIndexLeavesTheAnswerAlone pins the failure direction. The cluster
// reports a deleted index as an error rather than an empty mapping, so this goes down the same path
// an unreachable cluster does - and the important property is what it does NOT do: a transient
// failure must not silently flip semantic search off for a deployment that has it.
func TestCheckVectorField_DeletedIndexLeavesTheAnswerAlone(t *testing.T) {
	idx := newVectorIntegrationIndex(t, 8)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	idx.checkVectorField(ctx)

	if !idx.vectorReady.Load() {
		t.Fatal("expected the fresh index to report the vector field")
	}

	if _, err := idx.client.Indices.Delete(ctx, opensearchapi.IndicesDeleteReq{Indices: []string{idx.index}}); err != nil {
		t.Fatalf("Indices.Delete: %s", err)
	}

	idx.checkVectorField(ctx)

	if !idx.vectorReady.Load() {
		t.Error("expected a failed probe to leave the previous answer in place, not disable vectors")
	}
}

// TestSearch_MetadataFilter covers the metadata predicate in the query builder. It is applied
// inside the index rather than to the results, so limit still means what it says.
func TestSearch_MetadataFilter(t *testing.T) {
	idx := newIntegrationIndex(t)

	docs := []Doc{
		{Id: "m1", Body: "shared body text", Metadata: []string{"source=slack"}},
		{Id: "m2", Body: "shared body text", Metadata: []string{"source=email"}},
	}

	for _, doc := range docs {
		mustApply(t, idx, op{kind: opIndex, doc: doc})
	}

	got := mustSearch(t, idx, Query{
		Text:     "shared",
		Limit:    10,
		Metadata: map[string]string{"source": "slack"},
	})

	if len(got) != 1 || got[0] != "m1" {
		t.Errorf("expected only the slack-sourced memory, got %+v", got)
	}
}
