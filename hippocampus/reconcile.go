package hippocampus

import (
	"context"
	"time"

	log "github.com/sirupsen/logrus"
)

// defaultReconcileBatchSize is the page size the reconciliation sweep reads the primary store in
// when opensearch.reconcileBatchSize is unset or non-positive.
const defaultReconcileBatchSize = 500

// reconcileInitialDelay is how long after startup the first sweep runs. Firing after a short delay
// rather than immediately lets the service settle first, while still healing a sparse index soon
// after a restart rather than waiting a whole interval. A var so tests can shorten it.
var reconcileInitialDelay = 60 * time.Second

// reconcilePageDelay paces a sweep: after enqueuing each page of re-index operations it waits this
// long, so a full-store sweep trickles into the asynchronous index queue instead of flooding it.
// Overflowing the queue would be self-correcting anyway (the next sweep re-indexes whatever was
// dropped), so this only smooths the load. A var so tests can shorten it.
var reconcilePageDelay = 200 * time.Millisecond

// reconcileLoop periodically re-indexes every live memory into the search index, healing documents
// that never landed - operations dropped under queue overflow, lost to a crash before the worker
// drained, or missed while the cluster was unreachable. Re-indexing is idempotent (each document is
// keyed by memory id), and the index is strictly secondary, so a sweep runs beside live traffic
// without coordinating with it: the worst case is briefly re-writing a document a concurrent write
// also wrote, which converges.
//
// It runs in both directions. The forward pass heals *missing* documents, which is all it ever did:
// the memory is still there to re-index. The stale pass (staleSweep, in outbox.go) enumerates the
// index and removes documents the primary store no longer holds - the direction that used to be the
// job of --backfill-search --reindex, on the argument that a stale document is harmless because
// SearchMemories re-verifies every hit against the primary store. Harmless to a caller, yes; not
// harmless to the cluster, once we measured a live deployment holding twenty-one documents for every
// row the store actually had. opensearch.staleSweep turns that half off.
//
// The two directions converge rather than fight: the stale pass removes only what the store says is
// gone, and the forward pass re-indexes anything it should not have.
//
// New gates this on consolidation.enabled, so the single consolidating instance is the sole owner of
// index maintenance and replicas never duplicate it.
func (s *Server) reconcileLoop() {
	defer close(s.reconcileStopped)

	log.Infof("search-index reconciliation enabled: sweeping every %s", s.reconcileInterval)

	// A timer (reset after each sweep) rather than a ticker, so a slow sweep does not queue up
	// back-to-back ticks; the interval is measured between the end of one sweep and the start of the
	// next.
	timer := time.NewTimer(reconcileInitialDelay)
	defer timer.Stop()

	for {
		select {

		case <-s.stopReconcile:
			return

		case <-timer.C:
			s.reconcileOnce()

			// After the forward pass, not before: a memory re-indexed a moment ago is one the stale
			// pass would otherwise have to reason about, and this ordering means it never sees a
			// document mid-heal.
			if s.staleSweepEnabled {
				s.staleSweep(context.Background())
			}

			timer.Reset(s.reconcileInterval)
		}
	}
}

// reconcileOnce runs a single sweep: it pages through every memory in the primary store and
// re-indexes the non-binary ones, pausing between pages so the asynchronous index queue is not
// flooded. It stops promptly when the server is shutting down. A failed page read abandons the
// sweep - the next one retries from the start - and indexing itself is fire-and-forget, so no error
// escapes.
func (s *Server) reconcileOnce() {
	log.Trace("func() reconcileOnce")

	ctx := context.Background()

	afterId := ""
	reindexed := 0
	started := time.Now()

	for {
		select {

		case <-s.stopReconcile:
			return

		default:
		}

		memories, err := s.db.GetMemoriesPage(ctx, afterId, s.reconcileBatchSize, nil)
		if err != nil {
			log.Warnf("search reconcile: failed to read memories after id '%s' (abandoning this sweep; the next one retries): %s", afterId, err.Error())

			return
		}

		if len(memories) == 0 {
			break
		}

		for _, memory := range memories {
			// Binary memories are never indexed - the body is opaque to content search.
			if memory.IsBinary {
				continue
			}

			// Through the embedding-aware helper, not idx.IndexMemory directly: a document
			// re-indexed without its vector replaces one that had it, so a sweep meant to heal the
			// index would strip semantic search from every memory it touched.
			//
			// The consequence is that with semantic search on, a sweep re-embeds every memory it
			// visits, which is far more expensive than the plain re-index it used to be. Raise
			// opensearch.reconcileIntervalSeconds accordingly, or rely on --backfill-search instead.
			s.indexMemory(ctx, memory)

			reindexed++
		}

		afterId = memories[len(memories)-1].Id

		// Pace the sweep, but wake immediately on shutdown.
		select {

		case <-s.stopReconcile:
			return

		case <-time.After(reconcilePageDelay):
		}
	}

	log.Debugf("search reconcile: re-indexed %d memories in %s", reindexed, time.Since(started).Round(time.Millisecond))
}
