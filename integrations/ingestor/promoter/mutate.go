package promoter

import (
	"context"
	"fmt"

	log "github.com/sirupsen/logrus"

	"github.com/fastbean-au/hippocampus/contract"

	"github.com/fastbean-au/hippocampus/integrations/ingestor/rules"
)

// judgement is one event, everything read about it, and what the ruleset decided. It exists so the
// promote path does not have to rebuild the facts (which copy every memory body) in order to
// evaluate a mutation against the same view the rule matched on.
type judgement struct {
	event    *contract.Event
	memories []*contract.Memory
	facts    rules.Facts
	decision rules.Decision
}

// applyEventSet writes the matched rule's event-scoped overrides onto the event.
//
// The event is mutated IN PLACE rather than cloned: it is this pass's own copy, read fresh from the
// source and used afterwards only for its id, so there is nothing for a stale field to corrupt. A
// failure leaves the event on the source, where the next pass re-reads it unmodified.
func (p *Promoter) applyEventSet(ctx context.Context, judged *judgement) error {
	mutation := judged.decision.Mutation
	if !mutation.HasEvent() {
		return nil
	}

	overrides, err := mutation.EventOverrides(ctx, judged.facts)
	if err != nil {
		return fmt.Errorf("setting fields on event %q: %w", judged.event.GetId(), err)
	}

	if overrides.Significance != nil {
		judged.event.Significance = *overrides.Significance
	}

	if overrides.Group != nil {
		judged.event.Group = *overrides.Group
	}

	if overrides.Name != nil {
		judged.event.Name = *overrides.Name
	}

	if overrides.Description != nil {
		judged.event.Description = *overrides.Description
	}

	if overrides.Metadata != nil {
		judged.event.Metadata = overrides.Metadata
	}

	log.WithFields(log.Fields{
		"event_id":     judged.event.GetId(),
		"rule":         decisionRule(judged.decision),
		"significance": judged.event.GetSignificance(),
		"group":        judged.event.GetGroup(),
	}).
		Debug("event fields set for promotion")

	return nil
}

// applyMemorySet writes the matched rule's memory-scoped overrides onto each memory that is about
// to cross, in place and for the same reason applyEventSet does.
//
// Each memory's expressions are evaluated against that memory as it stands, never against one
// already rewritten by this loop, so the result does not depend on the order the slice happens to
// be in.
func (p *Promoter) applyMemorySet(ctx context.Context, judged *judgement, memories []*contract.Memory) error {
	mutation := judged.decision.Mutation
	if !mutation.HasMemory() {
		return nil
	}

	for _, memory := range memories {
		overrides, err := mutation.MemoryOverrides(ctx, judged.facts, memoryFact(memory))
		if err != nil {
			return fmt.Errorf("setting fields on memory %q of event %q: %w", memory.GetId(), judged.event.GetId(), err)
		}

		if overrides.Significance != nil {
			memory.Significance = *overrides.Significance
		}

		if overrides.Group != nil {
			memory.Group = *overrides.Group
		}

		if overrides.Metadata != nil {
			memory.Metadata = overrides.Metadata
		}
	}

	return nil
}
