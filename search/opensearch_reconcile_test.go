package search

import (
	"context"
	"fmt"
	"sort"
	"testing"
)

// The enumeration's algorithm is exercised without a cluster in hippocampus/outbox_test.go, against
// a fake that mirrors the paging rules. What only a real cluster can settle is whether the QUERY
// behaves as those rules assume - that the timestamp comes back exactly, that `from` counts what the
// cursor thinks it counts, and that a page of identical timestamps arrives whole. So these tests
// exist, and they skip without one.

// TestEnumerateIdsPage_WalksEveryDocument is the base property: every document is visited exactly
// once, whatever the page size, with timestamps that collide freely - which they do in any store
// that has ever had data imported into it.
func TestEnumerateIdsPage_WalksEveryDocument(t *testing.T) {
	idx := newIntegrationIndex(t)
	ctx := context.Background()

	// Forty documents over five instants: eight share each, so most page sizes below land inside a
	// group rather than between two.
	want := map[string]bool{}

	for i := range 40 {
		id := fmt.Sprintf("walk-%02d", i)
		want[id] = true

		if err := idx.apply(ctx, op{kind: opIndex, doc: Doc{
			Id:           id,
			Body:         "enumeration fixture",
			Significance: 10,
			Timestamp:    int64(1_700_000_000_000_000_000 + 1_000_000*(i/8)),
		}}); err != nil {
			t.Fatalf("indexing %s: %s", id, err)
		}
	}

	if err := idx.refresh(ctx); err != nil {
		t.Fatalf("refresh: %s", err)
	}

	for _, size := range []int{1, 3, 8, 17, 500} {
		t.Run(fmt.Sprintf("size-%d", size), func(t *testing.T) {
			seen := map[string]int{}

			var cursor IndexCursor

			// Bounded so a cursor that fails to advance fails the test rather than hanging it.
			for pages := 0; pages < 500; pages++ {
				page, err := idx.EnumerateIdsPage(ctx, cursor, size)
				if err != nil {
					t.Fatalf("EnumerateIdsPage: %s", err)
				}

				if page.Done {

					break
				}

				for _, id := range page.Ids {
					seen[id]++
				}

				cursor = page.Next
			}

			var missing, duplicated []string

			for id := range want {
				switch {

				case seen[id] == 0:
					missing = append(missing, id)

				case seen[id] > 1:
					duplicated = append(duplicated, id)

				}
			}

			sort.Strings(missing)
			sort.Strings(duplicated)

			if len(missing) > 0 {
				t.Errorf("page size %d never visited %v - a skipped document is one the sweep would never clean up", size, missing)
			}

			if len(duplicated) > 0 {
				t.Errorf("page size %d visited %v more than once", size, duplicated)
			}
		})
	}
}

// TestEnumerateIdsPage_SurvivesDeletionMidWalk is the case the caller actually creates: the stale
// sweep deletes what it enumerates, so every removal shifts the documents behind it. A page that
// ends on a timestamp boundary is immune; a page that cannot (every document sharing one instant)
// leaves the cursor addressing documents by offset, and the caller has to subtract its own
// deletions. This drives both, with a group far larger than the page.
func TestEnumerateIdsPage_SurvivesDeletionMidWalk(t *testing.T) {
	idx := newIntegrationIndex(t)
	ctx := context.Background()

	const timestamp = int64(1_700_000_000_000_000_000)

	// Twenty documents sharing a single instant, of which every fourth survives. One page can never
	// end on a boundary here, so the offset is in play for the whole walk.
	keep := map[string]bool{}

	for i := range 20 {
		id := fmt.Sprintf("shift-%02d", i)

		if i%4 == 0 {
			keep[id] = true
		}

		if err := idx.apply(ctx, op{kind: opIndex, doc: Doc{
			Id: id, Body: "deletion fixture", Significance: 10, Timestamp: timestamp,
		}}); err != nil {
			t.Fatalf("indexing %s: %s", id, err)
		}
	}

	if err := idx.refresh(ctx); err != nil {
		t.Fatalf("refresh: %s", err)
	}

	var cursor IndexCursor

	for pages := 0; pages < 200; pages++ {
		page, err := idx.EnumerateIdsPage(ctx, cursor, 3)
		if err != nil {
			t.Fatalf("EnumerateIdsPage: %s", err)
		}

		if page.Done {

			break
		}

		doomed := make([]string, 0, len(page.Ids))

		for _, id := range page.Ids {
			if !keep[id] {
				doomed = append(doomed, id)
			}
		}

		if len(doomed) > 0 {
			if err := idx.DeleteMemoriesSync(ctx, doomed); err != nil {
				t.Fatalf("DeleteMemoriesSync: %s", err)
			}

			// Deletes are only visible to the next search once refreshed; a live sweep gets this for
			// free from the cluster's own refresh interval, and waiting for it here would make the
			// test slow and flaky rather than realistic.
			if err := idx.refresh(ctx); err != nil {
				t.Fatalf("refresh: %s", err)
			}
		}

		cursor = page.Next

		if page.Partial {
			cursor.Offset -= len(doomed)
		}
	}

	remaining := map[string]bool{}

	var walk IndexCursor

	for pages := 0; pages < 200; pages++ {
		page, err := idx.EnumerateIdsPage(ctx, walk, 100)
		if err != nil {
			t.Fatalf("EnumerateIdsPage: %s", err)
		}

		if page.Done {

			break
		}

		for _, id := range page.Ids {
			remaining[id] = true
		}

		walk = page.Next
	}

	for id := range keep {
		if !remaining[id] {
			t.Errorf("%s should have survived the walk", id)
		}
	}

	for id := range remaining {
		if !keep[id] {
			t.Errorf("%s should have been deleted during the walk, but the enumeration stepped over it", id)
		}
	}
}

// TestEnumerateIdsPage_ReadsLargeTimestampsExactly is the reason the timestamp is asked for as a
// doc-value field rather than read off the sort value: a sort value arrives through an `any`, so it
// has been through float64, whose 53-bit mantissa cannot hold a UnixNano. At this magnitude one
// float64 covers a 256-nanosecond span.
//
// Rounding DOWN is harmless - the cursor merely re-reads. The direction that loses data is rounding
// UP, because the cursor then begins past documents nothing will come back for. So these timestamps
// are chosen to be ones float64 rounds up (asserted below, since the whole test rests on it): the
// first sits 127ns below the value it would round to, and the two documents behind it are inside
// that gap, exactly where a lossy cursor steps over them.
func TestEnumerateIdsPage_ReadsLargeTimestampsExactly(t *testing.T) {
	idx := newIntegrationIndex(t)
	ctx := context.Background()

	// 1700000000000000129 -> 1700000000000000256 as a float64.
	const base = int64(1_700_000_000_000_000_129)

	if int64(float64(base)) <= base {
		t.Fatalf("this test rests on float64(%d) rounding UP, and on this platform it does not - "+
			"pick a timestamp that does, or the test proves nothing", base)
	}

	for i := range 3 {
		if err := idx.apply(ctx, op{kind: opIndex, doc: Doc{
			Id:           fmt.Sprintf("nano-%d", i),
			Body:         "precision fixture",
			Significance: 10,
			Timestamp:    base + int64(i),
		}}); err != nil {
			t.Fatalf("indexing nano-%d: %s", i, err)
		}
	}

	if err := idx.refresh(ctx); err != nil {
		t.Fatalf("refresh: %s", err)
	}

	seen := map[string]bool{}

	var cursor IndexCursor

	// One document per page, so the cursor is rebuilt from a timestamp on every step rather than
	// once - a lossy read has three chances to lose something and needs only one.
	for pages := 0; pages < 20; pages++ {
		page, err := idx.EnumerateIdsPage(ctx, cursor, 1)
		if err != nil {
			t.Fatalf("EnumerateIdsPage: %s", err)
		}

		if page.Done {

			break
		}

		for _, id := range page.Ids {
			seen[id] = true
		}

		cursor = page.Next
	}

	for i := range 3 {
		id := fmt.Sprintf("nano-%d", i)

		if !seen[id] {
			t.Errorf("%s was never visited - the cursor lost nanosecond precision and stepped over it", id)
		}
	}
}
