package hippocampus

import (
	"context"
	"errors"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/fastbean-au/hippocampus/db"
	"github.com/fastbean-au/hippocampus/search"
	"github.com/fastbean-au/hippocampus/types"
)

// recordingIndex is a search.Index that records the ids passed to IndexMemory, so a reconcile sweep
// can be observed without a real cluster. Every other method is an inert no-op.
type recordingIndex struct {
	mu      sync.Mutex
	indexed []string
}

func (r *recordingIndex) IndexMemory(doc search.Doc) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.indexed = append(r.indexed, doc.Id)
}

func (r *recordingIndex) indexedIds() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]string, len(r.indexed))
	copy(out, r.indexed)
	sort.Strings(out)

	return out
}

func (*recordingIndex) DeleteMemories(ids []string)                     {}
func (*recordingIndex) DeleteByEventId(eventId string)                  {}
func (*recordingIndex) SetEventId(fromEventId string, toEventId string) {}
func (*recordingIndex) Purge()                                          {}
func (*recordingIndex) Search(ctx context.Context, q search.Query) ([]string, error) {
	return nil, nil
}
func (*recordingIndex) Enabled() bool { return true }
func (*recordingIndex) Close() error  { return nil }

// TestReconcileOnce_ReindexesNonBinaryMemories verifies a sweep re-indexes every non-binary memory
// in the primary store - the self-healing that recovers documents a dropped index operation missed
// - while skipping binary memories, whose bodies are opaque to content search.
func TestReconcileOnce_ReindexesNonBinaryMemories(t *testing.T) {
	restore := reconcilePageDelay
	reconcilePageDelay = time.Millisecond
	t.Cleanup(func() { reconcilePageDelay = restore })

	database, err := db.New("")
	if err != nil {
		t.Fatalf("db.New: %s", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	idx := &recordingIndex{}

	s := &Server{
		db:                 database,
		search:             idx,
		reconcileBatchSize: 2, // small, so the sweep pages more than once
		stopReconcile:      make(chan struct{}),
	}

	// Five memories across two pages; m3 is binary and must be skipped.
	memories := []types.Memory{
		{Id: "m1", TimeStamp: 100, Significance: 5, Body: "one"},
		{Id: "m2", TimeStamp: 100, Significance: 5, Body: "two"},
		{Id: "m3", TimeStamp: 100, Significance: 5, Body: "binary", IsBinary: true},
		{Id: "m4", TimeStamp: 100, Significance: 5, Body: "four"},
		{Id: "m5", TimeStamp: 100, Significance: 5, Body: "five"},
	}

	for _, m := range memories {
		if _, err := database.CreateMemory(context.Background(), m); err != nil {
			t.Fatalf("CreateMemory(%s): %s", m.Id, err)
		}
	}

	s.reconcileOnce()

	got := idx.indexedIds()
	want := []string{"m1", "m2", "m4", "m5"}

	if len(got) != len(want) {
		t.Fatalf("re-indexed %v, want %v", got, want)
	}

	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("re-indexed %v, want %v", got, want)
		}
	}
}

// TestReconcileOnce_StopsPromptlyOnShutdown verifies a sweep in progress abandons its remaining
// work as soon as the server is shutting down, rather than paging the whole store first.
func TestReconcileOnce_StopsPromptlyOnShutdown(t *testing.T) {
	restore := reconcilePageDelay
	reconcilePageDelay = 50 * time.Millisecond
	t.Cleanup(func() { reconcilePageDelay = restore })

	database, err := db.New("")
	if err != nil {
		t.Fatalf("db.New: %s", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	for i := range 20 {
		if _, err := database.CreateMemory(context.Background(), types.Memory{
			Id:           string(rune('a' + i)),
			TimeStamp:    100,
			Significance: 5,
			Body:         "x",
		}); err != nil {
			t.Fatalf("CreateMemory: %s", err)
		}
	}

	idx := &recordingIndex{}

	s := &Server{
		db:                 database,
		search:             idx,
		reconcileBatchSize: 1, // one memory per page, so the pacing delay dominates
		stopReconcile:      make(chan struct{}),
	}

	done := make(chan struct{})

	go func() {
		s.reconcileOnce()
		close(done)
	}()

	// Let a couple of pages through, then signal shutdown.
	time.Sleep(60 * time.Millisecond)
	close(s.stopReconcile)

	select {

	case <-done:

	case <-time.After(2 * time.Second):
		t.Fatal("reconcileOnce did not stop promptly on shutdown")
	}

	if got := len(idx.indexedIds()); got >= 20 {
		t.Errorf("expected the sweep to stop early, but it indexed all %d memories", got)
	}
}

// TestReconcileLoop_RunsSweepThenStops exercises the outer timer-driven loop (reconcileLoop
// itself, as opposed to a single reconcileOnce call): after reconcileInitialDelay it must run a
// sweep on its own, and closing stopReconcile must make it return promptly and close
// reconcileStopped, exactly as startReconcile's shutdown path (server.go) expects.
func TestReconcileLoop_RunsSweepThenStops(t *testing.T) {
	restoreDelay := reconcileInitialDelay
	reconcileInitialDelay = 10 * time.Millisecond
	t.Cleanup(func() { reconcileInitialDelay = restoreDelay })

	restorePage := reconcilePageDelay
	reconcilePageDelay = time.Millisecond
	t.Cleanup(func() { reconcilePageDelay = restorePage })

	database, err := db.New("")
	if err != nil {
		t.Fatalf("db.New: %s", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	if _, err := database.CreateMemory(context.Background(), types.Memory{Id: "m1", TimeStamp: 100, Significance: 5, Body: "one"}); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	idx := &recordingIndex{}

	s := &Server{
		db:                 database,
		search:             idx,
		reconcileInterval:  time.Hour, // long enough that only the initial-delay sweep can fire
		reconcileBatchSize: 10,
		stopReconcile:      make(chan struct{}),
		reconcileStopped:   make(chan struct{}),
	}

	go s.reconcileLoop()

	// Wait for the initial-delay sweep to index the one memory.
	deadline := time.Now().Add(2 * time.Second)
	for len(idx.indexedIds()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}

	if got := idx.indexedIds(); len(got) != 1 || got[0] != "m1" {
		t.Fatalf("expected the initial sweep to index [m1], got %v", got)
	}

	close(s.stopReconcile)

	select {

	case <-s.reconcileStopped:

	case <-time.After(2 * time.Second):
		t.Fatal("reconcileLoop did not stop promptly after stopReconcile was closed")
	}
}

// TestReconcileLoop_StopsBeforeInitialSweep verifies reconcileLoop can be stopped while still
// waiting out the initial delay, without ever running a sweep - it must not block shutdown behind
// a timer that has not fired yet.
func TestReconcileLoop_StopsBeforeInitialSweep(t *testing.T) {
	restoreDelay := reconcileInitialDelay
	reconcileInitialDelay = time.Hour
	t.Cleanup(func() { reconcileInitialDelay = restoreDelay })

	database, err := db.New("")
	if err != nil {
		t.Fatalf("db.New: %s", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	idx := &recordingIndex{}

	s := &Server{
		db:                 database,
		search:             idx,
		reconcileInterval:  time.Hour,
		reconcileBatchSize: 10,
		stopReconcile:      make(chan struct{}),
		reconcileStopped:   make(chan struct{}),
	}

	go s.reconcileLoop()

	close(s.stopReconcile)

	select {

	case <-s.reconcileStopped:

	case <-time.After(2 * time.Second):
		t.Fatal("reconcileLoop did not stop promptly while waiting out the initial delay")
	}

	if got := idx.indexedIds(); len(got) != 0 {
		t.Errorf("expected no sweep to have run, got %v", got)
	}
}

// TestReconcileOnce_StopsAtLoopTopBeforeFirstPage verifies the loop-top shutdown check (distinct
// from the pacing-delay check the other stop tests exercise): with stopReconcile already closed
// before reconcileOnce is even called, it must return immediately without reading a single page.
func TestReconcileOnce_StopsAtLoopTopBeforeFirstPage(t *testing.T) {
	database, err := db.New("")
	if err != nil {
		t.Fatalf("db.New: %s", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	if _, err := database.CreateMemory(context.Background(), types.Memory{Id: "m1", TimeStamp: 100, Significance: 5, Body: "x"}); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	idx := &recordingIndex{}

	s := &Server{
		db:                 database,
		search:             idx,
		reconcileBatchSize: 10,
		stopReconcile:      make(chan struct{}),
	}

	close(s.stopReconcile)

	s.reconcileOnce()

	if got := idx.indexedIds(); len(got) != 0 {
		t.Errorf("expected no memories indexed when already stopped before the first page, got %v", got)
	}
}

// failGetMemoriesPageStore wraps a real db.Store but forces GetMemoriesPage to fail, so
// reconcileOnce's page-read error arm (abandoning the sweep for the next one to retry) can be
// exercised without a broken database.
type failGetMemoriesPageStore struct {
	db.Store
	err error
}

func (f failGetMemoriesPageStore) GetMemoriesPage(ctx context.Context, afterId string, limit int) ([]types.Memory, error) {
	return nil, f.err
}

// TestReconcileOnce_PageReadErrorAbandonsSweep verifies a failing GetMemoriesPage is logged and
// abandons the sweep cleanly (no panic, nothing indexed) rather than propagating - the next sweep
// simply retries from the start.
func TestReconcileOnce_PageReadErrorAbandonsSweep(t *testing.T) {
	database, err := db.New("")
	if err != nil {
		t.Fatalf("db.New: %s", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	idx := &recordingIndex{}

	s := &Server{
		db:                 failGetMemoriesPageStore{Store: database, err: errors.New("page read boom")},
		search:             idx,
		reconcileBatchSize: 10,
		stopReconcile:      make(chan struct{}),
	}

	s.reconcileOnce()

	if got := idx.indexedIds(); len(got) != 0 {
		t.Errorf("expected no memories indexed after a page-read failure, got %v", got)
	}
}
