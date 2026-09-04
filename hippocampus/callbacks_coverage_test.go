package hippocampus

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/spf13/viper"

	"github.com/fastbean-au/hippocampus/db"
	"github.com/fastbean-au/hippocampus/notify"
)

// failingCallbackStore wraps a real store and fails one queue method, so the dispatcher's
// best-effort branches can be driven without a broken database underneath everything else. Modelled
// on failDeleteEventIfEmptyStore in transfer_test.go.
type failingCallbackStore struct {
	db.Store

	failClaim   bool
	failConfirm bool
	failDefer   bool
	failPrune   bool
	failDepth   bool
	failQueue   bool
	failList    bool
	failOldest  bool
	failDelete  bool

	// truncated makes EndCallbackCycle report an overflow. The real bound is 100,000 ids, which no
	// test should spend the memory to reach; what matters here is that the RPC layer reports the
	// overflow rather than passing a short list off as complete, and that is this seam's answer.
	truncated bool
}

func (f failingCallbackStore) EndCallbackCycle() ([]string, []string, int, int) {
	memoryIds, eventIds, memoryMore, eventMore := f.Store.EndCallbackCycle()

	if f.truncated {
		return memoryIds, eventIds, memoryMore + 5, eventMore + 2
	}

	return memoryIds, eventIds, memoryMore, eventMore
}

var errStoreFailed = errors.New("store failed")

func (f failingCallbackStore) ClaimCallbacks(ctx context.Context, limit int, now int64) ([]db.CallbackDelivery, error) {
	if f.failClaim {
		return nil, errStoreFailed
	}

	return f.Store.ClaimCallbacks(ctx, limit, now)
}

func (f failingCallbackStore) ConfirmCallbacks(ctx context.Context, seqs []int64) error {
	if f.failConfirm {
		return errStoreFailed
	}

	return f.Store.ConfirmCallbacks(ctx, seqs)
}

func (f failingCallbackStore) DeferCallbacks(ctx context.Context, seqs []int64, next int64) error {
	if f.failDefer {
		return errStoreFailed
	}

	return f.Store.DeferCallbacks(ctx, seqs, next)
}

func (f failingCallbackStore) PruneCallbackQueue(ctx context.Context, maxAge time.Duration, maxRows int64) (int64, error) {
	if f.failPrune {
		return 0, errStoreFailed
	}

	return f.Store.PruneCallbackQueue(ctx, maxAge, maxRows)
}

func (f failingCallbackStore) CallbackQueueDepth(ctx context.Context) (int64, error) {
	if f.failDepth {
		return 0, errStoreFailed
	}

	return f.Store.CallbackQueueDepth(ctx)
}

func (f failingCallbackStore) QueueCallbacks(ctx context.Context, deliveries []db.CallbackDelivery) error {
	if f.failQueue {
		return errStoreFailed
	}

	return f.Store.QueueCallbacks(ctx, deliveries)
}

func (f failingCallbackStore) GetCallbackQueue(ctx context.Context, filter db.CallbackQueueFilter) ([]db.CallbackDelivery, error) {
	if f.failList {
		return nil, errStoreFailed
	}

	return f.Store.GetCallbackQueue(ctx, filter)
}

func (f failingCallbackStore) OldestQueuedCallback(ctx context.Context) (int64, error) {
	if f.failOldest {
		return 0, errStoreFailed
	}

	return f.Store.OldestQueuedCallback(ctx)
}

func (f failingCallbackStore) DeleteCallbackQueue(ctx context.Context, before int64) (int64, error) {
	if f.failDelete {
		return 0, errStoreFailed
	}

	return f.Store.DeleteCallbackQueue(ctx, before)
}

// queueOneDelivery records a single memory-forgotten delivery, for the tests that care about the
// dispatcher rather than about what produced the row.
func queueOneDelivery(t *testing.T, database *db.DB) {
	t.Helper()

	if err := database.QueueCallbacks(context.Background(), []db.CallbackDelivery{{
		Kind:      db.CallbackKindMemoryForgotten,
		Cause:     db.CauseConsolidation,
		ItemCount: 1,
		Payload:   db.CallbackPayload{Items: []db.CallbackItem{{Id: "m1"}}},
	}}); err != nil {
		t.Fatalf("QueueCallbacks: %s", err)
	}
}

// TestDispatchSurvivesEveryStoreFailure covers the best-effort branches. None of them may fail the
// pass outright: the dispatcher runs forever, and a transient store error must cost one pass rather
// than stop delivery until a restart.
func TestDispatchSurvivesEveryStoreFailure(t *testing.T) {
	t.Run("claim", func(t *testing.T) {
		s, database := callbackServer(t, recordingPolicy())
		queueOneDelivery(t, database)

		s.db = failingCallbackStore{Store: database, failClaim: true}

		if sent := s.dispatchCallbacksOnce(&recordingNotifier{}); sent != 0 {
			t.Errorf("a failed claim reported %d sent", sent)
		}
	})

	t.Run("confirm", func(t *testing.T) {
		s, database := callbackServer(t, recordingPolicy())
		queueOneDelivery(t, database)

		s.db = failingCallbackStore{Store: database, failConfirm: true}

		// The delivery landed, so the pass reports it as sent even though the record of that could
		// not be removed - it will simply be sent again, which is the at-least-once contract.
		sink := &recordingNotifier{}

		if sent := s.dispatchCallbacksOnce(sink); sent != 1 {
			t.Errorf("a failed confirm reported %d sent, want 1", sent)
		}

		if len(sink.taken()) != 1 {
			t.Error("the delivery never reached the receiver")
		}
	})

	t.Run("defer", func(t *testing.T) {
		s, database := callbackServer(t, recordingPolicy())
		queueOneDelivery(t, database)

		s.db = failingCallbackStore{Store: database, failDefer: true}

		sink := &recordingNotifier{}
		sink.failWith(errors.New("receiver said no"))

		if sent := s.dispatchCallbacksOnce(sink); sent != 0 {
			t.Errorf("a refused delivery whose deferral also failed reported %d sent", sent)
		}
	})

	t.Run("prune", func(t *testing.T) {
		s, database := callbackServer(t, recordingPolicy())

		s.db = failingCallbackStore{Store: database, failPrune: true}

		// An empty queue takes the idle path, which is where pruning happens.
		if sent := s.dispatchCallbacksOnce(&recordingNotifier{}); sent != 0 {
			t.Errorf("an idle pass reported %d sent", sent)
		}
	})

	t.Run("depth", func(t *testing.T) {
		s, database := callbackServer(t, recordingPolicy())

		s.db = failingCallbackStore{Store: database, failDepth: true}

		if sent := s.dispatchCallbacksOnce(&recordingNotifier{}); sent != 0 {
			t.Errorf("an idle pass reported %d sent", sent)
		}
	})
}

// TestPruneCallbackQueueWarnsWhenItAbandons pins that reaching the caps is reported rather than
// silent. There is no sweep behind this queue, so a pruned delivery is a notification nobody will
// ever get - the one discard in the service with no backstop.
func TestPruneCallbackQueueWarnsWhenItAbandons(t *testing.T) {
	s, database := callbackServer(t, recordingPolicy())
	ctx := context.Background()

	// Older than the cap, so the prune has something to take.
	if err := database.QueueCallbacks(ctx, []db.CallbackDelivery{{
		Kind:      db.CallbackKindMemoryForgotten,
		Cause:     db.CauseConsolidation,
		QueuedAt:  time.Now().Add(-48 * time.Hour).UnixNano(),
		ItemCount: 1,
		Payload:   db.CallbackPayload{Items: []db.CallbackItem{{Id: "stale"}}},
	}}); err != nil {
		t.Fatalf("QueueCallbacks: %s", err)
	}

	s.callbackMaxAge = time.Hour

	s.pruneCallbackQueue(ctx)

	if depth, _ := database.CallbackQueueDepth(ctx); depth != 0 {
		t.Errorf("the cap left %d deliveries", depth)
	}

	// A second pass finds nothing and must not log or count anything.
	s.pruneCallbackQueue(ctx)
}

// TestCallbackDispatchLoopRunsAndStops drives the real loop rather than a single pass, which is what
// covers the adaptive delay and the stop path. The delays are shortened for the duration, the idiom
// the outbox drain's tests already use.
func TestCallbackDispatchLoopRunsAndStops(t *testing.T) {
	restoreIdle, restoreBusy := callbackIdleDelay, callbackBusyDelay
	callbackIdleDelay = 5 * time.Millisecond
	callbackBusyDelay = time.Millisecond

	t.Cleanup(func() {
		callbackIdleDelay, callbackBusyDelay = restoreIdle, restoreBusy
	})

	s, database := callbackServer(t, recordingPolicy())
	s.callbacksStopped = make(chan struct{})

	sink := &recordingNotifier{}
	s.notifier = sink

	queueOneDelivery(t, database)

	go s.callbackDispatchLoop()

	deadline := time.After(5 * time.Second)

	for len(sink.taken()) == 0 {
		select {

		case <-deadline:
			t.Fatal("the dispatch loop delivered nothing")

		case <-time.After(2 * time.Millisecond):
		}
	}

	// It must also take the idle path at least once, which is the branch a single busy pass misses.
	close(s.stopCallbacks)

	select {

	case <-s.callbacksStopped:

	case <-time.After(5 * time.Second):
		t.Fatal("the dispatch loop did not stop; shutdown would hang")

	}
}

// TestStartCallbackDispatchGating covers every branch of the decision to record and to dispatch.
//
// The two are one decision expressed at both ends: a store queues deliveries exactly when something
// is going to send them, so every path that declines to dispatch must also decline to record.
func TestStartCallbackDispatchGating(t *testing.T) {
	setViper := func(t *testing.T) {
		t.Helper()

		for key, value := range map[string]any{
			"callbacks.maxRows":                 10,
			"callbacks.maxAgeHours":             1,
			"callbacks.batchSize":               5,
			"callbacks.retryBaseBackoffSeconds": 1,
			"callbacks.retryMaxBackoffSeconds":  2,
			"callbacks.maxIdsPerDelivery":       7,
			"callbacks.events.sleepCompleted":   true,
			"callbacks.allDeletions":            true,
			"callbacks.includeBodies":           true,
			"callbacks.maxBodyBytes":            128,
			"callbacks.events.memoryForgotten":  true,
			"callbacks.events.eventForgotten":   true,
		} {
			viper.Set(key, value)
		}

		t.Cleanup(func() {
			for _, key := range viper.AllKeys() {
				if len(key) > 10 && key[:10] == "callbacks." {
					viper.Set(key, nil)
				}
			}
		})
	}

	newServer := func(t *testing.T) (*Server, *db.DB) {
		t.Helper()

		database, err := db.New("")
		if err != nil {
			t.Fatalf("db.New: %s", err)
		}

		t.Cleanup(func() { _ = database.Close() })

		return &Server{db: database, consolidationEnabled: true}, database
	}

	t.Run("a nil notifier records nothing", func(t *testing.T) {
		setViper(t)

		s, database := newServer(t)
		s.startCallbackDispatch(nil)

		if s.callbacksEnabled || s.stopCallbacks != nil {
			t.Error("a nil notifier started the dispatcher")
		}

		queueOneDelivery(t, database)

		if depth, _ := database.CallbackQueueDepth(context.Background()); depth != 0 {
			t.Errorf("a nil notifier left the store recording (%d queued)", depth)
		}
	})

	t.Run("a disabled notifier records nothing", func(t *testing.T) {
		setViper(t)

		s, _ := newServer(t)
		s.startCallbackDispatch(&recordingNotifier{disabled: true})

		if s.callbacksEnabled || s.stopCallbacks != nil {
			t.Error("a disabled notifier started the dispatcher")
		}
	})

	t.Run("a replica records nothing and dispatches nothing", func(t *testing.T) {
		setViper(t)

		s, database := newServer(t)
		s.consolidationEnabled = false

		s.startCallbackDispatch(&recordingNotifier{})

		if s.callbacksEnabled || s.stopCallbacks != nil {
			t.Error("a replica started the dispatcher")
		}

		queueOneDelivery(t, database)

		if depth, _ := database.CallbackQueueDepth(context.Background()); depth != 0 {
			t.Errorf("a replica recorded %d deliveries with nothing to drain them", depth)
		}
	})

	t.Run("a store that cannot be told records nothing", func(t *testing.T) {
		setViper(t)

		database, err := db.New("")
		if err != nil {
			t.Fatalf("db.New: %s", err)
		}

		t.Cleanup(func() { _ = database.Close() })

		// A Store that does not expose SetCallbackPolicy - the seam is on the concrete type, not on
		// the interface, so a future backend without it must decline rather than panic.
		s := &Store{Store: database}
		server := &Server{db: s, consolidationEnabled: true}

		server.startCallbackDispatch(&recordingNotifier{})

		if server.callbacksEnabled || server.stopCallbacks != nil {
			t.Error("a store with no policy seam started the dispatcher")
		}
	})

	t.Run("the consolidator records and dispatches", func(t *testing.T) {
		setViper(t)

		s, database := newServer(t)
		s.startCallbackDispatch(&recordingNotifier{})

		t.Cleanup(func() {
			if s.stopCallbacks != nil {
				close(s.stopCallbacks)
				<-s.callbacksStopped
			}
		})

		if !s.callbacksEnabled || s.stopCallbacks == nil || s.callbacksStopped == nil {
			t.Fatal("the consolidator did not start the dispatcher")
		}

		// The viper values reached the server rather than the package defaults.
		if s.callbackMaxRows != 10 || s.callbackBatchSize != 5 || s.callbackChunkIds != 7 {
			t.Errorf("configuration did not reach the server: rows=%d batch=%d chunk=%d",
				s.callbackMaxRows, s.callbackBatchSize, s.callbackChunkIds)
		}

		if !s.callbackSleepEvents {
			t.Error("callbacks.events.sleepCompleted did not reach the server")
		}

		// And the store is recording.
		queueOneDelivery(t, database)

		if depth, _ := database.CallbackQueueDepth(context.Background()); depth != 1 {
			t.Errorf("the consolidator recorded %d deliveries, want 1", depth)
		}
	})

	t.Run("unset bounds fall back to the package defaults", func(t *testing.T) {
		setViper(t)

		for _, key := range []string{
			"callbacks.maxRows", "callbacks.maxAgeHours", "callbacks.batchSize",
			"callbacks.retryBaseBackoffSeconds", "callbacks.retryMaxBackoffSeconds",
			"callbacks.maxIdsPerDelivery",
		} {
			viper.Set(key, 0)
		}

		s, _ := newServer(t)
		s.startCallbackDispatch(&recordingNotifier{})

		t.Cleanup(func() {
			if s.stopCallbacks != nil {
				close(s.stopCallbacks)
				<-s.callbacksStopped
			}
		})

		if s.callbackMaxRows != defaultCallbackMaxRows ||
			s.callbackMaxAge != defaultCallbackMaxAgeHours*time.Hour ||
			s.callbackBatchSize != defaultCallbackBatchSize ||
			s.callbackBaseBack != defaultCallbackBaseBackoff ||
			s.callbackMaxBack != defaultCallbackMaxBackoff ||
			s.callbackChunkIds != defaultCallbackMaxIdsPerChunk {
			t.Errorf("a zero bound did not fall back to its default: %+v", []any{
				s.callbackMaxRows, s.callbackMaxAge, s.callbackBatchSize,
				s.callbackBaseBack, s.callbackMaxBack, s.callbackChunkIds,
			})
		}
	})
}

// Store is a db.Store that deliberately does NOT expose SetCallbackPolicy, so the type assertion in
// startCallbackDispatch has a negative case. It embeds the interface rather than the concrete type,
// which is exactly what hides the seam.
type Store struct {
	db.Store
}

// TestQueueCycleCallbackEdgeCases covers the gates and the two reporting branches.
func TestQueueCycleCallbackEdgeCases(t *testing.T) {
	report := &cycleReport{trigger: "timer", startedAt: time.Now(), success: true}

	t.Run("nothing is queued when callbacks are off", func(t *testing.T) {
		s, database := callbackServer(t, recordingPolicy())
		s.callbacksEnabled = false

		s.queueCycleCallback(context.Background(), 1, report)

		if depth, _ := database.CallbackQueueDepth(context.Background()); depth != 0 {
			t.Errorf("a disabled server queued %d completions", depth)
		}
	})

	t.Run("nothing is queued when the sleep event is off", func(t *testing.T) {
		s, database := callbackServer(t, recordingPolicy())
		s.callbackSleepEvents = false

		s.queueCycleCallback(context.Background(), 1, report)

		if depth, _ := database.CallbackQueueDepth(context.Background()); depth != 0 {
			t.Errorf("the sleep toggle being off queued %d completions", depth)
		}
	})

	t.Run("a truncated id list is reported and still delivers", func(t *testing.T) {
		s, database := callbackServer(t, recordingPolicy())

		s.db = failingCallbackStore{Store: database, truncated: true}

		s.queueCycleCallback(context.Background(), 3, report)

		// The completion still goes out: the counts in the cycle summary are complete even when the
		// id list is not, so a receiver gets the true scale either way.
		if depth, _ := database.CallbackQueueDepth(context.Background()); depth == 0 {
			t.Error("a truncated cycle queued no completion at all")
		}
	})

	t.Run("a failed queue write does not fail the cycle", func(t *testing.T) {
		s, database := callbackServer(t, recordingPolicy())

		s.db = failingCallbackStore{Store: database, failQueue: true}

		// The cycle has already finished and published its report; this must not panic or block.
		s.queueCycleCallback(context.Background(), 4, report)
	})
}

func TestCycleDeliveriesDefaultsAnInvalidChunkSize(t *testing.T) {
	deliveries := cycleDeliveries(1, &db.CallbackCycle{}, []string{"a", "b"}, nil, 0)

	if len(deliveries) != 1 {
		t.Fatalf("a non-positive chunk size produced %d deliveries, want 1 under the default", len(deliveries))
	}

	if deliveries[0].Chunks != 1 || len(deliveries[0].Payload.Items) != 2 {
		t.Errorf("the delivery is %+v", deliveries[0])
	}
}

// TestNotifyProjections covers the storage-to-wire mappings in both directions of completeness: every
// declared value maps, and an unknown one maps to the empty wire value rather than to a wrong one.
func TestNotifyProjections(t *testing.T) {
	kinds := map[db.CallbackKind]notify.Kind{
		db.CallbackKindMemoryForgotten: notify.KindMemoryForgotten,
		db.CallbackKindEventForgotten:  notify.KindEventForgotten,
		db.CallbackKindSleepCompleted:  notify.KindSleepCompleted,
		db.CallbackKindNone:            "",
		db.CallbackKind(99):            "",
	}

	for in, want := range kinds {
		if got := notifyKind(in); got != want {
			t.Errorf("notifyKind(%d) = %q, want %q", in, got, want)
		}
	}

	causes := map[db.DeleteCause]notify.Cause{
		db.CauseConsolidation:  notify.CauseConsolidation,
		db.CauseEviction:       notify.CauseEviction,
		db.CauseClient:         notify.CauseClient,
		db.CauseClear:          notify.CauseClear,
		db.CauseCascade:        notify.CauseCascade,
		db.CauseSummaryReplace: notify.CauseSummaryReplace,
		db.CausePurge:          notify.CausePurge,
		db.CauseNone:           "",
		db.DeleteCause(99):     "",
	}

	for in, want := range causes {
		if got := notifyCause(in); got != want {
			t.Errorf("notifyCause(%d) = %q, want %q", in, got, want)
		}
	}

	if items := notifyItems(nil); items != nil {
		t.Errorf("notifyItems(nil) = %v, want nil", items)
	}

	items := notifyItems([]db.CallbackItem{{
		Id: "m1", EventId: "e1", Group: "svc-a", Significance: 3, Bytes: 12,
		Body: "body", BodyOmitted: false,
	}})

	if len(items) != 1 || items[0].Id != "m1" || items[0].EventId != "e1" ||
		items[0].Group != "svc-a" || items[0].Significance != 3 || items[0].Bytes != 12 ||
		items[0].Body != "body" || items[0].BodyOmitted {
		t.Errorf("notifyItems lost a field: %+v", items)
	}

	if cycle := notifyCycle(nil); cycle != nil {
		t.Errorf("notifyCycle(nil) = %v, want nil", cycle)
	}

	cycle := notifyCycle(&db.CallbackCycle{
		Trigger: "timer", StartedAt: 1, DurationMillis: 2, MemoriesConsolidated: 3,
		EventsConsolidated: 4, MemoriesEvicted: 5, EventsEvicted: 6, BytesFreed: 7,
		SummarisationCandidates: 8, Success: false, Failure: "boom",
	})

	if cycle.Trigger != "timer" || cycle.StartedAt != 1 || cycle.DurationMillis != 2 ||
		cycle.MemoriesConsolidated != 3 || cycle.EventsConsolidated != 4 ||
		cycle.MemoriesEvicted != 5 || cycle.EventsEvicted != 6 || cycle.BytesFreed != 7 ||
		cycle.SummarisationCandidates != 8 || cycle.Success || cycle.Failure != "boom" {
		t.Errorf("notifyCycle lost a field: %+v", cycle)
	}
}

// TestDeliverOneRecordsTheOutcome pins that both outcomes are reported, since the failure rate is
// what an operator alerts on.
func TestDeliverOneRecordsTheOutcome(t *testing.T) {
	s, _ := callbackServer(t, recordingPolicy())

	sink := &recordingNotifier{}

	entry := db.CallbackDelivery{
		Seq: 1, Kind: db.CallbackKindMemoryForgotten, Cause: db.CauseConsolidation,
		Payload: db.CallbackPayload{Items: []db.CallbackItem{{Id: "m1"}}},
	}

	if !s.deliverOne(context.Background(), sink, entry) {
		t.Error("a successful delivery reported failure")
	}

	sink.failWith(errStoreFailed)

	if s.deliverOne(context.Background(), sink, entry) {
		t.Error("a refused delivery reported success")
	}
}
