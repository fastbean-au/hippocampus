package db

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/fastbean-au/hippocampus/types"
)

// The external capacity axis: memories.external_bytes, the size of a payload this store only points
// at. What these pin is the pair of properties the axis is worth nothing without - that the column
// survives every path a memory can be written by, and that the two byte measures never contaminate
// one another.

// TestExternalBytesSurvivesEveryWritePath drives the column through create, read, partial update and
// import, because a value that is accepted and then silently dropped by one of the four presents as
// an axis that is simply reading low.
func TestExternalBytesSurvivesEveryWritePath(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	if _, err := db.CreateMemory(ctx, types.Memory{
		Id:            "m1",
		TimeStamp:     100,
		Significance:  5,
		Body:          "a pointer",
		ExternalBytes: 40960,
	}); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	stored := getMemory(t, db, "m1")
	if stored == nil {
		t.Fatal("m1 is missing")
	}

	if stored.ExternalBytes != 40960 {
		t.Errorf("CreateMemory stored %d external bytes, want 40960", stored.ExternalBytes)
	}

	// A partial update naming only the external size changes it and nothing else.
	if ok, err := db.UpdateMemory(ctx, types.Memory{Id: "m1", ExternalBytes: 81920}); err != nil || !ok {
		t.Fatalf("UpdateMemory: %v, %s", ok, err)
	}

	stored = getMemory(t, db, "m1")

	if stored.ExternalBytes != 81920 {
		t.Errorf("UpdateMemory left %d external bytes, want 81920", stored.ExternalBytes)
	}

	if stored.Body != "a pointer" {
		t.Errorf("an external-bytes-only update changed the body to %q", stored.Body)
	}

	// Zero means "leave unchanged" on an update, exactly as significance does. Without this a
	// partial update of any other field would silently zero the axis for that memory.
	if ok, err := db.UpdateMemory(ctx, types.Memory{Id: "m1", Body: "a different pointer"}); err != nil || !ok {
		t.Fatalf("UpdateMemory (body only): %v, %s", ok, err)
	}

	stored = getMemory(t, db, "m1")

	if stored.ExternalBytes != 81920 {
		t.Errorf("a body-only update changed external bytes to %d, want 81920 left alone", stored.ExternalBytes)
	}

	// An import is a full-state upsert, so it must carry the column both ways - an archive that
	// dropped it would restore a store whose external axis reads zero.
	if _, err := db.ImportMemories(ctx, []types.Memory{{
		Id:            "m2",
		TimeStamp:     200,
		Significance:  5,
		Body:          "imported",
		ExternalBytes: 12345,
	}}); err != nil {
		t.Fatalf("ImportMemories: %s", err)
	}

	imported := getMemory(t, db, "m2")
	if imported == nil {
		t.Fatal("m2 is missing")
	}

	if imported.ExternalBytes != 12345 {
		t.Errorf("ImportMemories stored %d external bytes, want 12345", imported.ExternalBytes)
	}
}

// TestExternalBytesAggregate covers the axis's measure: the empty store (SUM over no rows is NULL,
// not 0), and the sum itself.
func TestExternalBytesAggregate(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	external, err := db.ExternalBytes(ctx)
	if err != nil {
		t.Fatalf("ExternalBytes (empty): %s", err)
	}

	if external != 0 {
		t.Errorf("an empty store reports %d external bytes, want 0", external)
	}

	for i, size := range []int64{1000, 2000, 0} {
		memory := types.Memory{
			Id:            string(rune('a' + i)),
			TimeStamp:     100,
			Significance:  5,
			Body:          "body",
			ExternalBytes: size,
		}

		if _, err := db.CreateMemory(ctx, memory); err != nil {
			t.Fatalf("CreateMemory %s: %s", memory.Id, err)
		}
	}

	external, err = db.ExternalBytes(ctx)
	if err != nil {
		t.Fatalf("ExternalBytes: %s", err)
	}

	if external != 3000 {
		t.Errorf("ExternalBytes = %d, want 3000", external)
	}
}

// TestExternalBytesStaysOutOfUsedBytes is the separation the whole axis rests on. UsedBytes bounds
// this store's own disk and is the exact complement of eviction's freed-bytes estimate; if a payload
// held somewhere else contributed to it, eviction would chase bytes that deleting a memory does not
// return to this disk.
func TestExternalBytesStaysOutOfUsedBytes(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	if _, err := db.CreateMemory(ctx, types.Memory{
		Id: "m1", TimeStamp: 100, Significance: 5, Body: strings.Repeat("x", 64),
	}); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	before, err := db.UsedBytes(ctx)
	if err != nil {
		t.Fatalf("UsedBytes: %s", err)
	}

	// A gigabyte of payload elsewhere, and a body of exactly the same size as the first memory's,
	// so any difference beyond one row's own footprint would be the external bytes leaking in.
	if _, err := db.CreateMemory(ctx, types.Memory{
		Id: "m2", TimeStamp: 100, Significance: 5, Body: strings.Repeat("x", 64),
		ExternalBytes: 1 << 30,
	}); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	after, err := db.UsedBytes(ctx)
	if err != nil {
		t.Fatalf("UsedBytes (after): %s", err)
	}

	// The second row costs its body plus the per-row allowance, and on the embedded dialect a page
	// or two of slack. A gigabyte would be unmistakable either way.
	if grew := after - before; grew > 1<<20 {
		t.Errorf("UsedBytes grew by %d bytes for one small memory carrying 1 GiB of external payload "+
			"- external bytes must not count toward the store's own footprint", grew)
	}
}

// TestEvictMemories_ExternalAxisAlone pins the case the axis exists for: a store comfortably under
// its own byte target, over its external one, evicting anyway - and still in ascending value order.
func TestEvictMemories_ExternalAxisAlone(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	// Ascending significance, descending external size, so an eviction ordered by value takes the
	// SMALLEST external payload first. Ordering by value-per-byte - the thing this deliberately does
	// not do - would take m3.
	seeds := []struct {
		id           string
		significance int32
		external     int64
	}{
		{id: "m1", significance: 1, external: 1000},
		{id: "m2", significance: 2, external: 2000},
		{id: "m3", significance: 3, external: 9000},
	}

	for _, seed := range seeds {
		if _, err := db.CreateMemory(ctx, types.Memory{
			Id:            seed.id,
			TimeStamp:     100,
			Significance:  seed.significance,
			Body:          "body",
			ExternalBytes: seed.external,
		}); err != nil {
			t.Fatalf("CreateMemory %s: %s", seed.id, err)
		}
	}

	server := &decisionServer{
		value: func(candidate MemoryConsolidationCandidate) float64 {
			return float64(candidate.MemorySignificance)
		},
	}

	// Nothing asked for on this store's own axis; 2500 external bytes asked for. m1 and m2 together
	// release 3000, which is the first selection in value order that satisfies it.
	evicted, err := db.EvictMemories(ctx, server, EvictionTarget{ExternalBytes: 2500})
	if err != nil {
		t.Fatalf("EvictMemories: %s", err)
	}

	if evicted.Memories != 2 {
		t.Fatalf("expected 2 memories evicted on the external axis alone, got %d", evicted.Memories)
	}

	if evicted.ExternalBytes != 3000 {
		t.Errorf("released %d external bytes, want 3000", evicted.ExternalBytes)
	}

	if evicted.Bytes <= 0 {
		t.Error("expected the store's own freed-bytes estimate to be reported too")
	}

	if getMemory(t, db, "m3") == nil {
		t.Error("m3 is the most valuable memory and must survive, whatever its payload costs elsewhere")
	}
}

// TestEvictMemories_BothAxesMustBeSatisfied pins that one pass serves both targets rather than
// stopping at whichever is met first.
func TestEvictMemories_BothAxesMustBeSatisfied(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	// The least valuable memory carries no external payload at all, so a pass that stopped as soon
	// as its byte target was met would release nothing on the external axis.
	seeds := []struct {
		id           string
		significance int32
		external     int64
	}{
		{id: "m1", significance: 1, external: 0},
		{id: "m2", significance: 2, external: 5000},
	}

	for _, seed := range seeds {
		if _, err := db.CreateMemory(ctx, types.Memory{
			Id:            seed.id,
			TimeStamp:     100,
			Significance:  seed.significance,
			Body:          "body",
			ExternalBytes: seed.external,
		}); err != nil {
			t.Fatalf("CreateMemory %s: %s", seed.id, err)
		}
	}

	server := &decisionServer{
		value: func(candidate MemoryConsolidationCandidate) float64 {
			return float64(candidate.MemorySignificance)
		},
	}

	evicted, err := db.EvictMemories(ctx, server, EvictionTarget{Bytes: 1, ExternalBytes: 1})
	if err != nil {
		t.Fatalf("EvictMemories: %s", err)
	}

	if evicted.Memories != 2 {
		t.Fatalf("expected both memories evicted (one per unsatisfied axis), got %d", evicted.Memories)
	}

	if evicted.ExternalBytes != 5000 {
		t.Errorf("released %d external bytes, want 5000", evicted.ExternalBytes)
	}
}

// TestRetainedStatsReportsExternalBytes covers retention's hold on the third axis, which is what
// tells an operator the external target has become unreachable.
func TestRetainedStatsReportsExternalBytes(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	now := time.Now().UnixNano()
	day := int64(24 * time.Hour)

	seeds := []struct {
		id       string
		age      int64
		external int64
	}{
		{id: "fresh", age: 1, external: 7000},
		{id: "old", age: 30, external: 9000},
	}

	for _, seed := range seeds {
		if _, err := db.CreateMemory(ctx, types.Memory{
			Id:            seed.id,
			TimeStamp:     now - seed.age*day,
			Significance:  1,
			Body:          "body",
			ExternalBytes: seed.external,
		}); err != nil {
			t.Fatalf("CreateMemory %s: %s", seed.id, err)
		}
	}

	retained, err := db.RetainedStats(ctx, now-7*day)
	if err != nil {
		t.Fatalf("RetainedStats: %s", err)
	}

	if retained.Memories != 1 {
		t.Fatalf("expected 1 retained memory, got %d", retained.Memories)
	}

	if retained.ExternalBytes != 7000 {
		t.Errorf("retained external bytes = %d, want 7000 (the old memory's payload is not retained)",
			retained.ExternalBytes)
	}

	// The two figures measure different resources, so the external one is a bare sum: it must not
	// carry the per-row allowance the store's own accounting adds, which means nothing on a disk
	// this service does not own.
	if retained.Bytes <= db.dialect().memoryRowOverheadBytes {
		t.Errorf("retained bytes = %d, which does not include the per-row allowance", retained.Bytes)
	}
}

// TestPreviewMatchesACycleOnTheExternalAxis is TestPreviewMatchesASleepCycle's counterpart for the
// third axis. The preview reimplements eviction's selection rather than sharing it, so the two
// agreeing is a property that has to be tested rather than one the code guarantees.
func TestPreviewMatchesACycleOnTheExternalAxis(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	for i, external := range []int64{1000, 2000, 3000, 4000} {
		if _, err := db.CreateMemory(ctx, types.Memory{
			Id:            string(rune('a' + i)),
			TimeStamp:     100,
			Significance:  int32(i + 1),
			Body:          "body",
			ExternalBytes: external,
		}); err != nil {
			t.Fatalf("CreateMemory: %s", err)
		}
	}

	// Consolidates and retains nothing, so eviction alone decides and the two paths are comparable.
	server := &decisionServer{
		value: func(candidate MemoryConsolidationCandidate) float64 {
			return float64(candidate.MemorySignificance)
		},
	}

	const (
		externalCapacity = 6000
		externalUsed     = 10000
	)

	preview, err := db.PreviewConsolidation(ctx, server, PreviewOptions{
		ExternalBytes:         externalUsed,
		CapacityExternalBytes: externalCapacity,
		ExternalEvictionFloor: externalCapacity,
	})
	if err != nil {
		t.Fatalf("PreviewConsolidation: %s", err)
	}

	evicted, err := db.EvictMemories(ctx, server, EvictionTarget{ExternalBytes: externalUsed - externalCapacity})
	if err != nil {
		t.Fatalf("EvictMemories: %s", err)
	}

	if preview.MemoriesEvicted != evicted.Memories {
		t.Errorf("preview said %d memories evicted on the external axis, the cycle evicted %d",
			preview.MemoriesEvicted, evicted.Memories)
	}

	if preview.ExternalBytesFreed != evicted.ExternalBytes {
		t.Errorf("preview said %d external bytes freed, the cycle released %d",
			preview.ExternalBytesFreed, evicted.ExternalBytes)
	}
}

// TestPreviewLeavesTheExternalAxisAloneWhenUnconfigured pins the gate: with no external capacity
// there is nothing to be over, so the axis must never select a memory for eviction on its own.
func TestPreviewLeavesTheExternalAxisAloneWhenUnconfigured(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	if _, err := db.CreateMemory(ctx, types.Memory{
		Id: "m1", TimeStamp: 100, Significance: 1, Body: "body", ExternalBytes: 1 << 30,
	}); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	preview, err := db.PreviewConsolidation(ctx, &decisionServer{}, PreviewOptions{
		ExternalBytes: 1 << 30,
	})
	if err != nil {
		t.Fatalf("PreviewConsolidation: %s", err)
	}

	if preview.MemoriesEvicted != 0 || preview.ExternalBytesFreed != 0 {
		t.Errorf("an unconfigured external axis evicted %d memories and %d external bytes",
			preview.MemoriesEvicted, preview.ExternalBytesFreed)
	}
}
