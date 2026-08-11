package promoter

import (
	"github.com/fastbean-au/hippocampus/contract"

	"github.com/fastbean-au/hippocampus/integrations/ingestor/rules"
)

// nanosPerSecond converts the UnixNano timestamps this system stores everywhere into the seconds a
// rules file is written in.
const nanosPerSecond = 1e9

// buildFacts turns one event and its memories into the judgement input.
//
// The aggregates (count, bytes, significance min/max/mean) are computed here rather than left to the
// expression, so the common shape rules need no comprehension. withMemories carries
// Ruleset.NeedsMemories: when no rule reads the list, the per-memory structs - which copy every body
// into a second slice - are simply not built.
func buildFacts(event *contract.Event, memories []*contract.Memory, withMemories bool) rules.Facts {
	facts := rules.Facts{
		Event: rules.Event{
			Id:           event.GetId(),
			Name:         event.GetName(),
			Description:  event.GetDescription(),
			Group:        event.GetGroup(),
			Significance: int64(event.GetSignificance()),
			Metadata:     event.GetMetadata(),
			TimeStart:    event.GetTimeStart(),
			TimeEnd:      event.GetTimeEnd(),
			MemoryCount:  int64(len(memories)),
		},
	}

	// An event with no end (or one ending before it started, which validation refuses but an import
	// could carry) reports a zero duration rather than a negative or nonsensical one.
	if event.GetTimeEnd() > event.GetTimeStart() {
		facts.Event.DurationSeconds = float64(event.GetTimeEnd()-event.GetTimeStart()) / nanosPerSecond
	}

	if facts.Event.Metadata == nil {
		facts.Event.Metadata = map[string]string{}
	}

	if len(memories) > 0 {
		facts.Event.SignificanceMin, facts.Event.SignificanceMax, facts.Event.SignificanceMean, facts.Event.BodyBytes = aggregate(memories)
	}

	if !withMemories {
		return facts
	}

	facts.Memories = make([]rules.Memory, 0, len(memories))

	for _, memory := range memories {
		facts.Memories = append(facts.Memories, memoryFact(memory))
	}

	return facts
}

// memoryFact projects one memory onto what an expression sees of it.
//
// It is separate from the loop above because a memory-scoped mutation binds `memory` to the record
// it is about to write, which is not always one of the judged memories: after a summarise reduction
// it is the summary, which did not exist when the facts were built.
func memoryFact(memory *contract.Memory) rules.Memory {
	metadata := memory.GetMetadata()
	if metadata == nil {
		metadata = map[string]string{}
	}

	return rules.Memory{
		Id:           memory.GetId(),
		Body:         memory.GetBody(),
		Significance: int64(memory.GetSignificance()),
		IsBinary:     memory.GetIsBinary() == contract.Bool_TRUE,
		IsSummary:    memory.GetIsSummary(),
		RecallCount:  int64(memory.GetRecallCount()),
		TimeStamp:    memory.GetTimeStamp(),
		Metadata:     metadata,
	}
}

// aggregate computes the per-event memory statistics in one walk.
func aggregate(memories []*contract.Memory) (int64, int64, float64, int64) {
	min := int64(memories[0].GetSignificance())
	max := min
	total := int64(0)
	bytes := int64(0)

	for _, memory := range memories {
		significance := int64(memory.GetSignificance())

		if significance < min {
			min = significance
		}

		if significance > max {
			max = significance
		}

		total += significance
		bytes += int64(len(memory.GetBody()))
	}

	return min, max, float64(total) / float64(len(memories)), bytes
}
