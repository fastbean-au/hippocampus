package db

import (
	"context"
	"testing"
	"time"

	"github.com/fastbean-au/hippocampus/types"
)

// A change stream for writes (TODO-3 item 171): callbacks covered forgetting only, so a system
// mirroring the store learned of every deletion and no creation. memory_stored and memory_updated
// are opt-in (WriteEvents), and are queued inside the write's own transaction, so a write that
// commits always has its delivery and a write that fails never does.

func claimAll(t *testing.T, d *DB) []CallbackDelivery {
	t.Helper()

	claimed, err := d.ClaimCallbacks(context.Background(), 100, time.Now().UnixNano())
	if err != nil {
		t.Fatalf("ClaimCallbacks: %s", err)
	}

	return claimed
}

func TestWriteCallbacksReportStoresAndUpdates(t *testing.T) {
	d := callbackTestDB(t, CallbackPolicy{Enabled: true, WriteEvents: true})
	ctx := context.Background()

	if _, err := d.CreateMemory(ctx, types.Memory{Id: "m1", Body: "first", TimeStamp: 100, Significance: 5, Group: "g"}); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	if _, err := d.UpdateMemory(ctx, types.Memory{Id: "m1", Significance: 9}); err != nil {
		t.Fatalf("UpdateMemory: %s", err)
	}

	claimed := claimAll(t, d)

	if len(claimed) != 2 {
		t.Fatalf("queued %d deliveries, want a store and an update", len(claimed))
	}

	stored, updated := claimed[0], claimed[1]

	if stored.Kind != CallbackKindMemoryStored || len(stored.Payload.Items) != 1 || stored.Payload.Items[0].Id != "m1" {
		t.Errorf("the store delivery is %+v", stored)
	}

	if stored.Payload.Items[0].Group != "g" || stored.Payload.Items[0].Significance != 5 {
		t.Errorf("the store delivery does not carry the memory's group and significance: %+v", stored.Payload.Items[0])
	}

	if updated.Kind != CallbackKindMemoryUpdated || updated.Payload.Items[0].Significance != 9 {
		t.Errorf("the update delivery is %+v, want the memory as it now stands", updated)
	}
}

func TestWriteCallbacksAreOptIn(t *testing.T) {
	d := callbackTestDB(t, CallbackPolicy{Enabled: true, MemoryEvents: true, EventEvents: true})

	if _, err := d.CreateMemory(context.Background(), types.Memory{Id: "m1", Body: "x", TimeStamp: 100, Significance: 5}); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	if claimed := claimAll(t, d); len(claimed) != 0 {
		t.Errorf("a deployment that did not ask for write callbacks queued %d", len(claimed))
	}
}

// TestAFailedWriteQueuesNothing: the delivery shares the write's transaction.
func TestAFailedWriteQueuesNothing(t *testing.T) {
	d := callbackTestDB(t, CallbackPolicy{Enabled: true, WriteEvents: true})
	ctx := context.Background()

	if _, err := d.CreateMemory(ctx, types.Memory{Id: "m1", Body: "x", TimeStamp: 100, Significance: 5}); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	// Drain the store's own delivery, so what remains is only what the failed write queued.
	seqs := []int64{}
	for _, v := range claimAll(t, d) {
		seqs = append(seqs, v.Seq)
	}

	if err := d.ConfirmCallbacks(ctx, seqs); err != nil {
		t.Fatalf("ConfirmCallbacks: %s", err)
	}

	if _, err := d.CreateMemory(ctx, types.Memory{Id: "m1", Body: "again", TimeStamp: 100, Significance: 5}); err == nil {
		t.Fatal("a duplicate id was stored")
	}

	if claimed := claimAll(t, d); len(claimed) != 0 {
		t.Errorf("a write that failed queued %d deliveries", len(claimed))
	}
}

// TestAnUpdateOfAnUnknownMemoryQueuesNothing: nothing was written, so there is nothing to report.
func TestAnUpdateOfAnUnknownMemoryQueuesNothing(t *testing.T) {
	d := callbackTestDB(t, CallbackPolicy{Enabled: true, WriteEvents: true})

	if existed, err := d.UpdateMemory(context.Background(), types.Memory{Id: "never", Significance: 9}); err != nil || existed {
		t.Fatalf("UpdateMemory(never) = %v, %v", existed, err)
	}

	if claimed := claimAll(t, d); len(claimed) != 0 {
		t.Errorf("an update of nothing queued %d deliveries", len(claimed))
	}
}
