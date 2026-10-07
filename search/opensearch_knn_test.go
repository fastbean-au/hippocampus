package search

import (
	"context"
	"fmt"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"
)

// Real-cluster coverage for the parts of the OpenSearch backend the unit tests can only fake (TODO-3
// item 177): whether an existing index carries the vector field, a k-NN query against a real k-NN
// index, and the stale sweep's enumeration when many documents share one timestamp. Like the other
// integration tests here they skip unless HIPPOCAMPUS_TEST_OPENSEARCH_URL is set, which CI does.

// openIntegrationIndex connects to a named index with the given vector dimension (0 for none), and
// deletes the index at the end of the test. Two calls with one name share an index, which is how an
// index created before semantic search was configured is reproduced.
func openIntegrationIndex(t *testing.T, name string, dimension int) *OpenSearch {
	t.Helper()

	url := os.Getenv(opensearchTestURLEnv)
	if url == "" {
		t.Skipf("set %s to run opensearch integration tests", opensearchTestURLEnv)
	}

	idx, err := NewOpenSearch(Config{
		Addresses:       []string{url},
		Index:           name,
		QueueSize:       16,
		VectorDimension: dimension,
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

// TestOpenSearchIntegration_CheckVectorField runs checkVectorField against real mappings: an index
// created with a dimension carries the field, and an index created WITHOUT one does not gain it when
// a later process configures a dimension - index.knn is fixed at creation - so semantic search must
// report itself unavailable there rather than fail every query at the cluster.
func TestOpenSearchIntegration_CheckVectorField(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	withVector := openIntegrationIndex(t, fmt.Sprintf("hippocampus-test-knn-%d", time.Now().UnixNano()), 4)

	withVector.checkVectorField(ctx)

	if !withVector.vectorReady.Load() {
		t.Error("an index created with a dimension must report its vector field")
	}

	name := fmt.Sprintf("hippocampus-test-preknn-%d", time.Now().UnixNano())

	_ = openIntegrationIndex(t, name, 0)
	upgraded := openIntegrationIndex(t, name, 4)

	if upgraded.vectorReady.Load() {
		t.Error("an index created without a vector field must not report one after a dimension is configured")
	}

	upgraded.checkVectorField(ctx)

	if upgraded.vectorReady.Load() {
		t.Error("checkVectorField must find no vector field on the pre-existing index")
	}
}

// TestOpenSearchIntegration_KNNRoundTrip indexes documents with vectors and checks a vector query
// returns them nearest first, and that the filters still apply to a vector query.
func TestOpenSearchIntegration_KNNRoundTrip(t *testing.T) {
	idx := openIntegrationIndex(t, fmt.Sprintf("hippocampus-test-knnrt-%d", time.Now().UnixNano()), 4)

	mustApply(t, idx, op{kind: opIndex, doc: Doc{Id: "north", Body: "a", Timestamp: 100, Group: "g1", Vector: []float32{1, 0, 0, 0}}})
	mustApply(t, idx, op{kind: opIndex, doc: Doc{Id: "north-ish", Body: "b", Timestamp: 200, Group: "g2", Vector: []float32{0.9, 0.1, 0, 0}}})
	mustApply(t, idx, op{kind: opIndex, doc: Doc{Id: "east", Body: "c", Timestamp: 300, Group: "g1", Vector: []float32{0, 1, 0, 0}}})

	ids := mustSearch(t, idx, Query{Vector: []float32{1, 0, 0, 0}, Limit: 3})

	if len(ids) < 2 || ids[0] != "north" || ids[1] != "north-ish" {
		t.Errorf("expected the nearest vectors first, got %v", ids)
	}

	ids = mustSearch(t, idx, Query{Vector: []float32{1, 0, 0, 0}, Group: "g1", Limit: 3})

	for _, id := range ids {
		if id == "north-ish" {
			t.Errorf("the group filter must apply to a vector query, got %v", ids)
		}
	}
}

// TestOpenSearchIntegration_EnumerationAcrossCollidingTimestamps walks an index in which most
// documents share one timestamp, deleting part of each page as the stale sweep does, and checks the
// walk sees every document exactly once. The page size is smaller than the colliding group, so the
// walk must cross that instant through Partial pages and the caller's offset adjustment - the path
// the cursor design exists for and that no test had run against a cluster.
func TestOpenSearchIntegration_EnumerationAcrossCollidingTimestamps(t *testing.T) {
	idx := openIntegrationIndex(t, fmt.Sprintf("hippocampus-test-enum-%d", time.Now().UnixNano()), 0)

	var want []string

	for i := range 3 {
		id := fmt.Sprintf("early-%d", i)
		want = append(want, id)

		mustApply(t, idx, op{kind: opIndex, doc: Doc{Id: id, Body: "x", Timestamp: int64(100 + i)}})
	}

	for i := range 12 {
		id := fmt.Sprintf("same-%02d", i)
		want = append(want, id)

		mustApply(t, idx, op{kind: opIndex, doc: Doc{Id: id, Body: "x", Timestamp: 500}})
	}

	for i := range 3 {
		id := fmt.Sprintf("late-%d", i)
		want = append(want, id)

		mustApply(t, idx, op{kind: opIndex, doc: Doc{Id: id, Body: "x", Timestamp: int64(900 + i)}})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	seen := map[string]int{}
	cursor := IndexCursor{}

	for pages := 0; ; pages++ {
		if pages > 50 {
			t.Fatal("the walk did not finish within 50 pages")
		}

		if err := idx.refresh(ctx); err != nil {
			t.Fatalf("refresh: %s", err)
		}

		page, err := idx.EnumerateIdsPage(ctx, cursor, 5)
		if err != nil {
			t.Fatalf("EnumerateIdsPage: %s", err)
		}

		if page.Done {
			break
		}

		// Delete every other id, as a sweep deletes what the store no longer holds.
		var deleted []string

		for i, id := range page.Ids {
			seen[id]++

			if i%2 == 0 {
				deleted = append(deleted, id)
			}
		}

		if len(deleted) > 0 {
			mustApply(t, idx, op{kind: opDeleteIds, ids: deleted})
		}

		cursor = page.Next

		if page.Partial {
			cursor.Offset -= len(deleted)
		}
	}

	var got []string

	for id, count := range seen {
		got = append(got, id)

		if count != 1 {
			t.Errorf("%s was enumerated %d times", id, count)
		}
	}

	sort.Strings(got)
	sort.Strings(want)

	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("the walk saw %v, want every document once: %v", got, want)
	}
}
