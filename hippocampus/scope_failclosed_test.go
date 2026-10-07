package hippocampus

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/fastbean-au/hippocampus/contract"
	"github.com/fastbean-au/hippocampus/db"
	"github.com/fastbean-au/hippocampus/types"
)

// The scope filter's failure paths (TODO-3 item 177). The happy paths are covered by the isolation
// suite; these are the branches that decide what a scoped caller sees when the scope check itself
// cannot be answered, and the one fixture shape the suite lacked - a page whose links point both
// into and out of the caller's scope.

// TestDropOutOfScopeLinksFailsClosed pins that a failed scope check clears the links rather than
// returning them unchecked. The links are supplementary to the records they hang off, so losing them
// is the cheap failure; returning a far end the caller may not see is the leak this exists to stop.
func TestDropOutOfScopeLinksFailsClosed(t *testing.T) {
	t.Parallel()

	s := &Server{}

	kind := linkKind{
		name: "memory",
		outside: func(context.Context, []string) ([]string, error) {
			return nil, errors.New("store unavailable")
		},
	}

	links := map[string][]types.Link{
		"a1": {{Id: "a2", Significance: 1}, {Id: "b1", Significance: 1}},
		"a3": {{Id: "a2", Significance: 1}},
	}

	s.dropOutOfScopeLinks(context.Background(), kind, links)

	if len(links) != 0 {
		t.Errorf("a failed scope check must clear every link, got %v", links)
	}
}

// TestDropOutOfScopeLinksKeepsOnlyInScopeFarEnds checks the filter resolves every far end in one
// call and drops exactly the out-of-scope ones, leaving an item whose only link went out of scope
// with an empty list rather than removing the item.
func TestDropOutOfScopeLinksKeepsOnlyInScopeFarEnds(t *testing.T) {
	t.Parallel()

	s := &Server{}

	var asked [][]string

	kind := linkKind{
		name: "memory",
		outside: func(_ context.Context, ids []string) ([]string, error) {
			asked = append(asked, append([]string(nil), ids...))

			return []string{"b1"}, nil
		},
	}

	links := map[string][]types.Link{
		"a1": {{Id: "a2", Significance: 1}, {Id: "b1", Significance: 2}},
		"a3": {{Id: "b1", Significance: 3}},
	}

	s.dropOutOfScopeLinks(context.Background(), kind, links)

	want := map[string][]types.Link{
		"a1": {{Id: "a2", Significance: 1}},
		"a3": {},
	}

	if !reflect.DeepEqual(links, want) {
		t.Errorf("got %v, want %v", links, want)
	}

	if len(asked) != 1 || len(asked[0]) != 2 {
		t.Errorf("expected one scope query over the two distinct far ends, got %v", asked)
	}
}

// TestScopedListingDropsLinksCrossingTheBoundary is the same rule end to end, through GetMemories with
// links attached: a memory in the caller's group linked both to another in-scope memory and to one
// in a group the caller cannot see comes back with the in-scope link only.
func TestScopedListingDropsLinksCrossingTheBoundary(t *testing.T) {
	t.Parallel()

	s := newTestServer(t)
	ctx := context.Background()

	for _, m := range []types.Memory{
		{Id: "a1", Group: "a", Body: "one", Significance: 1, TimeStamp: 100},
		{Id: "a2", Group: "a", Body: "two", Significance: 1, TimeStamp: 200},
		{Id: "b1", Group: "b", Body: "other", Significance: 1, TimeStamp: 300},
	} {
		if _, err := s.db.CreateMemory(ctx, m); err != nil {
			t.Fatalf("CreateMemory(%s): %s", m.Id, err)
		}
	}

	if err := s.db.LinkMemories(ctx, "a1", []types.Link{{Id: "a2", Significance: 1}, {Id: "b1", Significance: 1}}); err != nil {
		t.Fatalf("LinkMemories: %s", err)
	}

	res, err := s.GetMemories(scopedContext("a"), &contract.GetMemoriesRequest{Ids: []string{"a1"}, Links: true})
	if err != nil {
		t.Fatalf("GetMemories: %s", err)
	}

	if len(res.GetMemories()) != 1 {
		t.Fatalf("expected a1 alone, got %d memories", len(res.GetMemories()))
	}

	var farEnds []string

	for _, link := range res.GetMemories()[0].GetLinks() {
		farEnds = append(farEnds, link.GetId())
	}

	if !reflect.DeepEqual(farEnds, []string{"a2"}) {
		t.Errorf("a scoped caller must see only the in-scope far end, got %v", farEnds)
	}
}

// failFilteredCountsStore makes the scoped pre-flight's counts fail, so the walk must fall back to
// its in-walk guard exactly as an unscoped walk does when its own counts fail.
type failFilteredCountsStore struct {
	db.Store
}

func (failFilteredCountsStore) CountMemoriesFiltered(context.Context, db.MemoryFilter) (int, error) {
	return 0, errors.New("count failed")
}

// TestScopedManifestPreflightCountsThePartition runs the scoped branch of the manifest pre-flight,
// which no test reached: a scoped caller is measured against its own partition, so a small partition
// in a large store passes a cap the whole store would exceed, a large partition is refused before
// any work with a figure the caller can act on, and a failed count falls through to the in-walk
// guard rather than letting the walk run uncapped.
func TestScopedManifestPreflightCountsThePartition(t *testing.T) {
	t.Parallel()

	s := newTestServer(t)
	ctx := context.Background()

	for i, group := range []string{"a", "b", "b", "b", "b", "b", "b"} {
		m := types.Memory{Id: group + string(rune('0'+i)), Group: group, Body: "x", Significance: 1, TimeStamp: int64(100 + i)}

		if _, err := s.db.CreateMemory(ctx, m); err != nil {
			t.Fatalf("CreateMemory: %s", err)
		}
	}

	s.transfer.maxManifestRows = 3

	noop := func([]types.Event) error { return nil }
	noopMem := func([]types.Memory) error { return nil }

	if _, _, _, err := s.walkStore(ctx, nil, noop, noopMem); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("an unscoped walk of 7 records under a cap of 3 must be refused, got %v", err)
	}

	if _, _, memories, err := s.walkStore(scopedContext("a"), nil, noop, noopMem); err != nil || memories != 1 {
		t.Errorf("group a holds 1 record and must pass the cap: memories=%d err=%v", memories, err)
	}

	_, _, _, err := s.walkStore(scopedContext("b"), nil, noop, noopMem)
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "store holds 6 records") {
		t.Errorf("group b's 6 records must be refused by the pre-flight, reporting the partition's size: %v", err)
	}

	s.db = failFilteredCountsStore{Store: s.db}

	if _, _, _, err := s.walkStore(scopedContext("b"), nil, noop, noopMem); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("with the pre-flight count failing, the in-walk guard must still refuse: %v", err)
	}
}
