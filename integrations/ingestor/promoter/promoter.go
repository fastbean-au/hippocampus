// Package promoter is the ingestor's loop: it finds completed events on the source (edge) instance,
// judges each against the rules, and either promotes it to the target (central) instance or drops
// it - draining it from the source either way.
//
// The design turns on one property of the RPCs it uses. ImportBatch is a full-state upsert by id,
// so promotion is idempotent; promote-then-drain is therefore at-least-once with an idempotent
// receiver, and a crash between the two re-promotes identical rows on the next pass. That is why
// this package holds no cursor, no bookmark and no state of any kind: THE SOURCE STORE IS THE QUEUE,
// and what it contains is exactly what has not been judged yet.
package promoter

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync/atomic"
	"time"

	"github.com/fastbean-au/hippocampus/observability"
	log "github.com/sirupsen/logrus"
	"go.opentelemetry.io/otel/attribute"
	"google.golang.org/grpc"

	"github.com/fastbean-au/hippocampus/contract"

	"github.com/fastbean-au/hippocampus/integrations/ingestor/rules"
)

// Defaults for the Config fields left unset.
const (
	DefaultInterval      = 30 * time.Second
	DefaultSettle        = 60 * time.Second
	DefaultPageSize      = int32(100)
	DefaultCallTimeout   = 30 * time.Second
	DefaultMaxBatchBytes = 3 * 1024 * 1024

	// DefaultMaxEventMemories bounds how many memories one event may hold and still be judged. An
	// event over the cap is reported and left alone rather than judged on a truncated view of
	// itself: promoting or dropping on partial facts is exactly the failure an admission gate must
	// not have. See pass.
	DefaultMaxEventMemories = 10_000

	// memoryOverheadBytes approximates a memory's serialised proto size beyond its body when
	// byte-budgeting an ImportBatch, mirroring the service's own transferMemoryOverheadBytes.
	memoryOverheadBytes = 128
)

// OrphanPolicy says what to do with memories carrying no event id. Rules key on events, so an
// orphan is never judged - and stranding it silently is the one outcome worth ruling out.
type OrphanPolicy string

const (
	// OrphanIgnore leaves orphans alone, reporting how many were seen. The source's own decay
	// eventually reaps them.
	OrphanIgnore OrphanPolicy = "ignore"

	// OrphanPromote promotes orphans older than OrphanAge, then deletes them from the source.
	OrphanPromote OrphanPolicy = "promote"

	// OrphanDrop deletes orphans older than OrphanAge from the source without promoting them.
	OrphanDrop OrphanPolicy = "drop"
)

// ValidOrphanPolicy reports whether p is one of the three policies.
func ValidOrphanPolicy(p OrphanPolicy) bool {
	switch p {

	case OrphanIgnore, OrphanPromote, OrphanDrop:
		return true

	default:
		return false

	}
}

// Config bounds one pass. Zero values select the package defaults.
type Config struct {
	// Interval is how often Run starts a pass.
	Interval time.Duration

	// Settle is how long an event must have been ended before it is judged. It is what makes
	// fetch-judge-promote-drain safe against a memory landing against an already-ended event: the
	// window belongs to the writer, not to this loop. Drain also re-checks the count before
	// deleting, so Settle is the cheap guard and that is the correct one.
	Settle time.Duration

	// PageSize is the page size used for both the event scan and the per-event memory reads.
	PageSize int32

	// CallTimeout bounds each individual RPC.
	CallTimeout time.Duration

	// MaxBatchBytes bounds the estimated serialised size of one ImportBatch call, so a page of
	// large bodies is split rather than overflowing the target's receive frame.
	MaxBatchBytes int

	// MaxEventMemories bounds how many memories an event may hold and still be judged.
	MaxEventMemories int

	// Orphans selects what happens to memories with no event, and OrphanAge is how old one must be
	// before the policy acts on it.
	Orphans   OrphanPolicy
	OrphanAge time.Duration

	// DryRun judges and reports without promoting, dropping or draining anything.
	DryRun bool
}

func (c Config) interval() time.Duration {
	if c.Interval <= 0 {
		return DefaultInterval
	}

	return c.Interval
}

func (c Config) settle() time.Duration {
	if c.Settle < 0 {
		return DefaultSettle
	}

	return c.Settle
}

func (c Config) pageSize() int32 {
	if c.PageSize <= 0 {
		return DefaultPageSize
	}

	return c.PageSize
}

func (c Config) callTimeout() time.Duration {
	if c.CallTimeout <= 0 {
		return DefaultCallTimeout
	}

	return c.CallTimeout
}

func (c Config) maxBatchBytes() int {
	if c.MaxBatchBytes <= 0 {
		return DefaultMaxBatchBytes
	}

	return c.MaxBatchBytes
}

func (c Config) maxEventMemories() int {
	if c.MaxEventMemories <= 0 {
		return DefaultMaxEventMemories
	}

	return c.MaxEventMemories
}

// Client is the slice of the generated Hippocampus client this package needs, at both ends. Both
// the source and the target are described by the same interface because the pair is symmetric -
// which is also what lets the tests drive a whole pass against two fakes.
type Client interface {
	GetEvents(ctx context.Context, in *contract.GetEventsRequest, opts ...grpc.CallOption) (*contract.GetEventsResponse, error)
	GetMemories(ctx context.Context, in *contract.GetMemoriesRequest, opts ...grpc.CallOption) (*contract.GetMemoriesResponse, error)
	GetEventLinks(ctx context.Context, in *contract.GetEventLinksRequest, opts ...grpc.CallOption) (*contract.GetLinksResponse, error)
	SummariseMemories(ctx context.Context, in *contract.SummariseMemoriesRequest, opts ...grpc.CallOption) (*contract.SummariseMemoriesResponse, error)
	DeleteEvent(ctx context.Context, in *contract.DeleteEventRequest, opts ...grpc.CallOption) (*contract.GeneralResponse, error)
	DeleteMemories(ctx context.Context, in *contract.DeleteMemoriesRequest, opts ...grpc.CallOption) (*contract.GeneralResponse, error)
	ImportBatch(ctx context.Context, in *contract.ImportBatchRequest, opts ...grpc.CallOption) (*contract.ImportBatchResponse, error)
}

// Promoter drains judged events from a source instance into a target instance.
type Promoter struct {
	source Client
	target Client
	rules  *rules.Watcher
	cfg    Config

	// now is time.Now, replaceable in tests so the settle window is exact rather than racy.
	now func() time.Time

	// lastPass is the UnixNano of the last SUCCESSFUL pass, backing the staleness gauge. Atomic
	// because Run's loop and a probe handler may both touch it.
	lastPass atomic.Int64
}

// New builds a Promoter. The watcher is read once per pass, so every event in one pass is judged by
// one ruleset and a reload landing mid-pass takes effect at the next one.
func New(source Client, target Client, watcher *rules.Watcher, cfg Config) *Promoter {
	return &Promoter{
		source: source,
		target: target,
		rules:  watcher,
		cfg:    cfg,
		now:    time.Now,
	}
}

// Stats reports what one pass did.
type Stats struct {
	EventsJudged     int
	EventsPromoted   int
	EventsDropped    int
	MemoriesPromoted int
	OrphansSeen      int
	OrphansPromoted  int
	OrphansDropped   int

	// Skipped counts events left for a later pass - over the memory cap, or changed underneath the
	// judgement. Errors counts failures that were logged and stepped over.
	Skipped int
	Errors  int
}

// Run starts a pass every Interval until the context is cancelled. A pass failing is logged and the
// loop continues: the source store is the queue, so whatever was not drained is simply retried.
func (p *Promoter) Run(ctx context.Context) error {
	log.Trace("func() Promoter.Run")

	ticker := time.NewTicker(p.cfg.interval())
	defer ticker.Stop()

	for {
		stats, err := p.Pass(ctx)
		if err != nil {
			log.Errorf("ingestor pass failed: %s", err.Error())
		}

		logStats(stats)

		select {

		case <-ctx.Done():
			return ctx.Err()

		case <-ticker.C:

		}
	}
}

// logStats reports a pass at info when it did something, and at debug when it did not - so a quiet
// ingestor does not fill a log with empty passes.
func logStats(stats Stats) {
	if stats == (Stats{}) {
		log.Debug("ingestor pass: nothing to judge")

		return
	}

	log.WithFields(log.Fields{
		"events_judged":     stats.EventsJudged,
		"events_promoted":   stats.EventsPromoted,
		"events_dropped":    stats.EventsDropped,
		"memories_promoted": stats.MemoriesPromoted,
		"orphans_seen":      stats.OrphansSeen,
		"orphans_promoted":  stats.OrphansPromoted,
		"orphans_dropped":   stats.OrphansDropped,
		"skipped":           stats.Skipped,
		"errors":            stats.Errors,
	}).
		Info("ingestor pass complete")
}

// Pass judges every settled, completed event on the source once.
//
// It re-reads the FIRST page each time rather than paging with an offset, because a judged event is
// deleted and every later event shifts back into the window an offset would have skipped past. The
// ids already seen bound the loop: a page yielding nothing new means the remaining events are ones
// this pass could not drain (over cap, changed underneath it, or erroring), and they belong to the
// next pass.
func (p *Promoter) Pass(ctx context.Context) (Stats, error) {
	log.Trace("func() Promoter.Pass")

	var stats Stats

	start := p.now()

	ruleset := p.rules.Current()
	if ruleset == nil {
		p.recordPass(ctx, start, false)

		return stats, fmt.Errorf("no ruleset is loaded")
	}

	cutoff := p.now().Add(-p.cfg.settle()).UnixNano()
	seen := make(map[string]struct{})

	for {
		events, err := p.completedEvents(ctx, cutoff)
		if err != nil {
			p.recordPass(ctx, start, false)

			return stats, err
		}

		fresh := 0

		for _, event := range events {
			if _, already := seen[event.GetId()]; already {
				continue
			}

			seen[event.GetId()] = struct{}{}
			fresh++

			p.judge(ctx, ruleset, event, &stats)
		}

		if fresh == 0 {
			break
		}
	}

	p.handleOrphans(ctx, &stats)

	p.recordPass(ctx, start, true)

	return stats, nil
}

// recordPass reports the pass's duration and outcome, and resets the staleness gauge.
//
// The gauge is the point of this: every other metric here is a counter, and a counter that stops
// advancing is indistinguishable from an ingestor with nothing to do. Seconds-since-last-pass rises
// on its own whether the loop is stalled, deadlocked or dead, so it is the one signal that alerts on
// silence. It is only reset on a SUCCESSFUL pass - a pass that failed has not proved the ingestor is
// working, and marking it fresh would hide exactly the failure worth catching.
func (p *Promoter) recordPass(ctx context.Context, start time.Time, ok bool) {
	outcome := outcomeFailed
	if ok {
		outcome = "ok"

		p.lastPass.Store(p.now().UnixNano())
	}

	attrs := observability.WithGroup(attribute.String(attrOutcome, outcome))

	tel.passes.Add(ctx, 1, attrs)
	tel.passDuration.Record(ctx, p.now().Sub(start).Seconds(), attrs)

	p.observeStaleness(ctx)
}

// observeStaleness publishes seconds-since-last-pass. It is recorded here, at the end of each pass,
// rather than through an observable-gauge callback: a callback fires on the exporter's schedule and
// would keep reporting a plausible value from a goroutine that has stopped running passes entirely,
// which is the opposite of what this measures.
func (p *Promoter) observeStaleness(ctx context.Context) {
	last := p.lastPass.Load()
	if last == 0 {
		return
	}

	tel.sinceLastRun.Record(ctx, p.now().Sub(time.Unix(0, last)).Seconds())
}

// completedEvents reads one page of events that have ended at or before the cutoff. TimeEndMin of 1
// excludes events still open: an open event stores a time_end of 0, and 0 in this API means "no
// bound" rather than a literal zero.
func (p *Promoter) completedEvents(ctx context.Context, cutoff int64) ([]*contract.Event, error) {
	callCtx, cancel := p.callContext(ctx)
	defer cancel()

	res, err := p.source.GetEvents(callCtx, &contract.GetEventsRequest{
		TimeEndMin: 1,
		TimeEndMax: cutoff,
		OrderBy:    "timestamp",
		Limit:      p.cfg.pageSize(),
	})
	if err != nil {
		return nil, fmt.Errorf("listing completed events: %w", err)
	}

	return res.GetEvents(), nil
}

// judge evaluates one event and acts on the decision, recording the outcome. Every failure here is
// logged and counted rather than returned: one bad event must not stop the pass, and the event stays
// on the source to be retried.
func (p *Promoter) judge(ctx context.Context, ruleset *rules.Ruleset, event *contract.Event, stats *Stats) {
	memories, ok := p.eventMemories(ctx, event, stats)
	if !ok {
		return
	}

	facts := buildFacts(event, memories, ruleset.NeedsMemories())

	decision, err := ruleset.Evaluate(ctx, facts)
	if err != nil {
		// Reported, never silently treated as a non-match: a rule erroring on every event would
		// otherwise look exactly like a rule that simply does not apply.
		log.Errorf("rule evaluation error on event '%s': %s", event.GetId(), err.Error())

		stats.Errors++

		p.recordRuleError(ctx, err)
	}

	stats.EventsJudged++

	log.WithFields(log.Fields{
		"event_id": event.GetId(),
		"rule":     decisionRule(decision),
		"action":   decision.Action,
		"memories": len(memories),
	}).
		Debug("event judged")

	judged := &judgement{event: event, memories: memories, facts: facts, decision: decision}

	if p.cfg.DryRun {
		p.dryRun(ctx, judged, stats)

		return
	}

	switch decision.Action {

	case rules.ActionPromote:
		p.promote(ctx, judged, stats)

	case rules.ActionDrop:
		if err := p.drain(ctx, event.GetId(), len(memories)); err != nil {
			log.Errorf("dropping event '%s': %s", event.GetId(), err.Error())

			stats.Errors++
			p.recordEvent(ctx, decision, outcomeFailed)

			return
		}

		stats.EventsDropped++
		p.recordEvent(ctx, decision, outcomeDropped)

	}
}

// failEvent reports a failure that leaves the event where it is, to be re-judged next pass.
//
// A mutation failing is treated exactly as a promotion failing, and deliberately NOT as a rule that
// did not match: there is no safe fallback for a field the operator asked to have set. Promoting at
// the edge's own significance would put the record into the central store's decay model at a weight
// the rules file explicitly rejected, and silently. Failing leaves it on the source, which is the
// one outcome that loses nothing.
func (p *Promoter) failEvent(ctx context.Context, judged *judgement, stats *Stats, err error) {
	log.Error(err.Error())

	stats.Errors++

	p.recordEvent(ctx, judged.decision, outcomeFailed)
	p.recordRuleError(ctx, err)
}

// recordRuleError counts an evaluation failure against the rule that caused it, which is what makes
// it attributable rather than merely visible: "rule X errors on every event" is the diagnosis, and a
// bare error total cannot give it. Errors that are not a rule's fault carry no EvalError and are
// left to the pass-level counters.
func (p *Promoter) recordRuleError(ctx context.Context, err error) {
	var evalErr *rules.EvalError

	if errors.As(err, &evalErr) {
		tel.ruleErrors.Add(ctx, 1, observability.WithGroup(attribute.String(attrRule, evalErr.Rule)))
	}
}

// dryRun reports what a judgement would have done without moving or deleting anything.
//
// The mutation IS evaluated, because it reads only facts already in hand and because the numbers it
// produces are exactly what a dry run exists to check - a scoring rule whose significance is not
// shown has not been tested. The one part that cannot be shown is a summarising rule's per-memory
// mutation: the summary does not exist until the source is asked to write it, which a dry run must
// not do.
func (p *Promoter) dryRun(ctx context.Context, judged *judgement, stats *Stats) {
	if err := p.applyEventSet(ctx, judged); err != nil {
		p.failEvent(ctx, judged, stats, err)

		return
	}

	if !judged.decision.Reduce.Summarise {
		if err := p.applyMemorySet(ctx, judged, judged.memories); err != nil {
			p.failEvent(ctx, judged, stats, err)

			return
		}
	}

	p.countDecision(judged.decision, judged.memories, stats)
}

// recordEvent counts one judged event against the rule that decided it.
//
// The outcome is four-valued rather than a success bool because an event DROPPED by a rule is the
// admission gate working, not a failure - a rules file that discards most of what it sees would
// otherwise be indistinguishable from an ingestor that cannot promote anything. `skipped` and
// `failed` are the two that warrant an alert.
func (p *Promoter) recordEvent(ctx context.Context, decision rules.Decision, outcome string) {
	tel.events.Add(ctx, 1, observability.WithGroup(
		attribute.String(attrOutcome, outcome),
		attribute.String(attrRule, decisionRule(decision)),
	))
}

// countDecision records what a dry run would have done. The reduction is applied to the COUNT here,
// even though nothing is moved: reporting the pre-reduction figure would tell an operator that six
// memories were about to cross when the rule they are testing only lets four through, which is
// exactly the number a dry run exists to check.
//
// It never calls summarise - that mutates the source - so a summarising rule reports the one memory
// a summary is.
func (p *Promoter) countDecision(decision rules.Decision, memories []*contract.Memory, stats *Stats) {
	switch decision.Action {

	case rules.ActionPromote:
		stats.EventsPromoted++
		stats.MemoriesPromoted += p.wouldPromote(memories, decision.Reduce)

	case rules.ActionDrop:
		stats.EventsDropped++

	}
}

// wouldPromote is how many memories the reduction would let across.
func (p *Promoter) wouldPromote(memories []*contract.Memory, reduction rules.Reduce) int {
	if reduction.Summarise {
		return 1
	}

	count := len(memories)

	if reduction.MinSignificance > 0 {
		count = 0

		for _, memory := range memories {
			if memory.GetSignificance() < reduction.MinSignificance {
				continue
			}

			count++
		}
	}

	if reduction.KeepTopN > 0 && count > reduction.KeepTopN {
		count = reduction.KeepTopN
	}

	return count
}

// decisionRule names the rule that decided, or the default.
func decisionRule(decision rules.Decision) string {
	if decision.Rule == "" {
		return defaultRuleLabel
	}

	return decision.Rule
}

// eventMemories pages one event's memories, using the event_id filter rather than GetEventById with
// memories: true - the latter returns every memory in a single message and overruns the receive
// frame on a large event. The false return means the event was skipped and already counted.
func (p *Promoter) eventMemories(ctx context.Context, event *contract.Event, stats *Stats) ([]*contract.Memory, bool) {
	memories, total, err := p.readEventMemories(ctx, event.GetId())
	if err != nil {
		log.Errorf("reading memories of event '%s': %s", event.GetId(), err.Error())

		stats.Errors++

		// No decision was reached, so there is no rule to attribute this to - hence the default
		// label rather than a blank one, which would make the series unqueryable.
		p.recordEvent(ctx, rules.Decision{}, outcomeFailed)

		return nil, false
	}

	if total > p.cfg.maxEventMemories() {
		// Judging a truncated view of the event would promote or drop on facts that are not the
		// event's, so it is left alone and said out loud. Raise maxEventMemories, or narrow what the
		// writer groups into one event.
		log.Errorf(
			"event '%s' holds %d memories, over the %d cap - left unjudged (raise --max-event-memories or split the event)",
			event.GetId(),
			total,
			p.cfg.maxEventMemories(),
		)

		stats.Skipped++
		p.recordEvent(ctx, rules.Decision{}, outcomeSkipped)

		return nil, false
	}

	return memories, true
}

// readEventMemories pages every memory of one event, returning them and the total the store
// reported. Links are requested so a promoted memory carries the edges it declared.
func (p *Promoter) readEventMemories(ctx context.Context, eventId string) ([]*contract.Memory, int, error) {
	var out []*contract.Memory

	offset := int32(0)
	total := 0

	for {
		callCtx, cancel := p.callContext(ctx)

		res, err := p.source.GetMemories(callCtx, &contract.GetMemoriesRequest{
			EventId: eventId,
			OrderBy: "timestamp",
			Limit:   p.cfg.pageSize(),
			Offset:  offset,
			Links:   true,
		})

		cancel()

		if err != nil {
			return nil, 0, err
		}

		total = int(res.GetTotalCount())

		if total > p.cfg.maxEventMemories() {
			return nil, total, nil
		}

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

// promote sends the event and its (possibly reduced) memories to the target, then drains the whole
// event from the source. The order is deliberate: the target is written first and the source is
// emptied only once it has accepted everything, so a failure anywhere leaves the records where they
// still exist rather than nowhere.
func (p *Promoter) promote(ctx context.Context, judged *judgement, stats *Stats) {
	event := judged.event
	decision := judged.decision

	// The event's own mutation runs first, against the facts the rule matched on rather than against
	// a reduced or summarised version of the event that no rule ever saw.
	if err := p.applyEventSet(ctx, judged); err != nil {
		p.failEvent(ctx, judged, stats, err)

		return
	}

	// onSource is what the event is expected to hold when the drain re-checks it, which is NOT the
	// number about to be promoted: a keepTopN/minSignificance reduction chooses what crosses to the
	// central store and leaves the rest in place to be drained, while summarise actually replaces
	// them on the source. Conflating the two makes every reduced event look like it changed
	// underneath the judgement, and nothing is ever drained.
	memories, onSource, err := p.reduce(ctx, judged)
	if err != nil {
		p.failEvent(ctx, judged, stats, fmt.Errorf("reducing event '%s': %w", event.GetId(), err))

		return
	}

	if err := p.sendEvent(ctx, event); err != nil {
		p.failEvent(ctx, judged, stats, fmt.Errorf("promoting event '%s': %w", event.GetId(), err))

		return
	}

	sent, err := p.sendMemories(ctx, memories)

	// Counted before the error check: memories that reached the target before a mid-batch failure
	// really did cross, and not counting them would understate what the central store received.
	if sent > 0 {
		tel.memories.Add(ctx, int64(sent), observability.WithGroup(attribute.String(attrKind, "event")))
	}

	if err != nil {
		p.failEvent(ctx, judged, stats, fmt.Errorf(
			"promoting memories of event '%s' (%d sent before the failure): %w",
			event.GetId(),
			sent,
			err,
		))

		return
	}

	// The drain is separate from the promotion and may legitimately not happen (a memory landed
	// underneath), so the event counts as promoted either way - re-promoting it next pass is an
	// idempotent upsert of identical rows.
	stats.EventsPromoted++
	stats.MemoriesPromoted += sent

	p.recordEvent(ctx, decision, outcomePromoted)

	if err := p.drain(ctx, event.GetId(), onSource); err != nil {
		log.Errorf("draining promoted event '%s': %s", event.GetId(), err.Error())

		stats.Errors++
	}
}

// reduce applies the decision's memory-scoped mutation and then its reduction, returning the
// memories to promote and how many the SOURCE is expected to hold afterwards (what the drain
// re-checks against).
//
// The two kinds of reduction differ in exactly that second number. A selection reduction is a
// decision about what crosses the boundary and changes nothing on the source, so the source still
// holds every memory the judgement saw. Summarise is a real mutation of the source - it replaces
// them - so what remains is the summary.
//
// The MUTATION RUNS BEFORE THE SELECTION, which is the ordering that makes the two compose: a rule
// that scores memories and then keeps the top ten means the top ten BY ITS OWN SCORE, not by the
// significance the edge happened to store. The cost is evaluating expressions for memories that are
// then discarded; ranking by a number the rule just declared irrelevant would be worse. After a
// summarise reduction there is one memory to score - the summary - and it is scored for the same
// reason: it is what crosses.
func (p *Promoter) reduce(ctx context.Context, judged *judgement) ([]*contract.Memory, int, error) {
	reduction := judged.decision.Reduce
	memories := judged.memories
	onSource := len(memories)

	if reduction.Summarise {
		summarised, err := p.summarise(ctx, judged.event.GetId())
		if err != nil {
			return nil, 0, err
		}

		memories = summarised
		onSource = len(summarised)
	}

	if err := p.applyMemorySet(ctx, judged, memories); err != nil {
		return nil, 0, err
	}

	if reduction.Summarise {
		return memories, onSource, nil
	}

	if reduction.MinSignificance > 0 {
		kept := make([]*contract.Memory, 0, len(memories))

		for _, memory := range memories {
			if memory.GetSignificance() < reduction.MinSignificance {
				continue
			}

			kept = append(kept, memory)
		}

		memories = kept
	}

	if reduction.KeepTopN > 0 && len(memories) > reduction.KeepTopN {
		// A stable sort so equally significant memories keep their timestamp order, making the
		// selection reproducible rather than dependent on the sort's internals.
		sorted := make([]*contract.Memory, len(memories))
		copy(sorted, memories)

		sort.SliceStable(sorted, func(i int, j int) bool {
			return sorted[i].GetSignificance() > sorted[j].GetSignificance()
		})

		memories = sorted[:reduction.KeepTopN]
	}

	return memories, onSource, nil
}

// summarise asks the SOURCE to replace the event's memories with one generated summary, then reads
// back what is left. It requires ollama.enabled on that instance; a service without it reports
// FailedPrecondition, which is returned rather than swallowed - quietly promoting the whole
// unsummarised event would be the opposite of what the rule asked for.
func (p *Promoter) summarise(ctx context.Context, eventId string) ([]*contract.Memory, error) {
	callCtx, cancel := p.callContext(ctx)

	_, err := p.source.SummariseMemories(callCtx, &contract.SummariseMemoriesRequest{EventId: eventId})

	cancel()

	if err != nil {
		return nil, fmt.Errorf("summarising event %q (the source instance needs ollama.enabled): %w", eventId, err)
	}

	memories, _, err := p.readEventMemories(ctx, eventId)
	if err != nil {
		return nil, fmt.Errorf("reading the summary of event %q: %w", eventId, err)
	}

	return memories, nil
}

// sendEvent imports the event itself, carrying its outbound links. GetEvents does not populate them
// - only the archive walk does - so they are read separately, which is one extra RPC per promoted
// event and the only way an event's graph survives the hop.
func (p *Promoter) sendEvent(ctx context.Context, event *contract.Event) error {
	links, err := p.eventLinks(ctx, event.GetId())
	if err != nil {
		// A link that does not make the crossing costs significance on the far side, not
		// correctness, so this is reported and the event still goes.
		log.Warnf("reading links of event '%s', promoting without them: %s", event.GetId(), err.Error())
	}

	// The nested memories are write-only input on StoreEvent and are not part of an import; they are
	// sent separately and in byte-bounded batches.
	promoted := &contract.Event{
		Id:                   event.GetId(),
		TimeStart:            event.GetTimeStart(),
		TimeEnd:              event.GetTimeEnd(),
		Significance:         event.GetSignificance(),
		Name:                 event.GetName(),
		Description:          event.GetDescription(),
		Group:                event.GetGroup(),
		Metadata:             event.GetMetadata(),
		MemoriesConsolidated: event.GetMemoriesConsolidated(),
		Links:                links,
	}

	callCtx, cancel := p.callContext(ctx)
	defer cancel()

	if _, err := p.target.ImportBatch(callCtx, &contract.ImportBatchRequest{Events: []*contract.Event{promoted}}); err != nil {
		return err
	}

	return nil
}

// eventLinks reads an event's outbound links, the direction Event.links carries (see the contract:
// a Link is a directed edge from the item declaring it, so returning both would double every edge
// on import).
func (p *Promoter) eventLinks(ctx context.Context, eventId string) ([]*contract.Link, error) {
	callCtx, cancel := p.callContext(ctx)
	defer cancel()

	res, err := p.source.GetEventLinks(callCtx, &contract.GetEventLinksRequest{
		Id:        eventId,
		Direction: contract.LinkDirection_LINK_DIRECTION_OUTBOUND,
	})
	if err != nil {
		return nil, err
	}

	links := make([]*contract.Link, 0, len(res.GetLinks()))

	for _, edge := range res.GetLinks() {
		links = append(links, &contract.Link{Id: edge.GetId(), Significance: edge.GetSignificance()})
	}

	return links, nil
}

// sendMemories imports the memories in byte-bounded batches, returning how many were accepted before
// any failure.
func (p *Promoter) sendMemories(ctx context.Context, memories []*contract.Memory) (int, error) {
	sent := 0

	for _, batch := range batchByBytes(memories, p.cfg.maxBatchBytes()) {
		callCtx, cancel := p.callContext(ctx)

		_, err := p.target.ImportBatch(callCtx, &contract.ImportBatchRequest{Memories: batch})

		cancel()

		if err != nil {
			return sent, err
		}

		sent += len(batch)
	}

	return sent, nil
}

// drain deletes the event and every memory it still holds from the source, but only while the event
// still looks the way it did when it was judged. A memory landing against an already-ended event
// after the settle window would otherwise be deleted without ever having been judged - so the count
// is re-read and a change leaves the whole event for the next pass, where it is re-judged whole.
func (p *Promoter) drain(ctx context.Context, eventId string, expected int) error {
	current, err := p.memoryCount(ctx, eventId)
	if err != nil {
		return fmt.Errorf("re-checking event %q before draining: %w", eventId, err)
	}

	if current != expected {
		log.Warnf(
			"event '%s' changed underneath the judgement (%d memories, expected %d) - left for the next pass",
			eventId,
			current,
			expected,
		)

		return nil
	}

	callCtx, cancel := p.callContext(ctx)
	defer cancel()

	if _, err := p.source.DeleteEvent(callCtx, &contract.DeleteEventRequest{Id: eventId, Memories: true}); err != nil {
		return err
	}

	return nil
}

// memoryCount reads how many memories an event currently holds, without reading any of them: a page
// size of 1 still reports the full total_count.
func (p *Promoter) memoryCount(ctx context.Context, eventId string) (int, error) {
	callCtx, cancel := p.callContext(ctx)
	defer cancel()

	res, err := p.source.GetMemories(callCtx, &contract.GetMemoriesRequest{
		EventId: eventId,
		Limit:   1,
	})
	if err != nil {
		return 0, err
	}

	return int(res.GetTotalCount()), nil
}

// callContext bounds one RPC.
func (p *Promoter) callContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, p.cfg.callTimeout())
}

// batchByBytes splits memories into sub-batches each estimated to serialise within maxBytes, so no
// ImportBatch message overflows the target's receive frame. A single oversized memory goes alone
// rather than being dropped - the target must accept it.
func batchByBytes(memories []*contract.Memory, maxBytes int) [][]*contract.Memory {
	var batches [][]*contract.Memory
	var batch []*contract.Memory

	batchBytes := 0

	for _, memory := range memories {
		size := len(memory.GetBody()) + len(memory.GetId()) + len(memory.GetGroup()) + memoryOverheadBytes

		for k, v := range memory.GetMetadata() {
			size += len(k) + len(v)
		}

		if len(batch) > 0 && batchBytes+size > maxBytes {
			batches = append(batches, batch)
			batch = nil
			batchBytes = 0
		}

		batch = append(batch, memory)
		batchBytes += size
	}

	if len(batch) > 0 {
		batches = append(batches, batch)
	}

	return batches
}
