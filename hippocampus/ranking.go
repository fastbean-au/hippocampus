package hippocampus

import (
	"math"
	"sort"

	"github.com/fastbean-au/hippocampus/search"
	"github.com/fastbean-au/hippocampus/types"
)

// Search ranking blends how well a memory's body matched with how much the store thinks the memory
// is worth. Without it, the one thing this service knows that a plain search engine does not - a
// per-memory significance, and how often the memory has been recalled - plays no part in what a
// search returns first. Two memories that mention a term equally often are not equally useful, and
// the store already knows which is which.
//
// Where this runs is a deliberate choice. It could have been pushed down into each backend (a
// function_score query on OpenSearch, an ORDER BY expression on SQLite), and that would have been
// worse in two ways. It would need significance and recall_count mirrored INTO the OpenSearch
// index, which breaks the one-way primary->index propagation the whole design rests on and makes
// the ranking read a stale copy of a number that changes on every recall. And it would let the two
// backends drift apart on ordering. Running it here instead means both backends are re-ranked by
// the same code, reading significance and recall from the primary store at query time - always
// current, never propagated.
//
// The cost of that choice is honest and worth stating: the backend applies the limit, so re-ranking
// only ever sees the candidates the backend already chose. A memory of enormous significance that
// the text match ranked just outside the candidate window cannot be promoted into the results,
// because nothing ever fetched it. rankingOverFetch is what buys headroom against that; it does not
// eliminate it.

// rankingOverFetch multiplies the caller's limit to widen the candidate set the re-ranking sees, so
// a significant memory ranked slightly outside the requested window can still be promoted into it.
// It is only paid when ranking is actually active - with both weights zero the backend's own order
// is returned and there is nothing to promote, so nothing is over-fetched.
//
// Four is a judgement, not a measurement: enough that significance can meaningfully reorder a page
// of results, small enough that the extra rows cost little (the backends return ids and scores, and
// the primary-store fetch is a single keyed read either way).
const rankingOverFetch = 4

// rankingWeights carries the ranking configuration, read from the search.* viper keys in New(). The
// zero value disables re-ranking entirely, which is what makes a Server constructed directly in a
// test behave exactly as the backend ordered.
type rankingWeights struct {
	// significance scales the memory's stored significance, and recall its recall count, relative
	// to a text-relevance contribution fixed at 1. Both are applied to values normalised across the
	// candidate set, so a weight of 1 makes that signal able to move a result about as far as the
	// full spread of relevance can.
	significance float64
	recall       float64
}

// active reports whether either signal would change the order. Both weights zero is not merely a
// no-op to compute - it also means no over-fetching, so the "ranking off" path is exactly the code
// path that existed before ranking did.
func (w rankingWeights) active() bool {
	return w.significance != 0 || w.recall != 0
}

// candidateLimit returns how many candidates to ask the backend for to satisfy a caller's limit.
func (w rankingWeights) candidateLimit(limit int) int {
	if !w.active() {
		return limit
	}

	return limit * rankingOverFetch
}

// rankMemories orders memories by relevance blended with significance and recall count, and
// truncates to limit. hits carries the backend's relevance scores; memories are the rows the
// primary store actually returned, which is a subset when the index held stale entries.
//
// With ranking inactive this is exactly the previous behaviour: the backend's order, filtered to
// the rows that still exist. That equivalence is deliberate - it is what makes the feature safe to
// turn off.
func rankMemories(hits []search.Hit, memories []types.Memory, weights rankingWeights, limit int) []types.Memory {
	byId := make(map[string]types.Memory, len(memories))
	for _, memory := range memories {
		byId[memory.Id] = memory
	}

	// Walk the hits rather than the memories so the backend's relevance order is the starting
	// point, and ids the primary store no longer holds drop out here.
	ordered := make([]types.Memory, 0, len(memories))
	scores := make([]float64, 0, len(memories))

	for _, hit := range hits {
		memory, ok := byId[hit.Id]
		if !ok {
			continue
		}

		ordered = append(ordered, memory)
		scores = append(scores, hit.Score)
	}

	if weights.active() && len(ordered) > 1 {
		ordered = reorder(ordered, scores, weights)
	}

	if limit > 0 && len(ordered) > limit {
		ordered = ordered[:limit]
	}

	return ordered
}

// reorder sorts the candidates by their blended score. It is split from rankMemories so the
// normalise-and-combine logic is testable on its own and rankMemories stays about assembly.
func reorder(memories []types.Memory, scores []float64, weights rankingWeights) []types.Memory {
	relevance := normalise(scores)

	significances := make([]float64, 0, len(memories))
	recalls := make([]float64, 0, len(memories))

	for _, memory := range memories {
		significances = append(significances, float64(memory.Significance))

		// Recall counts are heavily skewed - most memories are never recalled and a few are
		// recalled constantly - so they are damped before normalising. Without this one memory
		// recalled a thousand times flattens the whole signal for everything else, and the
		// difference between 0 and 1 recalls (which is the interesting one) disappears.
		recalls = append(recalls, math.Log1p(float64(memory.RecallCount)))
	}

	normalisedSignificance := normalise(significances)
	normalisedRecall := normalise(recalls)

	blended := make([]float64, len(memories))

	for i := range memories {
		blended[i] = relevance[i] +
			weights.significance*normalisedSignificance[i] +
			weights.recall*normalisedRecall[i]
	}

	// A stable sort so that candidates scoring identically - which happens whenever a signal is
	// flat across the set - keep the backend's relevance order rather than an arbitrary one.
	ranked := make([]types.Memory, len(memories))
	copy(ranked, memories)

	indices := make([]int, len(memories))
	for i := range indices {
		indices[i] = i
	}

	sort.SliceStable(indices, func(a int, b int) bool {
		return blended[indices[a]] > blended[indices[b]]
	})

	for i, index := range indices {
		ranked[i] = memories[index]
	}

	return ranked
}

// normalise scales values onto 0..1 as a proportion of the largest in the set.
//
// Normalising within the candidate set, rather than against an absolute range, is what makes the
// three signals combinable at all: bm25 scores have no fixed range and differ between backends,
// significance is an unbounded registry rank, and recall counts are unbounded too. Their absolute
// magnitudes are not comparable; their proportions within one set of results are.
//
// It divides by the maximum rather than rescaling between the minimum and the maximum, and that
// distinction is the whole correctness of the blend rather than a detail. Min-max rescaling maps
// the weakest candidate to 0 and the strongest to 1 whatever the real gap between them, so two
// memories whose relevance differs by one percent come out looking as different as it is possible
// to be - and significance, which is only supposed to break ties, would instead be deciding
// everything. Dividing by the maximum keeps a one-percent difference worth one percent, so a
// decisive text match stays decisive and near-equal matches stay near-equal, which is exactly when
// the store's own view of worth should get to speak.
//
// All three signals are non-negative by construction (relevance has had bm25's sign flipped at the
// backend boundary, significance is validated non-negative, and log1p of a count cannot be
// negative). A non-positive maximum therefore means the signal is empty or degenerate, and returns
// all zeros so it contributes nothing rather than dividing by zero or inverting the order.
func normalise(values []float64) []float64 {
	out := make([]float64, len(values))

	if len(values) == 0 {
		return out
	}

	highest := values[0]

	for _, v := range values {
		if v > highest {
			highest = v
		}
	}

	if highest <= 0 {
		return out
	}

	for i, v := range values {
		// Defensive: a negative would subtract from the blend, which no signal is meant to do.
		if v <= 0 {
			continue
		}

		out[i] = v / highest
	}

	return out
}
