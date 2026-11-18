package db

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/fastbean-au/hippocampus/types"
)

// This file targets expiry.go's remaining coverage gaps: the scan's failures, scripted on a mocked
// handle, and the delete and event follow-up failures, injected on SQLite with a trigger that aborts
// the one statement under test.

// expiryFailureDB is a store holding one memory past the ceiling in event e1, with a trigger that
// aborts every statement of the given kind on the given table.
func expiryFailureDB(t *testing.T, trigger string) (*DB, int64) {
	t.Helper()

	requireSQLite(t) // a trigger is the portable-enough way to fail one statement

	d := newTestDB(t)
	ctx := context.Background()

	old := time.Now().Add(-100 * 24 * time.Hour).UnixNano()

	if _, err := d.CreateEvent(ctx, types.Event{Id: "e1", Name: "e1", TimeStart: old, Significance: 5}); err != nil {
		t.Fatalf("CreateEvent: %s", err)
	}

	if _, err := d.CreateMemory(ctx, types.Memory{Id: "m1", TimeStamp: old, Significance: 5, EventId: "e1", Body: "x"}); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	if _, err := d.exec(ctx, `CREATE TRIGGER fail_expiry `+trigger+` BEGIN SELECT RAISE(ABORT, 'injected'); END`); err != nil {
		t.Fatalf("creating the trigger: %s", err)
	}

	return d, time.Now().Add(-90 * 24 * time.Hour).UnixNano()
}

// TestExpireMemories_DeleteFails: a memory delete that fails is reported, with nothing counted as
// expired.
func TestExpireMemories_DeleteFails(t *testing.T) {
	d, cutoff := expiryFailureDB(t, `BEFORE DELETE ON memories`)

	result, err := d.ExpireMemories(context.Background(), cutoff)
	if err == nil {
		t.Fatal("expected ExpireMemories to report the failed delete")
	}

	if result.Memories != 0 || result.Events != 0 {
		t.Errorf("a failed delete reported %+v as expired, want nothing", result)
	}
}

// TestExpireMemories_EmptiedEventDeleteFails: the memory goes, the event it emptied cannot, and the
// failure is reported alongside the memory that did expire.
func TestExpireMemories_EmptiedEventDeleteFails(t *testing.T) {
	d, cutoff := expiryFailureDB(t, `BEFORE DELETE ON events`)

	result, err := d.ExpireMemories(context.Background(), cutoff)
	if err == nil {
		t.Fatal("expected ExpireMemories to report the failed event delete")
	}

	if result.Memories != 1 || result.Events != 0 {
		t.Errorf("expired %+v, want the memory and not the event", result)
	}
}

// TestExpireMemories_ConsolidatedFlagFails: an event that keeps a memory is flagged consolidated, and
// a flag that cannot be written is reported rather than lost.
func TestExpireMemories_ConsolidatedFlagFails(t *testing.T) {
	d, cutoff := expiryFailureDB(t, `BEFORE UPDATE OF memories_consolidated ON events`)

	if _, err := d.CreateMemory(context.Background(), types.Memory{
		Id: "m2", TimeStamp: time.Now().UnixNano(), Significance: 5, EventId: "e1", Body: "x",
	}); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	result, err := d.ExpireMemories(context.Background(), cutoff)
	if err == nil {
		t.Fatal("expected ExpireMemories to report the failed consolidated flag")
	}

	if result.Memories != 1 || result.Events != 0 {
		t.Errorf("expired %+v, want the old memory and not the event", result)
	}
}

// TestExpireMemories_ScanFailures: the scan's query, a row that does not scan and a failed iteration
// each stop the pass before anything is deleted.
func TestExpireMemories_ScanFailures(t *testing.T) {
	columns := []string{"id", "time_recalled", "recall_count", "event_id"}

	cases := map[string]func(mock sqlmock.Sqlmock){
		"query": func(mock sqlmock.Sqlmock) {
			mock.ExpectQuery("SELECT id, time_recalled, recall_count, event_id FROM memories").WillReturnError(errChangeLogMock)
		},
		"scan": func(mock sqlmock.Sqlmock) {
			mock.ExpectQuery("SELECT id, time_recalled, recall_count, event_id FROM memories").
				WillReturnRows(sqlmock.NewRows(columns).AddRow("m1", "not a time", 0, ""))
		},
		"iteration": func(mock sqlmock.Sqlmock) {
			mock.ExpectQuery("SELECT id, time_recalled, recall_count, event_id FROM memories").
				WillReturnRows(sqlmock.NewRows(columns).AddRow("m1", 0, 0, "").RowError(0, errChangeLogMock))
		},
	}

	for k, v := range cases {
		t.Run(k, func(t *testing.T) {
			d, mock := newMockDB(t, driverSQLite)

			v(mock)

			if _, err := d.ExpireMemories(context.Background(), 1); err == nil {
				t.Errorf("expected the %s failure to stop the pass", k)
			}

			expectationsMet(t, mock)
		})
	}
}
