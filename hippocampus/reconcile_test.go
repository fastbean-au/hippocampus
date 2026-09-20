package hippocampus

import (
	"context"
	"errors"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/spf13/viper"

	"github.com/fastbean-au/hippocampus/db"
	"github.com/fastbean-au/hippocampus/search"
	"github.com/fastbean-au/hippocampus/types"
)

// recordingIndex is a search.Index that records the ids passed to IndexMemory, so a reconcile sweep
// can be observed without a real cluster. Every other method is an inert no-op.
//
// It also implements presenceProbe, because the sweep is gated on that capability: only a backend
// whose propagation can lose an operation has anything to reconcile.
type recordingIndex struct {
	mu      sync.Mutex
	indexed []string

	// held is the set of ids the index claims to hold. Nil means it holds nothing, so every memory
	// the sweep is shown is missing and gets indexed.
	held map[string]bool

	// absentErr makes the probe fail; absentCalls counts probe requests.
	absentErr   error
	absentCalls int
}

func (r *recordingIndex) AbsentIds(ctx context.Context, ids []string) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.absentCalls++

	if r.absentErr != nil {
		return nil, r.absentErr
	}

	out := make([]string, 0, len(ids))

	for _, id := range ids {
		if r.held[id] {
			continue
		}

		out = append(out, id)
	}

	return out, nil
}

func (r *recordingIndex) probeCalls() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.absentCalls
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
func (*recordingIndex) Search(ctx context.Context, q search.Query) ([]search.Hit, error) {
	return nil, nil
}

func (*recordingIndex) SupportsVectors() bool { return false }
func (*recordingIndex) Enabled() bool         { return true }
func (*recordingIndex) Close() error          { return nil }

// TestReconcileOnce_IndexesNonBinaryMemories verifies a sweep indexes every non-binary memory the
// index does not hold - the self-healing that recovers documents a dropped index operation missed -
// while skipping binary memories, whose bodies are opaque to content search. The index here holds
// nothing, so everything indexable is missing.
func TestReconcileOnce_IndexesNonBinaryMemories(t *testing.T) {
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

func (f failGetMemoriesPageStore) GetMemoriesPage(ctx context.Context, afterId string, limit int, groups []string) ([]types.Memory, error) {
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

// reconcileTestStore builds a store holding the given ids as plain text memories, ready for a sweep.
func reconcileTestStore(t *testing.T, ids ...string) *db.DB {
	t.Helper()

	database, err := db.New("")
	if err != nil {
		t.Fatalf("db.New: %s", err)
	}

	t.Cleanup(func() { _ = database.Close() })

	for _, id := range ids {
		if _, err := database.CreateMemory(context.Background(), types.Memory{
			Id:           id,
			TimeStamp:    100,
			Significance: 5,
			Body:         "body of " + id,
		}); err != nil {
			t.Fatalf("CreateMemory(%s): %s", id, err)
		}
	}

	return database
}

// TestReconcileOnce_IndexesOnlyWhatIsMissing is the finding behind item 125. The sweep used to
// re-index every live memory on every pass, which made it the dominant producer on the very queue it
// exists to compensate for - and, because a re-index is a delete plus an insert in Lucene even when
// the document has not changed, it tombstoned the whole index once per pass. It must now write only
// the documents the index does not hold.
func TestReconcileOnce_IndexesOnlyWhatIsMissing(t *testing.T) {
	restore := reconcilePageDelay
	reconcilePageDelay = time.Millisecond
	t.Cleanup(func() { reconcilePageDelay = restore })

	database := reconcileTestStore(t, "m1", "m2", "m3", "m4")

	idx := &recordingIndex{held: map[string]bool{"m1": true, "m3": true}}

	s := &Server{
		db:                 database,
		search:             idx,
		reconcileBatchSize: 10,
		stopReconcile:      make(chan struct{}),
	}

	s.reconcileOnce()

	got := idx.indexedIds()
	want := []string{"m2", "m4"}

	if len(got) != len(want) {
		t.Fatalf("indexed %v, want only the missing %v", got, want)
	}

	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("indexed %v, want only the missing %v", got, want)
		}
	}
}

// TestReconcileOnce_AsksOncePerPage pins the cost of asking: one probe request per page of memories,
// not one per memory. A probe per memory would trade a write round trip for a read one and leave the
// sweep as expensive as it was.
func TestReconcileOnce_AsksOncePerPage(t *testing.T) {
	restore := reconcilePageDelay
	reconcilePageDelay = time.Millisecond
	t.Cleanup(func() { reconcilePageDelay = restore })

	database := reconcileTestStore(t, "m1", "m2", "m3", "m4", "m5", "m6")

	// Everything is held, so the sweep writes nothing at all - which is the steady state on a healthy
	// index, and the whole point.
	idx := &recordingIndex{held: map[string]bool{
		"m1": true, "m2": true, "m3": true, "m4": true, "m5": true, "m6": true,
	}}

	s := &Server{
		db:                 database,
		search:             idx,
		reconcileBatchSize: 3, // two pages of three
		stopReconcile:      make(chan struct{}),
	}

	s.reconcileOnce()

	if got := idx.indexedIds(); len(got) != 0 {
		t.Errorf("a sweep over a complete index wrote %v; it must write nothing", got)
	}

	// Two full pages, then the short page that ends the walk: the third read returns nothing and is
	// never probed.
	if got := idx.probeCalls(); got != 2 {
		t.Errorf("the sweep made %d probe requests over two pages, want 2", got)
	}
}

// TestReconcileOnce_ProbeFailureIndexesNothing pins the deliberate absence of a fallback. A probe
// fails for the same reasons an index write does - an unreachable or overloaded cluster - so
// re-indexing the page anyway would offer a full page of writes at exactly the moment nothing can be
// applied, which is the behaviour this change removes.
func TestReconcileOnce_ProbeFailureIndexesNothing(t *testing.T) {
	restore := reconcilePageDelay
	reconcilePageDelay = time.Millisecond
	t.Cleanup(func() { reconcilePageDelay = restore })

	database := reconcileTestStore(t, "m1", "m2")

	idx := &recordingIndex{absentErr: errors.New("cluster unreachable")}

	s := &Server{
		db:                 database,
		search:             idx,
		reconcileBatchSize: 10,
		stopReconcile:      make(chan struct{}),
	}

	s.reconcileOnce()

	if got := idx.indexedIds(); len(got) != 0 {
		t.Errorf("a failed probe indexed %v; it must index nothing and leave it to the next sweep", got)
	}
}

// TestReconcileOnce_BinaryMemoriesAreNeverProbed pins that a binary memory is dropped before the
// probe rather than after it. It has no document to be missing, so asking about it would report it
// absent every sweep, for ever - the sweep would then index it, which is the one thing content search
// must never hold.
func TestReconcileOnce_BinaryMemoriesAreNeverProbed(t *testing.T) {
	restore := reconcilePageDelay
	reconcilePageDelay = time.Millisecond
	t.Cleanup(func() { reconcilePageDelay = restore })

	database := reconcileTestStore(t, "m1")

	if _, err := database.CreateMemory(context.Background(), types.Memory{
		Id:           "b1",
		TimeStamp:    100,
		Significance: 5,
		Body:         "opaque",
		IsBinary:     true,
	}); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	probed := make([]string, 0, 2)

	idx := &probeRecordingIndex{
		recordingIndex: recordingIndex{held: map[string]bool{"m1": true}},
		onProbe:        func(ids []string) { probed = append(probed, ids...) },
	}

	s := &Server{
		db:                 database,
		search:             idx,
		reconcileBatchSize: 10,
		stopReconcile:      make(chan struct{}),
	}

	s.reconcileOnce()

	for _, id := range probed {
		if id == "b1" {
			t.Fatal("the sweep asked the index about a binary memory, which is never indexed")
		}
	}

	if got := idx.indexedIds(); len(got) != 0 {
		t.Errorf("the sweep indexed %v, want nothing", got)
	}
}

// probeRecordingIndex records which ids the probe was asked about, which recordingIndex's own
// accounting (a count and a held set) cannot show.
type probeRecordingIndex struct {
	recordingIndex

	onProbe func(ids []string)
}

func (p *probeRecordingIndex) AbsentIds(ctx context.Context, ids []string) ([]string, error) {
	if p.onProbe != nil {
		p.onProbe(ids)
	}

	return p.recordingIndex.AbsentIds(ctx, ids)
}

// TestStartReconcile_SkipsTheStoreBackedIndex pins the gating, which is a fix rather than tidiness.
// The SQL backend keeps its content index inside the primary write - its own doc comment says it has
// "no reconciliation sweep to run" - yet startReconcile launched one, so every deployment on the
// default content index paged its whole store every hour to call a no-op on each row.
//
// It asserts against the real backend, not a fake, so adding a no-op AbsentIds to search.SQL would
// fail here rather than quietly reinstating the sweep.
func TestStartReconcile_SkipsTheStoreBackedIndex(t *testing.T) {
	database := reconcileTestStore(t)

	idx, err := search.NewSQL(database)
	if err != nil {
		t.Fatalf("search.NewSQL: %s", err)
	}

	if !idx.Enabled() {
		t.Fatal("the store-backed index reports itself disabled; this test would pass for the wrong reason")
	}

	viper.Set("consolidation.enabled", true)
	viper.Set("opensearch.reconcileIntervalSeconds", 3600)

	t.Cleanup(func() { viper.Set("opensearch.reconcileIntervalSeconds", 0) })

	s := &Server{db: database, search: idx, consolidationEnabled: true}

	s.startReconcile(idx)

	if s.stopReconcile != nil {
		t.Error("startReconcile launched a sweep for a backend that indexes inside the primary write")
	}
}
