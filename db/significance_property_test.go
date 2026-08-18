package db

import (
	"context"
	"fmt"
	"math/rand"
	"testing"

	"github.com/fastbean-au/hippocampus/types"
)

// TestPlacementInvariantsUnderRandomSequences is a property test over the significance registry,
// the most intricate thing in the store: a relative placement has to invent a rank between two
// existing ones, and when there is no room it rewrites the ranks around it to make some.
//
// The hand-written placement tests each pin one scenario that once went wrong. This asks instead
// whether the two invariants the whole feature rests on survive an arbitrary sequence of
// placements, which is what a real store produces and what a fixed scenario cannot represent:
//
//  1. Ranks stay unique IN THE REGISTRY. level_rank is uniquely indexed, so a collision surfaces as
//     a failed write - the bug TestPlacement_ShiftAcrossConsecutiveRanks was written for, found only
//     after a particular order of operations produced it. Note this is a property of the registry
//     and NOT of items: two memories written with the same absolute significance share one level,
//     and so share its rank, which is the point of the registry.
//  2. Every relationship a placement ASSERTED still holds. "Above m3" has to mean above m3 for as
//     long as both exist, including after later placements have shifted the ranks underneath it.
//     A gap-open that renumbered one side of a pair without the other would satisfy uniqueness and
//     silently invert an ordering the caller was promised.
//
// The seed is fixed so a failure is reproducible; raising the iteration count is the way to search
// harder.
func TestPlacementInvariantsUnderRandomSequences(t *testing.T) {
	const (
		seed       = 20260819
		iterations = 300
	)

	d := newTestDB(t)

	t.Cleanup(func() { _ = d.Close() })

	ctx := context.Background()
	rng := rand.New(rand.NewSource(seed))

	// ranks holds what the store last told us each item's rank was; asserted holds the ordering
	// promises made so far, as (higher, lower) pairs.
	ranks := map[string]int32{}

	type promise struct{ higher, lower string }

	var asserted []promise

	ids := func() []string {
		out := make([]string, 0, len(ranks))
		for id := range ranks {
			out = append(out, id)
		}

		// Map iteration is random; sort by rank so a pick is reproducible under the seed.
		for i := range out {
			for j := i + 1; j < len(out); j++ {
				if ranks[out[j]] > ranks[out[i]] || (ranks[out[j]] == ranks[out[i]] && out[j] < out[i]) {
					out[i], out[j] = out[j], out[i]
				}
			}
		}

		return out
	}

	for i := range iterations {
		id := fmt.Sprintf("m%03d", i)
		existing := ids()

		spec := SignificanceSpec{}

		switch {

		case len(existing) < 2 || rng.Intn(3) == 0:
			// An absolute value, which is also how the registry gets its first ranks.
			spec = SignificanceSpec{Value: int32(1 + rng.Intn(100))}

		case rng.Intn(3) == 0:
			anchor := existing[rng.Intn(len(existing))]
			spec = SignificanceSpec{Placement: PlacementAbove, AnchorID: anchor, AnchorKind: AnchorMemory}

		case rng.Intn(2) == 0:
			anchor := existing[rng.Intn(len(existing))]
			spec = SignificanceSpec{Placement: PlacementBelow, AnchorID: anchor, AnchorKind: AnchorMemory}

		default:
			// BETWEEN needs two anchors of DIFFERENT rank - two items sharing one (which happens
			// whenever they were written with the same absolute significance) describe no interval,
			// and the service rightly refuses. Adjacent distinct ranks are picked so the gap is as
			// tight as the registry ever has to deal with, which is where it has to renumber.
			upper, lower := "", ""

			for at := 0; at < len(existing)-1; at++ {
				if ranks[existing[at]] > ranks[existing[at+1]] {
					upper, lower = existing[at], existing[at+1]

					if rng.Intn(3) == 0 {
						break
					}
				}
			}

			if upper == "" {
				spec = SignificanceSpec{Value: int32(1 + rng.Intn(100))}

				break
			}

			spec = SignificanceSpec{
				Placement:  PlacementBetween,
				AnchorID:   lower,
				AnchorKind: AnchorMemory,
				UpperID:    upper,
				UpperKind:  AnchorMemory,
			}

		}

		levelID, rank, err := d.ResolveSignificanceLevel(ctx, spec)
		if err != nil {
			t.Fatalf("iteration %d: ResolveSignificanceLevel(%+v): %s", i, spec, err)
		}

		memory := types.Memory{
			Id:           id,
			TimeStamp:    int64(1_000_000 + i),
			Significance: rank,
			Body:         "x",
		}

		if levelID.Valid {
			resolved := levelID.Int64
			memory.SignificanceLevelID = &resolved
		}

		if _, err := d.CreateMemory(ctx, memory); err != nil {
			t.Fatalf("iteration %d: CreateMemory: %s", i, err)
		}

		ranks[id] = rank

		switch spec.Placement {

		case PlacementAbove:
			asserted = append(asserted, promise{higher: id, lower: spec.AnchorID})

		case PlacementBelow:
			asserted = append(asserted, promise{higher: spec.AnchorID, lower: id})

		case PlacementBetween:
			asserted = append(asserted,
				promise{higher: spec.UpperID, lower: id},
				promise{higher: id, lower: spec.AnchorID})

		}

		// Re-read every rank: a placement may have rewritten ranks other than its own.
		current, err := d.GetMemories(ctx, MemoryFilter{OrderBy: "significance", Limit: iterations + 1})
		if err != nil {
			t.Fatalf("iteration %d: GetMemories: %s", i, err)
		}

		for _, m := range *current {
			ranks[m.Id] = m.Significance
		}

		assertRegistryRanksUnique(t, d, i)

		for _, p := range asserted {
			high, okHigh := ranks[p.higher]
			low, okLow := ranks[p.lower]

			if !okHigh || !okLow {
				continue
			}

			if high <= low {
				t.Fatalf("iteration %d: %s was placed above %s, but ranks are %d and %d",
					i, p.higher, p.lower, high, low)
			}
		}
	}

	t.Logf("held %d ordering promises across %d placements", len(asserted), iterations)
}

// assertRegistryRanksUnique checks the invariant the unique index enforces, from the outside: no two
// levels may claim the same rank. Asked of the registry rather than of the items, since items share
// a level whenever they share an absolute significance.
func assertRegistryRanksUnique(t *testing.T, d *DB, iteration int) {
	t.Helper()

	rows, err := d.sql.Query(`SELECT level_rank, COUNT(*) FROM ` + significanceLevelsTable +
		` GROUP BY level_rank HAVING COUNT(*) > 1`)
	if err != nil {
		t.Fatalf("iteration %d: reading the registry: %s", iteration, err)
	}

	defer rows.Close()

	for rows.Next() {
		var rank, count int64

		if err := rows.Scan(&rank, &count); err != nil {
			t.Fatalf("iteration %d: scanning the registry: %s", iteration, err)
		}

		t.Errorf("iteration %d: rank %d is held by %d levels", iteration, rank, count)
	}

	if err := rows.Err(); err != nil {
		t.Fatalf("iteration %d: reading the registry: %s", iteration, err)
	}
}
