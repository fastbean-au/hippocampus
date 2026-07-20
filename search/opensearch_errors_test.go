package search

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestOpenSearch_Enabled verifies the real client reports Enabled() == true, the opposite of the
// noop implementation - callers (e.g. the RPC layer) branch on this to decide whether SearchMemories
// is available at all.
func TestOpenSearch_Enabled(t *testing.T) {
	idx := newTestOpenSearch(t, &fakeTransport{}, 16)
	defer func() { _ = idx.Close() }()

	if !idx.Enabled() {
		t.Error("expected a real OpenSearch index to report Enabled() == true")
	}
}

// TestOpenSearch_CloseTimesOut verifies Close reports an error rather than blocking forever when
// the worker cannot drain the queue within closeDrainTimeout - here because the transport never
// answers the in-flight operation.
func TestOpenSearch_CloseTimesOut(t *testing.T) {
	gate := make(chan struct{})
	transport := &fakeTransport{gate: gate}

	done := make(chan *OpenSearch)
	go func() {
		idx, err := NewOpenSearch(Config{
			Addresses:         []string{"http://opensearch.invalid:9200"},
			Index:             "test-index",
			QueueSize:         16,
			Transport:         transport,
			CloseDrainTimeout: 30 * time.Millisecond,
		})
		if err != nil {
			t.Errorf("NewOpenSearch: %s", err)
		}

		done <- idx
	}()

	gate <- struct{}{} // release the startup Exists call
	gate <- struct{}{} // release the startup mapping update

	idx := <-done

	idx.IndexMemory(Doc{Id: "m1", Body: "x"})

	// The worker is now blocked in the transport applying m1; Close must give up rather than wait
	// forever.
	if err := idx.Close(); err == nil {
		t.Error("expected Close to time out while the worker is stuck applying an operation")
	}

	// Unblock the stuck worker so its goroutine can exit and the test does not leak it.
	close(gate)
}

// TestOpenSearch_RetryAbortsDuringShutdown verifies applyWithRetry's backoff wait is itself
// interruptible by Close: a persistently failing operation gives up its current backoff (rather
// than sleeping it out) as soon as shutdown begins, so Close is not needlessly delayed.
func TestOpenSearch_RetryAbortsDuringShutdown(t *testing.T) {
	restore := applyRetryBaseBackoff
	applyRetryBaseBackoff = 300 * time.Millisecond
	t.Cleanup(func() { applyRetryBaseBackoff = restore })

	transport := &fakeTransport{failDocWrites: 1000}
	idx := newTestOpenSearch(t, transport, 16)

	idx.IndexMemory(Doc{Id: "m1", Body: "x"})

	// Wait for the first attempt to fail so the worker is sitting in the backoff wait.
	waitFor(t, "the first attempt to fail", func() bool { return countDocWrites(transport) >= 1 })

	start := time.Now()

	if err := idx.Close(); err != nil {
		t.Fatalf("Close: %s", err)
	}

	// If the backoff wait were not interrupted, Close would take close to 300ms+jitter. Interrupted,
	// it should return quickly.
	if elapsed := time.Since(start); elapsed > 250*time.Millisecond {
		t.Errorf("Close took %s - the in-flight retry backoff does not appear to have been interrupted", elapsed)
	}
}

// TestOpenSearch_NewOpenSearch_Defaults verifies an unset Index and QueueSize fall back to the
// package defaults.
func TestOpenSearch_NewOpenSearch_Defaults(t *testing.T) {
	idx, err := NewOpenSearch(Config{
		Addresses: []string{"http://opensearch.invalid:9200"},
		Transport: &fakeTransport{},
	})
	if err != nil {
		t.Fatalf("NewOpenSearch: %s", err)
	}
	defer func() { _ = idx.Close() }()

	if idx.index != "hippocampus-memories" {
		t.Errorf("expected the default index name, got %q", idx.index)
	}

	if cap(idx.queue) != 1024 {
		t.Errorf("expected the default queue size 1024, got %d", cap(idx.queue))
	}
}

// TestOpenSearch_NewOpenSearch_InvalidTLSConfig verifies a malformed TLS block fails construction
// with the underlying error, rather than starting with a broken transport.
func TestOpenSearch_NewOpenSearch_InvalidTLSConfig(t *testing.T) {
	_, err := NewOpenSearch(Config{
		Addresses: []string{"http://opensearch.invalid:9200"},
		Index:     "test-index",
		QueueSize: 16,
		TLS:       TLSConfig{CertFile: "only-cert-set"},
	})
	if err == nil {
		t.Fatal("expected NewOpenSearch to fail on a half-configured client certificate")
	}
}

// TestOpenSearch_NewOpenSearch_InvalidAddress verifies an address the client cannot parse fails
// construction with a clear error instead of panicking or silently misbehaving later.
func TestOpenSearch_NewOpenSearch_InvalidAddress(t *testing.T) {
	_, err := NewOpenSearch(Config{
		Addresses: []string{"://not-a-url"},
		Index:     "test-index",
		QueueSize: 16,
	})
	if err == nil {
		t.Fatal("expected NewOpenSearch to fail on an unparseable address")
	}
}

// TestOpenSearch_StartupEnsureIndexFailsWarnsAndRetries covers ensureIndex's genuine cluster-error
// branches, none of which a real (reachable) cluster naturally produces: the Exists probe itself
// erroring with a non-404 status, and the retry path when an operation is applied while the index
// is still not ready. NewOpenSearch must not fail construction - only warn - and later operations
// must keep retrying ensureIndex until it succeeds or the operation is dropped.
func TestOpenSearch_StartupEnsureIndexFailsWarnsAndRetries(t *testing.T) {
	restore := applyRetryBaseBackoff
	applyRetryBaseBackoff = time.Millisecond
	t.Cleanup(func() { applyRetryBaseBackoff = restore })

	transport := &fakeTransport{respond: func(req *http.Request) (int, error) {
		if req.Method == http.MethodHead {
			return http.StatusInternalServerError, nil
		}

		return 0, nil
	}}

	idx, err := NewOpenSearch(Config{
		Addresses: []string{"http://opensearch.invalid:9200"},
		Index:     "test-index",
		QueueSize: 16,
		Transport: transport,
	})
	if err != nil {
		t.Fatalf("NewOpenSearch should not fail outright on a startup ensureIndex error: %s", err)
	}

	if idx.indexReady.Load() {
		t.Error("expected indexReady to stay false after a failing startup ensureIndex")
	}

	idx.IndexMemory(Doc{Id: "m1", Body: "x"})

	// applyOnce retries ensureIndex (still failing) every attempt; the operation is eventually
	// dropped once applyMaxAttempts is exhausted.
	waitFor(t, "the operation to exhaust its retries against a permanently failing index probe", func() bool {
		heads := 0

		for _, r := range transport.recorded() {
			if r.method == http.MethodHead {
				heads++
			}
		}

		// One at startup plus at least applyMaxAttempts more from applyOnce's retries.
		return heads > applyMaxAttempts
	})

	if err := idx.Close(); err != nil {
		t.Fatalf("Close: %s", err)
	}
}

// TestOpenSearch_EnsureIndexMappingUpdateFails covers the branch where the index already exists
// (Exists succeeds) but putting the group-field mapping onto it fails.
func TestOpenSearch_EnsureIndexMappingUpdateFails(t *testing.T) {
	transport := &fakeTransport{respond: func(req *http.Request) (int, error) {
		if req.Method == http.MethodPut && strings.HasSuffix(req.URL.Path, "/_mapping") {
			return 0, fmt.Errorf("simulated mapping update failure")
		}

		return 0, nil
	}}

	idx, err := NewOpenSearch(Config{
		Addresses: []string{"http://opensearch.invalid:9200"},
		Index:     "test-index",
		QueueSize: 16,
		Transport: transport,
	})
	if err != nil {
		t.Fatalf("NewOpenSearch: %s", err)
	}
	defer func() { _ = idx.Close() }()

	if idx.indexReady.Load() {
		t.Error("expected indexReady to stay false when the mapping update fails")
	}
}

// TestOpenSearch_EnsureIndexCreateFails covers the branch where the index does not exist (a 404
// Exists probe) and the subsequent create call itself fails.
func TestOpenSearch_EnsureIndexCreateFails(t *testing.T) {
	transport := &fakeTransport{respond: func(req *http.Request) (int, error) {
		if req.Method == http.MethodHead {
			return http.StatusNotFound, nil
		}

		if req.Method == http.MethodPut && req.URL.Path == "/test-index" {
			return 0, fmt.Errorf("simulated create failure")
		}

		return 0, nil
	}}

	idx, err := NewOpenSearch(Config{
		Addresses: []string{"http://opensearch.invalid:9200"},
		Index:     "test-index",
		QueueSize: 16,
		Transport: transport,
	})
	if err != nil {
		t.Fatalf("NewOpenSearch: %s", err)
	}
	defer func() { _ = idx.Close() }()

	if idx.indexReady.Load() {
		t.Error("expected indexReady to stay false when index creation fails")
	}
}

// TestOpenSearch_SyncPathsFailWhenIndexNeverBecomesReady verifies IndexMemorySync and RecreateIndex
// - the backfill CLI mode's synchronous surface - both retry ensureIndex and surface its error
// rather than proceeding against a nonexistent index.
func TestOpenSearch_SyncPathsFailWhenIndexNeverBecomesReady(t *testing.T) {
	transport := &fakeTransport{respond: func(req *http.Request) (int, error) {
		if req.Method == http.MethodHead {
			return http.StatusInternalServerError, nil
		}

		return 0, nil
	}}

	idx, err := NewOpenSearch(Config{
		Addresses: []string{"http://opensearch.invalid:9200"},
		Index:     "test-index",
		QueueSize: 16,
		Transport: transport,
	})
	if err != nil {
		t.Fatalf("NewOpenSearch: %s", err)
	}
	defer func() { _ = idx.Close() }()

	if err := idx.IndexMemorySync(context.Background(), Doc{Id: "m1", Body: "x"}); err == nil {
		t.Error("expected IndexMemorySync to fail while the index is not ready")
	}

	if err := idx.RecreateIndex(context.Background()); err == nil {
		t.Error("expected RecreateIndex to fail while the index is not ready")
	}
}

// TestOpenSearch_ApplyUnknownKind verifies apply's fallthrough for an operation kind outside the
// known set - defensive code guarding against a future opKind added without a case here.
func TestOpenSearch_ApplyUnknownKind(t *testing.T) {
	idx := newTestOpenSearch(t, &fakeTransport{}, 16)
	defer func() { _ = idx.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	if err := idx.apply(ctx, op{kind: opKind(99)}); err == nil {
		t.Error("expected apply to reject an unknown operation kind")
	}
}

// TestOpenSearch_ApplyRefreshFailure covers the shared refresh-before-query-scoped-op guard: both
// opDeleteByEvent and opSetEventId refresh first, and either must surface a refresh failure instead
// of running the query against stale data.
func TestOpenSearch_ApplyRefreshFailure(t *testing.T) {
	transport := &fakeTransport{respond: func(req *http.Request) (int, error) {
		if strings.HasSuffix(req.URL.Path, "/_refresh") {
			return 0, fmt.Errorf("simulated refresh failure")
		}

		return 0, nil
	}}
	idx := newTestOpenSearch(t, transport, 16)
	defer func() { _ = idx.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	if err := idx.apply(ctx, op{kind: opDeleteByEvent, eventId: "e1"}); err == nil {
		t.Error("expected opDeleteByEvent to surface a refresh failure")
	}

	if err := idx.apply(ctx, op{kind: opSetEventId, eventId: "e1", toEventId: "e2"}); err == nil {
		t.Error("expected opSetEventId to surface a refresh failure")
	}

	if err := idx.refresh(ctx); err == nil {
		t.Error("expected refresh itself to surface the cluster error")
	}
}

// TestOpenSearch_ApplyDeleteByEventQueryFailure covers the delete-by-query call itself failing
// after a successful refresh.
func TestOpenSearch_ApplyDeleteByEventQueryFailure(t *testing.T) {
	transport := &fakeTransport{respond: func(req *http.Request) (int, error) {
		if strings.HasSuffix(req.URL.Path, "/_delete_by_query") {
			return 0, fmt.Errorf("simulated delete_by_query failure")
		}

		return 0, nil
	}}
	idx := newTestOpenSearch(t, transport, 16)
	defer func() { _ = idx.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	if err := idx.apply(ctx, op{kind: opDeleteByEvent, eventId: "e1"}); err == nil {
		t.Error("expected opDeleteByEvent to surface a delete_by_query failure")
	}
}

// TestOpenSearch_ApplySetEventIdQueryFailure covers the update-by-query call itself failing after
// a successful refresh.
func TestOpenSearch_ApplySetEventIdQueryFailure(t *testing.T) {
	transport := &fakeTransport{respond: func(req *http.Request) (int, error) {
		if strings.HasSuffix(req.URL.Path, "/_update_by_query") {
			return 0, fmt.Errorf("simulated update_by_query failure")
		}

		return 0, nil
	}}
	idx := newTestOpenSearch(t, transport, 16)
	defer func() { _ = idx.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	if err := idx.apply(ctx, op{kind: opSetEventId, eventId: "e1", toEventId: "e2"}); err == nil {
		t.Error("expected opSetEventId to surface an update_by_query failure")
	}
}

// TestOpenSearch_ApplyPurgeDeleteFailure covers opPurge's index-delete call failing.
func TestOpenSearch_ApplyPurgeDeleteFailure(t *testing.T) {
	transport := &fakeTransport{respond: func(req *http.Request) (int, error) {
		if req.Method == http.MethodDelete {
			return 0, fmt.Errorf("simulated index delete failure")
		}

		return 0, nil
	}}
	idx := newTestOpenSearch(t, transport, 16)
	defer func() { _ = idx.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	if err := idx.apply(ctx, op{kind: opPurge}); err == nil {
		t.Error("expected opPurge to surface an index-delete failure")
	}
}

// TestOpenSearch_ApplyDeleteIdsSkipsNotFound verifies deleting an id the cluster reports 404 for
// is treated as already-gone (no error), matching the documented "nothing to delete" behaviour.
func TestOpenSearch_ApplyDeleteIdsSkipsNotFound(t *testing.T) {
	transport := &fakeTransport{respond: func(req *http.Request) (int, error) {
		if req.Method == http.MethodDelete && strings.Contains(req.URL.Path, "/_doc/") {
			return http.StatusNotFound, nil
		}

		return 0, nil
	}}
	idx := newTestOpenSearch(t, transport, 16)
	defer func() { _ = idx.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	if err := idx.apply(ctx, op{kind: opDeleteIds, ids: []string{"never-indexed"}}); err != nil {
		t.Errorf("expected a 404 delete to be treated as a no-op, got %s", err)
	}
}

// TestOpenSearch_SearchClusterFailure verifies Search surfaces a cluster-side error instead of
// returning an empty result set, which would be indistinguishable from "no matches".
func TestOpenSearch_SearchClusterFailure(t *testing.T) {
	transport := &fakeTransport{}
	idx := newTestOpenSearch(t, transport, 16)
	defer func() { _ = idx.Close() }()

	// The worker is idle (nothing enqueued), so mutating the fake after startup is race-free.
	transport.status = http.StatusInternalServerError

	if _, err := idx.Search(context.Background(), Query{Text: "hello", Limit: 10}); err == nil {
		t.Error("expected Search to surface a cluster error")
	}
}
