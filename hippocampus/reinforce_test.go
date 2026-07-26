package hippocampus

import (
	"context"
	"testing"

	"github.com/fastbean-au/hippocampus/auth"
	"github.com/fastbean-au/hippocampus/contract"
	"github.com/fastbean-au/hippocampus/types"
)

// recallCount reads the current recall_count of a memory straight from the store, so a test can
// tell a reinforcing recall (count incremented) from a suppressed one (count unchanged).
func recallCount(t *testing.T, s *Server, id string) int32 {
	t.Helper()

	memories, err := s.db.GetMemoriesByIds(context.Background(), []string{id})
	if err != nil {
		t.Fatalf("GetMemoriesByIds: %s", err)
	}

	if len(*memories) != 1 {
		t.Fatalf("expected 1 memory, got %d", len(*memories))
	}

	return (*memories)[0].RecallCount
}

func seedMemory(t *testing.T, s *Server) {
	t.Helper()

	if _, err := s.db.CreateMemory(context.Background(), types.Memory{Id: "m1", TimeStamp: 100, Significance: 5, Body: "one"}); err != nil {
		t.Fatalf("CreateMemory: %s", err)
	}
}

// TestRecallMemories_ReinforcementGate verifies mayReinforce governs the RecallMemories write
// side effect: writers/admins and the auth-disabled path reinforce; a reader reinforces only when
// readerRecallReinforces is set, otherwise the recall is a plain read.
func TestRecallMemories_ReinforcementGate(t *testing.T) {
	cases := []struct {
		name           string
		ctx            context.Context
		readerAllowed  bool
		wantReinforced bool
	}{
		{"no tier (auth disabled) reinforces", context.Background(), false, true},
		{"writer reinforces", auth.ContextWithTier(context.Background(), auth.TierWriter), false, true},
		{"admin reinforces", auth.ContextWithTier(context.Background(), auth.TierAdmin), false, true},
		{"reader suppressed by default", auth.ContextWithTier(context.Background(), auth.TierReader), false, false},
		{"reader reinforces when enabled", auth.ContextWithTier(context.Background(), auth.TierReader), true, true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newTestServer(t)
			s.readerRecallReinforces = c.readerAllowed
			seedMemory(t, s)

			if _, err := s.RecallMemories(c.ctx, &contract.RecallMemoriesRequest{Ids: []string{"m1"}}); err != nil {
				t.Fatalf("RecallMemories: %s", err)
			}

			got := recallCount(t, s, "m1")

			if c.wantReinforced && got != 1 {
				t.Fatalf("expected recall_count 1 (reinforced), got %d", got)
			}

			if !c.wantReinforced && got != 0 {
				t.Fatalf("expected recall_count 0 (suppressed), got %d", got)
			}
		})
	}
}

// TestRecallMemories_StillReturnsMemories confirms a reader whose reinforcement is suppressed still
// gets the memory back - the recall is downgraded to a read, not denied.
func TestRecallMemories_StillReturnsMemories(t *testing.T) {
	s := newTestServer(t)
	seedMemory(t, s)

	ctx := auth.ContextWithTier(context.Background(), auth.TierReader)

	res, err := s.RecallMemories(ctx, &contract.RecallMemoriesRequest{Ids: []string{"m1"}})
	if err != nil {
		t.Fatalf("RecallMemories: %s", err)
	}

	if len(res.GetMemories()) != 1 || res.GetMemories()[0].GetId() != "m1" {
		t.Fatalf("expected the memory returned despite suppressed reinforcement, got %v", res.GetMemories())
	}

	if got := recallCount(t, s, "m1"); got != 0 {
		t.Fatalf("expected no reinforcement, got recall_count %d", got)
	}
}
