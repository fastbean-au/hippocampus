package hippocampus

import (
	"context"
	"testing"
	"time"

	"github.com/fastbean-au/hippocampus/db"
	"github.com/fastbean-au/hippocampus/types"
)

// TestRetained_Boundary verifies the retained() helper: a non-positive window disables the floor,
// and otherwise items younger than minimumRetentionInDays (measured in whole wall-clock days) are
// retained while older ones are not.
func TestRetained_Boundary(t *testing.T) {
	s := &Server{consolidation: Consolidation{minimumRetentionInDays: 30}}

	now := time.Now().UnixNano()
	tenDaysAgo := now - int64(10*DAY_IN_NANOSECONDS)
	thirtyOneDaysAgo := now - int64(31*DAY_IN_NANOSECONDS)

	if !s.retained(tenDaysAgo) {
		t.Error("an item 10 days old must be retained under a 30-day floor")
	}

	if s.retained(thirtyOneDaysAgo) {
		t.Error("an item 31 days old must not be retained under a 30-day floor")
	}

	// A non-positive window disables the floor entirely - nothing is retained on this basis.
	s.consolidation.minimumRetentionInDays = 0
	if s.retained(tenDaysAgo) {
		t.Error("a zero retention window must retain nothing")
	}

	s.consolidation.minimumRetentionInDays = -5
	if s.retained(tenDaysAgo) {
		t.Error("a negative retention window must retain nothing")
	}
}

// TestShouldConsolidateMemory_RetentionOverridesThreshold verifies that a memory whose value has
// decayed well below the deletion threshold is still NOT consolidated while inside its retention
// window, and IS consolidated once past it - the hard floor overriding value-based forgetting.
func TestShouldConsolidateMemory_RetentionOverridesThreshold(t *testing.T) {
	s := &Server{
		consolidation: Consolidation{
			method:                 1,
			aggressiveness:         1.0,
			unitsOfAgeInDays:       1.0,
			minimumAgeInDays:       0,
			minimumRetentionInDays: 30,
			deletionThreshold:      1.0,
		},
	}

	// method 1: (es + ms) / age. At 10 days, value = 5/10 = 0.5 < 1.0, so absent retention this
	// memory would be consolidated - but it is only 10 days old, inside the 30-day floor.
	tenDaysAgo := time.Now().UnixNano() - int64(10*DAY_IN_NANOSECONDS)

	if s.ShouldConsolidateMemory(candidate(2, 3, 0, tenDaysAgo)) {
		t.Error("a memory inside its retention window must not be consolidated despite decaying below threshold")
	}

	// The same memory well past the retention floor is consolidated as usual.
	fortyDaysAgo := time.Now().UnixNano() - int64(40*DAY_IN_NANOSECONDS)

	if !s.ShouldConsolidateMemory(candidate(2, 3, 0, fortyDaysAgo)) {
		t.Error("a memory past its retention window must consolidate normally when below threshold")
	}
}

// TestShouldConsolidateMemory_RetentionRenewedByRecall verifies that a recall resets the retention
// clock along with the decay clock: an old memory recalled recently is protected again.
func TestShouldConsolidateMemory_RetentionRenewedByRecall(t *testing.T) {
	s := &Server{
		consolidation: Consolidation{
			method:                 1,
			aggressiveness:         1.0,
			unitsOfAgeInDays:       1.0,
			minimumAgeInDays:       0,
			minimumRetentionInDays: 30,
			deletionThreshold:      1.0,
		},
	}

	now := time.Now().UnixNano()

	c := candidate(2, 3, 0, now-int64(100*DAY_IN_NANOSECONDS))
	c.TimeRecalled = now - int64(5*DAY_IN_NANOSECONDS) // recalled 5 days ago

	if s.ShouldConsolidateMemory(c) {
		t.Error("a memory recalled 5 days ago must be retained even though it was created 100 days ago")
	}
}

// TestShouldConsolidateEvent_RetentionOverridesThreshold verifies the event consolidation pass
// honours the retention floor too (both memory and event consolidation flow through
// shouldConsolidate).
func TestShouldConsolidateEvent_RetentionOverridesThreshold(t *testing.T) {
	s := &Server{
		consolidation: Consolidation{
			method:                 1,
			aggressiveness:         1.0,
			unitsOfAgeInDays:       1.0,
			minimumAgeInDays:       0,
			minimumRetentionInDays: 30,
			deletionThreshold:      1.0,
		},
	}

	now := time.Now().UnixNano()

	// An event decayed below threshold (significance 1 over ~10 days = 0.1 < 1.0) but only 10 days
	// old is retained; the same event past the floor consolidates.
	young := db.EventConsolidationCandidate{Significance: 1, TimeStart: now - int64(10*DAY_IN_NANOSECONDS)}
	if s.ShouldConsolidateEvent(young) {
		t.Error("an event inside its retention window must not be consolidated")
	}

	old := db.EventConsolidationCandidate{Significance: 1, TimeStart: now - int64(40*DAY_IN_NANOSECONDS)}
	if !s.ShouldConsolidateEvent(old) {
		t.Error("an event past its retention window must consolidate normally when below threshold")
	}
}

// TestMemoryRetained verifies MemoryRetained (the hook eviction consults) tracks the decay
// timestamp, including recall renewal.
func TestMemoryRetained(t *testing.T) {
	s := &Server{consolidation: Consolidation{minimumRetentionInDays: 30}}

	now := time.Now().UnixNano()

	if !s.MemoryRetained(candidate(1, 1, 0, now-int64(10*DAY_IN_NANOSECONDS))) {
		t.Error("a 10-day-old memory must be reported retained under a 30-day floor")
	}

	if s.MemoryRetained(candidate(1, 1, 0, now-int64(40*DAY_IN_NANOSECONDS))) {
		t.Error("a 40-day-old memory must not be reported retained under a 30-day floor")
	}

	c := candidate(1, 1, 0, now-int64(40*DAY_IN_NANOSECONDS))
	c.TimeRecalled = now - int64(2*DAY_IN_NANOSECONDS)
	if !s.MemoryRetained(c) {
		t.Error("a recently recalled memory must be reported retained regardless of its creation age")
	}
}

// TestEvict_RetainedMemoriesSurviveCapacityPressure is the end-to-end guarantee TODO #26 asks for:
// with the store far over its byte target, eviction must still leave a retained memory in place
// (retention overrides the capacity limit), while evicting a non-retained one of equal size.
func TestEvict_RetainedMemoriesSurviveCapacityPressure(t *testing.T) {
	s := newTestServer(t)
	s.consolidation = Consolidation{
		method:                 1,
		aggressiveness:         1.0,
		unitsOfAgeInDays:       1.0,
		minimumRetentionInDays: 30,
	}

	now := time.Now().UnixNano()
	old := now - int64(90*DAY_IN_NANOSECONDS)  // well past the floor - evictable
	fresh := now - int64(5*DAY_IN_NANOSECONDS) // inside the floor - retained
	body := "a reasonably sized memory body xxxxxxxxxxxxxxxxxxxx"

	if _, err := s.db.CreateMemory(context.Background(), types.Memory{Id: "old", TimeStamp: old, Significance: 1, Body: body}); err != nil {
		t.Fatalf("CreateMemory(old): %s", err)
	}

	if _, err := s.db.CreateMemory(context.Background(), types.Memory{Id: "fresh", TimeStamp: fresh, Significance: 1, Body: body}); err != nil {
		t.Fatalf("CreateMemory(fresh): %s", err)
	}

	// A 1-byte target demands eviction reclaim essentially everything - but the retention floor must
	// still shield the fresh memory even though the store stays over target as a result.
	s.consolidation.capacityBytes = 1

	if err := s.evict(context.Background()); err != nil {
		t.Fatalf("evict: %s", err)
	}

	oldMs, err := s.db.GetMemoriesByIds(context.Background(), []string{"old"})
	if err != nil {
		t.Fatalf("GetMemoriesByIds(old): %s", err)
	}

	if len(*oldMs) != 0 {
		t.Error("the old, non-retained memory should have been evicted under capacity pressure")
	}

	freshMs, err := s.db.GetMemoriesByIds(context.Background(), []string{"fresh"})
	if err != nil {
		t.Fatalf("GetMemoriesByIds(fresh): %s", err)
	}

	if len(*freshMs) != 1 {
		t.Error("the retained memory must survive eviction even though the store is over its byte target")
	}
}

// TestEvict_RetainedMemoryKeepsItsEventAlive is a correctness regression guard: when an event has
// one retained and one evictable memory, evicting the latter must NOT delete the event out from
// under the retained memory (which would orphan it). The retained memory is still counted toward
// the event's memory total so the event is never seen as fully evicted.
func TestEvict_RetainedMemoryKeepsItsEventAlive(t *testing.T) {
	s := newTestServer(t)
	s.consolidation = Consolidation{
		method:                 1,
		aggressiveness:         1.0,
		unitsOfAgeInDays:       1.0,
		minimumRetentionInDays: 30,
	}

	ctx := context.Background()

	if _, err := s.db.CreateEvent(ctx, types.Event{Id: "e1", Name: "evt", TimeStart: 100, Significance: 1}); err != nil {
		t.Fatalf("CreateEvent: %s", err)
	}

	now := time.Now().UnixNano()
	body := "a reasonably sized memory body xxxxxxxxxxxxxxxxxxxx"

	// One old (evictable) and one fresh (retained) memory, both on the same event.
	if _, err := s.db.CreateMemory(ctx, types.Memory{Id: "old", TimeStamp: now - int64(90*DAY_IN_NANOSECONDS), Significance: 1, EventId: "e1", Body: body}); err != nil {
		t.Fatalf("CreateMemory(old): %s", err)
	}

	if _, err := s.db.CreateMemory(ctx, types.Memory{Id: "fresh", TimeStamp: now - int64(5*DAY_IN_NANOSECONDS), Significance: 1, EventId: "e1", Body: body}); err != nil {
		t.Fatalf("CreateMemory(fresh): %s", err)
	}

	s.consolidation.capacityBytes = 1

	if err := s.evict(ctx); err != nil {
		t.Fatalf("evict: %s", err)
	}

	// The event must still exist - its retained memory keeps it alive.
	if exists, err := s.db.EventExists(ctx, "e1"); err != nil {
		t.Fatalf("EventExists: %s", err)
	} else if !exists {
		t.Error("the event must survive: it still has a retained memory attached")
	}

	freshMs, err := s.db.GetMemoriesByIds(ctx, []string{"fresh"})
	if err != nil {
		t.Fatalf("GetMemoriesByIds(fresh): %s", err)
	}

	if len(*freshMs) != 1 {
		t.Fatal("the retained memory must survive eviction")
	}

	if (*freshMs)[0].EventId != "e1" {
		t.Errorf("the retained memory must keep its event_id, got %q", (*freshMs)[0].EventId)
	}
}

// TestEvict_RetentionDisabledEvictsEverything confirms the default (minimumRetentionInDays == 0)
// leaves eviction behaviour unchanged: with no floor, even fresh memories are evicted under
// sufficient capacity pressure.
func TestEvict_RetentionDisabledEvictsEverything(t *testing.T) {
	s := newTestServer(t)
	s.consolidation = Consolidation{
		method:                 1,
		aggressiveness:         1.0,
		unitsOfAgeInDays:       1.0,
		minimumRetentionInDays: 0, // disabled
	}

	now := time.Now().UnixNano()
	body := "a reasonably sized memory body xxxxxxxxxxxxxxxxxxxx"

	for _, id := range []string{"a", "b", "c"} {
		if _, err := s.db.CreateMemory(context.Background(), types.Memory{Id: id, TimeStamp: now - int64(5*DAY_IN_NANOSECONDS), Significance: 1, Body: body}); err != nil {
			t.Fatalf("CreateMemory(%s): %s", id, err)
		}
	}

	s.consolidation.capacityBytes = 1

	if err := s.evict(context.Background()); err != nil {
		t.Fatalf("evict: %s", err)
	}

	with, without := s.db.CountMemories(context.Background())
	if with+without >= 3 {
		t.Fatalf("with retention disabled, fresh memories should be evicted under pressure; still have %d", with+without)
	}
}
