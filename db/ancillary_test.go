package db

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

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

	measured, err := d.AncillaryStorage(ctx, AncillaryBounds{})
	if err != nil {
		t.Fatalf("AncillaryStorage: %s", err)
	}

	if !measured.ForgottenLog.Enabled {
		t.Error("the forgotten log is enabled on this store but reports enabled false")
	}

	if measured.ForgottenLog.Rows != 200 {
		t.Errorf("forgotten log rows = %d, want the 200 memories the pass forgot", measured.ForgottenLog.Rows)
	}

	if want := int64(200) * d.dialect().tombstoneRowBytes; measured.ForgottenLog.Bytes != want {
		t.Errorf("forgotten log bytes = %d, want %d (rows times the flat allowance)", measured.ForgottenLog.Bytes, want)
	}

	if structuralBytes(measured) != measured.ForgottenLog.Bytes {
		t.Errorf("the structural total is %d with only the log enabled, want %d",
			structuralBytes(measured), measured.ForgottenLog.Bytes)
	}

	// The identity: emptying the log has to move UsedBytes by what was reported as being outside it.
	withLog, err := d.UsedBytes(ctx)
	if err != nil {
		t.Fatalf("UsedBytes: %s", err)
	}

	if _, err := d.DeleteForgottenMemories(ctx, 0, nil); err != nil {
		t.Fatalf("DeleteForgottenMemories: %s", err)
	}

	emptied, err := d.AncillaryStorage(ctx, AncillaryBounds{})
	if err != nil {
		t.Fatalf("AncillaryStorage after emptying: %s", err)
	}

	// Structural bytes, not the footprint: an emptied table holds no rows, but on a server dialect
	// the relation keeps the pages it allocated until the engine reuses them - which is real disk
	// and is exactly what DiskBytes exists to report. UsedBytes excludes the structural figure, so
	// that is the one this identity is about.
	if structuralBytes(emptied) != 0 {
		t.Errorf("an emptied log still reports %d structural bytes outside the target", structuralBytes(emptied))
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

	measured, err := d.AncillaryStorage(context.Background(), AncillaryBounds{})
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

	if structuralBytes(measured) != 0 {
		t.Errorf("the structural total is %d with nothing enabled", structuralBytes(measured))
	}
}

// structuralBytes sums what the three tables' ROWS occupy, which is the figure UsedBytes excludes
// and the caps are enforced in. AncillaryStorage.TotalBytes deliberately sums the footprint instead
// - what the engine is holding, unreclaimed space included - so it is not the figure these identities
// are about, and on a server dialect the two differ by whatever the engine has not yet reused.
func structuralBytes(storage AncillaryStorage) int64 {
	return storage.ForgottenLog.Bytes + storage.SearchOutbox.Bytes + storage.CallbackQueue.Bytes
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

	measured, err := d.AncillaryStorage(ctx, AncillaryBounds{})
	if err != nil {
		t.Fatalf("AncillaryStorage: %s", err)
	}

	if !measured.SearchOutbox.Enabled || measured.SearchOutbox.Rows != 5 {
		t.Fatalf("search outbox = %+v, want 5 enabled rows", measured.SearchOutbox)
	}

	if want := int64(5) * d.dialect().outboxRowBytes; measured.SearchOutbox.Bytes != want {
		t.Errorf("search outbox bytes = %d, want %d", measured.SearchOutbox.Bytes, want)
	}

	if structuralBytes(measured) != measured.SearchOutbox.Bytes {
		t.Errorf("the structural total is %d, want the outbox's %d",
			structuralBytes(measured), measured.SearchOutbox.Bytes)
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

	measured, err := d.AncillaryStorage(ctx, AncillaryBounds{})
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

				mock.ExpectQuery(`SELECT COUNT\(\*\).* FROM ` + before).
					WillReturnRows(sqlmock.NewRows([]string{"count", "payload", "oldest"}).AddRow(1, 0, 0))
			}

			mock.ExpectQuery(`SELECT COUNT\(\*\).* FROM ` + table).WillReturnError(errors.New("boom"))

			measured, err := d.AncillaryStorage(context.Background(), AncillaryBounds{})
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

// TestRowsWithinBytes is the whole implementation of the byte cap on the two fixed-width tables, so
// it is worth stating exactly. Zero on either side means unbounded on that side, the tighter of the
// two wins, and a cap below one row's allowance resolves to one row rather than to zero - which the
// prune paths would read as "no row cap", making the strictest bound an operator can express into
// no bound at all.
func TestRowsWithinBytes(t *testing.T) {
	for _, one := range []struct {
		name     string
		bounds   QueueBounds
		rowBytes int64
		want     int64
	}{
		{"neither bound", QueueBounds{}, 192, 0},
		{"rows only", QueueBounds{MaxRows: 500}, 192, 500},
		{"bytes only", QueueBounds{MaxBytes: 1920}, 192, 10},
		{"bytes are tighter", QueueBounds{MaxRows: 500, MaxBytes: 1920}, 192, 10},
		{"rows are tighter", QueueBounds{MaxRows: 5, MaxBytes: 1920}, 192, 5},
		{"bytes below one row still keep one", QueueBounds{MaxBytes: 10}, 192, 1},
		{"an unknown allowance leaves the row cap alone", QueueBounds{MaxRows: 7, MaxBytes: 1920}, 0, 7},
	} {
		t.Run(one.name, func(t *testing.T) {
			if got := rowsWithinBytes(one.bounds, one.rowBytes); got != one.want {
				t.Errorf("rowsWithinBytes(%+v, %d) = %d, want %d", one.bounds, one.rowBytes, got, one.want)
			}
		})
	}
}

// TestEffectiveRowCapNamesTheSettingThatWon is the half of the same arithmetic the report reads. The
// number was always resolved; what was missing was which of the two settings produced it, and a
// second derivation from the bounds is exactly what would let a report say "rows" while the prune
// applies the byte cap.
func TestEffectiveRowCapNamesTheSettingThatWon(t *testing.T) {
	for _, one := range []struct {
		name   string
		bounds QueueBounds
		want   AncillaryBinding
	}{
		{"neither bound", QueueBounds{}, BindingNone},
		{"rows only", QueueBounds{MaxRows: 500}, BindingRows},
		{"bytes only", QueueBounds{MaxBytes: 1920}, BindingBytes},
		{"bytes are tighter", QueueBounds{MaxRows: 500, MaxBytes: 1920}, BindingBytes},
		{"rows are tighter", QueueBounds{MaxRows: 5, MaxBytes: 1920}, BindingRows},
		// An age cap alone leaves no row cap at all, and the SOURCE of a row cap is still none - what
		// turns that into BindingAge is bindingLimit, which is the only place the three caps meet.
		{"age only", QueueBounds{MaxAge: time.Hour}, BindingNone},
	} {
		t.Run(one.name, func(t *testing.T) {
			if _, got := effectiveRowCap(one.bounds, 192); got != one.want {
				t.Errorf("effectiveRowCap(%+v) named %q, want %q", one.bounds, got, one.want)
			}
		})
	}
}

// TestBindingLimit is TODO-2 item 126's answer in one table: which cap is actually deciding what a
// table drops.
//
// The case that matters is the last two. A table AT its row cap while an age cap is also configured
// is bound by the row cap, and the age cap is then a window the store forgets too fast to reach -
// which is the state that reported nothing anywhere, since both caps were enforced and neither was
// violated. Below the row cap, the age cap is by elimination the only bound that can act, and that
// is decided by elimination rather than by comparing the span against the window: the prune has just
// removed everything older, so an age-bound table's span is always a little UNDER its own cap.
func TestBindingLimit(t *testing.T) {
	const rowBytes = 192

	for _, one := range []struct {
		name   string
		bounds QueueBounds
		rows   int64
		want   AncillaryBinding
		cap    int64
	}{
		{"unbounded", QueueBounds{}, 1000, BindingNone, 0},
		{"inside the row cap with no age cap", QueueBounds{MaxRows: 500}, 100, BindingNone, 500},
		{"at the row cap", QueueBounds{MaxRows: 500}, 500, BindingRows, 500},
		{"over the row cap", QueueBounds{MaxRows: 500}, 900, BindingRows, 500},
		{"at the cap the byte bound set", QueueBounds{MaxBytes: 1920}, 10, BindingBytes, 10},
		{"inside every cap but an age cap is set", QueueBounds{MaxRows: 500, MaxAge: time.Hour}, 100, BindingAge, 500},
		{"the row cap wins over the age cap", QueueBounds{MaxRows: 500, MaxAge: time.Hour}, 500, BindingRows, 500},
	} {
		t.Run(one.name, func(t *testing.T) {
			binding, rowCap := bindingLimit(one.bounds, rowBytes, one.rows)

			if binding != one.want {
				t.Errorf("bindingLimit(%+v, %d rows) = %q, want %q", one.bounds, one.rows, binding, one.want)
			}

			if rowCap != one.cap {
				t.Errorf("the effective row cap is %d, want %d", rowCap, one.cap)
			}
		})
	}
}

// TestFootprintPrefersTheMeasurement pins which of the two byte figures answers "how much disk".
// Bytes is the structural estimate the caps are enforced in and DiskBytes is what the engine says it
// is really holding, so a table with both reports the second - anything else has the gauge and the
// alert reading the smaller of two available answers.
func TestFootprintPrefersTheMeasurement(t *testing.T) {
	if got := (AncillaryTable{Bytes: 100, DiskBytes: 250}).Footprint(); got != 250 {
		t.Errorf("footprint = %d, want the measured 250", got)
	}

	if got := (AncillaryTable{Bytes: 100}).Footprint(); got != 100 {
		t.Errorf("footprint = %d, want the estimated 100 where nothing measured it", got)
	}

	// And the total is summed from those, not from Bytes: a disk is sized against what the engine is
	// holding.
	storage := AncillaryStorage{
		ForgottenLog:  AncillaryTable{Bytes: 100, DiskBytes: 250},
		SearchOutbox:  AncillaryTable{Bytes: 10},
		CallbackQueue: AncillaryTable{Bytes: 5, DiskBytes: 40},
	}

	if got := storage.TotalBytes(); got != 300 {
		t.Errorf("total = %d, want 300 - the two measurements plus the one estimate", got)
	}
}

// TestAncillaryReportsHowFarBackATableReaches is the other half of item 126: a row count says
// nothing about how much history it is, and the deployment that raised this held five days of a
// window it had configured as thirty.
func TestAncillaryReportsHowFarBackATableReaches(t *testing.T) {
	d := recordingDB(t, TombstonePolicy{Enabled: true, MaxRows: 2, MaxAgeInDays: 30})

	ctx := context.Background()

	before := time.Now().UnixNano()

	for i := range 5 {
		seedForgettableMemory(t, d, fmt.Sprintf("m%d", i), "", "")
	}

	if _, err := d.ConsolidateMemories(ctx, forgetAll{}); err != nil {
		t.Fatalf("ConsolidateMemories: %s", err)
	}

	if _, err := d.PruneTombstones(ctx); err != nil {
		t.Fatalf("PruneTombstones: %s", err)
	}

	measured, err := d.AncillaryStorage(ctx, AncillaryBounds{})
	if err != nil {
		t.Fatalf("AncillaryStorage: %s", err)
	}

	forgotten := measured.ForgottenLog

	if forgotten.Oldest < before {
		t.Errorf("the oldest tombstone is reported at %d, before the run began at %d", forgotten.Oldest, before)
	}

	// The row cap took it to two rows well inside the thirty-day window, so the row cap is what is
	// deciding what this log drops and the age cap is unreachable - which is the whole finding.
	if forgotten.Binding != BindingRows {
		t.Errorf("binding = %q, want %q: two rows of a five-row run is the row cap acting",
			forgotten.Binding, BindingRows)
	}

	if forgotten.LimitRows != 2 {
		t.Errorf("limit_rows = %d, want the configured 2", forgotten.LimitRows)
	}

	if forgotten.LimitAge != 30*24*time.Hour {
		t.Errorf("limit_age = %s, want 720h", forgotten.LimitAge)
	}
}

// TestAnEmptyTableReportsNoOldestRow is the boundary MIN returns NULL at, and a COALESCE that was
// missing would fail the scan rather than report a zero.
func TestAnEmptyTableReportsNoOldestRow(t *testing.T) {
	d := recordingDB(t, TombstonePolicy{Enabled: true})

	measured, err := d.AncillaryStorage(context.Background(), AncillaryBounds{})
	if err != nil {
		t.Fatalf("AncillaryStorage: %s", err)
	}

	if measured.ForgottenLog.Oldest != 0 {
		t.Errorf("an empty log reports its oldest row at %d, want 0", measured.ForgottenLog.Oldest)
	}
}

// TestRelationBytesIsSilentWhereTheDialectCannotAnswer pins the embedded dialect's half of the
// footprint: it declares no relation measurement, so nothing is queried and the structural estimate
// stands. The alternative - a per-relation reading from dbstat - is a walk of the table's pages, and
// this runs once per sleep cycle.
func TestRelationBytesIsSilentWhereTheDialectCannotAnswer(t *testing.T) {
	d := newTestDB(t)

	if d.dialect().relationBytes != "" {
		t.Skip("this dialect measures its relations; the case under test is the one that does not")
	}

	bytes, err := d.relationBytes(context.Background(), tombstonesTable)
	if err != nil {
		t.Fatalf("relationBytes: %s", err)
	}

	if bytes != 0 {
		t.Errorf("a dialect with no relation measurement reported %d bytes", bytes)
	}
}

// TestRelationBytesReportsAFailure keeps the measurement on the report's error policy rather than
// the exclusion's: a reading that cannot be taken is returned, because a footprint quietly falling
// back to the estimate would understate by exactly the thing it was added to show.
func TestRelationBytesReportsAFailure(t *testing.T) {
	d, mock := newMockDB(t, driverPostgres)

	mock.ExpectQuery(`pg_total_relation_size`).WillReturnError(errors.New("boom"))

	if _, err := d.relationBytes(context.Background(), tombstonesTable); err == nil {
		t.Fatal("a failed relation measurement was reported as zero bytes")
	}

	expectationsMet(t, mock)
}
