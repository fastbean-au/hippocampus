package db

import "sort"

// orderClause is one order_by value's ordering, written out in full in both directions.
//
// Both clauses are spelled out rather than one being derived from the other by flipping ASC/DESC
// tokens, because the derivation is the part that goes wrong: it has to know which tokens are
// directions and which are column names, and a column called `descriptor` or an expression carrying
// a keyword would quietly become a different ordering. These are constants, so nothing has to know.
//
// ascending is the exact reverse of descending, tiebreaker included, so a caller flipping the
// direction walks the same rows the other way rather than a differently-tied version of them.
//
// naturallyAscending says which of the two a caller who named no direction gets. It is per-value
// rather than a single package-wide default because "most significant first" and "alphabetical" are
// both what a person means by "sort by this", and they are opposite directions.
type orderClause struct {
	ascending          string
	descending         string
	naturallyAscending bool
}

// memoryOrderClauses maps the API order_by values to fixed, injection-safe ORDER BY clauses. The
// order_by string is never interpolated into SQL directly — only these constant clauses are. A
// stable id tiebreaker keeps offset pagination deterministic across pages.
//
// The columns named here are the ones memoriesFrom exposes, so significance is the registry's
// resolved level_rank rather than the stored foreign key.
var memoryOrderClauses = map[string]orderClause{
	"significance": {
		descending: `significance DESC, timestamp DESC, id ASC`,
		ascending:  `significance ASC, timestamp ASC, id DESC`,
	},
	"timestamp": {
		descending: `timestamp DESC, id ASC`,
		ascending:  `timestamp ASC, id DESC`,
	},
	// A never-recalled memory stores time_recalled 0, so it sorts as the least recently recalled
	// rather than dropping out. That is deliberate: excluding it here would make the ordering a
	// filter, and `recalled` already answers "which have I never recalled?" without one.
	"time_recalled": {
		descending: `time_recalled DESC, timestamp DESC, id ASC`,
		ascending:  `time_recalled ASC, timestamp ASC, id DESC`,
	},
	"recall_count": {
		descending: `recall_count DESC, timestamp DESC, id ASC`,
		ascending:  `recall_count ASC, timestamp ASC, id DESC`,
	},
	"link_significance": {
		descending: `link_significance DESC, significance DESC, id ASC`,
		ascending:  `link_significance ASC, significance ASC, id DESC`,
	},
	"group": {
		descending:         `group_name DESC, timestamp DESC, id ASC`,
		ascending:          `group_name ASC, timestamp ASC, id DESC`,
		naturallyAscending: true,
	},
	"id": {
		descending:         `id DESC`,
		ascending:          `id ASC`,
		naturallyAscending: true,
	},
}

// eventOrderClauses is the events' counterpart to memoryOrderClauses, on the columns eventsFrom
// exposes. "timestamp" names time_start, keeping the value the memories listing uses rather than
// introducing a second name for one ordering.
var eventOrderClauses = map[string]orderClause{
	"significance": {
		descending: `significance DESC, time_start DESC, id ASC`,
		ascending:  `significance ASC, time_start ASC, id DESC`,
	},
	"timestamp": {
		descending: `time_start DESC, id ASC`,
		ascending:  `time_start ASC, id DESC`,
	},
	// An event that has not ended stores time_end 0, so it sorts as the oldest-ended rather than
	// dropping out — the same rule time_recalled follows, and for the same reason. time_end_min
	// excludes them for a caller who wants only ended events.
	"time_end": {
		descending: `time_end DESC, time_start DESC, id ASC`,
		ascending:  `time_end ASC, time_start ASC, id DESC`,
	},
	"name": {
		descending:         `name DESC, id ASC`,
		ascending:          `name ASC, id DESC`,
		naturallyAscending: true,
	},
	"link_significance": {
		descending: `link_significance DESC, significance DESC, id ASC`,
		ascending:  `link_significance ASC, significance ASC, id DESC`,
	},
	"group": {
		descending:         `group_name DESC, time_start DESC, id ASC`,
		ascending:          `group_name ASC, time_start ASC, id DESC`,
		naturallyAscending: true,
	},
	"id": {
		descending:         `id DESC`,
		ascending:          `id ASC`,
		naturallyAscending: true,
	},
}

// The default ordering for a listing that names none.
//
// Timestamp rather than significance, which is what both were until the listing index landed. The
// significance ordering cannot be served by any index on memories or events: significance is
// COALESCE(l.level_rank, 0) from the LEFT JOIN onto the registry, a column that exists only inside
// memoriesFrom/eventsFrom. So the default read - the one a client makes when it expresses no
// preference at all, which is most of them - forced a full scan and a temp B-tree sort of everything
// the filter matched, growing with the store.
//
// Timestamp is served by idx_memories_listing_v1 / idx_events_listing_v1, whose columns AND
// directions match these clauses exactly, so the default page is now a walk of `limit` index entries
// (0.16 ms out of 100,000 rows, flat in store size, against 84 ms sorted - TODO 74.3).
//
// It is also the better default on its own terms: "the most recent" is what a listing usually means,
// and significance-ordered was a poor fit for the store's own premise, since it returns the same
// head of the list until something more significant is written. Significance ordering remains one
// order_by value away.
const (
	defaultMemoryOrderBy = "timestamp"
	defaultEventOrderBy  = "timestamp"
)

// MemoryOrderByValues returns the accepted GetMemories order_by values in sorted order, and
// EventOrderByValues does the same for GetEvents.
//
// They are exported so the RPC layer validates against the very set the clauses are declared in.
// The two enforce different things — the RPC rejects an unknown value, this package falls back to
// the default one — and a hand-copied list would let a value be accepted here and rejected there,
// or worse accepted there and silently reordered here.
func MemoryOrderByValues() []string {
	return orderByValues(memoryOrderClauses)
}

func EventOrderByValues() []string {
	return orderByValues(eventOrderClauses)
}

// ValidMemoryOrderBy reports whether name is an accepted GetMemories order_by value; the empty
// string is accepted, meaning the default. ValidEventOrderBy is the events' counterpart.
func ValidMemoryOrderBy(name string) bool {
	return validOrderBy(memoryOrderClauses, name)
}

func ValidEventOrderBy(name string) bool {
	return validOrderBy(eventOrderClauses, name)
}

func orderByValues(clauses map[string]orderClause) []string {
	out := make([]string, 0, len(clauses))

	for k := range clauses {
		out = append(out, k)
	}

	sort.Strings(out)

	return out
}

func validOrderBy(clauses map[string]orderClause, name string) bool {
	if name == "" {
		return true
	}

	_, ok := clauses[name]

	return ok
}

// resolveOrder returns the ORDER BY clause for an order_by value and direction, falling back to
// fallback's clause when the value is not one this package knows. The fallback is defence in depth:
// the RPC layer rejects an unknown value outright, so reaching it means a caller inside the process
// built a filter by hand.
func resolveOrder(clauses map[string]orderClause, name string, direction SortDirection, fallback string) string {
	clause, ok := clauses[name]
	if !ok {
		clause = clauses[fallback]
	}

	ascending := clause.naturallyAscending

	switch direction {

	case SortDirectionAscending:
		ascending = true

	case SortDirectionDescending:
		ascending = false

	}

	if ascending {
		return clause.ascending
	}

	return clause.descending
}
