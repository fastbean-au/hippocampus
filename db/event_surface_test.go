package db

import (
	"context"
	"sort"
	"testing"

	"github.com/fastbean-au/hippocampus/types"
)

// The event listing's surface, brought up to the memory listing's (TODO-2 item 94): the open/ended
// tri-state and the time_end_max bug it pairs with, the name substring, and the link neighbourhood.
//
// seedEventSurface builds the same five events for each: two still running, three ended at known
// instants, with names chosen so the substring cases have something to be wrong about.
func seedEventSurface(t *testing.T) *DB {
	t.Helper()

	d := newTestDB(t)

	events := []types.Event{
		{Id: "open-1", Name: "Election night", TimeStart: 100, Significance: 3},
		{Id: "open-2", Name: "budget REPLY thread", TimeStart: 200, Significance: 3},
		{Id: "ended-early", Name: "The election recount", TimeStart: 300, TimeEnd: 400, Significance: 3},
		{Id: "ended-late", Name: "100% of the vote", TimeStart: 500, TimeEnd: 900, Significance: 3},
		{Id: "ended-latest", Name: "an_underscore", TimeStart: 600, TimeEnd: 1000, Significance: 3},
	}

	for _, e := range events {
		if _, err := d.CreateEvent(context.Background(), e); err != nil {
			t.Fatalf("CreateEvent(%s): %s", e.Id, err)
		}
	}

	return d
}

// sortedIds reads a listing back as a sorted id slice, so a test states the set it expects rather
// than an order the filter does not promise. (ids, in event_test.go, keeps the order, which is what
// the ordering tests there need.)
func sortedIds(events *[]types.Event) []string {
	out := ids(events)

	sort.Strings(out)

	return out
}

func assertIds(t *testing.T, what string, got []string, want []string) {
	t.Helper()

	if len(got) != len(want) {
		t.Fatalf("%s returned %v, want %v", what, got, want)
	}

	for i, id := range want {
		if got[i] != id {
			t.Fatalf("%s returned %v, want %v", what, got, want)
		}
	}
}

// TestEventFilter_TimeEndMaxExcludesOpenEvents is the bug item 94.2 names: an open event stores
// time_end = 0, so a plain `time_end <= ?` matched every one of them and "events that ended before
// X" answered with everything still running.
func TestEventFilter_TimeEndMaxExcludesOpenEvents(t *testing.T) {
	d := seedEventSurface(t)

	events, err := d.GetEvents(context.Background(), EventFilter{TimeEndMax: 500})
	if err != nil {
		t.Fatalf("GetEvents: %s", err)
	}

	assertIds(t, "GetEvents(time_end_max=500)", sortedIds(events), []string{"ended-early"})

	// The count must agree with the page, or pagination reports on rows the page will never show.
	count, err := d.CountEventsFiltered(context.Background(), EventFilter{TimeEndMax: 500})
	if err != nil {
		t.Fatalf("CountEventsFiltered: %s", err)
	}

	if count != 1 {
		t.Errorf("CountEventsFiltered(time_end_max=500) = %d, want 1", count)
	}
}

// TestEventFilter_Ended covers the tri-state that answers the question time_end_min cannot, now
// that the bound above no longer answers it by accident.
func TestEventFilter_Ended(t *testing.T) {
	d := seedEventSurface(t)

	cases := []struct {
		name  string
		ended TriState
		want  []string
	}{
		{"unset", TriStateUnset, []string{"ended-early", "ended-late", "ended-latest", "open-1", "open-2"}},
		{"false", TriStateFalse, []string{"open-1", "open-2"}},
		{"true", TriStateTrue, []string{"ended-early", "ended-late", "ended-latest"}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			events, err := d.GetEvents(context.Background(), EventFilter{Ended: c.ended})
			if err != nil {
				t.Fatalf("GetEvents: %s", err)
			}

			assertIds(t, "GetEvents(ended="+c.name+")", sortedIds(events), c.want)

			count, err := d.CountEventsFiltered(context.Background(), EventFilter{Ended: c.ended})
			if err != nil {
				t.Fatalf("CountEventsFiltered: %s", err)
			}

			if count != len(c.want) {
				t.Errorf("CountEventsFiltered = %d, want %d", count, len(c.want))
			}
		})
	}
}

// TestEventFilter_NameContains covers the substring filter, including the two properties that are
// not free: the match is case-insensitive on every dialect (SQLite folds ASCII case in LIKE,
// Postgres does not, MySQL follows the column's collation), and a LIKE wildcard in the caller's
// text is a literal character rather than a pattern.
func TestEventFilter_NameContains(t *testing.T) {
	d := seedEventSurface(t)

	cases := []struct {
		name     string
		contains string
		want     []string
	}{
		{"substring", "election", []string{"ended-early", "open-1"}},
		{"upper case query", "ELECTION", []string{"ended-early", "open-1"}},
		{"upper case stored", "reply", []string{"open-2"}},
		{"no match", "referendum", nil},
		{"percent is a literal", "100%", []string{"ended-late"}},
		{"underscore is a literal", "an_und", []string{"ended-latest"}},

		// Without escaping, '_' matches any single character and this would also match "an_und".
		{"underscore matches nothing else", "anxund", nil},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			events, err := d.GetEvents(context.Background(), EventFilter{NameContains: c.contains})
			if err != nil {
				t.Fatalf("GetEvents: %s", err)
			}

			got := sortedIds(events)

			if len(c.want) == 0 {
				if len(got) != 0 {
					t.Fatalf("GetEvents(name_contains=%q) returned %v, want nothing", c.contains, got)
				}

				return
			}

			assertIds(t, "GetEvents(name_contains="+c.contains+")", got, c.want)
		})
	}
}

// TestEventFilter_Ids covers the id restriction the linked_to filter passes down, and that it
// composes with the other predicates rather than replacing them.
func TestEventFilter_Ids(t *testing.T) {
	d := seedEventSurface(t)

	events, err := d.GetEvents(context.Background(), EventFilter{Ids: []string{"open-1", "ended-late"}})
	if err != nil {
		t.Fatalf("GetEvents: %s", err)
	}

	assertIds(t, "GetEvents(ids)", sortedIds(events), []string{"ended-late", "open-1"})

	events, err = d.GetEvents(context.Background(), EventFilter{
		Ids:   []string{"open-1", "ended-late"},
		Ended: TriStateTrue,
	})
	if err != nil {
		t.Fatalf("GetEvents: %s", err)
	}

	assertIds(t, "GetEvents(ids, ended)", sortedIds(events), []string{"ended-late"})
}

// TestLinkedEventIds covers the event graph's one-hop read, in both directions and excluding the
// events named - exactly as LinkedMemoryIds behaves for memories.
func TestLinkedEventIds(t *testing.T) {
	d := seedEventSurface(t)
	ctx := context.Background()

	if err := d.LinkEvents(ctx, "open-1", []types.Link{{Id: "ended-early", Significance: 5}}); err != nil {
		t.Fatalf("LinkEvents(outbound): %s", err)
	}

	if err := d.LinkEvents(ctx, "ended-late", []types.Link{{Id: "open-1", Significance: 5}}); err != nil {
		t.Fatalf("LinkEvents(inbound): %s", err)
	}

	linked, err := d.LinkedEventIds(ctx, []string{"open-1"})
	if err != nil {
		t.Fatalf("LinkedEventIds: %s", err)
	}

	sort.Strings(linked)
	assertIds(t, "LinkedEventIds(open-1)", linked, []string{"ended-early", "ended-late"})

	none, err := d.LinkedEventIds(ctx, []string{"open-2"})
	if err != nil {
		t.Fatalf("LinkedEventIds(open-2): %s", err)
	}

	if len(none) != 0 {
		t.Errorf("LinkedEventIds(open-2) = %v, want nothing", none)
	}
}
