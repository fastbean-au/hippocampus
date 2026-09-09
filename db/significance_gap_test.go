package db

import (
	"context"
	"testing"

	"github.com/fastbean-au/hippocampus/types"
)

// The significance registry's remaining arms: the rank clamp, the unranked short-circuit, and the
// two listing reads' failure paths.

// TestResolveSignificanceLevelClampsToARealRank covers the guard on the resolved target. It is
// defensive rather than reachable through any placement the RPC layer builds - anchorRank refuses
// an anchor below 1, so no supported combination produces a target of 0 - and the reason it must
// stay is that 0 is not merely a small rank: it is the value that means UNRANKED, so a target that
// fell through to it would silently take an item off the scale it was being placed on.
func TestResolveSignificanceLevelClampsToARealRank(t *testing.T) {
	database := newTestDB(t)

	// A placement mode outside the enum: none of the switch's arms assigns a target, which is the
	// only way to reach the clamp.
	id, rank, err := database.ResolveSignificanceLevel(context.Background(), SignificanceSpec{
		Placement: PlacementBetween + 1,
		Anchor:    5,
	})
	if err != nil {
		t.Fatalf("ResolveSignificanceLevel: %s", err)
	}

	if rank != 1 {
		t.Errorf("rank = %d, want the clamped 1", rank)
	}

	if !id.Valid {
		t.Error("a clamped placement must still be ranked, not left unranked")
	}
}

// TestEnsureSignificanceLevelLeavesAnUnrankedItemAlone covers the short-circuit and the nil return
// beyond it. A non-positive significance is a deliberate "rank it later" rather than a mistake, so
// it must not create a level - a registry row per unranked write would fill the scale with a rank
// nothing is at.
func TestEnsureSignificanceLevelLeavesAnUnrankedItemAlone(t *testing.T) {
	database := newTestDB(t)
	ctx := context.Background()

	id, err := database.ensureSignificanceLevel(ctx, 0, nil)
	if err != nil {
		t.Fatalf("ensureSignificanceLevel: %s", err)
	}

	if id != nil {
		t.Errorf("an unranked significance resolved to level %d, want none", *id)
	}

	// An id already supplied is returned untouched, so the RPC layer's placement resolution is not
	// redone (and possibly re-decided) by the storage layer underneath it.
	existing := int64(42)

	if got, err := database.ensureSignificanceLevel(ctx, 5, &existing); err != nil || got != &existing {
		t.Errorf("ensureSignificanceLevel(with an id) = %v, %v; want the id it was given", got, err)
	}

	levels, err := database.SignificanceLevels(ctx, SignificanceLevelFilter{})
	if err != nil {
		t.Fatalf("SignificanceLevels: %s", err)
	}

	if len(levels) != 0 {
		t.Errorf("the registry gained %v from an unranked write", levels)
	}
}

// TestSignificanceLevelsPaginate covers the Limit and Offset arms of the query builder, which the
// RPC's own tests reach only through the default page size.
func TestSignificanceLevelsPaginate(t *testing.T) {
	database := newTestDB(t)
	ctx := context.Background()

	for i, rank := range []int32{3, 5, 8, 13} {
		if _, err := database.CreateMemory(ctx, types.Memory{
			Id:           string(rune('a' + i)),
			Body:         "x",
			TimeStamp:    1,
			Significance: rank,
		}); err != nil {
			t.Fatalf("CreateMemory: %s", err)
		}
	}

	page, err := database.SignificanceLevels(ctx, SignificanceLevelFilter{Limit: 2, Offset: 1})
	if err != nil {
		t.Fatalf("SignificanceLevels: %s", err)
	}

	if len(page) != 2 || page[0] != 5 || page[1] != 8 {
		t.Errorf("page = %v, want [5 8]", page)
	}

	// The count ignores Limit and Offset, which is what lets a caller size its own pagination from
	// a page that has already been narrowed.
	total, err := database.CountSignificanceLevels(ctx, SignificanceLevelFilter{Limit: 2, Offset: 1})
	if err != nil {
		t.Fatalf("CountSignificanceLevels: %s", err)
	}

	if total != 4 {
		t.Errorf("total = %d, want 4", total)
	}
}

// TestSignificanceLevelReadsSurfaceStorageFailures covers both listing reads against a closed
// database. Neither is in the Store-surface sweep, and both are on the path a console page takes.
func TestSignificanceLevelReadsSurfaceStorageFailures(t *testing.T) {
	database := newTestDB(t)
	ctx := context.Background()

	if err := database.Close(); err != nil {
		t.Fatalf("Close: %s", err)
	}

	if _, err := database.SignificanceLevels(ctx, SignificanceLevelFilter{}); err == nil {
		t.Error("expected SignificanceLevels to surface the query failure")
	}

	if _, err := database.CountSignificanceLevels(ctx, SignificanceLevelFilter{}); err == nil {
		t.Error("expected CountSignificanceLevels to surface the query failure")
	}

	if _, err := database.ensureSignificanceLevel(ctx, 5, nil); err == nil {
		t.Error("expected ensureSignificanceLevel to surface the resolution failure")
	}
}
