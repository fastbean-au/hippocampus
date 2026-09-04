package db

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// The gates and error paths of the callback queue.
//
// Two harnesses, because the branches divide cleanly in two. The guards that return before touching
// the database are driven against a zero-value *DB - which is exactly what a read-only open is, one
// that skipped initSchema and so must never query a table it cannot be sure exists. Everything that
// fails mid-statement is driven with sqlmock, which is the only way to make a specific statement in
// a multi-statement method fail while its predecessors succeed.

// mockCallbackDB is a sqlmock store with the queue recording, which is the state every error path
// below needs: the guards return early otherwise and the statement never runs.
func mockCallbackDB(t *testing.T) (*DB, sqlmock.Sqlmock) {
	t.Helper()

	d, mock := newMockDB(t, driverSQLite)

	d.callbackTable = true
	d.callbacks = CallbackPolicy{Enabled: true, MemoryEvents: true, EventEvents: true}

	return d, mock
}

// TestCallbackQueueGuardsAgainstAnUninitialisedTable covers every read that must be a no-op on a
// store whose initCallbackQueue has not run.
//
// That is not hypothetical: the read-only opens (--backfill-search, --schema-version) skip
// initSchema entirely, so they hold a *DB whose callbackTable is false against a store that may
// have no such table at all. Querying it would turn a diagnostic tool into a failure.
func TestCallbackQueueGuardsAgainstAnUninitialisedTable(t *testing.T) {
	// No sql handle at all, which is the strongest possible statement that nothing is queried: any
	// of these reaching a statement would panic rather than merely fail.
	d := &DB{driver: driverSQLite, callbacks: CallbackPolicy{Enabled: true, MemoryEvents: true}}
	ctx := context.Background()

	if depth, err := d.CallbackQueueDepth(ctx); depth != 0 || err != nil {
		t.Errorf("CallbackQueueDepth = (%d, %v), want (0, nil)", depth, err)
	}

	if oldest, err := d.OldestQueuedCallback(ctx); oldest != 0 || err != nil {
		t.Errorf("OldestQueuedCallback = (%d, %v), want (0, nil)", oldest, err)
	}

	if page, err := d.GetCallbackQueue(ctx, CallbackQueueFilter{}); page != nil || err != nil {
		t.Errorf("GetCallbackQueue = (%v, %v), want (nil, nil)", page, err)
	}

	if deleted, err := d.DeleteCallbackQueue(ctx, 0); deleted != 0 || err != nil {
		t.Errorf("DeleteCallbackQueue = (%d, %v), want (0, nil)", deleted, err)
	}

	if bytes := d.callbackQueueBytes(ctx); bytes != 0 {
		t.Errorf("callbackQueueBytes = %d, want 0 - an unmeasurable queue must not be charged to the store", bytes)
	}

	if claimed, err := d.ClaimCallbacks(ctx, 10, 1); claimed != nil || err != nil {
		t.Errorf("ClaimCallbacks = (%v, %v), want (nil, nil)", claimed, err)
	}

	if pruned, err := d.PruneCallbackQueue(ctx, time.Hour, 10); pruned != 0 || err != nil {
		t.Errorf("PruneCallbackQueue = (%d, %v), want (0, nil)", pruned, err)
	}

	if err := d.QueueCallbacks(ctx, []CallbackDelivery{{Kind: CallbackKindSleepCompleted}}); err != nil {
		t.Errorf("QueueCallbacks = %v, want nil", err)
	}
}

// TestCallbackNoOpsOnEmptyInput covers the "nothing to do" returns, which are what let the delete
// chokepoints call these unconditionally rather than guarding at every call site.
func TestCallbackNoOpsOnEmptyInput(t *testing.T) {
	d := callbackTestDB(t, CallbackPolicy{Enabled: true, MemoryEvents: true, EventEvents: true})
	ctx := context.Background()

	if err := d.ConfirmCallbacks(ctx, nil); err != nil {
		t.Errorf("ConfirmCallbacks(nil) = %v", err)
	}

	if err := d.DeferCallbacks(ctx, nil, 1); err != nil {
		t.Errorf("DeferCallbacks(nil) = %v", err)
	}

	if err := d.QueueCallbacks(ctx, nil); err != nil {
		t.Errorf("QueueCallbacks(nil) = %v", err)
	}
}

// TestClaimCallbacksNormalisesTheLimit pins that a non-positive limit selects the package default
// rather than asking the database for no rows, which would stall the drain silently.
func TestClaimCallbacksNormalisesTheLimit(t *testing.T) {
	d := callbackTestDB(t, CallbackPolicy{Enabled: true, MemoryEvents: true, EventEvents: true})
	ctx := context.Background()

	for i := range 3 {
		if err := d.QueueCallbacks(ctx, []CallbackDelivery{{
			Kind:      CallbackKindMemoryForgotten,
			Cause:     CauseConsolidation,
			ItemCount: 1,
			Payload:   CallbackPayload{Items: []CallbackItem{{Id: string(rune('a' + i))}}},
		}}); err != nil {
			t.Fatalf("QueueCallbacks: %s", err.Error())
		}
	}

	claimed, err := d.ClaimCallbacks(ctx, 0, time.Now().UnixNano())
	if err != nil {
		t.Fatalf("ClaimCallbacks: %s", err.Error())
	}

	if len(claimed) != 3 {
		t.Errorf("a non-positive limit claimed %d deliveries, want all 3 under the default", len(claimed))
	}
}

// TestQueueCallbacksBatchesSeveralDeliveries covers the multi-row INSERT's separator, which only one
// call site produces: a sleep cycle whose id list is chunked into several deliveries at once.
func TestQueueCallbacksBatchesSeveralDeliveries(t *testing.T) {
	d := callbackTestDB(t, CallbackPolicy{Enabled: true, MemoryEvents: true, EventEvents: true})
	ctx := context.Background()

	deliveries := make([]CallbackDelivery, 0, 3)

	for i := range 3 {
		deliveries = append(deliveries, CallbackDelivery{
			Kind:      CallbackKindSleepCompleted,
			CycleId:   77,
			Chunk:     i + 1,
			Chunks:    3,
			ItemCount: 1,
			Payload:   CallbackPayload{Items: []CallbackItem{{Id: string(rune('a' + i))}}},
		})
	}

	if err := d.QueueCallbacks(ctx, deliveries); err != nil {
		t.Fatalf("QueueCallbacks: %s", err.Error())
	}

	claimed, err := d.ClaimCallbacks(ctx, 10, time.Now().UnixNano())
	if err != nil {
		t.Fatalf("ClaimCallbacks: %s", err.Error())
	}

	if len(claimed) != 3 {
		t.Fatalf("a batch of 3 produced %d rows", len(claimed))
	}

	for i, delivery := range claimed {
		if delivery.Chunk != i+1 || delivery.CycleId != 77 {
			t.Errorf("row %d is %+v", i, delivery)
		}
	}
}

// TestMemoryDeliveryEdgeCases covers the builder's two branches that the delete paths do not
// normally reach.
func TestMemoryDeliveryEdgeCases(t *testing.T) {
	// Nothing went, so there is nothing to announce - which is what lets a chunk whose every memory
	// was spared by the recall-race guard queue no row at all.
	if _, ok := memoryDelivery(map[string]tombstoneRow{"m1": {id: "m1"}}, nil, forgetReason{cause: CauseConsolidation}, 0); ok {
		t.Error("an empty deletion produced a delivery")
	}

	// An id that went with no captured row still has to be reported: the id is the part a receiver
	// cannot do without, and dropping it would under-report the deletion silently.
	delivery, ok := memoryDelivery(
		map[string]tombstoneRow{"known": {id: "known", group: "svc-a", significance: 4}},
		[]string{"known", "uncaptured"},
		forgetReason{cause: CauseEviction},
		9,
	)
	if !ok {
		t.Fatal("a non-empty deletion produced no delivery")
	}

	if len(delivery.Payload.Items) != 2 {
		t.Fatalf("the delivery carries %d items, want both ids", len(delivery.Payload.Items))
	}

	if delivery.Payload.Items[1].Id != "uncaptured" || delivery.Payload.Items[1].Group != "" {
		t.Errorf("the uncaptured id is %+v, want a bare id", delivery.Payload.Items[1])
	}

	if delivery.CycleId != 9 || delivery.Cause != CauseEviction {
		t.Errorf("the delivery lost its stamping: %+v", delivery)
	}
}

func TestEventItemIds(t *testing.T) {
	if ids := eventItemIds(nil); len(ids) != 0 {
		t.Errorf("eventItemIds(nil) = %v", ids)
	}

	ids := eventItemIds([]CallbackItem{{Id: "e1"}, {Id: "e2"}})

	if len(ids) != 2 || ids[0] != "e1" || ids[1] != "e2" {
		t.Errorf("eventItemIds = %v", ids)
	}
}

// TestCaptureEventCallbackIsGated covers the two early returns, including the per-kind toggle: an
// operator who has switched event callbacks off must not pay for the capture.
func TestCaptureEventCallbackIsGated(t *testing.T) {
	d := callbackTestDB(t, CallbackPolicy{Enabled: true, MemoryEvents: true, EventEvents: false})

	tx, err := d.sql.Begin()
	if err != nil {
		t.Fatalf("Begin: %s", err.Error())
	}

	defer func() { _ = tx.Rollback() }()

	items, err := d.captureEventCallback(tx, []string{"e1"}, CauseConsolidation)
	if err != nil || items != nil {
		t.Errorf("captureEventCallback with the toggle off = (%v, %v), want (nil, nil)", items, err)
	}

	// And with the toggle on but no ids.
	d.callbacks.EventEvents = true

	if items, err := d.captureEventCallback(tx, nil, CauseConsolidation); err != nil || items != nil {
		t.Errorf("captureEventCallback(nil) = (%v, %v), want (nil, nil)", items, err)
	}

	// And with a cause the narrow default does not want.
	if items, err := d.captureEventCallback(tx, []string{"e1"}, CauseClient); err != nil || items != nil {
		t.Errorf("captureEventCallback(client) = (%v, %v), want (nil, nil)", items, err)
	}
}

// TestBeginCallbackCycleIsGated covers the paths that open no collection: callbacks off, and a
// non-positive id. Both must leave nothing open, so a later EndCallbackCycle cannot return a
// previous cycle's ids.
func TestBeginCallbackCycleIsGated(t *testing.T) {
	d := callbackTestDB(t, CallbackPolicy{Enabled: true, MemoryEvents: true, EventEvents: true})

	// A live collection first, so the gated calls are shown to CLEAR it rather than merely not
	// setting it - a stale collection would attribute one cycle's deletions to the next.
	d.BeginCallbackCycle(1)
	d.collectForgotten(CauseConsolidation, []string{"stale"}, nil)

	d.BeginCallbackCycle(0)

	if memoryIds, _, _, _ := d.EndCallbackCycle(); len(memoryIds) != 0 {
		t.Errorf("a non-positive cycle id left %v collected", memoryIds)
	}

	d.BeginCallbackCycle(1)
	d.collectForgotten(CauseConsolidation, []string{"stale"}, nil)

	d.SetCallbackPolicy(CallbackPolicy{})
	d.BeginCallbackCycle(2)

	if memoryIds, _, _, _ := d.EndCallbackCycle(); len(memoryIds) != 0 {
		t.Errorf("a disabled store left %v collected", memoryIds)
	}
}

// TestCycleIdStampsOnlyDecayInsideACycle covers the stamping's three answers.
func TestCycleIdStampsOnlyDecayInsideACycle(t *testing.T) {
	d := callbackTestDB(t, CallbackPolicy{Enabled: true, MemoryEvents: true, EventEvents: true})

	d.BeginCallbackCycle(42)

	if id := d.cycleId(CauseConsolidation); id != 42 {
		t.Errorf("a decay cause inside a cycle stamped %d, want 42", id)
	}

	if id := d.cycleId(CauseEviction); id != 42 {
		t.Errorf("eviction inside a cycle stamped %d, want 42", id)
	}

	// A client delete landing mid-cycle is not the cycle's work and must not be stamped as it.
	if id := d.cycleId(CauseClient); id != 0 {
		t.Errorf("a client delete inside a cycle stamped %d, want 0", id)
	}

	d.EndCallbackCycle()

	if id := d.cycleId(CauseConsolidation); id != 0 {
		t.Errorf("a decay cause outside a cycle stamped %d, want 0", id)
	}
}

func TestAppendBoundedIgnoresAnEmptyAppend(t *testing.T) {
	into := []string{"a"}

	kept, dropped := appendBounded(into, nil, 3)

	if len(kept) != 1 || dropped != 3 {
		t.Errorf("appendBounded(_, nil, 3) = (%v, %d), want the input unchanged", kept, dropped)
	}
}

// TestDecodeCallbackPayloadHandlesBothFailures covers the decompression error and the empty blob.
func TestDecodeCallbackPayloadHandlesBothFailures(t *testing.T) {
	// Flagged compressed but not gzip: unrecoverable, and reported rather than returned as empty.
	if _, err := decodeCallbackPayload([]byte("not gzip"), true); err == nil {
		t.Error("a corrupt compressed payload decoded without error")
	}

	// An empty blob is a valid, empty payload rather than a decode failure - json.Unmarshal would
	// refuse "" outright, so this branch is what keeps a payload-less row readable.
	payload, err := decodeCallbackPayload(nil, false)
	if err != nil {
		t.Fatalf("decodeCallbackPayload(nil) = %s", err.Error())
	}

	if len(payload.Items) != 0 || payload.Cycle != nil {
		t.Errorf("an empty blob decoded to %+v", payload)
	}
}

// --- error paths, driven with sqlmock ---

// TestInitCallbackQueueSurfacesIndexFailures pins that a failure creating either index stops schema
// initialisation rather than leaving a table the drain cannot page through.
func TestInitCallbackQueueSurfacesIndexFailures(t *testing.T) {
	for _, failAt := range []int{0, 1} {
		d, mock := newMockDB(t, driverSQLite)

		mock.ExpectExec(`CREATE TABLE IF NOT EXISTS callback_queue`).WillReturnResult(sqlmock.NewResult(0, 0))

		for i := range failAt {
			_ = i
			mock.ExpectExec(`CREATE INDEX IF NOT EXISTS idx_callback_queue`).WillReturnResult(sqlmock.NewResult(0, 0))
		}

		mock.ExpectExec(`CREATE INDEX IF NOT EXISTS idx_callback_queue`).WillReturnError(errors.New("boom"))

		if err := d.initCallbackQueue(); err == nil {
			t.Errorf("a failure creating index %d did not stop initialisation", failAt)
		}

		if d.callbackTable {
			t.Errorf("callbackTable was set despite index %d failing - reads would query a half-built table", failAt)
		}
	}
}

func TestQueueCallbacksSurfacesTheInsertFailure(t *testing.T) {
	d, mock := mockCallbackDB(t)

	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO callback_queue`).WillReturnError(errors.New("boom"))
	mock.ExpectRollback()

	err := d.QueueCallbacks(context.Background(), []CallbackDelivery{{
		Kind:    CallbackKindSleepCompleted,
		Payload: CallbackPayload{Items: []CallbackItem{{Id: "m1"}}},
	}})
	if err == nil {
		t.Fatal("a failed insert was reported as queued")
	}

	expectationsMet(t, mock)
}

func TestQueueCallbacksSurfacesTheBeginFailure(t *testing.T) {
	d, mock := mockCallbackDB(t)

	mock.ExpectBegin().WillReturnError(errors.New("boom"))

	if err := d.QueueCallbacks(context.Background(), []CallbackDelivery{{
		Kind: CallbackKindSleepCompleted,
	}}); err == nil {
		t.Fatal("a failed BEGIN was reported as queued")
	}

	expectationsMet(t, mock)
}

// TestClaimCallbacksSurfacesAScanFailure covers the row-scan branch, which a query error alone never
// reaches: the statement succeeds and the columns are wrong.
func TestClaimCallbacksSurfacesAScanFailure(t *testing.T) {
	d, mock := mockCallbackDB(t)

	mock.ExpectQuery(`FROM callback_queue`).WillReturnRows(
		sqlmock.NewRows([]string{
			"seq", "kind", "cause", "cycle_id", "chunk", "chunks", "item_count", "payload",
			"is_compressed", "queued_at", "attempts", "next_attempt_at",
		}).AddRow("not a number", 1, 1, 0, 0, 0, 0, []byte("{}"), false, 0, 0, 0),
	)

	if _, err := d.ClaimCallbacks(context.Background(), 10, 1); err == nil {
		t.Fatal("a malformed row was claimed without error")
	}

	expectationsMet(t, mock)
}

func TestGetCallbackQueueSurfacesAScanFailure(t *testing.T) {
	d, mock := mockCallbackDB(t)

	mock.ExpectQuery(`FROM callback_queue`).WillReturnRows(
		sqlmock.NewRows([]string{
			"seq", "kind", "cause", "cycle_id", "chunk", "chunks", "item_count", "queued_at",
			"attempts", "next_attempt_at",
		}).AddRow("not a number", 1, 1, 0, 0, 0, 0, 0, 0, 0),
	)

	if _, err := d.GetCallbackQueue(context.Background(), CallbackQueueFilter{}); err == nil {
		t.Fatal("a malformed row was listed without error")
	}

	expectationsMet(t, mock)
}

// TestPruneCallbackQueueSurfacesTheRowCapFailure covers the second of the two caps, which the age
// cap succeeding is what makes reachable.
func TestPruneCallbackQueueSurfacesTheRowCapFailure(t *testing.T) {
	d, mock := mockCallbackDB(t)

	mock.ExpectExec(`DELETE FROM callback_queue WHERE queued_at`).WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectExec(`DELETE FROM callback_queue WHERE seq`).WillReturnError(errors.New("boom"))

	pruned, err := d.PruneCallbackQueue(context.Background(), time.Hour, 10)
	if err == nil {
		t.Fatal("a failed row-cap prune was reported as successful")
	}

	// What the age cap already removed is still reported, so a caller's counter does not lose it.
	if pruned != 2 {
		t.Errorf("pruned = %d, want the 2 the age cap removed before the failure", pruned)
	}

	expectationsMet(t, mock)
}

// TestCallbackQueueBytesFailsSafe pins that an unmeasurable queue is charged as zero rather than
// propagating an error into UsedBytes - a failure to measure the queue must not stop the store
// measuring itself.
func TestCallbackQueueBytesFailsSafe(t *testing.T) {
	d, mock := mockCallbackDB(t)

	mock.ExpectQuery(`COUNT\(\*\) FROM callback_queue`).WillReturnError(errors.New("boom"))

	if bytes := d.callbackQueueBytes(context.Background()); bytes != 0 {
		t.Errorf("callbackQueueBytes = %d on a failed measurement, want 0", bytes)
	}

	expectationsMet(t, mock)
}

func TestCaptureEventsSurfacesQueryAndScanFailures(t *testing.T) {
	t.Run("query", func(t *testing.T) {
		d, mock := mockCallbackDB(t)

		mock.ExpectBegin()
		mock.ExpectQuery(`FROM events e LEFT JOIN`).WillReturnError(errors.New("boom"))

		tx, _ := d.sql.Begin()
		defer func() { _ = tx.Rollback() }()

		if _, err := d.captureEvents(tx, []string{"e1"}); err == nil {
			t.Fatal("a failed capture query returned no error")
		}
	})

	t.Run("scan", func(t *testing.T) {
		d, mock := mockCallbackDB(t)

		mock.ExpectBegin()
		mock.ExpectQuery(`FROM events e LEFT JOIN`).WillReturnRows(
			sqlmock.NewRows([]string{"id", "group_name", "level_rank"}).AddRow("e1", "svc-a", "not a number"),
		)

		tx, _ := d.sql.Begin()
		defer func() { _ = tx.Rollback() }()

		if _, err := d.captureEvents(tx, []string{"e1"}); err == nil {
			t.Fatal("a malformed event row was captured without error")
		}
	})
}

// TestCaptureMemoryCallbackSurfacesTheCaptureFailure pins that a failed capture fails the delete
// rather than queueing a delivery with nothing in it.
func TestCaptureMemoryCallbackSurfacesTheCaptureFailure(t *testing.T) {
	d, mock := mockCallbackDB(t)

	mock.ExpectBegin()
	mock.ExpectQuery(`FROM memories m LEFT JOIN`).WillReturnError(errors.New("boom"))

	tx, _ := d.sql.Begin()
	defer func() { _ = tx.Rollback() }()

	if _, _, err := d.captureMemoryCallback(tx, []string{"m1"}, CauseConsolidation); err == nil {
		t.Fatal("a failed capture returned no error")
	}
}

// TestCaptureEventsIgnoresAnEmptyIdList covers the early return that lets the event delete paths
// call the capture without first checking they have anything to capture.
func TestCaptureEventsIgnoresAnEmptyIdList(t *testing.T) {
	d := callbackTestDB(t, CallbackPolicy{Enabled: true, MemoryEvents: true, EventEvents: true})

	tx, err := d.sql.Begin()
	if err != nil {
		t.Fatalf("Begin: %s", err.Error())
	}

	defer func() { _ = tx.Rollback() }()

	items, err := d.captureEvents(tx, nil)
	if err != nil || items != nil {
		t.Errorf("captureEvents(nil) = (%v, %v), want (nil, nil)", items, err)
	}
}

// TestDeleteCallbackQueueReportsZeroWhenTheCountIsUnavailable pins that a driver which cannot report
// RowsAffected does not turn a successful delete into an error. The rows are gone either way; only
// the number is unknown, and reporting 0 is the honest answer to "how many", not a failure.
func TestDeleteCallbackQueueReportsZeroWhenTheCountIsUnavailable(t *testing.T) {
	d, mock := mockCallbackDB(t)

	mock.ExpectExec(`DELETE FROM callback_queue`).
		WillReturnResult(sqlmock.NewErrorResult(errors.New("no row count from this driver")))

	deleted, err := d.DeleteCallbackQueue(context.Background(), 0)
	if err != nil {
		t.Fatalf("DeleteCallbackQueue = %s, want a successful delete with an unknown count", err.Error())
	}

	if deleted != 0 {
		t.Errorf("deleted = %d, want 0 when the count is unavailable", deleted)
	}

	expectationsMet(t, mock)
}
