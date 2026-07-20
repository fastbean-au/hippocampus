package search

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fastbean-au/hippocampus/types"
)

// TestNoop verifies the disabled implementation: mutators are safe no-ops, Search reports
// ErrDisabled, and Enabled is false, so the service behaves exactly as it does without a search
// index configured.
func TestNoop(t *testing.T) {
	idx := NewNoop()

	if idx.Enabled() {
		t.Error("noop index must report Enabled() == false")
	}

	idx.IndexMemory(Doc{Id: "m1", Body: "hello"})
	idx.DeleteMemories([]string{"m1"})
	idx.DeleteByEventId("e1")
	idx.SetEventId("e1", "e2")
	idx.Purge()

	if _, err := idx.Search(context.Background(), Query{Text: "hello", Limit: 10}); !errors.Is(err, ErrDisabled) {
		t.Errorf("noop Search should return ErrDisabled, got %v", err)
	}

	if err := idx.Close(); err != nil {
		t.Errorf("noop Close should return nil, got %v", err)
	}
}

// TestDocFromMemory verifies the indexed projection maps the searchable fields and drops recall
// state (time_recalled/recall_count), which the index never carries.
func TestDocFromMemory(t *testing.T) {
	doc := DocFromMemory(memoryFixture())

	if doc.Id != "m1" || doc.Body != "hello" || doc.EventId != "e1" ||
		doc.Significance != 7 || doc.Timestamp != 100 || !doc.IsSummary || doc.Group != "g1" {
		t.Errorf("unexpected doc projection: %+v", doc)
	}
}

// TestOpKindString verifies every operation kind has a stable label (used in the worker's warning
// logs) and that an out-of-range kind falls through to "unknown".
func TestOpKindString(t *testing.T) {
	cases := map[opKind]string{
		opIndex:         "index",
		opDeleteIds:     "delete_ids",
		opDeleteByEvent: "delete_by_event",
		opSetEventId:    "set_event_id",
		opPurge:         "purge",
		opKind(99):      "unknown",
	}

	for kind, want := range cases {
		if got := kind.String(); got != want {
			t.Errorf("opKind(%d).String() = %q, want %q", kind, got, want)
		}
	}
}

// memoryFixture is a fully-populated memory used by the projection test.
func memoryFixture() (m types.Memory) {
	m.Id = "m1"
	m.Body = "hello"
	m.EventId = "e1"
	m.Significance = 7
	m.TimeStamp = 100
	m.TimeRecalled = 555
	m.RecallCount = 3
	m.IsSummary = true
	m.Group = "g1"

	return m
}

// fakeTransport is an http.RoundTripper standing in for the cluster. Every request is recorded;
// an optional gate blocks request handling so tests can hold the worker mid-operation.
type fakeTransport struct {
	mu       sync.Mutex
	requests []recordedRequest

	// gate, when non-nil, is received from before each request completes.
	gate chan struct{}

	// status is the HTTP status returned; 0 means 200.
	status int

	// failDocWrites, when > 0, makes the first N document-write (PUT /_doc/...) requests fail with a
	// simulated network error, so the worker's retry path can be exercised. docAttempts counts every
	// document write seen, including the failed ones.
	failDocWrites int
	docAttempts   int
}

type recordedRequest struct {
	method string
	path   string
	body   string
}

func (f *fakeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if f.gate != nil {
		<-f.gate
	}

	body := ""
	if req.Body != nil {
		b, _ := io.ReadAll(req.Body)
		body = string(b)
	}

	f.mu.Lock()
	f.requests = append(f.requests, recordedRequest{method: req.Method, path: req.URL.Path, body: body})

	isDocWrite := req.Method == http.MethodPut && strings.Contains(req.URL.Path, "/_doc/")
	if isDocWrite {
		f.docAttempts++
	}

	fail := isDocWrite && f.docAttempts <= f.failDocWrites
	f.mu.Unlock()

	if fail {
		return nil, fmt.Errorf("simulated cluster failure")
	}

	status := f.status
	if status == 0 {
		status = http.StatusOK
	}

	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{}`)),
		Request:    req,
	}, nil
}

func (f *fakeTransport) recorded() []recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := make([]recordedRequest, len(f.requests))
	copy(out, f.requests)

	return out
}

func newTestOpenSearch(t *testing.T, transport *fakeTransport, queueSize int) *OpenSearch {
	t.Helper()

	idx, err := NewOpenSearch(Config{
		Addresses: []string{"http://opensearch.invalid:9200"},
		Index:     "test-index",
		QueueSize: queueSize,
		Transport: transport,
	})
	if err != nil {
		t.Fatalf("NewOpenSearch: %s", err)
	}

	return idx
}

// waitFor polls until the condition holds or the deadline passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)

	for time.Now().Before(deadline) {
		if cond() {
			return
		}

		time.Sleep(5 * time.Millisecond)
	}

	t.Fatalf("timed out waiting for %s", what)
}

// TestOpenSearch_WorkerAppliesInOrder verifies FIFO ordering through the single worker: the
// delete-then-index pair emitted by ReplaceMemoriesWithSummary must never be reordered.
func TestOpenSearch_WorkerAppliesInOrder(t *testing.T) {
	transport := &fakeTransport{}
	idx := newTestOpenSearch(t, transport, 16)

	idx.DeleteByEventId("e1")
	idx.IndexMemory(Doc{Id: "summary", Body: "the summary", EventId: "e1"})

	// startup Exists + mapping update + (refresh + delete_by_query) + index = 5 requests
	waitFor(t, "worker to apply both operations", func() bool { return len(transport.recorded()) >= 5 })

	if err := idx.Close(); err != nil {
		t.Fatalf("Close: %s", err)
	}

	var paths []string
	for _, r := range transport.recorded() {
		paths = append(paths, r.method+" "+r.path)
	}

	// The startup Exists check and mapping update come first; then refresh, delete-by-query, and
	// finally the summary's index write.
	want := []string{
		"HEAD /test-index",
		"PUT /test-index/_mapping",
		"POST /test-index/_refresh",
		"POST /test-index/_delete_by_query",
		"PUT /test-index/_doc/summary",
	}

	if fmt.Sprint(paths) != fmt.Sprint(want) {
		t.Errorf("requests out of order:\n got %v\nwant %v", paths, want)
	}
}

// TestOpenSearch_OverflowDropsWithoutBlocking verifies that a full queue drops operations rather
// than blocking the caller - the write path's latency must never depend on the cluster.
func TestOpenSearch_OverflowDropsWithoutBlocking(t *testing.T) {
	gate := make(chan struct{})
	transport := &fakeTransport{gate: gate}

	// The startup ensureIndex blocks on the gate, holding the worker before it consumes
	// anything, so the queue fills deterministically.
	done := make(chan *OpenSearch)
	go func() { done <- newTestOpenSearch(t, transport, 2) }()

	gate <- struct{}{} // release the startup Exists call
	gate <- struct{}{} // release the startup mapping update

	idx := <-done

	finished := make(chan struct{})

	go func() {
		for i := range 10 {
			idx.IndexMemory(Doc{Id: fmt.Sprintf("m%d", i), Body: "x"})
		}

		close(finished)
	}()

	select {

	case <-finished:

	case <-time.After(2 * time.Second):
		t.Fatal("enqueue blocked on a full queue")
	}

	// Release the worker (a closed gate never blocks again) and let it drain whatever was
	// accepted.
	close(gate)

	if err := idx.Close(); err != nil {
		t.Fatalf("Close: %s", err)
	}

	// Startup Exists + mapping update, then at most queueSize(2)+1 accepted index writes (one
	// may have been pulled by the worker while blocked on the gate).
	docs := 0
	for _, r := range transport.recorded() {
		if strings.HasPrefix(r.path, "/test-index/_doc/") {
			docs++
		}
	}

	if docs > 3 {
		t.Errorf("expected at most 3 index writes to survive the overflow, got %d", docs)
	}
}

// countDocWrites returns how many document-write requests (PUT /_doc/...) the transport saw.
func countDocWrites(transport *fakeTransport) int {
	n := 0

	for _, r := range transport.recorded() {
		if r.method == http.MethodPut && strings.Contains(r.path, "/_doc/") {
			n++
		}
	}

	return n
}

// TestOpenSearch_RetriesTransientFailure verifies the worker retries a transient cluster failure
// instead of dropping the operation on the first error (the old behaviour, which lost the write).
// The transport fails the first two document writes, so the operation must land on the third
// attempt - proving it retried past the failures and then stopped once it succeeded.
func TestOpenSearch_RetriesTransientFailure(t *testing.T) {
	restore := applyRetryBaseBackoff
	applyRetryBaseBackoff = time.Millisecond
	t.Cleanup(func() { applyRetryBaseBackoff = restore })

	transport := &fakeTransport{failDocWrites: 2}
	idx := newTestOpenSearch(t, transport, 16)

	idx.IndexMemory(Doc{Id: "m1", Body: "hello"})

	// Two failed attempts + one success = exactly three document writes.
	waitFor(t, "the write to succeed after retries", func() bool { return countDocWrites(transport) == 3 })

	if err := idx.Close(); err != nil {
		t.Fatalf("Close: %s", err)
	}

	// It must not keep trying after the success.
	if got := countDocWrites(transport); got != 3 {
		t.Errorf("expected exactly 3 document writes (2 failed, 1 succeeded), got %d", got)
	}
}

// TestOpenSearch_DropsAfterExhaustingRetries verifies the retry loop is bounded: a persistently
// failing operation is attempted applyMaxAttempts times and then dropped, rather than retried
// forever. A dropped operation is still recoverable via the reconciliation sweep and backfill.
func TestOpenSearch_DropsAfterExhaustingRetries(t *testing.T) {
	restore := applyRetryBaseBackoff
	applyRetryBaseBackoff = time.Millisecond
	t.Cleanup(func() { applyRetryBaseBackoff = restore })

	transport := &fakeTransport{failDocWrites: 1000}
	idx := newTestOpenSearch(t, transport, 16)

	idx.IndexMemory(Doc{Id: "m1", Body: "hello"})

	waitFor(t, "the worker to exhaust its retries", func() bool { return countDocWrites(transport) == applyMaxAttempts })

	if err := idx.Close(); err != nil {
		t.Fatalf("Close: %s", err)
	}

	if got := countDocWrites(transport); got != applyMaxAttempts {
		t.Errorf("expected exactly %d attempts before dropping, got %d", applyMaxAttempts, got)
	}
}

// TestOpenSearch_WorkerTuningConfig verifies the worker-tuning knobs are read from Config when set
// and fall back to the package defaults when zero.
func TestOpenSearch_WorkerTuningConfig(t *testing.T) {
	custom, err := NewOpenSearch(Config{
		Addresses:             []string{"http://opensearch.invalid:9200"},
		Index:                 "test-index",
		QueueSize:             16,
		Transport:             &fakeTransport{},
		ApplyTimeout:          3 * time.Second,
		ApplyMaxAttempts:      7,
		ApplyRetryBaseBackoff: 40 * time.Millisecond,
		CloseDrainTimeout:     2 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewOpenSearch (custom): %s", err)
	}
	t.Cleanup(func() { _ = custom.Close() })

	if custom.applyTimeout != 3*time.Second || custom.applyMaxAttempts != 7 ||
		custom.applyRetryBaseBackoff != 40*time.Millisecond || custom.closeDrainTimeout != 2*time.Second {
		t.Errorf("custom config not applied: got timeout=%s attempts=%d backoff=%s drain=%s",
			custom.applyTimeout, custom.applyMaxAttempts, custom.applyRetryBaseBackoff, custom.closeDrainTimeout)
	}

	defaults := newTestOpenSearch(t, &fakeTransport{}, 16)

	if defaults.applyTimeout != applyTimeout || defaults.applyMaxAttempts != applyMaxAttempts ||
		defaults.applyRetryBaseBackoff != applyRetryBaseBackoff || defaults.closeDrainTimeout != closeDrainTimeout {
		t.Errorf("defaults not applied: got timeout=%s attempts=%d backoff=%s drain=%s",
			defaults.applyTimeout, defaults.applyMaxAttempts, defaults.applyRetryBaseBackoff, defaults.closeDrainTimeout)
	}
}

// TestOpenSearch_IndexMemorySync verifies the backfill write path is synchronous - the request
// must be on the wire before the call returns, without waiting on the worker - and that a cluster
// error surfaces to the caller instead of being logged and dropped.
func TestOpenSearch_IndexMemorySync(t *testing.T) {
	transport := &fakeTransport{}
	idx := newTestOpenSearch(t, transport, 16)

	if err := idx.IndexMemorySync(context.Background(), Doc{Id: "m1", Body: "hello"}); err != nil {
		t.Fatalf("IndexMemorySync: %s", err)
	}

	// startup Exists + mapping update + the synchronous index write, recorded before the call
	// returned.
	requests := transport.recorded()

	if len(requests) != 3 || requests[2].method+" "+requests[2].path != "PUT /test-index/_doc/m1" {
		t.Errorf("expected a synchronous PUT /test-index/_doc/m1, got %v", requests)
	}

	// The worker is idle (nothing was ever enqueued), so mutating the fake is race-free.
	transport.status = 500

	if err := idx.IndexMemorySync(context.Background(), Doc{Id: "m2", Body: "doomed"}); err == nil {
		t.Error("IndexMemorySync should surface a cluster error to the caller")
	}

	if err := idx.Close(); err != nil {
		t.Fatalf("Close: %s", err)
	}
}

// TestOpenSearch_RecreateIndex verifies the --reindex path deletes the index and immediately
// re-runs the mapping bootstrap.
func TestOpenSearch_RecreateIndex(t *testing.T) {
	transport := &fakeTransport{}
	idx := newTestOpenSearch(t, transport, 16)

	if err := idx.RecreateIndex(context.Background()); err != nil {
		t.Fatalf("RecreateIndex: %s", err)
	}

	var paths []string
	for _, r := range transport.recorded() {
		paths = append(paths, r.method+" "+r.path)
	}

	// startup Exists + mapping update, the delete, then ensureIndex's Exists probe (the fake
	// answers 200, so a mapping update rather than a create follows).
	want := []string{
		"HEAD /test-index",
		"PUT /test-index/_mapping",
		"DELETE /test-index",
		"HEAD /test-index",
		"PUT /test-index/_mapping",
	}

	if fmt.Sprint(paths) != fmt.Sprint(want) {
		t.Errorf("requests:\n got %v\nwant %v", paths, want)
	}

	if err := idx.Close(); err != nil {
		t.Fatalf("Close: %s", err)
	}
}

// TestOpenSearch_DeleteMemories verifies the async id-delete path issues one DELETE per id through
// the worker, and that an empty id set enqueues nothing.
func TestOpenSearch_DeleteMemories(t *testing.T) {
	transport := &fakeTransport{}
	idx := newTestOpenSearch(t, transport, 16)

	// An empty slice must be a no-op - it never reaches the worker.
	idx.DeleteMemories(nil)

	idx.DeleteMemories([]string{"m1", "m2"})

	waitFor(t, "worker to delete both documents", func() bool {
		deletes := 0

		for _, r := range transport.recorded() {
			if r.method == "DELETE" && strings.HasPrefix(r.path, "/test-index/_doc/") {
				deletes++
			}
		}

		return deletes == 2
	})

	if err := idx.Close(); err != nil {
		t.Fatalf("Close: %s", err)
	}

	if _, ok := bodyForPathSuffix(transport.recorded(), "/_doc/m1"); !ok {
		t.Error("expected a delete for m1")
	}
}

// TestOpenSearch_Purge verifies the async Purge deletes the index and re-bootstraps its mapping.
func TestOpenSearch_Purge(t *testing.T) {
	transport := &fakeTransport{}
	idx := newTestOpenSearch(t, transport, 16)

	idx.Purge()

	waitFor(t, "worker to delete the index", func() bool {
		for _, r := range transport.recorded() {
			if r.method == "DELETE" && r.path == "/test-index" {
				return true
			}
		}

		return false
	})

	if err := idx.Close(); err != nil {
		t.Fatalf("Close: %s", err)
	}

	// After deleting the index, ensureIndex re-probes and re-applies the mapping.
	var reprobed bool
	sawDelete := false

	for _, r := range transport.recorded() {
		if r.method == "DELETE" && r.path == "/test-index" {
			sawDelete = true

			continue
		}

		if sawDelete && r.method == "HEAD" && r.path == "/test-index" {
			reprobed = true
		}
	}

	if !reprobed {
		t.Error("expected the index to be re-probed after the purge delete")
	}
}

// TestOpenSearch_CloseDrainsQueue verifies pending operations are applied before Close returns.
func TestOpenSearch_CloseDrainsQueue(t *testing.T) {
	transport := &fakeTransport{}
	idx := newTestOpenSearch(t, transport, 16)

	for i := range 5 {
		idx.IndexMemory(Doc{Id: fmt.Sprintf("m%d", i), Body: "x"})
	}

	if err := idx.Close(); err != nil {
		t.Fatalf("Close: %s", err)
	}

	docs := 0
	for _, r := range transport.recorded() {
		if strings.HasPrefix(r.path, "/test-index/_doc/") {
			docs++
		}
	}

	if docs != 5 {
		t.Errorf("Close should drain all 5 queued index writes, got %d", docs)
	}

	// Enqueues after Close are silent no-ops.
	idx.IndexMemory(Doc{Id: "late", Body: "x"})

	if err := idx.Close(); err != nil {
		t.Errorf("second Close should be a no-op, got %v", err)
	}
}

// bodyForPathSuffix returns the recorded request body whose path ends with suffix, or "" and false.
func bodyForPathSuffix(reqs []recordedRequest, suffix string) (string, bool) {
	for _, r := range reqs {
		if strings.HasSuffix(r.path, suffix) {
			return r.body, true
		}
	}

	return "", false
}

// TestOpenSearch_SearchBodyValidJSONWithControlChars is a regression test: the
// query bodies were built with fmt's %q, which emits Go escapes (\a, \v, \x07, ...) that JSON does
// not accept, so a text/id/group carrying a rare control character produced a malformed request.
// The bodies are now marshalled from maps, so json.Valid must accept them.
func TestOpenSearch_SearchBodyValidJSONWithControlChars(t *testing.T) {
	transport := &fakeTransport{}
	idx := newTestOpenSearch(t, transport, 16)

	defer func() { _ = idx.Close() }()

	// \x07 (bell) is the value %q renders as the JSON-invalid \a.
	if _, err := idx.Search(context.Background(), Query{
		Text:    "hello \x07 world",
		EventId: "evt\x07id",
		Group:   "grp\x07name",
		Limit:   10,
	}); err != nil {
		t.Fatalf("Search: %s", err)
	}

	body, ok := bodyForPathSuffix(transport.recorded(), "/_search")
	if !ok {
		t.Fatal("no _search request recorded")
	}

	if !json.Valid([]byte(body)) {
		t.Errorf("search body is not valid JSON: %q", body)
	}
}

// TestOpenSearch_EventOpBodiesValidJSONWithControlChars covers the delete-by-query and
// update-by-query bodies for event ids carrying control characters.
func TestOpenSearch_EventOpBodiesValidJSONWithControlChars(t *testing.T) {
	transport := &fakeTransport{}
	idx := newTestOpenSearch(t, transport, 16)

	idx.DeleteByEventId("del\x07evt")
	idx.SetEventId("from\x07evt", "to\x07evt")

	// Each event op is preceded by a refresh; wait until both have reached the transport.
	waitFor(t, "worker to apply both event ops", func() bool {
		reqs := transport.recorded()
		_, del := bodyForPathSuffix(reqs, "/_delete_by_query")
		_, upd := bodyForPathSuffix(reqs, "/_update_by_query")

		return del && upd
	})

	if err := idx.Close(); err != nil {
		t.Fatalf("Close: %s", err)
	}

	for _, suffix := range []string{"/_delete_by_query", "/_update_by_query"} {
		body, ok := bodyForPathSuffix(transport.recorded(), suffix)
		if !ok {
			t.Fatalf("no %s request recorded", suffix)
		}

		if !json.Valid([]byte(body)) {
			t.Errorf("%s body is not valid JSON: %q", suffix, body)
		}
	}
}
