package db

import (
	"context"
	"testing"

	"github.com/fastbean-au/hippocampus/types"
)

// Event deletion and the memories that go with it (TODO-3 item 165). DeleteEvent took the event in
// one transaction and its memories in another, so a failure between them left the memories pointing
// at an event that no longer existed - and a retry answered NotFound, with no way left to finish the
// job.

func seedCascade(t *testing.T) *DB {
	t.Helper()

	d := newTestDB(t)

	mustCreateEvent(t, d, types.Event{Id: "e1", Name: "one", TimeStart: 100, Significance: 5, Group: "a"})
	mustCreateMemory(t, d, types.Memory{Id: "m1", Body: "one", TimeStamp: 100, Significance: 5, EventId: "e1", Group: "a"})
	mustCreateMemory(t, d, types.Memory{Id: "m2", Body: "two", TimeStamp: 100, Significance: 5, EventId: "e1", Group: "b"})

	return d
}

func eventMemoryIds(t *testing.T, d *DB, eventId string) []string {
	t.Helper()

	ids, err := d.MemoryIdsMatching(context.Background(), MemoryFilter{EventId: eventId, Limit: 100})
	if err != nil {
		t.Fatalf("MemoryIdsMatching: %s", err)
	}

	return ids
}

func TestDeleteEventCascadeDeletesTheEventAndItsMemories(t *testing.T) {
	d := seedCascade(t)

	got, err := d.DeleteEventCascade(context.Background(), "e1", EventCascade{DeleteMemories: true})
	if err != nil {
		t.Fatalf("DeleteEventCascade: %s", err)
	}

	if !got.Deleted || len(got.MemoryIds) != 2 {
		t.Errorf("result = %+v, want the event deleted with both memories", got)
	}

	if missing, _ := d.MissingIds(context.Background(), memoryGraph, []string{"m1", "m2"}); len(missing) != 2 {
		t.Errorf("memories %v survived their event", []string{"m1", "m2"})
	}
}

// TestDeleteEventCascadeIsOneTransaction makes the memory delete fail inside the transaction: the
// event must still be there, with its memories still attached, so the request can simply be retried.
func TestDeleteEventCascadeIsOneTransaction(t *testing.T) {
	requireSQLite(t) // a trigger is the portable-enough way to fail one statement mid-transaction

	d := seedCascade(t)
	ctx := context.Background()

	if _, err := d.exec(ctx, `CREATE TRIGGER fail_memory_delete BEFORE DELETE ON memories BEGIN SELECT RAISE(ABORT, 'injected'); END`); err != nil {
		t.Fatalf("creating the trigger: %s", err)
	}

	if _, err := d.DeleteEventCascade(ctx, "e1", EventCascade{DeleteMemories: true}); err == nil {
		t.Fatal("the cascade succeeded through a failing memory delete")
	}

	if missing, _ := d.MissingIds(ctx, eventGraph, []string{"e1"}); len(missing) != 0 {
		t.Error("the event went although its memories could not, leaving them dangling and a retry with nothing to find")
	}

	if got := eventMemoryIds(t, d, "e1"); len(got) != 2 {
		t.Errorf("the event holds %v after a failed cascade, want both memories still attached", got)
	}

	if _, err := d.exec(ctx, `DROP TRIGGER fail_memory_delete`); err != nil {
		t.Fatalf("dropping the trigger: %s", err)
	}

	if got, err := d.DeleteEventCascade(ctx, "e1", EventCascade{DeleteMemories: true}); err != nil || !got.Deleted {
		t.Errorf("the retry = %+v, %v; want it to finish the job", got, err)
	}
}

func TestDeleteEventCascadeDetachesWhenNotDeleting(t *testing.T) {
	d := seedCascade(t)

	got, err := d.DeleteEventCascade(context.Background(), "e1", EventCascade{})
	if err != nil {
		t.Fatalf("DeleteEventCascade: %s", err)
	}

	if !got.Deleted || got.Detached != 2 || len(got.MemoryIds) != 2 {
		t.Errorf("result = %+v, want the event deleted and both memories detached", got)
	}

	if missing, _ := d.MissingIds(context.Background(), memoryGraph, []string{"m1", "m2"}); len(missing) != 0 {
		t.Errorf("detaching deleted %v", missing)
	}

	if got := eventMemoryIds(t, d, "e1"); len(got) != 0 {
		t.Errorf("%v still name the deleted event", got)
	}
}

// TestDeleteEventCascadeHonoursScope: a scoped caller's deletion takes only their own memories and
// detaches another group's, as clearEventMemories does for the predicate path.
func TestDeleteEventCascadeHonoursScope(t *testing.T) {
	d := seedCascade(t)

	got, err := d.DeleteEventCascade(context.Background(), "e1", EventCascade{DeleteMemories: true, Groups: []string{"a"}})
	if err != nil {
		t.Fatalf("DeleteEventCascade: %s", err)
	}

	if len(got.MemoryIds) != 1 || got.MemoryIds[0] != "m1" {
		t.Errorf("deleted %v, want only the caller's m1", got.MemoryIds)
	}

	if missing, _ := d.MissingIds(context.Background(), memoryGraph, []string{"m2"}); len(missing) != 0 {
		t.Error("another group's memory was deleted with the event")
	}
}

func TestDeleteEventCascadeIfEmptyRefusesAnEventStillHoldingMemories(t *testing.T) {
	d := seedCascade(t)
	ctx := context.Background()

	got, err := d.DeleteEventCascade(ctx, "e1", EventCascade{OnlyIfEmpty: true})
	if err != nil {
		t.Fatalf("DeleteEventCascade: %s", err)
	}

	if got.Deleted || !got.HeldMemories {
		t.Errorf("result = %+v, want a refusal because the event still holds memories", got)
	}

	if got := eventMemoryIds(t, d, "e1"); len(got) != 2 {
		t.Errorf("a refused delete moved memories: the event now holds %v", got)
	}

	// Scoped to group "b", only m2 counts; deleting it leaves an event that is empty for that caller.
	if _, err := d.DeleteMemories(ctx, []string{"m2"}); err != nil {
		t.Fatalf("DeleteMemories: %s", err)
	}

	got, err = d.DeleteEventCascade(ctx, "e1", EventCascade{OnlyIfEmpty: true, Groups: []string{"b"}})
	if err != nil || !got.Deleted || got.Detached != 1 {
		t.Errorf("an event empty within the caller's scope = %+v, %v; want deleted, detaching the other group's m1", got, err)
	}
}

func TestDeleteEventCascadeOfAnUnknownEventChangesNothing(t *testing.T) {
	d := seedCascade(t)

	mustCreateMemory(t, d, types.Memory{Id: "stray", Body: "x", TimeStamp: 100, Significance: 5, EventId: "never"})

	got, err := d.DeleteEventCascade(context.Background(), "never", EventCascade{DeleteMemories: true})
	if err != nil {
		t.Fatalf("DeleteEventCascade: %s", err)
	}

	if got.Deleted {
		t.Error("an unknown event reported deleted")
	}

	if missing, _ := d.MissingIds(context.Background(), memoryGraph, []string{"stray"}); len(missing) != 0 {
		t.Error("deleting an unknown event deleted a memory naming it")
	}
}

// TestDeleteEventsLeavesAnEventThatGainedAMemory: the predicate path clears an event's memories and
// then deletes the events in a batch, and a memory written between the two must not be left naming
// an event that is gone.
func TestDeleteEventsLeavesAnEventThatGainedAMemory(t *testing.T) {
	d := newTestDB(t)

	mustCreateEvent(t, d, types.Event{Id: "busy", Name: "busy", TimeStart: 100, Significance: 5})
	mustCreateEvent(t, d, types.Event{Id: "idle", Name: "idle", TimeStart: 100, Significance: 5})
	mustCreateMemory(t, d, types.Memory{Id: "late", Body: "x", TimeStamp: 100, Significance: 5, EventId: "busy"})

	cnt, err := d.DeleteEvents(context.Background(), []string{"busy", "idle"})
	if err != nil {
		t.Fatalf("DeleteEvents: %s", err)
	}

	if cnt != 1 {
		t.Errorf("deleted %d events, want only the empty one", cnt)
	}

	if missing, _ := d.MissingIds(context.Background(), eventGraph, []string{"busy"}); len(missing) != 0 {
		t.Error("an event holding a memory was deleted, leaving the memory dangling")
	}
}
