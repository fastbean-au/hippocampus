package db

import (
	"context"
	"fmt"
	"testing"

	"github.com/fastbean-au/hippocampus/types"
)

// Group scoping's storage half had no test of its own in this package. Every one of its branches
// was reachable only through the hippocampus package's RPC-level isolation suite, which instruments
// its own package and so credits none of this file - leaving the one predicate that decides which
// records a bound token may reach executing in CI with no assertion here that it does.
//
// These tests are deliberately at the db layer: what hippocampus/scope_isolation_test.go proves is
// that each RPC applies a scope, and what these prove is that the predicate the RPCs hand down
// actually restricts the query - including under the chunking that a large id list takes, which no
// RPC-level test reaches.

// seedScopedStore fills a store with two memories and two events per group, across three groups,
// plus one memory and one event carrying no group at all. The ungrouped pair is the case worth
// having: an empty group_name is a value like any other, so it must fall OUTSIDE every non-empty
// scope rather than being treated as a wildcard.
func seedScopedStore(t *testing.T, database *DB) {
	t.Helper()

	ctx := context.Background()

	for _, group := range []string{"alpha", "beta", "gamma", ""} {
		for i := range 2 {
			id := fmt.Sprintf("%s-%d", group, i)
			if group == "" {
				id = fmt.Sprintf("ungrouped-%d", i)
			}

			event := types.Event{
				Id:           "e-" + id,
				Name:         "event " + id,
				TimeStart:    1,
				Significance: 5,
				Group:        group,
			}

			if _, err := database.CreateEvent(ctx, event); err != nil {
				t.Fatalf("CreateEvent(%s): %s", event.Id, err)
			}

			memory := types.Memory{
				Id:           "m-" + id,
				Body:         "scoped body " + id,
				TimeStamp:    1,
				Significance: 5,
				Group:        group,
				EventId:      event.Id,
			}

			if _, err := database.CreateMemory(ctx, memory); err != nil {
				t.Fatalf("CreateMemory(%s): %s", memory.Id, err)
			}
		}
	}
}

// TestGroupScopeConditions covers the predicate builder itself, whose empty-scope arm is the one
// place in the package where "no groups" must mean the whole store rather than none of it. Reading
// that the other way empties every read on an unauthenticated instance, which is why the meaning is
// asserted here rather than left to the callers that thread it through.
func TestGroupScopeConditions(t *testing.T) {
	if clause, args := groupScopeConditions("", nil); clause != "" || args != nil {
		t.Errorf("an empty scope must produce no predicate at all, got %q with %v", clause, args)
	}

	if clause, args := groupScopeConditions("", []string{}); clause != "" || args != nil {
		t.Errorf("an empty (non-nil) scope must produce no predicate at all, got %q with %v", clause, args)
	}

	clause, args := groupScopeConditions("", []string{"alpha", "beta"})

	if clause == "" {
		t.Fatal("a non-empty scope must produce a predicate")
	}

	if len(args) != 2 || args[0] != "alpha" || args[1] != "beta" {
		t.Errorf("expected the groups bound as arguments in order, got %v", args)
	}

	// The alias exists for the one query that joins (search's "m."), and getting it wrong there is
	// an ambiguous-column error rather than a wrong answer, so the prefixing is asserted.
	aliased, _ := groupScopeConditions("m.", []string{"alpha"})

	unaliased, _ := groupScopeConditions("", []string{"alpha"})

	if aliased == unaliased {
		t.Error("the alias must prefix the column")
	}
}

// TestAppendGroupScope covers the append wrapper, including that an unscoped call leaves the query
// and its arguments untouched - the property every server-owned scan depends on.
func TestAppendGroupScope(t *testing.T) {
	base := `SELECT id FROM memories WHERE id > ?`
	baseArgs := []any{"cursor"}

	query, args := appendGroupScope(base, baseArgs, "", nil)

	if query != base || len(args) != 1 {
		t.Errorf("an unscoped append must be a no-op, got %q with %v", query, args)
	}

	query, args = appendGroupScope(base, baseArgs, "", []string{"alpha"})

	if query == base {
		t.Error("a scoped append must extend the query")
	}

	if len(args) != 2 || args[1] != "alpha" {
		t.Errorf("a scoped append must extend the arguments, got %v", args)
	}
}

// TestMemoryIdsOutsideGroups drives the id-check counterpart across every arm: in scope, out of
// scope, absent entirely, an unscoped caller, and an empty id list.
func TestMemoryIdsOutsideGroups(t *testing.T) {
	database := newTestDB(t)
	seedScopedStore(t, database)

	ctx := context.Background()

	checks := []struct {
		name    string
		ids     []string
		groups  []string
		outside []string
	}{
		{
			name:   "an unscoped caller reaches everything",
			ids:    []string{"m-alpha-0", "m-beta-0", "m-ungrouped-0"},
			groups: nil,
		},
		{
			name:   "no ids is no work",
			ids:    nil,
			groups: []string{"alpha"},
		},
		{
			name:   "every id inside the scope",
			ids:    []string{"m-alpha-0", "m-alpha-1"},
			groups: []string{"alpha"},
		},
		{
			name:    "an id in another group is outside",
			ids:     []string{"m-alpha-0", "m-beta-0"},
			groups:  []string{"alpha"},
			outside: []string{"m-beta-0"},
		},
		{
			name:    "an ungrouped memory is outside every scope",
			ids:     []string{"m-ungrouped-0"},
			groups:  []string{"alpha"},
			outside: []string{"m-ungrouped-0"},
		},
		{
			name:    "an id that does not exist reports as outside",
			ids:     []string{"m-alpha-0", "absent"},
			groups:  []string{"alpha"},
			outside: []string{"absent"},
		},
		{
			name:   "a multi-group scope reaches all of them",
			ids:    []string{"m-alpha-0", "m-beta-0", "m-gamma-0"},
			groups: []string{"alpha", "beta", "gamma"},
		},
	}

	for _, check := range checks {
		t.Run(check.name, func(t *testing.T) {
			outside, err := database.MemoryIdsOutsideGroups(ctx, check.ids, check.groups)
			if err != nil {
				t.Fatalf("MemoryIdsOutsideGroups: %s", err)
			}

			assertSameIds(t, outside, check.outside)
		})
	}
}

// TestEventIdsOutsideGroups is the event half. The two share one implementation, so this asserts
// that the graph selection actually reaches the events table rather than re-testing every arm: an
// event id and a memory id with the same suffix exist in the seed, so a wrong table would report
// the in-scope event as outside.
func TestEventIdsOutsideGroups(t *testing.T) {
	database := newTestDB(t)
	seedScopedStore(t, database)

	ctx := context.Background()

	outside, err := database.EventIdsOutsideGroups(ctx, []string{"e-alpha-0", "e-beta-0", "m-alpha-0"}, []string{"alpha"})
	if err != nil {
		t.Fatalf("EventIdsOutsideGroups: %s", err)
	}

	// m-alpha-0 is a memory id, so it is not an event at all and reads as outside - which is the
	// documented conflation of "absent" and "not yours", both of which a handler turns into the
	// same NotFound.
	assertSameIds(t, outside, []string{"e-beta-0", "m-alpha-0"})
}

// TestIdsOutsideGroupsChunks drives an id list spanning three chunks. The chunking exists so a
// large list cannot build an unbounded IN clause, and nothing at the RPC layer reaches it: the
// existing ids sit at either end so a chunk boundary mishandled in the middle is visible.
func TestIdsOutsideGroupsChunks(t *testing.T) {
	database := newTestDB(t)
	seedScopedStore(t, database)

	ctx := context.Background()

	ids := []string{"m-alpha-0"}
	for i := range deleteChunkSize * 2 {
		ids = append(ids, fmt.Sprintf("absent-%d", i))
	}

	ids = append(ids, "m-alpha-1")

	outside, err := database.MemoryIdsOutsideGroups(ctx, ids, []string{"alpha"})
	if err != nil {
		t.Fatalf("MemoryIdsOutsideGroups: %s", err)
	}

	if len(outside) != deleteChunkSize*2 {
		t.Fatalf("expected the %d absent ids reported outside, got %d", deleteChunkSize*2, len(outside))
	}

	for _, id := range outside {
		if id == "m-alpha-0" || id == "m-alpha-1" {
			t.Fatalf("%s is in scope and must not be reported outside", id)
		}
	}
}

// TestIdsOutsideGroupsOnClosedDB covers the query-failure arm, which the closed-database sweep does
// not reach because both exported methods short-circuit on an empty scope before touching the
// database at all.
func TestIdsOutsideGroupsOnClosedDB(t *testing.T) {
	database := newTestDB(t)

	if err := database.Close(); err != nil {
		t.Fatalf("Close: %s", err)
	}

	ctx := context.Background()

	if _, err := database.MemoryIdsOutsideGroups(ctx, []string{"m1"}, []string{"alpha"}); err == nil {
		t.Error("expected MemoryIdsOutsideGroups to surface the query failure")
	}

	if _, err := database.EventIdsOutsideGroups(ctx, []string{"e1"}, []string{"alpha"}); err == nil {
		t.Error("expected EventIdsOutsideGroups to surface the query failure")
	}
}

// TestGroupScopeNarrowsTheStoreWalks covers the predicate where it is threaded into a query rather
// than built: the export/transfer pagination, the event listing, the by-event memory count, and the
// content search's aliased form. Each is a separate call site, and a scope dropped at any one of
// them is a caller reading records that are not theirs.
func TestGroupScopeNarrowsTheStoreWalks(t *testing.T) {
	database := newTestDB(t)
	seedScopedStore(t, database)

	ctx := context.Background()
	scope := []string{"alpha"}

	memories, err := database.GetMemoriesPage(ctx, "", 100, scope)
	if err != nil {
		t.Fatalf("GetMemoriesPage: %s", err)
	}

	if len(memories) != 2 {
		t.Errorf("expected the 2 alpha memories, got %d", len(memories))
	}

	for _, memory := range memories {
		if memory.Group != "alpha" {
			t.Errorf("GetMemoriesPage returned %s from group %q", memory.Id, memory.Group)
		}
	}

	events, err := database.GetEventsPage(ctx, "", 100, scope)
	if err != nil {
		t.Fatalf("GetEventsPage: %s", err)
	}

	if len(events) != 2 {
		t.Errorf("expected the 2 alpha events, got %d", len(events))
	}

	listed, err := database.GetEvents(ctx, EventFilter{Groups: scope})
	if err != nil {
		t.Fatalf("GetEvents: %s", err)
	}

	if listed == nil || len(*listed) != 2 {
		t.Errorf("expected the 2 alpha events from GetEvents, got %v", listed)
	}

	// The by-event count is the one scoped query whose predicate sits beside an IN clause rather
	// than a filter, so a scope appended in the wrong place produces a count of the whole event.
	counts, err := database.CountMemoriesByEventIds(ctx, []string{"e-alpha-0", "e-beta-0"}, scope)
	if err != nil {
		t.Fatalf("CountMemoriesByEventIds: %s", err)
	}

	if counts["e-alpha-0"] != 1 {
		t.Errorf("expected the alpha event's memory counted, got %d", counts["e-alpha-0"])
	}

	if counts["e-beta-0"] != 0 {
		t.Errorf("expected the beta event's memory outside the scope, got %d", counts["e-beta-0"])
	}
}

// TestGroupScopeNarrowsContentSearch covers the aliased form, the only call site that passes one -
// a scope dropped there is not a wrong result but an ambiguous-column error, and a scope applied
// without the alias would not compile as SQL at all.
func TestGroupScopeNarrowsContentSearch(t *testing.T) {
	database := newTestDB(t)

	if !database.ContentSearchAvailable() {
		t.Skip("this dialect carries no content index")
	}

	seedScopedStore(t, database)

	ctx := context.Background()

	hits, err := database.SearchMemoryHits(ctx, ContentQuery{Text: "scoped", Limit: 100, Groups: []string{"alpha"}})
	if err != nil {
		t.Fatalf("SearchMemoryHits: %s", err)
	}

	if len(hits) != 2 {
		t.Fatalf("expected the 2 alpha memories, got %d", len(hits))
	}

	for _, hit := range hits {
		if hit.Id != "m-alpha-0" && hit.Id != "m-alpha-1" {
			t.Errorf("search returned %s from outside the scope", hit.Id)
		}
	}

	unscoped, err := database.SearchMemoryHits(ctx, ContentQuery{Text: "scoped", Limit: 100})
	if err != nil {
		t.Fatalf("SearchMemoryHits (unscoped): %s", err)
	}

	if len(unscoped) != 8 {
		t.Errorf("expected an unscoped search to reach all 8 memories, got %d", len(unscoped))
	}
}

// assertSameIds compares two id sets without regard to order, the ordering of the outside list
// being an artefact of the input rather than a promise.
func assertSameIds(t *testing.T, got []string, want []string) {
	t.Helper()

	if len(got) != len(want) {
		t.Fatalf("expected %v, got %v", want, got)
	}

	seen := make(map[string]bool, len(got))
	for _, id := range got {
		seen[id] = true
	}

	for _, id := range want {
		if !seen[id] {
			t.Errorf("expected %s among %v", id, got)
		}
	}
}
