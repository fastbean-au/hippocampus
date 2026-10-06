package db

import (
	"context"
	"testing"
	"time"

	"github.com/fastbean-au/hippocampus/types"
)

// TestExpireMemories pins the ceiling (TODO-3 item 157): a memory stored before the cutoff goes,
// whatever its value - and, the case the decay rules could never reach, however recently it was
// recalled. Age is measured from creation.
func TestExpireMemories(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()

	d.SetTombstonePolicy(TombstonePolicy{Enabled: true})

	now := time.Now().UnixNano()
	old := time.Now().Add(-100 * 24 * time.Hour).UnixNano()
	cutoff := time.Now().Add(-90 * 24 * time.Hour).UnixNano()

	for _, id := range []string{"emptied", "partial"} {
		if _, err := d.CreateEvent(ctx, types.Event{Id: id, Name: id, TimeStart: old, Significance: 5}); err != nil {
			t.Fatalf("CreateEvent(%s): %s", id, err)
		}
	}

	memories := []types.Memory{
		{Id: "old-loose", TimeStamp: old, Significance: 1000, Body: "x"},
		{Id: "old-recalled", TimeStamp: old, Significance: 1000, Body: "x"},
		{Id: "old-in-emptied", TimeStamp: old, Significance: 5, EventId: "emptied", Body: "x"},
		{Id: "old-in-partial", TimeStamp: old, Significance: 5, EventId: "partial", Body: "x"},
		{Id: "new-in-partial", TimeStamp: now, Significance: 5, EventId: "partial", Body: "x"},
		{Id: "new-loose", TimeStamp: now, Significance: 1, Body: "x"},
	}

	for _, m := range memories {
		if _, err := d.CreateMemory(ctx, m); err != nil {
			t.Fatalf("CreateMemory(%s): %s", m.Id, err)
		}
	}

	// Recalled now: the decay clock reads zero days, and expiry must take it anyway.
	if _, err := d.RecallMemories(ctx, []string{"old-recalled"}); err != nil {
		t.Fatalf("RecallMemories: %s", err)
	}

	result, err := d.ExpireMemories(ctx, cutoff)
	if err != nil {
		t.Fatalf("ExpireMemories: %s", err)
	}

	if result.Memories != 4 || result.Events != 1 {
		t.Errorf("expired %d memories and %d events, want 4 and 1", result.Memories, result.Events)
	}

	remaining, err := d.MemoryIdsMatching(ctx, MemoryFilter{})
	if err != nil {
		t.Fatalf("MemoryIdsMatching: %s", err)
	}

	if len(remaining) != 2 || remaining[0] != "new-in-partial" || remaining[1] != "new-loose" {
		t.Errorf("remaining memories %v, want [new-in-partial new-loose]", remaining)
	}

	if _, err := d.GetEvent(ctx, "emptied"); err == nil {
		t.Error("the event expiry emptied is still there")
	}

	partial, err := d.GetEvent(ctx, "partial")
	if err != nil {
		t.Fatalf("GetEvent(partial): %s", err)
	}

	if !partial.MemoriesConsolidated {
		t.Error("an event that lost a memory to expiry is not flagged")
	}

	logged, err := d.GetForgottenMemories(ctx, ForgottenFilter{Rule: ForgetRuleExpiry, Limit: 100})
	if err != nil {
		t.Fatalf("GetForgottenMemories: %s", err)
	}

	if len(logged) != 4 {
		t.Errorf("the forgotten log holds %d expiry records, want 4", len(logged))
	}
}

// TestExpireMemoriesWithNothingExpired: nothing past the ceiling is a no-op, not an error.
func TestExpireMemoriesWithNothingExpired(t *testing.T) {
	d := newTestDB(t)

	if _, err := d.CreateMemory(context.Background(), types.Memory{Id: "fresh", TimeStamp: time.Now().UnixNano(), Significance: 1, Body: "x"}); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	result, err := d.ExpireMemories(context.Background(), time.Now().Add(-time.Hour).UnixNano())
	if err != nil || result.Memories != 0 || result.Events != 0 {
		t.Errorf("ExpireMemories = %+v, %v; want nothing and no error", result, err)
	}
}

// TestPreviewReportsExpiryAsTheCycleDoesIt: the preview counts what ExpireMemories would take, and
// because expiry runs first, a memory past the ceiling is reported as expiring and never also as
// consolidating - even under a server that would consolidate everything.
func TestPreviewReportsExpiryAsTheCycleDoesIt(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()

	old := time.Now().Add(-100 * 24 * time.Hour).UnixNano()
	now := time.Now().UnixNano()
	cutoff := time.Now().Add(-90 * 24 * time.Hour).UnixNano()

	for i, ts := range []int64{old, old, now} {
		if _, err := d.CreateMemory(ctx, types.Memory{Id: string(rune('a' + i)), TimeStamp: ts, Significance: 1, Body: "x"}); err != nil {
			t.Fatalf("CreateMemory: %s", err)
		}
	}

	everything := &decisionServer{memory: func(MemoryConsolidationCandidate) bool { return true }}

	preview, err := d.PreviewConsolidation(ctx, everything, PreviewOptions{Limit: 10, ExpireBefore: cutoff})
	if err != nil {
		t.Fatalf("PreviewConsolidation: %s", err)
	}

	if preview.MemoriesExpired != 2 || preview.MemoriesConsolidated != 1 {
		t.Errorf("preview: %d expiring and %d consolidating, want 2 and 1", preview.MemoriesExpired, preview.MemoriesConsolidated)
	}

	expiryRules := 0

	for _, candidate := range preview.Candidates {
		if candidate.Rule == ForgetRuleExpiry {
			expiryRules++
		}
	}

	if expiryRules != 2 {
		t.Errorf("the sample carries %d expiry candidates, want 2", expiryRules)
	}

	result, err := d.ExpireMemories(ctx, cutoff)
	if err != nil {
		t.Fatalf("ExpireMemories: %s", err)
	}

	if result.Memories != preview.MemoriesExpired {
		t.Errorf("the preview said %d would expire and %d did", preview.MemoriesExpired, result.Memories)
	}
}
