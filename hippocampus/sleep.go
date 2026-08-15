package hippocampus

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	log "github.com/sirupsen/logrus"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/fastbean-au/hippocampus/db"
)

const DAY_IN_NANOSECONDS = 86400 * 1000000000

func (s *Server) sleep(trigger string) error {
	log.Debug("sleep()")

	// The sleep cycle runs in the background, so it starts its own trace rather than continuing
	// one from an RPC.
	ctx, span := tel.tracer.Start(context.Background(), "sleep")
	defer span.End()

	ts := time.Now()

	// The report this cycle publishes for GetConsolidationStatus. It is filled in beside the
	// existing telemetry as each pass returns, so the counts come from the passes themselves rather
	// than from the instruments they also feed - a deployment exporting no telemetry still gets
	// them, and there is no second source of truth to drift.
	report := &cycleReport{startedAt: ts, trigger: trigger}

	e1 := s.consolidate(ctx, report)

	s.scanSummarisationCandidates(ctx, report)

	s.autoSummariseCandidates(ctx)

	e2 := s.evict(ctx, report)

	// Before preserve(), so pages the trimmed log frees are returned by the same compaction that
	// returns the ones consolidation freed.
	s.pruneTombstones(ctx)

	e3 := s.preserve(ctx)

	// Best-effort registry maintenance: keeps significance ranks compact and inside int32 after
	// repeated relative insertions. It never fails the sleep cycle (a no-op until inflation warrants
	// it), so it sits outside the success flag.
	if err := s.db.CompactSignificanceLevels(ctx); err != nil {
		log.Warnf("significance registry compaction failed: %s", err.Error())
	}

	success := e1 == nil && e2 == nil && e3 == nil

	tel.sleeps.Add(ctx, 1, metric.WithAttributes(attribute.Bool("success", success)))
	tel.sleepDuration.Record(ctx, time.Since(ts).Seconds())

	var err error
	switch {

	case e1 != nil:
		err = fmt.Errorf("failed to consolidate memories: %w", e1)

	case e2 != nil:
		err = fmt.Errorf("failed to evict memories to the capacity target: %w", e2)

	case e3 != nil:
		err = fmt.Errorf("failed to preserve consolidated memories: %w", e3)
	}

	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}

	// Publish last, so a reader either sees the previous cycle's report or this one complete, never
	// a half-filled one. The counts above stay accurate for whatever did complete even on a failure,
	// which is why a failed cycle still publishes rather than leaving the last success standing:
	// "the last cycle deleted 40 and then failed" is the reading an operator needs.
	report.duration = time.Since(ts)
	report.success = success

	if err != nil {
		report.failure = err.Error()
	}

	s.lastCycle.Store(report)

	return err
}

// cycleReport is what one sleep cycle did. The RPC layer projects it into contract.CycleReport;
// this stays an internal struct so the passes can fill it in with Go types (a time.Duration, a
// time.Time) rather than the wire's integers.
//
// Counts only. Recording which ids went is the forgotten log's job (db/tombstone.go), and keeping
// that separation is what lets this be reader-tier while the log is admin.
type cycleReport struct {
	startedAt time.Time
	duration  time.Duration
	trigger   string

	memoriesConsolidated int
	eventsConsolidated   int
	memoriesEvicted      int
	eventsEvicted        int
	bytesFreed           int64

	summarisationCandidates int

	success bool
	failure string
}

func (s *Server) consolidate(ctx context.Context, report *cycleReport) error {
	log.Debug("consolidate()")

	ctx, span := tel.tracer.Start(ctx, "consolidate")
	defer span.End()

	if s.consolidation.defaultEventSignificancePercentile != 0 {

		// The percentile cannot be computed over an empty event store; retain the current value
		// (the configured fixed value, or the last computed percentile) rather than failing the
		// whole sleep cycle.
		v, err := s.db.CalculateSignificancePercentile(ctx, s.consolidation.defaultEventSignificancePercentile)
		if err != nil {
			log.Warnf(
				"default event significance percentile unavailable, retaining %d: %s",
				s.consolidation.defaultEventSignificanceValue,
				err.Error(),
			)

			span.AddEvent("default_event_significance_retained", trace.WithAttributes(
				attribute.Int("default_event_significance", int(s.consolidation.defaultEventSignificanceValue)),
			))
		} else {
			s.consolidation.defaultEventSignificanceValue = int32(v)
			log.Infof("default event significance: %d (%0.2f)", int(v), v)

			span.AddEvent("default_event_significance_calculated", trace.WithAttributes(
				attribute.Int("default_event_significance", int(v)),
			))
		}
	}

	with, without := s.db.CountMemories(ctx)
	if with >= 0 && without >= 0 {

		// The byte measure only contributes when a byte capacity is configured; it reuses the
		// reading eviction took at the end of the previous cycle rather than scanning the tables a
		// second time this cycle. Pressure is a smoothing factor, so a one-cycle-old
		// byte figure is fine - and the hard byte cap is still enforced against a fresh reading in
		// evict(). Zero on the first cycle (no prior reading yet), leaving pressure to the row count.
		var usedBytes int64
		if s.consolidation.capacityBytes > 0 {
			usedBytes = s.consolidation.lastUsedBytes
		}

		s.consolidation.capacityPressure = s.calculateCapacityPressure(with+without, usedBytes)
		log.Infof(
			"capacity pressure: %0.3f (%d memories, %d bytes used)",
			s.consolidation.capacityPressure,
			with+without,
			usedBytes,
		)

		tel.capacityPressure.Record(ctx, s.consolidation.capacityPressure)
		span.AddEvent("capacity_pressure_calculated", trace.WithAttributes(
			attribute.Float64("capacity_pressure", s.consolidation.capacityPressure),
			attribute.Int("memory_count", with+without),
			attribute.Int64("used_bytes", usedBytes),
		))
	}

	// First pass - memories without events
	md, e1 := s.db.ConsolidateMemories(ctx, s)
	log.Infof("consolidated %d memories not associated with an event", md)

	report.memoriesConsolidated += md

	tel.memoriesConsolidated.Add(ctx, int64(md), metric.WithAttributes(attribute.Bool("has_event", false)))
	span.AddEvent("memories_without_events_consolidated", trace.WithAttributes(
		attribute.Int("memories_deleted", md),
	))

	// Second pass - memories with events
	emd, e, ed, e2 := s.db.ConsolidateEventMemories(ctx, s)
	log.Infof("consolidated %d memories associated with an event from %d events, deleting %d events", emd, e, ed)

	report.memoriesConsolidated += emd
	report.eventsConsolidated += ed

	tel.memoriesConsolidated.Add(ctx, int64(emd), metric.WithAttributes(attribute.Bool("has_event", true)))
	tel.eventsConsolidated.Add(ctx, int64(ed), metric.WithAttributes(attribute.Bool("has_memories", true)))
	span.AddEvent("memories_with_events_consolidated", trace.WithAttributes(
		attribute.Int("memories_deleted", emd),
		attribute.Int("events_scanned", e),
		attribute.Int("events_deleted", ed),
	))

	// Third pass - events without memories
	ec, e3 := s.db.ConsolidateEvents(ctx, s)
	log.Infof("consolidated %d events without memories", ec)

	report.eventsConsolidated += ec

	tel.eventsConsolidated.Add(ctx, int64(ec), metric.WithAttributes(attribute.Bool("has_memories", false)))
	span.AddEvent("events_without_memories_consolidated", trace.WithAttributes(
		attribute.Int("events_deleted", ec),
	))

	// Every pass runs regardless of an earlier failure (each logs its own error and the counts
	// above stay accurate for what did complete); errors.Join collapses them into a single non-nil
	// result when any pass failed, so the sleep cycle's success metric is honest. errors.Join
	// returns nil when all three are nil.
	return errors.Join(e1, e2, e3)
}

// scanSummarisationCandidates identifies events whose memories have accumulated enough
// (consolidation.summarisationMinMemories) and gone quiet for long enough
// (consolidation.summarisationMinAgeInDays, measured from each memory's own decay timestamp) to
// be worth condensing into a single summary memory. The service has no visibility into memory
// content, so it cannot generate the summary itself: this only surfaces candidates via
// GetSummarisationCandidates, leaving the actual summarisation (ReplaceMemoriesWithSummary) to
// the caller. A non-positive summarisationMinMemories disables the scan. Failure is logged and
// otherwise ignored, matching the best-effort treatment of the percentile calculation above — a
// stale or empty candidate list must not fail the sleep cycle.
func (s *Server) scanSummarisationCandidates(ctx context.Context, report *cycleReport) {
	log.Debug("scanSummarisationCandidates()")

	if s.consolidation.summarisationMinMemories <= 0 {
		return
	}

	_, span := tel.tracer.Start(ctx, "scan_summarisation_candidates")
	defer span.End()

	maxTimestamp := time.Now().UnixNano() - int64(s.consolidation.summarisationMinAgeInDays)*DAY_IN_NANOSECONDS

	candidates, err := s.db.FindSummarisationCandidates(ctx,
		s.consolidation.summarisationMinMemories,
		maxTimestamp,
		s.consolidation.summarisationMaxCandidates,
	)
	if err != nil {
		log.Errorf("failed to scan for summarisation candidates: %s", err.Error())

		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())

		return
	}

	s.summarisationCandidatesMu.Lock()
	s.summarisationCandidates = candidates
	s.summarisationCandidatesMu.Unlock()

	log.Infof("identified %d summarisation candidates", len(candidates))

	report.summarisationCandidates = len(candidates)

	tel.summarisationCandidates.Record(ctx, int64(len(candidates)))
	span.AddEvent("summarisation_candidates_identified", trace.WithAttributes(
		attribute.Int("candidates", len(candidates)),
	))
}

// evictionFloor returns the level eviction reclaims down to once the capacity target has been
// crossed. A floor below the target (consolidation.capacityBytesFloor) provides hysteresis:
// each eviction leaves headroom, spacing evictions out instead of trimming a sliver every
// cycle. An unset or invalid floor (non-positive, or above the target) falls back to the
// capacity target itself.
func (s *Server) evictionFloor() int64 {
	floor := s.consolidation.capacityBytesFloor

	if floor <= 0 || floor > s.consolidation.capacityBytes {
		return s.consolidation.capacityBytes
	}

	return floor
}

// evict enforces the capacity target: when the store's used bytes still exceed
// consolidation.capacityBytes after the normal consolidation passes, the least valuable memories
// are deleted until the excess is reclaimed. Unlike consolidation this applies no minimum-age
// protection — the bound must be achievable even when everything in the store is fresh — but the
// value ranking still deletes the least significant, least recently recalled memories first.
func (s *Server) evict(ctx context.Context, report *cycleReport) error {
	log.Debug("evict()")

	if s.consolidation.capacityBytes <= 0 {
		return nil
	}

	ctx, span := tel.tracer.Start(ctx, "evict")
	defer span.End()

	used, err := s.db.UsedBytes(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())

		return err
	}

	// Cache this fresh, post-consolidation reading for the next cycle's pressure calculation, so
	// pressure needs no UsedBytes scan of its own.
	s.consolidation.lastUsedBytes = used

	tel.usedBytes.Record(ctx, used)
	tel.capacityBytes.Record(ctx, s.consolidation.capacityBytes)

	s.recordRetention(ctx)

	if used <= s.consolidation.capacityBytes {
		return nil
	}

	// Reclaiming down to the floor rather than the target itself creates headroom, so the store
	// does not re-cross the target moments after the eviction and every cycle stays busy.
	excess := used - s.evictionFloor()
	log.Infof("store is using %d bytes, %d over the eviction floor - evicting", used, excess)

	span.AddEvent("capacity_target_exceeded", trace.WithAttributes(
		attribute.Int64("used_bytes", used),
		attribute.Int64("capacity_bytes", s.consolidation.capacityBytes),
	))

	memories, events, freed, err := s.db.EvictMemories(ctx, s, excess)
	log.Infof("evicted %d memories and %d events, freeing an estimated %d bytes", memories, events, freed)

	report.memoriesEvicted += memories
	report.eventsEvicted += events
	report.bytesFreed += freed

	tel.memoriesEvicted.Add(ctx, int64(memories))
	tel.eventsEvicted.Add(ctx, int64(events))
	tel.bytesEvicted.Add(ctx, freed)
	span.AddEvent("memories_evicted", trace.WithAttributes(
		attribute.Int("memories_deleted", memories),
		attribute.Int("events_deleted", events),
		attribute.Int64("bytes_freed", freed),
	))

	// Record what was evicted above regardless, then surface any failure so the sleep cycle's
	// success metric reflects it.
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())

		return err
	}

	return nil
}

// recordRetention publishes how much of the store the minimum retention floor is holding, as a
// count and as bytes.
//
// It is called from evict, so it costs nothing unless a byte capacity is configured - and it is
// skipped entirely without a retention floor, when the answer is always zero. That gate is the
// point: the pair only means anything against a capacity target, because retention OVERRIDES that
// target. Once retained_bytes approaches capacity_bytes the store cannot be brought back under its
// capacity however hard eviction runs, and this is what makes that visible before it becomes a disk
// problem rather than only when someone asks for a dry run.
//
// Best-effort: it is one extra aggregate scan per cycle, and a failure must not fail the cycle, so
// it logs and moves on, leaving the gauges at their previous reading.
func (s *Server) recordRetention(ctx context.Context) {
	if s.consolidation.minimumRetentionInDays <= 0 {
		return
	}

	cutoff := time.Now().UnixNano() - int64(s.consolidation.minimumRetentionInDays)*DAY_IN_NANOSECONDS

	count, bytes, err := s.db.RetainedStats(ctx, cutoff)
	if err != nil {
		log.Warnf("failed to measure retained memories: %s", err.Error())

		return
	}

	tel.memoriesRetained.Record(ctx, int64(count))
	tel.retainedBytes.Record(ctx, bytes)

	// Worth a log line, not only a metric: this is the condition under which the capacity target
	// silently stops being achievable, and not every deployment runs the metrics stack.
	if bytes >= s.consolidation.capacityBytes {
		log.Warnf(
			"minimum retention is holding %d bytes across %d memories, at or above the %d byte capacity target - eviction cannot bring the store under its capacity until this data ages out",
			bytes,
			count,
			s.consolidation.capacityBytes,
		)
	}
}

func (s *Server) preserve(ctx context.Context) error {
	log.Debug("preserve()")

	_, span := tel.tracer.Start(ctx, "preserve")
	defer span.End()

	if err := s.db.Preserve(ctx); err != nil {
		err = fmt.Errorf("failed to preserve data")
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())

		return err
	}

	return nil
}

func (s *Server) ShouldConsolidateMemory(candidate db.MemoryConsolidationCandidate) bool {
	return s.shouldConsolidate(s.memorySignificance(candidate), memoryDecayTimestamp(candidate))
}

// MemoryValue returns the memory's current decayed value under the configured consolidation
// algorithm. Capacity eviction uses it to rank memories from least to most valuable; a memory
// with no age yet (or a future timestamp) ranks as maximally valuable.
func (s *Server) MemoryValue(candidate db.MemoryConsolidationCandidate) float64 {
	return s.memoryValueUnder(candidate, s.consolidation.defaultEventSignificanceValue)
}

// memoryValueUnder is MemoryValue against an explicitly supplied default event significance; see
// memorySignificanceUnder.
func (s *Server) memoryValueUnder(candidate db.MemoryConsolidationCandidate, defaultEventSignificance int32) float64 {
	ageNanoSeconds := time.Now().UnixNano() - memoryDecayTimestamp(candidate)

	ageUnits := (float64(ageNanoSeconds) / float64(DAY_IN_NANOSECONDS)) / s.consolidation.unitsOfAgeInDays

	if ageUnits <= 0 {
		return math.MaxFloat64
	}

	return s.calculateValue(s.memorySignificanceUnder(candidate, defaultEventSignificance), ageUnits)
}

// MemoryRetained reports whether a memory is still within the configured minimum retention window
// (consolidation.minimumRetentionInDays), so capacity eviction must exclude it from the candidate
// pool entirely — a retained memory is never evicted even when the store is over its byte target.
// Eviction, unlike consolidation, has no per-item value threshold to fold the retention check into,
// so it consults this directly.
func (s *Server) MemoryRetained(candidate db.MemoryConsolidationCandidate) bool {
	return s.retained(memoryDecayTimestamp(candidate))
}

// memoryDecayTimestamp returns the timestamp the memory decays from. Recalling a memory resets
// its decay clock: age is measured from the most recent of the creation timestamp and the last
// recall.
func memoryDecayTimestamp(candidate db.MemoryConsolidationCandidate) int64 {
	timestamp := candidate.Timestamp
	if candidate.TimeRecalled > timestamp {
		timestamp = candidate.TimeRecalled
	}

	return timestamp
}

// linkContribution is what an item's links add to its effective significance: the weight applied to
// the natural logarithm of one plus the summed link significance.
//
// The damping is the whole point. A link is a client-supplied input into the decay maths, and the
// bounds in types (128 links of up to 1,000,000 each) still admit a summed significance of 1.28e8 -
// which, added linearly, would swamp every other term and make a well-connected memory
// unforgettable, defeating the storage bound that capacity eviction exists to hold. log1p turns that
// worst case into ~18.7 before weighting, so the tenth link adds far less than the second and the
// hundredth barely registers: being connected raises an item's standing, and cannot buy immortality.
//
// log1p rather than log because a sum of zero must contribute exactly zero rather than diverging,
// and the same damping is applied to recall counts in ranking.go for the same skew reason. A
// negative sum is impossible (validation rejects negative significances) but is treated as zero
// rather than trusted into a NaN.
func linkContribution(weight float64, sum int64) float64 {
	if sum <= 0 {
		return 0
	}

	return weight * math.Log1p(float64(sum))
}

// memorySignificance combines the memory's own significance with its event's, the damped
// contributions of the memory's own links and its event's links, and the weighted recall count.
func (s *Server) memorySignificance(candidate db.MemoryConsolidationCandidate) float64 {
	return s.memorySignificanceUnder(candidate, s.consolidation.defaultEventSignificanceValue)
}

// memorySignificanceUnder is memorySignificance against an explicitly supplied default event
// significance, for the same reason shouldConsolidateUnder takes a pressure: the field is
// recomputed by each sleep cycle when consolidation.defaultEventSignificancePercentile is set, so
// a preview running on an RPC goroutine must work from its own snapshot rather than read it.
func (s *Server) memorySignificanceUnder(candidate db.MemoryConsolidationCandidate, defaultEventSignificance int32) float64 {
	eventSignificance := candidate.EventSignificance
	if eventSignificance == 0 {
		eventSignificance = defaultEventSignificance
	}

	// The memory's links and its event's links are damped separately rather than summed first: they
	// are different populations - one memory among an event's memories, one event among the store's
	// events - and one logarithm over both would let a heavily linked event flatten the difference
	// between its own memories.
	weight := s.consolidation.linkSignificanceWeight

	return float64(eventSignificance+candidate.MemorySignificance) +
		linkContribution(weight, candidate.EventLinkSignificance) +
		linkContribution(weight, candidate.MemoryLinkSignificance) +
		s.consolidation.recallSignificanceWeight*float64(candidate.RecallCount)
}

// ShouldConsolidateEvent decides whether an event with no associated memories has decayed below
// the deletion threshold. The event's own significance and its damped link contribution count
// towards its value; its age is measured from the most recent of its start and end times.
func (s *Server) ShouldConsolidateEvent(candidate db.EventConsolidationCandidate) bool {
	return s.shouldConsolidateEventUnder(candidate, s.consolidation.capacityPressure)
}

// shouldConsolidateEventUnder is ShouldConsolidateEvent against an explicitly supplied capacity
// pressure; see shouldConsolidateUnder.
func (s *Server) shouldConsolidateEventUnder(candidate db.EventConsolidationCandidate, pressure float64) bool {

	timestamp := candidate.TimeStart
	if candidate.TimeEnd > timestamp {
		timestamp = candidate.TimeEnd
	}

	significance := float64(candidate.Significance) +
		linkContribution(s.consolidation.linkSignificanceWeight, candidate.LinkSignificance)

	return s.shouldConsolidateUnder(significance, timestamp, pressure)
}

// retained reports whether an item whose decay clock reads timestamp is still inside the configured
// minimum retention window, and so must never be reaped — by consolidation OR eviction — regardless
// of how far its value has decayed or how full the store is. This is the hard floor
// consolidation.minimumRetentionInDays provides: unlike minimumAgeInDays (which only defers
// value-based consolidation and is ignored by capacity eviction), retention also overrides the
// capacity target. A non-positive minimumRetentionInDays disables it, so nothing is retained on
// this basis. The window is measured from the same decay timestamp age is (creation, or the most
// recent recall for a memory; start or end for an event), so a recalled memory's retention is
// renewed with its decay clock — always at least the minimum since creation.
func (s *Server) retained(timestamp int64) bool {
	if s.consolidation.minimumRetentionInDays <= 0 {
		return false
	}

	ageNanoSeconds := time.Now().UnixNano() - timestamp

	return int(ageNanoSeconds/DAY_IN_NANOSECONDS) < s.consolidation.minimumRetentionInDays
}

// shouldConsolidate applies the decay and threshold rules shared by memories and events: items
// still inside the minimum retention window or younger than the minimum age never consolidate, and
// otherwise the decayed value is compared against the deletion threshold scaled by the current
// capacity pressure.
func (s *Server) shouldConsolidate(significance float64, timestamp int64) bool {
	return s.shouldConsolidateUnder(significance, timestamp, s.consolidation.capacityPressure)
}

// shouldConsolidateUnder is shouldConsolidate against an explicitly supplied capacity pressure,
// rather than the server's current one. The sleep cycle passes its own live field (via
// shouldConsolidate); PreviewConsolidation passes a snapshot it computed itself, because it runs
// on an RPC goroutine while the sleep goroutine may be rewriting that field - and a preview must
// in any case evaluate against one consistent pressure for the whole scan rather than whatever
// happens to be stored as each row is considered.
func (s *Server) shouldConsolidateUnder(significance float64, timestamp int64, pressure float64) bool {
	if s.retained(timestamp) {
		return false
	}

	ageNanoSeconds := time.Now().UnixNano() - timestamp

	if int(ageNanoSeconds/DAY_IN_NANOSECONDS) < s.consolidation.minimumAgeInDays {
		return false
	}

	ageUnits := (float64(ageNanoSeconds) / float64(DAY_IN_NANOSECONDS)) / s.consolidation.unitsOfAgeInDays

	if ageUnits <= 0 {
		return false
	}

	return s.calculateValue(significance, ageUnits) < s.deletionThresholdUnder(pressure)
}

// DeletionThreshold is the value a memory must stay above to survive consolidation, as it stands
// for the current cycle: the configured threshold scaled by the capacity pressure the last cycle
// computed. The forgotten log records it beside each memory's value (see db/tombstone.go), because
// pressure moves and a value with nothing to measure it against records nothing.
func (s *Server) DeletionThreshold() float64 {
	return s.deletionThresholdUnder(s.consolidation.capacityPressure)
}

// deletionThresholdUnder is DeletionThreshold against an explicitly supplied capacity pressure,
// for the same callers shouldConsolidateUnder has. A non-positive pressure means "no reading yet"
// - the first cycle, or both capacities disabled - and leaves the threshold unscaled.
func (s *Server) deletionThresholdUnder(pressure float64) float64 {
	if pressure <= 0 {
		pressure = 1.0
	}

	return s.consolidation.deletionThreshold * pressure
}

// calculateCapacityPressure returns the multiplier applied to the deletion threshold based on how
// full the memory store is. Fullness is the greater of the row-count utilisation (against
// capacityMemories) and the byte utilisation (against capacityBytes) — row count is a poor proxy
// for storage when bodies range from bytes to hundreds of kilobytes, so whichever axis is fuller
// drives the pressure. With both capacities disabled, or an empty store, the multiplier is 1 (no
// effect); it approaches 2 as the store reaches capacity and keeps growing beyond it. The
// exponent controls how sharply pressure ramps up: higher values keep pressure negligible until
// the store is nearly full.
func (s *Server) calculateCapacityPressure(memoryCount int, usedBytes int64) float64 {
	utilisation := 0.0

	if s.consolidation.capacityMemories > 0 {
		utilisation = float64(memoryCount) / float64(s.consolidation.capacityMemories)
	}

	if s.consolidation.capacityBytes > 0 {
		byteUtilisation := float64(usedBytes) / float64(s.consolidation.capacityBytes)
		if byteUtilisation > utilisation {
			utilisation = byteUtilisation
		}
	}

	if utilisation == 0 {
		return 1.0
	}

	return 1 + math.Pow(utilisation, s.consolidation.capacityPressureExponent)
}

// sigmoidSteepness controls how sharply method 6's consolidation-window curve transitions from
// "essentially undecayed" to "essentially gone" around its midpoint (consolidation.aggressiveness,
// in age units). At this multiplier the curve sits at ~99% of significance a full window before
// the midpoint and ~1% a full window after it, regardless of how large or small the midpoint
// itself is — the shape is self-similar under rescaling, the same property method 1's power law
// has under its own exponent.
const sigmoidSteepness = 5.0

func (s *Server) calculateValue(significance float64, age float64) float64 {
	switch s.consolidation.method {
	case 1:
		return significance / (math.Pow(age, s.consolidation.aggressiveness))
	case 2:
		return significance / (age * (math.Pow(math.E, s.consolidation.aggressiveness)))
	case 3:
		// math.Log of a negative aggressiveness is NaN, and NaN fails every comparison - including
		// factor <= 0 - so it must be caught explicitly or it propagates into MemoryValue and
		// corrupts eviction's sort order (a comparator is not a valid ordering over NaN). Startup
		// validation rejects a non-positive aggressiveness, so this only guards a future caller.
		factor := 1 + math.Log(s.consolidation.aggressiveness)
		if math.IsNaN(factor) || factor <= 0 {
			return math.MaxFloat64
		}

		return significance / (age * factor)
	case 4:
		// Exponential (half-life-style) decay: a constant relative decay rate, so the value
		// halves every fixed number of age units regardless of how old the memory already is.
		// The single most common recency-weighting curve, and — unlike methods 2 and 3, whose
		// names suggest exponential decay but which are in fact linear in age — the only one of
		// the six that is actually exponential.
		return significance / math.Exp(age*s.consolidation.aggressiveness)
	case 5:
		// Logarithmic (long-tail) decay: value falls off in proportion to the logarithm of age,
		// so even very old memories retain a sliver of value and only the lowest-significance,
		// oldest memories ever cross the threshold. Suited to archival or audit-log use cases
		// that want almost everything kept. age + e keeps the logarithm's argument >= e (so its
		// value is always >= 1) without needing an age > 0 special case, since age is already
		// guaranteed positive by the caller.
		// age > 0 is guaranteed by the caller, so math.Log(age+e) is a finite positive here and the
		// factor only goes non-positive (never NaN) for this method today; the IsNaN guard matches
		// method 3's, hardening against a future path that reaches this with a non-positive age.
		factor := s.consolidation.aggressiveness * math.Log(age+math.E)
		if math.IsNaN(factor) || factor <= 0 {
			return math.MaxFloat64
		}

		return significance / factor
	case 6:
		// Sigmoid ("consolidation window") decay: value stays close to significance while age is
		// well under the aggressiveness midpoint, falls sharply around it, and approaches zero
		// well beyond it — echoing the biological idea of a consolidation window during which a
		// memory is fragile and easily lost, after which what survives is comparatively durable.
		return significance / (1 + math.Exp(sigmoidSteepness*(age/s.consolidation.aggressiveness-1)))
	default:
		return math.MaxFloat64
	}
}
