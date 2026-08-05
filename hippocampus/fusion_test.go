package hippocampus

import (
	"testing"

	"github.com/fastbean-au/hippocampus/search"
)

// hitIds reduces a fused result to its ids in order.
func hitIds(hits []search.Hit) []string {
	ids := make([]string, 0, len(hits))
	for _, hit := range hits {
		ids = append(ids, hit.Id)
	}

	return ids
}

// The property that makes hybrid worth having: a memory both routes found should outrank one only
// a single route found, even when that one was top of its list.
func TestFuseHitsPrefersResultsFoundByBothRoutes(t *testing.T) {
	keyword := []search.Hit{{Id: "keyword-only", Score: 99}, {Id: "both", Score: 1}}
	semantic := []search.Hit{{Id: "semantic-only", Score: 0.99}, {Id: "both", Score: 0.1}}

	got := hitIds(fuseHits(keyword, semantic))

	if len(got) != 3 {
		t.Fatalf("fused to %v, want three distinct results", got)
	}

	if got[0] != "both" {
		t.Errorf("fused order %v, want the doubly-found result first", got)
	}
}

// Fusion must ignore score magnitudes entirely - that is the whole reason it works on ranks. A
// keyword list scored in the tens and a semantic list scored under one must fuse the same as if
// both were scored identically.
func TestFuseHitsIgnoresScoreMagnitudes(t *testing.T) {
	wildlyScaled := fuseHits(
		[]search.Hit{{Id: "a", Score: 10000}, {Id: "b", Score: 9999}},
		[]search.Hit{{Id: "b", Score: 0.0002}, {Id: "a", Score: 0.0001}},
	)

	evenlyScaled := fuseHits(
		[]search.Hit{{Id: "a", Score: 2}, {Id: "b", Score: 1}},
		[]search.Hit{{Id: "b", Score: 2}, {Id: "a", Score: 1}},
	)

	if hitIds(wildlyScaled)[0] != hitIds(evenlyScaled)[0] {
		t.Errorf("fusion changed with the score scale: %v vs %v", hitIds(wildlyScaled), hitIds(evenlyScaled))
	}
}

// Rank position must still matter within a list.
func TestFuseHitsRespectsRankOrder(t *testing.T) {
	single := []search.Hit{{Id: "first"}, {Id: "second"}, {Id: "third"}}

	got := hitIds(fuseHits(single))

	if len(got) != 3 || got[0] != "first" || got[2] != "third" {
		t.Errorf("fusing one list changed its order: %v", got)
	}
}

// Ties must break deterministically, or the same query returns the same memories in different
// orders on successive calls.
func TestFuseHitsIsDeterministic(t *testing.T) {
	build := func() [][]search.Hit {
		return [][]search.Hit{
			{{Id: "a"}, {Id: "b"}, {Id: "c"}, {Id: "d"}},
			{{Id: "d"}, {Id: "c"}, {Id: "b"}, {Id: "a"}},
		}
	}

	first := hitIds(fuseHits(build()...))

	for range 20 {
		next := hitIds(fuseHits(build()...))

		for i := range first {
			if first[i] != next[i] {
				t.Fatalf("fusion is not deterministic: %v then %v", first, next)
			}
		}
	}
}

func TestFuseHitsEdgeCases(t *testing.T) {
	if got := fuseHits(); len(got) != 0 {
		t.Errorf("fusing nothing returned %d hits, want 0", len(got))
	}

	if got := fuseHits(nil, nil); len(got) != 0 {
		t.Errorf("fusing empty lists returned %d hits, want 0", len(got))
	}

	// One empty list beside a real one must not disturb it - the case where semantic search
	// matched nothing.
	got := hitIds(fuseHits([]search.Hit{{Id: "a"}, {Id: "b"}}, nil))

	if len(got) != 2 || got[0] != "a" {
		t.Errorf("fusing with an empty list gave %v, want [a b]", got)
	}
}
