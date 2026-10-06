package db

import (
	"context"
	"testing"

	"github.com/fastbean-au/hippocampus/types"
)

// TestEventFilter_DescriptionContains (TODO-3 item 171): an event's description is where its account
// of what happened lives, and nothing could select on it - the name was the only text a listing could
// match. It is name_contains's counterpart, with the same case and escaping rules.
func TestEventFilter_DescriptionContains(t *testing.T) {
	d := newTestDB(t)

	for _, e := range []types.Event{
		{Id: "outage", Name: "incident 1", Description: "Database FAILOVER during the 2am deploy", TimeStart: 100, Significance: 3},
		{Id: "launch", Name: "incident 2", Description: "Launch went to 100% of traffic", TimeStart: 200, Significance: 3},
		{Id: "quiet", Name: "a failover drill", TimeStart: 300, Significance: 3},
	} {
		mustCreateEvent(t, d, e)
	}

	cases := []struct {
		name     string
		contains string
		want     []string
	}{
		{"case-insensitive", "failover", []string{"outage"}},
		{"the name is not searched", "drill", nil},
		{"percent is a literal", "100%", []string{"launch"}},
		{"no match", "rollback", nil},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			events, err := d.GetEvents(context.Background(), EventFilter{DescriptionContains: c.contains})
			if err != nil {
				t.Fatalf("GetEvents: %s", err)
			}

			got := sortedIds(events)

			if len(c.want) == 0 {
				if len(got) != 0 {
					t.Fatalf("description_contains=%q returned %v, want nothing", c.contains, got)
				}

				return
			}

			assertIds(t, "description_contains="+c.contains, got, c.want)
		})
	}
}
