package db

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fastbean-au/hippocampus/types"
)

// TestConcurrentRecallAndDeleteDoNotDeadlock reproduces a defect found by running the retention
// benchmark's writer against a Postgres store: four deadlocks in four and a half minutes, one of
// which surfaced to the client as codes.Internal and one of which failed an eviction, leaving the
// store over its capacity target for that cycle.
//
// The cause is lock ordering. Several statements mutate a SET of memory rows in one transaction -
// recall's `UPDATE ... WHERE id IN (...)`, link recall-propagation's, and the delete chokepoint's
// `DELETE ... WHERE id IN (...)` - and each took its ids in whatever order its caller happened to
// produce them. Recall's come from the request, propagation's from the link graph, and eviction's
// are sorted by computed value. Two transactions holding overlapping sets in different orders is
// the textbook recipe, and Postgres resolves it by killing one of them.
//
// The test drives the two paths at each other over one deliberately overlapping id set, with the
// deleting side walking the ids in the REVERSE order the recalling side uses - which is what
// eviction's value ordering amounts to in the worst case. Before the fix this fails within a
// handful of rounds; SQLite cannot exhibit it (one writer), so it is Postgres-gated like its
// neighbours.
func TestConcurrentRecallAndDeleteDoNotDeadlock(t *testing.T) {
	database := newPostgresTestDB(t)
	ctx := context.Background()

	const (
		memories = 120
		rounds   = 25
	)

	ids := make([]string, memories)

	for i := range ids {
		ids[i] = fmt.Sprintf("deadlock-%04d", i)
	}

	// A fully linked pair per neighbouring memory, so a recall of one propagates into the other and
	// the propagation UPDATE touches rows the deleting side is also holding.
	seed := func() {
		for i, id := range ids {
			if _, err := database.CreateMemory(ctx, types.Memory{
				Id:           id,
				Body:         "deadlock probe",
				Significance: int32(1000 + i),
				TimeStamp:    time.Now().UnixNano(),
			}); err != nil && !IsDuplicateKey(err) {
				// A memory the previous round's delete raced past is still there; re-seeding over it
				// is the point, not a failure.
				t.Fatalf("CreateMemory: %s", err)
			}
		}

		for i := 0; i+1 < len(ids); i += 2 {
			if err := database.LinkMemories(ctx, ids[i], []types.Link{{Id: ids[i+1], Significance: 100}}); err != nil {
				t.Fatalf("LinkMemories: %s", err)
			}
		}
	}

	seed()

	var deadlocks int
	var mu sync.Mutex

	note := func(stage string, err error) {
		if err == nil {

			return
		}

		// 40P01 is Postgres' serialization/deadlock code. Anything else here is a different fault
		// and should fail loudly rather than be counted.
		if !strings.Contains(err.Error(), "deadlock detected") {
			t.Errorf("%s: unexpected error: %s", stage, err)

			return
		}

		mu.Lock()
		deadlocks++

		// Which kind matters: a raw deadlock means the path is not wrapped in a retry at all, while
		// an exhausted ErrWriteConflict means it is wrapped but the contention outlasted the budget.
		kind := "RAW (unwrapped path)"

		if IsWriteConflict(err) && strings.Contains(err.Error(), "failed after") {
			kind = "EXHAUSTED (retries ran out)"
		}

		t.Logf("deadlock in %s: %s", stage, kind)
		mu.Unlock()
	}

	for round := 0; round < rounds; round++ {
		var wg sync.WaitGroup

		wg.Add(2)

		// The recalling side: ascending, with link propagation into the neighbours.
		go func() {
			defer wg.Done()

			if _, err := database.RecallMemories(ctx, ids); err != nil {
				note("recall", err)

				return
			}

			// Spreading activation is the second mutation of the same rows, and is what the live
			// run had enabled (consolidation.linkRecallPropagation).
			if err := database.ReinforceLinkedMemories(ctx, ids, 0.5); err != nil {
				note("propagate", err)
			}
		}()

		// The deleting side: descending, as eviction's value ordering can be. This is also the
		// transaction that takes memories -> memory_links -> memories, against the linking side's
		// memory_links -> memories.
		go func() {
			defer wg.Done()

			reversed := make([]string, len(ids))

			for i, v := range ids {
				reversed[len(ids)-1-i] = v
			}

			if _, err := database.DeleteMemories(ctx, reversed); err != nil {
				note("delete", err)
			}
		}()

		// The linking side, which is what makes this cross-TABLE rather than merely cross-row. It
		// was absent from the first version of this test, which is why the first fix - one lock
		// order for the ids WITHIN each table - looked complete and was not.
		wg.Add(1)

		go func() {
			defer wg.Done()

			for i := len(ids) - 2; i >= 0; i -= 2 {
				if err := database.LinkMemories(ctx, ids[i], []types.Link{{Id: ids[i+1], Significance: 200}}); err != nil {
					// A link whose end has just been deleted by the other goroutine is the expected
					// outcome of this race, not a failure of it.
					if strings.Contains(err.Error(), "deadlock detected") {
						note("link", err)
					}

					return
				}
			}
		}()

		wg.Wait()

		seed()
	}

	if deadlocks > 0 {
		t.Errorf("%d deadlocks across %d rounds - concurrent recall and delete must acquire row locks in one order",
			deadlocks, rounds)
	}
}
