package db

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/fastbean-au/hippocampus/types"
)

// drainDeliveries claims everything waiting, for a test that wants to assert on what a delete
// produced rather than on the queue mechanics.
func drainDeliveries(t *testing.T, d *DB) []CallbackDelivery {
	t.Helper()

	claimed, err := d.ClaimCallbacks(context.Background(), 1000, time.Now().UnixNano())
	if err != nil {
		t.Fatalf("ClaimCallbacks: %s", err.Error())
	}

	return claimed
}

// allItems flattens every delivery's items, in delivery order.
func allItems(deliveries []CallbackDelivery) []CallbackItem {
	var out []CallbackItem

	for _, delivery := range deliveries {
		out = append(out, delivery.Payload.Items...)
	}

	return out
}

// consolidationTestDB builds a store with three memories on one event and a server that consolidates
// whichever of them are named.
//
// The decision is made on SIGNIFICANCE rather than on the id, because a consolidation candidate
// deliberately carries no id - that is what keeps the two scans on the covering index. So the doomed
// memories are stored at significance 1 and the survivors at 9, and the server condemns the low ones.
func consolidationTestDB(t *testing.T, policy CallbackPolicy, doomed ...string) (*DB, *decisionServer) {
	t.Helper()

	d := newTestDB(t)
	d.SetCallbackPolicy(policy)

	ctx := context.Background()

	if _, err := d.CreateEvent(ctx, types.Event{Id: "e1", Name: "deploy", Significance: 5, Group: "svc-a", TimeStart: 100}); err != nil {
		t.Fatalf("CreateEvent: %s", err.Error())
	}

	condemned := make(map[string]bool, len(doomed))

	for _, id := range doomed {
		condemned[id] = true
	}

	for _, id := range []string{"m1", "m2", "m3"} {
		significance := int32(9)
		if condemned[id] {
			significance = 1
		}

		if _, err := d.CreateMemory(ctx, types.Memory{
			Id:           id,
			TimeStamp:    100,
			Significance: significance,
			EventId:      "e1",
			Group:        "svc-a",
			Body:         "the body of " + id,
		}); err != nil {
			t.Fatalf("CreateMemory(%s): %s", id, err.Error())
		}
	}

	server := &decisionServer{
		memory: func(candidate MemoryConsolidationCandidate) bool {
			return candidate.MemorySignificance == 1
		},
		value: func(candidate MemoryConsolidationCandidate) float64 {
			return 0.5
		},
	}

	return d, server
}

// TestConsolidationQueuesADelivery is the core of the feature: what the sleep cycle forgot reaches
// the queue, carrying the fields a receiver needs, in the same transaction as the deletion.
func TestConsolidationQueuesADelivery(t *testing.T) {
	d, server := consolidationTestDB(t, CallbackPolicy{Enabled: true, MemoryEvents: true, EventEvents: true}, "m1", "m2")

	if _, _, _, err := d.ConsolidateEventMemories(context.Background(), server); err != nil {
		t.Fatalf("ConsolidateEventMemories: %s", err.Error())
	}

	deliveries := drainDeliveries(t, d)

	var forgotten []CallbackItem

	for _, delivery := range deliveries {
		if delivery.Kind != CallbackKindMemoryForgotten {
			continue
		}

		if delivery.Cause != CauseConsolidation {
			t.Errorf("cause is %d, want CauseConsolidation", delivery.Cause)
		}

		forgotten = append(forgotten, delivery.Payload.Items...)
	}

	if len(forgotten) != 2 {
		t.Fatalf("%d memories were announced, want 2", len(forgotten))
	}

	byId := map[string]CallbackItem{}

	for _, item := range forgotten {
		byId[item.Id] = item
	}

	first, ok := byId["m1"]
	if !ok {
		t.Fatalf("m1 was not announced: %+v", forgotten)
	}

	if first.EventId != "e1" || first.Group != "svc-a" || first.Significance != 1 {
		t.Errorf("the item is missing context: %+v", first)
	}

	if first.Bytes == 0 {
		t.Error("the item reports no stored size")
	}

	// Bodies are off by default, so nothing here should carry one - and nothing should claim one was
	// omitted either, since none was asked for.
	if first.Body != "" || first.BodyOmitted {
		t.Errorf("a body was carried with includeBodies off: %+v", first)
	}

	// m3 survived and must not appear.
	if _, announced := byId["m3"]; announced {
		t.Error("a surviving memory was announced as forgotten")
	}
}

// TestConsolidationCarriesBodiesWhenAsked covers includeBodies and the size cap together, which is
// the pair that decides what a receiver can actually do with a deletion.
func TestConsolidationCarriesBodiesWhenAsked(t *testing.T) {
	d := newTestDB(t)
	d.SetCallbackPolicy(CallbackPolicy{Enabled: true, MemoryEvents: true, EventEvents: true, IncludeBodies: true, MaxBodyBytes: 64})

	ctx := context.Background()

	small := "a short body"
	large := strings.Repeat("x", 500)

	for id, body := range map[string]string{"small": small, "large": large} {
		if _, err := d.CreateMemory(ctx, types.Memory{
			Id: id, TimeStamp: 100, Significance: 3, Group: "svc-a", Body: body,
		}); err != nil {
			t.Fatalf("CreateMemory(%s): %s", id, err.Error())
		}
	}

	server := &decisionServer{
		memory: func(candidate MemoryConsolidationCandidate) bool { return true },
	}

	if _, err := d.ConsolidateMemories(ctx, server); err != nil {
		t.Fatalf("ConsolidateMemories: %s", err.Error())
	}

	byId := map[string]CallbackItem{}

	for _, item := range allItems(drainDeliveries(t, d)) {
		byId[item.Id] = item
	}

	if got := byId["small"]; got.Body != small || got.BodyOmitted {
		t.Errorf("a body under the cap was not carried: %+v", got)
	}

	// Over the cap: omitted and FLAGGED, never truncated. A receiver cannot tell a truncated body
	// from a whole one, which is why this is the contract.
	over := byId["large"]

	if over.Body != "" {
		t.Errorf("a body over the cap was carried (%d bytes)", len(over.Body))
	}

	if !over.BodyOmitted {
		t.Error("an omitted body was not flagged - a receiver would read it as an empty memory")
	}
}

// TestNonDecayDeletionsAreSilentByDefault pins the narrow default: a client that asked for a
// deletion already knows about it, so only decay speaks.
func TestNonDecayDeletionsAreSilentByDefault(t *testing.T) {
	d := newTestDB(t)
	d.SetCallbackPolicy(CallbackPolicy{Enabled: true, MemoryEvents: true, EventEvents: true})

	ctx := context.Background()

	if _, err := d.CreateMemory(ctx, types.Memory{Id: "m1", TimeStamp: 100, Significance: 3, Body: "x"}); err != nil {
		t.Fatalf("CreateMemory: %s", err.Error())
	}

	if _, err := d.CreateEvent(ctx, types.Event{Id: "e1", Name: "e", Significance: 5, TimeStart: 100}); err != nil {
		t.Fatalf("CreateEvent: %s", err.Error())
	}

	if _, err := d.DeleteMemories(ctx, []string{"m1"}); err != nil {
		t.Fatalf("DeleteMemories: %s", err.Error())
	}

	if _, err := d.DeleteEvent(ctx, "e1"); err != nil {
		t.Fatalf("DeleteEvent: %s", err.Error())
	}

	if deliveries := drainDeliveries(t, d); len(deliveries) != 0 {
		t.Errorf("the narrow default announced %d client deletions", len(deliveries))
	}
}

// TestAllDeletionsWidensTheFeed is the other half, and checks each cause reaches the queue under its
// own name rather than being flattened into one.
func TestAllDeletionsWidensTheFeed(t *testing.T) {
	d := newTestDB(t)
	d.SetCallbackPolicy(CallbackPolicy{Enabled: true, MemoryEvents: true, EventEvents: true, AllDeletions: true})

	ctx := context.Background()

	if _, err := d.CreateMemory(ctx, types.Memory{Id: "m1", TimeStamp: 100, Significance: 3, Group: "svc-a", Body: "x"}); err != nil {
		t.Fatalf("CreateMemory: %s", err.Error())
	}

	if _, err := d.CreateEvent(ctx, types.Event{Id: "e1", Name: "e", Significance: 5, Group: "svc-a", TimeStart: 100}); err != nil {
		t.Fatalf("CreateEvent: %s", err.Error())
	}

	if _, err := d.CreateMemory(ctx, types.Memory{Id: "m2", TimeStamp: 100, Significance: 3, EventId: "e1", Group: "svc-a", Body: "y"}); err != nil {
		t.Fatalf("CreateMemory: %s", err.Error())
	}

	if _, err := d.DeleteMemories(ctx, []string{"m1"}); err != nil {
		t.Fatalf("DeleteMemories: %s", err.Error())
	}

	if _, err := d.DeleteEventMemories(ctx, "e1"); err != nil {
		t.Fatalf("DeleteEventMemories: %s", err.Error())
	}

	if _, err := d.DeleteEvent(ctx, "e1"); err != nil {
		t.Fatalf("DeleteEvent: %s", err.Error())
	}

	seen := map[DeleteCause]CallbackKind{}

	for _, delivery := range drainDeliveries(t, d) {
		seen[delivery.Cause] = delivery.Kind
	}

	if kind := seen[CauseClient]; kind != CallbackKindMemoryForgotten && kind != CallbackKindEventForgotten {
		t.Errorf("a client deletion was not announced: %+v", seen)
	}

	if seen[CauseCascade] != CallbackKindMemoryForgotten {
		t.Errorf("a cascade was not announced as a memory deletion: %+v", seen)
	}
}

// TestAnUnknownIdIsNeverAnnounced pins that a delete naming an id the store never held announces
// nothing - a receiver acting on it would be acting on a deletion that did not happen.
func TestAnUnknownIdIsNeverAnnounced(t *testing.T) {
	d := newTestDB(t)
	d.SetCallbackPolicy(CallbackPolicy{Enabled: true, MemoryEvents: true, EventEvents: true, AllDeletions: true})

	ctx := context.Background()

	if _, err := d.CreateMemory(ctx, types.Memory{Id: "real", TimeStamp: 100, Significance: 3, Body: "x"}); err != nil {
		t.Fatalf("CreateMemory: %s", err.Error())
	}

	if _, err := d.DeleteMemories(ctx, []string{"real", "never-existed"}); err != nil {
		t.Fatalf("DeleteMemories: %s", err.Error())
	}

	items := allItems(drainDeliveries(t, d))

	if len(items) != 1 || items[0].Id != "real" {
		t.Errorf("announced %+v, want just the id the store actually held", items)
	}

	// And an event id that does not exist. The queue is emptied first because a claim deliberately
	// does not consume - that is what makes a crash mid-pass replay rather than lose - so anything
	// left from above would otherwise be counted twice.
	if _, err := d.DeleteCallbackQueue(ctx, 0); err != nil {
		t.Fatalf("DeleteCallbackQueue: %s", err.Error())
	}

	if _, err := d.DeleteEvent(ctx, "never-existed"); err != nil {
		t.Fatalf("DeleteEvent: %s", err.Error())
	}

	if deliveries := drainDeliveries(t, d); len(deliveries) != 0 {
		t.Errorf("deleting an unknown event announced %d deliveries", len(deliveries))
	}
}

// TestARecalledMemoryIsNeverAnnounced is the race-safety property, and the one that matters most: a
// memory recalled between the scan and the delete is spared by the delete's own guard, and telling a
// receiver it was forgotten would be a claim about a memory that is still there.
func TestARecalledMemoryIsNeverAnnounced(t *testing.T) {
	d := newTestDB(t)
	d.SetCallbackPolicy(CallbackPolicy{Enabled: true, MemoryEvents: true, EventEvents: true})

	ctx := context.Background()

	for _, id := range []string{"spared", "doomed"} {
		if _, err := d.CreateMemory(ctx, types.Memory{Id: id, TimeStamp: 100, Significance: 3, Body: "x"}); err != nil {
			t.Fatalf("CreateMemory(%s): %s", id, err.Error())
		}
	}

	// A snapshot whose recall state no longer matches is exactly what a mid-scan recall produces.
	deleted, err := d.deleteMemoriesIfUnrecalled(ctx, []memoryRecallSnapshot{
		{id: "spared", timeRecalled: 999, recallCount: 7},
		{id: "doomed"},
	}, forgetReason{cause: CauseConsolidation})
	if err != nil {
		t.Fatalf("deleteMemoriesIfUnrecalled: %s", err.Error())
	}

	if len(deleted) != 1 || deleted[0] != "doomed" {
		t.Fatalf("the guard deleted %v, want just doomed", deleted)
	}

	items := allItems(drainDeliveries(t, d))

	if len(items) != 1 || items[0].Id != "doomed" {
		t.Errorf("announced %+v - the recall-race guard's survivor must never be announced", items)
	}
}

// TestNothingIsQueuedWithCallbacksOff is the gate at the capture end: a store with callbacks off
// must not pay for the capture or write a row, whatever it deletes.
func TestNothingIsQueuedWithCallbacksOff(t *testing.T) {
	d, server := consolidationTestDB(t, CallbackPolicy{}, "m1", "m2", "m3")

	if _, _, _, err := d.ConsolidateEventMemories(context.Background(), server); err != nil {
		t.Fatalf("ConsolidateEventMemories: %s", err.Error())
	}

	depth, err := d.CallbackQueueDepth(context.Background())
	if err != nil {
		t.Fatalf("CallbackQueueDepth: %s", err.Error())
	}

	if depth != 0 {
		t.Errorf("a store with callbacks off queued %d deliveries", depth)
	}
}

// TestTheForgottenLogStillWorksAlongsideCallbacks pins that the shared capture did not change what
// the forgotten log records - in particular that it never gains a body.
func TestTheForgottenLogStillWorksAlongsideCallbacks(t *testing.T) {
	d, server := consolidationTestDB(t, CallbackPolicy{Enabled: true, MemoryEvents: true, EventEvents: true, IncludeBodies: true}, "m1")
	d.SetTombstonePolicy(TombstonePolicy{Enabled: true})

	if _, _, _, err := d.ConsolidateEventMemories(context.Background(), server); err != nil {
		t.Fatalf("ConsolidateEventMemories: %s", err.Error())
	}

	forgotten, err := d.GetForgottenMemories(context.Background(), ForgottenFilter{})
	if err != nil {
		t.Fatalf("GetForgottenMemories: %s", err.Error())
	}

	if len(forgotten) != 1 || forgotten[0].Id != "m1" {
		t.Fatalf("the forgotten log holds %+v, want just m1", forgotten)
	}

	if forgotten[0].Rule != ForgetRuleConsolidation || forgotten[0].Group != "svc-a" {
		t.Errorf("the tombstone lost its context: %+v", forgotten[0])
	}

	// And the callback got the body the log deliberately does not carry.
	items := allItems(drainDeliveries(t, d))

	if len(items) != 1 || items[0].Body == "" {
		t.Errorf("the callback did not carry the body: %+v", items)
	}
}
