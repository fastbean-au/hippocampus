package hippocampus

import (
	"context"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/viper"

	"github.com/fastbean-au/hippocampus/search"
)

// outboxDrainBatch is how many queued deletions one pass applies. Small enough that a failed pass
// re-does little, large enough that a backlog drains at a useful rate.
const outboxDrainBatch = 500

// outboxIdleDelay is how long the drain waits when it finds nothing to do, so an idle deployment is
// not polling a table every second. A var so tests can shorten it.
var outboxIdleDelay = 30 * time.Second

// outboxBusyDelay is the pause between passes while a backlog is still draining: short, because
// there is work, but not zero, so a large backlog does not monopolise the index. A var so tests can
// shorten it.
var outboxBusyDelay = time.Second

// defaultOutboxMaxAgeHours and defaultOutboxMaxRows bound the queue when opensearch.outbox.* is
// unset. Hours rather than the days the forgotten log's caps are measured in, deliberately: this
// queue is meant to drain in seconds, so a day of it is already an outage, and a week of it is a
// deployment nobody is watching.
const (
	defaultOutboxMaxAgeHours = 24
	defaultOutboxMaxRows     = 1_000_000
)

// deleteSyncer is the narrow capability the drain and the stale sweep need from the search backend:
// a delete that reports whether it landed.
//
// Declared here as an optional interface rather than added to search.Index for the reason
// RecreateIndex and IndexMemorySync already set - and because only one backend has the problem this
// solves. The SQLite FTS backend's deletes are an AFTER DELETE trigger inside the same transaction
// as the memory's own, and the no-op backend holds nothing to delete; neither has anything to drain,
// and a type assertion is what keeps that fact in one place instead of two no-op implementations.
type deleteSyncer interface {
	DeleteMemoriesSync(ctx context.Context, ids []string) error
}

// idEnumerator is the other optional capability, needed only by the stale half of the sweep: page
// through the ids the index currently holds. The cursor is the backend's own - see
// search.IndexCursor for why it is a timestamp and an offset rather than the document id it looks
// like it should be.
type idEnumerator interface {
	EnumerateIdsPage(ctx context.Context, cursor search.IndexCursor, size int) (search.IndexPage, error)
}

// Compile-time assertions that the OpenSearch backend provides both - the whole design rests on it
// being the one backend that does.
var (
	_ deleteSyncer = (*search.OpenSearch)(nil)
	_ idEnumerator = (*search.OpenSearch)(nil)
)

// startOutboxDrain launches the worker that applies queued index deletions, when there is a backend
// whose deletes can be lost and this is the instance that owns index maintenance.
//
// Gated on consolidation.enabled like the sweep: the outbox is a single queue, and replicas draining
// it concurrently would each claim the same rows. The claim/confirm split makes that merely wasteful
// rather than wrong, but there is no reason to pay for it.
//
// It also enables the recording. That is the same decision expressed at both ends - a store queues
// deletions exactly when something is going to drain them - and keeping it in one function is what
// stops a deployment writing a row per forgotten memory into a table nothing reads.
func (s *Server) startOutboxDrain(searchIndex search.Index) {
	s.outboxMaxRows = int64(viper.GetInt("opensearch.outbox.maxRows"))
	s.outboxMaxAge = time.Duration(viper.GetInt("opensearch.outbox.maxAgeHours")) * time.Hour

	if s.outboxMaxRows <= 0 {
		s.outboxMaxRows = defaultOutboxMaxRows
	}

	if s.outboxMaxAge <= 0 {
		s.outboxMaxAge = defaultOutboxMaxAgeHours * time.Hour
	}

	if searchIndex == nil || !searchIndex.Enabled() {

		return
	}

	if _, ok := searchIndex.(deleteSyncer); !ok {
		log.Debug("search outbox: this backend deletes transactionally, so nothing is queued")

		return
	}

	// The recording half. Only the concrete store can be told - the seam is the same one
	// SetMemoryDeleteObserver uses, and for the same reason: this is storage configuration, not part
	// of what the Store interface promises.
	store, ok := s.db.(interface{ SetSearchOutbox(enabled bool) })
	if !ok {

		return
	}

	if !s.consolidationEnabled {
		// A replica must still RECORD: it serves writes, so its deletes are as losable as anyone's,
		// and the consolidating instance's drain will apply them. It just must not drain.
		store.SetSearchOutbox(true)

		return
	}

	store.SetSearchOutbox(true)

	s.stopOutbox = make(chan struct{})
	s.outboxStopped = make(chan struct{})

	go s.outboxDrainLoop()
}

// outboxDrainLoop applies queued index deletions until stopped.
//
// It exists because propagation to OpenSearch is asynchronous and bounded, and drops on overflow. A
// dropped INDEX is self-correcting - the memory still exists, so the reconciliation sweep re-indexes
// it - while a dropped DELETE is not, because nothing afterwards knows the document should have
// gone. The outbox records the deletion in the same transaction as the memory's own, and this drains
// it.
func (s *Server) outboxDrainLoop() {
	defer close(s.outboxStopped)

	syncer, ok := s.searchIdx().(deleteSyncer)
	if !ok {

		return
	}

	log.Infof("search outbox drain enabled: caps %d rows / %s", s.outboxMaxRows, s.outboxMaxAge)

	for {
		applied := s.drainOutboxOnce(syncer)

		delay := outboxIdleDelay

		if applied > 0 {
			delay = outboxBusyDelay
		}

		select {

		case <-s.stopOutbox:

			return

		case <-time.After(delay):
		}
	}
}

// drainOutboxOnce applies one batch of queued deletions, returning how many landed.
//
// Claim, apply, THEN confirm. A row is removed only once the index has accepted the deletion, so a
// crash between the two replays the work rather than losing it - at-least-once, which is the right
// guarantee here because deleting a document that is already gone is a no-op.
//
// The deletion goes in synchronously, bypassing the asynchronous queue entirely. That is the whole
// point: handing the work back to the mechanism that loses it would reproduce the fault the outbox
// exists to fix, one indirection further along.
func (s *Server) drainOutboxOnce(syncer deleteSyncer) int {
	ctx := context.Background()

	entries, err := s.db.ClaimSearchDeletes(ctx, outboxDrainBatch)
	if err != nil {
		log.Warnf("search outbox: failed to claim queued deletions: %s", err.Error())

		return 0
	}

	if len(entries) == 0 {
		s.pruneOutbox(ctx)
		s.recordOutboxDepth(ctx)

		return 0
	}

	ids := make([]string, 0, len(entries))
	seqs := make([]int64, 0, len(entries))

	for _, v := range entries {
		ids = append(ids, v.ID)
		seqs = append(seqs, v.Seq)
	}

	if err := syncer.DeleteMemoriesSync(ctx, ids); err != nil {
		// Left queued deliberately: the next pass retries, and if the index stays unreachable long
		// enough the caps abandon them to the reconciliation sweep, which removes stale documents
		// whatever put them there.
		log.Warnf("search outbox: failed to apply %d queued deletions (they stay queued): %s",
			len(ids), err.Error())

		s.recordOutboxDepth(ctx)

		return 0
	}

	if err := s.db.ConfirmSearchDeletes(ctx, seqs); err != nil {
		// The deletions landed; only the bookkeeping failed. Re-applying them next pass is harmless,
		// which is why this is not worth escalating - but they are not counted as applied, because
		// the pass did not finish.
		log.Warnf("search outbox: applied %d deletions but failed to confirm them (they will be re-applied): %s",
			len(ids), err.Error())

		return 0
	}

	log.Debugf("search outbox: applied %d queued index deletions", len(ids))

	tel.searchOutboxApplied.Add(ctx, int64(len(ids)))

	return len(ids)
}

// pruneOutbox trims the queue to its caps, and says so loudly: reaching a cap means deletions are
// being discarded before the index took them, which is the one condition here an operator must act
// on. Logged as well as counted, for the deployments without a metrics stack.
//
// Run only on an empty claim, so pruning never races the drain for rows it is about to apply.
func (s *Server) pruneOutbox(ctx context.Context) {
	pruned, err := s.db.PruneSearchOutbox(ctx, s.outboxMaxAge, s.outboxMaxRows)
	if err != nil {
		log.Warnf("search outbox: failed to prune: %s", err.Error())

		return
	}

	if pruned == 0 {

		return
	}

	log.Warnf("search outbox: abandoned %d queued index deletions at the caps (%d rows / %s) - "+
		"the search index has stale documents until the reconciliation sweep finds them; is it reachable?",
		pruned, s.outboxMaxRows, s.outboxMaxAge)

	tel.searchOutboxAbandoned.Add(ctx, pruned)
}

// recordOutboxDepth publishes the queue's depth. Read on the idle path only - the drain already
// knows it is behind when it finds work, and a COUNT per busy pass would put a query on exactly the
// path that is already struggling.
func (s *Server) recordOutboxDepth(ctx context.Context) {
	depth, err := s.db.SearchOutboxDepth(ctx)
	if err != nil {

		return
	}

	tel.searchOutboxDepth.Record(ctx, depth)
}

// staleSweep is the reconciliation sweep's reverse direction: it enumerates the search index and
// removes documents whose memory the primary store no longer holds.
//
// The forward sweep heals MISSING documents, and always could: the memory is still there to
// re-index. Nothing healed the opposite, so a stale document survived until a manual
// --backfill-search --reindex. That was defensible while only a dropped delete could produce one and
// a stale hit was merely wasted work - SearchMemories re-verifies every hit against the primary
// store, so a stale document is invisible to a caller. It is not defensible now that we know the
// drop rate rises with the write rate: measured on a live deployment, twenty-one documents for every
// row the store actually held. That is disk, heap and query latency spent on records that no longer
// exist, growing without bound, on a deployment nobody had done anything wrong to.
//
// This is the backstop rather than the mechanism. The outbox is what makes deletes reliable; this
// bounds what the outbox cannot see - deletions abandoned at the caps, divergence that predates the
// outbox, and whatever future bug leaves a document behind.
//
// It removes only what the primary store says is gone, and the forward pass re-indexes anything
// wrongly removed, so the two directions converge rather than fight.
func (s *Server) staleSweep(ctx context.Context) {
	log.Trace("func() staleSweep")

	index := s.searchIdx()

	enumerator, ok := index.(idEnumerator)
	if !ok {

		return
	}

	syncer, ok := index.(deleteSyncer)
	if !ok {

		return
	}

	var cursor search.IndexCursor

	removed := 0
	scanned := 0
	started := time.Now()

	for {
		select {

		case <-s.stopReconcile:

			return

		default:
		}

		page, err := enumerator.EnumerateIdsPage(ctx, cursor, s.reconcileBatchSize)
		if err != nil {
			log.Warnf("search reconcile: failed to enumerate the index at %d+%d (abandoning the stale pass; the next sweep retries): %s",
				cursor.Timestamp, cursor.Offset, err.Error())

			return
		}

		if page.Done {

			break
		}

		scanned += len(page.Ids)

		// The primary store is the authority, and this is the cheap half: a primary-key lookup over
		// one page, not a scan.
		missing, err := s.db.MissingMemoryIds(ctx, page.Ids)
		if err != nil {
			log.Warnf("search reconcile: failed to check which indexed memories still exist (abandoning the stale pass): %s",
				err.Error())

			return
		}

		if len(missing) > 0 {
			if err := syncer.DeleteMemoriesSync(ctx, missing); err != nil {
				log.Warnf("search reconcile: failed to remove %d stale documents (abandoning the stale pass): %s",
					len(missing), err.Error())

				return
			}

			removed += len(missing)

			tel.staleDocumentsRemoved.Add(ctx, int64(len(missing)))
		}

		cursor = page.Next

		// A page that could not end on a timestamp boundary leaves the cursor addressing documents by
		// offset, and this pass just deleted some of the documents that offset counts past. Nothing
		// but the caller knows how many, so nothing but the caller can correct it - and without the
		// correction the next page would begin beyond whatever moved into the gap, skipping it
		// silently. See search.IndexPage.
		if page.Partial {
			cursor.Offset -= len(missing)
		}

		// Paced like the forward pass, and interruptible for the same reason.
		select {

		case <-s.stopReconcile:

			return

		case <-time.After(reconcilePageDelay):
		}
	}

	if removed > 0 {
		log.Infof("search reconcile: removed %d stale documents of %d indexed, in %s",
			removed, scanned, time.Since(started).Round(time.Millisecond))

		return
	}

	log.Debugf("search reconcile: scanned %d indexed documents, none stale, in %s",
		scanned, time.Since(started).Round(time.Millisecond))
}
