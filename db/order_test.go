package db

import (
	"context"
	"testing"

	"github.com/fastbean-au/hippocampus/types"
)

// reversed returns a copy of in with its elements in the opposite order, for the "ascending is the
// exact reverse of descending" assertions below.
func reversed(in []string) []string {
	out := make([]string, len(in))

	for i, v := range in {
		out[len(in)-1-i] = v
	}

	return out
}

// orderingMemories seeds a store whose four memories differ in EVERY sortable column, so no
// ordering can accidentally agree with another one and pass by coincidence. No two rows tie on any
// sort column either, which is what lets the reversal assertions be exact.
func orderingMemories(t *testing.T) *DB {
	t.Helper()

	db := newTestDB(t)

	memories := []types.Memory{
		{Id: "m3", TimeStamp: 100, Significance: 30, Group: "gc", Body: "a", TimeRecalled: 700, RecallCount: 2},
		{Id: "m1", TimeStamp: 200, Significance: 50, Group: "ga", Body: "b", TimeRecalled: 900, RecallCount: 4},
		{Id: "m4", TimeStamp: 300, Significance: 40, Group: "gd", Body: "c", TimeRecalled: 600, RecallCount: 1},
		{Id: "m2", TimeStamp: 400, Significance: 10, Group: "gb", Body: "d", TimeRecalled: 800, RecallCount: 3},
	}

	for _, m := range memories {
		if _, err := db.CreateMemory(context.Background(), m); err != nil {
			t.Fatalf("CreateMemory(%s): %s", m.Id, err)
		}
	}

	// link_significance is the denormalised aggregate, so it has to be created through the link
	// graph rather than written on the row. Both ends of a link gain its significance, so the
	// weights are chosen to leave all four totals distinct: m1=7, m2=12, m3=5, m4=9.
	links := []struct {
		from string
		link types.Link
	}{
		{"m1", types.Link{Id: "m3", Significance: 5}},
		{"m2", types.Link{Id: "m1", Significance: 2}},
		{"m2", types.Link{Id: "m4", Significance: 9}},
		{"m2", types.Link{Id: "m3", Significance: 1}},
	}

	for _, l := range links {
		if err := db.LinkMemories(context.Background(), l.from, []types.Link{l.link}); err != nil {
			t.Fatalf("LinkMemories(%s -> %s): %s", l.from, l.link.Id, err)
		}
	}

	return db
}

// TestMemoryOrderingFields pins the natural order of every accepted GetMemories order_by value, and
// that asking for the opposite direction returns exactly the reverse of it.
func TestMemoryOrderingFields(t *testing.T) {
	db := orderingMemories(t)

	cases := []struct {
		orderBy string
		natural []string
		// naturalDirection is the direction the value sorts in when the caller names none; the test
		// asserts it by asking for that direction explicitly and getting the same list back.
		naturalDirection SortDirection
	}{
		{"significance", []string{"m1", "m4", "m3", "m2"}, SortDirectionDescending},
		{"timestamp", []string{"m2", "m4", "m1", "m3"}, SortDirectionDescending},
		{"time_recalled", []string{"m1", "m2", "m3", "m4"}, SortDirectionDescending},
		{"recall_count", []string{"m1", "m2", "m3", "m4"}, SortDirectionDescending},
		{"link_significance", []string{"m2", "m4", "m1", "m3"}, SortDirectionDescending},
		{"group", []string{"m1", "m2", "m3", "m4"}, SortDirectionAscending},
		{"id", []string{"m1", "m2", "m3", "m4"}, SortDirectionAscending},
	}

	for _, c := range cases {
		t.Run(c.orderBy, func(t *testing.T) {
			natural, err := db.GetMemories(context.Background(), MemoryFilter{OrderBy: c.orderBy})
			if err != nil {
				t.Fatalf("GetMemories(%s): %s", c.orderBy, err)
			}

			if !equalStrings(memIds(natural), c.natural) {
				t.Fatalf("natural order = %v, want %v", memIds(natural), c.natural)
			}

			explicit, err := db.GetMemories(context.Background(),
				MemoryFilter{OrderBy: c.orderBy, OrderDirection: c.naturalDirection})
			if err != nil {
				t.Fatalf("GetMemories(%s, natural direction): %s", c.orderBy, err)
			}

			if !equalStrings(memIds(explicit), c.natural) {
				t.Errorf("naming the natural direction changed the order: %v, want %v",
					memIds(explicit), c.natural)
			}

			opposite := SortDirectionAscending
			if c.naturalDirection == SortDirectionAscending {
				opposite = SortDirectionDescending
			}

			flipped, err := db.GetMemories(context.Background(),
				MemoryFilter{OrderBy: c.orderBy, OrderDirection: opposite})
			if err != nil {
				t.Fatalf("GetMemories(%s, opposite direction): %s", c.orderBy, err)
			}

			if want := reversed(c.natural); !equalStrings(memIds(flipped), want) {
				t.Errorf("opposite direction = %v, want the exact reverse %v", memIds(flipped), want)
			}
		})
	}
}

// TestMemoryOrderingDirectionReversesTiebreakers is the tie case the field test deliberately avoids:
// two memories tied on the sort column must still come back in the exact opposite order, because the
// tiebreakers flip with the direction rather than staying pinned.
func TestMemoryOrderingDirectionReversesTiebreakers(t *testing.T) {
	db := newTestDB(t)

	// Same significance and same timestamp, so only the id tiebreaker separates them.
	for _, id := range []string{"a", "b", "c"} {
		if _, err := db.CreateMemory(context.Background(),
			types.Memory{Id: id, TimeStamp: 100, Significance: 5, Body: id}); err != nil {
			t.Fatalf("CreateMemory(%s): %s", id, err)
		}
	}

	desc, err := db.GetMemories(context.Background(),
		MemoryFilter{OrderBy: "significance", OrderDirection: SortDirectionDescending})
	if err != nil {
		t.Fatalf("GetMemories(desc): %s", err)
	}

	if want := []string{"a", "b", "c"}; !equalStrings(memIds(desc), want) {
		t.Errorf("descending = %v, want %v", memIds(desc), want)
	}

	asc, err := db.GetMemories(context.Background(),
		MemoryFilter{OrderBy: "significance", OrderDirection: SortDirectionAscending})
	if err != nil {
		t.Fatalf("GetMemories(asc): %s", err)
	}

	if want := []string{"c", "b", "a"}; !equalStrings(memIds(asc), want) {
		t.Errorf("ascending = %v, want %v", memIds(asc), want)
	}
}

// orderingEvents is orderingMemories' counterpart: four events differing in every sortable column,
// with no ties.
func orderingEvents(t *testing.T) *DB {
	t.Helper()

	db := newTestDB(t)

	events := []types.Event{
		{Id: "e3", Name: "nc", TimeStart: 100, TimeEnd: 700, Significance: 30, Group: "gc"},
		{Id: "e1", Name: "na", TimeStart: 200, TimeEnd: 900, Significance: 50, Group: "ga"},
		{Id: "e4", Name: "nd", TimeStart: 300, TimeEnd: 600, Significance: 40, Group: "gd"},
		{Id: "e2", Name: "nb", TimeStart: 400, TimeEnd: 800, Significance: 10, Group: "gb"},
	}

	for _, e := range events {
		if _, err := db.CreateEvent(context.Background(), e); err != nil {
			t.Fatalf("CreateEvent(%s): %s", e.Id, err)
		}
	}

	// As on the memory side, the totals are e1=7, e2=12, e3=5, e4=9.
	links := []struct {
		from string
		link types.Link
	}{
		{"e1", types.Link{Id: "e3", Significance: 5}},
		{"e2", types.Link{Id: "e1", Significance: 2}},
		{"e2", types.Link{Id: "e4", Significance: 9}},
		{"e2", types.Link{Id: "e3", Significance: 1}},
	}

	for _, l := range links {
		if err := db.LinkEvents(context.Background(), l.from, []types.Link{l.link}); err != nil {
			t.Fatalf("LinkEvents(%s -> %s): %s", l.from, l.link.Id, err)
		}
	}

	return db
}

// TestEventOrderingFields is TestMemoryOrderingFields' counterpart for the events listing.
func TestEventOrderingFields(t *testing.T) {
	db := orderingEvents(t)

	cases := []struct {
		orderBy          string
		natural          []string
		naturalDirection SortDirection
	}{
		{"significance", []string{"e1", "e4", "e3", "e2"}, SortDirectionDescending},
		{"timestamp", []string{"e2", "e4", "e1", "e3"}, SortDirectionDescending},
		{"time_end", []string{"e1", "e2", "e3", "e4"}, SortDirectionDescending},
		{"name", []string{"e1", "e2", "e3", "e4"}, SortDirectionAscending},
		{"link_significance", []string{"e2", "e4", "e1", "e3"}, SortDirectionDescending},
		{"group", []string{"e1", "e2", "e3", "e4"}, SortDirectionAscending},
		{"id", []string{"e1", "e2", "e3", "e4"}, SortDirectionAscending},
	}

	for _, c := range cases {
		t.Run(c.orderBy, func(t *testing.T) {
			natural, err := db.GetEvents(context.Background(), EventFilter{OrderBy: c.orderBy})
			if err != nil {
				t.Fatalf("GetEvents(%s): %s", c.orderBy, err)
			}

			if !equalStrings(ids(natural), c.natural) {
				t.Fatalf("natural order = %v, want %v", ids(natural), c.natural)
			}

			explicit, err := db.GetEvents(context.Background(),
				EventFilter{OrderBy: c.orderBy, OrderDirection: c.naturalDirection})
			if err != nil {
				t.Fatalf("GetEvents(%s, natural direction): %s", c.orderBy, err)
			}

			if !equalStrings(ids(explicit), c.natural) {
				t.Errorf("naming the natural direction changed the order: %v, want %v",
					ids(explicit), c.natural)
			}

			opposite := SortDirectionAscending
			if c.naturalDirection == SortDirectionAscending {
				opposite = SortDirectionDescending
			}

			flipped, err := db.GetEvents(context.Background(),
				EventFilter{OrderBy: c.orderBy, OrderDirection: opposite})
			if err != nil {
				t.Fatalf("GetEvents(%s, opposite direction): %s", c.orderBy, err)
			}

			if want := reversed(c.natural); !equalStrings(ids(flipped), want) {
				t.Errorf("opposite direction = %v, want the exact reverse %v", ids(flipped), want)
			}
		})
	}
}

// TestOrderClausesAreConsistent holds the two clause tables to the invariants the resolver relies
// on: every value declares both directions, and the default order_by is one of them. A value
// declaring only one direction would silently return an empty ORDER BY - valid SQL that drops the
// ordering altogether, which is exactly the failure a listing cannot see.
func TestOrderClausesAreConsistent(t *testing.T) {
	tables := map[string]struct {
		clauses  map[string]orderClause
		fallback string
	}{
		"memory": {memoryOrderClauses, defaultMemoryOrderBy},
		"event":  {eventOrderClauses, defaultEventOrderBy},
	}

	for name, table := range tables {
		t.Run(name, func(t *testing.T) {
			if _, ok := table.clauses[table.fallback]; !ok {
				t.Fatalf("the default order_by %q has no clause", table.fallback)
			}

			for k, v := range table.clauses {
				if v.ascending == "" || v.descending == "" {
					t.Errorf("%q declares only one direction (asc=%q desc=%q)", k, v.ascending, v.descending)
				}

				if v.ascending == v.descending {
					t.Errorf("%q sorts identically in both directions: %q", k, v.ascending)
				}
			}
		})
	}
}

// TestUnknownOrderByFallsBackToTheDefault pins the db layer's defence in depth: the RPC layer
// rejects an unknown order_by, so an in-process caller building a filter by hand gets the default
// ordering rather than no ORDER BY at all.
func TestUnknownOrderByFallsBackToTheDefault(t *testing.T) {
	if got, want := resolveOrder(memoryOrderClauses, "no-such-field", SortDirectionNatural, defaultMemoryOrderBy),
		memoryOrderClauses[defaultMemoryOrderBy].descending; got != want {
		t.Errorf("memory fallback = %q, want %q", got, want)
	}

	if got, want := resolveOrder(eventOrderClauses, "no-such-field", SortDirectionNatural, defaultEventOrderBy),
		eventOrderClauses[defaultEventOrderBy].descending; got != want {
		t.Errorf("event fallback = %q, want %q", got, want)
	}
}

// TestOrderByValuesMatchTheClauses pins the exported accessors the RPC layer validates against
// against the tables themselves, since the whole point of exporting them is that the two cannot
// drift.
func TestOrderByValuesMatchTheClauses(t *testing.T) {
	cases := []struct {
		name    string
		values  []string
		valid   func(string) bool
		clauses map[string]orderClause
	}{
		{"memory", MemoryOrderByValues(), ValidMemoryOrderBy, memoryOrderClauses},
		{"event", EventOrderByValues(), ValidEventOrderBy, eventOrderClauses},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if len(c.values) != len(c.clauses) {
				t.Fatalf("%d values reported for %d clauses: %v", len(c.values), len(c.clauses), c.values)
			}

			for i, v := range c.values {
				if _, ok := c.clauses[v]; !ok {
					t.Errorf("reported value %q has no clause", v)
				}

				if !c.valid(v) {
					t.Errorf("reported value %q is not accepted by the validator", v)
				}

				if i > 0 && c.values[i-1] >= v {
					t.Errorf("values are not sorted: %v", c.values)
				}
			}

			// The empty string means "the default" and must be accepted; an unknown one must not.
			if !c.valid("") {
				t.Error("the empty order_by should be accepted as the default")
			}

			if c.valid("no-such-field") {
				t.Error("an unknown order_by should be rejected")
			}
		})
	}
}
