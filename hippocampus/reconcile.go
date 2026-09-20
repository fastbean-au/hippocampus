package hippocampus

import (
	"context"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/fastbean-au/hippocampus/search"
	"github.com/fastbean-au/hippocampus/types"
)

// presenceProbe is the capability the forward half of the sweep needs from the search backend: ask
// which of a page of memory ids the index does not hold.
//
// An optional interface rather than a method on search.Index, for the reason DeleteMemoriesSync and
// EnumerateIdsPage already set - and because only one backend has the problem it solves. The SQL
// backend maintains its index inside the primary write itself, so nothing can be dropped and there
// is nothing to heal; the no-op backend holds nothing. Neither has a sweep to run, which its own doc
// comment has said all along while startReconcile ran one anyway.
type presenceProbe interface {
	AbsentIds(ctx context.Context, ids []string) ([]string, error)
}

// Compile-time assertion that the OpenSearch backend provides it - the sweep's gating rests on it
// being the one backend that does.
var _ presenceProbe = (*search.OpenSearch)(nil)

// defaultReconcileBatchSize is the page size the reconciliation sweep reads the primary store in
// when opensearch.reconcileBatchSize is unset or non-positive.
const defaultReconcileBatchSize = 500

// reconcileInitialDelay is how long after startup the first sweep runs. Firing after a short delay
// rather than immediately lets the service settle first, while still healing a sparse index soon
// after a restart rather than waiting a whole interval. A var so tests can shorten it.
var reconcileInitialDelay = 60 * time.Second

// reconcilePageDelay paces a sweep: after each page it waits this long, so a full-store sweep
// trickles rather than running the primary store and the cluster flat out. A var so tests can
// shorten it.
//
// It used to be the only thing standing between the sweep and the apply queue, and it was never
// enough: a page of 500 re-index operations every 200ms is ~2,500 a second offered to a worker that
// applies one document per round trip, so the sweep's own pages were what filled the queue and got
// dropped. Now a page normally enqueues nothing at all, and this paces the reads and the presence
// probe instead.
var reconcilePageDelay = 200 * time.Millisecond

// reconcileLoop periodically indexes the live memories the search index does not hold, healing
// documents that never landed - operations dropped under queue overflow, lost to a crash before the
// worker drained, or missed while the cluster was unreachable. The index is strictly secondary, so a
// sweep runs beside live traffic without coordinating with it: the worst case is briefly re-writing a
// document a concurrent write also wrote, which converges.
//
// It runs in both directions. The forward pass heals *missing* documents, which is all it ever did:
// the memory is still there to index. The stale pass (staleSweep, in outbox.go) enumerates the
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

// reconcileOnce runs a single sweep: it pages through every memory in the primary store, asks the
// index which of each page it does not hold, and indexes only those - pausing between pages. It stops
// promptly when the server is shutting down. A failed page read abandons the sweep (the next one
// retries from the start), and indexing itself is fire-and-forget, so no error escapes.
//
// Asking first is the whole change, and it is what makes the sweep affordable rather than merely
// faster. Re-indexing unconditionally cost three things, all of which scaled with the store: it was
// the dominant producer on the queue it exists to compensate for (so live writes were the operations
// dropped to make room for re-writes of documents already present); with semantic search on it
// re-embedded every memory in the store on every pass; and in Lucene a re-index is a delete plus an
// insert even when the document is byte-identical, so an hourly sweep tombstoned the entire index
// once an hour - measured on a live deployment as 921,741 deleted documents against 147,496 live
// ones, 341 MB of index that a force-merge took to 215 MB.
//
// One _mget per page answers it, so a healthy index costs one request per page and writes nothing.
func (s *Server) reconcileOnce() {
	log.Trace("func() reconcileOnce")

	probe, ok := s.searchIdx().(presenceProbe)
	if !ok {

		return
	}

	ctx := context.Background()

	afterId := ""
	checked := 0
	healed := 0
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

		afterId = memories[len(memories)-1].Id

		indexable := indexableMemories(memories)
		checked += len(indexable)

		healed += s.healPage(ctx, probe, indexable)

		// Pace the sweep, but wake immediately on shutdown.
		select {

		case <-s.stopReconcile:
			return

		case <-time.After(reconcilePageDelay):
		}
	}

	took := time.Since(started).Round(time.Millisecond)

	// A sweep that heals nothing is the expected outcome and stays at Debug; one that heals is news,
	// because it is the measure of how much the apply queue is losing - the number an operator sizes
	// opensearch.queueSize on, and the one thing the old sweep could not report at all, having
	// re-written every document whether it was there or not.
	if healed == 0 {
		log.Debugf("search reconcile: %d memories checked, none missing from the index, in %s", checked, took)

		return
	}

	log.Infof("search reconcile: indexed %d of %d memories that the index did not hold, in %s", healed, checked, took)
}

// indexableMemories drops the memories that are never indexed. Binary bodies are opaque to content
// search, so a binary memory has no document to be missing and must not be counted as one - it would
// be re-indexed every sweep, for ever.
func indexableMemories(in []types.Memory) []types.Memory {
	out := make([]types.Memory, 0, len(in))

	for _, memory := range in {
		if memory.IsBinary {
			continue
		}

		out = append(out, memory)
	}

	return out
}

// healPage indexes the memories of one page that the index does not hold, and reports how many.
//
// A failed probe indexes nothing rather than falling back to indexing the page. That is the opposite
// of the old behaviour and deliberate: the fallback is what this change exists to remove, and a probe
// fails for the same reasons an index write does - an unreachable or overloaded cluster - so falling
// back would offer a full page of writes to a queue at exactly the moment nothing can be applied. The
// next sweep asks again.
func (s *Server) healPage(ctx context.Context, probe presenceProbe, memories []types.Memory) int {
	if len(memories) == 0 {

		return 0
	}

	ids := make([]string, 0, len(memories))

	for _, memory := range memories {
		ids = append(ids, memory.Id)
	}

	absent, err := probe.AbsentIds(ctx, ids)
	if err != nil {
		log.Warnf("search reconcile: failed to ask the index which of %d memories it holds (skipping this page; the next sweep asks again): %s", len(ids), err.Error())

		return 0
	}

	if len(absent) == 0 {

		return 0
	}

	missing := make(map[string]bool, len(absent))

	for _, id := range absent {
		missing[id] = true
	}

	indexed := 0

	for _, memory := range memories {
		if !missing[memory.Id] {
			continue
		}

		// Through the embedding-aware helper, not idx.IndexMemory directly: a document indexed
		// without its vector replaces one that had it, so a sweep meant to heal the index would strip
		// semantic search from every memory it touched. With the probe in front of it, that cost is
		// now paid only for the documents genuinely absent rather than for the whole store.
		s.indexMemory(ctx, memory)

		indexed++
	}

	tel.documentsHealed.Add(ctx, int64(indexed))

	return indexed
}
