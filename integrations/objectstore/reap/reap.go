// Package reap is the actuator: the half of the retention-controller deployment that carries out
// what the decay cycle decided, by deleting the object a forgotten memory pointed at.
//
// # Three paths, one action
//
// A forget-instruction reaches this process three ways, and all three end in Reaper.Reap:
//
//   - the PUSH path, a memory_forgotten callback delivered as the cycle deletes (receiver.go);
//   - the PULL path, the forgotten log walked back over a window at startup (catchup.go), which is
//     what recovers the instructions that were dropped while this process was not running;
//   - the SWEEP, the bucket enumerated and each object's memory asked after (sweep.go), which is
//     what recovers anything both of the others lost.
//
// They exist because at-least-once delivery against a process that can be down is still lossy, and
// the loss here is not a missed notification but an orphaned payload - permanent, silent, and
// growing precisely with how well the decay cycle is working. The same lesson cost this project a
// search index that diverged to 4.38M documents against 211,657 rows.
//
// Deleting an object that is already gone is a no-op, which is what makes all three paths safe to
// overlap and safe to replay.
//
// # Shadow mode is the default
//
// A component that deletes data in somebody else's storage on the strength of a decay model has to
// be watched before it is trusted, so Config.Delete defaults to false: everything is selected,
// counted and logged, and nothing is deleted. The metrics a shadow run produces are exactly what a
// deployment needs in order to compare this controller's judgement against whatever flat expiry it
// is replacing, before making it authoritative.
//
// # What it refuses to act on
//
// An id that is not an object reference for THIS bucket is never deleted. That covers a store
// shared with other producers (a UUID id maps to nothing), an agent pointed at the wrong bucket (an
// id naming another one), and a key too long to have been minted here. None of these is an error;
// all of them are counted, because the rate at which they arrive is the signal that an agent is
// mis-wired.
//
// A cause this agent does not act on is likewise ignored rather than obeyed - see Causes.
package reap

import (
	"context"
	"errors"
	"fmt"
	"strings"

	log "github.com/sirupsen/logrus"
	"go.opentelemetry.io/otel/attribute"

	"github.com/fastbean-au/hippocampus/notify"
	"github.com/fastbean-au/hippocampus/observability"

	"github.com/fastbean-au/hippocampus/integrations/objectstore/keymap"
	"github.com/fastbean-au/hippocampus/integrations/objectstore/objects"
)

// DefaultCauses are the deletions this agent acts on unless told otherwise: the two decay passes
// and the cascade that follows them.
//
// The omissions are the interesting part, and each is a way to destroy data that is still wanted.
//
//   - CauseClear is the second half of an Export or Transfer, which is a MOVE: the memory now lives
//     in another instance, that instance is now the controller for these objects, and deleting the
//     payload here would destroy exactly the data the move was made to preserve.
//   - CausePurge is an operator resetting the store. Obeying it would empty the bucket on one
//     administrative command, which is not a decision a store of pointers should be able to make
//     for the system holding the payloads.
//   - CauseSummaryReplace is memories folded into a summary. Whether the summarised records'
//     payloads should go with them is a deployment's judgement, not this agent's default.
//   - CauseClient is an explicit DeleteMemories. It is defensible to act on - a client deleting the
//     pointer plausibly means the payload too - and it is left out of the default because the
//     callback feed only carries it when callbacks.allDeletions is set, and that key is about
//     visibility rather than about consent to delete.
//
// Any of them can be enabled with --causes; nothing here is a refusal, only a default.
var DefaultCauses = []notify.Cause{
	notify.CauseConsolidation,
	notify.CauseEviction,
	notify.CauseCascade,
}

// Causes is the set of deletion causes an agent acts on.
type Causes map[notify.Cause]bool

// NewCauses builds the set from a comma-separated list, validating every entry. An empty list
// selects DefaultCauses.
func NewCauses(list string) (Causes, error) {
	causes := Causes{}

	if strings.TrimSpace(list) == "" {
		for _, v := range DefaultCauses {
			causes[v] = true
		}

		return causes, nil
	}

	for _, v := range strings.Split(list, ",") {
		cause := notify.Cause(strings.TrimSpace(v))

		if !known(cause) {
			return nil, fmt.Errorf("unknown deletion cause %q", v)
		}

		causes[cause] = true
	}

	return causes, nil
}

// Acts reports whether this agent deletes for the given cause. An unset cause - which is what a
// delivery from a service predating the cause field carries, and what the pull path reports for a
// record whose rule is unspecified - is treated as a decay deletion, since the two decay passes are
// the only thing that has ever produced an unattributed one.
func (c Causes) Acts(cause notify.Cause) bool {
	if cause == "" {
		return c[notify.CauseConsolidation]
	}

	return c[cause]
}

func known(cause notify.Cause) bool {
	switch cause {

	case notify.CauseConsolidation,
		notify.CauseEviction,
		notify.CauseCascade,
		notify.CauseClient,
		notify.CauseClear,
		notify.CauseSummaryReplace,
		notify.CausePurge:
		return true

	}

	return false
}

// Config configures a Reaper.
type Config struct {
	// Store is the bucket being managed. Required.
	Store objects.Store

	// Delete arms the agent. False - the default - selects shadow mode, where every deletion is
	// selected, counted and logged, and none is carried out.
	Delete bool

	// Causes are the deletion causes acted on. Empty selects DefaultCauses.
	Causes Causes
}

// Reaper deletes the objects behind forgotten memories.
type Reaper struct {
	store  objects.Store
	delete bool
	causes Causes
}

// New builds a Reaper.
func New(cfg Config) (*Reaper, error) {
	if cfg.Store == nil {
		return nil, fmt.Errorf("a bucket is required")
	}

	causes := cfg.Causes
	if len(causes) == 0 {
		causes = Causes{}

		for _, v := range DefaultCauses {
			causes[v] = true
		}
	}

	return &Reaper{
		store:  cfg.Store,
		delete: cfg.Delete,
		causes: causes,
	}, nil
}

// Armed reports whether the agent actually deletes, for a caller that wants to say so at startup.
func (r *Reaper) Armed() bool {
	return r.delete
}

// Result is what one call to Reap did.
type Result struct {
	Deleted    int
	Shadowed   int
	Foreign    int
	Unmappable int
	Failed     int
}

// Reap acts on the memories named by ids, which have already been decided to be gone.
//
// It returns an error if any object could not be deleted, having tried every one of them first:
// the push path turns that error into a 5xx so the service's queue replays the whole delivery, and
// replaying a delivery whose other objects are already gone costs nothing because the deletes are
// idempotent. Stopping at the first failure would leave the rest of a batch to be recovered by a
// sweep that may not be enabled.
func (r *Reaper) Reap(ctx context.Context, path string, ids []string) (Result, error) {
	log.Trace("func() reap.Reaper.Reap")

	result := Result{}

	var failures []error

	for _, id := range ids {
		key, ok := r.objectFor(ctx, path, id, &result)
		if !ok {
			continue
		}

		if !r.delete {
			result.Shadowed++

			r.record(ctx, path, OutcomeShadow, 1)

			log.WithFields(log.Fields{
				"id":  id,
				"key": key,
			}).
				Info("would delete the object behind a forgotten memory (shadow mode)")

			continue
		}

		if err := r.store.Delete(ctx, key); err != nil {
			result.Failed++

			r.record(ctx, path, OutcomeFailed, 1)

			failures = append(failures, err)

			continue
		}

		result.Deleted++

		r.record(ctx, path, OutcomeDeleted, 1)

		log.WithFields(log.Fields{
			"id":  id,
			"key": key,
		}).
			Debug("deleted the object behind a forgotten memory")
	}

	if len(failures) > 0 {
		return result, fmt.Errorf("deleting %d of %d objects: %w", len(failures), len(ids), errors.Join(failures...))
	}

	return result, nil
}

// objectFor resolves one id to a key in this agent's bucket, counting and reporting the two ways it
// can be none of this agent's business.
func (r *Reaper) objectFor(ctx context.Context, path string, id string, result *Result) (string, bool) {
	bucket, key, err := keymap.Object(id)
	if err != nil {
		result.Unmappable++

		r.record(ctx, path, OutcomeUnmappable, 1)

		log.WithField("id", id).
			Debug("a forgotten memory's id is not an object reference; nothing to delete")

		return "", false
	}

	if bucket != r.store.Bucket() {
		result.Foreign++

		r.record(ctx, path, OutcomeForeign, 1)

		log.WithFields(log.Fields{
			"id":     id,
			"bucket": bucket,
		}).
			Debug("a forgotten memory names another bucket; nothing to delete here")

		return "", false
	}

	return key, true
}

func (r *Reaper) record(ctx context.Context, path string, outcome string, n int) {
	if n <= 0 {
		return
	}

	tel.deletions.Add(ctx, int64(n), observability.WithGroup(
		attribute.String(attrPath, path),
		attribute.String(attrOutcome, outcome),
	))
}

// Add accumulates another result, for a caller summing a sweep's pages.
func (r *Result) Add(other Result) {
	r.Deleted += other.Deleted
	r.Shadowed += other.Shadowed
	r.Foreign += other.Foreign
	r.Unmappable += other.Unmappable
	r.Failed += other.Failed
}

// Acted reports how many objects the result actually selected, deleted or not.
func (r Result) Acted() int {
	return r.Deleted + r.Shadowed
}
