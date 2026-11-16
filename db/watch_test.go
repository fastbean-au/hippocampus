package db

import (
	"context"
	"sync"
	"testing"

	"github.com/fastbean-au/hippocampus/types"
)

// The change feed behind the Watch RPC (TODO-3 item 193). These hold the store's half: a committed
// change reaches the sink with the group a scoped watcher is filtered by, a change that did not
// happen never does, and watching changes nothing about what the callback queue records.

// recordingSink is a ChangeSink that keeps what it was given.
type recordingSink struct {
	mu         sync.Mutex
	watching   bool
	deliveries []CallbackDelivery
}

func (r *recordingSink) Watching() bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.watching
}

func (r *recordingSink) Publish(deliveries []CallbackDelivery) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.deliveries = append(r.deliveries, deliveries...)
}

func (r *recordingSink) published() []CallbackDelivery {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]CallbackDelivery(nil), r.deliveries...)
}

func watchedTestDB(t *testing.T, policy CallbackPolicy) (*DB, *recordingSink) {
	t.Helper()

	d := callbackTestDB(t, policy)
	sink := &recordingSink{watching: true}

	d.SetChangeSink(sink)

	return d, sink
}

// TestTheChangeFeedCarriesWritesAndDeletions: every kind the feed carries, each with its group.
func TestTheChangeFeedCarriesWritesAndDeletions(t *testing.T) {
	d, sink := watchedTestDB(t, CallbackPolicy{})
	ctx := context.Background()

	if _, err := d.CreateEvent(ctx, types.Event{Id: "e1", Name: "e", TimeStart: 100, Significance: 5, Group: "g"}); err != nil {
		t.Fatalf("CreateEvent: %s", err)
	}

	if _, err := d.CreateMemory(ctx, types.Memory{Id: "m1", Body: "first", TimeStamp: 100, Significance: 5, Group: "g"}); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	if _, err := d.UpdateMemory(ctx, types.Memory{Id: "m1", Significance: 9}); err != nil {
		t.Fatalf("UpdateMemory: %s", err)
	}

	if _, err := d.DeleteMemories(ctx, []string{"m1"}); err != nil {
		t.Fatalf("DeleteMemories: %s", err)
	}

	if _, err := d.DeleteEvent(ctx, "e1"); err != nil {
		t.Fatalf("DeleteEvent: %s", err)
	}

	published := sink.published()

	want := []CallbackKind{
		CallbackKindMemoryStored,
		CallbackKindMemoryUpdated,
		CallbackKindMemoryForgotten,
		CallbackKindEventForgotten,
	}

	if len(published) != len(want) {
		t.Fatalf("published %d deliveries, want %d: %+v", len(published), len(want), published)
	}

	for i, kind := range want {
		delivery := published[i]

		if delivery.Kind != kind {
			t.Errorf("delivery %d is kind %d, want %d", i, delivery.Kind, kind)
		}

		if len(delivery.Payload.Items) != 1 || delivery.Payload.Items[0].Group != "g" {
			t.Errorf("delivery %d does not carry its one item's group: %+v", i, delivery.Payload.Items)
		}
	}

	if published[2].Cause != CauseClient || published[3].Cause != CauseClient {
		t.Errorf("the deletions do not carry their cause: %d, %d", published[2].Cause, published[3].Cause)
	}

	if published[1].Payload.Items[0].Significance != 9 {
		t.Errorf("the update reports significance %d, want the memory as it now stands", published[1].Payload.Items[0].Significance)
	}
}

// TestTheChangeFeedPublishesOnlyWhatCommitted: a write that changed nothing, and a delete naming an
// id the store does not hold, report nothing - and leave nothing staged behind them.
func TestTheChangeFeedPublishesOnlyWhatCommitted(t *testing.T) {
	d, sink := watchedTestDB(t, CallbackPolicy{})
	ctx := context.Background()

	if _, err := d.CreateMemory(ctx, types.Memory{Id: "m1", Body: "x", TimeStamp: 100, Significance: 5}); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	// A duplicate create is rolled back inside execMemoryWrite.
	if _, err := d.CreateMemory(ctx, types.Memory{Id: "m1", Body: "y", TimeStamp: 100, Significance: 5}); err == nil {
		t.Fatal("a duplicate create succeeded")
	}

	if _, err := d.DeleteMemories(ctx, []string{"never-stored"}); err != nil {
		t.Fatalf("DeleteMemories: %s", err)
	}

	if published := sink.published(); len(published) != 1 {
		t.Errorf("published %d deliveries, want only the first create: %+v", len(published), published)
	}

	if pending := d.changes.pending.Load(); pending != 0 {
		t.Errorf("%d transactions still have changes staged against them", pending)
	}
}

// TestNobodyWatchingPublishesNothing: with no subscriber the paths capture nothing at all.
func TestNobodyWatchingPublishesNothing(t *testing.T) {
	d, sink := watchedTestDB(t, CallbackPolicy{})
	ctx := context.Background()

	sink.watching = false

	if _, err := d.CreateMemory(ctx, types.Memory{Id: "m1", Body: "x", TimeStamp: 100, Significance: 5}); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	if _, err := d.DeleteMemories(ctx, []string{"m1"}); err != nil {
		t.Fatalf("DeleteMemories: %s", err)
	}

	if published := sink.published(); len(published) != 0 {
		t.Errorf("published %d deliveries with nobody watching", len(published))
	}
}

// TestWatchingLeavesTheCallbackQueueAlone: a watcher widens what is captured, not what is queued -
// and a body captured for callbacks.includeBodies never reaches the feed.
func TestWatchingLeavesTheCallbackQueueAlone(t *testing.T) {
	d, sink := watchedTestDB(t, CallbackPolicy{Enabled: true, MemoryEvents: true, EventEvents: true, IncludeBodies: true, AllDeletions: true})
	ctx := context.Background()

	if _, err := d.CreateMemory(ctx, types.Memory{Id: "m1", Body: "secret", TimeStamp: 100, Significance: 5}); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	if _, err := d.DeleteMemories(ctx, []string{"m1"}); err != nil {
		t.Fatalf("DeleteMemories: %s", err)
	}

	claimed := claimAll(t, d)

	if len(claimed) != 1 || claimed[0].Kind != CallbackKindMemoryForgotten {
		t.Fatalf("queued %+v, want only the deletion: the write was watched, not asked for", claimed)
	}

	if claimed[0].Payload.Items[0].Body != "secret" {
		t.Errorf("the queued delivery lost the body callbacks.includeBodies asked for")
	}

	for _, delivery := range sink.published() {
		for _, item := range delivery.Payload.Items {
			if item.Body != "" {
				t.Errorf("the feed carried a memory body in a %d delivery", delivery.Kind)
			}
		}
	}

}

// TestWatchingLeavesTheNarrowDefaultNarrow: under the narrow default a client's delete raises no
// callback, and a write raises none without callbacks.writes, watched or not.
func TestWatchingLeavesTheNarrowDefaultNarrow(t *testing.T) {
	d, sink := watchedTestDB(t, CallbackPolicy{Enabled: true, MemoryEvents: true, EventEvents: true})
	ctx := context.Background()

	if _, err := d.CreateMemory(ctx, types.Memory{Id: "m2", Body: "x", TimeStamp: 100, Significance: 5}); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	if _, err := d.DeleteMemories(ctx, []string{"m2"}); err != nil {
		t.Fatalf("DeleteMemories: %s", err)
	}

	if claimed := claimAll(t, d); len(claimed) != 0 {
		t.Errorf("queued %d callbacks for a client delete under the narrow default", len(claimed))
	}

	if published := sink.published(); len(published) != 2 {
		t.Errorf("published %d deliveries, want the store and the delete", len(published))
	}
}
