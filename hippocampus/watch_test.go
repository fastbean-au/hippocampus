package hippocampus

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/fastbean-au/hippocampus/contract"
	"github.com/fastbean-au/hippocampus/types"
)

// The Watch RPC (TODO-3 item 193), driven in process: a fake stream stands in for the transport, and
// the store's change sink is the server's real hub.

// watchStream is a server stream that hands what is sent to a channel.
type watchStream struct {
	grpc.ServerStream

	ctx    context.Context
	events chan *contract.WatchEvent
}

func (w *watchStream) Context() context.Context {
	return w.ctx
}

func (w *watchStream) SendHeader(metadata.MD) error {
	return nil
}

func (w *watchStream) Send(event *contract.WatchEvent) error {
	w.events <- event

	return nil
}

// watching is one open Watch call.
type watching struct {
	events chan *contract.WatchEvent
	done   chan error
	cancel context.CancelFunc
}

// watchServer is a test server with the hub registered, as New registers it.
func watchServer(t *testing.T) *Server {
	t.Helper()

	s := newTestServer(t)
	s.startWatch()

	return s
}

// startWatching opens a Watch call and returns once it is subscribed, so nothing the test does next
// can be published before the watcher is there to see it.
func startWatching(t *testing.T, s *Server, ctx context.Context, in *contract.WatchRequest) *watching {
	t.Helper()

	before := s.watch.count.Load()

	ctx, cancel := context.WithCancel(ctx)

	w := &watching{
		events: make(chan *contract.WatchEvent, 64),
		done:   make(chan error, 1),
		cancel: cancel,
	}

	go func() {
		w.done <- s.Watch(in, &watchStream{ctx: ctx, events: w.events})
	}()

	t.Cleanup(cancel)

	deadline := time.Now().Add(5 * time.Second)

	for s.watch.count.Load() == before {
		select {

		case err := <-w.done:
			t.Fatalf("Watch returned before subscribing: %v", err)

		default:

		}

		if time.Now().After(deadline) {
			t.Fatal("Watch never subscribed")
		}

		time.Sleep(time.Millisecond)
	}

	return w
}

// next is the next event, failing the test if none arrives.
func (w *watching) next(t *testing.T) *contract.WatchEvent {
	t.Helper()

	select {

	case event := <-w.events:
		return event

	case err := <-w.done:
		t.Fatalf("Watch ended: %v", err)

	case <-time.After(5 * time.Second):
		t.Fatal("no Watch event arrived")

	}

	return nil
}

// none requires that nothing is waiting.
func (w *watching) none(t *testing.T) {
	t.Helper()

	select {

	case event := <-w.events:
		t.Errorf("an unexpected Watch event arrived: %v", event)

	default:

	}
}

// TestWatchSeesAStoreAndAForgetInOrder is the item's own test: the write, then the decay pass that
// forgets it, each reported with the group and cause a watcher needs.
func TestWatchSeesAStoreAndAForgetInOrder(t *testing.T) {
	t.Parallel()

	s := watchServer(t)
	ctx := context.Background()

	w := startWatching(t, s, ctx, &contract.WatchRequest{})

	if _, err := s.db.CreateEvent(ctx, types.Event{Id: "e1", Name: "trip", TimeStart: 100, Significance: 5, Group: "g"}); err != nil {
		t.Fatalf("CreateEvent: %s", err)
	}

	if _, err := s.db.CreateMemory(ctx, types.Memory{Id: "m1", Body: "x", TimeStamp: 100, Significance: 5, EventId: "e1", Group: "g"}); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	if _, _, _, err := s.db.ConsolidateEventMemories(ctx, consolidateEverything{}); err != nil {
		t.Fatalf("ConsolidateEventMemories: %s", err)
	}

	stored := w.next(t)

	if stored.GetKind() != contract.CallbackKind_CALLBACK_KIND_MEMORY_STORED || stored.GetItems()[0].GetId() != "m1" {
		t.Fatalf("the first event is %v, want m1 stored", stored)
	}

	if stored.GetItems()[0].GetGroup() != "g" || stored.GetItems()[0].GetEventId() != "e1" {
		t.Errorf("the store event does not carry the memory's group and event: %v", stored.GetItems()[0])
	}

	forgotten := w.next(t)

	if forgotten.GetKind() != contract.CallbackKind_CALLBACK_KIND_MEMORY_FORGOTTEN || forgotten.GetItems()[0].GetId() != "m1" {
		t.Fatalf("the second event is %v, want m1 forgotten", forgotten)
	}

	if forgotten.GetCause() != contract.DeleteCause_DELETE_CAUSE_CONSOLIDATION {
		t.Errorf("the forget reports cause %s, want consolidation", forgotten.GetCause())
	}

	if forgotten.GetTime() < stored.GetTime() {
		t.Errorf("the forget is timed before the store it follows")
	}
}

// TestGroupScopeIsolation_Watch: a watcher bound to group "a" hears about its own group's changes
// and nothing of group "b"'s - not even that one happened.
func TestGroupScopeIsolation_Watch(t *testing.T) {
	t.Parallel()

	s := watchServer(t)
	ctx := context.Background()

	w := startWatching(t, s, scopedContext("a"), &contract.WatchRequest{})

	for _, group := range []string{"b", "a"} {
		if _, err := s.db.CreateMemory(ctx, types.Memory{Id: "m-" + group, Body: "x", TimeStamp: 100, Significance: 5, Group: group}); err != nil {
			t.Fatalf("CreateMemory(%s): %s", group, err)
		}
	}

	// One delete spanning both groups: the event is filtered, not withheld.
	if _, err := s.db.DeleteMemories(ctx, []string{"m-a", "m-b"}); err != nil {
		t.Fatalf("DeleteMemories: %s", err)
	}

	stored := w.next(t)

	if stored.GetKind() != contract.CallbackKind_CALLBACK_KIND_MEMORY_STORED || stored.GetItems()[0].GetId() != "m-a" {
		t.Fatalf("the first event is %v, want m-a stored and nothing of group b's", stored)
	}

	forgotten := w.next(t)

	if len(forgotten.GetItems()) != 1 || forgotten.GetItems()[0].GetId() != "m-a" {
		t.Fatalf("the delete reported %v, want m-a alone", forgotten.GetItems())
	}

	assertNoLeak(t, "Watch", stored.String()+forgotten.String())

	w.none(t)
}

// TestWatchFiltersByKind: a watcher that asked for forgetting hears nothing about writes.
func TestWatchFiltersByKind(t *testing.T) {
	t.Parallel()

	s := watchServer(t)
	ctx := context.Background()

	w := startWatching(t, s, ctx, &contract.WatchRequest{
		Kinds: []contract.CallbackKind{contract.CallbackKind_CALLBACK_KIND_MEMORY_FORGOTTEN},
	})

	if _, err := s.db.CreateMemory(ctx, types.Memory{Id: "m1", Body: "x", TimeStamp: 100, Significance: 5}); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	if _, err := s.db.DeleteMemories(ctx, []string{"m1"}); err != nil {
		t.Fatalf("DeleteMemories: %s", err)
	}

	if event := w.next(t); event.GetKind() != contract.CallbackKind_CALLBACK_KIND_MEMORY_FORGOTTEN {
		t.Errorf("the watcher was sent %s, which it did not ask for", event.GetKind())
	}

	w.none(t)
}

// TestWatchRefusesAKindItDoesNotCarry: at-risk is a callbacks.* delivery only, and an unspecified
// kind is a mistake rather than a wildcard.
func TestWatchRefusesAKindItDoesNotCarry(t *testing.T) {
	t.Parallel()

	s := watchServer(t)

	for _, kind := range []contract.CallbackKind{
		contract.CallbackKind_CALLBACK_KIND_MEMORIES_AT_RISK,
		contract.CallbackKind_CALLBACK_KIND_UNSPECIFIED,
	} {
		err := s.Watch(
			&contract.WatchRequest{Kinds: []contract.CallbackKind{kind}},
			&watchStream{ctx: context.Background(), events: make(chan *contract.WatchEvent, 1)},
		)

		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("Watch for %s returned %v, want InvalidArgument", kind, err)
		}
	}
}

// TestWatchReportsASleepCycleToEveryWatcher: a completion is store-global counts, so a scoped
// watcher receives it too, as GetConsolidationStatus serves them a scoped caller.
func TestWatchReportsASleepCycleToEveryWatcher(t *testing.T) {
	t.Parallel()

	s := watchServer(t)

	w := startWatching(t, s, scopedContext("a"), &contract.WatchRequest{})

	s.watch.publishCycle(42, &cycleReport{startedAt: time.Unix(0, 100), trigger: "manual", memoriesConsolidated: 3, success: true})

	event := w.next(t)

	if event.GetKind() != contract.CallbackKind_CALLBACK_KIND_SLEEP_COMPLETED || event.GetCycleId() != 42 {
		t.Fatalf("the completion is %v", event)
	}

	if event.GetCycle().GetMemoriesConsolidated() != 3 || event.GetCycle().GetTrigger() != "manual" {
		t.Errorf("the completion does not carry the cycle's report: %v", event.GetCycle())
	}

	if len(event.GetItems()) != 0 {
		t.Errorf("the completion names records: %v", event.GetItems())
	}
}

// TestASlowWatcherIsToldWhatItMissed: a full buffer drops rather than waits, and the next event the
// watcher does receive says how many went.
func TestASlowWatcherIsToldWhatItMissed(t *testing.T) {
	t.Parallel()

	w := &watcher{
		kinds:  watchKinds,
		events: make(chan *contract.WatchEvent, 1),
	}

	for range 3 {
		w.offer(context.Background(), &contract.WatchEvent{})
	}

	if len(w.events) != 1 || w.dropped.Load() != 2 {
		t.Errorf("buffered %d and dropped %d, want 1 and 2", len(w.events), w.dropped.Load())
	}
}

// TestCloseWatchersEndsTheStreams: shutdown must not wait on a stream that never ends by itself,
// and a watcher arriving afterwards is refused rather than left hanging.
func TestCloseWatchersEndsTheStreams(t *testing.T) {
	t.Parallel()

	s := watchServer(t)

	w := startWatching(t, s, context.Background(), &contract.WatchRequest{})

	s.CloseWatchers()

	select {

	case err := <-w.done:
		if status.Code(err) != codes.Unavailable {
			t.Errorf("a closed Watch returned %v, want Unavailable", err)
		}

	case <-time.After(5 * time.Second):
		t.Fatal("CloseWatchers did not end the stream")

	}

	err := s.Watch(&contract.WatchRequest{}, &watchStream{ctx: context.Background(), events: make(chan *contract.WatchEvent, 1)})

	if status.Code(err) != codes.Unavailable {
		t.Errorf("a Watch after close returned %v, want Unavailable", err)
	}

	if s.watch.count.Load() != 0 {
		t.Errorf("%d watchers are still subscribed after close", s.watch.count.Load())
	}
}

// TestAClientLeavingUnsubscribes: a cancelled stream ends and stops the store capturing for it.
func TestAClientLeavingUnsubscribes(t *testing.T) {
	t.Parallel()

	s := watchServer(t)

	w := startWatching(t, s, context.Background(), &contract.WatchRequest{})

	w.cancel()

	if err := <-w.done; status.Code(err) != codes.Canceled {
		t.Errorf("a cancelled Watch returned %v, want Canceled", err)
	}

	if s.watch.Watching() {
		t.Error("the hub still reports a watcher after its client left")
	}
}
