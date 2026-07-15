package search

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"
)

// opensearchTestURLEnv names the environment variable carrying the URL of a disposable
// single-node OpenSearch (security disabled) for the integration tests below. When unset, those
// tests skip: they need a real cluster, unlike the queue tests' fake transport.
const opensearchTestURLEnv = "HIPPOCAMPUS_TEST_OPENSEARCH_URL"

// newIntegrationIndex connects to the cluster named by HIPPOCAMPUS_TEST_OPENSEARCH_URL with a
// uniquely named index, deleting the index when the test ends. Skips when the variable is unset.
//
// Tests exercise the synchronous apply() path directly (queue mechanics are unit-tested against
// the fake transport) and refresh explicitly before searching, so nothing depends on OpenSearch's
// near-real-time refresh interval.
func newIntegrationIndex(t *testing.T) *OpenSearch {
	t.Helper()

	url := os.Getenv(opensearchTestURLEnv)
	if url == "" {
		t.Skipf("set %s to run opensearch integration tests", opensearchTestURLEnv)
	}

	idx, err := NewOpenSearch(Config{
		Addresses: []string{url},
		Index:     fmt.Sprintf("hippocampus-test-%d", time.Now().UnixNano()),
		QueueSize: 16,
	})
	if err != nil {
		t.Fatalf("NewOpenSearch: %s", err)
	}

	if !idx.indexReady.Load() {
		t.Fatalf("index bootstrap failed - is OpenSearch reachable at %s?", url)
	}

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		_, _ = idx.client.Indices.Delete(ctx, opensearchapi.IndicesDeleteReq{Indices: []string{idx.index}})
		_ = idx.Close()
	})

	return idx
}

// mustApply applies an operation synchronously, failing the test on error.
func mustApply(t *testing.T, idx *OpenSearch, v op) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := idx.apply(ctx, v); err != nil {
		t.Fatalf("apply(%s): %s", v.kind, err)
	}
}

// mustSearch refreshes the index then searches, failing the test on error.
func mustSearch(t *testing.T, idx *OpenSearch, query Query) []string {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := idx.refresh(ctx); err != nil {
		t.Fatalf("refresh: %s", err)
	}

	ids, err := idx.Search(ctx, query)
	if err != nil {
		t.Fatalf("Search: %s", err)
	}

	return ids
}

// TestOpenSearchIntegration_MappingBootstrap verifies ensureIndex created the index with the
// explicit mapping - event_id must be a keyword field, or the term filters would misbehave.
func TestOpenSearchIntegration_MappingBootstrap(t *testing.T) {
	idx := newIntegrationIndex(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	resp, err := idx.client.Indices.Mapping.Get(ctx, &opensearchapi.MappingGetReq{Indices: []string{idx.index}})
	if err != nil {
		t.Fatalf("Mapping.Get: %s", err)
	}

	index, ok := resp.Indices[idx.index]
	if !ok {
		t.Fatalf("no mapping returned for index '%s'", idx.index)
	}

	var mappings struct {
		Properties map[string]struct {
			Type string `json:"type"`
		} `json:"properties"`
	}

	if err := json.Unmarshal(index.Mappings, &mappings); err != nil {
		t.Fatalf("failed to decode mapping: %s", err)
	}

	if mappings.Properties["event_id"].Type != "keyword" {
		t.Errorf("event_id should be mapped as keyword, got %+v", mappings.Properties["event_id"])
	}

	if mappings.Properties["group"].Type != "keyword" {
		t.Errorf("group should be mapped as keyword, got %+v", mappings.Properties["group"])
	}
}

// TestOpenSearchIntegration_RoundTrip covers index -> search with relevance ordering, the
// event_id filter, and delete-by-id.
func TestOpenSearchIntegration_RoundTrip(t *testing.T) {
	idx := newIntegrationIndex(t)

	mustApply(t, idx, op{kind: opIndex, doc: Doc{Id: "m1", Body: "the quick brown fox", EventId: "e1", Significance: 5, Timestamp: 100, Group: "g1"}})
	mustApply(t, idx, op{kind: opIndex, doc: Doc{Id: "m2", Body: "fox fox fox everywhere", EventId: "e2", Significance: 5, Timestamp: 200, Group: "g2"}})
	mustApply(t, idx, op{kind: opIndex, doc: Doc{Id: "m3", Body: "nothing relevant here", EventId: "e1", Significance: 5, Timestamp: 300}})

	ids := mustSearch(t, idx, Query{Text: "fox", Limit: 10})

	if len(ids) != 2 {
		t.Fatalf("expected 2 matches for 'fox', got %v", ids)
	}

	if ids[0] != "m2" {
		t.Errorf("expected m2 (three foxes) to rank first, got %v", ids)
	}

	// The event filter must exclude m2 even though it matches better.
	ids = mustSearch(t, idx, Query{Text: "fox", EventId: "e1", Limit: 10})

	if len(ids) != 1 || ids[0] != "m1" {
		t.Errorf("expected only m1 within event e1, got %v", ids)
	}

	// The group filter likewise.
	ids = mustSearch(t, idx, Query{Text: "fox", Group: "g1", Limit: 10})

	if len(ids) != 1 || ids[0] != "m1" {
		t.Errorf("expected only m1 within group g1, got %v", ids)
	}

	// Delete-by-id, including an id that was never indexed (must not error).
	mustApply(t, idx, op{kind: opDeleteIds, ids: []string{"m2", "never-indexed"}})

	ids = mustSearch(t, idx, Query{Text: "fox", Limit: 10})

	if len(ids) != 1 || ids[0] != "m1" {
		t.Errorf("expected only m1 after deleting m2, got %v", ids)
	}
}

// TestOpenSearchIntegration_DeleteByEventSeesUnrefreshedDocs verifies the forced refresh inside
// the event-scoped delete: documents indexed moments earlier must not survive it.
func TestOpenSearchIntegration_DeleteByEventSeesUnrefreshedDocs(t *testing.T) {
	idx := newIntegrationIndex(t)

	mustApply(t, idx, op{kind: opIndex, doc: Doc{Id: "m1", Body: "memory one", EventId: "e1"}})
	mustApply(t, idx, op{kind: opIndex, doc: Doc{Id: "m2", Body: "memory two", EventId: "e1"}})
	mustApply(t, idx, op{kind: opIndex, doc: Doc{Id: "m3", Body: "memory three", EventId: "e2"}})

	// No explicit refresh here - apply must handle it itself.
	mustApply(t, idx, op{kind: opDeleteByEvent, eventId: "e1"})

	ids := mustSearch(t, idx, Query{Text: "memory", Limit: 10})

	if len(ids) != 1 || ids[0] != "m3" {
		t.Errorf("expected only m3 (other event) to survive, got %v", ids)
	}
}

// TestOpenSearchIntegration_SetEventId verifies the update-by-query event rewrite, including
// detaching (empty toEventId).
func TestOpenSearchIntegration_SetEventId(t *testing.T) {
	idx := newIntegrationIndex(t)

	mustApply(t, idx, op{kind: opIndex, doc: Doc{Id: "m1", Body: "merged memory", EventId: "from"}})

	mustApply(t, idx, op{kind: opSetEventId, eventId: "from", toEventId: "to"})

	if ids := mustSearch(t, idx, Query{Text: "merged", EventId: "to", Limit: 10}); len(ids) != 1 {
		t.Errorf("expected m1 under event 'to' after the merge, got %v", ids)
	}

	if ids := mustSearch(t, idx, Query{Text: "merged", EventId: "from", Limit: 10}); len(ids) != 0 {
		t.Errorf("expected nothing left under event 'from', got %v", ids)
	}

	mustApply(t, idx, op{kind: opSetEventId, eventId: "to", toEventId: ""})

	if ids := mustSearch(t, idx, Query{Text: "merged", Limit: 10}); len(ids) != 1 {
		t.Errorf("detached memory should still match without an event filter, got %v", ids)
	}
}

// TestOpenSearchIntegration_BackfillSyncPath covers the backfill tool's cluster surface:
// synchronous idempotent index writes (re-indexing an id overwrites, never duplicates) and
// RecreateIndex leaving an empty but usable index.
func TestOpenSearchIntegration_BackfillSyncPath(t *testing.T) {
	idx := newIntegrationIndex(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := idx.IndexMemorySync(ctx, Doc{Id: "m1", Body: "backfilled memory", EventId: "e1"}); err != nil {
		t.Fatalf("IndexMemorySync(m1): %s", err)
	}

	if err := idx.IndexMemorySync(ctx, Doc{Id: "m2", Body: "another backfilled memory"}); err != nil {
		t.Fatalf("IndexMemorySync(m2): %s", err)
	}

	// A rerun re-indexes the same id; it must overwrite the document, not duplicate it.
	if err := idx.IndexMemorySync(ctx, Doc{Id: "m1", Body: "backfilled memory", EventId: "e1"}); err != nil {
		t.Fatalf("IndexMemorySync(m1, rerun): %s", err)
	}

	if ids := mustSearch(t, idx, Query{Text: "backfilled", Limit: 10}); len(ids) != 2 {
		t.Errorf("expected 2 documents after an idempotent rerun, got %v", ids)
	}

	if err := idx.RecreateIndex(ctx); err != nil {
		t.Fatalf("RecreateIndex: %s", err)
	}

	if ids := mustSearch(t, idx, Query{Text: "backfilled", Limit: 10}); len(ids) != 0 {
		t.Errorf("expected an empty index after RecreateIndex, got %v", ids)
	}

	if err := idx.IndexMemorySync(ctx, Doc{Id: "m3", Body: "fresh after reindex"}); err != nil {
		t.Fatalf("IndexMemorySync(m3): %s", err)
	}

	if ids := mustSearch(t, idx, Query{Text: "fresh", Limit: 10}); len(ids) != 1 || ids[0] != "m3" {
		t.Errorf("expected m3 indexed after RecreateIndex, got %v", ids)
	}
}

// TestOpenSearchIntegration_Purge verifies purge leaves an empty but usable index.
func TestOpenSearchIntegration_Purge(t *testing.T) {
	idx := newIntegrationIndex(t)

	mustApply(t, idx, op{kind: opIndex, doc: Doc{Id: "m1", Body: "soon gone"}})

	mustApply(t, idx, op{kind: opPurge})

	if ids := mustSearch(t, idx, Query{Text: "soon", Limit: 10}); len(ids) != 0 {
		t.Errorf("expected an empty index after purge, got %v", ids)
	}

	// The index must be immediately usable again.
	mustApply(t, idx, op{kind: opIndex, doc: Doc{Id: "m2", Body: "after the purge"}})

	if ids := mustSearch(t, idx, Query{Text: "purge", Limit: 10}); len(ids) != 1 || ids[0] != "m2" {
		t.Errorf("expected m2 indexed after purge, got %v", ids)
	}
}
