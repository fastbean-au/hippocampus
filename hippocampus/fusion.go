package hippocampus

import (
	"sort"

	"github.com/fastbean-au/hippocampus/search"
)

// Hybrid search runs a keyword search and a semantic search and combines their results. The two
// come back with scores that cannot be compared: one is a bm25 relevance, the other a cosine
// similarity, and they share neither a range nor a distribution nor even a notion of what "good"
// looks like. Adding or averaging them - or normalising each and then adding, as the significance
// blend in ranking.go does - would produce a number whose meaning depends on the shape of whichever
// result set happened to arrive.
//
// So hybrid fuses RANKS rather than scores, using Reciprocal Rank Fusion. Each result contributes
// 1/(k + position) from each list it appears in, and the contributions are summed. Nothing about
// that depends on either scoring scale, which is exactly the property needed here; a memory found
// by both routes outranks one found by only a single route, which is the behaviour that makes
// hybrid better than either half.
//
// This is deliberately a different technique from the significance blend in ranking.go, for a
// reason worth keeping straight: there, all the signals came with meaningful magnitudes within one
// result set, so preserving those magnitudes mattered. Here the magnitudes are not comparable in
// the first place, so only the ordering can be trusted.

// rrfK damps the influence of the very top ranks, so a result that is first in one list and absent
// from the other does not automatically beat one placed respectably in both. 60 is the value from
// the original RRF work and the de-facto default in search engines that ship it; the ranking is
// not sensitive to small changes in it.
const rrfK = 60.0

// fuseHits combines several ranked result lists into one by Reciprocal Rank Fusion, most relevant
// first. The returned scores are RRF scores - meaningful only relative to each other within this
// result, like every other Hit score.
func fuseHits(lists ...[]search.Hit) []search.Hit {
	contributions := make(map[string]float64)

	// firstSeen keeps the fusion deterministic when two results tie: without it the output order
	// would depend on map iteration, so the same query could return the same memories in different
	// orders on successive calls.
	firstSeen := make(map[string]int)
	order := 0

	for _, list := range lists {
		for position, hit := range list {
			contributions[hit.Id] += 1 / (rrfK + float64(position))

			if _, seen := firstSeen[hit.Id]; !seen {
				firstSeen[hit.Id] = order
				order++
			}
		}
	}

	fused := make([]search.Hit, 0, len(contributions))

	for id, score := range contributions {
		fused = append(fused, search.Hit{Id: id, Score: score})
	}

	sort.Slice(fused, func(a int, b int) bool {
		if fused[a].Score != fused[b].Score {
			return fused[a].Score > fused[b].Score
		}

		return firstSeen[fused[a].Id] < firstSeen[fused[b].Id]
	})

	return fused
}
