package promoter

import (
	"context"

	"github.com/fastbean-au/hippocampus/observability"
	log "github.com/sirupsen/logrus"
	"go.opentelemetry.io/otel/attribute"

	"github.com/fastbean-au/hippocampus/contract"
)

// handleOrphans deals with memories carrying no event id.
//
// Rules key on events, so an orphan is never judged - and a staging buffer that silently accumulates
// records nothing will ever look at is the one outcome worth ruling out. Every policy therefore
// REPORTS the count, including the default one that acts on nothing: an operator watching an
// ingestor should be able to see that its writers are not associating memories with events, which is
// almost always the actual bug.
func (p *Promoter) handleOrphans(ctx context.Context, stats *Stats) {
	policy := p.cfg.Orphans
	if policy == "" {
		policy = OrphanIgnore
	}

	// Orphans older than OrphanAge only: a memory written moments ago may simply be waiting for the
	// writer to create its event. Zero leaves no age bound, which is why the flag defaults to a
	// generous window rather than to nothing.
	cutoff := int64(0)
	if p.cfg.OrphanAge > 0 {
		cutoff = p.now().Add(-p.cfg.OrphanAge).UnixNano()
	}

	memories, total, err := p.readOrphans(ctx, cutoff)
	if err != nil {
		log.Errorf("listing memories with no event: %s", err.Error())

		stats.Errors++

		return
	}

	stats.OrphansSeen += total

	// Reported under every policy, including the one that acts on nothing: this is the count that
	// tells an operator their writers are not associating memories with events, and it is a gauge
	// of a backlog rather than a rate, so it is recorded even when it is zero - a series that
	// disappears when the problem clears is harder to alert on than one that goes to zero.
	tel.orphans.Record(ctx, int64(total))

	if total == 0 {
		return
	}

	if policy == OrphanIgnore {
		// Warn rather than debug: this is the count that tells an operator their writers are not
		// associating memories with events. The source's own decay is what eventually reaps them.
		log.Warnf(
			"%d memory/memories on the source carry no event and so are never judged (--orphans=%s); they will be forgotten by the source's own decay",
			total,
			OrphanIgnore,
		)

		return
	}

	if p.cfg.DryRun {
		p.countOrphans(policy, len(memories), stats)

		return
	}

	if policy == OrphanPromote {
		sent, err := p.sendMemories(ctx, memories)
		if err != nil {
			log.Errorf("promoting %d orphan memories (%d sent before the failure): %s", len(memories), sent, err.Error())

			stats.Errors++

			return
		}

		stats.OrphansPromoted += sent
		stats.MemoriesPromoted += sent

		tel.orphansHandled.Add(ctx, int64(sent), observability.WithGroup(attribute.String(attrOutcome, outcomePromoted)))
		tel.memories.Add(ctx, int64(sent), observability.WithGroup(attribute.String(attrKind, "orphan")))
	}

	// Both acting policies end the same way: the records leave the source. Under OrphanPromote this
	// runs only once the target has accepted every batch, so the ordering guarantee is the same one
	// promote() relies on.
	deleted, err := p.deleteMemories(ctx, memories)
	if err != nil {
		log.Errorf("deleting orphan memories from the source: %s", err.Error())

		stats.Errors++

		return
	}

	if policy == OrphanDrop {
		stats.OrphansDropped += deleted

		tel.orphansHandled.Add(ctx, int64(deleted), observability.WithGroup(attribute.String(attrOutcome, outcomeDropped)))
	}
}

// countOrphans records what a dry run would have done with them.
func (p *Promoter) countOrphans(policy OrphanPolicy, count int, stats *Stats) {
	switch policy {

	case OrphanPromote:
		stats.OrphansPromoted += count
		stats.MemoriesPromoted += count

	case OrphanDrop:
		stats.OrphansDropped += count

	}
}

// readOrphans pages the event-less memories older than the cutoff. has_event FALSE is what asks the
// question: an event-less memory stores an empty event_id, which is also the event_id filter's "no
// bound" value, so that filter could not ask it.
//
// One pass handles at most maxEventMemories of them, so a source holding a large backlog of orphans
// is worked through over several passes rather than in one unbounded read.
func (p *Promoter) readOrphans(ctx context.Context, cutoff int64) ([]*contract.Memory, int, error) {
	var out []*contract.Memory

	limit := p.cfg.maxEventMemories()
	offset := int32(0)
	total := 0

	for len(out) < limit {
		callCtx, cancel := p.callContext(ctx)

		res, err := p.source.GetMemories(callCtx, &contract.GetMemoriesRequest{
			HasEvent:     contract.Bool_FALSE,
			TimestampMax: cutoff,
			OrderBy:      "timestamp",
			Limit:        p.cfg.pageSize(),
			Offset:       offset,
			Links:        true,
		})

		cancel()

		if err != nil {
			return nil, 0, err
		}

		total = int(res.GetTotalCount())

		page := res.GetMemories()
		if len(page) == 0 {
			break
		}

		out = append(out, page...)
		offset += int32(len(page))

		if len(out) >= total {
			break
		}
	}

	return out, total, nil
}

// deleteMemories removes the given memories from the source by id, in pages, returning how many were
// requested before any failure. Deleting by the exact ids that were read is what keeps a memory
// written since the read from being caught up in it.
func (p *Promoter) deleteMemories(ctx context.Context, memories []*contract.Memory) (int, error) {
	deleted := 0
	size := int(p.cfg.pageSize())

	for start := 0; start < len(memories); start += size {
		end := min(start+size, len(memories))

		ids := make([]string, 0, end-start)
		for _, memory := range memories[start:end] {
			ids = append(ids, memory.GetId())
		}

		callCtx, cancel := p.callContext(ctx)

		_, err := p.source.DeleteMemories(callCtx, &contract.DeleteMemoriesRequest{Ids: ids})

		cancel()

		if err != nil {
			return deleted, err
		}

		deleted += len(ids)
	}

	return deleted, nil
}
