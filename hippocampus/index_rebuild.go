package hippocampus

import (
	"context"
	"time"

	log "github.com/sirupsen/logrus"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/fastbean-au/hippocampus/db"
)

// Returning the space forgetting leaves in the indexes (TODO-3 item 183).
//
// db/index_rebuild.go carries the finding: on PostgreSQL a store that forgets bloats its indexes by
// two mechanisms, no storage parameter holds either back, and REINDEX INDEX CONCURRENTLY returns
// both online. This is the half that decides when to ask for it, which is once a sleep cycle, on the
// consolidator, and only when storage.indexRebuild.enabled says so.
//
// It is off by default, because a configuration key's default preserves the behaviour before it,
// and before it the service never rebuilt anything. That was a decision, recorded in footprint.go:
// a store that reindexes itself surprises its operator. It still never does so on a timer. An index
// is rebuilt when its own reading - bytes against entries, the two series HippocampusIndexBloated
// reads - says it is mostly air, and then each cycle that finds it so.
//
// Two things carry the shape.
//
// It runs AFTER preserve() and BEFORE the two measurements that close the cycle, so the gauges and
// the status response describe the store the rebuild left rather than the one it found. Nothing
// else in the cycle reads the indexes after it.
//
// A failed rebuild is not retried for indexRebuildRetry. The likeliest failure is not transient: a
// role that does not own the table cannot rebuild its indexes, and a pass that retried every cycle
// would put a Warn in the log every sleep.periodSeconds for as long as the deployment ran. An hour
// is long enough for that to read as a standing fault and short enough that a timeout under load is
// tried again the same day.

// indexRebuildRetry is how long an index whose rebuild failed is left alone before the next attempt.
// A var rather than a constant so a test can shorten it.
var indexRebuildRetry = time.Hour

// IndexRebuildConfig is storage.indexRebuild.*: whether the consolidator rebuilds bloated indexes,
// and the two bounds that say an index is bloated. See db.IndexRebuildPolicy for what each bound is.
type IndexRebuildConfig struct {
	Enabled          bool
	MinBytes         int64
	MaxBytesPerEntry int64
}

// rebuildBloatedIndexes runs one rebuild pass, if the deployment asked for them, and reports what it
// did. Best-effort in the same sense the measurements after it are: it never fails a cycle, since a
// store whose indexes are large is still a store that answers.
func (s *Server) rebuildBloatedIndexes(ctx context.Context) {
	if !s.indexRebuild.Enabled {
		return
	}

	log.Debug("rebuildBloatedIndexes()")

	ctx, span := tel.tracer.Start(ctx, "rebuild_bloated_indexes")
	defer span.End()

	policy := db.IndexRebuildPolicy{
		MinBytes:         s.indexRebuild.MinBytes,
		MaxBytesPerEntry: s.indexRebuild.MaxBytesPerEntry,
		Skip:             s.indexRebuildsHeldBack(time.Now()),
	}

	rebuilds, err := s.db.RebuildBloatedIndexes(ctx, policy)
	if err != nil {
		log.Warnf("failed to measure the indexes for a rebuild pass: %s", err.Error())
		span.RecordError(err)
	}

	var reclaimed int64

	for _, rebuild := range rebuilds {
		reclaimed += s.reportIndexRebuild(ctx, span, rebuild)
	}

	span.SetAttributes(
		attribute.Int("indexes_rebuilt", len(rebuilds)),
		attribute.Int64("reclaimed_bytes", reclaimed),
	)
}

// indexRebuildsHeldBack names the indexes whose last rebuild failed within indexRebuildRetry, and
// forgets the ones whose wait is over.
func (s *Server) indexRebuildsHeldBack(now time.Time) map[string]bool {
	held := make(map[string]bool)

	for k, v := range s.indexRebuildFailures {
		if now.Sub(v) >= indexRebuildRetry {
			delete(s.indexRebuildFailures, k)

			continue
		}

		held[k] = true
	}

	return held
}

// reportIndexRebuild logs, counts and traces one rebuild, records a failure so the next passes hold
// it back, and returns the bytes it returned.
func (s *Server) reportIndexRebuild(ctx context.Context, span trace.Span, rebuild db.IndexRebuild) int64 {
	key := rebuild.Table + "." + rebuild.Index

	if rebuild.Err != nil {
		s.indexRebuildFailures[key] = time.Now()

		tel.indexRebuilds.Add(ctx, 1, metric.WithAttributes(
			attribute.String("table", rebuild.Table),
			attribute.String("outcome", "failed"),
		))

		span.AddEvent("index rebuild failed", trace.WithAttributes(
			attribute.String("table", rebuild.Table),
			attribute.String("index", rebuild.Index),
		))

		log.Warnf(
			"failed to rebuild %s on %s, at %d bytes over %d entries; it is left alone for %s: %s",
			rebuild.Index,
			rebuild.Table,
			rebuild.BytesBefore,
			rebuild.Entries,
			indexRebuildRetry,
			rebuild.Err.Error(),
		)

		return 0
	}

	reclaimed := max(rebuild.BytesBefore-rebuild.BytesAfter, 0)

	tel.indexRebuilds.Add(ctx, 1, metric.WithAttributes(
		attribute.String("table", rebuild.Table),
		attribute.String("outcome", "rebuilt"),
	))

	tel.indexRebuildReclaimedBytes.Add(ctx, reclaimed, metric.WithAttributes(
		attribute.String("table", rebuild.Table),
	))

	span.AddEvent("index rebuilt", trace.WithAttributes(
		attribute.String("table", rebuild.Table),
		attribute.String("index", rebuild.Index),
		attribute.Int64("bytes_before", rebuild.BytesBefore),
		attribute.Int64("bytes_after", rebuild.BytesAfter),
		attribute.Int64("duration_ms", rebuild.Duration.Milliseconds()),
	))

	log.Infof(
		"rebuilt %s on %s: %d bytes to %d over %d entries, in %s",
		rebuild.Index,
		rebuild.Table,
		rebuild.BytesBefore,
		rebuild.BytesAfter,
		rebuild.Entries,
		rebuild.Duration.Round(time.Millisecond),
	)

	return reclaimed
}
