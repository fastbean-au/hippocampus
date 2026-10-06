package db

import (
	"context"
	"testing"

	"github.com/fastbean-au/hippocampus/types"
)

// TestSearchMemoryHits_TimeBounds (TODO-3 item 171): the bounds are applied inside the query, like
// the group and metadata filters, so the limit still returns a full page when one exists.
func TestSearchMemoryHits_TimeBounds(t *testing.T) {
	d := newTestDB(t)

	for _, m := range []types.Memory{
		{Id: "early", Body: "deploy failed", TimeStamp: 100, Significance: 5},
		{Id: "middle", Body: "deploy failed", TimeStamp: 200, Significance: 5},
		{Id: "late", Body: "deploy failed", TimeStamp: 300, Significance: 5},
	} {
		mustCreateMemory(t, d, m)
	}

	hits, err := d.SearchMemoryHits(context.Background(), ContentQuery{Text: "deploy", TimestampMin: 150, TimestampMax: 300, Limit: 10})
	if err != nil {
		t.Fatalf("SearchMemoryHits: %s", err)
	}

	got := map[string]bool{}
	for _, hit := range hits {
		got[hit.Id] = true
	}

	if len(got) != 2 || !got["middle"] || !got["late"] {
		t.Errorf("hits %v, want middle and late (both bounds inclusive)", got)
	}
}
