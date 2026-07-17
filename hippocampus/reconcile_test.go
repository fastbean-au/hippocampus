package hippocampus

import (
	"context"
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
