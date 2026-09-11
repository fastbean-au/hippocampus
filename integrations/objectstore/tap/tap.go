// Package tap turns "somebody read this object" into "reinforce the memory that points at it".
//
// It is the half of the retention-controller deployment that a store holding the payload gets for
// free. When the payload lives elsewhere, reads happen elsewhere too, so recall reinforcement - the
// one input no expiry policy has - goes dark unless something feeds it. This is that something.
//
// # Why it can hold no state
//
// RecallMemories is an UPDATE ... WHERE id IN (...) that matches nothing when an id is absent. So
// the tap never asks whether a memory exists, keeps no table of what it has written, and survives a
// restart with nothing to rebuild: it fires a speculative recall for every object read, and the
// misses cost one row of a WHERE clause. That is the same design the Bluesky bridge uses for likes,
// and it is what makes attaching a tap cheap enough to be worth doing at all.
//
// # The trade in batching
//
// A read counts as tapped once its id is BUFFERED, so a crash inside the window loses at most one
// window of reinforcement. That is deliberate and is the right trade here: a lost read decays a
// memory slightly sooner, it does not make it wrong. A batch size of 0 turns every read into its
// own synchronous call for anyone who disagrees.
//
// # The one silent failure
//
// The ids recalled here are derived (see the keymap package) and must match the ids the producer
// stored its pointer-memories under. If they do not, every recall misses, and a miss is by design
// indistinguishable from a memory that has already been forgotten. Nothing fails, nothing errors,
// and reinforcement simply never happens. The hit rate is therefore published as a metric and a
// sustained zero is logged at Warn naming that cause - which is the only way this fault is ever
// going to be noticed.
package tap

import (
	"context"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
	"go.opentelemetry.io/otel/attribute"

	"github.com/fastbean-au/hippocampus/observability"
)

// Recaller reinforces memories by id, reporting how many of them the store held. It is the tap's
// only dependency on the service, declared here rather than imported so a test drives it with a
// counter and a slice.
type Recaller interface {
	Recall(ctx context.Context, ids []string) (int, error)
}

const (
	// shutdownFlushTimeout bounds the last flush after the context is cancelled.
	shutdownFlushTimeout = 5 * time.Second

	// reportInterval and reportMinimumIds gate the zero-hit warning: it is logged at most this
	// often, and only once enough ids have been submitted for a run of misses to mean something. A
	// handful of misses on a quiet gateway is ordinary; a hundred with no hit at all is a
	// configuration fault.
	reportInterval   = 5 * time.Minute
	reportMinimumIds = 100
)

// Config configures a Tap.
type Config struct {
	// Recaller is the service side. Required.
	Recaller Recaller

	// BatchSize is how many distinct ids accumulate before a recall is issued. Zero recalls each id
	// synchronously as it arrives, which costs one RPC per read.
	BatchSize int

	// Window is how long a partial batch waits before being flushed by Run. It has no effect when
	// BatchSize is zero.
	Window time.Duration
}

// Tap coalesces object reads into bulk RecallMemories calls.
type Tap struct {
	recaller  Recaller
	batchSize int
	window    time.Duration

	mu   sync.Mutex
	ids  []string
	seen map[string]struct{}

	// The hit-rate accounting behind the zero-hit warning, under its own mutex so a recall in
	// flight does not hold up the buffer.
	reportMu     sync.Mutex
	reportedAt   time.Time
	sinceIds     int
	sinceHits    int
	everReported bool
}

// New builds a Tap.
func New(cfg Config) *Tap {
	return &Tap{
		recaller:   cfg.Recaller,
		batchSize:  cfg.BatchSize,
		window:     cfg.Window,
		seen:       make(map[string]struct{}),
		reportedAt: time.Now(),
	}
}

// Touch records that the memory with this id was read, reinforcing it now or shortly.
//
// Ids are deduplicated within a window. The service deduplicates too, but doing it here shrinks the
// request and makes the reinforced count a number of DISTINCT memories, which is what somebody
// reading the hit rate actually wants when one object is being fetched forty times a second.
func (t *Tap) Touch(ctx context.Context, id string) error {
	if id == "" {
		return nil
	}

	if t.batchSize <= 0 {
		return t.recall(ctx, []string{id})
	}

	t.mu.Lock()

	if _, ok := t.seen[id]; ok {
		t.mu.Unlock()

		return nil
	}

	t.seen[id] = struct{}{}
	t.ids = append(t.ids, id)

	if len(t.ids) < t.batchSize {
		t.mu.Unlock()

		return nil
	}

	batch := t.take()

	t.mu.Unlock()

	return t.recall(ctx, batch)
}

// Flush recalls whatever is buffered. Safe to call on an empty buffer, which is the common case for
// the ticker.
func (t *Tap) Flush(ctx context.Context) error {
	t.mu.Lock()
	batch := t.take()
	t.mu.Unlock()

	return t.recall(ctx, batch)
}

// Run flushes on the window until ctx is cancelled, then flushes once more so a clean shutdown does
// not drop a partial batch. Touch covers the full-batch case inline, so a busy gateway rarely waits
// for the ticker at all.
func (t *Tap) Run(ctx context.Context) {
	if t.batchSize <= 0 || t.window <= 0 {
		return
	}

	ticker := time.NewTicker(t.window)
	defer ticker.Stop()

	for {
		select {

		case <-ctx.Done():
			// The parent context is already cancelled, so the final flush needs its own bounded one
			// or it would be refused before it was sent.
			flushCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownFlushTimeout)
			defer cancel()

			if err := t.Flush(flushCtx); err != nil {
				log.WithError(err).Debug("the final reinforcement flush failed during shutdown")
			}

			return

		case <-ticker.C:
			if err := t.Flush(ctx); err != nil {
				log.WithError(err).Warn("flushing buffered reinforcement failed; those reads are lost")
			}

		}
	}
}

// take empties the buffer and returns what was in it. The caller must hold the mutex.
func (t *Tap) take() []string {
	if len(t.ids) == 0 {
		return nil
	}

	batch := t.ids

	t.ids = nil
	t.seen = make(map[string]struct{})

	return batch
}

// recall issues one call and records what it did.
func (t *Tap) recall(ctx context.Context, batch []string) error {
	if len(batch) == 0 {
		return nil
	}

	tel.batchSize.Record(ctx, int64(len(batch)), observability.WithGroup())

	hits, err := t.recaller.Recall(ctx, batch)
	if err != nil {
		record(ctx, OutcomeFailed, len(batch))

		return err
	}

	record(ctx, OutcomeReinforced, hits)
	record(ctx, OutcomeMissing, len(batch)-hits)

	t.report(len(batch), hits)

	log.WithFields(log.Fields{
		"ids":  len(batch),
		"hits": hits,
	}).
		Debug("reinforced memories for objects that were read")

	return nil
}

// report accumulates the hit rate and warns when a meaningful number of reads have reinforced
// nothing at all.
//
// This is the only detector for the fault described in the package doc, and it is worth being
// precise about what it can and cannot say. A store that has genuinely forgotten everything the
// gateway is serving produces the same reading, which is why the message names both possibilities
// rather than asserting the misconfiguration. What it rules out is silence: before this, a gateway
// reinforcing nothing looked exactly like a gateway doing its job.
func (t *Tap) report(ids int, hits int) {
	t.reportMu.Lock()
	defer t.reportMu.Unlock()

	t.sinceIds += ids
	t.sinceHits += hits

	if time.Since(t.reportedAt) < reportInterval {
		return
	}

	// Reset before the threshold test, so a quiet period does not accumulate into a warning about
	// an interval nobody was reading during.
	sinceIds, sinceHits := t.sinceIds, t.sinceHits

	t.reportedAt = time.Now()
	t.sinceIds = 0
	t.sinceHits = 0

	switch {

	case sinceIds < reportMinimumIds:
		return

	case sinceHits > 0:
		// A working tap says so once, so an operator who has just wired one up gets a positive
		// confirmation rather than only ever hearing about failure.
		if !t.everReported {
			t.everReported = true

			log.WithFields(log.Fields{
				"ids":  sinceIds,
				"hits": sinceHits,
			}).
				Info("reinforcement is reaching the store")
		}

		return

	}

	t.everReported = true

	log.WithField("ids", sinceIds).
		Warn("every read reinforced nothing: either the store has forgotten all of these objects, " +
			"or the pointer-memories were not stored under the ids this agent derives (bucket/key)")
}

// record reports n ids as having had one outcome. A zero count is skipped so a fully-hitting batch
// does not publish a "missing" series of zeroes.
func record(ctx context.Context, outcome string, n int) {
	if n <= 0 {
		return
	}

	tel.recalls.Add(ctx, int64(n), observability.WithGroup(attribute.String(attrOutcome, outcome)))
}
