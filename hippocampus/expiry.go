package hippocampus

import (
	"context"
	"time"

	log "github.com/sirupsen/logrus"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// expiryCutoff is the creation time before which a memory has outlived
// consolidation.maximumRetentionInDays, as of now. 0 when no maximum is configured, which is what
// tells both the pass and the preview that there is no ceiling.
func (s *Server) expiryCutoff(now time.Time) int64 {
	days := s.consolidation.maximumRetentionInDays
	if days <= 0 {
		return 0
	}

	return now.UnixNano() - int64(days)*DAY_IN_NANOSECONDS
}

// expire is the sleep cycle's maximum-retention pass: every memory stored longer ago than
// consolidation.maximumRetentionInDays goes, whatever its value and however recently it was
// recalled (TODO-3 item 157). It is the store's one rule that is a ceiling rather than a
// judgement - the thing a storage-limitation or log-retention policy needs, which the decay rules
// cannot express because recall keeps resetting their clock. See db.ExpireMemories for why it
// overrides consolidation.minimumRetentionInDays.
func (s *Server) expire(ctx context.Context, report *cycleReport) error {
	cutoff := s.expiryCutoff(time.Now())
	if cutoff == 0 {
		return nil
	}

	ctx, span := tel.tracer.Start(ctx, "expire")
	defer span.End()

	result, err := s.db.ExpireMemories(ctx, cutoff)

	report.memoriesExpired += result.Memories
	report.eventsExpired += result.Events

	tel.memoriesExpired.Add(ctx, int64(result.Memories))

	span.SetAttributes(
		attribute.Int("memories_expired", result.Memories),
		attribute.Int("events_expired", result.Events),
	)

	if result.Memories > 0 {
		span.AddEvent("memories expired", trace.WithAttributes(attribute.Int("count", result.Memories)))

		log.WithFields(log.Fields{
			"memories_expired": result.Memories,
			"events_expired":   result.Events,
			"maximum_days":     s.consolidation.maximumRetentionInDays,
		}).
			Info("expired memories older than the maximum retention")
	}

	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())

		return err
	}

	return nil
}
