package hippocampus

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/fastbean-au/hippocampus/db"
	"github.com/fastbean-au/hippocampus/search"
	"github.com/fastbean-au/hippocampus/types"
)

// syncingIndex is a recordingIndex that also implements the two optional capabilities the drain and
// the stale sweep look for: a synchronous delete and an id enumeration. It stands in for the
// OpenSearch backend, which is the only real implementation of either.
// indexedDoc is what the fake index holds: an id and the timestamp the enumeration cursors on.
type indexedDoc struct {
	id        string
	timestamp int64
}

type syncingIndex struct {
	recordingIndex

	mu        sync.Mutex
	documents []indexedDoc
	deleted   []string

	deleteErr    error
	enumerateErr error
}

func (s *syncingIndex) DeleteMemoriesSync(ctx context.Context, ids []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.deleteErr != nil {

		return s.deleteErr
	}

	s.deleted = append(s.deleted, ids...)

	gone := map[string]bool{}

	for _, v := range ids {
		gone[v] = true
	}

	kept := s.documents[:0]

	for _, v := range s.documents {
		if !gone[v.id] {
			kept = append(kept, v)
		}
	}

	s.documents = kept

	return nil
}

func (s *syncingIndex) EnumerateIdsPage(ctx context.Context, cursor search.IndexCursor, size int) (search.IndexPage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.enumerateErr != nil {

		return search.IndexPage{}, s.enumerateErr
	}

	// Ordered by timestamp then id: the real backend sorts on timestamp alone and breaks ties
	// arbitrarily, but a stable order here is what lets a test reason about where a page ends.
	docs := make([]indexedDoc, len(s.documents))
	copy(docs, s.documents)

	sort.Slice(docs, func(i int, j int) bool {
		if docs[i].timestamp != docs[j].timestamp {

			return docs[i].timestamp < docs[j].timestamp
		}

		return docs[i].id < docs[j].id
	})

	// The inclusive range and the offset within it, exactly as the query expresses them.
	matching := make([]indexedDoc, 0, len(docs))

	for _, v := range docs {
		if v.timestamp >= cursor.Timestamp {
			matching = append(matching, v)
		}
	}

	if cursor.Offset < len(matching) {
		matching = matching[cursor.Offset:]
	} else {
		matching = nil
	}

	if len(matching) == 0 {

		return search.IndexPage{Done: true}, nil
	}

	full := len(matching) >= size

	if full {
		matching = matching[:size]
	}

	last := matching[len(matching)-1].timestamp

	// Hold back the trailing group sharing the final timestamp, so the page ends on a boundary the
	// caller's own deletions cannot move.
	keep := len(matching)

	if full {
		for keep > 0 && matching[keep-1].timestamp == last {
			keep--
		}
	}

	if keep == 0 {
		offset := len(matching)

		if last == cursor.Timestamp {
			offset += cursor.Offset
		}

		return search.IndexPage{
			Ids:     idsOf(matching),
			Next:    search.IndexCursor{Timestamp: last, Offset: offset},
			Partial: true,
		}, nil
	}

	return search.IndexPage{
		Ids:  idsOf(matching[:keep]),
		Next: search.IndexCursor{Timestamp: matching[keep-1].timestamp + 1},
	}, nil
}

// idsOf pulls the ids out of a run of indexed documents.
func idsOf(docs []indexedDoc) []string {
	out := make([]string, 0, len(docs))

	for _, v := range docs {
		out = append(out, v.id)
	}

	return out
}

func (s *syncingIndex) deletedIds() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]string, len(s.deleted))
	copy(out, s.deleted)
	sort.Strings(out)

	return out
}

func (s *syncingIndex) heldIds() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]string, 0, len(s.documents))

	for _, v := range s.documents {
		out = append(out, v.id)
	}

	sort.Strings(out)

	return out
}

// outboxServer builds a Server over an in-memory store with the outbox recording, an index that can
// be drained, and the caps a test wants.
func outboxServer(t *testing.T, idx search.Index) (*Server, *db.DB) {
	t.Helper()

	database, err := db.New("")
	if err != nil {
		t.Fatalf("db.New: %s", err)
	}

	t.Cleanup(func() { _ = database.Close() })

	database.SetSearchOutbox(true)

	return &Server{
		db:                 database,
		search:             idx,
		reconcileBatchSize: 2,
		stopReconcile:      make(chan struct{}),
		stopOutbox:         make(chan struct{}),
		outboxMaxRows:      1000,
		outboxMaxAge:       time.Hour,
	}, database
}

// TestDrainAppliesQueuedDeletions is the mechanism end-to-end: deleting a memory records the index
// deletion in the same transaction, and the drain applies it. The whole point is that no step
// between the two can drop it.
func TestDrainAppliesQueuedDeletions(t *testing.T) {
	idx := &syncingIndex{}
	s, database := outboxServer(t, idx)
	ctx := context.Background()

	for _, id := range []string{"d1", "d2", "d3"} {
		if _, err := database.CreateMemory(ctx, types.Memory{
			Id: id, Body: "body", Significance: 10, TimeStamp: time.Now().UnixNano(),
		}); err != nil {
			t.Fatalf("CreateMemory(%s): %s", id, err)
		}
	}

	if _, err := database.DeleteMemories(ctx, []string{"d1", "d3"}); err != nil {
		t.Fatalf("DeleteMemories: %s", err)
	}

	if applied := s.drainOutboxOnce(idx); applied != 2 {
		t.Fatalf("drained %d deletions, want 2", applied)
	}

	got := idx.deletedIds()

	if len(got) != 2 || got[0] != "d1" || got[1] != "d3" {
		t.Fatalf("applied deletions for %v, want [d1 d3]", got)
	}

	// And the queue is empty afterwards, so the work is not repeated forever.
	if depth, err := database.SearchOutboxDepth(ctx); err != nil || depth != 0 {
		t.Fatalf("outbox depth after a successful drain: got %d (err %v), want 0", depth, err)
	}
}

// TestDrainKeepsWorkQueuedWhenTheIndexRefuses is the at-least-once contract, and the difference
// between the outbox and the in-memory queue it replaced: an index that cannot take the deletion now
// must still be given it later, rather than the deletion being forgotten at the point of failure.
func TestDrainKeepsWorkQueuedWhenTheIndexRefuses(t *testing.T) {
	idx := &syncingIndex{deleteErr: errors.New("cluster unreachable")}
	s, database := outboxServer(t, idx)
	ctx := context.Background()

	if _, err := database.CreateMemory(ctx, types.Memory{
		Id: "stubborn", Body: "body", Significance: 10, TimeStamp: time.Now().UnixNano(),
	}); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	if _, err := database.DeleteMemories(ctx, []string{"stubborn"}); err != nil {
		t.Fatalf("DeleteMemories: %s", err)
	}

	if applied := s.drainOutboxOnce(idx); applied != 0 {
		t.Fatalf("drained %d deletions against a failing index, want 0", applied)
	}

	if depth, err := database.SearchOutboxDepth(ctx); err != nil || depth != 1 {
		t.Fatalf("outbox depth after a failed apply: got %d (err %v), want 1 - the deletion must survive to be retried", depth, err)
	}

	// The cluster comes back; the next pass applies what was held.
	idx.mu.Lock()
	idx.deleteErr = nil
	idx.mu.Unlock()

	if applied := s.drainOutboxOnce(idx); applied != 1 {
		t.Fatalf("drained %d deletions after recovery, want 1", applied)
	}

	if got := idx.deletedIds(); len(got) != 1 || got[0] != "stubborn" {
		t.Fatalf("applied deletions for %v, want [stubborn]", got)
	}
}

// TestPruneAbandonsQueuedDeletionsAtTheCaps covers the escape valve: an index that never recovers
// must not grow the queue without bound. What is discarded becomes the stale sweep's to find, which
// is why abandoning it is survivable at all.
func TestPruneAbandonsQueuedDeletionsAtTheCaps(t *testing.T) {
	idx := &syncingIndex{deleteErr: errors.New("cluster unreachable")}
	s, database := outboxServer(t, idx)
	ctx := context.Background()

	for _, id := range []string{"c1", "c2", "c3"} {
		if _, err := database.CreateMemory(ctx, types.Memory{
			Id: id, Body: "body", Significance: 10, TimeStamp: time.Now().UnixNano(),
		}); err != nil {
			t.Fatalf("CreateMemory(%s): %s", id, err)
		}
	}

	if _, err := database.DeleteMemories(ctx, []string{"c1", "c2", "c3"}); err != nil {
		t.Fatalf("DeleteMemories: %s", err)
	}

	s.outboxMaxRows = 1
	s.pruneOutbox(ctx)

	depth, err := database.SearchOutboxDepth(ctx)
	if err != nil {
		t.Fatalf("SearchOutboxDepth: %s", err)
	}

	if depth != 1 {
		t.Fatalf("outbox depth after pruning to a cap of 1: got %d, want 1", depth)
	}
}

// TestStaleSweepRemovesDocumentsTheStoreNoLongerHas is the backstop this whole change turns on: an
// index holding documents for memories that are gone must converge back on the store, whatever put
// them there - a dropped delete, a deletion abandoned at the caps, or divergence that predates the
// outbox entirely.
func TestStaleSweepRemovesDocumentsTheStoreNoLongerHas(t *testing.T) {
	restore := reconcilePageDelay
	reconcilePageDelay = time.Millisecond

	t.Cleanup(func() { reconcilePageDelay = restore })

	// The index holds five documents; the store only ever had two of them. Three share one
	// timestamp, which is the case the cursor's offset exists for - a page boundary landing inside a
	// group of identical timestamps must not step over the rest of the group.
	idx := &syncingIndex{documents: []indexedDoc{
		{id: "live-1", timestamp: 100},
		{id: "ghost-1", timestamp: 200},
		{id: "ghost-2", timestamp: 200},
		{id: "ghost-3", timestamp: 200},
		{id: "live-2", timestamp: 300},
	}}
	s, database := outboxServer(t, idx)
	ctx := context.Background()

	for _, id := range []string{"live-1", "live-2"} {
		if _, err := database.CreateMemory(ctx, types.Memory{
			Id: id, Body: "body", Significance: 10, TimeStamp: time.Now().UnixNano(),
		}); err != nil {
			t.Fatalf("CreateMemory(%s): %s", id, err)
		}
	}

	s.staleSweep(ctx)

	held := idx.heldIds()

	if len(held) != 2 || held[0] != "live-1" || held[1] != "live-2" {
		t.Fatalf("index holds %v after the stale sweep, want [live-1 live-2]", held)
	}

	// And a second pass finds nothing left to do, so the sweep converges rather than oscillating.
	before := len(idx.deletedIds())

	s.staleSweep(ctx)

	if after := len(idx.deletedIds()); after != before {
		t.Fatalf("a second stale sweep removed %d more documents; it must converge", after-before)
	}
}

// TestStaleSweepStopsPromptlyOnShutdown pins that the sweep is interruptible. It enumerates the
// whole index, which on the deployment that motivated this change was several million documents, so
// a sweep that ran to completion regardless would hold shutdown open for as long as it took.
func TestStaleSweepStopsPromptlyOnShutdown(t *testing.T) {
	restore := reconcilePageDelay
	reconcilePageDelay = time.Hour

	t.Cleanup(func() { reconcilePageDelay = restore })

	idx := &syncingIndex{documents: []indexedDoc{
		{id: "a", timestamp: 1}, {id: "b", timestamp: 2},
		{id: "c", timestamp: 3}, {id: "d", timestamp: 4},
	}}

	s, _ := outboxServer(t, idx)

	close(s.stopReconcile)

	done := make(chan struct{})

	go func() {
		s.staleSweep(context.Background())
		close(done)
	}()

	select {

	case <-done:

	case <-time.After(2 * time.Second):
		t.Fatal("staleSweep did not stop promptly on shutdown")
	}
}

// TestStaleSweepAbandonsOnAnEnumerationFailure keeps a cluster failure from becoming a data-loss
// path: if the index cannot be listed, the sweep must stop rather than reason about a partial answer.
func TestStaleSweepAbandonsOnAnEnumerationFailure(t *testing.T) {
	idx := &syncingIndex{
		documents:    []indexedDoc{{id: "a", timestamp: 1}, {id: "b", timestamp: 2}},
		enumerateErr: errors.New("cluster unreachable"),
	}

	s, _ := outboxServer(t, idx)

	s.staleSweep(context.Background())

	if got := idx.deletedIds(); len(got) != 0 {
		t.Fatalf("removed %v after failing to enumerate the index; it must remove nothing", got)
	}
}

// TestStaleSweepIgnoresABackendThatCannotEnumerate covers the optional-interface gate: the SQLite
// FTS and no-op backends implement neither capability, and the sweep must simply not run rather
// than fail.
func TestStaleSweepIgnoresABackendThatCannotEnumerate(t *testing.T) {
	s, _ := outboxServer(t, &recordingIndex{})

	// Nothing to assert but that it returns: the point is that it neither panics nor blocks.
	s.staleSweep(context.Background())
}

// TestStaleSweepConvergesUnderTimestampCollisions is the guard the two enumeration bugs found during
// development would both have failed, and it is worth more than either specific case.
//
// The sweep pages an index that has no snapshot behind it, while deleting from that same index as it
// goes - so every removal shifts what follows. Enumerating on the memory timestamp (chosen because
// sorting on _id costs heap fielddata proportional to the index) means page boundaries land inside
// groups of documents sharing an instant, which is exactly where a shifted position steps over
// something. A skip there is silent: the document is simply never looked at, and the pass reports
// success.
//
// So this drives deliberately colliding timestamps - far more documents per instant than fit in a
// page - at several page sizes, and demands the only outcome that means anything: every stale
// document gone, every live one still there.
func TestStaleSweepConvergesUnderTimestampCollisions(t *testing.T) {
	restore := reconcilePageDelay
	reconcilePageDelay = time.Millisecond

	t.Cleanup(func() { reconcilePageDelay = restore })

	for _, batch := range []int{1, 2, 3, 7, 50} {
		t.Run(fmt.Sprintf("batch-%d", batch), func(t *testing.T) {
			// Sixty documents over six instants, so ten share each - more than most of the page
			// sizes above, which is what forces the unaligned path. Every third is live.
			documents := make([]indexedDoc, 0, 60)
			live := map[string]bool{}

			for i := range 60 {
				id := fmt.Sprintf("doc-%02d", i)
				documents = append(documents, indexedDoc{id: id, timestamp: int64(100 * (i / 10))})

				if i%3 == 0 {
					live[id] = true
				}
			}

			idx := &syncingIndex{documents: documents}
			s, database := outboxServer(t, idx)
			ctx := context.Background()

			s.reconcileBatchSize = batch

			for _, v := range documents {
				if !live[v.id] {
					continue
				}

				if _, err := database.CreateMemory(ctx, types.Memory{
					Id: v.id, Body: "body", Significance: 10, TimeStamp: time.Now().UnixNano(),
				}); err != nil {
					t.Fatalf("CreateMemory(%s): %s", v.id, err)
				}
			}

			s.staleSweep(ctx)

			held := map[string]bool{}

			for _, id := range idx.heldIds() {
				held[id] = true
			}

			for _, v := range documents {
				switch {

				case live[v.id] && !held[v.id]:
					t.Errorf("%s is still stored but its document was removed", v.id)

				case !live[v.id] && held[v.id]:
					t.Errorf("%s was forgotten but its document survived the sweep", v.id)

				}
			}
		})
	}
}
