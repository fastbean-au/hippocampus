package promoter

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/fastbean-au/hippocampus/contract"

	"github.com/fastbean-au/hippocampus/integrations/ingestor/rules"
)

// A fixed clock so the settle window is exact rather than racy.
var testNow = time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)

func nanos(t time.Time) int64 { return t.UnixNano() }

// newPromoter wires a promoter over two fake stores with the given rules document.
func newPromoter(t *testing.T, source *fakeStore, target *fakeStore, doc string, cfg Config) *Promoter {
	t.Helper()

	set, err := rules.Parse([]byte(doc), rules.Options{})
	if err != nil {
		t.Fatalf("parsing the test rules: %s", err)
	}

	p := New(source, target, rules.NewStaticWatcher(set), cfg)
	p.now = func() time.Time { return testNow }

	return p
}

// endedEvent builds an event that ended well inside the settle window.
func endedEvent(id string, name string, metadata map[string]string) *contract.Event {
	return &contract.Event{
		Id:           id,
		Name:         name,
		Significance: 5,
		TimeStart:    nanos(testNow.Add(-time.Hour)),
		TimeEnd:      nanos(testNow.Add(-30 * time.Minute)),
		Metadata:     metadata,
	}
}

func memory(id string, eventId string, body string, significance int32) *contract.Memory {
	return &contract.Memory{
		Id:           id,
		EventId:      eventId,
		Body:         body,
		Significance: significance,
		TimeStamp:    nanos(testNow.Add(-45 * time.Minute)),
	}
}

// TestPassPromotesAndDrains is the whole loop end to end: a matching event is written to the target
// with its memories and then removed from the source, and a non-matching one is dropped from the
// source without anything reaching the target.
func TestPassPromotesAndDrains(t *testing.T) {
	source := newFakeStore()
	target := newFakeStore()

	source.putEvent(endedEvent("e1", "keep", map[string]string{"severity": "error"}))
	source.putEvent(endedEvent("e2", "discard", nil))
	source.putMemory(memory("m1", "e1", "first", 3))
	source.putMemory(memory("m2", "e1", "second", 7))
	source.putMemory(memory("m3", "e2", "noise", 1))

	p := newPromoter(t, source, target, `{
		"defaultAction": "drop",
		"rules": [{"name":"errors","expr":"event.metadata[?'severity'].orValue('') == 'error'","action":"promote"}]
	}`, Config{})

	stats, err := p.Pass(context.Background())
	if err != nil {
		t.Fatalf("Pass: %s", err)
	}

	if stats.EventsJudged != 2 || stats.EventsPromoted != 1 || stats.EventsDropped != 1 {
		t.Errorf("unexpected stats: %+v", stats)
	}

	if stats.MemoriesPromoted != 2 {
		t.Errorf("expected 2 memories promoted, got %d", stats.MemoriesPromoted)
	}

	if got := target.eventIds(); !reflect.DeepEqual(got, []string{"e1"}) {
		t.Errorf("expected only the promoted event on the target, got %v", got)
	}

	if got := target.memoryIds(); !reflect.DeepEqual(got, []string{"m1", "m2"}) {
		t.Errorf("expected the promoted event's memories on the target, got %v", got)
	}

	// The source is the queue: everything judged has left it.
	if got := source.eventIds(); len(got) != 0 {
		t.Errorf("expected the source to be drained of events, got %v", got)
	}

	if got := source.memoryIds(); len(got) != 0 {
		t.Errorf("expected the source to be drained of memories, got %v", got)
	}
}

// TestPassIgnoresOpenAndUnsettledEvents pins the two reasons an event is not judged yet: it has not
// ended, or it ended too recently for the settle window.
func TestPassIgnoresOpenAndUnsettledEvents(t *testing.T) {
	source := newFakeStore()
	target := newFakeStore()

	open := endedEvent("open", "still going", nil)
	open.TimeEnd = 0

	// Ended one second ago, well inside a one-minute settle window.
	fresh := endedEvent("fresh", "just ended", nil)
	fresh.TimeEnd = nanos(testNow.Add(-time.Second))

	source.putEvent(open)
	source.putEvent(fresh)
	source.putEvent(endedEvent("settled", "long done", nil))

	p := newPromoter(t, source, target, `{"defaultAction":"promote","rules":[]}`, Config{Settle: time.Minute})

	stats, err := p.Pass(context.Background())
	if err != nil {
		t.Fatalf("Pass: %s", err)
	}

	if stats.EventsJudged != 1 {
		t.Errorf("expected only the settled event to be judged, got %+v", stats)
	}

	if got := source.eventIds(); !reflect.DeepEqual(got, []string{"fresh", "open"}) {
		t.Errorf("expected the open and unsettled events to remain, got %v", got)
	}
}

// TestPassReducesBeforePromoting covers the two content-blind reductions. The memories NOT promoted
// are still drained - a reduction says what crosses to the central store, not what survives on the
// edge.
func TestPassReducesBeforePromoting(t *testing.T) {
	cases := []struct {
		name   string
		reduce string
		want   []string
	}{
		{"keep the most significant", `{"keepTopN":2}`, []string{"m2", "m4"}},
		{"keep those at or above a significance", `{"minSignificance":5}`, []string{"m2", "m4"}},
		{"both compose", `{"keepTopN":1,"minSignificance":5}`, []string{"m4"}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			source := newFakeStore()
			target := newFakeStore()

			source.putEvent(endedEvent("e1", "busy", nil))
			source.putMemory(memory("m1", "e1", "least", 1))
			source.putMemory(memory("m2", "e1", "middling", 5))
			source.putMemory(memory("m3", "e1", "low", 2))
			source.putMemory(memory("m4", "e1", "most", 9))

			doc := fmt.Sprintf(
				`{"defaultAction":"drop","rules":[{"name":"r","expr":"true","action":"promote","reduce":%s}]}`,
				c.reduce,
			)

			p := newPromoter(t, source, target, doc, Config{})

			if _, err := p.Pass(context.Background()); err != nil {
				t.Fatalf("Pass: %s", err)
			}

			if got := target.memoryIds(); !reflect.DeepEqual(got, c.want) {
				t.Errorf("expected %v on the target, got %v", c.want, got)
			}

			if got := source.memoryIds(); len(got) != 0 {
				t.Errorf("the whole event should be drained regardless of the reduction, got %v", got)
			}
		})
	}
}

// TestPassSummarisesBeforePromoting covers the LLM reduction: the SOURCE replaces the memories with
// one summary, and it is the summary that is promoted.
func TestPassSummarisesBeforePromoting(t *testing.T) {
	source := newFakeStore()
	target := newFakeStore()

	source.summariser = func(eventId string, memories []*contract.Memory) *contract.Memory {
		return &contract.Memory{
			Id:           "summary-" + eventId,
			EventId:      eventId,
			Body:         fmt.Sprintf("a summary of %d memories", len(memories)),
			Significance: 9,
			IsSummary:    true,
			TimeStamp:    nanos(testNow),
		}
	}

	source.putEvent(endedEvent("e1", "long session", nil))
	source.putMemory(memory("m1", "e1", "one", 1))
	source.putMemory(memory("m2", "e1", "two", 2))

	p := newPromoter(t, source, target, `{
		"defaultAction": "drop",
		"rules": [{"name":"r","expr":"event.memory_count > 1","action":"promote","reduce":{"summarise":true}}]
	}`, Config{})

	stats, err := p.Pass(context.Background())
	if err != nil {
		t.Fatalf("Pass: %s", err)
	}

	if got := target.memoryIds(); !reflect.DeepEqual(got, []string{"summary-e1"}) {
		t.Errorf("expected only the summary on the target, got %v", got)
	}

	if stats.MemoriesPromoted != 1 {
		t.Errorf("expected 1 memory promoted, got %d", stats.MemoriesPromoted)
	}

	if got := source.memoryIds(); len(got) != 0 {
		t.Errorf("expected the source drained, got %v", got)
	}
}

// TestPassSummariseFailsLoudly is the behaviour the plan calls for explicitly: a source without a
// summariser must fail the event, not quietly promote everything the rule asked to have condensed.
func TestPassSummariseFailsLoudly(t *testing.T) {
	source := newFakeStore()
	target := newFakeStore()

	// summariser left nil: the fake reports FailedPrecondition, as a service without ollama.enabled
	// does.
	source.putEvent(endedEvent("e1", "long session", nil))
	source.putMemory(memory("m1", "e1", "one", 1))

	p := newPromoter(t, source, target, `{
		"defaultAction": "drop",
		"rules": [{"name":"r","expr":"true","action":"promote","reduce":{"summarise":true}}]
	}`, Config{})

	stats, err := p.Pass(context.Background())
	if err != nil {
		t.Fatalf("Pass: %s", err)
	}

	if stats.Errors != 1 || stats.EventsPromoted != 0 {
		t.Errorf("expected the event to fail rather than promote, got %+v", stats)
	}

	if got := target.memoryIds(); len(got) != 0 {
		t.Errorf("nothing should have reached the target, got %v", got)
	}

	// The event stays on the source, so a fixed configuration promotes it on a later pass.
	if got := source.eventIds(); !reflect.DeepEqual(got, []string{"e1"}) {
		t.Errorf("expected the event to remain on the source, got %v", got)
	}
}

// TestPassLeavesTheSourceIntactWhenPromotionFails is the ordering guarantee: the target is written
// first and the source drained only afterwards, so a failed promotion never loses records.
func TestPassLeavesTheSourceIntactWhenPromotionFails(t *testing.T) {
	source := newFakeStore()
	target := newFakeStore()

	source.putEvent(endedEvent("e1", "keep", nil))
	source.putMemory(memory("m1", "e1", "body", 5))

	target.failNext("ImportBatch", errors.New("target unavailable"))

	p := newPromoter(t, source, target, `{"defaultAction":"promote","rules":[]}`, Config{})

	stats, err := p.Pass(context.Background())
	if err != nil {
		t.Fatalf("Pass: %s", err)
	}

	if stats.Errors != 1 || stats.EventsPromoted != 0 {
		t.Errorf("expected the promotion to fail, got %+v", stats)
	}

	if got := source.eventIds(); !reflect.DeepEqual(got, []string{"e1"}) {
		t.Errorf("expected the event to survive on the source, got %v", got)
	}

	if got := source.memoryIds(); !reflect.DeepEqual(got, []string{"m1"}) {
		t.Errorf("expected the memories to survive on the source, got %v", got)
	}
}

// TestPassRepromotesAfterAFailedDrain is the exactly-once argument: promotion succeeded, the drain
// did not, and the next pass re-promotes the identical rows through an idempotent upsert rather than
// duplicating anything.
func TestPassRepromotesAfterAFailedDrain(t *testing.T) {
	source := newFakeStore()
	target := newFakeStore()

	source.putEvent(endedEvent("e1", "keep", nil))
	source.putMemory(memory("m1", "e1", "body", 5))

	source.failNext("DeleteEvent", errors.New("source busy"))

	p := newPromoter(t, source, target, `{"defaultAction":"promote","rules":[]}`, Config{})

	stats, err := p.Pass(context.Background())
	if err != nil {
		t.Fatalf("first Pass: %s", err)
	}

	if stats.EventsPromoted != 1 || stats.Errors != 1 {
		t.Errorf("expected a promotion with a failed drain, got %+v", stats)
	}

	if got := source.eventIds(); !reflect.DeepEqual(got, []string{"e1"}) {
		t.Fatalf("expected the undrained event to remain, got %v", got)
	}

	stats, err = p.Pass(context.Background())
	if err != nil {
		t.Fatalf("second Pass: %s", err)
	}

	if stats.EventsPromoted != 1 || stats.Errors != 0 {
		t.Errorf("expected the retry to promote and drain cleanly, got %+v", stats)
	}

	if got := target.memoryIds(); !reflect.DeepEqual(got, []string{"m1"}) {
		t.Errorf("the re-promotion must upsert, not duplicate; target holds %v", got)
	}

	if got := source.eventIds(); len(got) != 0 {
		t.Errorf("expected the source drained on the retry, got %v", got)
	}
}

// TestDrainSkipsWhenTheEventChangedUnderneath covers the guard behind the settle window: a memory
// that landed against the event after it was read must not be deleted without ever being judged.
func TestDrainSkipsWhenTheEventChangedUnderneath(t *testing.T) {
	source := newFakeStore()
	target := newFakeStore()

	source.putEvent(endedEvent("e1", "keep", nil))
	source.putMemory(memory("m1", "e1", "read", 5))

	p := newPromoter(t, source, target, `{"defaultAction":"promote","rules":[]}`, Config{})

	// The judgement saw one memory; the drain's re-check will see two.
	if err := p.drainAfterInsert(source, "e1", 1, memory("m2", "e1", "landed late", 5)); err != nil {
		t.Fatalf("drain: %s", err)
	}

	if got := source.eventIds(); !reflect.DeepEqual(got, []string{"e1"}) {
		t.Errorf("expected the changed event to be left alone, got %v", got)
	}

	if got := source.memoryIds(); !reflect.DeepEqual(got, []string{"m1", "m2"}) {
		t.Errorf("expected both memories to survive, got %v", got)
	}
}

// drainAfterInsert writes a memory and then drains against the pre-insert count, which is the race
// the re-check exists for.
func (p *Promoter) drainAfterInsert(source *fakeStore, eventId string, expected int, late *contract.Memory) error {
	source.putMemory(late)

	return p.drain(context.Background(), eventId, expected)
}

// TestPassSkipsAnEventOverTheMemoryCap pins that an event too large to judge whole is left alone and
// reported, rather than judged on a truncated view of itself.
func TestPassSkipsAnEventOverTheMemoryCap(t *testing.T) {
	source := newFakeStore()
	target := newFakeStore()

	source.putEvent(endedEvent("e1", "huge", nil))

	for i := range 5 {
		source.putMemory(memory(fmt.Sprintf("m%d", i), "e1", "body", 1))
	}

	p := newPromoter(t, source, target, `{"defaultAction":"promote","rules":[]}`, Config{MaxEventMemories: 3})

	stats, err := p.Pass(context.Background())
	if err != nil {
		t.Fatalf("Pass: %s", err)
	}

	if stats.Skipped != 1 || stats.EventsJudged != 0 || stats.EventsPromoted != 0 {
		t.Errorf("expected the event to be skipped unjudged, got %+v", stats)
	}

	if got := source.eventIds(); !reflect.DeepEqual(got, []string{"e1"}) {
		t.Errorf("expected the event to remain, got %v", got)
	}

	if got := target.eventIds(); len(got) != 0 {
		t.Errorf("nothing should have reached the target, got %v", got)
	}
}

// TestPassCarriesLinks covers the graph surviving the hop: a memory's outbound links ride on the
// memory, and an event's are read separately because GetEvents does not populate them.
func TestPassCarriesLinks(t *testing.T) {
	source := newFakeStore()
	target := newFakeStore()

	source.putEvent(endedEvent("e1", "keep", nil))
	source.eventLinks["e1"] = []*contract.Link{{Id: "e0", Significance: 4}}

	linked := memory("m1", "e1", "body", 5)
	linked.Links = []*contract.Link{{Id: "m0", Significance: 3}}
	source.putMemory(linked)

	p := newPromoter(t, source, target, `{"defaultAction":"promote","rules":[]}`, Config{})

	if _, err := p.Pass(context.Background()); err != nil {
		t.Fatalf("Pass: %s", err)
	}

	promoted := target.events["e1"]
	if promoted == nil {
		t.Fatal("the event did not reach the target")
	}

	if len(promoted.GetLinks()) != 1 || promoted.GetLinks()[0].GetId() != "e0" {
		t.Errorf("expected the event's outbound link to be carried, got %+v", promoted.GetLinks())
	}

	if got := target.memories["m1"]; got == nil || len(got.GetLinks()) != 1 || got.GetLinks()[0].GetId() != "m0" {
		t.Errorf("expected the memory's link to be carried, got %+v", got)
	}
}

// TestPassDryRunTouchesNothing covers the flag an operator reaches for first when writing rules.
func TestPassDryRunTouchesNothing(t *testing.T) {
	source := newFakeStore()
	target := newFakeStore()

	source.putEvent(endedEvent("e1", "keep", nil))
	source.putMemory(memory("m1", "e1", "body", 5))

	p := newPromoter(t, source, target, `{"defaultAction":"promote","rules":[]}`, Config{DryRun: true})

	stats, err := p.Pass(context.Background())
	if err != nil {
		t.Fatalf("Pass: %s", err)
	}

	if stats.EventsPromoted != 1 || stats.MemoriesPromoted != 1 {
		t.Errorf("expected the dry run to report what it would do, got %+v", stats)
	}

	if got := target.eventIds(); len(got) != 0 {
		t.Errorf("a dry run must not write to the target, got %v", got)
	}

	if got := source.eventIds(); !reflect.DeepEqual(got, []string{"e1"}) {
		t.Errorf("a dry run must not drain the source, got %v", got)
	}
}

// TestPassContinuesPastABrokenRule pins that a rule erroring on every event neither matches nor
// stops the pass - the error is counted and the default action still applies.
func TestPassContinuesPastABrokenRule(t *testing.T) {
	source := newFakeStore()
	target := newFakeStore()

	source.putEvent(endedEvent("e1", "no metadata at all", nil))

	p := newPromoter(t, source, target, `{
		"defaultAction": "drop",
		"rules": [{"name":"unguarded","expr":"event.metadata['team'] == 'x'","action":"promote"}]
	}`, Config{})

	stats, err := p.Pass(context.Background())
	if err != nil {
		t.Fatalf("Pass: %s", err)
	}

	if stats.Errors != 1 {
		t.Errorf("expected the evaluation error to be counted, got %+v", stats)
	}

	if stats.EventsDropped != 1 {
		t.Errorf("expected the default action to apply, got %+v", stats)
	}
}

// TestPassPagesPastTheFirstPage covers the re-read-the-first-page loop: draining shifts later events
// into the window an offset would have skipped, so the pass keeps asking until a page brings nothing
// new.
func TestPassPagesPastTheFirstPage(t *testing.T) {
	source := newFakeStore()
	target := newFakeStore()

	for i := range 7 {
		id := fmt.Sprintf("e%d", i)
		source.putEvent(endedEvent(id, "one of many", nil))
		source.putMemory(memory("m"+id, id, "body", 3))
	}

	p := newPromoter(t, source, target, `{"defaultAction":"promote","rules":[]}`, Config{PageSize: 2})

	stats, err := p.Pass(context.Background())
	if err != nil {
		t.Fatalf("Pass: %s", err)
	}

	if stats.EventsJudged != 7 || stats.EventsPromoted != 7 {
		t.Errorf("expected every event to be judged in one pass, got %+v", stats)
	}

	if got := len(target.eventIds()); got != 7 {
		t.Errorf("expected 7 events on the target, got %d", got)
	}

	if got := source.eventIds(); len(got) != 0 {
		t.Errorf("expected the source drained, got %v", got)
	}
}

// TestPassStopsWhenNothingNewCanBeDrained guards the loop's termination: an event that cannot be
// judged (over the cap) is seen once and does not spin the pass forever.
func TestPassStopsWhenNothingNewCanBeDrained(t *testing.T) {
	source := newFakeStore()
	target := newFakeStore()

	source.putEvent(endedEvent("e1", "stuck", nil))
	source.putMemory(memory("m1", "e1", "body", 1))
	source.putMemory(memory("m2", "e1", "body", 1))

	p := newPromoter(t, source, target, `{"defaultAction":"promote","rules":[]}`, Config{MaxEventMemories: 1})

	done := make(chan Stats, 1)

	go func() {
		stats, _ := p.Pass(context.Background())
		done <- stats
	}()

	select {

	case stats := <-done:
		if stats.Skipped != 1 {
			t.Errorf("expected the stuck event to be skipped once, got %+v", stats)
		}

	case <-time.After(5 * time.Second):
		t.Fatal("Pass did not terminate with an undrainable event")

	}
}

// TestRunStopsOnContextCancellation covers the loop shell.
func TestRunStopsOnContextCancellation(t *testing.T) {
	source := newFakeStore()
	target := newFakeStore()

	p := newPromoter(t, source, target, `{"defaultAction":"drop","rules":[]}`, Config{Interval: time.Millisecond})

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	if err := p.Run(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("expected the context error, got %v", err)
	}
}

// TestPassWithoutARuleset fails rather than defaulting to some action of its own - there is no safe
// guess about what to do with an operator's data.
func TestPassWithoutARuleset(t *testing.T) {
	p := New(newFakeStore(), newFakeStore(), rules.NewStaticWatcher(nil), Config{})

	if _, err := p.Pass(context.Background()); err == nil {
		t.Fatal("expected a pass with no ruleset to fail")
	}
}

// TestBatchByBytes covers the batching rule, including the one case worth stating: a single memory
// over the budget goes alone rather than being dropped.
func TestBatchByBytes(t *testing.T) {
	small := func(id string) *contract.Memory {
		return &contract.Memory{Id: id, Body: "x"}
	}

	batches := batchByBytes([]*contract.Memory{small("a"), small("b"), small("c")}, memoryOverheadBytes*2)
	if len(batches) != 3 {
		t.Errorf("expected each memory in its own batch, got %d batches", len(batches))
	}

	huge := &contract.Memory{Id: "big", Body: string(make([]byte, 4096))}

	batches = batchByBytes([]*contract.Memory{huge}, 10)
	if len(batches) != 1 || len(batches[0]) != 1 {
		t.Errorf("an oversized memory must still be sent, alone; got %d batches", len(batches))
	}

	if batches := batchByBytes(nil, 100); batches != nil {
		t.Errorf("expected no batches for no memories, got %v", batches)
	}
}

// TestDryRunCountsTheReduction pins that a dry run reports what would actually cross, not the
// pre-reduction figure - the number an operator is running --dry-run to check.
func TestDryRunCountsTheReduction(t *testing.T) {
	cases := []struct {
		name   string
		reduce string
		want   int
	}{
		{"no reduction", `null`, 4},
		{"keepTopN", `{"keepTopN":2}`, 2},
		{"minSignificance", `{"minSignificance":5}`, 2},
		{"summarise is one memory", `{"summarise":true}`, 1},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			source := newFakeStore()
			target := newFakeStore()

			source.putEvent(endedEvent("e1", "busy", nil))
			source.putMemory(memory("m1", "e1", "a", 1))
			source.putMemory(memory("m2", "e1", "b", 5))
			source.putMemory(memory("m3", "e1", "c", 2))
			source.putMemory(memory("m4", "e1", "d", 9))

			doc := fmt.Sprintf(
				`{"defaultAction":"drop","rules":[{"name":"r","expr":"true","action":"promote","reduce":%s}]}`,
				c.reduce,
			)

			p := newPromoter(t, source, target, doc, Config{DryRun: true})

			stats, err := p.Pass(context.Background())
			if err != nil {
				t.Fatalf("Pass: %s", err)
			}

			if stats.MemoriesPromoted != c.want {
				t.Errorf("expected %d memories reported, got %d", c.want, stats.MemoriesPromoted)
			}

			// Whatever the reduction says, a dry run leaves both sides exactly as they were - in
			// particular it never calls SummariseMemories, which mutates the source.
			if got := source.memoryIds(); len(got) != 4 {
				t.Errorf("a dry run must not change the source, got %v", got)
			}

			if n := source.callCount("SummariseMemories"); n != 0 {
				t.Errorf("a dry run must not summarise, got %d calls", n)
			}
		})
	}
}

// TestStalenessClockOnlyAdvancesOnASuccessfulPass is the property the staleness gauge depends on.
// Every other metric here is a counter, and a counter that stops advancing looks exactly like an
// ingestor with nothing to do - so seconds-since-last-pass is what alerts on silence. Marking a
// FAILED pass as fresh would hide precisely the failure worth catching.
func TestStalenessClockOnlyAdvancesOnASuccessfulPass(t *testing.T) {
	source := newFakeStore()
	target := newFakeStore()

	p := newPromoter(t, source, target, `{"defaultAction":"drop","rules":[]}`, Config{})

	if got := p.lastPass.Load(); got != 0 {
		t.Fatalf("expected no recorded pass before the first run, got %d", got)
	}

	if _, err := p.Pass(context.Background()); err != nil {
		t.Fatalf("Pass: %s", err)
	}

	first := p.lastPass.Load()
	if first == 0 {
		t.Fatal("expected a successful pass to stamp the staleness clock")
	}

	// A pass that cannot even list events has proved nothing about the ingestor working.
	source.failNext("GetEvents", errors.New("source unreachable"))

	if _, err := p.Pass(context.Background()); err == nil {
		t.Fatal("expected the failed listing to fail the pass")
	}

	if got := p.lastPass.Load(); got != first {
		t.Errorf("a failed pass must not refresh the staleness clock: %d became %d", first, got)
	}

	// A pass with no ruleset is the other failure, and must behave the same way.
	unloaded := New(source, target, rules.NewStaticWatcher(nil), Config{})
	unloaded.now = p.now

	if _, err := unloaded.Pass(context.Background()); err == nil {
		t.Fatal("expected a pass with no ruleset to fail")
	}

	if got := unloaded.lastPass.Load(); got != 0 {
		t.Errorf("expected no staleness stamp from a pass that never ran, got %d", got)
	}
}
