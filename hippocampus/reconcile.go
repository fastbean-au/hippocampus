package hippocampus

import (
	"context"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/fastbean-au/hippocampus/search"
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
// It heals only *missing* documents. A stale document (one the primary store no longer has) is
// already harmless - SearchMemories re-verifies every hit against the primary store - and clearing
// stale documents needs a full enumeration of the index, which stays the job of
// --backfill-search --reindex. New gates this on consolidation.enabled, so the single consolidating
// instance is the sole owner of the sweep and replicas never duplicate it.
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

	idx := s.searchIdx()
	afterId := ""
	reindexed := 0
	started := time.Now()

	for {
		select {

		case <-s.stopReconcile:
			return

		default:

		}

		memories, err := s.db.GetMemoriesPage(ctx, afterId, s.reconcileBatchSize)
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

			idx.IndexMemory(search.DocFromMemory(memory))
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
