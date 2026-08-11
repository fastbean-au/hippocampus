package promoter

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/fastbean-au/hippocampus/contract"
)

// TestPassSetsFieldsOnThePromotedCopy is the whole capability end to end: a rule rewrites fields on
// the way across, the TARGET holds the new values, and the source is drained as usual.
func TestPassSetsFieldsOnThePromotedCopy(t *testing.T) {
	source := newFakeStore()
	target := newFakeStore()

	source.putEvent(endedEvent("e1", "checkout failed", map[string]string{"severity": "error"}))
	source.putMemory(memory("m1", "e1", "timeout talking to the gateway", 3))
	source.putMemory(memory("m2", "e1", "panic: nil map", 4))

	p := newPromoter(t, source, target, `{
		"defaultAction": "drop",
		"rules": [{
			"name": "errors",
			"expr": "event.metadata[?'severity'].orValue('') == 'error'",
			"action": "promote",
			"set": {
				"event": {
					"significance": "math.least(100, event.significance * 10)",
					"group": "'incidents'",
					"metadata": "{'promoted_by': 'edge-a'}"
				},
				"memory": {
					"significance": "memory.body.contains('panic') ? 90 : memory.significance",
					"group": "'incidents'"
				}
			}
		}]
	}`, Config{})

	stats, err := p.Pass(context.Background())
	if err != nil {
		t.Fatalf("Pass: %s", err)
	}

	if stats.EventsPromoted != 1 || stats.MemoriesPromoted != 2 {
		t.Fatalf("unexpected stats: %+v", stats)
	}

	promoted := target.events["e1"]

	if promoted.GetSignificance() != 50 {
		t.Errorf("expected the event's significance to be set to 50, got %d", promoted.GetSignificance())
	}

	if promoted.GetGroup() != "incidents" {
		t.Errorf("expected the event's group to be set, got %q", promoted.GetGroup())
	}

	// Stamped over what the event already carried, not substituted for it.
	want := map[string]string{"severity": "error", "promoted_by": "edge-a"}

	if !reflect.DeepEqual(promoted.GetMetadata(), want) {
		t.Errorf("expected metadata %v, got %v", want, promoted.GetMetadata())
	}

	if got := target.memories["m2"].GetSignificance(); got != 90 {
		t.Errorf("expected the matching memory to be re-scored to 90, got %d", got)
	}

	// The other memory's expression returned its own significance, so it crosses unchanged.
	if got := target.memories["m1"].GetSignificance(); got != 3 {
		t.Errorf("expected the other memory to keep significance 3, got %d", got)
	}

	if got := target.memories["m1"].GetGroup(); got != "incidents" {
		t.Errorf("expected every memory's group to be set, got %q", got)
	}

	if got := source.eventIds(); len(got) != 0 {
		t.Errorf("expected the source to be drained, got %v", got)
	}
}

// TestKeepTopNRanksByTheScoreTheRuleSet is the ordering decision made visible. The memory the edge
// stored as LEAST significant is the one the rule scores highest, so a reduction applied before the
// mutation would promote the wrong one - which is exactly the surprise this ordering avoids.
func TestKeepTopNRanksByTheScoreTheRuleSet(t *testing.T) {
	source := newFakeStore()
	target := newFakeStore()

	source.putEvent(endedEvent("e1", "mixed", nil))
	source.putMemory(memory("m1", "e1", "panic: nil map dereference", 1))
	source.putMemory(memory("m2", "e1", "routine chatter", 50))
	source.putMemory(memory("m3", "e1", "more chatter", 40))

	p := newPromoter(t, source, target, `{
		"defaultAction": "drop",
		"rules": [{
			"name": "score-then-keep",
			"expr": "true",
			"action": "promote",
			"reduce": {"keepTopN": 1},
			"set": {"memory": {"significance": "memory.body.contains('panic') ? 100 : 5"}}
		}]
	}`, Config{})

	stats, err := p.Pass(context.Background())
	if err != nil {
		t.Fatalf("Pass: %s", err)
	}

	if stats.MemoriesPromoted != 1 {
		t.Fatalf("expected one memory to cross, got %+v", stats)
	}

	if got := target.memoryIds(); !reflect.DeepEqual(got, []string{"m1"}) {
		t.Errorf("expected the rule's own top-scoring memory, got %v", got)
	}

	// The memories that did not cross are still drained: a reduction says what is kept centrally,
	// not what survives on the edge.
	if got := source.memoryIds(); len(got) != 0 {
		t.Errorf("expected the source to be drained, got %v", got)
	}
}

// TestMinSignificanceUsesTheSetScore is the same ordering seen through the other selection: the
// threshold is applied to what the rule declared, not to what the edge happened to store.
func TestMinSignificanceUsesTheSetScore(t *testing.T) {
	source := newFakeStore()
	target := newFakeStore()

	source.putEvent(endedEvent("e1", "mixed", nil))
	source.putMemory(memory("m1", "e1", "keep: an error", 1))
	source.putMemory(memory("m2", "e1", "drop this one", 99))

	p := newPromoter(t, source, target, `{
		"defaultAction": "drop",
		"rules": [{
			"name": "rescore",
			"expr": "true",
			"action": "promote",
			"reduce": {"minSignificance": 50},
			"set": {"memory": {"significance": "memory.body.startsWith('keep') ? 80 : 2"}}
		}]
	}`, Config{})

	if _, err := p.Pass(context.Background()); err != nil {
		t.Fatalf("Pass: %s", err)
	}

	if got := target.memoryIds(); !reflect.DeepEqual(got, []string{"m1"}) {
		t.Errorf("expected only the re-scored memory to cross, got %v", got)
	}
}

// TestSummarisedMemoryIsScored pins the one case where the record being written did not exist when
// the rule matched: the summary is what crosses, so the mutation is applied to it.
func TestSummarisedMemoryIsScored(t *testing.T) {
	source := newFakeStore()
	target := newFakeStore()

	source.summariser = func(eventId string, memories []*contract.Memory) *contract.Memory {
		return &contract.Memory{
			Id:           "summary-1",
			EventId:      eventId,
			Body:         "a summary of everything",
			Significance: 1,
			IsSummary:    true,
			TimeStamp:    nanos(testNow.Add(-40 * time.Minute)),
		}
	}

	source.putEvent(endedEvent("e1", "chatty", nil))
	source.putMemory(memory("m1", "e1", "one", 2))
	source.putMemory(memory("m2", "e1", "two", 3))

	p := newPromoter(t, source, target, `{
		"defaultAction": "drop",
		"rules": [{
			"name": "condense",
			"expr": "true",
			"action": "promote",
			"reduce": {"summarise": true},
			"set": {"memory": {"significance": "memory.is_summary ? 70 : 1"}}
		}]
	}`, Config{})

	stats, err := p.Pass(context.Background())
	if err != nil {
		t.Fatalf("Pass: %s", err)
	}

	if stats.MemoriesPromoted != 1 {
		t.Fatalf("expected only the summary to cross, got %+v", stats)
	}

	if got := target.memories["summary-1"].GetSignificance(); got != 70 {
		t.Errorf("expected the summary to be scored, got %d", got)
	}
}

// TestMutationFailureLeavesEverythingWhereItIs is the error doctrine. A mutation that errors is NOT
// treated as "promote at the edge's own significance": the event fails, nothing is written to the
// target, and nothing is drained from the source - so the records still exist and the next pass
// (or a fixed rules file) can still act on them.
func TestMutationFailureLeavesEverythingWhereItIs(t *testing.T) {
	source := newFakeStore()
	target := newFakeStore()

	source.putEvent(endedEvent("e1", "unlabelled", nil))
	source.putMemory(memory("m1", "e1", "body", 3))

	p := newPromoter(t, source, target, `{
		"defaultAction": "drop",
		"rules": [{
			"name": "weighted",
			"expr": "true",
			"action": "promote",
			"set": {"event": {"significance": "int(event.metadata['weight'])"}}
		}]
	}`, Config{})

	stats, err := p.Pass(context.Background())
	if err != nil {
		t.Fatalf("Pass: %s", err)
	}

	if stats.Errors != 1 || stats.EventsPromoted != 0 {
		t.Errorf("expected one failed event and no promotion, got %+v", stats)
	}

	if got := target.eventIds(); len(got) != 0 {
		t.Errorf("expected nothing to reach the target, got %v", got)
	}

	if got := source.eventIds(); !reflect.DeepEqual(got, []string{"e1"}) {
		t.Errorf("expected the event to be left on the source, got %v", got)
	}

	if got := source.memoryIds(); !reflect.DeepEqual(got, []string{"m1"}) {
		t.Errorf("expected the memories to be left on the source, got %v", got)
	}
}

// TestDryRunReportsAFailingMutation pins that a dry run finds a broken expression rather than
// reporting a promotion that would not have happened - which is most of the value of testing a
// rules file before deploying it.
func TestDryRunReportsAFailingMutation(t *testing.T) {
	source := newFakeStore()
	target := newFakeStore()

	source.putEvent(endedEvent("e1", "unlabelled", nil))
	source.putMemory(memory("m1", "e1", "body", 3))

	p := newPromoter(t, source, target, `{
		"defaultAction": "drop",
		"rules": [{
			"name": "weighted",
			"expr": "true",
			"action": "promote",
			"set": {"memory": {"significance": "int(memory.metadata['weight'])"}}
		}]
	}`, Config{DryRun: true})

	stats, err := p.Pass(context.Background())
	if err != nil {
		t.Fatalf("Pass: %s", err)
	}

	if stats.Errors != 1 || stats.EventsPromoted != 0 {
		t.Errorf("expected the dry run to report the failure, got %+v", stats)
	}

	if got := source.memoryIds(); len(got) != 1 {
		t.Errorf("a dry run deleted something: %v", got)
	}
}

// TestDryRunReportsTheScoredCounts is what a dry run is for: the reduction is counted against the
// significance the rule would SET, so testing a scoring rule shows the number it produces rather
// than the one it replaces. Nothing moves.
func TestDryRunReportsTheScoredCounts(t *testing.T) {
	source := newFakeStore()
	target := newFakeStore()

	source.putEvent(endedEvent("e1", "mixed", nil))
	source.putMemory(memory("m1", "e1", "keep: one", 1))
	source.putMemory(memory("m2", "e1", "keep: two", 1))
	source.putMemory(memory("m3", "e1", "chatter", 90))

	p := newPromoter(t, source, target, `{
		"defaultAction": "drop",
		"rules": [{
			"name": "rescore",
			"expr": "true",
			"action": "promote",
			"reduce": {"minSignificance": 50},
			"set": {"memory": {"significance": "memory.body.startsWith('keep') ? 80 : 2"}}
		}]
	}`, Config{DryRun: true})

	stats, err := p.Pass(context.Background())
	if err != nil {
		t.Fatalf("Pass: %s", err)
	}

	// Two would cross under the rule's own scores; the memory the edge ranked at 90 would not.
	if stats.MemoriesPromoted != 2 {
		t.Errorf("expected the dry run to count the scored memories, got %+v", stats)
	}

	if got := target.eventIds(); len(got) != 0 {
		t.Errorf("a dry run moved something: %v", got)
	}

	if got := source.memoryIds(); len(got) != 3 {
		t.Errorf("a dry run deleted something: %v", got)
	}
}
