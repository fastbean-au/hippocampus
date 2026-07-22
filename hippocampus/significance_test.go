package hippocampus

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/fastbean-au/hippocampus/contract"
	"github.com/fastbean-au/hippocampus/db"
)

// memorySignificance reads a stored memory's significance (its level's rank) back through GetMemories.
func memorySignificance(t *testing.T, s *Server, id string) int32 {
	t.Helper()

	ms, err := s.db.GetMemoriesByIds(context.Background(), []string{id})
	if err != nil {
		t.Fatalf("GetMemoriesByIds(%s): %s", id, err)
	}

	if len(*ms) != 1 {
		t.Fatalf("expected 1 memory for %s, got %d", id, len(*ms))
	}

	return (*ms)[0].Significance
}

// TestStoreMemory_PlacementAbove exercises the RPC placement path end to end: storing a memory
// "above" an existing significance opens a gap and lands it between the neighbours.
func TestStoreMemory_PlacementAbove(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()

	if _, err := s.StoreMemory(ctx, &contract.Memory{Id: "five", Significance: 5, Body: "a"}); err != nil {
		t.Fatalf("store five: %s", err)
	}

	if _, err := s.StoreMemory(ctx, &contract.Memory{Id: "six", Significance: 6, Body: "b"}); err != nil {
		t.Fatalf("store six: %s", err)
	}

	// "just above the 5s" - with 6 adjacent, the gap opens upward.
	if _, err := s.StoreMemory(ctx, &contract.Memory{
		Id:   "between",
		Body: "c",
		Placement: &contract.SignificancePlacement{
			Mode:   contract.SignificancePlacement_ABOVE,
			Anchor: 5,
		},
	}); err != nil {
		t.Fatalf("store between: %s", err)
	}

	if got := memorySignificance(t, s, "between"); got != 6 {
		t.Fatalf("between significance = %d, want 6", got)
	}

	if got := memorySignificance(t, s, "six"); got != 7 {
		t.Fatalf("six significance = %d, want 7 (shifted up)", got)
	}

	if got := memorySignificance(t, s, "five"); got != 5 {
		t.Fatalf("five significance = %d, want 5", got)
	}
}

// TestStoreMemory_PlacementBelow exercises the BELOW placement mode (untested elsewhere): placing
// "just below" an anchor should land the new memory at the anchor's own rank and shift the anchor
// (and anything at or above it) up by one.
func TestStoreMemory_PlacementBelow(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()

	if _, err := s.StoreMemory(ctx, &contract.Memory{Id: "five", Significance: 5, Body: "a"}); err != nil {
		t.Fatalf("store five: %s", err)
	}

	if _, err := s.StoreMemory(ctx, &contract.Memory{Id: "six", Significance: 6, Body: "b"}); err != nil {
		t.Fatalf("store six: %s", err)
	}

	if _, err := s.StoreMemory(ctx, &contract.Memory{
		Id:   "below",
		Body: "c",
		Placement: &contract.SignificancePlacement{
			Mode:     contract.SignificancePlacement_BELOW,
			AnchorId: "six",
		},
	}); err != nil {
		t.Fatalf("store below: %s", err)
	}

	if got := memorySignificance(t, s, "below"); got != 6 {
		t.Fatalf("below significance = %d, want 6", got)
	}

	if got := memorySignificance(t, s, "six"); got != 7 {
		t.Fatalf("six significance = %d, want 7 (shifted up)", got)
	}

	if got := memorySignificance(t, s, "five"); got != 5 {
		t.Fatalf("five significance = %d, want 5", got)
	}
}

// TestStoreMemory_UnrankedThenDeferredRanking verifies a memory can be created without a
// significance (unranked) and ranked later via UpdateMemory - the "significance may arrive later"
// requirement.
func TestStoreMemory_UnrankedThenDeferredRanking(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()

	if _, err := s.StoreMemory(ctx, &contract.Memory{Id: "later", Body: "a"}); err != nil {
		t.Fatalf("store unranked: %s", err)
	}

	if got := memorySignificance(t, s, "later"); got != 0 {
		t.Fatalf("unranked significance = %d, want 0", got)
	}

	res, err := s.UpdateMemory(ctx, &contract.Memory{Id: "later", Significance: 8})
	if err != nil {
		t.Fatalf("update significance: %s", err)
	}

	if !res.GetOk() {
		t.Fatal("update reported not ok")
	}

	if got := memorySignificance(t, s, "later"); got != 8 {
		t.Fatalf("ranked significance = %d, want 8", got)
	}
}

// TestStoreMemory_PlacementUnknownAnchor confirms a placement naming a missing anchor is a client
// error (InvalidArgument), not an internal failure.
func TestStoreMemory_PlacementUnknownAnchor(t *testing.T) {
	s := newTestServer(t)

	_, err := s.StoreMemory(context.Background(), &contract.Memory{
		Id:   "x",
		Body: "a",
		Placement: &contract.SignificancePlacement{
			Mode:     contract.SignificancePlacement_ABOVE,
			AnchorId: "does-not-exist",
		},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument, got %s (%v)", status.Code(err), err)
	}

	// Nothing should have been stored.
	if ms, err := s.db.GetMemoriesByIds(context.Background(), []string{"x"}); err == nil && len(*ms) != 0 {
		t.Fatalf("expected no memory stored, got %d", len(*ms))
	}

	_ = db.ErrInvalidPlacement
}

// TestSignificanceSpecFromProto_NilAndUnspecified exercises significanceSpecFromProto's two
// non-placement branches directly: a nil placement (no relative ranking requested at all) and an
// explicit UNSPECIFIED mode (the switch's default arm) must both yield a plain absolute spec, with
// no placement fields stamped on it.
func TestSignificanceSpecFromProto_NilAndUnspecified(t *testing.T) {
	spec := significanceSpecFromProto(5, nil, db.AnchorMemory)

	if spec.Value != 5 || spec.AnchorKind != db.AnchorMemory || spec.UpperKind != db.AnchorMemory {
		t.Errorf("nil placement: expected a plain absolute spec, got %+v", spec)
	}

	if spec.Placement != db.PlacementNone {
		t.Errorf("nil placement: expected no placement set, got %v", spec.Placement)
	}

	unspecified := significanceSpecFromProto(7, &contract.SignificancePlacement{
		Mode: contract.SignificancePlacement_UNSPECIFIED,
	}, db.AnchorEvent)

	if unspecified.Value != 7 || unspecified.AnchorKind != db.AnchorEvent {
		t.Errorf("UNSPECIFIED placement: expected a plain absolute spec, got %+v", unspecified)
	}

	if unspecified.Placement != db.PlacementNone {
		t.Errorf("UNSPECIFIED placement: expected no placement set, got %v", unspecified.Placement)
	}
}
