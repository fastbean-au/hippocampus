package search

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"
)

// buildIndexPage and hitTimestamp are the enumeration's decisions, and they are pure - so they are
// tested here without a cluster, unlike the query itself (opensearch_reconcile_test.go, which skips
// without one). CI has no OpenSearch, which is exactly why the reasoning worth pinning lives here.

// hitAt builds a search hit carrying an id and a timestamp doc-value, in the wire shape OpenSearch
// returns.
func hitAt(id string, timestamp int64) opensearchapi.SearchHit {
	fields, err := json.Marshal(map[string]any{"timestamp": []int64{timestamp}})
	if err != nil {
		panic(err)
	}

	return opensearchapi.SearchHit{ID: id, Fields: fields}
}

func TestBuildIndexPage(t *testing.T) {
	tests := []struct {
		name        string
		cursor      IndexCursor
		hits        []opensearchapi.SearchHit
		size        int
		wantIds     []string
		wantNext    IndexCursor
		wantPartial bool
	}{
		{
			// The ordinary case. The trailing group at 300 is held back rather than returned,
			// because a page that ends mid-group leaves a cursor the caller's own deletions can
			// shift - and the next page starts at the following instant, which nothing it deleted
			// can reach.
			name:     "a full page ends on a timestamp boundary",
			hits:     []opensearchapi.SearchHit{hitAt("a", 100), hitAt("b", 200), hitAt("c", 300), hitAt("d", 300)},
			size:     4,
			wantIds:  []string{"a", "b"},
			wantNext: IndexCursor{Timestamp: 201},
		},
		{
			// Nothing is held back: a short page means the query exhausted what matched, so every
			// document at the closing timestamp is already here and there is no group still to come.
			name:     "a short page keeps its trailing group",
			hits:     []opensearchapi.SearchHit{hitAt("a", 100), hitAt("b", 200), hitAt("c", 200)},
			size:     10,
			wantIds:  []string{"a", "b", "c"},
			wantNext: IndexCursor{Timestamp: 201},
		},
		{
			// No boundary exists inside this page, so the offset comes into play - and the caller
			// must reduce it by whatever it deletes, which is what Partial announces.
			name:        "a page wholly at one timestamp is partial",
			cursor:      IndexCursor{Timestamp: 200, Offset: 3},
			hits:        []opensearchapi.SearchHit{hitAt("a", 200), hitAt("b", 200)},
			size:        2,
			wantIds:     []string{"a", "b"},
			wantNext:    IndexCursor{Timestamp: 200, Offset: 5},
			wantPartial: true,
		},
		{
			// The trap: a page can be wholly one timestamp and yet a LATER one than the cursor
			// named, its own instant having been exhausted by its offset. Carrying the old offset
			// across would begin the next page three documents into an instant it has never read.
			name:        "a partial page at a new timestamp restarts the offset",
			cursor:      IndexCursor{Timestamp: 200, Offset: 3},
			hits:        []opensearchapi.SearchHit{hitAt("a", 500), hitAt("b", 500)},
			size:        2,
			wantIds:     []string{"a", "b"},
			wantNext:    IndexCursor{Timestamp: 500, Offset: 2},
			wantPartial: true,
		},
		{
			// A UnixNano-scale timestamp, which is the only scale this ever runs at, arriving
			// exactly rather than through the float64 the SDK's sort values go through.
			name:     "nanosecond timestamps survive intact",
			hits:     []opensearchapi.SearchHit{hitAt("a", 1_700_000_000_000_000_129)},
			size:     10,
			wantIds:  []string{"a"},
			wantNext: IndexCursor{Timestamp: 1_700_000_000_000_000_130},
		},
	}

	for _, v := range tests {
		t.Run(v.name, func(t *testing.T) {
			page, err := buildIndexPage(v.cursor, v.hits, v.size)
			if err != nil {
				t.Fatalf("buildIndexPage: %s", err)
			}

			if strings.Join(page.Ids, ",") != strings.Join(v.wantIds, ",") {
				t.Errorf("ids: got %v, want %v", page.Ids, v.wantIds)
			}

			if page.Next != v.wantNext {
				t.Errorf("next cursor: got %+v, want %+v", page.Next, v.wantNext)
			}

			if page.Partial != v.wantPartial {
				t.Errorf("partial: got %t, want %t", page.Partial, v.wantPartial)
			}
		})
	}
}

// TestHitTimestamp_Failures covers the readings that cannot be trusted. Each returns an error rather
// than a zero, deliberately: a zero timestamp is a valid cursor position that would silently restart
// the enumeration from the beginning of the index on every pass.
func TestHitTimestamp_Failures(t *testing.T) {
	tests := []struct {
		name string
		hit  opensearchapi.SearchHit
	}{
		{
			name: "no fields at all",
			hit:  opensearchapi.SearchHit{ID: "a"},
		},
		{
			name: "unparseable fields",
			hit:  opensearchapi.SearchHit{ID: "a", Fields: json.RawMessage(`{`)},
		},
		{
			name: "the field is present but empty",
			hit:  opensearchapi.SearchHit{ID: "a", Fields: json.RawMessage(`{"timestamp":[]}`)},
		},
		{
			name: "the value is not a number",
			hit:  opensearchapi.SearchHit{ID: "a", Fields: json.RawMessage(`{"timestamp":["soon"]}`)},
		},
	}

	for _, v := range tests {
		t.Run(v.name, func(t *testing.T) {
			if _, err := hitTimestamp(v.hit); err == nil {
				t.Error("expected an error; an unreadable timestamp must not resolve to a usable cursor position")
			}
		})
	}
}

// TestBuildIndexPage_PropagatesAnUnreadableTimestamp is the same rule one level up: a page that
// cannot be trusted must abandon the pass, not return a cursor built from a guess.
func TestBuildIndexPage_PropagatesAnUnreadableTimestamp(t *testing.T) {
	hits := []opensearchapi.SearchHit{hitAt("a", 100), {ID: "b"}}

	if _, err := buildIndexPage(IndexCursor{}, hits, 10); err == nil {
		t.Error("expected an error when a hit carries no readable timestamp")
	}
}

// TestEnumerateIdsPage_QueryShape pins the three properties of the request the cursor design rests
// on, against the fake transport so it runs in CI where there is no cluster.
//
// Each one is load-bearing and each fails silently if it regresses: sorting on something other than
// timestamp changes what the cursor means, dropping docvalue_fields sends the timestamp back through
// the SDK's lossy float64 sort value, and a non-inclusive range would skip the documents sharing the
// boundary instant - which is the whole reason the offset exists.
func TestEnumerateIdsPage_QueryShape(t *testing.T) {
	transport := &fakeTransport{}
	idx := newTestOpenSearch(t, transport, 4)

	t.Cleanup(func() { _ = idx.Close() })

	if _, err := idx.EnumerateIdsPage(t.Context(), IndexCursor{Timestamp: 4242, Offset: 7}, 25); err != nil {
		t.Fatalf("EnumerateIdsPage: %s", err)
	}

	var body string

	for _, v := range transport.recorded() {
		if strings.Contains(v.path, "_search") {
			body = v.body
		}
	}

	if body == "" {
		t.Fatal("no search request was issued")
	}

	for _, want := range []string{
		`"docvalue_fields":["timestamp"]`,
		`"sort":[{"timestamp":"asc"}]`,
		`"gte":4242`,
		`"from":7`,
		`"size":25`,
		`"_source":false`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the enumeration query is missing %s\ngot: %s", want, body)
		}
	}
}

// TestEnumerateIdsPage_ReportsAFailedSearch keeps a cluster failure from reading as an exhausted
// index: Done means "there is nothing more", and answering it to a request that never completed
// would end the sweep believing it had seen everything.
func TestEnumerateIdsPage_ReportsAFailedSearch(t *testing.T) {
	transport := &fakeTransport{status: http.StatusInternalServerError}
	idx := newTestOpenSearch(t, transport, 4)

	t.Cleanup(func() { _ = idx.Close() })

	page, err := idx.EnumerateIdsPage(t.Context(), IndexCursor{}, 10)
	if err == nil {
		t.Fatal("expected an error from a failing cluster")
	}

	if page.Done {
		t.Error("a failed search reported the index as exhausted; the sweep would stop believing it had seen everything")
	}
}

// TestDeleteMemoriesSync_NoIdsIsNotARequest keeps the drain from issuing an empty round trip per
// pass on an idle deployment.
func TestDeleteMemoriesSync_NoIdsIsNotARequest(t *testing.T) {
	transport := &fakeTransport{}
	idx := newTestOpenSearch(t, transport, 4)

	t.Cleanup(func() { _ = idx.Close() })

	before := len(transport.recorded())

	if err := idx.DeleteMemoriesSync(t.Context(), nil); err != nil {
		t.Fatalf("DeleteMemoriesSync: %s", err)
	}

	if after := len(transport.recorded()); after != before {
		t.Errorf("deleting no ids issued %d requests, want none", after-before)
	}
}

// TestDeleteMemoriesSync_IsOneRequestPerBatch is the regression guard for a fault found in
// production, not in review.
//
// Deletes used to be one Document.Delete round trip per id, inside a single applyTimeout sized for
// ONE operation. That was survivable while every caller passed a handful of ids, and the delete
// outbox's drain and the stale sweep then began passing a page at a time. Against a real backlog -
// 4.4 million stale documents - five hundred sequential round trips could not finish inside a
// ten-second deadline, so every sweep pass abandoned and restarted from the top of the index. The
// sweep thrashed for hours instead of converging, and the symptom (slow) looked nothing like the
// cause (a deadline covering the wrong unit of work).
//
// So the count is what is asserted. A batch is one bulk request; if this ever goes back to a loop
// the number rises with the batch and this fails.
func TestDeleteMemoriesSync_IsOneRequestPerBatch(t *testing.T) {
	transport := &fakeTransport{}
	idx := newTestOpenSearch(t, transport, 4)

	t.Cleanup(func() { _ = idx.Close() })

	before := len(transport.recorded())

	ids := make([]string, 0, 500)

	for i := range 500 {
		ids = append(ids, fmt.Sprintf("bulk-%03d", i))
	}

	if err := idx.DeleteMemoriesSync(t.Context(), ids); err != nil {
		t.Fatalf("DeleteMemoriesSync: %s", err)
	}

	issued := transport.recorded()[before:]

	bulk := 0

	for _, v := range issued {
		if strings.Contains(v.path, "_bulk") {
			bulk++
		}
	}

	if bulk != 1 {
		t.Errorf("deleting 500 ids issued %d bulk requests, want exactly 1", bulk)
	}

	if len(issued) > 2 {
		t.Errorf("deleting 500 ids issued %d requests in total; a per-document loop has come back", len(issued))
	}

	// And the body must carry every id as a delete action, or the batch would silently under-delete.
	var body string

	for _, v := range issued {
		if strings.Contains(v.path, "_bulk") {
			body = v.body
		}
	}

	if n := strings.Count(body, `"delete"`); n != 500 {
		t.Errorf("the bulk body carries %d delete actions, want 500", n)
	}
}
