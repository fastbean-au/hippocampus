package db

import (
	"context"
	"strings"
	"testing"
	"time"
)

// callbackTestDB opens a store with callbacks recording, which is the state every test here needs.
func callbackTestDB(t *testing.T, policy CallbackPolicy) *DB {
	t.Helper()

	d := newTestDB(t)
	d.SetCallbackPolicy(policy)

	return d
}

// queueOne records a delivery through the exported entry point, which takes its own transaction.
func queueOne(t *testing.T, d *DB, delivery CallbackDelivery) {
	t.Helper()

	if err := d.QueueCallbacks(context.Background(), []CallbackDelivery{delivery}); err != nil {
		t.Fatalf("QueueCallbacks: %s", err.Error())
	}
}

func testMemoryDelivery(ids ...string) CallbackDelivery {
	items := make([]CallbackItem, 0, len(ids))

	for _, id := range ids {
		items = append(items, CallbackItem{Id: id, Group: "svc-a", Significance: 3})
	}

	return CallbackDelivery{
		Kind:      CallbackKindMemoryForgotten,
		Cause:     CauseConsolidation,
		ItemCount: len(items),
		Payload:   CallbackPayload{Items: items},
	}
}

// TestCallbackQueueRoundTrip is the whole lifecycle: record, claim, confirm.
func TestCallbackQueueRoundTrip(t *testing.T) {
	d := callbackTestDB(t, CallbackPolicy{Enabled: true, MemoryEvents: true, EventEvents: true})

	queueOne(t, d, testMemoryDelivery("m1", "m2"))
	queueOne(t, d, testMemoryDelivery("m3"))

	depth, err := d.CallbackQueueDepth(context.Background())
	if err != nil {
		t.Fatalf("CallbackQueueDepth: %s", err.Error())
	}

	if depth != 2 {
		t.Fatalf("depth is %d, want 2", depth)
	}

	claimed, err := d.ClaimCallbacks(context.Background(), 10, time.Now().UnixNano())
	if err != nil {
		t.Fatalf("ClaimCallbacks: %s", err.Error())
	}

	if len(claimed) != 2 {
		t.Fatalf("claimed %d deliveries, want 2", len(claimed))
	}

	// Ordered by seq, so the oldest is first - the order the receiver will see them.
	if claimed[0].Seq >= claimed[1].Seq {
		t.Errorf("deliveries were not claimed in seq order: %d then %d", claimed[0].Seq, claimed[1].Seq)
	}

	first := claimed[0]

	if first.Kind != CallbackKindMemoryForgotten || first.Cause != CauseConsolidation {
		t.Errorf("kind/cause did not round-trip: %+v", first)
	}

	if len(first.Payload.Items) != 2 || first.Payload.Items[0].Id != "m1" || first.Payload.Items[1].Group != "svc-a" {
		t.Errorf("the payload did not round-trip: %+v", first.Payload.Items)
	}

	if first.ItemCount != 2 {
		t.Errorf("item_count is %d, want 2", first.ItemCount)
	}

	// A claim does not consume: claiming again returns the same rows, which is what makes a crash
	// mid-pass replay rather than lose.
	again, err := d.ClaimCallbacks(context.Background(), 10, time.Now().UnixNano())
	if err != nil {
		t.Fatalf("ClaimCallbacks (again): %s", err.Error())
	}

	if len(again) != 2 {
		t.Fatalf("a claim consumed rows: got %d on the second pass, want 2", len(again))
	}

	if err := d.ConfirmCallbacks(context.Background(), []int64{first.Seq}); err != nil {
		t.Fatalf("ConfirmCallbacks: %s", err.Error())
	}

	depth, _ = d.CallbackQueueDepth(context.Background())
	if depth != 1 {
		t.Errorf("depth after confirming one is %d, want 1", depth)
	}
}

// TestCallbackQueueRecordsNothingWhenDisabled pins the gate: a store with callbacks off must queue
// nothing at all, because rows nothing drains are strictly worse than no rows.
func TestCallbackQueueRecordsNothingWhenDisabled(t *testing.T) {
	d := callbackTestDB(t, CallbackPolicy{})

	queueOne(t, d, testMemoryDelivery("m1"))

	depth, err := d.CallbackQueueDepth(context.Background())
	if err != nil {
		t.Fatalf("CallbackQueueDepth: %s", err.Error())
	}

	if depth != 0 {
		t.Errorf("a disabled store queued %d deliveries", depth)
	}

	claimed, err := d.ClaimCallbacks(context.Background(), 10, time.Now().UnixNano())
	if err != nil {
		t.Fatalf("ClaimCallbacks: %s", err.Error())
	}

	if len(claimed) != 0 {
		t.Errorf("a disabled store claimed %d deliveries", len(claimed))
	}
}

// TestCallbackPolicyWants covers the decay-versus-everything gate that callbacks.allDeletions moves.
func TestCallbackPolicyWants(t *testing.T) {
	decayOnly := CallbackPolicy{Enabled: true, MemoryEvents: true, EventEvents: true}
	everything := CallbackPolicy{Enabled: true, MemoryEvents: true, EventEvents: true, AllDeletions: true}
	off := CallbackPolicy{}

	cases := []struct {
		cause      DeleteCause
		wantNarrow bool
	}{
		{CauseConsolidation, true},
		{CauseEviction, true},
		{CauseClient, false},
		{CauseClear, false},
		{CauseCascade, false},
		{CauseSummaryReplace, false},
		{CausePurge, false},
	}

	for _, c := range cases {
		if got := decayOnly.wants(c.cause); got != c.wantNarrow {
			t.Errorf("decay-only wants(%d) = %t, want %t", c.cause, got, c.wantNarrow)
		}

		if !everything.wants(c.cause) {
			t.Errorf("allDeletions did not want cause %d", c.cause)
		}

		if off.wants(c.cause) {
			t.Errorf("a disabled policy wanted cause %d", c.cause)
		}
	}

	// The zero cause is never recorded, whatever the policy: it is how a delete says it is not
	// forgetting anything.
	if everything.wants(CauseNone) {
		t.Error("the zero cause was recorded")
	}
}

// TestCallbackBackoffIsHonoured pins that a deferred delivery is skipped until it is due, which is
// what stops the drain hammering a refusing receiver every pass.
func TestCallbackBackoffIsHonoured(t *testing.T) {
	d := callbackTestDB(t, CallbackPolicy{Enabled: true, MemoryEvents: true, EventEvents: true})

	queueOne(t, d, testMemoryDelivery("m1"))

	now := time.Now().UnixNano()

	claimed, err := d.ClaimCallbacks(context.Background(), 10, now)
	if err != nil {
		t.Fatalf("ClaimCallbacks: %s", err.Error())
	}

	if len(claimed) != 1 {
		t.Fatalf("claimed %d, want 1", len(claimed))
	}

	deadline := now + int64(time.Minute)

	if err := d.DeferCallbacks(context.Background(), []int64{claimed[0].Seq}, deadline); err != nil {
		t.Fatalf("DeferCallbacks: %s", err.Error())
	}

	// Not due yet.
	before, err := d.ClaimCallbacks(context.Background(), 10, now)
	if err != nil {
		t.Fatalf("ClaimCallbacks (before): %s", err.Error())
	}

	if len(before) != 0 {
		t.Errorf("a deferred delivery was claimed %d times before its deadline", len(before))
	}

	// Still there - a deferral is not a deletion.
	if depth, _ := d.CallbackQueueDepth(context.Background()); depth != 1 {
		t.Errorf("depth after a deferral is %d, want 1", depth)
	}

	after, err := d.ClaimCallbacks(context.Background(), 10, deadline+1)
	if err != nil {
		t.Fatalf("ClaimCallbacks (after): %s", err.Error())
	}

	if len(after) != 1 {
		t.Fatalf("claimed %d after the deadline, want 1", len(after))
	}

	if after[0].Attempts != 1 {
		t.Errorf("attempts is %d after one deferral, want 1", after[0].Attempts)
	}
}

// TestCallbackQueuePruning covers both caps.
func TestCallbackQueuePruning(t *testing.T) {
	d := callbackTestDB(t, CallbackPolicy{Enabled: true, MemoryEvents: true, EventEvents: true})

	old := time.Now().Add(-48 * time.Hour).UnixNano()

	for range 3 {
		queueOne(t, d, CallbackDelivery{
			Kind:     CallbackKindMemoryForgotten,
			Cause:    CauseConsolidation,
			QueuedAt: old,
			Payload:  CallbackPayload{Items: []CallbackItem{{Id: "stale"}}},
		})
	}

	for range 4 {
		queueOne(t, d, testMemoryDelivery("fresh"))
	}

	pruned, err := d.PruneCallbackQueue(context.Background(), 24*time.Hour, 0)
	if err != nil {
		t.Fatalf("PruneCallbackQueue (age): %s", err.Error())
	}

	if pruned != 3 {
		t.Errorf("the age cap pruned %d, want 3", pruned)
	}

	if depth, _ := d.CallbackQueueDepth(context.Background()); depth != 4 {
		t.Fatalf("depth after the age prune is %d, want 4", depth)
	}

	pruned, err = d.PruneCallbackQueue(context.Background(), 0, 2)
	if err != nil {
		t.Fatalf("PruneCallbackQueue (rows): %s", err.Error())
	}

	if pruned != 2 {
		t.Errorf("the row cap pruned %d, want 2", pruned)
	}

	// The NEWEST are kept: an older undelivered notification has had longer to matter less.
	remaining, err := d.ClaimCallbacks(context.Background(), 10, time.Now().UnixNano())
	if err != nil {
		t.Fatalf("ClaimCallbacks: %s", err.Error())
	}

	if len(remaining) != 2 {
		t.Fatalf("%d deliveries survived the row cap, want 2", len(remaining))
	}
}

// TestCallbackQueuePruningIsGated pins that turning callbacks off stops the trimming as well as the
// recording, so a configuration change never destroys queued deliveries somebody meant to keep.
func TestCallbackQueuePruningIsGated(t *testing.T) {
	d := callbackTestDB(t, CallbackPolicy{Enabled: true, MemoryEvents: true, EventEvents: true})

	queueOne(t, d, CallbackDelivery{
		Kind:     CallbackKindMemoryForgotten,
		Cause:    CauseConsolidation,
		QueuedAt: time.Now().Add(-48 * time.Hour).UnixNano(),
		Payload:  CallbackPayload{Items: []CallbackItem{{Id: "stale"}}},
	})

	d.SetCallbackPolicy(CallbackPolicy{})

	pruned, err := d.PruneCallbackQueue(context.Background(), time.Hour, 1)
	if err != nil {
		t.Fatalf("PruneCallbackQueue: %s", err.Error())
	}

	if pruned != 0 {
		t.Errorf("a disabled store pruned %d rows", pruned)
	}

	// Emptying it is always the explicit request.
	deleted, err := d.DeleteCallbackQueue(context.Background(), 0)
	if err != nil {
		t.Fatalf("DeleteCallbackQueue: %s", err.Error())
	}

	if deleted != 1 {
		t.Errorf("DeleteCallbackQueue removed %d, want 1", deleted)
	}
}

// TestDeleteCallbackQueueBeforeACutoff covers the partial form.
func TestDeleteCallbackQueueBeforeACutoff(t *testing.T) {
	d := callbackTestDB(t, CallbackPolicy{Enabled: true, MemoryEvents: true, EventEvents: true})

	cutoff := time.Now().UnixNano()

	queueOne(t, d, CallbackDelivery{
		Kind:     CallbackKindMemoryForgotten,
		Cause:    CauseConsolidation,
		QueuedAt: cutoff - int64(time.Hour),
		Payload:  CallbackPayload{Items: []CallbackItem{{Id: "old"}}},
	})

	queueOne(t, d, CallbackDelivery{
		Kind:     CallbackKindMemoryForgotten,
		Cause:    CauseConsolidation,
		QueuedAt: cutoff + int64(time.Hour),
		Payload:  CallbackPayload{Items: []CallbackItem{{Id: "new"}}},
	})

	deleted, err := d.DeleteCallbackQueue(context.Background(), cutoff)
	if err != nil {
		t.Fatalf("DeleteCallbackQueue: %s", err.Error())
	}

	if deleted != 1 {
		t.Fatalf("removed %d, want 1", deleted)
	}

	remaining, _ := d.ClaimCallbacks(context.Background(), 10, cutoff+int64(2*time.Hour))
	if len(remaining) != 1 || remaining[0].Payload.Items[0].Id != "new" {
		t.Errorf("the wrong delivery survived: %+v", remaining)
	}
}

// TestCallbackQueueListing covers the operator's view: oldest first, paged, and carrying no payload.
func TestCallbackQueueListing(t *testing.T) {
	d := callbackTestDB(t, CallbackPolicy{Enabled: true, MemoryEvents: true, EventEvents: true})

	for i := range 3 {
		delivery := testMemoryDelivery("m" + string(rune('1'+i)))
		delivery.Payload.Items[0].Body = "a body nobody should see here"

		queueOne(t, d, delivery)
	}

	queueOne(t, d, CallbackDelivery{
		Kind:    CallbackKindSleepCompleted,
		CycleId: 9,
		Chunk:   1,
		Chunks:  1,
		Payload: CallbackPayload{Cycle: &CallbackCycle{Trigger: "timer", Success: true}},
	})

	page, err := d.GetCallbackQueue(context.Background(), CallbackQueueFilter{Limit: 2})
	if err != nil {
		t.Fatalf("GetCallbackQueue: %s", err.Error())
	}

	if len(page) != 2 {
		t.Fatalf("page holds %d, want 2", len(page))
	}

	// The listing must never carry a payload: a queued delivery may hold memory bodies, and this is
	// an operator's view of a backlog rather than a second way to read the store.
	for _, delivery := range page {
		if len(delivery.Payload.Items) != 0 || delivery.Payload.Cycle != nil {
			t.Errorf("the listing carried a payload: %+v", delivery.Payload)
		}
	}

	next, err := d.GetCallbackQueue(context.Background(), CallbackQueueFilter{AfterSeq: page[1].Seq, Limit: 10})
	if err != nil {
		t.Fatalf("GetCallbackQueue (page 2): %s", err.Error())
	}

	if len(next) != 2 {
		t.Fatalf("the second page holds %d, want 2", len(next))
	}

	filtered, err := d.GetCallbackQueue(context.Background(), CallbackQueueFilter{Kind: CallbackKindSleepCompleted})
	if err != nil {
		t.Fatalf("GetCallbackQueue (filtered): %s", err.Error())
	}

	if len(filtered) != 1 || filtered[0].CycleId != 9 || filtered[0].Chunks != 1 {
		t.Errorf("the kind filter returned %+v", filtered)
	}
}

func TestCallbackQueueLimit(t *testing.T) {
	if got := CallbackQueueLimit(0); got != callbackQueueDefaultLimit {
		t.Errorf("CallbackQueueLimit(0) = %d, want the default", got)
	}

	if got := CallbackQueueLimit(-5); got != callbackQueueDefaultLimit {
		t.Errorf("CallbackQueueLimit(-5) = %d, want the default", got)
	}

	if got := CallbackQueueLimit(callbackQueueMaxLimit + 1); got != callbackQueueMaxLimit {
		t.Errorf("an over-cap request resolved to %d, want the cap", got)
	}

	if got := CallbackQueueLimit(7); got != 7 {
		t.Errorf("CallbackQueueLimit(7) = %d", got)
	}
}

// TestOldestQueuedCallback covers the figure that tells a backlog of minutes from one of days.
func TestOldestQueuedCallback(t *testing.T) {
	d := callbackTestDB(t, CallbackPolicy{Enabled: true, MemoryEvents: true, EventEvents: true})

	oldest, err := d.OldestQueuedCallback(context.Background())
	if err != nil {
		t.Fatalf("OldestQueuedCallback: %s", err.Error())
	}

	if oldest != 0 {
		t.Errorf("an empty queue reported an oldest of %d, want 0", oldest)
	}

	early := time.Now().Add(-time.Hour).UnixNano()

	queueOne(t, d, testMemoryDelivery("recent"))
	queueOne(t, d, CallbackDelivery{
		Kind:     CallbackKindMemoryForgotten,
		Cause:    CauseConsolidation,
		QueuedAt: early,
		Payload:  CallbackPayload{Items: []CallbackItem{{Id: "early"}}},
	})

	oldest, err = d.OldestQueuedCallback(context.Background())
	if err != nil {
		t.Fatalf("OldestQueuedCallback: %s", err.Error())
	}

	if oldest != early {
		t.Errorf("oldest is %d, want %d", oldest, early)
	}
}

// TestCallbackPayloadRoundTripsABody is the compression path: a payload large enough to be worth
// packing must come back byte-for-byte.
func TestCallbackPayloadRoundTripsABody(t *testing.T) {
	d := callbackTestDB(t, CallbackPolicy{Enabled: true, MemoryEvents: true, EventEvents: true, IncludeBodies: true})

	body := strings.Repeat("the quick brown fox jumps over the lazy dog. ", 200)

	queueOne(t, d, CallbackDelivery{
		Kind:      CallbackKindMemoryForgotten,
		Cause:     CauseEviction,
		ItemCount: 2,
		Payload: CallbackPayload{Items: []CallbackItem{
			{Id: "big", Body: body, Bytes: int64(len(body))},
			{Id: "omitted", BodyOmitted: true},
		}},
	})

	claimed, err := d.ClaimCallbacks(context.Background(), 10, time.Now().UnixNano())
	if err != nil {
		t.Fatalf("ClaimCallbacks: %s", err.Error())
	}

	if len(claimed) != 1 {
		t.Fatalf("claimed %d, want 1", len(claimed))
	}

	items := claimed[0].Payload.Items

	if len(items) != 2 {
		t.Fatalf("the payload holds %d items, want 2", len(items))
	}

	if items[0].Body != body {
		t.Errorf("the body did not survive the round trip (%d bytes back, %d out)", len(items[0].Body), len(body))
	}

	if items[1].Body != "" || !items[1].BodyOmitted {
		t.Errorf("the omitted flag did not round-trip: %+v", items[1])
	}
}

// TestCallbackPayloadEncodingChoosesTheSmaller pins that compression is never a storage loss: an
// incompressible payload is stored verbatim, exactly as a memory body is.
func TestCallbackPayloadEncodingChoosesTheSmaller(t *testing.T) {
	small, compressed, err := encodeCallbackPayload(CallbackPayload{Items: []CallbackItem{{Id: "a"}}})
	if err != nil {
		t.Fatalf("encodeCallbackPayload: %s", err.Error())
	}

	if compressed {
		t.Errorf("a tiny payload was stored compressed at %d bytes", len(small))
	}

	big := strings.Repeat("aaaaaaaaaa", 500)

	packed, compressed, err := encodeCallbackPayload(CallbackPayload{Items: []CallbackItem{{Id: "a", Body: big}}})
	if err != nil {
		t.Fatalf("encodeCallbackPayload: %s", err.Error())
	}

	if !compressed {
		t.Error("a highly compressible payload was stored verbatim")
	}

	if len(packed) >= len(big) {
		t.Errorf("the stored payload (%d) is not smaller than the body it carries (%d)", len(packed), len(big))
	}

	// Both forms decode, driven by the flag rather than the current configuration.
	back, err := decodeCallbackPayload(packed, compressed)
	if err != nil {
		t.Fatalf("decodeCallbackPayload: %s", err.Error())
	}

	if back.Items[0].Body != big {
		t.Error("a compressed payload did not decode to what went in")
	}
}

// TestCallbackQueueIsExcludedFromUsedBytes is the property that stops the record of undelivered
// notifications evicting live memories to make room for itself.
func TestCallbackQueueIsExcludedFromUsedBytes(t *testing.T) {
	requireSQLite(t)

	d := callbackTestDB(t, CallbackPolicy{Enabled: true, MemoryEvents: true, EventEvents: true})

	before, err := d.UsedBytes(context.Background())
	if err != nil {
		t.Fatalf("UsedBytes: %s", err.Error())
	}

	body := strings.Repeat("x", 4096)

	for range 50 {
		queueOne(t, d, CallbackDelivery{
			Kind:      CallbackKindMemoryForgotten,
			Cause:     CauseConsolidation,
			ItemCount: 1,
			Payload:   CallbackPayload{Items: []CallbackItem{{Id: "m", Body: body}}},
		})
	}

	after, err := d.UsedBytes(context.Background())
	if err != nil {
		t.Fatalf("UsedBytes: %s", err.Error())
	}

	// The allowance is flat and the payloads are compressible, so the figure need not be identical -
	// what must not happen is the queue driving it UP, which is what would raise capacity pressure.
	if after > before {
		t.Errorf("fifty queued deliveries raised UsedBytes from %d to %d - the queue would evict live memories to make room for itself", before, after)
	}
}

// TestPurgeEmptiesTheCallbackQueue pins the one case where queued deliveries are destroyed without a
// separate request, because Purge is itself that request.
func TestPurgeEmptiesTheCallbackQueue(t *testing.T) {
	d := callbackTestDB(t, CallbackPolicy{Enabled: true, MemoryEvents: true, EventEvents: true})

	queueOne(t, d, testMemoryDelivery("m1"))

	if err := d.Purge(context.Background()); err != nil {
		t.Fatalf("Purge: %s", err.Error())
	}

	depth, err := d.CallbackQueueDepth(context.Background())
	if err != nil {
		t.Fatalf("CallbackQueueDepth: %s", err.Error())
	}

	if depth != 0 {
		t.Errorf("%d deliveries survived a purge", depth)
	}
}

// TestClaimSkipsAnUndecodablePayload pins that one corrupt row cannot wedge the queue behind it.
func TestClaimSkipsAnUndecodablePayload(t *testing.T) {
	d := callbackTestDB(t, CallbackPolicy{Enabled: true, MemoryEvents: true, EventEvents: true})

	queueOne(t, d, testMemoryDelivery("good"))

	// A row whose blob is neither gzip nor JSON, which no amount of retrying will fix.
	if _, err := d.exec(
		context.Background(),
		`INSERT INTO `+callbackQueueTable+` (kind, cause, item_count, payload, is_compressed, queued_at, next_attempt_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		int(CallbackKindMemoryForgotten), int(CauseConsolidation), 1, []byte("not json"), false, 1, 1,
	); err != nil {
		t.Fatalf("inserting a corrupt row: %s", err.Error())
	}

	claimed, err := d.ClaimCallbacks(context.Background(), 10, time.Now().UnixNano())
	if err != nil {
		t.Fatalf("ClaimCallbacks: %s", err.Error())
	}

	if len(claimed) != 1 || claimed[0].Payload.Items[0].Id != "good" {
		t.Errorf("a corrupt row was not skipped: %+v", claimed)
	}
}

// TestTheCycleCollectionIsBounded pins that a cycle forgetting more than the bound reports the
// overflow rather than silently returning a short list as complete.
func TestTheCycleCollectionIsBounded(t *testing.T) {
	kept, dropped := appendBounded(nil, make([]string, maxCycleIds+10), 0)

	if len(kept) != maxCycleIds {
		t.Errorf("kept %d ids, want the bound (%d)", len(kept), maxCycleIds)
	}

	if dropped != 10 {
		t.Errorf("reported %d dropped, want 10", dropped)
	}

	// Once full, everything further is counted rather than appended.
	kept, dropped = appendBounded(kept, []string{"a", "b"}, dropped)

	if len(kept) != maxCycleIds || dropped != 12 {
		t.Errorf("a full collection grew to %d with %d dropped", len(kept), dropped)
	}
}

// TestTheCycleCollectionIsClosedByTakingIt pins that the collection's lifetime is one call: a second
// EndCallbackCycle returns nothing, so a cycle can never inherit the previous one's ids.
func TestTheCycleCollectionIsClosedByTakingIt(t *testing.T) {
	d := callbackTestDB(t, CallbackPolicy{Enabled: true, MemoryEvents: true, EventEvents: true})

	d.BeginCallbackCycle(5)
	d.collectForgotten(CauseConsolidation, []string{"m1"}, []string{"e1"})

	memoryIds, eventIds, _, _ := d.EndCallbackCycle()

	if len(memoryIds) != 1 || len(eventIds) != 1 {
		t.Fatalf("the collection held %v / %v", memoryIds, eventIds)
	}

	memoryIds, eventIds, _, _ = d.EndCallbackCycle()

	if len(memoryIds) != 0 || len(eventIds) != 0 {
		t.Errorf("a second take returned %v / %v - a cycle could inherit the last one's ids", memoryIds, eventIds)
	}

	// And outside a cycle nothing is collected or stamped.
	d.collectForgotten(CauseConsolidation, []string{"m2"}, nil)

	if id := d.cycleId(CauseConsolidation); id != 0 {
		t.Errorf("a delivery outside a cycle was stamped with cycle %d", id)
	}
}

// TestPerKindTogglesStopTheRowsBeingWritten pins that turning a kind off is cheaper than filtering:
// nothing is queued at all.
func TestPerKindTogglesStopTheRowsBeingWritten(t *testing.T) {
	policy := CallbackPolicy{Enabled: true, MemoryEvents: false, EventEvents: true}

	if policy.wantsKind(CallbackKindMemoryForgotten) {
		t.Error("memory callbacks are recorded with the toggle off")
	}

	if !policy.wantsKind(CallbackKindEventForgotten) {
		t.Error("event callbacks are not recorded with the toggle on")
	}

	// A sleep-cycle delivery is not gated here - it is queued by the RPC layer, which consults
	// callbacks.events.sleepCompleted itself.
	if !policy.wantsKind(CallbackKindSleepCompleted) {
		t.Error("the completion callback is gated by a per-kind toggle it should not consult")
	}
}
