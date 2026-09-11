package reap

import (
	"context"
	"fmt"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/fastbean-au/hippocampus/observability"

	"github.com/fastbean-au/hippocampus/integrations/objectstore/keymap"
	"github.com/fastbean-au/hippocampus/integrations/objectstore/objects"
)

// HeldReader reports which of the given memory ids the store still holds.
type HeldReader interface {
	Held(ctx context.Context, ids []string) (map[string]bool, error)
}

const (
	// defaultSweepMinAge is the grace period an object gets before the sweep will consider it an
	// orphan. It is not optional and cannot be set to zero - see SweepConfig.MinAge.
	defaultSweepMinAge = 24 * time.Hour

	// defaultSweepBatch is how many objects are judged per round trip. It matches the service's own
	// cap on one ExplainConsolidation call, so a batch is exactly one RPC.
	defaultSweepBatch = 200
)

// SweepConfig configures the reverse sweep.
type SweepConfig struct {
	// Store is the bucket being swept. Required.
	Store objects.Store

	// Memories answers which ids the store still holds. Required.
	Memories HeldReader

	// Reaper carries out the deletions. Required.
	Reaper *Reaper

	// MinAge is how long an object must have existed before the sweep will judge it. Non-positive
	// selects defaultSweepMinAge; it cannot be disabled.
	MinAge time.Duration

	// Prefix narrows the sweep to one part of the bucket. Empty sweeps all of it.
	Prefix string

	// BatchSize is how many objects are judged per round trip. Non-positive selects
	// defaultSweepBatch; anything larger is clamped to it, since it is the service's own cap.
	BatchSize int
}

// Sweep is the backstop: the bucket enumerated, each object's memory asked after, and anything the
// store no longer holds deleted.
//
// # Why it is needed even with two delivery paths
//
// The push path can be dropped by the queue's caps and the pull path by the forgotten log being off
// or trimmed. Both failures are silent and permanent, and what they leave behind is an orphaned
// payload that nothing will ever mention again. This is the only mechanism that can find one, and
// it is the same reverse pass the OpenSearch integration needed for exactly the same reason.
//
// # What it assumes, which is the dangerous part
//
// It assumes the bucket (or the prefix) is managed solely by this controller. An object with no
// pointer-memory is, under that assumption, an orphan - but it is indistinguishable from an object
// somebody else put there, and from one whose pointer-memory has not been written yet. Three things
// hold that assumption to something safe:
//
//   - MinAge, which cannot be disabled, so an object is never judged before its producer has had
//     time to write the memory for it. A sweep with no grace period would race every write.
//   - Shadow mode, which is the default everywhere in this package, so the first thing a sweep does
//     in a new deployment is report what it would have deleted.
//   - The unmappable and foreign guards in Reaper, plus Prefix, which is how a shared bucket is
//     narrowed to the part this controller owns.
//
// And one thing it refuses to do: if the store cannot be asked which ids it holds, the sweep STOPS.
// Treating "cannot ask" as "not held" would delete the whole bucket on an outage.
type Sweep struct {
	store     objects.Store
	memories  HeldReader
	reaper    *Reaper
	minAge    time.Duration
	prefix    string
	batchSize int
}

// NewSweep validates cfg and builds the sweep.
func NewSweep(cfg SweepConfig) (*Sweep, error) {
	switch {

	case cfg.Store == nil:
		return nil, fmt.Errorf("a bucket is required")

	case cfg.Memories == nil:
		return nil, fmt.Errorf("a store reader is required")

	case cfg.Reaper == nil:
		return nil, fmt.Errorf("a reaper is required")

	}

	minAge := cfg.MinAge
	if minAge <= 0 {
		minAge = defaultSweepMinAge
	}

	batch := cfg.BatchSize
	if batch <= 0 || batch > defaultSweepBatch {
		batch = defaultSweepBatch
	}

	return &Sweep{
		store:     cfg.Store,
		memories:  cfg.Memories,
		reaper:    cfg.Reaper,
		minAge:    minAge,
		prefix:    cfg.Prefix,
		batchSize: batch,
	}, nil
}

// SweepResult is what one pass did.
type SweepResult struct {
	Examined int
	Skipped  int
	Result   Result
}

// Run makes one pass over the bucket.
func (s *Sweep) Run(ctx context.Context) (SweepResult, error) {
	log.Trace("func() reap.Sweep.Run")

	started := time.Now()
	summary := SweepResult{}
	cutoff := started.Add(-s.minAge)

	batch := make([]string, 0, s.batchSize)

	// judge is called with a full batch and at the end of the walk, so the tail is never left
	// unjudged.
	judge := func() error {
		if len(batch) == 0 {
			return nil
		}

		result, err := s.judge(ctx, batch)
		summary.Result.Add(result)

		batch = batch[:0]

		return err
	}

	err := s.store.List(ctx, s.prefix, func(object objects.Object) error {
		summary.Examined++

		tel.examined.Add(ctx, 1, observability.WithGroup())

		id, err := keymap.MemoryId(s.store.Bucket(), object.Key)
		if err != nil {
			// Unmappable objects are skipped rather than counted as orphans: this agent could never
			// have minted a memory for one, so their having none says nothing at all.
			summary.Skipped++

			return nil
		}

		// The grace period. An object newer than the cutoff is never even asked about, so the sweep
		// cannot race a producer that writes the object before the memory.
		if object.Modified.After(cutoff) {
			summary.Skipped++

			return nil
		}

		batch = append(batch, id)

		if len(batch) < s.batchSize {
			return nil
		}

		return judge()
	})
	if err != nil {
		return summary, fmt.Errorf("sweeping the bucket: %w", err)
	}

	if err := judge(); err != nil {
		return summary, err
	}

	tel.sweep.Record(ctx, time.Since(started).Seconds(), observability.WithGroup())

	log.WithFields(log.Fields{
		"examined": summary.Examined,
		"skipped":  summary.Skipped,
		"deleted":  summary.Result.Deleted,
		"shadowed": summary.Result.Shadowed,
		"failed":   summary.Result.Failed,
		"duration": time.Since(started).Truncate(time.Millisecond),
	}).
		Info("swept the bucket for objects the store no longer remembers")

	return summary, nil
}

// judge asks the store which of a batch's ids it still holds, and reaps the rest.
func (s *Sweep) judge(ctx context.Context, ids []string) (Result, error) {
	held, err := s.memories.Held(ctx, ids)
	if err != nil {
		// Deliberately fatal to the pass. An error here means the store could not be asked, and the
		// only other reading available - "it does not hold them" - would delete every object in the
		// bucket the first time the service was unreachable.
		return Result{}, fmt.Errorf("asking the store which memories it holds: %w", err)
	}

	orphans := make([]string, 0, len(ids))

	for _, id := range ids {
		if held[id] {
			continue
		}

		orphans = append(orphans, id)
	}

	return s.reaper.Reap(ctx, PathSweep, orphans)
}
