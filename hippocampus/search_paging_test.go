package hippocampus

import (
	"context"
	"fmt"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/fastbean-au/hippocampus/contract"
)

// SearchMemories gains offset and time bounds (TODO-3 item 171): GetMemories had both and search had
// neither, so a client could read no further than the first page of matches, nor ask for matches
// from one period.

func TestSearchMemories_OffsetPagesThroughTheRankedMatches(t *testing.T) {
	ids := []string{"m1", "m2", "m3", "m4", "m5"}
	idx := &fakeIndex{enabled: true, searchIds: ids}
	s := newSearchTestServer(t, idx)

	for _, id := range ids {
		if _, err := s.db.CreateMemory(context.Background(), testMemory(id, 5)); err != nil {
			t.Fatalf("CreateMemory(%s): %s", id, err)
		}
	}

	page := func(offset int32) []string {
		res, err := s.SearchMemories(context.Background(), &contract.SearchMemoriesRequest{Query: "hello", Limit: 2, Offset: offset})
		if err != nil {
			t.Fatalf("SearchMemories(offset %d): %s", offset, err)
		}

		out := []string{}
		for _, m := range res.GetMemories() {
			out = append(out, m.GetId())
		}

		return out
	}

	if got := fmt.Sprint(page(0)); got != "[m1 m2]" {
		t.Errorf("page 1 = %s, want [m1 m2]", got)
	}

	if got := fmt.Sprint(page(2)); got != "[m3 m4]" {
		t.Errorf("page 2 = %s, want [m3 m4]", got)
	}

	if got := fmt.Sprint(page(4)); got != "[m5]" {
		t.Errorf("page 3 = %s, want [m5]", got)
	}

	// The index is asked for enough to rank the whole window, not just the page.
	if idx.searchLimit < 6 {
		t.Errorf("the index was asked for %d candidates for a window of 6", idx.searchLimit)
	}
}

func TestSearchMemories_OffsetIsBounded(t *testing.T) {
	s := newSearchTestServer(t, &fakeIndex{enabled: true})

	for _, offset := range []int32{-1, maxSearchOffset + 1} {
		_, err := s.SearchMemories(context.Background(), &contract.SearchMemoriesRequest{Query: "hello", Offset: offset})
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("offset %d = %v, want InvalidArgument", offset, err)
		}
	}
}

func TestSearchMemories_TimeBoundsReachTheIndex(t *testing.T) {
	idx := &fakeIndex{enabled: true}
	s := newSearchTestServer(t, idx)

	if _, err := s.SearchMemories(context.Background(), &contract.SearchMemoriesRequest{Query: "hello", TimestampMin: 100, TimestampMax: 200}); err != nil {
		t.Fatalf("SearchMemories: %s", err)
	}

	if len(idx.queries) != 1 || idx.queries[0].TimestampMin != 100 || idx.queries[0].TimestampMax != 200 {
		t.Errorf("the index was asked %+v, want the time bounds applied inside it", idx.queries)
	}

	_, err := s.SearchMemories(context.Background(), &contract.SearchMemoriesRequest{Query: "hello", TimestampMin: 300, TimestampMax: 200})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("an inverted range = %v, want InvalidArgument", err)
	}
}
