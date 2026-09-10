package hippocampus

import (
	"context"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/viper"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/fastbean-au/hippocampus/db"
	"github.com/fastbean-au/hippocampus/notify"
)

// callbackIdleDelay is how long the dispatcher waits when it finds nothing to send, so an idle
// deployment is not polling a table every second. A var so tests can shorten it.
var callbackIdleDelay = 15 * time.Second

// callbackBusyDelay is the pause between passes while a backlog is still draining: short, because
// there is work, but not zero, so a large backlog does not become a denial of service against
// somebody else's endpoint. A var so tests can shorten it.
var callbackBusyDelay = 250 * time.Millisecond

// The defaults applied when callbacks.* leaves a bound unset.
//
// The queue's caps are measured in hours rather than the days the forgotten log uses, and for the
// same reason the outbox's are: this queue is meant to drain in seconds, so a day of it is already
// an outage. Unlike the outbox there is no sweep behind it - a delivery the caps remove is a
// notification nobody will ever get - which is why passing them is logged at Warn and counted.
const (
	defaultCallbackMaxAgeHours    = 24
	defaultCallbackMaxRows        = 1_000_000
	defaultCallbackBatchSize      = 100
	defaultCallbackBaseBackoff    = time.Second
	defaultCallbackMaxBackoff     = 5 * time.Minute
	defaultCallbackMaxIdsPerChunk = 500
)

// BacklogPolicy is what happens to a DELETION delivery that reaches the queue's caps while the
// receiver is still refusing it - which is to say, who absorbs the failure.
//
// The queue was built as a NOTIFICATION channel and the caps are right for one: a receiver that has
// been unreachable for a day is not helped by a day-old summary of a cycle. A forget-callback is a
// different thing. It is an INSTRUCTION about a payload this store does not hold and cannot see, and
// dropping it leaves that payload orphaned in the far system forever - a leak that is monotonic,
// silent, and grows precisely with how well the decay cycle is working. This repository has already
// paid for that lesson once, in the OpenSearch index that self-healed in one direction and dropped
// in the other until it held 4.38M documents against 211,657 rows.
//
// So there are three answers and they differ in who pays, which is why this is configuration rather
// than a fix. Under BacklogAbandon the far end pays, in orphans. Under BacklogRetain this disk pays,
// in a queue nothing trims. Under BacklogStall the workload pays, in a store that stops forgetting
// until somebody fixes the receiver. Nothing here can make the failure cost nothing.
type BacklogPolicy int

const (
	// BacklogAbandon is the default and is what every deployment had before this existed: the caps
	// discard undelivered deliveries of every kind, deletions included.
	BacklogAbandon BacklogPolicy = iota

	// BacklogRetain exempts the deletion kinds from the caps. Nothing is lost and nothing stops; the
	// queue grows for as long as the receiver is down, on a disk the capacity target deliberately
	// does not count.
	BacklogRetain

	// BacklogStall is BacklogRetain plus a bound: once the retained backlog is past the caps the
	// two decay passes stop running, so the store cannot go on generating instructions nobody is
	// taking delivery of.
	BacklogStall
)

// backlogPolicyNames maps the configuration spelling onto the policy, and is the only place the two
// are related - ParseBacklogPolicy and the startup validation both read it, so a value the service
// accepts and a value it refuses cannot come apart.
var backlogPolicyNames = map[string]BacklogPolicy{
	"abandon": BacklogAbandon,
	"retain":  BacklogRetain,
	"stall":   BacklogStall,
}

// String is the configuration spelling, for the log lines and the stall reason.
func (p BacklogPolicy) String() string {
	for name, policy := range backlogPolicyNames {
		if policy != p {
			continue
		}

		return name
	}

	return "abandon"
}

// retains reports whether the deletion kinds are exempt from the queue's caps under this policy.
func (p BacklogPolicy) retains() bool {
	return p == BacklogRetain || p == BacklogStall
}

// ParseBacklogPolicy resolves callbacks.backlogPolicy, reporting whether the spelling was one this
// build knows. An empty setting is the default rather than an error, so the key stays optional.
//
// Exported because main.go validates the configuration before the server is constructed, and a value
// the validator accepted but the server silently defaulted would be the worst of both.
func ParseBacklogPolicy(setting string) (BacklogPolicy, bool) {
	setting = strings.ToLower(strings.TrimSpace(setting))

	if setting == "" {

		return BacklogAbandon, true
	}

	policy, ok := backlogPolicyNames[setting]

	return policy, ok
}

// BacklogPolicyNames lists the accepted spellings, sorted, for an error message that names them.
func BacklogPolicyNames() []string {
	names := make([]string, 0, len(backlogPolicyNames))

	for name := range backlogPolicyNames {
		names = append(names, name)
	}

	sort.Strings(names)

	return names
}

// startCallbackDispatch launches the worker that posts queued deliveries, when a sink is configured
// and this is the instance that owns queue maintenance.
//
// It also enables the RECORDING, and that is one decision expressed at both ends rather than two:
// a store queues deliveries exactly when something is going to send them. Keeping it in one function
// is what stops a deployment writing a row per forgotten memory into a table nothing reads - the
// lesson db/outbox.go's SetSearchOutbox records, and sharper here, because a callback row can carry
// a memory body.
//
// The replica split is different from the outbox's, deliberately. A replica does NOT record: a
// callback is about forgetting, forgetting happens on the consolidating instance, and a replica's
// own deletions are client-initiated ones the client already knows about. Recording them on every
// replica would multiply a widened feed by the number of replicas with nothing to drain them.
func (s *Server) startCallbackDispatch(notifier notify.Notifier) {
	log.Trace("func() startCallbackDispatch")

	s.notifier = notifier

	s.callbackBounds = db.QueueBounds{
		MaxAge:   time.Duration(viper.GetInt("callbacks.maxAgeHours")) * time.Hour,
		MaxRows:  int64(viper.GetInt("callbacks.maxRows")),
		MaxBytes: viper.GetInt64("callbacks.maxBytes"),
	}

	s.callbackBatchSize = viper.GetInt("callbacks.batchSize")
	s.callbackBaseBack = time.Duration(viper.GetInt("callbacks.retryBaseBackoffSeconds")) * time.Second
	s.callbackMaxBack = time.Duration(viper.GetInt("callbacks.retryMaxBackoffSeconds")) * time.Second
	s.callbackChunkIds = viper.GetInt("callbacks.maxIdsPerDelivery")
	s.callbackSleepEvents = viper.GetBool("callbacks.events.sleepCompleted")
	s.callbackAtRiskEvents = viper.GetBool("callbacks.events.memoriesAtRisk")
	s.callbackAtRiskLimit = viper.GetInt("callbacks.atRiskLimit")
	s.callbackAtRiskMargin = viper.GetFloat64("callbacks.atRiskMargin")

	// An unknown spelling has already failed startup (validateCallbackConfig), so this cannot
	// silently choose a policy - which for a key whose values differ in what they are willing to lose
	// is the one failure mode worth ruling out.
	s.callbackBacklogPolicy, _ = ParseBacklogPolicy(viper.GetString("callbacks.backlogPolicy"))

	if s.callbackBounds.MaxRows <= 0 {
		s.callbackBounds.MaxRows = defaultCallbackMaxRows
	}

	if s.callbackBounds.MaxAge <= 0 {
		s.callbackBounds.MaxAge = defaultCallbackMaxAgeHours * time.Hour
	}

	// MaxBytes is deliberately NOT defaulted, unlike the two beside it. A byte cap discards
	// undelivered notifications, and how many bytes of them a deployment can afford is a property
	// of its disk rather than of this service - there is no figure that is right for both a laptop
	// store and a server. What stands in for a default is a startup warning where the shape is
	// dangerous (callbacks.includeBodies with no cap), which is validateCallbackConfig's job.

	if s.callbackBatchSize <= 0 {
		s.callbackBatchSize = defaultCallbackBatchSize
	}

	if s.callbackBaseBack <= 0 {
		s.callbackBaseBack = defaultCallbackBaseBackoff
	}

	if s.callbackMaxBack < s.callbackBaseBack {
		s.callbackMaxBack = defaultCallbackMaxBackoff
	}

	if s.callbackChunkIds <= 0 {
		s.callbackChunkIds = defaultCallbackMaxIdsPerChunk
	}

	// Left to db.PreviewLimit to normalise, which is what keeps this bound meaning the same thing
	// as the RPC's: a non-positive value takes the preview's own default and anything above its cap
	// is clamped to it.
	s.callbackAtRiskLimit = db.PreviewLimit(s.callbackAtRiskLimit)

	// A negative margin would lower the bar below the one in force, so the delivery would omit
	// memories the cycle is about to take - the one thing this kind must never do.
	if s.callbackAtRiskMargin < 0 {
		s.callbackAtRiskMargin = 0
	}

	if notifier == nil || !notifier.Enabled() {

		return
	}

	// Only the concrete store can be told: this is storage configuration, not part of what the Store
	// interface promises - the seam SetSearchOutbox and SetMemoryDeleteObserver already use.
	store, ok := s.db.(interface{ SetCallbackPolicy(db.CallbackPolicy) })
	if !ok {

		return
	}

	if !s.consolidationEnabled {
		log.Info("callbacks: this instance does not consolidate, so it neither records nor dispatches")

		return
	}

	store.SetCallbackPolicy(db.CallbackPolicy{
		Enabled:         true,
		AllDeletions:    viper.GetBool("callbacks.allDeletions"),
		IncludeBodies:   viper.GetBool("callbacks.includeBodies"),
		MaxBodyBytes:    viper.GetInt("callbacks.maxBodyBytes"),
		RetainDeletions: s.callbackBacklogPolicy.retains(),
		MemoryEvents:    viper.GetBool("callbacks.events.memoryForgotten"),
		EventEvents:     viper.GetBool("callbacks.events.eventForgotten"),
	})

	s.callbacksEnabled = true

	s.stopCallbacks = make(chan struct{})
	s.callbacksStopped = make(chan struct{})

	go s.callbackDispatchLoop()
}

// callbackDispatchLoop posts queued deliveries until stopped.
func (s *Server) callbackDispatchLoop() {
	defer close(s.callbacksStopped)

	log.Infof(
		"callback dispatch enabled: caps %d rows / %s / %s, batches of %d, backlog policy %s",
		s.callbackBounds.MaxRows,
		s.callbackBounds.MaxAge,
		describeByteCap(s.callbackBounds.MaxBytes),
		s.callbackBatchSize,
		s.callbackBacklogPolicy,
	)

	for {
		sent := s.dispatchCallbacksOnce(s.notifier)

		delay := callbackIdleDelay

		if sent > 0 {
			delay = callbackBusyDelay
		}

		select {

		case <-s.stopCallbacks:

			return

		case <-time.After(delay):
		}
	}
}

// dispatchCallbacksOnce sends one batch of queued deliveries, returning how many landed.
//
// Claim, send, THEN confirm. A row is removed only once the receiver has accepted the delivery, so a
// crash between the two replays the work rather than losing it - at-least-once, which is the honest
// guarantee for an HTTP endpoint that may have processed a request whose response never arrived.
//
// A failed delivery is DEFERRED rather than dropped: its attempt count rises and its next attempt
// moves out on a jittered exponential backoff, up to a ceiling. Nothing is abandoned on an attempt
// count - the row and age caps are the only thing that removes an undelivered row, which is one
// policy for "how long do we keep trying" instead of two that can disagree.
func (s *Server) dispatchCallbacksOnce(notifier notify.Notifier) int {
	if notifier == nil || !notifier.Enabled() {
		return 0
	}

	ctx := context.Background()

	claimed, err := s.db.ClaimCallbacks(ctx, s.callbackBatchSize, time.Now().UnixNano())
	if err != nil {
		log.Warnf("callbacks: failed to claim queued deliveries: %s", err.Error())

		return 0
	}

	if len(claimed) == 0 {
		s.pruneCallbackQueue(ctx)
		s.recordCallbackDepth(ctx)

		return 0
	}

	var (
		delivered []int64
		deferred  []int64
		attempts  int
	)

DELIVERIES:
	for _, entry := range claimed {

		// Checked between deliveries as well as between passes: a batch of a hundred against a slow
		// receiver would otherwise hold shutdown open for as many timeouts.
		select {

		case <-s.stopCallbacks:
			break DELIVERIES

		default:
		}

		if s.deliverOne(ctx, notifier, entry) {
			delivered = append(delivered, entry.Seq)

			continue
		}

		deferred = append(deferred, entry.Seq)
		attempts = max(attempts, entry.Attempts+1)
	}

	if len(delivered) > 0 {
		if err := s.db.ConfirmCallbacks(ctx, delivered); err != nil {
			// The deliveries landed; only the record of it did not. They will be sent again, which
			// is exactly the at-least-once contract, so this is a warning rather than a failure.
			log.Warnf("callbacks: failed to confirm %d delivered callbacks: %s", len(delivered), err.Error())
		}
	}

	if len(deferred) > 0 {
		if err := s.db.DeferCallbacks(ctx, deferred, time.Now().Add(s.callbackBackoff(attempts)).UnixNano()); err != nil {
			log.Warnf("callbacks: failed to defer %d callbacks: %s", len(deferred), err.Error())
		}
	}

	return len(delivered)
}

// deliverOne sends a single delivery, recording the outcome. It reports whether the receiver
// accepted it.
func (s *Server) deliverOne(ctx context.Context, notifier notify.Notifier, entry db.CallbackDelivery) bool {
	kind := notifyKind(entry.Kind)

	started := time.Now()

	err := notifier.Deliver(ctx, notify.Delivery{
		Kind:     kind,
		Cause:    notifyCause(entry.Cause),
		QueuedAt: entry.QueuedAt,
		CycleId:  entry.CycleId,
		Chunk:    entry.Chunk,
		Chunks:   entry.Chunks,
		Items:    notifyItems(entry.Payload.Items),
		Cycle:    notifyCycle(entry.Payload.Cycle),
		AtRisk:   notifyAtRisk(entry.Payload.AtRisk),
	})

	outcome := "ok"

	if err != nil {
		outcome = "failed"

		log.Warnf("callbacks: delivery %d (%s, attempt %d) failed: %s", entry.Seq, kind, entry.Attempts+1, err.Error())
	}

	tel.callbacksDelivered.Add(ctx, 1, metric.WithAttributes(
		attribute.String("kind", string(kind)),
		attribute.String("outcome", outcome),
	))

	tel.callbackDuration.Record(ctx, time.Since(started).Seconds(), metric.WithAttributes(
		attribute.String("outcome", outcome),
	))

	return err == nil
}

// callbackBackoff is the wait before the next attempt: exponential in the attempt count, jittered so
// a batch that failed together does not retry in lockstep, and capped.
func (s *Server) callbackBackoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}

	// Bounded before shifting: 1 << 62 is already past any sane ceiling, and shifting further
	// overflows into a negative duration, which would make the next attempt due in the past.
	shift := min(attempt-1, 16)

	backoff := s.callbackBaseBack << shift

	if backoff > s.callbackMaxBack || backoff <= 0 {
		backoff = s.callbackMaxBack
	}

	return backoff + time.Duration(rand.Int63n(int64(s.callbackBaseBack)))
}

// pruneCallbackQueue applies the caps, on the idle path only so it never races the dispatcher.
//
// Loud, unlike the outbox's equivalent. A pruned index deletion is picked up by the stale sweep; a
// pruned callback is a notification that nobody will ever receive, and an operator whose receiver
// has been down long enough for that to happen has genuinely lost data about their own store.
func (s *Server) pruneCallbackQueue(ctx context.Context) {
	pruned, err := s.db.PruneCallbackQueue(ctx, s.callbackBounds)
	if err != nil {
		log.Warnf("callbacks: failed to prune the queue: %s", err.Error())

		return
	}

	if pruned == 0 {
		return
	}

	log.Warnf(
		"callbacks: abandoned %d undelivered callbacks past the queue's caps (%d rows / %s / %s) - "+
			"these notifications are lost; check that the receiver at callbacks.url is reachable",
		pruned,
		s.callbackBounds.MaxRows,
		s.callbackBounds.MaxAge,
		describeByteCap(s.callbackBounds.MaxBytes),
	)

	tel.callbacksAbandoned.Add(ctx, pruned)
}

// recordCallbackDepth publishes the backlog gauge, on the idle path for the same reason the outbox's
// is: it is a count, and paying for one on every busy pass would slow the drain it is measuring.
func (s *Server) recordCallbackDepth(ctx context.Context) {
	depth, err := s.db.CallbackQueueDepth(ctx)
	if err != nil {
		log.Debugf("callbacks: failed to read the queue depth: %s", err.Error())

		return
	}

	tel.callbackQueueDepth.Record(ctx, depth)
}

// queueCycleCallback records what a finished sleep cycle forgot, chunked so that one cycle's id list
// never becomes one unbounded HTTP request.
//
// Every chunk carries the same cycle id and the full cycle summary, numbered from 1, so a receiver
// can tell a partial view from a complete one and can assemble the whole without holding state. A
// cycle that forgot nothing still reports, because "the cycle ran and took nothing" is information -
// it is what a store at rest looks like, and its absence is indistinguishable from a consolidator
// that has stopped.
//
// Best-effort throughout: a failure here is logged and never fails the cycle, which has already
// finished and already published its report.
func (s *Server) queueCycleCallback(ctx context.Context, cycleId int64, report *cycleReport) {
	memoryIds, eventIds, memoryMore, eventMore := s.db.EndCallbackCycle()

	if !s.callbacksEnabled || !s.callbackSleepEvents {
		return
	}

	if memoryMore > 0 || eventMore > 0 {
		log.Warnf(
			"callbacks: the sleep cycle's id list was truncated (%d memory and %d event ids beyond the bound); "+
				"the completion callbacks report the counts in full",
			memoryMore,
			eventMore,
		)
	}

	summary := &db.CallbackCycle{
		Trigger:                 report.trigger,
		StartedAt:               report.startedAt.UnixNano(),
		DurationMillis:          report.duration.Milliseconds(),
		MemoriesConsolidated:    report.memoriesConsolidated,
		EventsConsolidated:      report.eventsConsolidated,
		MemoriesEvicted:         report.memoriesEvicted,
		EventsEvicted:           report.eventsEvicted,
		BytesFreed:              report.bytesFreed,
		SummarisationCandidates: report.summarisationCandidates,
		Stalled:                 report.stalled,
		StalledReason:           report.stalledReason,
		Success:                 report.success,
		Failure:                 report.failure,
	}

	deliveries := cycleDeliveries(cycleId, summary, memoryIds, eventIds, s.callbackChunkIds)

	if err := s.db.QueueCallbacks(ctx, deliveries); err != nil {
		log.Warnf("callbacks: failed to queue the sleep-cycle callback: %s", err.Error())
	}
}

// cycleDeliveries splits one cycle's ids into chunks and builds a delivery for each.
//
// Memory ids and event ids are chunked together into one stream rather than into two, so a receiver
// reassembling a cycle has one sequence to follow rather than two that must be correlated. Every
// chunk carries the cycle summary, which costs a few dozen bytes and means a receiver that drops one
// chunk still knows what the cycle did.
func cycleDeliveries(
	cycleId int64,
	summary *db.CallbackCycle,
	memoryIds []string,
	eventIds []string,
	perChunk int,
) []db.CallbackDelivery {
	if perChunk <= 0 {
		perChunk = defaultCallbackMaxIdsPerChunk
	}

	items := make([]db.CallbackItem, 0, len(memoryIds)+len(eventIds))

	for _, id := range memoryIds {
		items = append(items, db.CallbackItem{Id: id})
	}

	// Event ids are marked by carrying themselves as the event, which is what tells a receiver the
	// id names an event rather than a memory without a second list to keep in step.
	for _, id := range eventIds {
		items = append(items, db.CallbackItem{Id: id, EventId: id})
	}

	chunks := max((len(items)+perChunk-1)/perChunk, 1)

	deliveries := make([]db.CallbackDelivery, 0, chunks)

	for i := range chunks {
		start := i * perChunk
		end := min(start+perChunk, len(items))

		var chunk []db.CallbackItem

		if start < len(items) {
			chunk = items[start:end]
		}

		deliveries = append(deliveries, db.CallbackDelivery{
			Kind:      db.CallbackKindSleepCompleted,
			CycleId:   cycleId,
			Chunk:     i + 1,
			Chunks:    chunks,
			ItemCount: len(chunk),
			Payload:   db.CallbackPayload{Items: chunk, Cycle: summary},
		})
	}

	return deliveries
}

// The projections from the storage layer's types onto the wire's.
//
// Written out rather than shared, on the rule the MCP bridge and the --schema-version command
// already follow: the delivery shape is the callback's contract with whatever receives it, and the
// storage layer should not acquire one by being marshalled.

func notifyKind(kind db.CallbackKind) notify.Kind {
	switch kind {

	case db.CallbackKindMemoryForgotten:
		return notify.KindMemoryForgotten

	case db.CallbackKindEventForgotten:
		return notify.KindEventForgotten

	case db.CallbackKindSleepCompleted:
		return notify.KindSleepCompleted

	case db.CallbackKindMemoriesAtRisk:
		return notify.KindMemoriesAtRisk

	}

	return ""
}

func notifyCause(cause db.DeleteCause) notify.Cause {
	switch cause {

	case db.CauseConsolidation:
		return notify.CauseConsolidation

	case db.CauseEviction:
		return notify.CauseEviction

	case db.CauseClient:
		return notify.CauseClient

	case db.CauseClear:
		return notify.CauseClear

	case db.CauseCascade:
		return notify.CauseCascade

	case db.CauseSummaryReplace:
		return notify.CauseSummaryReplace

	case db.CausePurge:
		return notify.CausePurge

	}

	return ""
}

func notifyItems(items []db.CallbackItem) []notify.Item {
	if len(items) == 0 {
		return nil
	}

	out := make([]notify.Item, 0, len(items))

	for _, in := range items {
		out = append(out, notify.Item{
			Id:           in.Id,
			EventId:      in.EventId,
			Group:        in.Group,
			Significance: in.Significance,
			Bytes:        in.Bytes,
			Value:        in.Value,
			Body:         in.Body,
			BodyOmitted:  in.BodyOmitted,
		})
	}

	return out
}

func notifyAtRisk(in *db.CallbackAtRisk) *notify.AtRisk {
	if in == nil {
		return nil
	}

	return &notify.AtRisk{
		Consolidating:    in.Consolidating,
		Evicting:         in.Evicting,
		Events:           in.Events,
		Bytes:            in.Bytes,
		Threshold:        in.Threshold,
		AtRiskThreshold:  in.AtRiskThreshold,
		CapacityPressure: in.CapacityPressure,
		Truncated:        in.Truncated,
	}
}

func notifyCycle(in *db.CallbackCycle) *notify.Cycle {
	if in == nil {
		return nil
	}

	return &notify.Cycle{
		Trigger:                 in.Trigger,
		StartedAt:               in.StartedAt,
		DurationMillis:          in.DurationMillis,
		MemoriesConsolidated:    in.MemoriesConsolidated,
		EventsConsolidated:      in.EventsConsolidated,
		MemoriesEvicted:         in.MemoriesEvicted,
		EventsEvicted:           in.EventsEvicted,
		BytesFreed:              in.BytesFreed,
		SummarisationCandidates: in.SummarisationCandidates,
		Stalled:                 in.Stalled,
		StalledReason:           in.StalledReason,
		Success:                 in.Success,
		Failure:                 in.Failure,
	}
}

// forgettingStalled reports whether the two decay passes should be held off this cycle, and why.
//
// It is the bound that replaces the caps under BacklogStall. Retaining deletion deliveries removes
// the only thing that was trimming the queue, so without this the policy would trade a silent
// unbounded leak in the far system for a silent unbounded one on this disk - which is the same
// failure wearing a different hat. Stalling instead converts it into the loudest thing a store like
// this can say: it stops forgetting.
//
// Judged on the same three figures the caps are expressed in, so an operator who set a row cap is
// stalled by rows and one who set a byte cap by bytes. A cap left unset bounds nothing here either -
// there is no figure this service could invent that would be right for both a laptop store and a
// server, which is why validateCallbackConfig refuses BacklogStall with all three caps unset rather
// than defaulting one.
//
// Only the two DECAY passes are held. A client's own DeleteMemories still deletes: the caller
// already knows what they asked for, and refusing it would make somebody else's request fail for a
// receiver outage they have nothing to do with. Summarisation is untouched for the same reason - it
// replaces memories rather than forgetting them.
func (s *Server) forgettingStalled(ctx context.Context) (bool, string) {
	if s.callbackBacklogPolicy != BacklogStall || !s.callbacksEnabled {

		return false, ""
	}

	backlog, err := s.db.CallbackBacklog(ctx)
	if err != nil {
		// Not a stall: a cycle held off because the check itself failed would stop the store
		// forgetting for a reason that has nothing to do with the backlog, and the fault is already
		// logged. The policy protects against a receiver that is down, not against this store's own
		// storage being unreadable - which has larger problems.
		log.Warnf("callbacks: failed to measure the retained backlog, not stalling this cycle: %s", err.Error())

		return false, ""
	}

	if backlog.Rows == 0 {

		return false, ""
	}

	bounds := s.callbackBounds

	switch {

	case bounds.MaxRows > 0 && backlog.Rows > bounds.MaxRows:

		return true, fmt.Sprintf(
			"the outbound callback queue holds %d undelivered forget-callbacks, past callbacks.maxRows (%d)",
			backlog.Rows,
			bounds.MaxRows,
		)

	case bounds.MaxBytes > 0 && backlog.Bytes > bounds.MaxBytes:

		return true, fmt.Sprintf(
			"the outbound callback queue holds %d bytes of undelivered forget-callbacks, past callbacks.maxBytes (%d)",
			backlog.Bytes,
			bounds.MaxBytes,
		)

	case bounds.MaxAge > 0 && backlog.OldestAt > 0 && time.Since(time.Unix(0, backlog.OldestAt)) > bounds.MaxAge:

		return true, fmt.Sprintf(
			"the oldest undelivered forget-callback has been queued for %s, past callbacks.maxAgeHours (%s)",
			time.Since(time.Unix(0, backlog.OldestAt)).Round(time.Second),
			bounds.MaxAge,
		)

	}

	return false, ""
}
