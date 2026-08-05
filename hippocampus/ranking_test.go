package hippocampus

import (
	"math"
	"testing"

	"github.com/fastbean-au/hippocampus/search"
	"github.com/fastbean-au/hippocampus/types"
)

// rankedIds reduces a ranking result to the ids in order, which is what every ordering assertion
// actually cares about.
func rankedIds(memories []types.Memory) []string {
	ids := make([]string, 0, len(memories))
	for _, memory := range memories {
		ids = append(ids, memory.Id)
	}

	return ids
}

// equalIds compares an ordered id list against what was expected.
func equalIds(got []string, want []string) bool {
	if len(got) != len(want) {
		return false
	}

	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}

	return true
}

func TestNormalise(t *testing.T) {
	tests := []struct {
		name string
		in   []float64
		want []float64
	}{
		{name: "proportional to the maximum", in: []float64{1, 2, 4}, want: []float64{0.25, 0.5, 1}},
		{name: "order preserved", in: []float64{10, 0, 5}, want: []float64{1, 0, 0.5}},

		// The property min-max rescaling would destroy: near-equal values must stay near-equal,
		// so that a trivial difference in one signal cannot decide the whole ranking.
		{name: "near-equal stays near-equal", in: []float64{1.01, 1.0}, want: []float64{1, 1.0 / 1.01}},

		// A flat signal scales to a uniform value, which shifts every candidate equally and so
		// leaves the order alone.
		{name: "flat", in: []float64{7, 7, 7}, want: []float64{1, 1, 1}},
		{name: "single", in: []float64{3}, want: []float64{1}},

		// Degenerate inputs contribute nothing rather than dividing by zero or inverting.
		{name: "all zero", in: []float64{0, 0}, want: []float64{0, 0}},
		{name: "negatives", in: []float64{-4, -2}, want: []float64{0, 0}},
		{name: "empty", in: []float64{}, want: []float64{}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := normalise(test.in)

			if len(got) != len(test.want) {
				t.Fatalf("normalise(%v) returned %d values, want %d", test.in, len(got), len(test.want))
			}

			for i := range got {
				if math.Abs(got[i]-test.want[i]) > 1e-9 {
					t.Errorf("normalise(%v) = %v, want %v", test.in, got, test.want)

					break
				}
			}
		})
	}
}

// With both weights zero the backend's order must survive untouched. This is the guarantee that
// makes the feature safe to turn off, so it is worth pinning explicitly.
func TestRankMemoriesInactiveKeepsBackendOrder(t *testing.T) {
	hits := []search.Hit{{Id: "m1", Score: 3}, {Id: "m2", Score: 2}, {Id: "m3", Score: 1}}

	// m3 is by far the most significant and most recalled; with ranking off none of that matters.
	memories := []types.Memory{
		{Id: "m1", Significance: 1},
		{Id: "m2", Significance: 2},
		{Id: "m3", Significance: 100, RecallCount: 500},
	}

	got := rankedIds(rankMemories(hits, memories, rankingWeights{}, 10))

	if want := []string{"m1", "m2", "m3"}; !equalIds(got, want) {
		t.Errorf("ranking off: got %v, want the backend order %v", got, want)
	}
}

// The point of the feature: among comparable textual matches, the memory the store rates higher
// should come first.
func TestRankMemoriesPromotesSignificance(t *testing.T) {
	// Near-identical relevance, so significance is free to decide.
	hits := []search.Hit{{Id: "low", Score: 1.01}, {Id: "high", Score: 1.0}}

	memories := []types.Memory{
		{Id: "low", Significance: 1},
		{Id: "high", Significance: 10},
	}

	got := rankedIds(rankMemories(hits, memories, rankingWeights{significance: 0.3}, 10))

	if want := []string{"high", "low"}; !equalIds(got, want) {
		t.Errorf("got %v, want the significant memory promoted: %v", got, want)
	}
}

// Recall count is the other half of the store's own view of worth: a memory people keep coming
// back to should outrank an equally relevant one nobody has.
func TestRankMemoriesPromotesRecallCount(t *testing.T) {
	hits := []search.Hit{{Id: "unrecalled", Score: 1.01}, {Id: "recalled", Score: 1.0}}

	memories := []types.Memory{
		{Id: "unrecalled", Significance: 5, RecallCount: 0},
		{Id: "recalled", Significance: 5, RecallCount: 20},
	}

	got := rankedIds(rankMemories(hits, memories, rankingWeights{recall: 0.3}, 10))

	if want := []string{"recalled", "unrecalled"}; !equalIds(got, want) {
		t.Errorf("got %v, want the recalled memory promoted: %v", got, want)
	}
}

// Relevance must still lead. A clearly better textual match should not be displaced by
// significance at the shipped weights - that would make search stop being search.
func TestRankMemoriesKeepsRelevanceDominantAtDefaultWeights(t *testing.T) {
	// A decisive relevance gap: the top hit is far better than the rest.
	hits := []search.Hit{{Id: "best", Score: 100}, {Id: "weak", Score: 1}}

	memories := []types.Memory{
		{Id: "best", Significance: 1, RecallCount: 0},
		{Id: "weak", Significance: 1000, RecallCount: 1000},
	}

	weights := rankingWeights{significance: 0.3, recall: 0.2}

	got := rankedIds(rankMemories(hits, memories, weights, 10))

	if want := []string{"best", "weak"}; !equalIds(got, want) {
		t.Errorf("got %v, want relevance to lead: %v", got, want)
	}
}

// Recall counts are heavily skewed, so they are damped before normalising. Without the damping one
// hugely recalled memory flattens the signal for every other, and the interesting difference -
// between never recalled and recalled once or twice - disappears.
func TestRankMemoriesDampsSkewedRecallCounts(t *testing.T) {
	hits := []search.Hit{
		{Id: "never", Score: 1.0},
		{Id: "twice", Score: 1.0},
		{Id: "constantly", Score: 1.0},
	}

	memories := []types.Memory{
		{Id: "never", RecallCount: 0},
		{Id: "twice", RecallCount: 2},
		{Id: "constantly", RecallCount: 1000},
	}

	ranked := rankMemories(hits, memories, rankingWeights{recall: 1}, 10)

	if got, want := rankedIds(ranked), []string{"constantly", "twice", "never"}; !equalIds(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}

	// With linear normalisation, 2 recalls against a maximum of 1000 would score 0.002 - visually
	// indistinguishable from never recalled. Damped, it must retain a real share of the signal.
	damped := normalise([]float64{
		math.Log1p(0),
		math.Log1p(2),
		math.Log1p(1000),
	})

	if damped[1] < 0.1 {
		t.Errorf("two recalls scored %f against a 1000-recall maximum; the damping is not working", damped[1])
	}
}

// A signal that is flat across the candidates must not disturb the order, and must not produce
// NaN by dividing by a zero spread.
func TestRankMemoriesWithFlatSignals(t *testing.T) {
	hits := []search.Hit{{Id: "m1", Score: 5}, {Id: "m2", Score: 5}, {Id: "m3", Score: 5}}

	memories := []types.Memory{
		{Id: "m1", Significance: 7, RecallCount: 3},
		{Id: "m2", Significance: 7, RecallCount: 3},
		{Id: "m3", Significance: 7, RecallCount: 3},
	}

	weights := rankingWeights{significance: 0.5, recall: 0.5}

	got := rankedIds(rankMemories(hits, memories, weights, 10))

	// Everything identical: the stable sort must leave the backend order alone.
	if want := []string{"m1", "m2", "m3"}; !equalIds(got, want) {
		t.Errorf("got %v, want the backend order preserved %v", got, want)
	}
}

// Ids the index returned that the primary store no longer holds are stale entries and must drop
// out - the store stays authoritative. This behaviour predates ranking and must survive it.
func TestRankMemoriesDropsStaleIds(t *testing.T) {
	hits := []search.Hit{{Id: "m1", Score: 3}, {Id: "stale", Score: 2}, {Id: "m2", Score: 1}}

	memories := []types.Memory{{Id: "m1"}, {Id: "m2"}}

	got := rankedIds(rankMemories(hits, memories, rankingWeights{}, 10))

	if want := []string{"m1", "m2"}; !equalIds(got, want) {
		t.Errorf("got %v, want the stale id dropped: %v", got, want)
	}
}

// The caller asked for a page, not the whole candidate set the over-fetch produced.
func TestRankMemoriesTruncatesToLimit(t *testing.T) {
	var hits []search.Hit
	var memories []types.Memory

	for _, id := range []string{"m1", "m2", "m3", "m4", "m5"} {
		hits = append(hits, search.Hit{Id: id, Score: 1})
		memories = append(memories, types.Memory{Id: id})
	}

	if got := rankMemories(hits, memories, rankingWeights{}, 2); len(got) != 2 {
		t.Errorf("got %d memories, want 2", len(got))
	}

	// A non-positive limit must not truncate to nothing.
	if got := rankMemories(hits, memories, rankingWeights{}, 0); len(got) != 5 {
		t.Errorf("got %d memories for an unlimited rank, want 5", len(got))
	}
}

func TestRankingWeightsCandidateLimit(t *testing.T) {
	// Inactive: ask for exactly what the caller wanted, so the "ranking off" path costs nothing.
	if got := (rankingWeights{}).candidateLimit(10); got != 10 {
		t.Errorf("inactive candidateLimit(10) = %d, want 10", got)
	}

	// Active: widen the window so a significant memory just outside it can be promoted in.
	if got := (rankingWeights{significance: 0.3}).candidateLimit(10); got != 10*rankingOverFetch {
		t.Errorf("active candidateLimit(10) = %d, want %d", got, 10*rankingOverFetch)
	}

	if got := (rankingWeights{recall: 0.1}).candidateLimit(5); got != 5*rankingOverFetch {
		t.Errorf("recall-only candidateLimit(5) = %d, want %d", got, 5*rankingOverFetch)
	}
}

func TestRankingWeightsActive(t *testing.T) {
	tests := []struct {
		name    string
		weights rankingWeights
		want    bool
	}{
		{name: "zero value", weights: rankingWeights{}, want: false},
		{name: "significance only", weights: rankingWeights{significance: 0.3}, want: true},
		{name: "recall only", weights: rankingWeights{recall: 0.2}, want: true},
		{name: "both", weights: rankingWeights{significance: 0.3, recall: 0.2}, want: true},

		// A negative weight is a deliberate inversion, not "off" - it must still be applied.
		{name: "negative", weights: rankingWeights{significance: -1}, want: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := test.weights.active(); got != test.want {
				t.Errorf("active() = %v, want %v", got, test.want)
			}
		})
	}
}

// The empty candidate set must not panic or produce a nil-vs-empty surprise.
func TestRankMemoriesWithNoCandidates(t *testing.T) {
	if got := rankMemories(nil, nil, rankingWeights{significance: 1}, 10); len(got) != 0 {
		t.Errorf("got %d memories from an empty ranking, want 0", len(got))
	}
}
