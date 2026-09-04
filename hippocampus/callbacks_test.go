package hippocampus

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/fastbean-au/hippocampus/db"
	"github.com/fastbean-au/hippocampus/notify"
	"github.com/fastbean-au/hippocampus/types"
)

// recordingNotifier is a sink that remembers what it was handed and can be made to fail. It stands
// in for a receiver, which is what lets the dispatcher be driven a pass at a time with no timing.
type recordingNotifier struct {
	mu        sync.Mutex
	delivered []notify.Delivery
	fail      error
	disabled  bool
}

func (n *recordingNotifier) Enabled() bool {
	return !n.disabled
}

func (n *recordingNotifier) Deliver(ctx context.Context, delivery notify.Delivery) error {
	n.mu.Lock()
	defer n.mu.Unlock()

	if n.fail != nil {
		return n.fail
	}

	n.delivered = append(n.delivered, delivery)

	return nil
}

func (n *recordingNotifier) taken() []notify.Delivery {
	n.mu.Lock()
	defer n.mu.Unlock()

	out := make([]notify.Delivery, len(n.delivered))
	copy(out, n.delivered)

	return out
}

func (n *recordingNotifier) failWith(err error) {
	n.mu.Lock()
	defer n.mu.Unlock()

	n.fail = err
}

// callbackServer builds a Server over an in-memory store with callbacks recording, mirroring
// outboxServer: the loop is never started, so every test drives dispatchCallbacksOnce directly and
// asserts on what it returns rather than on how long it waited.
func callbackServer(t *testing.T, policy db.CallbackPolicy) (*Server, *db.DB) {
	t.Helper()

	database, err := db.New("")
	if err != nil {
		t.Fatalf("db.New: %s", err)
	}

	t.Cleanup(func() { _ = database.Close() })

	database.SetCallbackPolicy(policy)

	return &Server{
		db:                  database,
		callbacksEnabled:    policy.Enabled,
		callbackMaxRows:     1000,
		callbackMaxAge:      time.Hour,
		callbackBatchSize:   10,
		callbackBaseBack:    time.Second,
		callbackMaxBack:     time.Minute,
		callbackChunkIds:    3,
		callbackSleepEvents: true,
		stopCallbacks:       make(chan struct{}),
	}, database
}

func recordingPolicy() db.CallbackPolicy {
	return db.CallbackPolicy{Enabled: true, MemoryEvents: true, EventEvents: true}
}

func storeMemories(t *testing.T, database *db.DB, ids ...string) {
	t.Helper()

	for _, id := range ids {
		if _, err := database.CreateMemory(context.Background(), types.Memory{
			Id: id, Body: "body of " + id, Significance: 10, Group: "svc-a", TimeStamp: time.Now().UnixNano(),
		}); err != nil {
			t.Fatalf("CreateMemory(%s): %s", id, err)
		}
	}
}

// TestDispatchSendsQueuedDeliveries is the mechanism end-to-end: a decay deletion records a delivery
// in the same transaction, and the dispatcher sends it and removes it from the queue.
func TestDispatchSendsQueuedDeliveries(t *testing.T) {
	s, database := callbackServer(t, recordingPolicy())
	ctx := context.Background()

	storeMemories(t, database, "d1", "d2")

	if _, err := database.ClearMemories(ctx, []db.MemoryRecallSnapshot{{Id: "d1"}, {Id: "d2"}}); err != nil {
		t.Fatalf("ClearMemories: %s", err)
	}

	// Clear is not a decay cause, so the narrow default says nothing about it.
	if sent := s.dispatchCallbacksOnce(&recordingNotifier{}); sent != 0 {
		t.Fatalf("a Clear was announced under the narrow default (%d deliveries)", sent)
	}

	storeMemories(t, database, "c1", "c2")

	// A real decay deletion.
	if _, _, _, err := database.EvictMemories(ctx, &evictAllServer{}, 1<<20); err != nil {
		t.Fatalf("EvictMemories: %s", err)
	}

	sink := &recordingNotifier{}

	sent := s.dispatchCallbacksOnce(sink)
	if sent == 0 {
		t.Fatal("the dispatcher sent nothing for an eviction")
	}

	delivered := sink.taken()

	if len(delivered) != sent {
		t.Fatalf("the dispatcher reported %d sent but the receiver saw %d", sent, len(delivered))
	}

	if delivered[0].Kind != notify.KindMemoryForgotten || delivered[0].Cause != notify.CauseEviction {
		t.Errorf("the delivery describes itself wrongly: %+v", delivered[0])
	}

	// Confirmed, so the queue is empty and a second pass sends nothing.
	if again := s.dispatchCallbacksOnce(sink); again != 0 {
		t.Errorf("a confirmed delivery was sent again (%d)", again)
	}
}

// evictAllServer evicts whatever it is shown, which is enough to drive one real decay deletion.
type evictAllServer struct{}

func (evictAllServer) ShouldConsolidateMemory(db.MemoryConsolidationCandidate) bool { return false }
func (evictAllServer) ShouldConsolidateEvent(db.EventConsolidationCandidate) bool   { return false }
func (evictAllServer) MemoryValue(db.MemoryConsolidationCandidate) float64          { return 1 }
func (evictAllServer) MemoryRetained(db.MemoryConsolidationCandidate) bool          { return false }
func (evictAllServer) DeletionThreshold() float64                                   { return 0 }

// TestAFailedDeliveryIsDeferredNotDropped is the durability property: a receiver that refuses leaves
// the delivery in the queue with a later deadline, rather than losing it.
func TestAFailedDeliveryIsDeferredNotDropped(t *testing.T) {
	s, database := callbackServer(t, recordingPolicy())
	ctx := context.Background()

	if err := database.QueueCallbacks(ctx, []db.CallbackDelivery{{
		Kind:      db.CallbackKindMemoryForgotten,
		Cause:     db.CauseConsolidation,
		ItemCount: 1,
		Payload:   db.CallbackPayload{Items: []db.CallbackItem{{Id: "m1"}}},
	}}); err != nil {
		t.Fatalf("QueueCallbacks: %s", err)
	}

	sink := &recordingNotifier{}
	sink.failWith(errors.New("receiver said no"))

	if sent := s.dispatchCallbacksOnce(sink); sent != 0 {
		t.Fatalf("a refused delivery was reported as sent (%d)", sent)
	}

	// Still queued.
	depth, err := database.CallbackQueueDepth(ctx)
	if err != nil {
		t.Fatalf("CallbackQueueDepth: %s", err)
	}

	if depth != 1 {
		t.Fatalf("depth after a refusal is %d, want 1 - the delivery was dropped", depth)
	}

	// And deferred: not due now, but due after the backoff.
	due, err := database.ClaimCallbacks(ctx, 10, time.Now().UnixNano())
	if err != nil {
		t.Fatalf("ClaimCallbacks: %s", err)
	}

	if len(due) != 0 {
		t.Errorf("a refused delivery is immediately due again - the dispatcher would hammer the receiver")
	}

	later, err := database.ClaimCallbacks(ctx, 10, time.Now().Add(time.Hour).UnixNano())
	if err != nil {
		t.Fatalf("ClaimCallbacks (later): %s", err)
	}

	if len(later) != 1 || later[0].Attempts != 1 {
		t.Fatalf("after the backoff the delivery is %+v, want one attempt recorded", later)
	}

	// It succeeds on the retry and leaves.
	sink.failWith(nil)

	// Bring the deadline forward so the retry is due, as the passage of time would.
	if err := database.DeferCallbacks(ctx, []int64{later[0].Seq}, 1); err != nil {
		t.Fatalf("DeferCallbacks: %s", err)
	}

	if sent := s.dispatchCallbacksOnce(sink); sent != 1 {
		t.Fatalf("the retry sent %d, want 1", sent)
	}

	if depth, _ := database.CallbackQueueDepth(ctx); depth != 0 {
		t.Errorf("a delivered callback stayed queued (depth %d)", depth)
	}
}

// TestBackoffGrowsAndIsCapped covers the retry curve, including the shift bound - without it a high
// attempt count overflows into a negative duration and makes the next attempt due in the past.
func TestBackoffGrowsAndIsCapped(t *testing.T) {
	s := &Server{callbackBaseBack: time.Second, callbackMaxBack: time.Minute}

	first := s.callbackBackoff(1)
	second := s.callbackBackoff(2)

	if first < time.Second || first >= 2*time.Second {
		t.Errorf("the first backoff is %s, want one base plus jitter", first)
	}

	if second <= first {
		t.Errorf("the backoff did not grow: %s then %s", first, second)
	}

	for _, attempt := range []int{0, 1, 8, 40, 1000, 1 << 30} {
		got := s.callbackBackoff(attempt)

		if got <= 0 {
			t.Errorf("callbackBackoff(%d) = %s - a non-positive backoff makes the retry due in the past", attempt, got)
		}

		if got > s.callbackMaxBack+s.callbackBaseBack {
			t.Errorf("callbackBackoff(%d) = %s, above the ceiling plus jitter", attempt, got)
		}
	}
}

// TestDispatchPrunesOnTheIdlePathOnly pins that the caps are applied when there is nothing to send,
// so pruning never races the dispatcher over the rows it is about to claim.
func TestDispatchPrunesOnTheIdlePathOnly(t *testing.T) {
	s, database := callbackServer(t, recordingPolicy())
	ctx := context.Background()

	s.callbackMaxAge = time.Hour

	if err := database.QueueCallbacks(ctx, []db.CallbackDelivery{{
		Kind:      db.CallbackKindMemoryForgotten,
		Cause:     db.CauseConsolidation,
		QueuedAt:  time.Now().Add(-48 * time.Hour).UnixNano(),
		ItemCount: 1,
		Payload:   db.CallbackPayload{Items: []db.CallbackItem{{Id: "stale"}}},
	}}); err != nil {
		t.Fatalf("QueueCallbacks: %s", err)
	}

	// The stale row is claimable, so this pass is busy and prunes nothing.
	sink := &recordingNotifier{}

	if sent := s.dispatchCallbacksOnce(sink); sent != 1 {
		t.Fatalf("the busy pass sent %d, want 1", sent)
	}

	// Now idle: the caps run.
	if err := database.QueueCallbacks(ctx, []db.CallbackDelivery{{
		Kind:      db.CallbackKindMemoryForgotten,
		Cause:     db.CauseConsolidation,
		QueuedAt:  time.Now().Add(-48 * time.Hour).UnixNano(),
		ItemCount: 1,
		Payload:   db.CallbackPayload{Items: []db.CallbackItem{{Id: "stale2"}}},
	}}); err != nil {
		t.Fatalf("QueueCallbacks: %s", err)
	}

	// Deferred far out so the pass finds nothing due and takes the idle path.
	claimed, _ := database.ClaimCallbacks(ctx, 10, time.Now().UnixNano())
	if err := database.DeferCallbacks(ctx, []int64{claimed[0].Seq}, time.Now().Add(time.Hour).UnixNano()); err != nil {
		t.Fatalf("DeferCallbacks: %s", err)
	}

	if sent := s.dispatchCallbacksOnce(sink); sent != 0 {
		t.Fatalf("the idle pass sent %d, want 0", sent)
	}

	if depth, _ := database.CallbackQueueDepth(ctx); depth != 0 {
		t.Errorf("the age cap left %d stale deliveries", depth)
	}
}

// TestDispatchIsANoOpWithoutASink pins that a disabled notifier claims nothing, so a deployment that
// has turned callbacks off does not poll a table it will never drain.
func TestDispatchIsANoOpWithoutASink(t *testing.T) {
	s, _ := callbackServer(t, recordingPolicy())

	if sent := s.dispatchCallbacksOnce(nil); sent != 0 {
		t.Errorf("a nil notifier sent %d", sent)
	}

	if sent := s.dispatchCallbacksOnce(&recordingNotifier{disabled: true}); sent != 0 {
		t.Errorf("a disabled notifier sent %d", sent)
	}
}

// TestCycleDeliveriesChunkTheIdList is the sleep-completion shape: one cycle becomes several
// numbered deliveries sharing an id, each repeating the summary.
func TestCycleDeliveriesChunkTheIdList(t *testing.T) {
	summary := &db.CallbackCycle{Trigger: "timer", MemoriesConsolidated: 5, Success: true}

	deliveries := cycleDeliveries(42, summary, []string{"m1", "m2", "m3", "m4", "m5"}, []string{"e1"}, 2)

	if len(deliveries) != 3 {
		t.Fatalf("six ids at two per chunk produced %d deliveries, want 3", len(deliveries))
	}

	var seen []string

	for i, delivery := range deliveries {
		if delivery.Kind != db.CallbackKindSleepCompleted || delivery.CycleId != 42 {
			t.Errorf("chunk %d describes itself wrongly: %+v", i, delivery)
		}

		if delivery.Chunk != i+1 || delivery.Chunks != 3 {
			t.Errorf("chunk %d is numbered %d of %d", i, delivery.Chunk, delivery.Chunks)
		}

		// Every chunk repeats the summary, so a receiver that misses one still knows what the cycle
		// did.
		if delivery.Payload.Cycle == nil || delivery.Payload.Cycle.MemoriesConsolidated != 5 {
			t.Errorf("chunk %d lost the cycle summary", i)
		}

		for _, item := range delivery.Payload.Items {
			seen = append(seen, item.Id)
		}
	}

	if len(seen) != 6 {
		t.Errorf("the chunks carry %d ids between them, want 6", len(seen))
	}

	// An event id is marked by carrying itself as the event, which is how a receiver tells the two
	// apart without a second list to keep in step.
	last := deliveries[2].Payload.Items[len(deliveries[2].Payload.Items)-1]

	if last.Id != "e1" || last.EventId != "e1" {
		t.Errorf("the event id is not marked as one: %+v", last)
	}
}

// TestCycleDeliveriesAlwaysReportsOnce pins that a cycle which forgot nothing still produces a
// delivery: "the cycle ran and took nothing" is what a store at rest looks like, and its absence is
// indistinguishable from a consolidator that has stopped.
func TestCycleDeliveriesAlwaysReportsOnce(t *testing.T) {
	deliveries := cycleDeliveries(7, &db.CallbackCycle{Trigger: "manual", Success: true}, nil, nil, 500)

	if len(deliveries) != 1 {
		t.Fatalf("an empty cycle produced %d deliveries, want 1", len(deliveries))
	}

	if deliveries[0].Chunk != 1 || deliveries[0].Chunks != 1 || deliveries[0].ItemCount != 0 {
		t.Errorf("the empty cycle's delivery is %+v", deliveries[0])
	}

	if deliveries[0].Payload.Cycle == nil || deliveries[0].Payload.Cycle.Trigger != "manual" {
		t.Error("the empty cycle's delivery lost its summary")
	}
}

// TestTheSleepCycleQueuesItsCompletion is the plumbing end-to-end: a cycle collects what it forgot
// and reports it under its own id, and the per-deletion deliveries carry the same id so a receiver
// can assemble the cycle.
func TestTheSleepCycleQueuesItsCompletion(t *testing.T) {
	s, database := callbackServer(t, recordingPolicy())
	ctx := context.Background()

	storeMemories(t, database, "s1", "s2")

	const cycleId = 99

	database.BeginCallbackCycle(cycleId)

	if _, _, _, err := database.EvictMemories(ctx, &evictAllServer{}, 1<<20); err != nil {
		t.Fatalf("EvictMemories: %s", err)
	}

	s.queueCycleCallback(ctx, cycleId, &cycleReport{
		trigger:         "timer",
		startedAt:       time.Now(),
		memoriesEvicted: 2,
		success:         true,
	})

	claimed, err := database.ClaimCallbacks(ctx, 100, time.Now().UnixNano())
	if err != nil {
		t.Fatalf("ClaimCallbacks: %s", err)
	}

	var (
		forgotten int
		completed int
		ids       []string
	)

	for _, delivery := range claimed {
		if delivery.CycleId != cycleId {
			t.Errorf("a delivery carries cycle %d, want %d", delivery.CycleId, cycleId)
		}

		switch delivery.Kind {

		case db.CallbackKindMemoryForgotten:
			forgotten++

		case db.CallbackKindSleepCompleted:
			completed++

			for _, item := range delivery.Payload.Items {
				ids = append(ids, item.Id)
			}

			if delivery.Payload.Cycle == nil || delivery.Payload.Cycle.MemoriesEvicted != 2 {
				t.Errorf("the completion lost its summary: %+v", delivery.Payload.Cycle)
			}

		}
	}

	if forgotten == 0 {
		t.Error("the eviction queued no memory-forgotten delivery")
	}

	if completed == 0 {
		t.Fatal("the cycle queued no completion delivery")
	}

	if len(ids) != 2 {
		t.Errorf("the completion carries %v, want both evicted ids", ids)
	}
}

// TestTheCycleCollectionIgnoresConcurrentClientDeletes is the property that makes the collection
// safe: only decay contributes, so a client's delete landing mid-cycle is never reported as
// something the cycle forgot.
func TestTheCycleCollectionIgnoresConcurrentClientDeletes(t *testing.T) {
	_, database := callbackServer(t, db.CallbackPolicy{
		Enabled: true, MemoryEvents: true, EventEvents: true, AllDeletions: true,
	})

	ctx := context.Background()

	storeMemories(t, database, "mine", "theirs")

	database.BeginCallbackCycle(5)

	if _, err := database.DeleteMemories(ctx, []string{"theirs"}); err != nil {
		t.Fatalf("DeleteMemories: %s", err)
	}

	memoryIds, eventIds, more, eventMore := database.EndCallbackCycle()

	if len(memoryIds) != 0 || len(eventIds) != 0 || more != 0 || eventMore != 0 {
		t.Errorf("a client delete was collected as the cycle's work: %v / %v", memoryIds, eventIds)
	}
}

// TestDispatchStopsPromptly pins that a batch against a slow receiver does not hold shutdown open
// for one timeout per delivery.
func TestDispatchStopsPromptly(t *testing.T) {
	s, database := callbackServer(t, recordingPolicy())
	ctx := context.Background()

	for range 5 {
		if err := database.QueueCallbacks(ctx, []db.CallbackDelivery{{
			Kind:      db.CallbackKindMemoryForgotten,
			Cause:     db.CauseConsolidation,
			ItemCount: 1,
			Payload:   db.CallbackPayload{Items: []db.CallbackItem{{Id: "m"}}},
		}}); err != nil {
			t.Fatalf("QueueCallbacks: %s", err)
		}
	}

	close(s.stopCallbacks)

	done := make(chan int, 1)

	go func() { done <- s.dispatchCallbacksOnce(&recordingNotifier{}) }()

	select {

	case sent := <-done:
		if sent != 0 {
			t.Errorf("a pass that saw the stop signal still sent %d deliveries", sent)
		}

	case <-time.After(5 * time.Second):
		t.Fatal("dispatchCallbacksOnce did not return after the stop signal; shutdown would hang")

	}
}
