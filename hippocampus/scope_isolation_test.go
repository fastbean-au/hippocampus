package hippocampus

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/fastbean-au/hippocampus/contract"
	"github.com/fastbean-au/hippocampus/db"
	"github.com/fastbean-au/hippocampus/search"
	"github.com/fastbean-au/hippocampus/types"
)

// The group-scope acceptance test.
//
// hippocampus/scope.go's table declares how each RPC honours a caller's scope; this file is what
// verifies the handlers actually do it. It seeds two groups, then drives EVERY RPC as a caller bound
// to group "a" and asserts that group "b"'s records are never returned, never mutated, and never
// confirmed to exist.
//
// A per-RPC test would not have caught what this is for. The failure being guarded against is a
// handler that forgot the scope entirely - which looks like a perfectly working RPC from every
// angle except this one - so the value is in the enumeration being exhaustive and in it being
// checked against the service descriptor (TestEveryRPCIsCoveredByIsolationTest, below).

// seedTwoGroups creates one event and one memory in each of groups "a" and "b", plus a link between
// the two groups' memories, and returns the server. The cross-group link is deliberate: it is the
// one edge that a scoped caller can legitimately encounter and must not be able to follow.
func seedTwoGroups(t *testing.T) *Server {
	t.Helper()

	s := newTestServer(t)
	ctx := context.Background()

	for _, group := range []string{"a", "b"} {
		if _, err := s.db.CreateEvent(ctx, types.Event{
			Id:           "e-" + group,
			Name:         "event " + group,
			TimeStart:    100,
			Significance: 5,
			Group:        group,
		}); err != nil {
			t.Fatalf("CreateEvent(%s): %s", group, err)
		}

		if _, err := s.db.CreateMemory(ctx, types.Memory{
			Id:           "m-" + group,
			Body:         "secret belonging to " + group,
			TimeStamp:    100,
			Significance: 5,
			EventId:      "e-" + group,
			Group:        group,
		}); err != nil {
			t.Fatalf("CreateMemory(%s): %s", group, err)
		}
	}

	// An unscoped caller can create a link across the boundary; a scoped one must not be able to
	// read the far end back through it.
	if err := s.db.LinkMemories(ctx, "m-a", []types.Link{{Id: "m-b", Significance: 3}}); err != nil {
		t.Fatalf("LinkMemories across groups: %s", err)
	}

	if err := s.db.LinkEvents(ctx, "e-a", []types.Link{{Id: "e-b", Significance: 3}}); err != nil {
		t.Fatalf("LinkEvents across groups: %s", err)
	}

	return s
}

// assertNotFound requires an error that neither succeeds nor reveals why it failed. NotFound is the
// only acceptable code: PermissionDenied would confirm the record exists, which is the leak these
// checks exist to prevent.
func assertNotFound(t *testing.T, rpc string, err error) {
	t.Helper()

	if err == nil {
		t.Errorf("%s: a scoped caller reached another group's record and got no error", rpc)

		return
	}

	if code := status.Code(err); code != codes.NotFound {
		t.Errorf("%s: got code %v, want NotFound (any other code distinguishes 'exists elsewhere' from 'does not exist')", rpc, code)
	}
}

// noopEvents and noopMemories are walkStore's callbacks for the tests that care only about what the
// walk captured, not what an Export/Transfer would then do with it.
func noopEvents(_ []types.Event) error { return nil }

func noopMemories(_ []types.Memory) error { return nil }

// forgetBothGroups turns the forgotten log on and consolidates every memory in the store, so both
// groups have a tombstone for the scope checks to tell apart. It uses the real consolidation pass
// rather than writing rows directly: a tombstone that did not come from a delete would not prove
// the group was carried across from the memory it records.
func forgetBothGroups(t *testing.T, s *Server) {
	t.Helper()

	database, ok := s.db.(*db.DB)
	if !ok {
		t.Fatal("the forgotten log needs the concrete store to set its policy")
	}

	database.SetTombstonePolicy(db.TombstonePolicy{Enabled: true})
	s.consolidation.tombstones = true

	if _, _, _, err := s.db.ConsolidateEventMemories(context.Background(), consolidateEverything{}); err != nil {
		t.Fatalf("ConsolidateEventMemories: %s", err)
	}
}

// consolidateEverything is a db.Server that forgets whatever it is shown.
type consolidateEverything struct{}

func (consolidateEverything) ShouldConsolidateMemory(db.MemoryConsolidationCandidate) bool {
	return true
}

func (consolidateEverything) ShouldConsolidateEvent(db.EventConsolidationCandidate) bool { return true }

func (consolidateEverything) MemoryValue(db.MemoryConsolidationCandidate) float64 { return 0.25 }

func (consolidateEverything) MemoryRetained(db.MemoryConsolidationCandidate) bool { return false }

func (consolidateEverything) DeletionThreshold() float64 { return 1.0 }

// assertNoLeak requires that no rendered response mentions group b's data.
func assertNoLeak(t *testing.T, rpc string, rendered string) {
	t.Helper()

	for _, needle := range []string{"m-b", "e-b", "belonging to b"} {
		if strings.Contains(rendered, needle) {
			t.Errorf("%s: response leaked %q from another group: %s", rpc, needle, rendered)
		}
	}
}

// TestGroupScopeIsolation_Reads drives every read RPC as a caller scoped to group "a".
func TestGroupScopeIsolation_Reads(t *testing.T) {
	s := seedTwoGroups(t)
	ctx := scopedContext("a")

	t.Run("GetMemories", func(t *testing.T) {
		res, err := s.GetMemories(ctx, &contract.GetMemoriesRequest{})
		if err != nil {
			t.Fatalf("GetMemories: %s", err)
		}

		if len(res.GetMemories()) != 1 || res.GetMemories()[0].GetId() != "m-a" {
			t.Errorf("GetMemories returned %d memories, want only m-a", len(res.GetMemories()))
		}

		// The count must be scoped too: a total larger than the page reports how much is hidden.
		if res.GetTotalCount() != 1 {
			t.Errorf("GetMemories total_count = %d, want 1 - the count must be scoped, not just the page", res.GetTotalCount())
		}

		assertNoLeak(t, "GetMemories", res.String())
	})

	t.Run("GetMemories with an out-of-scope group filter", func(t *testing.T) {
		res, err := s.GetMemories(ctx, &contract.GetMemoriesRequest{Group: "b"})

		// An empty page, not an error: an error would confirm the group exists.
		if err != nil {
			t.Fatalf("GetMemories(group=b): %s", err)
		}

		if len(res.GetMemories()) != 0 {
			t.Errorf("filtering by an out-of-scope group returned %d memories, want 0", len(res.GetMemories()))
		}
	})

	t.Run("GetEvents", func(t *testing.T) {
		res, err := s.GetEvents(ctx, &contract.GetEventsRequest{Memories: true})
		if err != nil {
			t.Fatalf("GetEvents: %s", err)
		}

		if len(res.GetEvents()) != 1 || res.GetEvents()[0].GetId() != "e-a" {
			t.Errorf("GetEvents returned %d events, want only e-a", len(res.GetEvents()))
		}

		if res.GetTotalCount() != 1 {
			t.Errorf("GetEvents total_count = %d, want 1", res.GetTotalCount())
		}

		assertNoLeak(t, "GetEvents", res.String())
	})

	t.Run("GetEventById", func(t *testing.T) {
		_, err := s.GetEventById(ctx, &contract.GetEventByIdRequest{Id: "e-b", Memories: true})
		assertNotFound(t, "GetEventById", err)
	})

	t.Run("RecallMemories", func(t *testing.T) {
		_, err := s.RecallMemories(ctx, &contract.RecallMemoriesRequest{Ids: []string{"m-b"}})
		assertNotFound(t, "RecallMemories", err)

		// And a mixed request must not partially succeed: recall is a write, so returning m-a while
		// silently skipping m-b would still have been the right answer only by accident.
		_, err = s.RecallMemories(ctx, &contract.RecallMemoriesRequest{Ids: []string{"m-a", "m-b"}})
		assertNotFound(t, "RecallMemories (mixed)", err)
	})

	t.Run("RecallMemories include_linked does not follow the cross-group edge", func(t *testing.T) {
		res, err := s.RecallMemories(ctx, &contract.RecallMemoriesRequest{
			Ids:           []string{"m-a"},
			IncludeLinked: true,
		})
		if err != nil {
			t.Fatalf("RecallMemories: %s", err)
		}

		assertNoLeak(t, "RecallMemories include_linked", res.String())
	})

	t.Run("GetMemories linked_to does not follow the cross-group edge", func(t *testing.T) {
		res, err := s.GetMemories(ctx, &contract.GetMemoriesRequest{LinkedTo: "m-a", Links: true})
		if err != nil {
			t.Fatalf("GetMemories(linked_to): %s", err)
		}

		assertNoLeak(t, "GetMemories linked_to", res.String())

		// The anchor itself must be unreachable from the other side.
		_, err = s.GetMemories(ctx, &contract.GetMemoriesRequest{LinkedTo: "m-b"})
		assertNotFound(t, "GetMemories(linked_to=m-b)", err)
	})

	t.Run("GetMemoryLinks", func(t *testing.T) {
		res, err := s.GetMemoryLinks(ctx, &contract.GetMemoryLinksRequest{Id: "m-a"})
		if err != nil {
			t.Fatalf("GetMemoryLinks: %s", err)
		}

		assertNoLeak(t, "GetMemoryLinks", res.String())

		_, err = s.GetMemoryLinks(ctx, &contract.GetMemoryLinksRequest{Id: "m-b"})
		assertNotFound(t, "GetMemoryLinks(m-b)", err)
	})

	t.Run("GetEventLinks", func(t *testing.T) {
		res, err := s.GetEventLinks(ctx, &contract.GetEventLinksRequest{Id: "e-a"})
		if err != nil {
			t.Fatalf("GetEventLinks: %s", err)
		}

		assertNoLeak(t, "GetEventLinks", res.String())

		_, err = s.GetEventLinks(ctx, &contract.GetEventLinksRequest{Id: "e-b"})
		assertNotFound(t, "GetEventLinks(e-b)", err)
	})

	t.Run("ExplainConsolidation", func(t *testing.T) {
		s.consolidationEnabled = true
		defer func() { s.consolidationEnabled = false }()

		_, err := s.ExplainConsolidation(ctx, &contract.ExplainConsolidationRequest{
			MemoryIds: []string{"m-b"},
		})
		assertNotFound(t, "ExplainConsolidation", err)
	})

	t.Run("GetSummarisationCandidates", func(t *testing.T) {
		// Seed the cache as the sleep cycle would - store-wide, covering both groups.
		s.summarisationCandidatesMu.Lock()
		s.summarisationCandidates = []db.SummarisationCandidate{
			{EventId: "e-a", EventName: "event a", MemoryCount: 9, Group: "a"},
			{EventId: "e-b", EventName: "event b", MemoryCount: 9, Group: "b"},
		}
		s.summarisationCandidatesMu.Unlock()

		res, err := s.GetSummarisationCandidates(ctx, &contract.EmptyRequest{})
		if err != nil {
			t.Fatalf("GetSummarisationCandidates: %s", err)
		}

		if len(res.GetCandidates()) != 1 || res.GetCandidates()[0].GetEventId() != "e-a" {
			t.Errorf("GetSummarisationCandidates returned %d candidates, want only e-a", len(res.GetCandidates()))
		}

		assertNoLeak(t, "GetSummarisationCandidates", res.String())
	})

	t.Run("WhoAmI reads no stored record", func(t *testing.T) {
		res, err := s.WhoAmI(ctx, &contract.EmptyRequest{})
		if err != nil {
			t.Fatalf("WhoAmI: %s", err)
		}

		assertNoLeak(t, "WhoAmI", res.String())
	})

	// The status RPC is scopeNone: it reports the cycle's schedule and aggregate counts, naming no
	// record. It is answered rather than refused to a scoped caller, unlike Purge/Sleep/Preview -
	// so what needs asserting is not that it is rejected but that what comes back carries nothing
	// from the group this caller cannot see.
	t.Run("GetConsolidationStatus reads no stored record", func(t *testing.T) {
		res, err := s.GetConsolidationStatus(ctx, &contract.EmptyRequest{})
		if err != nil {
			t.Fatalf("GetConsolidationStatus: %s", err)
		}

		assertNoLeak(t, "GetConsolidationStatus", res.String())
	})
}

// TestGroupScopeIsolation_Writes drives every mutating RPC as a caller scoped to group "a".
func TestGroupScopeIsolation_Writes(t *testing.T) {
	s := seedTwoGroups(t)
	ctx := scopedContext("a")

	t.Run("StoreMemory stamps the caller's group", func(t *testing.T) {
		res, err := s.StoreMemory(ctx, &contract.Memory{Id: "new-1", Body: "x", Significance: 5})
		if err != nil {
			t.Fatalf("StoreMemory: %s", err)
		}

		stored, err := s.db.GetMemoriesByIds(context.Background(), []string{res.GetId()})
		if err != nil || len(*stored) != 1 {
			t.Fatalf("reading back the stored memory: %v", err)
		}

		if got := (*stored)[0].Group; got != "a" {
			t.Errorf("StoreMemory stamped group %q, want %q - a bound writer must not create records it cannot read back", got, "a")
		}
	})

	t.Run("StoreMemory refuses another group", func(t *testing.T) {
		_, err := s.StoreMemory(ctx, &contract.Memory{Id: "new-2", Body: "x", Significance: 5, Group: "b"})

		if status.Code(err) != codes.PermissionDenied {
			t.Errorf("StoreMemory(group=b) = %v, want PermissionDenied", err)
		}
	})

	t.Run("StoreMemory refuses another group's event", func(t *testing.T) {
		_, err := s.StoreMemory(ctx, &contract.Memory{Id: "new-3", Body: "x", Significance: 5, EventId: "e-b"})
		assertNotFound(t, "StoreMemory(event_id=e-b)", err)
	})

	t.Run("StoreEvent stamps the caller's group", func(t *testing.T) {
		res, err := s.StoreEvent(ctx, &contract.Event{Id: "new-e", Name: "n", Significance: 5})
		if err != nil {
			t.Fatalf("StoreEvent: %s", err)
		}

		stored, err := s.db.GetEvent(context.Background(), res.GetId())
		if err != nil {
			t.Fatalf("reading back the stored event: %s", err)
		}

		if stored.Group != "a" {
			t.Errorf("StoreEvent stamped group %q, want %q", stored.Group, "a")
		}
	})

	t.Run("UpdateMemory", func(t *testing.T) {
		_, err := s.UpdateMemory(ctx, &contract.Memory{Id: "m-b", Body: "overwritten"})
		assertNotFound(t, "UpdateMemory", err)

		// The record must be untouched.
		stored, err := s.db.GetMemoriesByIds(context.Background(), []string{"m-b"})
		if err != nil || len(*stored) != 1 {
			t.Fatalf("reading back m-b: %v", err)
		}

		if (*stored)[0].Body == "overwritten" {
			t.Error("UpdateMemory mutated another group's memory despite returning an error")
		}
	})

	t.Run("UpdateMemory cannot push a record out of scope", func(t *testing.T) {
		_, err := s.UpdateMemory(ctx, &contract.Memory{Id: "m-a", Group: "b"})

		if status.Code(err) != codes.PermissionDenied {
			t.Errorf("UpdateMemory(group=b) = %v, want PermissionDenied", err)
		}

		_, err = s.UpdateMemory(ctx, &contract.Memory{Id: "m-a", ClearGroup: true})

		if status.Code(err) != codes.PermissionDenied {
			t.Errorf("UpdateMemory(clear_group) = %v, want PermissionDenied", err)
		}
	})

	t.Run("DeleteMemories", func(t *testing.T) {
		_, err := s.DeleteMemories(ctx, &contract.DeleteMemoriesRequest{Ids: []string{"m-b"}})
		assertNotFound(t, "DeleteMemories", err)

		stored, err := s.db.GetMemoriesByIds(context.Background(), []string{"m-b"})
		if err != nil || len(*stored) != 1 {
			t.Error("DeleteMemories deleted another group's memory despite returning an error")
		}
	})

	t.Run("EndEvent", func(t *testing.T) {
		_, err := s.EndEvent(ctx, &contract.EndEventRequest{Id: "e-b", TimeEnd: 999})
		assertNotFound(t, "EndEvent", err)
	})

	t.Run("UpdateEventSignificance", func(t *testing.T) {
		_, err := s.UpdateEventSignificance(ctx, &contract.UpdateEventSignificanceRequest{Id: "e-b", Significance: 99})
		assertNotFound(t, "UpdateEventSignificance", err)
	})

	t.Run("DeleteEvent", func(t *testing.T) {
		_, err := s.DeleteEvent(ctx, &contract.DeleteEventRequest{Id: "e-b", Memories: true})
		assertNotFound(t, "DeleteEvent", err)

		if _, err := s.db.GetEvent(context.Background(), "e-b"); err != nil {
			t.Error("DeleteEvent deleted another group's event despite returning an error")
		}
	})

	t.Run("MergeEvents", func(t *testing.T) {
		_, err := s.MergeEvents(ctx, &contract.MergeEventsRequest{MergeTo: "e-a", MergeFrom: "e-b"})
		assertNotFound(t, "MergeEvents(from another group)", err)

		_, err = s.MergeEvents(ctx, &contract.MergeEventsRequest{MergeTo: "e-b", MergeFrom: "e-a"})
		assertNotFound(t, "MergeEvents(to another group)", err)
	})

	t.Run("LinkMemories", func(t *testing.T) {
		_, err := s.LinkMemories(ctx, &contract.LinkMemoriesRequest{
			Id:    "m-a",
			Links: []*contract.Link{{Id: "m-b", Significance: 1}},
		})
		assertNotFound(t, "LinkMemories(far end in another group)", err)

		_, err = s.LinkMemories(ctx, &contract.LinkMemoriesRequest{
			Id:    "m-b",
			Links: []*contract.Link{{Id: "m-a", Significance: 1}},
		})
		assertNotFound(t, "LinkMemories(near end in another group)", err)
	})

	t.Run("LinkEvents", func(t *testing.T) {
		_, err := s.LinkEvents(ctx, &contract.LinkEventsRequest{
			Id:    "e-a",
			Links: []*contract.Link{{Id: "e-b", Significance: 1}},
		})
		assertNotFound(t, "LinkEvents", err)
	})

	t.Run("UnlinkMemories drops an out-of-scope target rather than refusing", func(t *testing.T) {
		// The existing m-a -> m-b link must survive: it is not this caller's to remove, and the
		// call must behave exactly as it would for a target that does not exist.
		res, err := s.UnlinkMemories(ctx, &contract.UnlinkMemoriesRequest{Id: "m-a", Ids: []string{"m-b"}})
		if err != nil {
			t.Fatalf("UnlinkMemories: %s", err)
		}

		if !res.GetOk() {
			t.Error("UnlinkMemories should report Ok, matching how it treats a nonexistent target")
		}

		edges, _, err := s.db.GetMemoryLinks(context.Background(), "m-a", types.LinkDirectionBoth)
		if err != nil {
			t.Fatalf("GetMemoryLinks: %s", err)
		}

		found := false
		for _, e := range edges {
			if e.Id == "m-b" {
				found = true
			}
		}

		if !found {
			t.Error("UnlinkMemories removed a cross-group link the caller cannot see")
		}
	})

	t.Run("UnlinkEvents near end in another group", func(t *testing.T) {
		_, err := s.UnlinkEvents(ctx, &contract.UnlinkEventsRequest{Id: "e-b", Ids: []string{"e-a"}})
		assertNotFound(t, "UnlinkEvents", err)
	})

	t.Run("ReplaceMemoriesWithSummary", func(t *testing.T) {
		_, err := s.ReplaceMemoriesWithSummary(ctx, &contract.ReplaceMemoriesWithSummaryRequest{
			EventId: "e-b",
			Summary: &contract.Memory{Body: "summary", Significance: 5},
		})
		assertNotFound(t, "ReplaceMemoriesWithSummary", err)
	})

	t.Run("SummariseMemories", func(t *testing.T) {
		_, err := s.SummariseMemories(ctx, &contract.SummariseMemoriesRequest{EventId: "e-b"})

		// Scope is checked before the summariser is consulted, so this must be NotFound rather than
		// the FailedPrecondition an unconfigured summariser would give - otherwise the two codes
		// would tell the caller whether e-b exists.
		assertNotFound(t, "SummariseMemories", err)
	})
}

// TestGroupScopeIsolation_Admin covers the RPCs refused outright to a scoped caller, and the
// data-movement ones that are narrowed to the caller's partition instead.
func TestGroupScopeIsolation_Admin(t *testing.T) {
	s := seedTwoGroups(t)
	s.consolidationEnabled = true

	ctx := scopedContext("a")

	t.Run("Purge is refused", func(t *testing.T) {
		_, err := s.Purge(ctx, &contract.EmptyRequest{})

		if status.Code(err) != codes.PermissionDenied {
			t.Errorf("Purge = %v, want PermissionDenied", err)
		}

		if n := s.db.CountEvents(context.Background()); n != 2 {
			t.Errorf("Purge emptied the store despite being refused (%d events remain, want 2)", n)
		}
	})

	t.Run("Sleep is refused", func(t *testing.T) {
		_, err := s.Sleep(ctx, &contract.EmptyRequest{})

		if status.Code(err) != codes.PermissionDenied {
			t.Errorf("Sleep = %v, want PermissionDenied", err)
		}
	})

	t.Run("PreviewConsolidation is refused", func(t *testing.T) {
		_, err := s.PreviewConsolidation(ctx, &contract.PreviewConsolidationRequest{})

		if status.Code(err) != codes.PermissionDenied {
			t.Errorf("PreviewConsolidation = %v, want PermissionDenied", err)
		}
	})

	t.Run("the callback queue is refused", func(t *testing.T) {
		// Both halves, because a queued delivery batches memories across groups and so cannot be
		// partitioned: a bound caller could only ever be shown - or empty - infrastructure that is
		// not theirs.
		if _, err := s.GetCallbackQueue(ctx, &contract.GetCallbackQueueRequest{}); status.Code(err) != codes.PermissionDenied {
			t.Errorf("GetCallbackQueue = %v, want PermissionDenied", err)
		}

		if _, err := s.DeleteCallbackQueue(ctx, &contract.DeleteCallbackQueueRequest{All: true}); status.Code(err) != codes.PermissionDenied {
			t.Errorf("DeleteCallbackQueue = %v, want PermissionDenied", err)
		}
	})

	t.Run("GetTopology is refused", func(t *testing.T) {
		_, err := s.GetTopology(ctx, &contract.EmptyRequest{})

		if status.Code(err) != codes.PermissionDenied {
			t.Errorf("GetTopology = %v, want PermissionDenied", err)
		}
	})

	t.Run("the store walk is narrowed to the caller's partition", func(t *testing.T) {
		manifest, events, memories, err := s.walkStore(ctx, noopEvents, noopMemories)
		if err != nil {
			t.Fatalf("walkStore: %s", err)
		}

		if events != 1 || memories != 1 {
			t.Errorf("walkStore captured %d events and %d memories, want 1 and 1", events, memories)
		}

		for _, id := range manifest.eventIds {
			if id == "e-b" {
				t.Error("walkStore captured another group's event - Export/Transfer/Clear would move or delete it")
			}
		}

		for _, snapshot := range manifest.memories {
			if snapshot.Id == "m-b" {
				t.Error("walkStore captured another group's memory")
			}
		}
	})

	t.Run("an unscoped caller still walks the whole store", func(t *testing.T) {
		_, events, memories, err := s.walkStore(context.Background(), noopEvents, noopMemories)
		if err != nil {
			t.Fatalf("walkStore: %s", err)
		}

		if events != 2 || memories != 2 {
			t.Errorf("an unscoped walkStore captured %d events and %d memories, want 2 and 2", events, memories)
		}
	})

	t.Run("the forgotten log is scoped to the caller's partition", func(t *testing.T) {
		forgetBothGroups(t, s)

		res, err := s.GetForgottenMemories(ctx, &contract.GetForgottenMemoriesRequest{})
		if err != nil {
			t.Fatalf("GetForgottenMemories: %s", err)
		}

		if len(res.GetMemories()) != 1 || res.GetMemories()[0].GetId() != "m-a" {
			t.Fatalf("GetForgottenMemories returned %d records, want only m-a", len(res.GetMemories()))
		}

		// total counts within the scope too: an unscoped total would report how many of another
		// group's memories had been forgotten without naming one of them.
		if res.GetTotal() != 1 {
			t.Errorf("GetForgottenMemories total = %d, want 1 (the caller's partition only)", res.GetTotal())
		}

		assertNoLeak(t, "GetForgottenMemories", res.String())
	})

	t.Run("clearing the forgotten log leaves another group's records alone", func(t *testing.T) {
		deleted, err := s.DeleteForgottenMemories(ctx, &contract.DeleteForgottenMemoriesRequest{All: true})
		if err != nil {
			t.Fatalf("DeleteForgottenMemories: %s", err)
		}

		if deleted.GetDeleted() != 1 {
			t.Errorf("DeleteForgottenMemories deleted %d records, want 1", deleted.GetDeleted())
		}

		// The unscoped view still holds group b's record.
		remaining, err := s.GetForgottenMemories(context.Background(), &contract.GetForgottenMemoriesRequest{})
		if err != nil {
			t.Fatalf("GetForgottenMemories (unscoped): %s", err)
		}

		if len(remaining.GetMemories()) != 1 || remaining.GetMemories()[0].GetId() != "m-b" {
			t.Errorf("a scoped clear removed another group's tombstones: %v", remaining.GetMemories())
		}
	})

	t.Run("ImportBatch stamps the caller's group", func(t *testing.T) {
		_, err := s.ImportBatch(ctx, &contract.ImportBatchRequest{
			Memories: []*contract.Memory{{Id: "imported-1", Body: "x", Significance: 5, Group: "b"}},
		})

		// The group on the wire is not honoured: an archive is a file, so trusting it would let a
		// scoped caller write into any partition by editing one.
		if status.Code(err) != codes.PermissionDenied {
			t.Errorf("ImportBatch(group=b) = %v, want PermissionDenied", err)
		}

		if _, err := s.ImportBatch(ctx, &contract.ImportBatchRequest{
			Memories: []*contract.Memory{{Id: "imported-2", Body: "x", Significance: 5}},
		}); err != nil {
			t.Fatalf("ImportBatch: %s", err)
		}

		stored, err := s.db.GetMemoriesByIds(context.Background(), []string{"imported-2"})
		if err != nil || len(*stored) != 1 {
			t.Fatalf("reading back the imported memory: %v", err)
		}

		if got := (*stored)[0].Group; got != "a" {
			t.Errorf("ImportBatch stamped group %q, want %q", got, "a")
		}
	})
}

// TestGroupScopeIsolation_UnscopedCallerIsUnchanged is the other half of the guarantee: everything
// above must not have narrowed anything for a deployment that does not use group scoping, which is
// every existing one. An unscoped caller sees both groups through every read.
// TestGroupScopeIsolation_EventMemoryCounts pins the one thing GetEvents' memory_count could leak
// that its nested memories cannot: a number. The event is in the caller's scope, so the row itself
// is theirs to read, but it holds a memory carrying a group they do not — and an unscoped count
// would report that memory's existence without ever naming it, which assertNoLeak could not catch.
//
// This needs its own store rather than seedTwoGroups', whose events each hold only their own
// group's memory: the mismatch is the whole point of the case.
func TestGroupScopeIsolation_EventMemoryCounts(t *testing.T) {
	s := newEventTestServer(t)
	ctx := context.Background()

	if _, err := s.db.CreateEvent(ctx, types.Event{Id: "e-a", Name: "event a", TimeStart: 100, Significance: 5, Group: "a"}); err != nil {
		t.Fatalf("CreateEvent: %s", err)
	}

	for _, m := range []types.Memory{
		{Id: "m-a", Body: "mine", TimeStamp: 100, Significance: 5, EventId: "e-a", Group: "a"},
		{Id: "m-b", Body: "not mine", TimeStamp: 100, Significance: 5, EventId: "e-a", Group: "b"},
	} {
		if _, err := s.db.CreateMemory(ctx, m); err != nil {
			t.Fatalf("CreateMemory(%s): %s", m.Id, err)
		}
	}

	scoped, err := s.GetEvents(scopedContext("a"), &contract.GetEventsRequest{MemoryCounts: true})
	if err != nil {
		t.Fatalf("GetEvents (scoped): %s", err)
	}

	if len(scoped.GetEvents()) != 1 {
		t.Fatalf("expected 1 event, got %d", len(scoped.GetEvents()))
	}

	if got := scoped.GetEvents()[0].GetMemoryCount(); got != 1 {
		t.Errorf("a caller scoped to group a saw a memory count of %d, want 1 - the count must be scoped exactly as the memories are", got)
	}

	unscoped, err := s.GetEvents(ctx, &contract.GetEventsRequest{MemoryCounts: true})
	if err != nil {
		t.Fatalf("GetEvents (unscoped): %s", err)
	}

	if got := unscoped.GetEvents()[0].GetMemoryCount(); got != 2 {
		t.Errorf("an unscoped caller saw a memory count of %d, want 2", got)
	}

	// GetEventById carries the same count and so the same leak; it takes the scope separately, so
	// it needs its own assertion rather than inheriting this one.
	byId, err := s.GetEventById(scopedContext("a"), &contract.GetEventByIdRequest{Id: "e-a", MemoryCounts: true})
	if err != nil {
		t.Fatalf("GetEventById (scoped): %s", err)
	}

	if got := byId.GetEvent().GetMemoryCount(); got != 1 {
		t.Errorf("GetEventById reported a memory count of %d to a caller scoped to group a, want 1", got)
	}
}

func TestGroupScopeIsolation_UnscopedCallerIsUnchanged(t *testing.T) {
	s := seedTwoGroups(t)
	ctx := context.Background()

	memories, err := s.GetMemories(ctx, &contract.GetMemoriesRequest{})
	if err != nil {
		t.Fatalf("GetMemories: %s", err)
	}

	if len(memories.GetMemories()) != 2 {
		t.Errorf("an unscoped caller saw %d memories, want 2", len(memories.GetMemories()))
	}

	events, err := s.GetEvents(ctx, &contract.GetEventsRequest{})
	if err != nil {
		t.Fatalf("GetEvents: %s", err)
	}

	if len(events.GetEvents()) != 2 {
		t.Errorf("an unscoped caller saw %d events, want 2", len(events.GetEvents()))
	}

	if _, err := s.GetEventById(ctx, &contract.GetEventByIdRequest{Id: "e-b"}); err != nil {
		t.Errorf("an unscoped caller could not read e-b: %s", err)
	}

	links, err := s.GetMemoryLinks(ctx, &contract.GetMemoryLinksRequest{Id: "m-a"})
	if err != nil {
		t.Fatalf("GetMemoryLinks: %s", err)
	}

	if len(links.GetLinks()) != 1 {
		t.Errorf("an unscoped caller saw %d links from m-a, want 1 - the cross-group edge must still be visible", len(links.GetLinks()))
	}
}

// TestEveryRPCIsCoveredByIsolationTest ties the tests above to the service descriptor. Without it,
// an RPC added later would be caught by TestScopesCoverEveryRPC (which forces a declaration) but
// could still ship with a declaration that nothing verifies - the table would say "scopeFilter" and
// the handler would filter by nothing.
//
// The list is maintained by hand rather than derived, because "an RPC is exercised by a subtest"
// cannot be observed from inside the test binary. What the descriptor check buys is that adding an
// RPC forces somebody to look at this file.
func TestEveryRPCIsCoveredByIsolationTest(t *testing.T) {
	covered := map[string]bool{
		// TestGroupScopeIsolation_Reads
		"GetMemories":                true,
		"GetEvents":                  true,
		"GetEventById":               true,
		"RecallMemories":             true,
		"GetMemoryLinks":             true,
		"GetEventLinks":              true,
		"ExplainConsolidation":       true,
		"GetSummarisationCandidates": true,
		"WhoAmI":                     true,
		"GetConsolidationStatus":     true,

		// TestGroupScopeIsolation_Writes
		"StoreMemory":                true,
		"StoreEvent":                 true,
		"UpdateMemory":               true,
		"DeleteMemories":             true,
		"EndEvent":                   true,
		"UpdateEventSignificance":    true,
		"DeleteEvent":                true,
		"MergeEvents":                true,
		"LinkMemories":               true,
		"LinkEvents":                 true,
		"UnlinkMemories":             true,
		"UnlinkEvents":               true,
		"ReplaceMemoriesWithSummary": true,
		"SummariseMemories":          true,

		// TestGroupScopeIsolation_Admin
		"Purge":                   true,
		"Sleep":                   true,
		"PreviewConsolidation":    true,
		"GetTopology":             true,
		"ImportBatch":             true,
		"GetForgottenMemories":    true,
		"DeleteForgottenMemories": true,
		"GetCallbackQueue":        true,
		"DeleteCallbackQueue":     true,

		// Export, Transfer and Clear are covered through walkStore, which is the entirety of what
		// each of them scopes - they differ only in what they do with the manifest it returns (an S3
		// upload, a gRPC stream to another instance, a delete), none of which re-reads the store.
		// Exercising the walk once is therefore the honest test; driving the three RPCs would need
		// an object store and a second instance to assert the same property.
		"Export":   true,
		"Transfer": true,
		"Clear":    true,

		// SearchMemories needs a content-search backend. It is covered by
		// TestSearchMemories_GroupScope below, which skips when one is unavailable.
		"SearchMemories": true,

		// Import takes an archive body; the group stamping it shares with ImportBatch is in
		// ingestMemories/ingestEvents, exercised above.
		"Import": true,
	}

	for _, m := range contract.Hippocampus_ServiceDesc.Methods {
		if !covered[m.MethodName] {
			t.Errorf("RPC %q is not covered by the group-scope isolation tests - add a subtest in scope_isolation_test.go, or record here why it needs none", m.MethodName)
		}
	}
}

// TestSearchMemories_GroupScope covers the search path, which needs a content-search backend.
func TestSearchMemories_GroupScope(t *testing.T) {
	s := seedTwoGroups(t)

	idx, err := search.NewSQL(s.db.(*db.DB))
	if err != nil {
		t.Skipf("content search is unavailable on this store: %s", err)
	}

	s.search = idx

	res, err := s.SearchMemories(scopedContext("a"), &contract.SearchMemoriesRequest{Query: "secret"})
	if err != nil {
		t.Fatalf("SearchMemories: %s", err)
	}

	assertNoLeak(t, "SearchMemories", res.String())

	for _, m := range res.GetMemories() {
		if m.GetId() != "m-a" {
			t.Errorf("SearchMemories returned %q, want only m-a", m.GetId())
		}
	}
}
