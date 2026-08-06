package hippocampus

import (
	"context"
	"strconv"
	"time"

	log "github.com/sirupsen/logrus"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	grpccodes "google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/fastbean-au/hippocampus/contract"
	"github.com/fastbean-au/hippocampus/db"
)

// previewDecider applies the consolidation rules on behalf of a preview, against a snapshot of the
// two values a sleep cycle recomputes as it runs: the capacity pressure scaling the deletion
// threshold, and the default event significance standing in for a memory with no event.
//
// It exists because a preview runs on an RPC goroutine while the sleep goroutine may be rewriting
// both fields. Reading them per row would be a data race, and - even without one - would let a
// single scan evaluate its first rows against different numbers from its last, reporting an
// outcome no real cycle would ever produce. Every actual decision still goes through the Server's
// own methods, so the preview cannot drift from the cycle it predicts.
type previewDecider struct {
	server                   *Server
	capacityPressure         float64
	defaultEventSignificance int32
}

func (d previewDecider) ShouldConsolidateMemory(candidate db.MemoryConsolidationCandidate) bool {
	return d.server.shouldConsolidateUnder(
		d.server.memorySignificanceUnder(candidate, d.defaultEventSignificance),
		memoryDecayTimestamp(candidate),
		d.capacityPressure,
	)
}

func (d previewDecider) MemoryValue(candidate db.MemoryConsolidationCandidate) float64 {
	return d.server.memoryValueUnder(candidate, d.defaultEventSignificance)
}

func (d previewDecider) MemoryRetained(candidate db.MemoryConsolidationCandidate) bool {
	return d.server.MemoryRetained(candidate)
}

func (d previewDecider) ShouldConsolidateEvent(candidate db.EventConsolidationCandidate) bool {
	return d.server.shouldConsolidateEventUnder(candidate, d.capacityPressure)
}

// PreviewConsolidation reports what a consolidation cycle would forget if one ran now, and deletes
// nothing.
//
// It deliberately does NOT go through sleepOnce's singleflight group. Joining an in-flight cycle
// would return the results of a run that is at that moment deleting the very memories it claims
// only to be describing, which is the opposite of what a dry run is for. The cost of standing
// outside the group is that a cycle can start while the preview scans, so the preview describes
// the store as it was - which is true of any preview, and why the response reports the inputs it
// decided against.
func (s *Server) PreviewConsolidation(
	ctx context.Context,
	in *contract.PreviewConsolidationRequest,
) (*contract.PreviewConsolidationResponse, error) {
	log.Debug("PreviewConsolidation()")

	// A replica never runs a cycle of its own, so it has nothing to preview: its store is
	// consolidated by whichever instance holds the single-consolidator lock, under that instance's
	// configuration rather than this one's. Rejecting matches Sleep, and stops a preview reporting
	// a forgetting schedule this instance would never carry out.
	if !s.consolidationEnabled {
		return nil, status.Error(grpccodes.FailedPrecondition, "consolidation is disabled on this instance")
	}

	ctx, span := tel.tracer.Start(ctx, "preview_consolidation")
	defer span.End()

	result, err := s.previewOnce(ctx, db.PreviewLimit(int(in.GetLimit())))
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())

		return nil, mapError(err)
	}

	span.AddEvent("consolidation_previewed", trace.WithAttributes(
		attribute.Int("memories_consolidated", result.preview.MemoriesConsolidated),
		attribute.Int("memories_evicted", result.preview.MemoriesEvicted),
		attribute.Int("events_deleted", result.preview.EventsDeleted),
		attribute.Int64("bytes_freed", result.preview.BytesFreed),
	))

	return s.previewResponse(result), nil
}

// previewResult is the scan's outcome in the form concurrent callers share: plain values, not a
// proto message. That distinction is load-bearing - a proto message is not safe to marshal from
// several goroutines at once (marshalling writes the message's internal size cache), so each
// caller builds its own response from this instead of being handed a pointer to one.
type previewResult struct {
	preview   db.ConsolidationPreview
	pressure  float64
	usedBytes int64
}

// previewOnce runs a preview through previewGroup, so callers arriving while one is already
// scanning join it rather than each starting a scan of their own.
//
// The key is the sample size, because that is the only request field that changes the result:
// callers asking for the same number of rows can share a scan, while one asking for more is not
// handed a shorter list. The limit is normalised first (via db.PreviewLimit), so the default and
// its explicit equivalent share a key rather than scanning twice for the same answer.
//
// The usual singleflight caveat applies and is accepted: the leader's context governs the shared
// call, so a follower whose leader disconnects sees that cancellation and must retry. The
// alternative - detaching the work from every caller's context - would keep a scan running for
// callers who have all gone away, which is precisely the load this group exists to avoid.
func (s *Server) previewOnce(ctx context.Context, limit int) (previewResult, error) {
	shared, err, _ := s.previewGroup.Do(strconv.Itoa(limit), func() (any, error) {
		state, err := s.decisionSnapshot(ctx)
		if err != nil {
			return previewResult{}, err
		}

		preview, err := s.db.PreviewConsolidation(ctx, state.decider, db.PreviewOptions{
			Limit:         limit,
			UsedBytes:     state.usedBytes,
			CapacityBytes: s.consolidation.capacityBytes,
			EvictionFloor: s.evictionFloor(),
		})
		if err != nil {
			return previewResult{}, err
		}

		return previewResult{
			preview:   preview,
			pressure:  state.decider.capacityPressure,
			usedBytes: state.usedBytes,
		}, nil
	})
	if err != nil {
		return previewResult{}, err
	}

	result, ok := shared.(previewResult)
	if !ok {
		return previewResult{}, status.Error(grpccodes.Internal, "preview produced an unexpected result")
	}

	return result, nil
}

// previewResponse projects a shared result onto this caller's own response message. It only reads
// the shared value, so several callers may build from one result concurrently.
func (s *Server) previewResponse(result previewResult) *contract.PreviewConsolidationResponse {
	preview := result.preview

	res := contract.PreviewConsolidationResponse{
		MemoriesConsolidated: int32(preview.MemoriesConsolidated),
		MemoriesEvicted:      int32(preview.MemoriesEvicted),
		EventsDeleted:        int32(preview.EventsDeleted),
		BytesFreed:           preview.BytesFreed,
		MemoriesRetained:     int32(preview.MemoriesRetained),
		RetainedBytes:        preview.RetainedBytes,
		CapacityPressure:     result.pressure,
		DeletionThreshold:    s.consolidation.deletionThreshold * result.pressure,
		UsedBytes:            result.usedBytes,
		CapacityBytes:        s.consolidation.capacityBytes,
		Truncated:            preview.Truncated,
		Candidates:           make([]*contract.ForgetCandidate, 0, len(preview.Candidates)),
	}

	for _, candidate := range preview.Candidates {
		res.Candidates = append(res.Candidates, &contract.ForgetCandidate{
			Id:           candidate.Id,
			EventId:      candidate.EventId,
			Group:        candidate.Group,
			Significance: candidate.Significance,
			Value:        candidate.Value,
			Threshold:    res.DeletionThreshold,
			BodyBytes:    candidate.Bytes,
			Rule:         forgetRules[candidate.Rule],
			TimeStamp:    candidate.TimeStamp,
			TimeRecalled: candidate.TimeRecalled,
			RecallCount:  candidate.RecallCount,
		})
	}

	return &res
}

// forgetRules maps the storage layer's rule onto the wire enum. An unknown rule maps to
// UNSPECIFIED rather than to a plausible-looking one, so a rule added to db/ without a wire
// counterpart is visibly missing instead of silently mislabelled.
var forgetRules = map[db.ForgetRule]contract.ForgetRule{
	db.ForgetRuleConsolidation: contract.ForgetRule_FORGET_RULE_CONSOLIDATION,
	db.ForgetRuleEviction:      contract.ForgetRule_FORGET_RULE_EVICTION,
}

// decisionState is what a consolidation cycle starting now would decide against: the decider
// carrying the snapshot, alongside the two store readings that produced its capacity pressure.
// PreviewConsolidation takes a fresh one per scan; ExplainConsolidation caches one briefly, being
// asked far more often (see cachedDecisionSnapshot).
type decisionState struct {
	decider     previewDecider
	usedBytes   int64
	memoryCount int

	// at is when the snapshot was computed, and is set only by cachedDecisionSnapshot - a zero
	// value marks a snapshot that has never been cached rather than one cached at the epoch.
	at time.Time
}

// decisionSnapshot computes the inputs a cycle starting now would decide against, and returns them
// alongside the store readings behind them.
//
// Everything is computed rather than read from the server's live fields. That is what keeps a
// preview off the sleep goroutine's mutable state, and it also makes it more faithful than reusing
// those fields would be: the cycle's pressure deliberately reuses the previous cycle's byte reading
// to avoid a second scan, whereas a preview - being occasional and asked for precisely when someone
// wants the truth - can afford a fresh one.
func (s *Server) decisionSnapshot(ctx context.Context) (decisionState, error) {
	state := decisionState{
		decider: previewDecider{
			server:                   s,
			capacityPressure:         1.0,
			defaultEventSignificance: s.consolidation.defaultEventSignificanceValue,
		},
	}

	usedBytes, err := s.db.UsedBytes(ctx)
	if err != nil {
		log.Errorf("failed to read used bytes for preview: %s", err.Error())

		return state, err
	}

	state.usedBytes = usedBytes

	// Mirrors consolidate(): the byte axis contributes to pressure only when a byte capacity is
	// configured, otherwise pressure rides on the row count alone.
	var pressureBytes int64
	if s.consolidation.capacityBytes > 0 {
		pressureBytes = usedBytes
	}

	with, without := s.db.CountMemories(ctx)
	if with < 0 || without < 0 {
		return state, status.Error(grpccodes.Internal, "failed to count memories for the preview")
	}

	state.memoryCount = with + without
	state.decider.capacityPressure = s.calculateCapacityPressure(state.memoryCount, pressureBytes)

	// When the default event significance is derived from a percentile it is recomputed by every
	// cycle, so the preview recomputes it too rather than reading the value the last cycle left
	// behind - which also keeps it clear of the field the sleep goroutine writes.
	if s.consolidation.defaultEventSignificancePercentile != 0 {
		percentile, err := s.db.CalculateSignificancePercentile(ctx, s.consolidation.defaultEventSignificancePercentile)
		if err != nil {

			// Exactly as the cycle does: an empty event store cannot yield a percentile, so retain
			// the configured value rather than failing.
			log.Warnf("default event significance percentile unavailable for preview, retaining %d: %s",
				s.consolidation.defaultEventSignificanceValue,
				err.Error(),
			)
		} else {
			state.decider.defaultEventSignificance = int32(percentile)
		}
	}

	return state, nil
}
