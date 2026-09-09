package hippocampus

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/fastbean-au/hippocampus/contract"
	"github.com/fastbean-au/hippocampus/db"
	"github.com/fastbean-au/hippocampus/types"
)

// The listing RPCs' bounds and their storage-failure arms. What each of these covers is a branch
// that only fires on a page shape or a store failure the happy-path tests never produce - the
// clamps that stop a caller asking for an unbounded page, and the second pass a short page is meant
// to avoid.

// listingFaultStore forces any one of the listing-adjacent storage calls to fail. Each is a
// separate field rather than a single "fail the next call", because several of these RPCs make
// three or four calls and a blanket failure would only ever reach the first.
type listingFaultStore struct {
	db.Store

	significanceLevelsErr error
	countLevelsErr        error
	countEventsErr        error
	missingEventIdsErr    error
	linkedEventIdsErr     error
	countByEventIdsErr    error
	memoriesByEventIdsErr error
}

func (l *listingFaultStore) SignificanceLevels(ctx context.Context, filter db.SignificanceLevelFilter) ([]int32, error) {
	if l.significanceLevelsErr != nil {

		return nil, l.significanceLevelsErr
	}

	return l.Store.SignificanceLevels(ctx, filter)
}

func (l *listingFaultStore) CountSignificanceLevels(ctx context.Context, filter db.SignificanceLevelFilter) (int, error) {
	if l.countLevelsErr != nil {

		return 0, l.countLevelsErr
	}

	return l.Store.CountSignificanceLevels(ctx, filter)
}

func (l *listingFaultStore) CountEventsFiltered(ctx context.Context, filter db.EventFilter) (int, error) {
	if l.countEventsErr != nil {

		return 0, l.countEventsErr
	}

	return l.Store.CountEventsFiltered(ctx, filter)
}

func (l *listingFaultStore) MissingEventIds(ctx context.Context, ids []string) ([]string, error) {
	if l.missingEventIdsErr != nil {

		return nil, l.missingEventIdsErr
	}

	return l.Store.MissingEventIds(ctx, ids)
}

func (l *listingFaultStore) LinkedEventIds(ctx context.Context, ids []string) ([]string, error) {
	if l.linkedEventIdsErr != nil {

		return nil, l.linkedEventIdsErr
	}

	return l.Store.LinkedEventIds(ctx, ids)
}

func (l *listingFaultStore) CountMemoriesByEventIds(ctx context.Context, ids []string, groups []string) (map[string]int, error) {
	if l.countByEventIdsErr != nil {

		return nil, l.countByEventIdsErr
	}

	return l.Store.CountMemoriesByEventIds(ctx, ids, groups)
}

func (l *listingFaultStore) GetMemoriesByEventIds(ctx context.Context, ids []string) (*[]types.Memory, error) {
	if l.memoriesByEventIdsErr != nil {

		return nil, l.memoriesByEventIdsErr
	}

	return l.Store.GetMemoriesByEventIds(ctx, ids)
}

// withListingFaults re-points a seeded server's store at a fault injector, leaving everything the
// seed created readable through it.
func withListingFaults(s *Server, faults *listingFaultStore) *Server {
	faults.Store = s.db
	s.db = faults

	return s
}

// TestGetSignificanceLevelsBoundsItsPage covers the two clamps. Both matter for the same reason the
// registry is paginated at all: it holds one row per distinct significance value, which nothing in
// the service bounds, so a caller asking for everything must be given a page rather than the store.
func TestGetSignificanceLevelsBoundsItsPage(t *testing.T) {
	s := seedEventRPCs(t)
	ctx := context.Background()

	// Above the cap. The clamp is not observable in the response - three values fit in any page -
	// so what is asserted is that the request is served rather than refused or run unbounded.
	res, err := s.GetSignificanceLevels(ctx, &contract.GetSignificanceLevelsRequest{
		Limit: maxSignificanceLevelPageSize * 10,
	})
	if err != nil {
		t.Fatalf("GetSignificanceLevels(over the cap): %s", err)
	}

	if len(res.GetSignificances()) != 3 {
		t.Errorf("significances = %v, want all 3", res.GetSignificances())
	}

	// A negative offset is treated as the start rather than refused, matching how the listings read
	// their own bounds: a nonsensical page position is a client mistake with an obvious right
	// answer, and rejecting it buys nothing.
	negative, err := s.GetSignificanceLevels(ctx, &contract.GetSignificanceLevelsRequest{Offset: -5})
	if err != nil {
		t.Fatalf("GetSignificanceLevels(negative offset): %s", err)
	}

	if len(negative.GetSignificances()) != 3 {
		t.Errorf("significances = %v, want all 3 from the start", negative.GetSignificances())
	}
}

// TestGetSignificanceLevelsSurfacesStorageFailures covers the two storage calls. The second is only
// reachable on a page that ran off its own end, which is exactly the shape the total-count
// short-circuit exists to avoid paying for - so nothing on the happy path reaches it.
func TestGetSignificanceLevelsSurfacesStorageFailures(t *testing.T) {
	ctx := context.Background()

	t.Run("the page read", func(t *testing.T) {
		s := withListingFaults(seedEventRPCs(t), &listingFaultStore{significanceLevelsErr: errors.New("boom")})

		if _, err := s.GetSignificanceLevels(ctx, &contract.GetSignificanceLevelsRequest{}); err == nil {
			t.Error("expected the page read's failure to surface")
		}
	})

	t.Run("the total count", func(t *testing.T) {
		s := withListingFaults(seedEventRPCs(t), &listingFaultStore{countLevelsErr: errors.New("boom")})

		// A full page is what sends the RPC for a real count rather than deriving one.
		if _, err := s.GetSignificanceLevels(ctx, &contract.GetSignificanceLevelsRequest{Limit: 1}); err == nil {
			t.Error("expected the count's failure to surface")
		}
	})
}

// TestCountEventsUsesTheCache covers both arms of the count cache: the miss that populates it and
// the hit that answers without a second unbounded pass. The cache is why a listing can report an
// exact total at all - the count is a full scan, and a page view asking for one per request is what
// item 25.9 was about.
func TestCountEventsUsesTheCache(t *testing.T) {
	s := seedEventRPCs(t)
	s.listingCounts = newCountCache(time.Minute)

	ctx := context.Background()
	faults := &listingFaultStore{}
	s = withListingFaults(s, faults)

	// A full page, so the total comes from the count rather than from the page's own length.
	first, err := s.GetEvents(ctx, &contract.GetEventsRequest{Limit: 1})
	if err != nil {
		t.Fatalf("GetEvents: %s", err)
	}

	if first.GetTotalCount() != 3 {
		t.Fatalf("total_count = %d, want 3", first.GetTotalCount())
	}

	// The count is now cached, so the same listing must not touch the store's counter again - which
	// is asserted by making that call fail.
	faults.countEventsErr = errors.New("boom")

	second, err := s.GetEvents(ctx, &contract.GetEventsRequest{Limit: 1})
	if err != nil {
		t.Fatalf("GetEvents (cached): %s", err)
	}

	if second.GetTotalCount() != 3 {
		t.Errorf("total_count = %d from the cache, want 3", second.GetTotalCount())
	}
}

// TestGetEventsSurfacesStorageFailures walks the four storage calls a fully-loaded listing makes
// after the page itself. Each returns rather than serving a partial answer, which is the point: a
// listing that dropped its memory counts on a failure would be indistinguishable from one whose
// events genuinely hold nothing.
func TestGetEventsSurfacesStorageFailures(t *testing.T) {
	ctx := context.Background()

	cases := []struct {
		name    string
		faults  *listingFaultStore
		request *contract.GetEventsRequest
	}{
		{
			name:    "the count on a full page",
			faults:  &listingFaultStore{countEventsErr: errors.New("boom")},
			request: &contract.GetEventsRequest{Limit: 1},
		},
		{
			name:    "the linked_to anchor check",
			faults:  &listingFaultStore{missingEventIdsErr: errors.New("boom")},
			request: &contract.GetEventsRequest{LinkedTo: "e-open"},
		},
		{
			name:    "the linked_to neighbour lookup",
			faults:  &listingFaultStore{linkedEventIdsErr: errors.New("boom")},
			request: &contract.GetEventsRequest{LinkedTo: "e-open"},
		},
		{
			name:    "the memory counts",
			faults:  &listingFaultStore{countByEventIdsErr: errors.New("boom")},
			request: &contract.GetEventsRequest{MemoryCounts: true},
		},
		{
			name:    "the nested memories",
			faults:  &listingFaultStore{memoriesByEventIdsErr: errors.New("boom")},
			request: &contract.GetEventsRequest{Memories: true},
		},
	}

	for _, v := range cases {
		t.Run(v.name, func(t *testing.T) {
			s := withListingFaults(seedEventRPCs(t), v.faults)
			s.listingCounts = newCountCache(time.Minute)

			if _, err := s.GetEvents(ctx, v.request); err == nil {
				t.Error("expected the storage failure to surface")
			}
		})
	}
}

// TestGetEventsCountsAShortPageFromItsOffset covers the third arm of the total-count switch: a page
// that is neither full nor empty at a positive offset already knows the total exactly, and paying
// for a count there would be a full scan for an answer in hand.
func TestGetEventsCountsAShortPageFromItsOffset(t *testing.T) {
	s := seedEventRPCs(t)
	s.listingCounts = newCountCache(time.Minute)

	ctx := context.Background()
	faults := &listingFaultStore{countEventsErr: errors.New("the count must not be reached")}
	s = withListingFaults(s, faults)

	res, err := s.GetEvents(ctx, &contract.GetEventsRequest{Limit: 10, Offset: 1})
	if err != nil {
		t.Fatalf("GetEvents: %s", err)
	}

	if res.GetTotalCount() != 3 {
		t.Errorf("total_count = %d, want 3 derived from the offset and the page", res.GetTotalCount())
	}
}

// TestGetEventByIdSurfacesTheMemoryCountFailure covers the single-event counterpart, which reaches
// the same store call by a different route.
func TestGetEventByIdSurfacesTheMemoryCountFailure(t *testing.T) {
	s := withListingFaults(seedEventRPCs(t), &listingFaultStore{countByEventIdsErr: errors.New("boom")})

	_, err := s.GetEventById(context.Background(), &contract.GetEventByIdRequest{Id: "e-open", MemoryCounts: true})
	if err == nil {
		t.Error("expected the memory count's failure to surface")
	}
}

// TestStoreEventRejectsAnInvalidEvent covers the validation arm, which also counts a rejection
// against the reason attribute - the one that separates a client sending nonsense from a store
// refusing a write.
func TestStoreEventRejectsAnInvalidEvent(t *testing.T) {
	s := newTestServer(t)

	// A negative significance: ranks are non-negative by design, 0 already meaning unranked.
	res, err := s.StoreEvent(context.Background(), &contract.Event{Name: "x", TimeStart: 1, Significance: -1})
	if err == nil {
		t.Fatal("expected an invalid event to be refused")
	}

	if res.GetId() != "" {
		t.Errorf("a refused event must not report a stored id, got %q", res.GetId())
	}
}

// TestUpdateEventRejectsAnInvalidPlacement covers the placement arm's InvalidArgument mapping. It is
// worth pinning separately from the generic error mapping because a placement naming an anchor that
// does not exist is a client mistake, and mapping it to Internal would send a caller looking for a
// server fault.
func TestUpdateEventRejectsAnInvalidPlacement(t *testing.T) {
	s := seedEventRPCs(t)

	_, err := s.UpdateEvent(context.Background(), &contract.Event{
		Id: "e-open",
		Placement: &contract.SignificancePlacement{
			Mode:     contract.SignificancePlacement_ABOVE,
			AnchorId: "no-such-event",
		},
	})

	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("UpdateEvent(unknown anchor) = %v, want InvalidArgument", err)
	}
}
