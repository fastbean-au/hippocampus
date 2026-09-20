package search

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// mgetTransport answers an _mget with a `found` flag per requested id, taken from held, and records
// every request body so a test can assert what was asked and in how many round trips. Everything
// else (the index-exists probe ensureIndex makes) is answered 200 with an empty body, which is what
// the other fake transports in this package do.
type mgetTransport struct {
	mu sync.Mutex

	held map[string]bool

	// bodies holds the _mget request bodies, one per round trip; queries the query strings they
	// travelled with, which is where _source=false appears.
	bodies  []string
	queries []string

	// status, when non-zero, is returned for the _mget instead of 200.
	status int

	// malformed answers the _mget with a body that is not the expected shape.
	malformed bool
}

func (m *mgetTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !strings.HasSuffix(req.URL.Path, "/_mget") {

		return jsonResponse(req, http.StatusOK, `{}`), nil
	}

	raw, _ := io.ReadAll(req.Body)

	m.mu.Lock()
	m.bodies = append(m.bodies, string(raw))
	m.queries = append(m.queries, req.URL.RawQuery)
	status := m.status
	malformed := m.malformed
	held := m.held
	m.mu.Unlock()

	if status != 0 {

		return jsonResponse(req, status, `{"error":"nope"}`), nil
	}

	if malformed {

		return jsonResponse(req, http.StatusOK, `{"docs":"not an array"}`), nil
	}

	var asked struct {
		Ids []string `json:"ids"`
	}

	if err := json.Unmarshal(raw, &asked); err != nil {

		return nil, err
	}

	docs := make([]map[string]any, 0, len(asked.Ids))

	for _, id := range asked.Ids {
		docs = append(docs, map[string]any{"_index": "idx", "_id": id, "found": held[id]})
	}

	body, err := json.Marshal(map[string]any{"docs": docs})
	if err != nil {

		return nil, err
	}

	return jsonResponse(req, http.StatusOK, string(body)), nil
}

func (m *mgetTransport) requestBodies() []string {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make([]string, len(m.bodies))
	copy(out, m.bodies)

	return out
}

func (m *mgetTransport) requestQueries() []string {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make([]string, len(m.queries))
	copy(out, m.queries)

	return out
}

func jsonResponse(req *http.Request, status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}
}

// newProbeIndex builds an OpenSearch backend over a fake cluster, with the index already marked
// ready so the probe does not first try to create it.
func newProbeIndex(t *testing.T, transport http.RoundTripper) *OpenSearch {
	t.Helper()

	o, err := NewOpenSearch(Config{
		Addresses: []string{"http://opensearch.invalid:9200"},
		Index:     "idx",
		Transport: transport,
	})
	if err != nil {
		t.Fatalf("NewOpenSearch: %s", err)
	}

	t.Cleanup(func() { _ = o.Close() })

	o.indexReady.Store(true)

	return o
}

// TestAbsentIds reports the ids the index does not hold, in the order they were given.
func TestAbsentIds(t *testing.T) {
	transport := &mgetTransport{held: map[string]bool{"m1": true, "m3": true}}
	o := newProbeIndex(t, transport)

	absent, err := o.AbsentIds(context.Background(), []string{"m1", "m2", "m3", "m4"})
	if err != nil {
		t.Fatalf("AbsentIds: %s", err)
	}

	want := []string{"m2", "m4"}

	if len(absent) != len(want) {
		t.Fatalf("AbsentIds = %v, want %v", absent, want)
	}

	for i := range want {
		if absent[i] != want[i] {
			t.Fatalf("AbsentIds = %v, want %v", absent, want)
		}
	}
}

// TestAbsentIds_AsksInOneRequest pins the cost: the whole page travels in one _mget. A request per id
// would replace one write round trip per memory with one read round trip per memory, which is the
// cost the probe exists to remove.
func TestAbsentIds_AsksInOneRequest(t *testing.T) {
	transport := &mgetTransport{}
	o := newProbeIndex(t, transport)

	ids := make([]string, 0, 300)

	for i := range 300 {
		ids = append(ids, fmt.Sprintf("m%d", i))
	}

	if _, err := o.AbsentIds(context.Background(), ids); err != nil {
		t.Fatalf("AbsentIds: %s", err)
	}

	bodies := transport.requestBodies()

	if len(bodies) != 1 {
		t.Fatalf("asked about 300 ids in %d requests, want 1", len(bodies))
	}
}

// TestAbsentIds_ChunksALargePage pins that a page larger than the probe's chunk is split rather than
// sent as one enormous request, and that the answers are joined.
func TestAbsentIds_ChunksALargePage(t *testing.T) {
	transport := &mgetTransport{}
	o := newProbeIndex(t, transport)

	ids := make([]string, 0, presenceProbeChunk+10)

	for i := range presenceProbeChunk + 10 {
		ids = append(ids, fmt.Sprintf("m%d", i))
	}

	absent, err := o.AbsentIds(context.Background(), ids)
	if err != nil {
		t.Fatalf("AbsentIds: %s", err)
	}

	if len(absent) != len(ids) {
		t.Errorf("the index holds nothing, so %d ids should be absent; got %d", len(ids), len(absent))
	}

	if got := len(transport.requestBodies()); got != 2 {
		t.Errorf("split %d ids into %d requests, want 2", len(ids), got)
	}
}

// TestAbsentIds_RequestsNoSource pins that the probe asks for existence and not content. A page of
// bodies - with a ~3 KiB embedding vector in each - is orders of magnitude larger than the answer,
// and would make asking more expensive than the writing it replaces.
func TestAbsentIds_RequestsNoSource(t *testing.T) {
	transport := &mgetTransport{}
	o := newProbeIndex(t, transport)

	if _, err := o.AbsentIds(context.Background(), []string{"m1"}); err != nil {
		t.Fatalf("AbsentIds: %s", err)
	}

	queries := transport.requestQueries()

	if len(queries) != 1 {
		t.Fatalf("made %d requests, want 1", len(queries))
	}

	// Asserted on the wire query string rather than on the SDK's params struct, because it is the
	// request the cluster sees that decides what comes back.
	if !strings.Contains(queries[0], "_source=false") {
		t.Errorf("the probe asked for document content: query was %q, want _source=false", queries[0])
	}
}

// TestAbsentIds_EmptyIdsAsksNothing: a page with nothing indexable in it must not cost a round trip.
func TestAbsentIds_EmptyIdsAsksNothing(t *testing.T) {
	transport := &mgetTransport{}
	o := newProbeIndex(t, transport)

	absent, err := o.AbsentIds(context.Background(), nil)
	if err != nil {
		t.Fatalf("AbsentIds: %s", err)
	}

	if len(absent) != 0 {
		t.Errorf("AbsentIds(nil) = %v, want nothing", absent)
	}

	if got := len(transport.requestBodies()); got != 0 {
		t.Errorf("AbsentIds(nil) made %d requests, want none", got)
	}
}

// TestAbsentIds_ClusterFailureIsReported: the sweep must be able to tell a cluster it could not ask
// from an index holding everything, because the two lead to opposite actions.
func TestAbsentIds_ClusterFailureIsReported(t *testing.T) {
	transport := &mgetTransport{status: http.StatusServiceUnavailable}
	o := newProbeIndex(t, transport)

	if _, err := o.AbsentIds(context.Background(), []string{"m1"}); err == nil {
		t.Error("a 503 from the cluster was reported as a successful probe")
	}
}

// TestAbsentIds_TreatsAnUnanswerableDocumentAsPresent pins the safe direction. An answer this package
// cannot read is not evidence a document is missing, and reporting it absent would enqueue a write -
// which is the churn the probe exists to remove.
func TestAbsentIds_TreatsAnUnanswerableDocumentAsPresent(t *testing.T) {
	transport := &mgetTransport{malformed: true}
	o := newProbeIndex(t, transport)

	absent, err := o.AbsentIds(context.Background(), []string{"m1"})

	// A malformed body is a decode failure at the SDK, so this reports an error rather than a
	// verdict; what must not happen is a confident "absent".
	if err == nil && len(absent) != 0 {
		t.Errorf("an unreadable answer produced absent = %v; it must not claim a document is missing", absent)
	}
}
