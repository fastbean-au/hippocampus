package hippocampus

import (
	"context"
	"fmt"
	"sort"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/fastbean-au/hippocampus/contract"
	"github.com/fastbean-au/hippocampus/types"
)

// GetMemoriesRequest.ids is the way to read memories by id without reinforcing them (TODO-3 item
// 156). RecallMemories was the only by-id read that returned a body, and recalling resets the decay
// clock - so an inspection read (an operator checking a record, a subject-access lookup, a sync
// check) made the memory it looked at more durable, in a store whose whole premise is that reading
// changes the future.

func seedIdMemories(t *testing.T, s *Server, memories ...types.Memory) {
	t.Helper()

	for _, m := range memories {
		if m.Body == "" {
			m.Body = "body of " + m.Id
		}

		if m.TimeStamp == 0 {
			m.TimeStamp = 1000
		}

		if m.Significance == 0 {
			m.Significance = 5
		}

		if _, err := s.db.CreateMemory(context.Background(), m); err != nil {
			t.Fatalf("CreateMemory(%s): %s", m.Id, err)
		}
	}
}

func returnedIds(res *contract.GetMemoriesResponse) []string {
	ids := make([]string, 0, len(res.GetMemories()))

	for _, v := range res.GetMemories() {
		ids = append(ids, v.GetId())
	}

	sort.Strings(ids)

	return ids
}

func TestGetMemories_ByIdReturnsJustThoseAndDoesNotReinforce(t *testing.T) {
	t.Parallel()

	s := newTestServer(t)
	ctx := context.Background()

	seedIdMemories(t, s, types.Memory{Id: "m1"}, types.Memory{Id: "m2"}, types.Memory{Id: "m3"})

	res, err := s.GetMemories(ctx, &contract.GetMemoriesRequest{Ids: []string{"m1", "m3"}})
	if err != nil {
		t.Fatalf("GetMemories: %s", err)
	}

	if got := fmt.Sprint(returnedIds(res)); got != "[m1 m3]" {
		t.Fatalf("returned %s, want [m1 m3]", got)
	}

	if res.GetTotalCount() != 2 {
		t.Errorf("total_count = %d, want 2", res.GetTotalCount())
	}

	if res.GetMemories()[0].GetBody() == "" {
		t.Error("the read returned no body, which is the reason to read by id rather than explain")
	}

	stored, err := s.db.GetMemoriesByIds(ctx, []string{"m1", "m3"})
	if err != nil {
		t.Fatalf("GetMemoriesByIds: %s", err)
	}

	for _, m := range *stored {
		if m.RecallCount != 0 || m.TimeRecalled != 0 {
			t.Errorf("%s was reinforced by a read (recall_count %d, time_recalled %d)", m.Id, m.RecallCount, m.TimeRecalled)
		}
	}
}

// An id the store does not hold is absent rather than an error: the answer must not distinguish
// "never stored" from "outside your scope", and a caller asking about several ids wants the ones
// that exist.
func TestGetMemories_ByIdOmitsAnUnknownId(t *testing.T) {
	t.Parallel()

	s := newTestServer(t)

	seedIdMemories(t, s, types.Memory{Id: "m1"})

	res, err := s.GetMemories(context.Background(), &contract.GetMemoriesRequest{Ids: []string{"m1", "never-stored"}})
	if err != nil {
		t.Fatalf("GetMemories: %s", err)
	}

	if got := fmt.Sprint(returnedIds(res)); got != "[m1]" {
		t.Errorf("returned %s, want [m1]", got)
	}
}

func TestGetMemories_ByIdComposesWithTheOtherFilters(t *testing.T) {
	t.Parallel()

	s := newTestServer(t)

	seedIdMemories(t, s, types.Memory{Id: "m1", Group: "a"}, types.Memory{Id: "m2", Group: "b"})

	res, err := s.GetMemories(context.Background(), &contract.GetMemoriesRequest{Ids: []string{"m1", "m2"}, Group: "a"})
	if err != nil {
		t.Fatalf("GetMemories: %s", err)
	}

	if got := fmt.Sprint(returnedIds(res)); got != "[m1]" {
		t.Errorf("returned %s, want [m1]", got)
	}
}

// With linked_to, which also narrows to a set of ids, the answer is the intersection - and an empty
// intersection is an empty page, never the whole store.
func TestGetMemories_ByIdIntersectsLinkedTo(t *testing.T) {
	t.Parallel()

	s := newTestServer(t)
	ctx := context.Background()

	seedIdMemories(t, s, types.Memory{Id: "m1"}, types.Memory{Id: "m2"}, types.Memory{Id: "m3"}, types.Memory{Id: "m4"})

	if err := s.db.LinkMemories(ctx, "m1", []types.Link{{Id: "m2", Significance: 1}, {Id: "m3", Significance: 1}}); err != nil {
		t.Fatalf("LinkMemories: %s", err)
	}

	res, err := s.GetMemories(ctx, &contract.GetMemoriesRequest{LinkedTo: "m1", Ids: []string{"m2", "m4"}})
	if err != nil {
		t.Fatalf("GetMemories: %s", err)
	}

	if got := fmt.Sprint(returnedIds(res)); got != "[m2]" {
		t.Errorf("returned %s, want [m2], the intersection", got)
	}

	res, err = s.GetMemories(ctx, &contract.GetMemoriesRequest{LinkedTo: "m1", Ids: []string{"m4"}})
	if err != nil {
		t.Fatalf("GetMemories: %s", err)
	}

	if len(res.GetMemories()) != 0 || res.GetTotalCount() != 0 {
		t.Errorf("an empty intersection returned %v (total %d), want nothing", returnedIds(res), res.GetTotalCount())
	}
}

func TestGetMemories_ByIdRefusesTooMany(t *testing.T) {
	t.Parallel()

	s := newTestServer(t)

	ids := make([]string, 201)
	for i := range ids {
		ids[i] = fmt.Sprintf("m%d", i)
	}

	_, err := s.GetMemories(context.Background(), &contract.GetMemoriesRequest{Ids: ids})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("GetMemories with 201 ids = %v, want InvalidArgument", err)
	}
}

// A scoped caller naming another group's id gets nothing back and no error - the same answer as for
// an id that was never stored, so the read cannot confirm the record exists.
func TestGetMemories_ByIdHonoursGroupScope(t *testing.T) {
	t.Parallel()

	s := seedTwoGroups(t)

	res, err := s.GetMemories(scopedContext("a"), &contract.GetMemoriesRequest{Ids: []string{"m-a", "m-b"}})
	if err != nil {
		t.Fatalf("GetMemories: %s", err)
	}

	if got := fmt.Sprint(returnedIds(res)); got != "[m-a]" {
		t.Errorf("returned %s, want only the caller's [m-a]", got)
	}

	assertNoLeak(t, "GetMemories(ids)", res.String())
}
