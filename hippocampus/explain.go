package hippocampus

import (
	"context"
	"math"
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

const (
	// explainMaxMemoryIds bounds one ExplainConsolidation call. It is sized for a console page or a
	// search result set rather than for bulk work: the RPC answers about memories the caller already
	// has in hand, and a caller wanting the whole store's standing wants PreviewConsolidation.
	explainMaxMemoryIds = 200

	// curveDefaultPoints / curveMaxPoints bound the sampled decay curve. Sixty points draw a smooth
	// line at any plot width; the cap stops a client asking for a response far larger than the curve
	// it can show.
	curveDefaultPoints = 60
	curveMaxPoints     = 500

	// curveHorizonUnits bounds every search for a threshold crossing, in age units. Some
	// configurations never cross in any timeframe worth reporting - method 5's logarithmic decay
	// reaches a low threshold only at an age with no physical meaning - so a horizon is what lets
	// "not within any useful span" be reported as such (-1) instead of as a number that looks like
	// an answer.
	curveHorizonUnits = 1e6

	// curveDefaultSpanUnits is the span a curve covers when no crossing was found to size it
	// against: enough age units to show the algorithm's shape without running off to the horizon.
	curveDefaultSpanUnits = 100

	// curveCrossingIterations is the bisection depth used to locate a crossing. Every decay method
	// is monotonically non-increasing in age, so bisection is exact to within the horizon divided by
	// 2^n - at this depth, far finer than any timeframe a client displays.
	curveCrossingIterations = 60

	// explainStateKey is the sole key used with Server.explainGroup: concurrent callers refreshing
	// the decision snapshot share one refresh rather than each scanning for it.
	explainStateKey = "explain_state"
)

// explainStateTTL is how long ExplainConsolidation reuses a decision snapshot before recomputing
// it. Both readings behind it - used bytes and the memory count - cost a scan on every driver, and
// this RPC is called once per console page rather than once per operator decision, so recomputing
// per call would reintroduce exactly the per-call full scan item 25.9 removed from the stats path.
// Capacity pressure moves over a sleep cycle, not over seconds, so a reading this fresh describes
// the store just as truthfully. A var so tests can shorten it.
var explainStateTTL = 10 * time.Second

// ExplainConsolidation reports where the named memories stand against the consolidation rules, and
// optionally the decay curve of the current configuration. It deletes nothing and reads nothing but
// the memories asked about.
//
// The values it reports come from the same methods the sleep cycle decides with, evaluated against
// the same kind of snapshot PreviewConsolidation takes (see previewDecider), so what a client plots
// or colours cannot drift from what the service will actually do. That is the whole point of
// serving it rather than letting each client reimplement docs/consolidation.md.
func (s *Server) ExplainConsolidation(
	ctx context.Context,
	in *contract.ExplainConsolidationRequest,
) (*contract.ExplainConsolidationResponse, error) {
	log.Debug("ExplainConsolidation()")

	// As for PreviewConsolidation: a replica's store is consolidated by whichever instance holds the
	// single-consolidator lock, under that instance's configuration. Reporting this one's decay
	// policy would describe a forgetting schedule nothing carries out.
	if !s.consolidationEnabled {
		return nil, status.Error(grpccodes.FailedPrecondition, "consolidation is disabled on this instance")
	}

	ids := uniqueIds(in.GetMemoryIds())

	if len(ids) > explainMaxMemoryIds {
		return nil, status.Errorf(grpccodes.InvalidArgument,
			"at most %d memory ids may be explained in one call, %d were given",
			explainMaxMemoryIds,
			len(ids),
		)
	}

	// Explain answers only about ids the caller supplies, so it is scoped by checking them rather
	// than refused outright the way the preview is. The pressure and threshold it also reports are
	// store-global figures, which is correct and not a leak: they are what actually decides this
	// caller's own memories' fate, and withholding them would leave a value with nothing to measure
	// it against.
	if err := s.scopeMemoryIds(ctx, ids); err != nil {
		return nil, err
	}

	curve, err := curveRequest(in.GetCurve())
	if err != nil {
		return nil, err
	}

	ctx, span := tel.tracer.Start(ctx, "explain_consolidation")
	defer span.End()

	state, err := s.cachedDecisionSnapshot(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())

		return nil, mapError(err)
	}

	candidates, err := s.db.GetMemoryConsolidationCandidates(ctx, ids)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())

		return nil, mapError(err)
	}

	res := s.explainResponse(state, candidates, ids, curve)

	span.AddEvent("consolidation_explained", trace.WithAttributes(
		attribute.Int("memories_valued", len(res.Valuations)),
		attribute.Bool("curve_requested", curve != nil),
	))

	return res, nil
}

// uniqueIds collapses repeated ids while keeping the caller's order, so a duplicate costs neither a
// second row in the response nor a second placeholder in the query.
func uniqueIds(ids []string) []string {
	seen := make(map[string]bool, len(ids))
	unique := make([]string, 0, len(ids))

	for _, v := range ids {
		if v == "" || seen[v] {
			continue
		}

		seen[v] = true

		unique = append(unique, v)
	}

	return unique
}

// curveSpec is a validated DecayCurveRequest. A nil spec means no curve was asked for.
type curveSpec struct {
	significance float64
	maxAgeDays   float64
	points       int
}

// curveRequest validates the requested curve. A significance that is not a positive, finite number
// has no curve to draw, so it is rejected rather than answered with a line of infinities; the span
// and sample count are normalised instead, since both have a sensible server-chosen default.
func curveRequest(in *contract.DecayCurveRequest) (*curveSpec, error) {
	if in == nil {
		return nil, nil
	}

	significance := in.GetSignificance()

	if math.IsNaN(significance) || math.IsInf(significance, 0) || significance <= 0 {
		return nil, status.Error(grpccodes.InvalidArgument, "curve significance must be a positive number")
	}

	maxAgeDays := in.GetMaxAgeDays()
	if math.IsNaN(maxAgeDays) || math.IsInf(maxAgeDays, 0) || maxAgeDays < 0 {
		return nil, status.Error(grpccodes.InvalidArgument, "curve max_age_days must be a positive number, or zero to have one chosen")
	}

	points := int(in.GetPoints())

	switch {

	case points <= 0:
		points = curveDefaultPoints

	case points > curveMaxPoints:
		points = curveMaxPoints

	}

	return &curveSpec{significance: significance, maxAgeDays: maxAgeDays, points: points}, nil
}

// cachedDecisionSnapshot returns the decision snapshot, recomputing it only once it has aged past
// explainStateTTL. Concurrent refreshes collapse onto one through explainGroup - a separate group
// from both sleepGroup and previewGroup, since this shares neither their key space nor their
// meaning.
//
// The cache is what makes the RPC cheap enough to call on every console page: the snapshot itself
// costs a used-bytes reading and a memory count, both full scans on the server drivers, while the
// per-memory work below is an indexed lookup of the ids the caller named.
func (s *Server) cachedDecisionSnapshot(ctx context.Context) (decisionState, error) {
	s.explainStateMu.Lock()
	cached := s.explainState
	s.explainStateMu.Unlock()

	if !cached.at.IsZero() && time.Since(cached.at) < explainStateTTL {
		return cached, nil
	}

	shared, err, _ := s.explainGroup.Do(explainStateKey, func() (any, error) {
		state, err := s.decisionSnapshot(ctx)
		if err != nil {
			return decisionState{}, err
		}

		state.at = time.Now()

		s.explainStateMu.Lock()
		s.explainState = state
		s.explainStateMu.Unlock()

		return state, nil
	})
	if err != nil {
		return decisionState{}, err
	}

	state, ok := shared.(decisionState)
	if !ok {
		return decisionState{}, status.Error(grpccodes.Internal, "consolidation state produced an unexpected result")
	}

	return state, nil
}

// explainResponse builds the response from a snapshot and the candidates found for it. Valuations
// come back in the order the ids were asked for - the store returns them in its own order, and a
// client that laid out a table from its request should not have to re-sort the answer.
func (s *Server) explainResponse(
	state decisionState,
	candidates []db.IdentifiedMemoryCandidate,
	ids []string,
	curve *curveSpec,
) *contract.ExplainConsolidationResponse {
	threshold := s.consolidation.deletionThreshold * state.decider.capacityPressure

	res := contract.ExplainConsolidationResponse{
		CapacityPressure:       state.decider.capacityPressure,
		DeletionThreshold:      threshold,
		UsedBytes:              state.usedBytes,
		CapacityBytes:          s.consolidation.capacityBytes,
		MemoryCount:            int32(state.memoryCount),
		CapacityMemories:       int32(s.consolidation.capacityMemories),
		Method:                 int32(s.consolidation.method),
		Aggressiveness:         s.consolidation.aggressiveness,
		UnitsOfAgeInDays:       s.consolidation.unitsOfAgeInDays,
		MinimumAgeInDays:       int32(s.consolidation.minimumAgeInDays),
		MinimumRetentionInDays: int32(s.consolidation.minimumRetentionInDays),
		Valuations:             make([]*contract.MemoryValuation, 0, len(candidates)),
	}

	found := make(map[string]db.IdentifiedMemoryCandidate, len(candidates))
	for _, v := range candidates {
		found[v.Id] = v
	}

	for _, id := range ids {
		candidate, ok := found[id]
		if !ok {
			continue
		}

		res.Valuations = append(res.Valuations, s.valuation(candidate, state, threshold))
	}

	if curve != nil {
		res.Curve = s.decayCurve(*curve, threshold)
	}

	return &res
}

// valuation reports where one memory stands: what the decay algorithm makes of it now, and what
// would have to change for a cycle to take it. Every decision goes through the snapshot's decider,
// so the flags agree with the cycle rather than approximating it.
func (s *Server) valuation(
	candidate db.IdentifiedMemoryCandidate,
	state decisionState,
	threshold float64,
) *contract.MemoryValuation {
	decayTimestamp := memoryDecayTimestamp(candidate.Candidate)
	significance := s.memorySignificanceUnder(candidate.Candidate, state.decider.defaultEventSignificance)
	ageDays := float64(time.Now().UnixNano()-decayTimestamp) / float64(DAY_IN_NANOSECONDS)

	// The two link terms are reported alongside effective_significance rather than folded silently
	// into it. That field documents itself as the full breakdown of what decay acts on, and a
	// component the caller cannot see is a number they cannot reconcile - which matters more than
	// usual here, because the damping means the raw significance and its contribution differ by
	// orders of magnitude, and re-tuning the weight is done by reading exactly these two figures.
	weight := s.consolidation.linkSignificanceWeight

	return &contract.MemoryValuation{
		Id:                    candidate.Id,
		EventId:               candidate.EventId,
		Significance:          candidate.Candidate.MemorySignificance,
		EffectiveSignificance: significance,

		LinkSignificance:      candidate.Candidate.MemoryLinkSignificance,
		LinkContribution:      linkContribution(weight, candidate.Candidate.MemoryLinkSignificance),
		EventLinkSignificance: candidate.Candidate.EventLinkSignificance,
		EventLinkContribution: linkContribution(weight, candidate.Candidate.EventLinkSignificance),

		Value:              state.decider.MemoryValue(candidate.Candidate),
		Threshold:          threshold,
		AgeDays:            ageDays,
		RecallCount:        candidate.Candidate.RecallCount,
		TimeRecalled:       candidate.Candidate.TimeRecalled,
		WouldConsolidate:   state.decider.ShouldConsolidateMemory(candidate.Candidate),
		Retained:           s.retained(decayTimestamp),
		BelowMinimumAge:    int(ageDays) < s.consolidation.minimumAgeInDays,
		DaysUntilForgotten: s.daysUntilForgotten(significance, ageDays, threshold),
	}
}

// daysUntilForgotten projects how long a memory has left: the age at which its value falls below
// the threshold, deferred by whichever of the two age floors (minimumAgeInDays, and the harder
// minimumRetentionInDays) has not yet passed, less the age it has already reached.
//
// It holds today's threshold and capacity pressure and assumes no further recall, both of which
// would move the answer - a recall resets the clock outright, and a filling store pulls the date
// forward. It is a projection of the current configuration against a memory, not a promise. Zero
// means the memory is already due; -1 means it is not due within curveHorizonUnits, which is what
// a configuration that will effectively never forget this memory looks like.
func (s *Server) daysUntilForgotten(significance float64, ageDays float64, threshold float64) float64 {
	crossingUnits := s.crossingAgeUnits(significance, threshold)
	if crossingUnits < 0 {
		return -1
	}

	dueDays := crossingUnits * s.consolidation.unitsOfAgeInDays

	// The floors are compared in whole days because that is how shouldConsolidate and retained
	// measure them: an item becomes eligible as its age reaches the configured day count.
	if floor := float64(s.consolidation.minimumAgeInDays); floor > dueDays {
		dueDays = floor
	}

	if floor := float64(s.consolidation.minimumRetentionInDays); floor > dueDays {
		dueDays = floor
	}

	if dueDays <= ageDays {
		return 0
	}

	return dueDays - ageDays
}

// crossingAgeUnits returns the age, in age units, at which an item of the given significance first
// falls below the threshold, or -1 when it does not do so within curveHorizonUnits.
//
// Every decay method is non-increasing in age, so the crossing is found by bisection over the
// horizon rather than by inverting six different curves - which keeps this honest as methods are
// added, since it asks calculateValue rather than restating it.
func (s *Server) crossingAgeUnits(significance float64, threshold float64) float64 {
	if threshold <= 0 {
		return -1
	}

	if s.calculateValue(significance, curveHorizonUnits) >= threshold {
		return -1
	}

	low, high := 0.0, curveHorizonUnits

	for range curveCrossingIterations {
		mid := (low + high) / 2

		if s.calculateValue(significance, mid) < threshold {
			high = mid
		} else {
			low = mid
		}
	}

	return high
}

// decayCurve samples the configured algorithm for one significance value. The span, when the caller
// did not choose one, is sized to show the threshold crossing rather than an arbitrary window: a
// curve that stops short of the crossing hides the one thing an operator is looking for.
func (s *Server) decayCurve(spec curveSpec, threshold float64) *contract.DecayCurve {
	crossingUnits := s.crossingAgeUnits(spec.significance, threshold)

	crossingDays := -1.0
	if crossingUnits >= 0 {
		crossingDays = crossingUnits * s.consolidation.unitsOfAgeInDays
	}

	maxAgeDays := spec.maxAgeDays

	if maxAgeDays <= 0 {
		spanUnits := float64(curveDefaultSpanUnits)

		if crossingUnits > 0 {
			spanUnits = crossingUnits * 1.5
		}

		maxAgeDays = spanUnits * s.consolidation.unitsOfAgeInDays
	}

	curve := contract.DecayCurve{
		Significance: spec.significance,
		MaxAgeDays:   maxAgeDays,
		Points:       make([]*contract.DecayPoint, 0, spec.points),
	}

	// Reported only when it falls inside the projected span: a crossing off the end of the plot is
	// not something a client can mark, and claiming one it cannot show would be misleading.
	if crossingDays >= 0 && crossingDays <= maxAgeDays {
		curve.CrossingAgeDays = crossingDays
	} else {
		curve.CrossingAgeDays = -1
	}

	step := maxAgeDays / float64(spec.points)

	for i := 1; i <= spec.points; i++ {
		ageDays := step * float64(i)

		value := s.calculateValue(spec.significance, ageDays/s.consolidation.unitsOfAgeInDays)

		// Sampling starts one step in rather than at age zero, where several of the methods are
		// unbounded; a step that still lands on a non-finite value (an age that underflows, a
		// misconfigured unit size) is dropped rather than emitted, since neither JSON nor a plot has
		// anywhere to put an infinity.
		if math.IsNaN(value) || math.IsInf(value, 0) {
			continue
		}

		curve.Points = append(curve.Points, &contract.DecayPoint{AgeDays: ageDays, Value: value})
	}

	return &curve
}
