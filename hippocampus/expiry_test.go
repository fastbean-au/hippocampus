package hippocampus

import (
	"context"
	"testing"
	"time"

	"github.com/fastbean-au/hippocampus/contract"
	"github.com/fastbean-au/hippocampus/types"
)

// expiryServer is a server with a 90-day maximum retention and a decay configuration that would keep
// a significant memory for a very long time, so anything that goes, goes because of the ceiling.
func expiryServer(t *testing.T) *Server {
	t.Helper()

	s := newTestServer(t)
	s.consolidationEnabled = true
	s.consolidation = Consolidation{
		method:                 1,
		aggressiveness:         1,
		unitsOfAgeInDays:       1,
		deletionThreshold:      1,
		linkSignificanceWeight: 1,
		maximumRetentionInDays: 90,
	}

	return s
}

// TestSleepExpiresMemoriesPastTheCeiling: the cycle takes a memory stored before the ceiling even
// though it is highly significant and was recalled a moment ago, reports it as expired rather than
// consolidated, and leaves a newer one alone (TODO-3 item 157).
func TestSleepExpiresMemoriesPastTheCeiling(t *testing.T) {
	t.Parallel()

	s := expiryServer(t)
	ctx := context.Background()

	old := time.Now().Add(-100 * 24 * time.Hour).UnixNano()

	for _, m := range []types.Memory{
		{Id: "ancient", Body: "x", TimeStamp: old, Significance: 1000},
		{Id: "recent", Body: "x", TimeStamp: time.Now().UnixNano(), Significance: 1000},
	} {
		if _, err := s.db.CreateMemory(ctx, m); err != nil {
			t.Fatalf("CreateMemory(%s): %s", m.Id, err)
		}
	}

	if _, err := s.db.RecallMemories(ctx, []string{"ancient"}); err != nil {
		t.Fatalf("RecallMemories: %s", err)
	}

	if err := s.sleep(triggerManual); err != nil {
		t.Fatalf("sleep: %s", err)
	}

	report := s.lastCycle.Load()
	if report == nil {
		t.Fatal("no cycle report")
	}

	if report.memoriesExpired != 1 || report.memoriesConsolidated != 0 {
		t.Errorf("expired %d, consolidated %d; want 1 and 0", report.memoriesExpired, report.memoriesConsolidated)
	}

	stored, err := s.db.GetMemoriesByIds(ctx, []string{"ancient", "recent"})
	if err != nil {
		t.Fatalf("GetMemoriesByIds: %s", err)
	}

	if len(*stored) != 1 || (*stored)[0].Id != "recent" {
		t.Errorf("survivors %v, want only recent", *stored)
	}

	status, err := s.GetConsolidationStatus(ctx, &contract.EmptyRequest{})
	if err != nil {
		t.Fatalf("GetConsolidationStatus: %s", err)
	}

	if status.GetLastCycle().GetMemoriesExpired() != 1 {
		t.Errorf("the status reports %d expired, want 1", status.GetLastCycle().GetMemoriesExpired())
	}
}

// TestSleepWithoutAMaximumExpiresNothing: with no maximum configured, nothing is taken for its age.
func TestSleepWithoutAMaximumExpiresNothing(t *testing.T) {
	t.Parallel()

	s := expiryServer(t)
	s.consolidation.maximumRetentionInDays = 0

	if _, err := s.db.CreateMemory(context.Background(), types.Memory{
		Id: "ancient", Body: "x", TimeStamp: time.Now().Add(-1000 * 24 * time.Hour).UnixNano(), Significance: 1000000,
	}); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}

	if err := s.sleep(triggerManual); err != nil {
		t.Fatalf("sleep: %s", err)
	}

	if got := s.lastCycle.Load().memoriesExpired; got != 0 {
		t.Errorf("expired %d with no maximum configured, want 0", got)
	}
}

// TestPreviewAndExplainAccountForExpiry: the preview counts what expiry will take, and explain says
// how long a memory has before the ceiling reaches it - and pulls days_until_forgotten forward when
// the ceiling comes before decay would.
func TestPreviewAndExplainAccountForExpiry(t *testing.T) {
	t.Parallel()

	s := expiryServer(t)
	ctx := context.Background()

	for _, m := range []types.Memory{
		{Id: "past", Body: "x", TimeStamp: time.Now().Add(-100 * 24 * time.Hour).UnixNano(), Significance: 1000},
		{Id: "eighty", Body: "x", TimeStamp: time.Now().Add(-80 * 24 * time.Hour).UnixNano(), Significance: 1000},
	} {
		if _, err := s.db.CreateMemory(ctx, m); err != nil {
			t.Fatalf("CreateMemory(%s): %s", m.Id, err)
		}
	}

	preview, err := s.PreviewConsolidation(ctx, &contract.PreviewConsolidationRequest{})
	if err != nil {
		t.Fatalf("PreviewConsolidation: %s", err)
	}

	if preview.GetMemoriesExpired() != 1 {
		t.Errorf("the preview counts %d expiring, want 1", preview.GetMemoriesExpired())
	}

	explained, err := s.ExplainConsolidation(ctx, &contract.ExplainConsolidationRequest{MemoryIds: []string{"past", "eighty"}})
	if err != nil {
		t.Fatalf("ExplainConsolidation: %s", err)
	}

	for _, v := range explained.GetValuations() {
		switch v.GetId() {

		case "past":
			if v.GetDaysUntilExpiry() != 0 || v.GetDaysUntilForgotten() != 0 {
				t.Errorf("past: days_until_expiry %v, days_until_forgotten %v; want 0 and 0", v.GetDaysUntilExpiry(), v.GetDaysUntilForgotten())
			}

		case "eighty":
			if v.GetDaysUntilExpiry() < 9.9 || v.GetDaysUntilExpiry() > 10.1 {
				t.Errorf("eighty: days_until_expiry %v, want about 10", v.GetDaysUntilExpiry())
			}

			if v.GetDaysUntilForgotten() > v.GetDaysUntilExpiry() || v.GetDaysUntilForgotten() < 0 {
				t.Errorf("eighty: days_until_forgotten %v is not bounded by the ceiling's %v", v.GetDaysUntilForgotten(), v.GetDaysUntilExpiry())
			}

		}
	}
}
