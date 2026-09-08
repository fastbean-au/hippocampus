package hippocampus

import (
	"context"

	log "github.com/sirupsen/logrus"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/fastbean-au/hippocampus/db"
)

// The pre-reap callback: the one delivery kind that speaks before the fact.
//
// Everything else the callback surface reports has already happened. memory_forgotten says a memory
// has gone, which is the one thing a client that cared about it can no longer act on - it cannot
// summarise it, archive it elsewhere, or recall it, because there is nothing left to reach. This is
// the counterpart to GetSummarisationCandidates, which does exactly this for events: a push feed of
// what the store is about to lose, raised early enough to be answered.
//
// Four things carry the design.
//
// It reuses the PREVIEW SCAN rather than a scan of its own. PreviewConsolidation already computes
// the set a cycle would take, from a snapshot, through the Server's own decision methods - so the
// warning and the deletion cannot disagree about an individual memory, which is the only property
// that makes acting on it worthwhile. What it does not reuse is previewGroup: the at-risk scan asks
// a different question (a raised threshold, see below), and it runs on the sleep goroutine, whose
// lifetime is the cycle's rather than a caller's.
//
// The MARGIN is what makes it actionable rather than merely earlier. Emitted at the top of a cycle
// with no margin, a warning about what that cycle is about to delete arrives while the cycle is
// deleting it, and the queue drains asynchronously afterwards - which is barely an improvement on
// memory_forgotten. callbacks.atRiskMargin raises the bar the scan selects on, so the delivery also
// carries memories that are still above the threshold but approaching it, which is a warning with a
// cycle or more of notice on it. The delivery reports both thresholds, and each item its value, so a
// receiver can tell the two populations apart.
//
// It costs a SCAN PER CYCLE, which is why callbacks.events.memoriesAtRisk defaults off while the
// three deletion toggles default on. A preview is a UsedBytes reading, a CountMemories and a full
// pass over the memories table with a sort - roughly what the cycle itself pays - so this is not a
// switch to leave on because it might one day be useful.
//
// And it is BEST-EFFORT AND NOT A VETO. A receiver that acts on the warning by recalling a memory is
// racing the pass that is about to delete it, and the race is already safe in its favour: the
// recall-race guard in deleteMemoriesIfUnrecalled re-checks the recall clock inside the delete, so a
// recall that lands mid-cycle protects its memory. That is a property worth documenting and not a
// guarantee worth promising - a receiver that was down misses the window entirely, and nothing here
// waits for one.

// queueAtRiskCallback runs the pre-reap scan and queues what it found, at the top of a cycle.
//
// Silent and free when the kind is off, which is the default: nothing here runs, and in particular
// no scan is paid for.
//
// Every failure is logged and swallowed. This runs before the cycle's real work and must never stop
// it: a store that cannot warn about what it is forgetting must still forget, or a receiver's outage
// would become a store that never consolidates.
func (s *Server) queueAtRiskCallback(ctx context.Context, cycleId int64) {
	if !s.callbacksEnabled || !s.callbackAtRiskEvents {

		return
	}

	log.Debug("queueAtRiskCallback()")

	ctx, span := tel.tracer.Start(ctx, "memories_at_risk")
	defer span.End()

	state, err := s.decisionSnapshot(ctx)
	if err != nil {
		log.Warnf("callbacks: failed to snapshot the store for the at-risk scan: %s", err.Error())

		return
	}

	// The bar in force, and the raised one the scan actually selects on. Both are computed from the
	// same pressure reading, so the margin is the only thing between them.
	threshold := s.deletionThresholdUnder(state.decider.capacityPressure)

	pressure := state.decider.capacityPressure
	state.decider.capacityPressure = pressure * (1 + s.callbackAtRiskMargin)

	atRiskThreshold := s.deletionThresholdUnder(state.decider.capacityPressure)

	preview, err := s.db.PreviewConsolidation(ctx, state.decider, db.PreviewOptions{
		Limit:         s.callbackAtRiskLimit,
		UsedBytes:     state.usedBytes,
		CapacityBytes: s.consolidation.capacityBytes,
		EvictionFloor: s.evictionFloor(),
	})
	if err != nil {
		log.Warnf("callbacks: the at-risk scan failed: %s", err.Error())

		return
	}

	// Only when the set is non-empty. A cycle that would take nothing is what a store at rest looks
	// like, and reporting it every period would be a heartbeat - which is what sleep_completed
	// already is, and which would put a delivery in the queue on every cycle of an idle deployment.
	//
	// The test is the sample rather than the counts, so it is exactly the condition under which
	// there is something to deliver: the sample holds at least one row whenever either memory count
	// is positive, and a positive event count on its own means only that some empty events have
	// decayed - which is not a memory at risk, and not what this kind is about.
	if len(preview.Candidates) == 0 {

		return
	}

	summary := &db.CallbackAtRisk{
		Consolidating:    preview.MemoriesConsolidated,
		Evicting:         preview.MemoriesEvicted,
		Events:           preview.EventsDeleted,
		Bytes:            preview.BytesFreed,
		Threshold:        threshold,
		AtRiskThreshold:  atRiskThreshold,
		CapacityPressure: pressure,
		Truncated:        preview.Truncated,
	}

	deliveries := atRiskDeliveries(cycleId, summary, preview.Candidates, s.callbackChunkIds)

	span.AddEvent("memories_at_risk", trace.WithAttributes(
		attribute.Int("consolidating", preview.MemoriesConsolidated),
		attribute.Int("evicting", preview.MemoriesEvicted),
		attribute.Int("reported", len(preview.Candidates)),
		attribute.Int("deliveries", len(deliveries)),
	))

	if err := s.db.QueueCallbacks(ctx, deliveries); err != nil {
		log.Warnf("callbacks: failed to queue the at-risk callback: %s", err.Error())
	}
}

// atRiskDeliveries splits a preview's candidates into deliveries, grouped by which path would take
// them and chunked within each group.
//
// Grouped by CAUSE rather than chunked as one stream, which is where this differs from
// cycleDeliveries. A cause is a per-delivery field, and the two causes mean genuinely different
// things to a receiver: a memory going to consolidation has decayed past the bar and a recall will
// save it, while one going to eviction is still above the bar and is being taken to make room, which
// a recall may not save and which an operator fixes by raising the capacity. Mixing them into one
// stream would leave the field empty on every delivery and the distinction to be re-derived from the
// values.
//
// A receiver therefore reassembles on (cycle_id, cause), each group numbered from 1. Every chunk
// repeats the summary, for the reason cycleDeliveries gives: it costs a few dozen bytes and means a
// receiver that drops one chunk still knows what the cycle is about to do.
func atRiskDeliveries(
	cycleId int64,
	summary *db.CallbackAtRisk,
	candidates []db.ForgetCandidate,
	perChunk int,
) []db.CallbackDelivery {
	if perChunk <= 0 {
		perChunk = defaultCallbackMaxIdsPerChunk
	}

	// Both groups keep the sample's value-ascending order, so chunk 1 of either is the closest to
	// going. The causes are listed rather than derived from the candidates so their order is stable
	// whatever a scan happened to find.
	byCause := []struct {
		rule  db.ForgetRule
		cause db.DeleteCause
	}{
		{db.ForgetRuleConsolidation, db.CauseConsolidation},
		{db.ForgetRuleEviction, db.CauseEviction},
	}

	deliveries := make([]db.CallbackDelivery, 0, 2)

	for _, group := range byCause {
		items := make([]db.CallbackItem, 0, len(candidates))

		for _, candidate := range candidates {
			if candidate.Rule != group.rule {
				continue
			}

			items = append(items, db.CallbackItem{
				Id:           candidate.Id,
				EventId:      candidate.EventId,
				Group:        candidate.Group,
				Significance: candidate.Significance,
				Bytes:        candidate.Bytes,
				Value:        candidate.Value,
			})
		}

		if len(items) == 0 {
			continue
		}

		chunks := (len(items) + perChunk - 1) / perChunk

		for i := range chunks {
			start := i * perChunk
			end := min(start+perChunk, len(items))
			chunk := items[start:end]

			deliveries = append(deliveries, db.CallbackDelivery{
				Kind:      db.CallbackKindMemoriesAtRisk,
				Cause:     group.cause,
				CycleId:   cycleId,
				Chunk:     i + 1,
				Chunks:    chunks,
				ItemCount: len(chunk),
				Payload:   db.CallbackPayload{Items: chunk, AtRisk: summary},
			})
		}
	}

	return deliveries
}
