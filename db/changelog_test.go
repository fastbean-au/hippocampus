package db

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/fastbean-au/hippocampus/types"
)

// The change log (TODO-3 item 204). These hold the store's half: with the log on, every change the
// feed carries is a row, written with the change and only if it committed, carrying no body; the
// in-process sink is not fed as well; and the caps, the purge and the capacity exclusion treat it as
// they treat the forgotten log.

// changeLogTestDB opens a store recording into the change log, with a sink registered so the test
// can see that nothing reaches it in process.
func changeLogTestDB(t *testing.T, policy ChangeLogPolicy) (*DB, *recordingSink) {
	t.Helper()

	policy.Enabled = true

	d, sink := watchedTestDB(t, CallbackPolicy{})
	d.SetChangeLogPolicy(policy)

	return d, sink
}

// readAllChanges is the whole log, oldest first.
func readAllChanges(t *testing.T, d *DB) []Change {
	t.Helper()

	changes, err := d.ReadChanges(context.Background(), 0, 0, 1000)
	if err != nil {
		t.Fatalf("ReadChanges: %s", err)
	}

	return changes
}

// TestTheChangeLogRecordsEveryChange: each kind the feed carries is a row, in commit order, with its
// group and without the body - and the in-process sink hears nothing, since every instance's reader
// publishes from the log.
func TestTheChangeLogRecordsEveryChange(t *testing.T) {
	d, sink := changeLogTestDB(t, ChangeLogPolicy{})
	ctx := context.Background()

	if _, err := d.CreateEvent(ctx, types.Event{Id: "e1", Name: "e", TimeStart: 100, Significance: 5, Group: "g"}); err != nil {
		t.Fatalf("CreateEvent: %s", err)
	}

	if _, err := d.CreateMemory(ctx, types.Memory{Id: "m1", Body: "secret body", TimeStamp: 100, Significance: 5, Group: "g"}); err != nil {
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

	changes := readAllChanges(t, d)

	want := []CallbackKind{
		CallbackKindMemoryStored,
		CallbackKindMemoryUpdated,
		CallbackKindMemoryForgotten,
		CallbackKindEventForgotten,
	}

	if len(changes) != len(want) {
		t.Fatalf("the log holds %d changes, want %d: %+v", len(changes), len(want), changes)
	}

	for i, change := range changes {
		if change.Kind != want[i] {
			t.Errorf("change %d is kind %d, want %d", i, change.Kind, want[i])
		}

		if i > 0 && change.Seq <= changes[i-1].Seq {
			t.Errorf("change %d has seq %d after %d: the log is not in commit order", i, change.Seq, changes[i-1].Seq)
		}

		if change.RecordedAt == 0 {
			t.Errorf("change %d has no recorded_at", i)
		}

		if len(change.Items) != 1 || change.Items[0].Group != "g" {
			t.Errorf("change %d carries %+v, want one item in group g", i, change.Items)
		}

		for _, item := range change.Items {
			if item.Body != "" {
				t.Errorf("change %d carries a body", i)
			}
		}
	}

	if changes[2].Cause != CauseClient {
		t.Errorf("the client delete is recorded with cause %d, want %d", changes[2].Cause, CauseClient)
	}

	if published := sink.published(); len(published) != 0 {
		t.Errorf("the in-process sink was handed %d deliveries with the log on; each would reach a watcher twice", len(published))
	}

	head, err := d.ChangeLogHead(ctx)
	if err != nil {
		t.Fatalf("ChangeLogHead: %s", err)
	}

	if head != changes[len(changes)-1].Seq {
		t.Errorf("the head is %d, want the newest seq %d", head, changes[len(changes)-1].Seq)
	}
}

// TestTheChangeLogRecordsOnlyWhatCommitted: a create that wrote nothing (the id was already held)
// rolls back, and leaves no row behind.
func TestTheChangeLogRecordsOnlyWhatCommitted(t *testing.T) {
	d, _ := changeLogTestDB(t, ChangeLogPolicy{})
	ctx := context.Background()

	if _, err := d.CreateMemory(ctx, types.Memory{Id: "m1", Body: "one", TimeStamp: 100, Significance: 5}); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	// The second create of the same id is skipped by the store, so its transaction rolls back.
	_, _ = d.CreateMemory(ctx, types.Memory{Id: "m1", Body: "two", TimeStamp: 100, Significance: 5})

	if changes := readAllChanges(t, d); len(changes) != 1 {
		t.Errorf("the log holds %d changes, want only the create that committed: %+v", len(changes), changes)
	}
}

// TestTheChangeLogIsOffByDefault: without the policy, nothing is recorded and the in-process feed is
// exactly what it was.
func TestTheChangeLogIsOffByDefault(t *testing.T) {
	d, sink := watchedTestDB(t, CallbackPolicy{})
	ctx := context.Background()

	if _, err := d.CreateMemory(ctx, types.Memory{Id: "m1", Body: "one", TimeStamp: 100, Significance: 5}); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	if d.ChangeLogging() {
		t.Error("ChangeLogging is true with no policy installed")
	}

	if changes := readAllChanges(t, d); len(changes) != 0 {
		t.Errorf("the log holds %d changes with the feature off", len(changes))
	}

	if published := sink.published(); len(published) != 1 {
		t.Errorf("the in-process sink was handed %d deliveries, want 1", len(published))
	}

	if err := d.AppendChange(ctx, Change{Kind: CallbackKindSleepCompleted}); err != nil {
		t.Fatalf("AppendChange: %s", err)
	}

	if changes := readAllChanges(t, d); len(changes) != 0 {
		t.Errorf("AppendChange recorded a change with the feature off")
	}

	if pruned, err := d.PruneChangeLog(ctx); err != nil || pruned != 0 {
		t.Errorf("PruneChangeLog with the feature off = %d, %v; want a no-op", pruned, err)
	}
}

// TestAppendChangeKeepsTheReport: a cycle's completion carries the service's encoded report through
// the log untouched.
func TestAppendChangeKeepsTheReport(t *testing.T) {
	d, _ := changeLogTestDB(t, ChangeLogPolicy{})
	ctx := context.Background()

	report := []byte(`{"trigger":"timer"}`)

	if err := d.AppendChange(ctx, Change{Kind: CallbackKindSleepCompleted, CycleId: 7, Report: report}); err != nil {
		t.Fatalf("AppendChange: %s", err)
	}

	changes := readAllChanges(t, d)

	if len(changes) != 1 {
		t.Fatalf("the log holds %d changes, want 1", len(changes))
	}

	if changes[0].Kind != CallbackKindSleepCompleted || changes[0].CycleId != 7 || string(changes[0].Report) != string(report) {
		t.Errorf("the completion read back as %+v", changes[0])
	}
}

// TestReadChangesHonoursItsBounds: after is exclusive, through inclusive, and limit caps the page.
func TestReadChangesHonoursItsBounds(t *testing.T) {
	d, _ := changeLogTestDB(t, ChangeLogPolicy{})
	ctx := context.Background()

	for range 5 {
		if err := d.AppendChange(ctx, Change{Kind: CallbackKindSleepCompleted}); err != nil {
			t.Fatalf("AppendChange: %s", err)
		}
	}

	all := readAllChanges(t, d)

	if len(all) != 5 {
		t.Fatalf("the log holds %d changes, want 5", len(all))
	}

	page, err := d.ReadChanges(ctx, all[0].Seq, all[3].Seq, 10)
	if err != nil {
		t.Fatalf("ReadChanges: %s", err)
	}

	if len(page) != 3 || page[0].Seq != all[1].Seq || page[2].Seq != all[3].Seq {
		t.Errorf("ReadChanges(after %d, through %d) = %+v, want seqs %d..%d", all[0].Seq, all[3].Seq, page, all[1].Seq, all[3].Seq)
	}

	limited, err := d.ReadChanges(ctx, 0, 0, 2)
	if err != nil {
		t.Fatalf("ReadChanges: %s", err)
	}

	if len(limited) != 2 {
		t.Errorf("a limit of 2 returned %d changes", len(limited))
	}

	present, err := d.HasChange(ctx, all[2].Seq)
	if err != nil || !present {
		t.Errorf("HasChange(%d) = %v, %v; want true", all[2].Seq, present, err)
	}

	absent, err := d.HasChange(ctx, all[4].Seq+100)
	if err != nil || absent {
		t.Errorf("HasChange of a seq never written = %v, %v; want false", absent, err)
	}
}

// TestPruneChangeLogAppliesItsCaps: the row cap keeps the newest rows, the byte cap keeps what fits,
// and the age cap removes what is older than the window.
func TestPruneChangeLogAppliesItsCaps(t *testing.T) {
	ctx := context.Background()

	t.Run("rows", func(t *testing.T) {
		d, _ := changeLogTestDB(t, ChangeLogPolicy{MaxRows: 3})

		for range 5 {
			if err := d.AppendChange(ctx, Change{Kind: CallbackKindSleepCompleted}); err != nil {
				t.Fatalf("AppendChange: %s", err)
			}
		}

		before := readAllChanges(t, d)

		pruned, err := d.PruneChangeLog(ctx)
		if err != nil {
			t.Fatalf("PruneChangeLog: %s", err)
		}

		after := readAllChanges(t, d)

		if pruned != 2 || len(after) != 3 || after[0].Seq != before[2].Seq {
			t.Errorf("the row cap pruned %d and kept %+v, want the newest 3", pruned, after)
		}
	})

	t.Run("bytes", func(t *testing.T) {
		report := []byte(strings.Repeat("r", 200))

		d, _ := changeLogTestDB(t, ChangeLogPolicy{MaxBytes: 2 * (changeLogRowOverheadBytes + 400)})

		for range 5 {
			if err := d.AppendChange(ctx, Change{Kind: CallbackKindSleepCompleted, Report: report}); err != nil {
				t.Fatalf("AppendChange: %s", err)
			}
		}

		if _, err := d.PruneChangeLog(ctx); err != nil {
			t.Fatalf("PruneChangeLog: %s", err)
		}

		after := readAllChanges(t, d)

		if len(after) == 0 || len(after) > 2 {
			t.Errorf("the byte cap kept %d rows, want between 1 and 2", len(after))
		}
	})

	t.Run("age", func(t *testing.T) {
		d, _ := changeLogTestDB(t, ChangeLogPolicy{MaxAge: time.Hour})

		old := time.Now().Add(-2 * time.Hour).UnixNano()

		if err := d.AppendChange(ctx, Change{Kind: CallbackKindSleepCompleted, RecordedAt: old}); err != nil {
			t.Fatalf("AppendChange: %s", err)
		}

		if err := d.AppendChange(ctx, Change{Kind: CallbackKindSleepCompleted}); err != nil {
			t.Fatalf("AppendChange: %s", err)
		}

		pruned, err := d.PruneChangeLog(ctx)
		if err != nil {
			t.Fatalf("PruneChangeLog: %s", err)
		}

		if after := readAllChanges(t, d); pruned != 1 || len(after) != 1 {
			t.Errorf("the age cap pruned %d and kept %d, want 1 and 1", pruned, len(after))
		}
	})
}

// TestPurgeEmptiesTheChangeLog: Purge is the explicit request to leave nothing behind, and a cursor
// into a purged store must fail its resume rather than replay a store that no longer exists.
func TestPurgeEmptiesTheChangeLog(t *testing.T) {
	d, _ := changeLogTestDB(t, ChangeLogPolicy{})
	ctx := context.Background()

	if err := d.AppendChange(ctx, Change{Kind: CallbackKindSleepCompleted}); err != nil {
		t.Fatalf("AppendChange: %s", err)
	}

	before := readAllChanges(t, d)

	if err := d.Purge(ctx); err != nil {
		t.Fatalf("Purge: %s", err)
	}

	if changes := readAllChanges(t, d); len(changes) != 0 {
		t.Errorf("%d changes survived a purge", len(changes))
	}

	// The seq carries on, so no cursor handed out before the purge can name a row written after it.
	if err := d.AppendChange(ctx, Change{Kind: CallbackKindSleepCompleted}); err != nil {
		t.Fatalf("AppendChange: %s", err)
	}

	if after := readAllChanges(t, d); len(after) != 1 || after[0].Seq <= before[0].Seq {
		t.Errorf("the first change after a purge has seq %+v, want one past %d", after, before[0].Seq)
	}
}

// TestTheChangeLogIsExcludedFromUsedBytes: the log grows with every write, so counting it would make
// writing evict. See TestCallbackQueueIsExcludedFromUsedBytes.
func TestTheChangeLogIsExcludedFromUsedBytes(t *testing.T) {
	requireSQLite(t)

	d, _ := changeLogTestDB(t, ChangeLogPolicy{})
	ctx := context.Background()

	before, err := d.UsedBytes(ctx)
	if err != nil {
		t.Fatalf("UsedBytes: %s", err)
	}

	// Rows the size real ones are: a handful of items, ids and groups, no body.
	items := make([]CallbackItem, 0, 8)

	for i := range 8 {
		items = append(items, CallbackItem{Id: strings.Repeat("a", 35) + string(rune('0'+i)), Group: "group", Significance: 5, Bytes: 1024})
	}

	for range 200 {
		if err := d.AppendChange(ctx, Change{Kind: CallbackKindMemoryForgotten, Cause: CauseConsolidation, Items: items}); err != nil {
			t.Fatalf("AppendChange: %s", err)
		}
	}

	after, err := d.UsedBytes(ctx)
	if err != nil {
		t.Fatalf("UsedBytes: %s", err)
	}

	if after > before {
		t.Errorf("two hundred logged changes raised UsedBytes from %d to %d - the log would evict live memories to make room for itself", before, after)
	}

	storage, err := d.AncillaryStorage(ctx, AncillaryBounds{})
	if err != nil {
		t.Fatalf("AncillaryStorage: %s", err)
	}

	if !storage.ChangeLog.Enabled || storage.ChangeLog.Rows != 200 || storage.ChangeLog.Bytes <= 200*changeLogRowOverheadBytes {
		t.Errorf("AncillaryStorage reports the change log as %+v, want 200 enabled rows with their payloads", storage.ChangeLog)
	}
}
