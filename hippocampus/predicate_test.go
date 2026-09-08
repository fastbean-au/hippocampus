package hippocampus

import (
	"context"
	"fmt"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/fastbean-au/hippocampus/contract"
	"github.com/fastbean-au/hippocampus/db"
	"github.com/fastbean-au/hippocampus/types"
)

// seedGroupedMemories writes n memories into the given group, each attached to the given event
// ("" for none), and returns their ids.
func seedGroupedMemories(t *testing.T, s *Server, group string, eventId string, n int) []string {
	t.Helper()

	ids := make([]string, 0, n)

	for i := range n {
		id := fmt.Sprintf("%s-m%03d", group, i)

		if _, err := s.db.CreateMemory(context.Background(), types.Memory{
			Id:           id,
			Body:         "body " + id,
			TimeStamp:    int64(1000 + i),
			Significance: 5,
			EventId:      eventId,
			Group:        group,
		}); err != nil {
			t.Fatalf("CreateMemory(%s): %s", id, err)
		}

		ids = append(ids, id)
	}

	return ids
}

func countMemoriesInGroup(t *testing.T, s *Server, group string) int {
	t.Helper()

	res, err := s.GetMemories(context.Background(), &contract.GetMemoriesRequest{Group: group, Limit: 1})
	if err != nil {
		t.Fatalf("GetMemories(%s): %s", group, err)
	}

	return int(res.GetTotalCount())
}

// TestDeleteMemoriesByFilter_RefusesAnEmptyFilter is the guard the whole feature rests on: an
// unfiltered request must never be read as "delete everything". Purge is that operation, and it is
// a different RPC with a different tier and a different scope rule.
func TestDeleteMemoriesByFilter_RefusesAnEmptyFilter(t *testing.T) {
	s := newTestServer(t)
	seedGroupedMemories(t, s, "a", "", 3)

	for name, req := range map[string]*contract.DeleteMemoriesByFilterRequest{
		"nothing at all":       {},
		"only a bound":         {MaxDeletions: 10},
		"only a cleanup flag":  {DeleteEmptyEvents: true},
		"both non-selectors":   {MaxDeletions: 10, DeleteEmptyEvents: true},
		"an empty group label": {Group: ""},
	} {
		t.Run(name, func(t *testing.T) {
			res, err := s.DeleteMemoriesByFilter(context.Background(), req)

			if status.Code(err) != codes.InvalidArgument {
				t.Fatalf("DeleteMemoriesByFilter(%s) = %v, want InvalidArgument", name, err)
			}

			if res.GetMemoriesDeleted() != 0 {
				t.Errorf("a refused request reported %d deletions", res.GetMemoriesDeleted())
			}
		})
	}

	if n := countMemoriesInGroup(t, s, "a"); n != 3 {
		t.Errorf("the refused requests deleted memories anyway: %d remain, want 3", n)
	}
}

// TestDeleteEventsByFilter_RefusesAnEmptyFilter is the events' half of the same guard, including
// the case that motivated naming the flag delete_memories rather than memories: a request carrying
// only that flag says what to do with the selection, not what to select, so it is still empty.
func TestDeleteEventsByFilter_RefusesAnEmptyFilter(t *testing.T) {
	s := newTestServer(t)

	if _, err := s.db.CreateEvent(context.Background(), types.Event{Id: "e1", Name: "e", TimeStart: 100, Significance: 5}); err != nil {
		t.Fatalf("CreateEvent: %s", err)
	}

	for name, req := range map[string]*contract.DeleteEventsByFilterRequest{
		"nothing at all":       {},
		"only a bound":         {MaxDeletions: 10},
		"only delete_memories": {DeleteMemories: true},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := s.DeleteEventsByFilter(context.Background(), req); status.Code(err) != codes.InvalidArgument {
				t.Fatalf("DeleteEventsByFilter(%s) = %v, want InvalidArgument", name, err)
			}
		})
	}

	if n := s.db.CountEvents(context.Background()); n != 1 {
		t.Errorf("the refused requests deleted events anyway: %d remain, want 1", n)
	}
}

// TestEverySelectingFieldIsRecognised is the drift guard behind the empty-filter refusal.
//
// The refusal is computed from the BUILT filter, so a selecting field that never reaches the filter
// would leave a request carrying only that field looking empty - and an empty request is refused,
// which is the safe direction but silently makes the field useless. This walks the request
// descriptors, sets each selecting field on its own, and requires the request to be accepted; the
// two fields that legitimately select nothing are named here and nowhere else, so a new field
// arriving with no handling fails the build's tests rather than shipping.
func TestEverySelectingFieldIsRecognised(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()

	memoryNonSelectors := map[string]bool{"max_deletions": true, "delete_empty_events": true}
	eventNonSelectors := map[string]bool{"max_deletions": true, "delete_memories": true}

	memorySetters := map[string]*contract.DeleteMemoriesByFilterRequest{
		"timestamp_min":         {TimestampMin: 1},
		"timestamp_max":         {TimestampMax: 1},
		"significance_min":      {SignificanceMin: 1},
		"significance_max":      {SignificanceMax: 1},
		"group":                 {Group: "a"},
		"significance_extremum": {SignificanceExtremum: contract.SignificanceExtremum_SIGNIFICANCE_EXTREMUM_HIGHEST},
		"metadata":              {Metadata: []string{"k=v"}},
		"recalled":              {Recalled: contract.Bool_FALSE},
		"recall_count_min":      {RecallCountMin: 1},
		"recall_count_max":      {RecallCountMax: 1},
		"time_recalled_min":     {TimeRecalledMin: 1},
		"time_recalled_max":     {TimeRecalledMax: 1},
		"is_summary":            {IsSummary: contract.Bool_FALSE},
		"is_binary":             {IsBinary: contract.Bool_FALSE},
		"event_id":              {EventId: "e1"},
		"has_event":             {HasEvent: contract.Bool_FALSE},
	}

	eventSetters := map[string]*contract.DeleteEventsByFilterRequest{
		"time_start_min":        {TimeStartMin: 1},
		"time_start_max":        {TimeStartMax: 1},
		"time_end_min":          {TimeEndMin: 1},
		"time_end_max":          {TimeEndMax: 1},
		"significance_min":      {SignificanceMin: 1},
		"significance_max":      {SignificanceMax: 1},
		"group":                 {Group: "a"},
		"significance_extremum": {SignificanceExtremum: contract.SignificanceExtremum_SIGNIFICANCE_EXTREMUM_HIGHEST},
		"metadata":              {Metadata: []string{"k=v"}},
		"ended":                 {Ended: contract.Bool_FALSE},
		"name_contains":         {NameContains: "x"},
	}

	fieldNames := func(m proto.Message) []string {
		fields := m.ProtoReflect().Descriptor().Fields()
		out := make([]string, 0, fields.Len())

		for i := range fields.Len() {
			out = append(out, string(fields.Get(i).Name()))
		}

		return out
	}

	for _, name := range fieldNames(&contract.DeleteMemoriesByFilterRequest{}) {
		if memoryNonSelectors[name] {
			continue
		}

		req, ok := memorySetters[name]
		if !ok {
			t.Errorf("DeleteMemoriesByFilterRequest.%s is not exercised here - add it to memorySetters, or to memoryNonSelectors if it selects nothing", name)

			continue
		}

		if _, err := s.DeleteMemoriesByFilter(ctx, req); status.Code(err) == codes.InvalidArgument {
			t.Errorf("a request setting only %s was refused as empty: the field never reaches the filter", name)
		}
	}

	for _, name := range fieldNames(&contract.DeleteEventsByFilterRequest{}) {
		if eventNonSelectors[name] {
			continue
		}

		req, ok := eventSetters[name]
		if !ok {
			t.Errorf("DeleteEventsByFilterRequest.%s is not exercised here - add it to eventSetters, or to eventNonSelectors if it selects nothing", name)

			continue
		}

		if _, err := s.DeleteEventsByFilter(ctx, req); status.Code(err) == codes.InvalidArgument {
			t.Errorf("a request setting only %s was refused as empty: the field never reaches the filter", name)
		}
	}
}

// TestDeleteMemoriesByFilter_DeletesOnlyWhatMatches is the offboarding case: one group goes, the
// other is untouched.
func TestDeleteMemoriesByFilter_DeletesOnlyWhatMatches(t *testing.T) {
	s := newTestServer(t)

	seedGroupedMemories(t, s, "a", "", 4)
	seedGroupedMemories(t, s, "b", "", 3)

	res, err := s.DeleteMemoriesByFilter(context.Background(), &contract.DeleteMemoriesByFilterRequest{Group: "a"})
	if err != nil {
		t.Fatalf("DeleteMemoriesByFilter: %s", err)
	}

	if res.GetMemoriesDeleted() != 4 {
		t.Errorf("memories_deleted = %d, want 4", res.GetMemoriesDeleted())
	}

	if !res.GetComplete() {
		t.Error("complete = false on an unbounded call that exhausted the filter")
	}

	if n := countMemoriesInGroup(t, s, "a"); n != 0 {
		t.Errorf("group a still holds %d memories", n)
	}

	if n := countMemoriesInGroup(t, s, "b"); n != 3 {
		t.Errorf("group b holds %d memories, want 3 - the filter reached past its group", n)
	}
}

// TestDeleteMemoriesByFilter_MatchesTheListing pins the promise the contract makes: GetMemories
// with the same fields is the dry run, so its total_count is exactly what the deletion removes.
// The two build their predicate with one function (selection.go), and this is what would fail if
// somebody gave either of them a second one.
func TestDeleteMemoriesByFilter_MatchesTheListing(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()

	seedGroupedMemories(t, s, "a", "", 6)
	seedGroupedMemories(t, s, "b", "", 4)

	// A filter with several dimensions, so a divergence in any one of them shows up.
	listed, err := s.GetMemories(ctx, &contract.GetMemoriesRequest{
		Group:        "a",
		TimestampMin: 1002,
		Recalled:     contract.Bool_FALSE,
		Limit:        1,
	})
	if err != nil {
		t.Fatalf("GetMemories: %s", err)
	}

	if listed.GetTotalCount() == 0 {
		t.Fatal("the dry run matched nothing, so the comparison would prove nothing")
	}

	deleted, err := s.DeleteMemoriesByFilter(ctx, &contract.DeleteMemoriesByFilterRequest{
		Group:        "a",
		TimestampMin: 1002,
		Recalled:     contract.Bool_FALSE,
	})
	if err != nil {
		t.Fatalf("DeleteMemoriesByFilter: %s", err)
	}

	if int64(listed.GetTotalCount()) != deleted.GetMemoriesDeleted() {
		t.Errorf(
			"the listing matched %d memories and the deletion removed %d - the dry run does not describe the deletion",
			listed.GetTotalCount(), deleted.GetMemoriesDeleted(),
		)
	}
}

// TestDeleteMemoriesByFilter_MaxDeletions checks the bound and, more importantly, that a bounded
// call reports itself incomplete - otherwise an operator taking a hundred rows at a time would
// have no way to know when to stop.
func TestDeleteMemoriesByFilter_MaxDeletions(t *testing.T) {
	s := newTestServer(t)

	seedGroupedMemories(t, s, "a", "", 5)

	res, err := s.DeleteMemoriesByFilter(context.Background(), &contract.DeleteMemoriesByFilterRequest{
		Group:        "a",
		MaxDeletions: 2,
	})
	if err != nil {
		t.Fatalf("DeleteMemoriesByFilter: %s", err)
	}

	if res.GetMemoriesDeleted() != 2 {
		t.Errorf("memories_deleted = %d, want 2", res.GetMemoriesDeleted())
	}

	if res.GetComplete() {
		t.Error("complete = true with three matching memories still standing")
	}

	if n := countMemoriesInGroup(t, s, "a"); n != 3 {
		t.Errorf("group a holds %d memories, want 3", n)
	}

	// The same request again picks up where it left off, which is the whole point of the bound.
	again, err := s.DeleteMemoriesByFilter(context.Background(), &contract.DeleteMemoriesByFilterRequest{
		Group:        "a",
		MaxDeletions: 10,
	})
	if err != nil {
		t.Fatalf("DeleteMemoriesByFilter (second pass): %s", err)
	}

	if again.GetMemoriesDeleted() != 3 || !again.GetComplete() {
		t.Errorf("second pass deleted %d complete=%v, want 3 complete=true", again.GetMemoriesDeleted(), again.GetComplete())
	}
}

// TestDeleteMemoriesByFilter_BeyondOneBatch drives the loop past its batch size. The selection
// always takes the first matching ids, so it is the deletion of one batch that makes the next
// selection return the next - and a bug there is an infinite loop or a silent stop at 500.
func TestDeleteMemoriesByFilter_BeyondOneBatch(t *testing.T) {
	if testing.Short() {
		t.Skip("seeds more than one batch of memories")
	}

	s := newTestServer(t)

	const n = deleteByFilterBatch + 17

	seedGroupedMemories(t, s, "a", "", n)

	res, err := s.DeleteMemoriesByFilter(context.Background(), &contract.DeleteMemoriesByFilterRequest{Group: "a"})
	if err != nil {
		t.Fatalf("DeleteMemoriesByFilter: %s", err)
	}

	if res.GetMemoriesDeleted() != n {
		t.Errorf("memories_deleted = %d, want %d", res.GetMemoriesDeleted(), n)
	}

	if !res.GetComplete() {
		t.Error("complete = false after exhausting the filter")
	}
}

// TestDeleteMemoriesByFilter_DeleteEmptyEvents checks both halves of the cleanup: an event whose
// memories all went is removed, and one that still holds a memory is not.
func TestDeleteMemoriesByFilter_DeleteEmptyEvents(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()

	for _, id := range []string{"e-emptied", "e-partial"} {
		if _, err := s.db.CreateEvent(ctx, types.Event{Id: id, Name: id, TimeStart: 100, Significance: 5, Group: "a"}); err != nil {
			t.Fatalf("CreateEvent(%s): %s", id, err)
		}
	}

	// Both of e-emptied's memories match the filter; only one of e-partial's does.
	for _, m := range []types.Memory{
		{Id: "m1", Body: "x", TimeStamp: 100, Significance: 5, EventId: "e-emptied", Group: "a"},
		{Id: "m2", Body: "x", TimeStamp: 100, Significance: 5, EventId: "e-emptied", Group: "a"},
		{Id: "m3", Body: "x", TimeStamp: 100, Significance: 5, EventId: "e-partial", Group: "a"},
		{Id: "m4", Body: "x", TimeStamp: 100, Significance: 5, EventId: "e-partial", Group: "b"},
	} {
		if _, err := s.db.CreateMemory(ctx, m); err != nil {
			t.Fatalf("CreateMemory(%s): %s", m.Id, err)
		}
	}

	res, err := s.DeleteMemoriesByFilter(ctx, &contract.DeleteMemoriesByFilterRequest{
		Group:             "a",
		DeleteEmptyEvents: true,
	})
	if err != nil {
		t.Fatalf("DeleteMemoriesByFilter: %s", err)
	}

	if res.GetMemoriesDeleted() != 3 {
		t.Errorf("memories_deleted = %d, want 3", res.GetMemoriesDeleted())
	}

	if res.GetEventsDeleted() != 1 {
		t.Errorf("events_deleted = %d, want 1 (only the event left with no memories)", res.GetEventsDeleted())
	}

	if _, err := s.db.GetEvent(ctx, "e-partial"); err != nil {
		t.Errorf("e-partial was deleted despite still holding a memory: %s", err)
	}

	if _, err := s.db.GetEvent(ctx, "e-emptied"); err == nil {
		t.Error("e-emptied survived with no memories left")
	}
}

// TestDeleteMemoriesByFilter_LeavesEventsAloneByDefault: the cleanup is opt-in, so without it an
// emptied event stays for the decay cycle to deal with.
func TestDeleteMemoriesByFilter_LeavesEventsAloneByDefault(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()

	if _, err := s.db.CreateEvent(ctx, types.Event{Id: "e1", Name: "e", TimeStart: 100, Significance: 5, Group: "a"}); err != nil {
		t.Fatalf("CreateEvent: %s", err)
	}

	seedGroupedMemories(t, s, "a", "e1", 2)

	res, err := s.DeleteMemoriesByFilter(ctx, &contract.DeleteMemoriesByFilterRequest{Group: "a"})
	if err != nil {
		t.Fatalf("DeleteMemoriesByFilter: %s", err)
	}

	if res.GetEventsDeleted() != 0 {
		t.Errorf("events_deleted = %d without delete_empty_events", res.GetEventsDeleted())
	}

	if _, err := s.db.GetEvent(ctx, "e1"); err != nil {
		t.Errorf("the event was deleted without being asked for: %s", err)
	}
}

// TestDeleteMemoriesByFilter_PrunesLinks: a predicate delete goes through the by-id chokepoint, so
// the link graph is maintained exactly as it is for any other deletion. A dangling edge would keep
// counting significance for its surviving end forever, which is the failure a DELETE ... WHERE
// would have introduced.
func TestDeleteMemoriesByFilter_PrunesLinks(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()

	seedGroupedMemories(t, s, "a", "", 1)
	seedGroupedMemories(t, s, "b", "", 1)

	if err := s.db.LinkMemories(ctx, "a-m000", []types.Link{{Id: "b-m000", Significance: 7}}); err != nil {
		t.Fatalf("LinkMemories: %s", err)
	}

	if _, err := s.DeleteMemoriesByFilter(ctx, &contract.DeleteMemoriesByFilterRequest{Group: "a"}); err != nil {
		t.Fatalf("DeleteMemoriesByFilter: %s", err)
	}

	links, significance, err := s.db.GetMemoryLinks(ctx, "b-m000", types.LinkDirectionBoth)
	if err != nil {
		t.Fatalf("GetMemoryLinks: %s", err)
	}

	if len(links) != 0 {
		t.Errorf("the survivor still carries %d link(s) to a deleted memory", len(links))
	}

	// And the denormalised aggregate the consolidation scans read went with them - a stale one
	// would keep the survivor propped up by an edge to something that no longer exists.
	if significance != 0 {
		t.Errorf("link_significance = %d after the far end was deleted, want 0", significance)
	}
}

// TestDeleteEventsByFilter_DeleteMemories: the events go and so do their memories.
func TestDeleteEventsByFilter_DeleteMemories(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()

	if _, err := s.db.CreateEvent(ctx, types.Event{Id: "e1", Name: "gone", TimeStart: 100, Significance: 5, Group: "a"}); err != nil {
		t.Fatalf("CreateEvent: %s", err)
	}

	if _, err := s.db.CreateEvent(ctx, types.Event{Id: "e2", Name: "kept", TimeStart: 100, Significance: 5, Group: "b"}); err != nil {
		t.Fatalf("CreateEvent: %s", err)
	}

	seedGroupedMemories(t, s, "a", "e1", 3)
	seedGroupedMemories(t, s, "b", "e2", 2)

	res, err := s.DeleteEventsByFilter(ctx, &contract.DeleteEventsByFilterRequest{
		Group:          "a",
		DeleteMemories: true,
	})
	if err != nil {
		t.Fatalf("DeleteEventsByFilter: %s", err)
	}

	if res.GetEventsDeleted() != 1 || res.GetMemoriesDeleted() != 3 || res.GetMemoriesOrphaned() != 0 {
		t.Errorf(
			"got events=%d memories=%d orphaned=%d, want 1/3/0",
			res.GetEventsDeleted(), res.GetMemoriesDeleted(), res.GetMemoriesOrphaned(),
		)
	}

	if !res.GetComplete() {
		t.Error("complete = false on an unbounded call")
	}

	if n := s.db.CountEvents(ctx); n != 1 {
		t.Errorf("%d events remain, want 1", n)
	}

	if n := countMemoriesInGroup(t, s, "b"); n != 2 {
		t.Errorf("group b holds %d memories, want 2", n)
	}
}

// TestDeleteEventsByFilter_OrphansMemories: without delete_memories the memories outlive their
// event with no event_id, exactly as DeleteEvent's default does.
func TestDeleteEventsByFilter_OrphansMemories(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()

	if _, err := s.db.CreateEvent(ctx, types.Event{Id: "e1", Name: "gone", TimeStart: 100, Significance: 5, Group: "a"}); err != nil {
		t.Fatalf("CreateEvent: %s", err)
	}

	seedGroupedMemories(t, s, "a", "e1", 3)

	res, err := s.DeleteEventsByFilter(ctx, &contract.DeleteEventsByFilterRequest{Group: "a"})
	if err != nil {
		t.Fatalf("DeleteEventsByFilter: %s", err)
	}

	if res.GetEventsDeleted() != 1 || res.GetMemoriesDeleted() != 0 || res.GetMemoriesOrphaned() != 3 {
		t.Errorf(
			"got events=%d memories=%d orphaned=%d, want 1/0/3",
			res.GetEventsDeleted(), res.GetMemoriesDeleted(), res.GetMemoriesOrphaned(),
		)
	}

	if n := countMemoriesInGroup(t, s, "a"); n != 3 {
		t.Errorf("group a holds %d memories, want 3 - orphaning should not delete them", n)
	}

	survivors, err := s.db.GetMemories(ctx, db.MemoryFilter{Limit: 10})
	if err != nil {
		t.Fatalf("GetMemories: %s", err)
	}

	for _, m := range *survivors {
		if m.EventId != "" {
			t.Errorf("memory %s still points at deleted event %q", m.Id, m.EventId)
		}
	}
}

// TestDeleteEventsByFilter_MaxDeletions bounds the events rather than the memories, and reports
// itself incomplete for the same reason its memory counterpart does.
func TestDeleteEventsByFilter_MaxDeletions(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()

	for i := range 4 {
		id := fmt.Sprintf("e%d", i)

		if _, err := s.db.CreateEvent(ctx, types.Event{Id: id, Name: id, TimeStart: 100, Significance: 5, Group: "a"}); err != nil {
			t.Fatalf("CreateEvent(%s): %s", id, err)
		}
	}

	res, err := s.DeleteEventsByFilter(ctx, &contract.DeleteEventsByFilterRequest{Group: "a", MaxDeletions: 1})
	if err != nil {
		t.Fatalf("DeleteEventsByFilter: %s", err)
	}

	if res.GetEventsDeleted() != 1 {
		t.Errorf("events_deleted = %d, want 1", res.GetEventsDeleted())
	}

	if res.GetComplete() {
		t.Error("complete = true with three matching events still standing")
	}

	if n := s.db.CountEvents(ctx); n != 3 {
		t.Errorf("%d events remain, want 3", n)
	}
}

// TestGroupScopeIsolation_DeleteByFilter drives both predicate deletions as a caller bound to group
// "a". It is a test of its own rather than a subtest of TestGroupScopeIsolation_Admin because it
// empties the partition it runs against, which the shared fixture's later subtests read.
func TestGroupScopeIsolation_DeleteByFilter(t *testing.T) {
	t.Run("a scoped caller cannot delete another group's memories", func(t *testing.T) {
		s := seedTwoGroups(t)

		// Asking explicitly for the other group matches nothing rather than erroring - the scope is
		// conjoined with the filter, exactly as it is on the listing, and an error would confirm the
		// group exists.
		res, err := s.DeleteMemoriesByFilter(scopedContext("a"), &contract.DeleteMemoriesByFilterRequest{Group: "b"})
		if err != nil {
			t.Fatalf("DeleteMemoriesByFilter: %s", err)
		}

		if res.GetMemoriesDeleted() != 0 {
			t.Errorf("a caller scoped to 'a' deleted %d of group b's memories", res.GetMemoriesDeleted())
		}

		if n := countMemoriesInGroup(t, s, "b"); n != 1 {
			t.Errorf("group b holds %d memories, want 1", n)
		}
	})

	t.Run("a broad filter is confined to the caller's own partition", func(t *testing.T) {
		s := seedTwoGroups(t)

		// No group named at all: the widest filter a bound caller can write, and it must still stop
		// at the boundary. This is the offboarding case for a tenant's own operator.
		res, err := s.DeleteMemoriesByFilter(scopedContext("a"), &contract.DeleteMemoriesByFilterRequest{TimestampMin: 1})
		if err != nil {
			t.Fatalf("DeleteMemoriesByFilter: %s", err)
		}

		if res.GetMemoriesDeleted() != 1 {
			t.Errorf("memories_deleted = %d, want 1 (its own group's only memory)", res.GetMemoriesDeleted())
		}

		if n := countMemoriesInGroup(t, s, "b"); n != 1 {
			t.Errorf("group b holds %d memories, want 1 - the scope did not confine the deletion", n)
		}
	})

	t.Run("a scoped caller cannot delete another group's events", func(t *testing.T) {
		s := seedTwoGroups(t)

		res, err := s.DeleteEventsByFilter(scopedContext("a"), &contract.DeleteEventsByFilterRequest{
			TimeStartMin:   1,
			DeleteMemories: true,
		})
		if err != nil {
			t.Fatalf("DeleteEventsByFilter: %s", err)
		}

		if res.GetEventsDeleted() != 1 {
			t.Errorf("events_deleted = %d, want 1", res.GetEventsDeleted())
		}

		if _, err := s.db.GetEvent(context.Background(), "e-b"); err != nil {
			t.Errorf("group b's event was deleted by a caller scoped to a: %s", err)
		}

		if n := countMemoriesInGroup(t, s, "b"); n != 1 {
			t.Errorf("group b holds %d memories, want 1", n)
		}
	})

	t.Run("an unscoped caller still reaches the whole store", func(t *testing.T) {
		s := seedTwoGroups(t)

		res, err := s.DeleteMemoriesByFilter(context.Background(), &contract.DeleteMemoriesByFilterRequest{TimestampMin: 1})
		if err != nil {
			t.Fatalf("DeleteMemoriesByFilter: %s", err)
		}

		if res.GetMemoriesDeleted() != 2 {
			t.Errorf("memories_deleted = %d, want 2 - an unscoped caller sees the whole store", res.GetMemoriesDeleted())
		}
	})
}

// failingPredicateStore wraps a real store and fails one of the methods a predicate deletion uses,
// so each error branch can be driven without a broken database underneath everything else.
// Modelled on failingCallbackStore in callbacks_coverage_test.go.
type failingPredicateStore struct {
	db.Store

	failMemoryIds        bool
	failMemoryIdsAfter   *int
	failEventIds         bool
	failEventIdsFor      bool
	failDeleteMemories   bool
	failDeleteEvents     bool
	failDeleteEventEmpty bool
	failEventMemories    bool
	failUnsetEventId     bool
}

func (f failingPredicateStore) MemoryIdsMatching(ctx context.Context, filter db.MemoryFilter) ([]string, error) {
	if f.failMemoryIds {
		return nil, errStoreFailed
	}

	// A pointer, because the store is held in an interface by value: a counter on the struct would
	// be reset on every call.
	if f.failMemoryIdsAfter != nil {
		if *f.failMemoryIdsAfter <= 0 {
			return nil, errStoreFailed
		}

		*f.failMemoryIdsAfter--
	}

	return f.Store.MemoryIdsMatching(ctx, filter)
}

func (f failingPredicateStore) EventIdsMatching(ctx context.Context, filter db.EventFilter) ([]string, error) {
	if f.failEventIds {
		return nil, errStoreFailed
	}

	return f.Store.EventIdsMatching(ctx, filter)
}

func (f failingPredicateStore) EventIdsForMemories(ctx context.Context, ids []string) ([]string, error) {
	if f.failEventIdsFor {
		return nil, errStoreFailed
	}

	return f.Store.EventIdsForMemories(ctx, ids)
}

func (f failingPredicateStore) DeleteMemories(ctx context.Context, ids []string) (int, error) {
	if f.failDeleteMemories {
		return 0, errStoreFailed
	}

	return f.Store.DeleteMemories(ctx, ids)
}

func (f failingPredicateStore) DeleteEvents(ctx context.Context, ids []string) (int, error) {
	if f.failDeleteEvents {
		return 0, errStoreFailed
	}

	return f.Store.DeleteEvents(ctx, ids)
}

func (f failingPredicateStore) DeleteEventIfEmpty(ctx context.Context, id string, cause db.DeleteCause) (bool, error) {
	if f.failDeleteEventEmpty {
		return false, errStoreFailed
	}

	return f.Store.DeleteEventIfEmpty(ctx, id, cause)
}

func (f failingPredicateStore) DeleteEventMemories(ctx context.Context, eventId string) (int, error) {
	if f.failEventMemories {
		return 0, errStoreFailed
	}

	return f.Store.DeleteEventMemories(ctx, eventId)
}

func (f failingPredicateStore) UnsetMemoriesEventId(ctx context.Context, eventId string) (int, error) {
	if f.failUnsetEventId {
		return 0, errStoreFailed
	}

	return f.Store.UnsetMemoriesEventId(ctx, eventId)
}

// TestDeleteByFilter_StoreFailures drives every error branch. Two properties matter beyond the
// error itself: a failure reports what had ALREADY been deleted rather than zero - those rows are
// gone whether or not the call finished - and it never reports itself complete.
func TestDeleteByFilter_StoreFailures(t *testing.T) {
	ctx := context.Background()

	seed := func(t *testing.T) *Server {
		t.Helper()

		s := newTestServer(t)

		if _, err := s.db.CreateEvent(ctx, types.Event{Id: "e1", Name: "e", TimeStart: 100, Significance: 5, Group: "a"}); err != nil {
			t.Fatalf("CreateEvent: %s", err)
		}

		seedGroupedMemories(t, s, "a", "e1", 2)

		return s
	}

	t.Run("the memory selection fails", func(t *testing.T) {
		s := seed(t)
		s.db = failingPredicateStore{Store: s.db, failMemoryIds: true}

		res, err := s.DeleteMemoriesByFilter(ctx, &contract.DeleteMemoriesByFilterRequest{Group: "a"})
		if err == nil {
			t.Fatal("a failing selection reported success")
		}

		if res.GetComplete() {
			t.Error("complete = true after a failure")
		}
	})

	t.Run("a failure part way through reports what had already gone", func(t *testing.T) {
		s := seed(t)

		rounds := 1
		s.db = failingPredicateStore{Store: s.db, failMemoryIdsAfter: &rounds}

		// One round succeeds (deleting both memories), the next selection fails. What went is gone
		// whether or not the call finished, so reporting zero would be a lie in the dangerous
		// direction.
		res, err := s.DeleteMemoriesByFilter(ctx, &contract.DeleteMemoriesByFilterRequest{Group: "a"})
		if err == nil {
			t.Fatal("a failing selection reported success")
		}

		if res.GetMemoriesDeleted() != 2 {
			t.Errorf("memories_deleted = %d after a mid-run failure, want 2", res.GetMemoriesDeleted())
		}

		if res.GetComplete() {
			t.Error("complete = true after a failure")
		}
	})

	t.Run("reading the events to clean up fails", func(t *testing.T) {
		s := seed(t)
		s.db = failingPredicateStore{Store: s.db, failEventIdsFor: true}

		if _, err := s.DeleteMemoriesByFilter(ctx, &contract.DeleteMemoriesByFilterRequest{
			Group:             "a",
			DeleteEmptyEvents: true,
		}); err == nil {
			t.Fatal("a failing event lookup reported success")
		}

		// It is read BEFORE the delete, so nothing has gone.
		if n := countMemoriesInGroup(t, s, "a"); n != 2 {
			t.Errorf("%d memories remain, want 2 - the lookup failure should precede the delete", n)
		}
	})

	t.Run("the memory delete fails", func(t *testing.T) {
		s := seed(t)
		s.db = failingPredicateStore{Store: s.db, failDeleteMemories: true}

		res, err := s.DeleteMemoriesByFilter(ctx, &contract.DeleteMemoriesByFilterRequest{Group: "a"})
		if err == nil {
			t.Fatal("a failing delete reported success")
		}

		if res.GetMemoriesDeleted() != 0 || res.GetComplete() {
			t.Errorf("got deleted=%d complete=%v, want 0/false", res.GetMemoriesDeleted(), res.GetComplete())
		}
	})

	t.Run("cleaning up an emptied event fails without failing the deletion", func(t *testing.T) {
		s := seed(t)
		s.db = failingPredicateStore{Store: s.db, failDeleteEventEmpty: true}

		res, err := s.DeleteMemoriesByFilter(ctx, &contract.DeleteMemoriesByFilterRequest{
			Group:             "a",
			DeleteEmptyEvents: true,
		})
		if err != nil {
			t.Fatalf("a tidy-up failure failed the whole deletion: %s", err)
		}

		if res.GetMemoriesDeleted() != 2 {
			t.Errorf("memories_deleted = %d, want 2", res.GetMemoriesDeleted())
		}

		if res.GetEventsDeleted() != 0 {
			t.Errorf("events_deleted = %d, want 0 - the cleanup failed", res.GetEventsDeleted())
		}
	})

	t.Run("the event selection fails", func(t *testing.T) {
		s := seed(t)
		s.db = failingPredicateStore{Store: s.db, failEventIds: true}

		if _, err := s.DeleteEventsByFilter(ctx, &contract.DeleteEventsByFilterRequest{Group: "a"}); err == nil {
			t.Fatal("a failing selection reported success")
		}
	})

	t.Run("emptying an event fails", func(t *testing.T) {
		s := seed(t)
		s.db = failingPredicateStore{Store: s.db, failEventMemories: true}

		if _, err := s.DeleteEventsByFilter(ctx, &contract.DeleteEventsByFilterRequest{
			Group:          "a",
			DeleteMemories: true,
		}); err == nil {
			t.Fatal("a failing memory delete reported success")
		}

		// The event is emptied before it is deleted, so a failure there leaves it standing rather
		// than leaving its memories pointing at nothing.
		if _, err := s.db.GetEvent(ctx, "e1"); err != nil {
			t.Errorf("the event went despite its memories failing to: %s", err)
		}
	})

	t.Run("orphaning an event's memories fails", func(t *testing.T) {
		s := seed(t)
		s.db = failingPredicateStore{Store: s.db, failUnsetEventId: true}

		if _, err := s.DeleteEventsByFilter(ctx, &contract.DeleteEventsByFilterRequest{Group: "a"}); err == nil {
			t.Fatal("a failing event_id clear reported success")
		}
	})

	t.Run("the event delete fails", func(t *testing.T) {
		s := seed(t)
		s.db = failingPredicateStore{Store: s.db, failDeleteEvents: true}

		res, err := s.DeleteEventsByFilter(ctx, &contract.DeleteEventsByFilterRequest{Group: "a"})
		if err == nil {
			t.Fatal("a failing delete reported success")
		}

		// The memories were orphaned before the event delete was attempted, and the response says
		// so rather than reporting nothing happened.
		if res.GetMemoriesOrphaned() != 2 {
			t.Errorf("memories_orphaned = %d, want 2", res.GetMemoriesOrphaned())
		}
	})
}

// TestDeleteMemoriesByFilter_RejectsAnInvalidSelection: the shared builder's validation reaches
// this RPC too, so a nonsensical range is refused here exactly as it is on the listing.
func TestDeleteMemoriesByFilter_RejectsAnInvalidSelection(t *testing.T) {
	s := newTestServer(t)

	if _, err := s.DeleteMemoriesByFilter(context.Background(), &contract.DeleteMemoriesByFilterRequest{
		SignificanceMin: 9,
		SignificanceMax: 2,
	}); err == nil {
		t.Error("an inverted significance range was accepted")
	}

	if _, err := s.DeleteMemoriesByFilter(context.Background(), &contract.DeleteMemoriesByFilterRequest{
		Metadata: []string{"not-a-pair"},
	}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("a malformed metadata filter = %v, want InvalidArgument", err)
	}
}

// TestDeleteEventsByFilter_RejectsAnInvalidSelection is the events' half.
func TestDeleteEventsByFilter_RejectsAnInvalidSelection(t *testing.T) {
	s := newTestServer(t)

	if _, err := s.DeleteEventsByFilter(context.Background(), &contract.DeleteEventsByFilterRequest{
		TimeStartMin: 500,
		TimeEndMin:   100,
	}); err == nil {
		t.Error("a time range ending before it starts was accepted")
	}

	if _, err := s.DeleteEventsByFilter(context.Background(), &contract.DeleteEventsByFilterRequest{
		Metadata: []string{"not-a-pair"},
	}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("a malformed metadata filter = %v, want InvalidArgument", err)
	}
}
