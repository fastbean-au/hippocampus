package hippocampus

import (
	"context"
	"fmt"
	"testing"

	"github.com/fastbean-au/hippocampus/contract"
	"github.com/fastbean-au/hippocampus/db"
	"github.com/fastbean-au/hippocampus/types"
)

// countingStore records how many times a listing asked the store to count, so the short-page
// shortcut can be asserted on what it AVOIDS rather than only on the number it reports.
type countingStore struct {
	db.Store

	counts int
}

func (c *countingStore) CountMemoriesFiltered(ctx context.Context, filter db.MemoryFilter) (int, error) {
	c.counts++

	return c.Store.CountMemoriesFiltered(ctx, filter)
}

// TestGetMemoriesTotalCountShortcut pins both halves of the rule that lets GetMemories skip its
// second unbounded pass: a first page that came back short is its own total, and anything else is
// not.
//
// The total has to stay EXACT in every case - it is the number a client pages by - so each case
// asserts the reported figure as well as whether the store was asked for it.
func TestGetMemoriesTotalCountShortcut(t *testing.T) {
	s := newTestServer(t)

	store := &countingStore{Store: s.db}
	s.db = store

	for i := 0; i < 5; i++ {
		if _, err := s.db.CreateMemory(context.Background(), types.Memory{
			Id:           fmt.Sprintf("m%d", i),
			TimeStamp:    int64(100 + i),
			Significance: 5,
			Body:         "x",
		}); err != nil {
			t.Fatalf("CreateMemory: %s", err)
		}
	}

	cases := []struct {
		name       string
		request    *contract.GetMemoriesRequest
		wantTotal  int32
		wantRows   int
		wantCounts int
	}{
		{
			name:      "a short first page is its own total",
			request:   &contract.GetMemoriesRequest{Limit: 10},
			wantTotal: 5, wantRows: 5, wantCounts: 0,
		},
		{
			name:      "a full first page cannot bound the total",
			request:   &contract.GetMemoriesRequest{Limit: 5},
			wantTotal: 5, wantRows: 5, wantCounts: 1,
		},
		{
			name:      "a full page with more behind it counts",
			request:   &contract.GetMemoriesRequest{Limit: 2},
			wantTotal: 5, wantRows: 2, wantCounts: 1,
		},
		{
			name:      "a short page at an offset still counts",
			request:   &contract.GetMemoriesRequest{Limit: 10, Offset: 3},
			wantTotal: 5, wantRows: 2, wantCounts: 1,
		},
		{
			name:      "an empty page at an offset past the end still counts",
			request:   &contract.GetMemoriesRequest{Limit: 10, Offset: 50},
			wantTotal: 5, wantRows: 0, wantCounts: 1,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before := store.counts

			res, err := s.GetMemories(context.Background(), c.request)
			if err != nil {
				t.Fatalf("GetMemories: %s", err)
			}

			if res.GetTotalCount() != c.wantTotal {
				t.Errorf("total: got %d, want %d", res.GetTotalCount(), c.wantTotal)
			}

			if len(res.GetMemories()) != c.wantRows {
				t.Errorf("rows: got %d, want %d", len(res.GetMemories()), c.wantRows)
			}

			if got := store.counts - before; got != c.wantCounts {
				t.Errorf("store counts: got %d, want %d", got, c.wantCounts)
			}
		})
	}
}
