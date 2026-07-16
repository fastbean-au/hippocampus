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

func (s *Server) sleep() error {
	log.Debug("sleep()")

	// The sleep cycle runs in the background, so it starts its own trace rather than continuing
	// one from an RPC.
	ctx, span := tel.tracer.Start(context.Background(), "sleep")
	defer span.End()

	ts := time.Now()

	e1 := s.consolidate(ctx)

	s.scanSummarizationCandidates(ctx)

	e2 := s.evict(ctx)

	e3 := s.preserve(ctx)

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

	return err
}

func (s *Server) consolidate(ctx context.Context) error {
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

	tel.memoriesConsolidated.Add(ctx, int64(md), metric.WithAttributes(attribute.Bool("has_event", false)))
	span.AddEvent("memories_without_events_consolidated", trace.WithAttributes(
		attribute.Int("memories_deleted", md),
	))

	// Second pass - memories with events
	emd, e, ed, e2 := s.db.ConsolidateEventMemories(ctx, s)
	log.Infof("consolidated %d memories associated with an event from %d events, deleting %d events", emd, e, ed)

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

// scanSummarizationCandidates identifies events whose memories have accumulated enough
// (consolidation.summarizationMinMemories) and gone quiet for long enough
// (consolidation.summarizationMinAgeInDays, measured from each memory's own decay timestamp) to
// be worth condensing into a single summary memory. The service has no visibility into memory
// content, so it cannot generate the summary itself: this only surfaces candidates via
// GetSummarizationCandidates, leaving the actual summarization (ReplaceMemoriesWithSummary) to
// the caller. A non-positive summarizationMinMemories disables the scan. Failure is logged and
// otherwise ignored, matching the best-effort treatment of the percentile calculation above — a
// stale or empty candidate list must not fail the sleep cycle.
func (s *Server) scanSummarizationCandidates(ctx context.Context) {
	log.Debug("scanSummarizationCandidates()")

	if s.consolidation.summarizationMinMemories <= 0 {
		return
	}

	_, span := tel.tracer.Start(ctx, "scan_summarization_candidates")
	defer span.End()

	maxTimestamp := time.Now().UnixNano() - int64(s.consolidation.summarizationMinAgeInDays)*DAY_IN_NANOSECONDS

	candidates, err := s.db.FindSummarizationCandidates(ctx,
		s.consolidation.summarizationMinMemories,
		maxTimestamp,
		s.consolidation.summarizationMaxCandidates,
	)
	if err != nil {
		log.Errorf("failed to scan for summarization candidates: %s", err.Error())

		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())

		return
	}

	s.summarizationCandidatesMu.Lock()
	s.summarizationCandidates = candidates
	s.summarizationCandidatesMu.Unlock()

	log.Infof("identified %d summarization candidates", len(candidates))

	tel.summarizationCandidates.Record(ctx, int64(len(candidates)))
	span.AddEvent("summarization_candidates_identified", trace.WithAttributes(
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
func (s *Server) evict(ctx context.Context) error {
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
	ageNanoSeconds := time.Now().UnixNano() - memoryDecayTimestamp(candidate)

	ageUnits := (float64(ageNanoSeconds) / float64(DAY_IN_NANOSECONDS)) / s.consolidation.unitsOfAgeInDays

	if ageUnits <= 0 {
		return math.MaxFloat64
	}

	return s.calculateValue(s.memorySignificance(candidate), ageUnits)
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

// memorySignificance combines the memory's own significance with its event's, the weighted
// relationship significance, and the weighted recall count.
func (s *Server) memorySignificance(candidate db.MemoryConsolidationCandidate) float64 {
	eventSignificance := candidate.EventSignificance
	if eventSignificance == 0 {
		eventSignificance = s.consolidation.defaultEventSignificanceValue
	}

	return float64(eventSignificance+candidate.MemorySignificance) +
		s.consolidation.relationshipSignificanceWeight*float64(candidate.RelationshipSignificance) +
		s.consolidation.recallSignificanceWeight*float64(candidate.RecallCount)
}

// ShouldConsolidateEvent decides whether an event with no associated memories has decayed below
// the deletion threshold. The event's own significance and its relationship significance count
// towards its value; its age is measured from the most recent of its start and end times.
func (s *Server) ShouldConsolidateEvent(candidate db.EventConsolidationCandidate) bool {

	timestamp := candidate.TimeStart
	if candidate.TimeEnd > timestamp {
		timestamp = candidate.TimeEnd
	}

	significance := float64(candidate.Significance) +
		s.consolidation.relationshipSignificanceWeight*float64(candidate.RelationshipSignificance)

	return s.shouldConsolidate(significance, timestamp)
}

// shouldConsolidate applies the decay and threshold rules shared by memories and events: items
// younger than the minimum age never consolidate, and otherwise the decayed value is compared
// against the deletion threshold scaled by the current capacity pressure.
func (s *Server) shouldConsolidate(significance float64, timestamp int64) bool {
	ageNanoSeconds := time.Now().UnixNano() - timestamp

	if int(ageNanoSeconds/DAY_IN_NANOSECONDS) < s.consolidation.minimumAgeInDays {
		return false
	}

	ageUnits := (float64(ageNanoSeconds) / float64(DAY_IN_NANOSECONDS)) / s.consolidation.unitsOfAgeInDays

	if ageUnits <= 0 {
		return false
	}

	val := s.calculateValue(significance, ageUnits)

	pressure := s.consolidation.capacityPressure
	if pressure <= 0 {
		pressure = 1.0
	}

	return val < s.consolidation.deletionThreshold*pressure
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
