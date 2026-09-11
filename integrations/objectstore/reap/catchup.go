package reap

import (
	"context"
	"fmt"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/fastbean-au/hippocampus/contract"
	"github.com/fastbean-au/hippocampus/notify"
)

// ForgottenReader walks the forgotten log, newest first, back to since. It is the pull path's only
// dependency on the service, declared here so a test drives it with a slice.
type ForgottenReader interface {
	Forgotten(
		ctx context.Context,
		since time.Time,
		fn func([]*contract.ForgottenMemory) error,
	) (bool, error)
}

// CatchUp is the pull path: the forgotten log read back over a window, so an agent that was down
// while a cycle ran still carries out what that cycle decided.
//
// It exists because the push path's durability stops at the queue's caps. A receiver that is
// unreachable for longer than callbacks.maxAgeHours loses those deliveries permanently under the
// default backlog policy - and under `retain` or `stall` it does not, but the queue is then holding
// them for a process that has to come back before they are trimmed. Either way the log is the
// record that outlives the queue, and paging it back is cheap.
//
// It is deliberately bounded by a WINDOW rather than by a cursor. A cursor would be state - a file
// to place, to back up, and to be wrong after a restore - and the whole design of this integration
// is that it holds none. A window is idempotent instead: re-reaping an object that is already gone
// is a no-op, so running the same window twice costs nothing but the listing.
type CatchUp struct {
	reaper   *Reaper
	memories ForgottenReader
	window   time.Duration
}

// NewCatchUp builds the pull path. A non-positive window disables it, which is what a caller that
// did not ask for one passes.
func NewCatchUp(reaper *Reaper, memories ForgottenReader, window time.Duration) (*CatchUp, error) {
	if reaper == nil {
		return nil, fmt.Errorf("a reaper is required")
	}

	if memories == nil {
		return nil, fmt.Errorf("a forgotten-log reader is required")
	}

	return &CatchUp{
		reaper:   reaper,
		memories: memories,
		window:   window,
	}, nil
}

// Run reaps everything the log says was forgotten inside the window.
func (c *CatchUp) Run(ctx context.Context) (Result, error) {
	log.Trace("func() reap.CatchUp.Run")

	total := Result{}

	if c.window <= 0 {
		return total, nil
	}

	since := time.Now().Add(-c.window)

	enabled, err := c.memories.Forgotten(ctx, since, func(page []*contract.ForgottenMemory) error {
		result, err := c.reaper.Reap(ctx, PathCatchUp, c.actionable(page))
		total.Add(result)

		return err
	})

	// The enabled flag is read before the error, because it explains an empty pass rather than a
	// failed one: an agent catching up against a store with no forgotten log will keep reporting
	// that it found nothing to do, which looks exactly like being up to date.
	if !enabled {
		log.Warn("the store is not recording a forgotten log, so the catch-up path can recover nothing " +
			"(set consolidation.tombstones.enabled on the service)")
	}

	if err != nil {
		return total, fmt.Errorf("catching up on the forgotten log: %w", err)
	}

	if total.Acted() > 0 || total.Failed > 0 {
		log.WithFields(log.Fields{
			"window":   c.window,
			"deleted":  total.Deleted,
			"shadowed": total.Shadowed,
			"failed":   total.Failed,
		}).
			Info("caught up on memories forgotten while this agent was not listening")
	}

	return total, nil
}

// actionable turns one page of the log into the ids this agent acts on, applying the same cause
// filter the push path applies. The log records a RULE rather than a cause, and the two decay
// passes are exactly the two rules, so the mapping is total.
func (c *CatchUp) actionable(page []*contract.ForgottenMemory) []string {
	ids := make([]string, 0, len(page))

	for _, v := range page {
		if !c.reaper.causes.Acts(cause(v.GetRule())) {
			continue
		}

		ids = append(ids, v.GetId())
	}

	return ids
}

func cause(rule contract.ForgetRule) notify.Cause {
	switch rule {

	case contract.ForgetRule_FORGET_RULE_CONSOLIDATION:
		return notify.CauseConsolidation

	case contract.ForgetRule_FORGET_RULE_EVICTION:
		return notify.CauseEviction

	}

	return ""
}
