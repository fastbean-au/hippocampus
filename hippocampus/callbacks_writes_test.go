package hippocampus

import (
	"context"
	"testing"

	"github.com/fastbean-au/hippocampus/contract"
	"github.com/fastbean-au/hippocampus/db"
	"github.com/fastbean-au/hippocampus/notify"
)

// TestAWriteReachesTheReceiverAsMemoryStored (TODO-3 item 171): the change stream end to end - a
// StoreMemory and an UpdateMemory each arrive at the receiver, as the memory now stands, through the
// same durable queue forgetting uses.
func TestAWriteReachesTheReceiverAsMemoryStored(t *testing.T) {
	s, _ := callbackServer(t, db.CallbackPolicy{Enabled: true, WriteEvents: true})
	ctx := context.Background()

	stored, err := s.StoreMemory(ctx, &contract.Memory{Id: "m1", Body: "hello", Significance: 5})
	if err != nil || stored.GetId() != "m1" {
		t.Fatalf("StoreMemory = %v, %v", stored, err)
	}

	if _, err := s.UpdateMemory(ctx, &contract.Memory{Id: "m1", Significance: 9}); err != nil {
		t.Fatalf("UpdateMemory: %s", err)
	}

	notifier := &recordingNotifier{}
	s.dispatchCallbacksOnce(notifier)

	sent := notifier.taken()

	if len(sent) != 2 {
		t.Fatalf("delivered %d, want a store and an update", len(sent))
	}

	if sent[0].Kind != notify.KindMemoryStored || sent[1].Kind != notify.KindMemoryUpdated {
		t.Errorf("delivered kinds %q and %q, want memory_stored then memory_updated", sent[0].Kind, sent[1].Kind)
	}

	if len(sent[1].Items) != 1 || sent[1].Items[0].Significance != 9 {
		t.Errorf("the update delivery carries %+v, want the memory as it now stands", sent[1].Items)
	}
}
