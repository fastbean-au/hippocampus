package hippocampus

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/spf13/viper"

	"github.com/fastbean-au/hippocampus/db"
	"github.com/fastbean-au/hippocampus/search"
	"github.com/fastbean-au/hippocampus/types"
)

// The outbox's wiring - which deployments record, which drain, and what the drain does when the
// store underneath it fails - had no test of its own. outbox_test.go drives drainOutboxOnce and
// staleSweep directly against a Server a test built by hand, which is the right shape for the
// mechanism but leaves startOutboxDrain's five arms and the loop that calls it unexecuted. Those
// arms are the ones that decide whether a deployment queues deletions nothing will ever drain.

// nonSyncingIndex is an enabled backend that implements neither optional capability - the SQLite
// FTS and no-op backends' shape, whose deletes are transactional and so have nothing to queue.
type nonSyncingIndex struct {
	recordingIndex
}

// disabledIndex stands in for a store carrying no index at all.
type disabledIndex struct {
	recordingIndex
}

func (*disabledIndex) Enabled() bool { return false }

// storeWithoutOutbox is a db.Store that does not expose SetSearchOutbox, so the type assertion in
// startOutboxDrain fails - the shape any future non-SQL backend would have.
type storeWithoutOutbox struct {
	db.Store
}

// outboxFaultStore forces each of the three storage calls the drain makes to fail independently,
// which a real store cannot be made to do selectively: a closed database fails the first call and
// so hides the two branches behind it.
type outboxFaultStore struct {
	db.Store

	claimErr   error
	confirmErr error
	pruneErr   error
	depthErr   error
}

func (o *outboxFaultStore) ClaimSearchDeletes(ctx context.Context, limit int) ([]db.SearchOutboxEntry, error) {
	if o.claimErr != nil {

		return nil, o.claimErr
	}

	return o.Store.ClaimSearchDeletes(ctx, limit)
}

func (o *outboxFaultStore) ConfirmSearchDeletes(ctx context.Context, seqs []int64) error {
	if o.confirmErr != nil {

		return o.confirmErr
	}

	return o.Store.ConfirmSearchDeletes(ctx, seqs)
}

func (o *outboxFaultStore) PruneSearchOutbox(ctx context.Context, bounds db.QueueBounds) (int64, error) {
	if o.pruneErr != nil {

		return 0, o.pruneErr
	}

	return o.Store.PruneSearchOutbox(ctx, bounds)
}

func (o *outboxFaultStore) SearchOutboxDepth(ctx context.Context) (int64, error) {
	if o.depthErr != nil {

		return 0, o.depthErr
	}

	return o.Store.SearchOutboxDepth(ctx)
}

// missingFaultStore forces the primary-store lookup the stale sweep depends on to fail, so the
// sweep abandons its pass rather than deleting on an answer it did not get.
type missingFaultStore struct {
	db.Store

	err error
}

func (m *missingFaultStore) MissingMemoryIds(ctx context.Context, ids []string) ([]string, error) {
	if m.err != nil {

		return nil, m.err
	}

	return m.Store.MissingMemoryIds(ctx, ids)
}

// outboxDrainServer builds the Server startOutboxDrain is called on, with the viper keys that
// function reads set to whatever the case wants.
func outboxDrainServer(t *testing.T, store db.Store, consolidating bool) *Server {
	t.Helper()

	return &Server{
		db:                   store,
		consolidationEnabled: consolidating,
	}
}

// TestStartOutboxDrainDefaultsTheCaps covers the cap resolution, which runs before every early
// return: a queue nothing bounds is the one failure mode this whole file exists to avoid, so the
// defaults must land even on the arms that never start a drain.
func TestStartOutboxDrainDefaultsTheCaps(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)

	s := outboxDrainServer(t, nil, false)

	s.startOutboxDrain(nil)

	if s.outboxBounds.MaxRows != defaultOutboxMaxRows {
		t.Errorf("expected the default row cap %d, got %d", defaultOutboxMaxRows, s.outboxBounds.MaxRows)
	}

	if s.outboxBounds.MaxAge != defaultOutboxMaxAgeHours*time.Hour {
		t.Errorf("expected the default age cap, got %s", s.outboxBounds.MaxAge)
	}

	viper.Set("opensearch.outbox.maxRows", 42)
	viper.Set("opensearch.outbox.maxAgeHours", 3)

	s = outboxDrainServer(t, nil, false)
	s.startOutboxDrain(nil)

	if s.outboxBounds.MaxRows != 42 {
		t.Errorf("expected the configured row cap 42, got %d", s.outboxBounds.MaxRows)
	}

	if s.outboxBounds.MaxAge != 3*time.Hour {
		t.Errorf("expected the configured age cap, got %s", s.outboxBounds.MaxAge)
	}
}

// TestStartOutboxDrainRecordsNothingWithoutABackendThatNeedsIt walks the three arms that must leave
// the recording OFF. Each is a deployment where a queued row would never be read, and the point of
// enabling the recording and the drain in one function is that this cannot drift apart.
func TestStartOutboxDrainRecordsNothingWithoutABackendThatNeedsIt(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)

	cases := []struct {
		name  string
		index search.Index
		store func(t *testing.T) db.Store
	}{
		{
			name:  "no index at all",
			index: nil,
		},
		{
			name:  "an index that is switched off",
			index: &disabledIndex{},
		},
		{
			name:  "a backend whose deletes are already transactional",
			index: &nonSyncingIndex{},
		},
		{
			name:  "a store that cannot record",
			index: &syncingIndex{},
			store: func(t *testing.T) db.Store {
				t.Helper()

				return &storeWithoutOutbox{Store: mustOutboxStore(t)}
			},
		},
	}

	for _, v := range cases {
		t.Run(v.name, func(t *testing.T) {
			var store db.Store

			if v.store != nil {
				store = v.store(t)
			} else {
				store = mustOutboxStore(t)
			}

			s := outboxDrainServer(t, store, true)
			s.startOutboxDrain(v.index)

			if s.stopOutbox != nil {
				t.Error("no drain goroutine should have been started")
			}

			// A store told to record would queue a row here; one that was not, does not.
			if database, ok := store.(*db.DB); ok {
				deleteOneMemory(t, database)

				depth, err := database.SearchOutboxDepth(context.Background())
				if err != nil {
					t.Fatalf("SearchOutboxDepth: %s", err)
				}

				if depth != 0 {
					t.Errorf("expected nothing queued, got a depth of %d", depth)
				}
			}
		})
	}
}

// TestStartOutboxDrainOnAReplicaRecordsWithoutDraining is the asymmetry: a replica serves writes, so
// its deletes are as losable as anyone's and must be queued, but the queue is single and only the
// consolidating instance may claim from it.
func TestStartOutboxDrainOnAReplicaRecordsWithoutDraining(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)

	database := mustOutboxStore(t)
	s := outboxDrainServer(t, database, false)

	s.startOutboxDrain(&syncingIndex{})

	if s.stopOutbox != nil {
		t.Fatal("a replica must not start a drain goroutine")
	}

	deleteOneMemory(t, database)

	depth, err := database.SearchOutboxDepth(context.Background())
	if err != nil {
		t.Fatalf("SearchOutboxDepth: %s", err)
	}

	if depth != 1 {
		t.Errorf("a replica must still record its deletions, got a depth of %d", depth)
	}
}

// TestStartOutboxDrainOnTheConsolidatorDrains is the other half, end to end through the goroutine
// rather than by calling drainOutboxOnce: the loop is what turns a queued row into a deleted
// document without anybody asking, and its delay selection and shutdown are only reachable here.
func TestStartOutboxDrainOnTheConsolidatorDrains(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)

	restoreIdle, restoreBusy := outboxIdleDelay, outboxBusyDelay
	outboxIdleDelay, outboxBusyDelay = time.Millisecond, time.Millisecond

	t.Cleanup(func() { outboxIdleDelay, outboxBusyDelay = restoreIdle, restoreBusy })

	database := mustOutboxStore(t)
	idx := &syncingIndex{}
	s := outboxDrainServer(t, database, true)
	s.search = idx

	s.startOutboxDrain(idx)

	if s.stopOutbox == nil || s.outboxStopped == nil {
		t.Fatal("the consolidating instance must start a drain goroutine")
	}

	deleteOneMemory(t, database)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(idx.deletedIds()) > 0 {

			break
		}

		time.Sleep(2 * time.Millisecond)
	}

	if got := idx.deletedIds(); len(got) != 1 || got[0] != "m1" {
		t.Errorf("expected the queued deletion applied by the loop, got %v", got)
	}

	close(s.stopOutbox)

	select {

	case <-s.outboxStopped:

	case <-time.After(5 * time.Second):
		t.Fatal("the drain loop did not stop")

	}
}

// TestOutboxDrainLoopStopsWhenTheBackendCannotSync covers the loop's own guard. startOutboxDrain
// checks the same capability, so this arm is only reachable if the index is swapped afterwards -
// but it is the difference between a goroutine that exits and one that spins on a nil assertion.
func TestOutboxDrainLoopStopsWhenTheBackendCannotSync(t *testing.T) {
	s := &Server{
		search:        &nonSyncingIndex{},
		stopOutbox:    make(chan struct{}),
		outboxStopped: make(chan struct{}),
	}

	go s.outboxDrainLoop()

	select {

	case <-s.outboxStopped:

	case <-time.After(5 * time.Second):
		t.Fatal("the drain loop must exit when the backend cannot sync deletes")

	}
}

// TestDrainOutboxOnceSurfacesStorageFailures covers the three storage calls the drain makes, each
// forced to fail on its own. All three are deliberately non-fatal - the queue is at-least-once, so
// a failed pass costs a retry - and what this asserts is that none of them is counted as applied,
// because a pass that did not finish must not shorten the next one's delay.
func TestDrainOutboxOnceSurfacesStorageFailures(t *testing.T) {
	t.Run("the claim fails", func(t *testing.T) {
		database := mustOutboxStore(t)
		store := &outboxFaultStore{Store: database, claimErr: errors.New("boom")}
		s := &Server{db: store, outboxBounds: db.QueueBounds{MaxAge: time.Hour, MaxRows: 10}}

		if applied := s.drainOutboxOnce(&syncingIndex{}); applied != 0 {
			t.Errorf("expected nothing applied, got %d", applied)
		}
	})

	t.Run("the confirmation fails", func(t *testing.T) {
		database := mustOutboxStore(t)
		store := &outboxFaultStore{Store: database, confirmErr: errors.New("boom")}
		s := &Server{db: store, outboxBounds: db.QueueBounds{MaxAge: time.Hour, MaxRows: 10}}

		database.SetSearchOutbox(true)
		deleteOneMemory(t, database)

		idx := &syncingIndex{}

		if applied := s.drainOutboxOnce(idx); applied != 0 {
			t.Errorf("a pass whose bookkeeping failed must not report work done, got %d", applied)
		}

		// The deletion did land - only the confirmation did not - so the row stays queued and the
		// next pass re-applies it, which is harmless against an already-absent document.
		if got := idx.deletedIds(); len(got) != 1 {
			t.Errorf("expected the deletion to have been applied, got %v", got)
		}
	})

	t.Run("the prune fails on an idle pass", func(t *testing.T) {
		database := mustOutboxStore(t)
		store := &outboxFaultStore{Store: database, pruneErr: errors.New("boom")}
		s := &Server{db: store, outboxBounds: db.QueueBounds{MaxAge: time.Hour, MaxRows: 10}}

		if applied := s.drainOutboxOnce(&syncingIndex{}); applied != 0 {
			t.Errorf("expected nothing applied on an empty queue, got %d", applied)
		}
	})

	t.Run("the depth read fails on an idle pass", func(t *testing.T) {
		database := mustOutboxStore(t)
		store := &outboxFaultStore{Store: database, depthErr: errors.New("boom")}
		s := &Server{db: store, outboxBounds: db.QueueBounds{MaxAge: time.Hour, MaxRows: 10}}

		if applied := s.drainOutboxOnce(&syncingIndex{}); applied != 0 {
			t.Errorf("expected nothing applied on an empty queue, got %d", applied)
		}
	})
}

// TestStaleSweepAbandonsWhenTheStoreCannotBeAsked covers the two failures inside the sweep's page
// loop. Both abandon the pass rather than continuing, which matters: the sweep DELETES, and
// carrying on past an unanswered "does this still exist" would remove documents on no evidence.
func TestStaleSweepAbandonsWhenTheStoreCannotBeAsked(t *testing.T) {
	restore := reconcilePageDelay
	reconcilePageDelay = time.Millisecond

	t.Cleanup(func() { reconcilePageDelay = restore })

	t.Run("the existence check fails", func(t *testing.T) {
		idx := &syncingIndex{documents: []indexedDoc{{id: "a", timestamp: 1}, {id: "b", timestamp: 2}}}
		s, database := outboxServer(t, idx)
		s.db = &missingFaultStore{Store: database, err: errors.New("boom")}

		s.staleSweep(context.Background())

		if got := idx.deletedIds(); len(got) != 0 {
			t.Errorf("an abandoned pass must delete nothing, got %v", got)
		}
	})

	t.Run("the removal fails", func(t *testing.T) {
		idx := &syncingIndex{
			documents: []indexedDoc{{id: "gone-a", timestamp: 1}, {id: "gone-b", timestamp: 2}},
			deleteErr: errors.New("boom"),
		}

		s, _ := outboxServer(t, idx)

		s.staleSweep(context.Background())

		if got := idx.heldIds(); len(got) != 2 {
			t.Errorf("a failed removal must leave the documents in place, got %v", got)
		}
	})
}

// TestStaleSweepIgnoresABackendThatCannotDelete is the second capability guard, the counterpart to
// the enumeration one already covered: a backend that can list its ids but not delete them
// synchronously has nothing this sweep can do.
func TestStaleSweepIgnoresABackendThatCannotDelete(t *testing.T) {
	s, _ := outboxServer(t, &enumerateOnlyIndex{})

	s.staleSweep(context.Background())
}

// enumerateOnlyIndex implements the enumeration half and not the delete half.
type enumerateOnlyIndex struct {
	recordingIndex
}

func (*enumerateOnlyIndex) EnumerateIdsPage(ctx context.Context, cursor search.IndexCursor, size int) (search.IndexPage, error) {
	return search.IndexPage{Done: true}, nil
}

// mustOutboxStore opens an in-memory store recording index deletions, holding one memory the tests
// above delete to produce a queued row.
func mustOutboxStore(t *testing.T) *db.DB {
	t.Helper()

	database, err := db.New("")
	if err != nil {
		t.Fatalf("db.New: %s", err)
	}

	t.Cleanup(func() { _ = database.Close() })

	if _, err := database.CreateMemory(context.Background(), types.Memory{
		Id:           "m1",
		Body:         "x",
		TimeStamp:    1,
		Significance: 1,
	}); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	return database
}

// deleteOneMemory removes the seeded memory, which is what queues a row when the store is recording.
func deleteOneMemory(t *testing.T, database *db.DB) {
	t.Helper()

	if _, err := database.DeleteMemories(context.Background(), []string{"m1"}); err != nil {
		t.Fatalf("DeleteMemories: %s", err)
	}
}
