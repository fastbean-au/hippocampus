package hippocampus

import (
	"context"
	"fmt"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/fastbean-au/hippocampus/archive"
)

// The scheduled export is the backup the service takes of itself (TODO-3 item 158): the whole store,
// as the archive format Export already writes, every archive.scheduledExport.intervalHours, keeping
// the newest archive.scheduledExport.keep. The archive format is the one representation that keeps
// everything the store knows - timestamps, recall history, groups, links - and Export was manual, so
// a deployment had no backup unless somebody took one.
//
// Four decisions carry it:
//
//   - It runs on the CONSOLIDATING instance only. Replicas of one store would otherwise each export
//     the same data on the same schedule.
//   - Its archives live under their own prefix (scheduledExportSegment), and only that prefix is ever
//     pruned, so keep can never delete an archive somebody exported by hand.
//   - It is stateless across restarts. The first export is scheduled one interval after the newest
//     archive already in the store, so a restart neither exports immediately nor waits a full
//     interval from scratch - the schedule is read back from what is there.
//   - Its health is published as two gauges rather than inferred: the time of the last success and
//     the interval it is meant to keep. An export that has stopped succeeding is otherwise silent,
//     and a backup nobody notices failing is worse than none.

// scheduledExportSegment is the key segment, after transfer.keyPrefix, under which scheduled archives
// are written and the only place pruning looks.
const scheduledExportSegment = "scheduled/"

// scheduledExportKeyTime is the timestamp layout in a scheduled archive's key. It sorts as text, which
// is what makes "newest" and "oldest" a sort of the listing.
const scheduledExportKeyTime = "20060102T150405Z"

// scheduledExportRetry bounds how soon a failed export is retried, so a short outage is recovered in
// minutes rather than at the next interval.
const scheduledExportRetry = 15 * time.Minute

// prefix is where scheduled archives live.
func (s *Server) scheduledExportPrefix() string {
	return s.transfer.keyPrefix + scheduledExportSegment
}

// startScheduledExport launches the scheduled export when it is configured and this instance is
// the one that should run it.
func (s *Server) startScheduledExport() {
	if s.scheduledExport.Interval <= 0 {
		return
	}

	if s.objects == nil {
		log.Warn("archive.scheduledExport.intervalHours is set but no archive store is configured (archive.directory or s3.bucket), so nothing is exported")

		return
	}

	if !s.runsConsolidatorWork() {
		log.Info("scheduled export: this instance is a replica; the consolidating instance takes the exports")

		return
	}

	tel.scheduledExportInterval.Record(context.Background(), int64(s.scheduledExport.Interval.Seconds()))

	s.stopScheduledExport = make(chan struct{})
	s.scheduledExportStopped = make(chan struct{})

	go s.scheduledExportLoop()
}

func (s *Server) scheduledExportLoop() {
	defer close(s.scheduledExportStopped)

	wait := s.firstScheduledExportWait(context.Background(), time.Now())

	log.WithFields(log.Fields{
		"interval": s.scheduledExport.Interval,
		"keep":     s.scheduledExport.Keep,
		"first_in": wait.Round(time.Second),
	}).
		Info("scheduled export enabled")

	for {
		timer := time.NewTimer(wait)

		select {

		case <-s.stopScheduledExport:
			timer.Stop()

			return

		case <-timer.C:

		}

		if err := s.runScheduledExport(context.Background(), time.Now()); err != nil {
			log.WithError(err).Error("scheduled export failed; retrying soon")

			wait = min(scheduledExportRetry, s.scheduledExport.Interval)

			continue
		}

		wait = s.scheduledExport.Interval
	}
}

// firstScheduledExportWait reads the schedule back from the archives already in the store: one
// interval after the newest, or now if that has passed. With no archive, or a store that cannot list,
// the first export is one interval away - the same as an instance that had just taken one.
func (s *Server) firstScheduledExportWait(ctx context.Context, now time.Time) time.Duration {
	pruner, ok := s.objects.(archive.Pruner)
	if !ok {
		return s.scheduledExport.Interval
	}

	keys, err := pruner.List(ctx, s.scheduledExportPrefix())
	if err != nil {
		log.WithError(err).Warn("scheduled export: could not list existing archives; scheduling one interval from now")

		return s.scheduledExport.Interval
	}

	var newest time.Time

	for _, key := range keys {
		if at, ok := scheduledExportTime(key, s.scheduledExportPrefix()); ok && at.After(newest) {
			newest = at
		}
	}

	if newest.IsZero() {
		return s.scheduledExport.Interval
	}

	if due := newest.Add(s.scheduledExport.Interval).Sub(now); due > 0 {
		return due
	}

	return 0
}

// scheduledExportTime parses the time out of a scheduled archive's key, reporting false for a key
// under the prefix that this schedule did not write.
func scheduledExportTime(key string, prefix string) (time.Time, bool) {
	name, ok := strings.CutPrefix(key, prefix)
	if !ok {
		return time.Time{}, false
	}

	stamp, _, ok := strings.Cut(name, ".")
	if !ok {
		return time.Time{}, false
	}

	at, err := time.Parse(scheduledExportKeyTime, stamp)
	if err != nil {
		return time.Time{}, false
	}

	return at, true
}

// runScheduledExport takes one export and prunes past keep. A failed prune is logged and does not
// fail the export: the archive it was making room for is already safely written.
func (s *Server) runScheduledExport(ctx context.Context, now time.Time) error {
	ctx, span := tel.tracer.Start(ctx, "scheduled_export")
	defer span.End()

	key := s.scheduledExportPrefix() + now.UTC().Format(scheduledExportKeyTime) + ".archive.gz"

	result, err := s.exportArchive(ctx, key, true, nil)
	if err != nil {
		span.RecordError(err)

		return fmt.Errorf("exporting to %s: %w", key, err)
	}

	tel.scheduledExportLastSuccess.Record(ctx, now.Unix())

	span.AddEvent("scheduled export written", trace.WithAttributes(
		attribute.Int("events", result.events),
		attribute.Int("memories", result.memories),
	))

	log.WithFields(log.Fields{
		"object_key": key,
		"events":     result.events,
		"memories":   result.memories,
	}).
		Info("scheduled export written")

	if err := s.pruneScheduledExports(ctx); err != nil {
		log.WithError(err).Warn("scheduled export: pruning old archives failed; they will be retried after the next export")
	}

	return nil
}

// pruneScheduledExports deletes the oldest scheduled archives beyond keep. A non-positive keep keeps
// everything, which is a choice an operator with their own lifecycle rules on the bucket may make.
func (s *Server) pruneScheduledExports(ctx context.Context) error {
	if s.scheduledExport.Keep <= 0 {
		return nil
	}

	pruner, ok := s.objects.(archive.Pruner)
	if !ok {
		return nil
	}

	keys, err := pruner.List(ctx, s.scheduledExportPrefix())
	if err != nil {
		return err
	}

	// Only what this schedule wrote: a key under the prefix it cannot parse is left alone.
	var ours []string

	for _, key := range keys {
		if _, ok := scheduledExportTime(key, s.scheduledExportPrefix()); ok {
			ours = append(ours, key)
		}
	}

	for i := 0; i < len(ours)-s.scheduledExport.Keep; i++ {
		if err := pruner.Delete(ctx, ours[i]); err != nil {
			return err
		}

		log.WithField("object_key", ours[i]).Debug("pruned an old scheduled export")
	}

	return nil
}
