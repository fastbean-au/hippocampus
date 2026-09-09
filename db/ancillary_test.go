package db

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// TestAncillaryStorageReportsWhatUsedBytesExcludes is the point of the whole file: the figure an
// operator is shown and the figure the capacity target ignores have to be the same figure, or the
// console reassures about one number while eviction runs on another.
//
// It asserts the identity rather than the arithmetic - a change to any of the three per-row
// allowances is meant to move both sides together.
func TestAncillaryStorageReportsWhatUsedBytesExcludes(t *testing.T) {
	d := recordingDB(t, TombstonePolicy{Enabled: true})

	ctx := context.Background()

	for i := range 200 {
		seedForgettableMemory(t, d, fmt.Sprintf("m%d", i), "", "")
	}

	if _, err := d.ConsolidateMemories(ctx, forgetAll{}); err != nil {
		t.Fatalf("ConsolidateMemories: %s", err)
	}

	measured, err := d.AncillaryStorage(ctx)
	if err != nil {
		t.Fatalf("AncillaryStorage: %s", err)
	}

	if !measured.ForgottenLog.Enabled {
		t.Error("the forgotten log is enabled on this store but reports enabled false")
	}

	if measured.ForgottenLog.Rows != 200 {
		t.Errorf("forgotten log rows = %d, want the 200 memories the pass forgot", measured.ForgottenLog.Rows)
	}

	if want := int64(200) * tombstoneRowBytes; measured.ForgottenLog.Bytes != want {
		t.Errorf("forgotten log bytes = %d, want %d (rows times the flat allowance)", measured.ForgottenLog.Bytes, want)
	}

	if measured.TotalBytes() != measured.ForgottenLog.Bytes {
		t.Errorf("total = %d with only the log enabled, want %d", measured.TotalBytes(), measured.ForgottenLog.Bytes)
	}

	// The identity: emptying the log has to move UsedBytes by what was reported as being outside it.
	withLog, err := d.UsedBytes(ctx)
	if err != nil {
		t.Fatalf("UsedBytes: %s", err)
	}

	if _, err := d.DeleteForgottenMemories(ctx, 0, nil); err != nil {
		t.Fatalf("DeleteForgottenMemories: %s", err)
	}

	emptied, err := d.AncillaryStorage(ctx)
	if err != nil {
		t.Fatalf("AncillaryStorage after emptying: %s", err)
	}

	if emptied.TotalBytes() != 0 {
		t.Errorf("an emptied log still reports %d bytes outside the target", emptied.TotalBytes())
	}

	// Still enabled, and that is the distinction the wire carries: a table holding nothing is not
	// the same fact as a table nobody turned on.
	if !emptied.ForgottenLog.Enabled {
		t.Error("emptying the log reported it as disabled")
	}

	withoutLog, err := d.UsedBytes(ctx)
	if err != nil {
		t.Fatalf("UsedBytes after emptying: %s", err)
	}

	// Page accounting is coarse, so the two readings need not differ by exactly the allowance - what
	// must hold is that the allowance was being subtracted while the rows were there, so removing
	// them cannot have made the store look SMALLER than the exclusion already claimed.
	if withoutLog < withLog-measured.ForgottenLog.Bytes {
		t.Errorf("UsedBytes fell from %d to %d after emptying a log worth %d - the exclusion double-counted",
			withLog,
			withoutLog,
			measured.ForgottenLog.Bytes,
		)
	}
}

// TestAncillaryStorageSeparatesDisabledFromEmpty pins the field the console's two zeroes depend on.
// A store with none of the three features enabled reports three disabled tables, not three empty
// ones - the second reads as "these queues are keeping up", which is the opposite conclusion.
func TestAncillaryStorageSeparatesDisabledFromEmpty(t *testing.T) {
	d := newTestDB(t)

	measured, err := d.AncillaryStorage(context.Background())
	if err != nil {
		t.Fatalf("AncillaryStorage: %s", err)
	}

	for name, table := range map[string]AncillaryTable{
		"forgotten log":  measured.ForgottenLog,
		"search outbox":  measured.SearchOutbox,
		"callback queue": measured.CallbackQueue,
	} {
		if table.Enabled {
			t.Errorf("%s reports enabled on a store that has not turned it on", name)
		}

		if table.Rows != 0 || table.Bytes != 0 {
			t.Errorf("%s reports %d rows / %d bytes while disabled", name, table.Rows, table.Bytes)
		}
	}

	if measured.TotalBytes() != 0 {
		t.Errorf("total = %d with nothing enabled", measured.TotalBytes())
	}
}

// TestAncillaryStorageCountsTheSearchOutbox covers the second table, and with it the fact that a
// deletion queue's size is measured while the deletions are still owed - which is the state the
// whole report exists for, since that is precisely when the queue is growing.
func TestAncillaryStorageCountsTheSearchOutbox(t *testing.T) {
	d := newTestDB(t)
	d.SetSearchOutbox(true)

	ctx := context.Background()

	for i := range 5 {
		seedForgettableMemory(t, d, fmt.Sprintf("m%d", i), "", "")
	}

	if _, err := d.ConsolidateMemories(ctx, forgetAll{}); err != nil {
		t.Fatalf("ConsolidateMemories: %s", err)
	}

	measured, err := d.AncillaryStorage(ctx)
	if err != nil {
		t.Fatalf("AncillaryStorage: %s", err)
	}

	if !measured.SearchOutbox.Enabled || measured.SearchOutbox.Rows != 5 {
		t.Fatalf("search outbox = %+v, want 5 enabled rows", measured.SearchOutbox)
	}

	if want := int64(5) * outboxRowBytes; measured.SearchOutbox.Bytes != want {
		t.Errorf("search outbox bytes = %d, want %d", measured.SearchOutbox.Bytes, want)
	}

	if measured.TotalBytes() != measured.SearchOutbox.Bytes {
		t.Errorf("total = %d, want the outbox's %d", measured.TotalBytes(), measured.SearchOutbox.Bytes)
	}
}

// TestAncillaryStorageCountsATableItIsNoLongerRecordingInto is the state disabling the feature
// leaves behind, and the one the report must not hide: turning the forgotten log off stops the
// writing and the trimming, and leaves every row already written exactly where it is. Those bytes
// are still outside the capacity target, so they are still the operator's problem - what changes is
// only that the setting is no longer what put them there.
func TestAncillaryStorageCountsATableItIsNoLongerRecordingInto(t *testing.T) {
	d := recordingDB(t, TombstonePolicy{Enabled: true})

	ctx := context.Background()

	for i := range 10 {
		seedForgettableMemory(t, d, fmt.Sprintf("m%d", i), "", "")
	}

	if _, err := d.ConsolidateMemories(ctx, forgetAll{}); err != nil {
		t.Fatalf("ConsolidateMemories: %s", err)
	}

	d.SetTombstonePolicy(TombstonePolicy{Enabled: false})

	measured, err := d.AncillaryStorage(ctx)
	if err != nil {
		t.Fatalf("AncillaryStorage: %s", err)
	}

	if measured.ForgottenLog.Enabled {
		t.Error("the log reports enabled after the policy was switched off")
	}

	if measured.ForgottenLog.Rows != 10 {
		t.Errorf("forgotten log rows = %d after disabling, want the 10 rows still stored", measured.ForgottenLog.Rows)
	}

	if measured.TotalBytes() == 0 {
		t.Error("a log holding 10 rows reports no bytes outside the capacity target")
	}
}

// TestAncillaryStorageReportsAFailedMeasurement is the counterpart of TestTombstoneBytesUnavailable,
// and the asymmetry is the point: the same failed count is swallowed where the figure is SUBTRACTED
// (which merely over-counts the store, so it errs toward forgetting a little harder) and returned
// where it is REPORTED, because zero bytes reads as "nothing is accumulating" - the false
// reassurance this report exists to end.
//
// Each of the three tables is driven in turn, since a measurement that gave up quietly on the second
// or third would report a total that looked plausible and was short.
func TestAncillaryStorageReportsAFailedMeasurement(t *testing.T) {
	for _, table := range []string{"memory_tombstones", "search_outbox", "callback_queue"} {
		t.Run(table, func(t *testing.T) {
			d, mock := newMockDB(t, driverSQLite)
			d.tombstoneTable = true
			d.searchOutbox = true
			d.callbackTable = true

			// The tables are measured in a fixed order, so every one before the failing one answers
			// first.
			for _, before := range []string{"memory_tombstones", "search_outbox", "callback_queue"} {
				if before == table {
					break
				}

				mock.ExpectQuery(`SELECT COUNT\(\*\) FROM ` + before).
					WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
			}

			mock.ExpectQuery(`SELECT COUNT\(\*\) FROM ` + table).WillReturnError(errors.New("boom"))

			measured, err := d.AncillaryStorage(context.Background())
			if err == nil {
				t.Fatalf("a failed count of %s reported %d bytes rather than an error", table, measured.TotalBytes())
			}

			if measured.TotalBytes() != 0 {
				t.Errorf("a failed measurement returned a partial total of %d", measured.TotalBytes())
			}

			expectationsMet(t, mock)
		})
	}
}
