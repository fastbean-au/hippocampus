package hippocampus

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/fastbean-au/hippocampus/contract"
	"github.com/fastbean-au/hippocampus/db"
	"github.com/fastbean-au/hippocampus/types"
)

// errLink is the failure the link stores below inject. It is deliberately a plain error, not a
// status, so the assertions confirm mapError turned it into Internal rather than passing a status
// straight through.
var errLink = errors.New("link store failure")

// missingErrStore fails the existence check every link write and read runs first.
type missingErrStore struct {
	db.Store
}

func (missingErrStore) MissingMemoryIds(ctx context.Context, ids []string) ([]string, error) {
	return nil, errLink
}

func (missingErrStore) MissingEventIds(ctx context.Context, ids []string) ([]string, error) {
	return nil, errLink
}

// getLinksErrStore fails the read-back of an item's existing links, which is both the capacity
// check's input and what readLinks returns.
type getLinksErrStore struct {
	db.Store
}

func (getLinksErrStore) GetMemoryLinks(
	ctx context.Context,
	id string,
	direction types.LinkDirection,
) ([]types.LinkEdge, int64, error) {
	return nil, 0, errLink
}

func (getLinksErrStore) GetEventLinks(
	ctx context.Context,
	id string,
	direction types.LinkDirection,
) ([]types.LinkEdge, int64, error) {
	return nil, 0, errLink
}

// writeLinksErrStore fails the write itself, after validation, existence and capacity have passed.
type writeLinksErrStore struct {
	db.Store
}

func (writeLinksErrStore) LinkMemories(ctx context.Context, id string, links []types.Link) error {
	return errLink
}

func (writeLinksErrStore) LinkEvents(ctx context.Context, id string, links []types.Link) error {
	return errLink
}

func (writeLinksErrStore) UnlinkMemories(ctx context.Context, id string, targets []string) error {
	return errLink
}

func (writeLinksErrStore) UnlinkEvents(ctx context.Context, id string, targets []string) error {
	return errLink
}

// linksForErrStore fails the bulk link read the attach helpers use, so their best-effort contract
// can be checked: the caller's memories or events must still come back, just without links.
type linksForErrStore struct {
	db.Store
}

func (linksForErrStore) LinksForMemories(ctx context.Context, ids []string) (map[string][]types.Link, error) {
	return nil, errLink
}

func (linksForErrStore) LinksForEvents(ctx context.Context, ids []string) (map[string][]types.Link, error) {
	return nil, errLink
}

// linkedIdsErrStore fails the one-hop neighbour lookup shared by spreading activation and
// associative retrieval.
type linkedIdsErrStore struct {
	db.Store
}

func (linkedIdsErrStore) LinkedMemoryIds(ctx context.Context, ids []string) ([]string, error) {
	return nil, errLink
}

// reinforceErrStore lets the neighbour lookup succeed and fails the reinforcement that follows.
type reinforceErrStore struct {
	db.Store
}

func (reinforceErrStore) ReinforceLinkedMemories(ctx context.Context, ids []string, fraction float64) error {
	return errLink
}

// getByIdsErrStore lets the neighbour lookup succeed and fails the read of those neighbours.
type getByIdsErrStore struct {
	db.Store
}

func (getByIdsErrStore) GetMemoriesByIds(ctx context.Context, ids []string) (*[]types.Memory, error) {
	return nil, errLink
}

// seedLinkMemories creates the named memories directly in the store, bypassing the RPC layer so a
// test can set up a graph without depending on the code under test.
func seedLinkMemories(t *testing.T, s *Server, ids ...string) {
	t.Helper()

	for _, id := range ids {
		memory := types.Memory{Id: id, TimeStamp: 100, Significance: 1, Body: "body"}

		if _, err := s.db.CreateMemory(context.Background(), memory); err != nil {
			t.Fatalf("CreateMemory(%s): %s", id, err)
		}
	}
}

// seedLinkEvents is seedLinkMemories for the event half of the graph.
func seedLinkEvents(t *testing.T, s *Server, ids ...string) {
	t.Helper()

	for _, id := range ids {
		event := types.Event{Id: id, Name: id, TimeStart: 100, Significance: 1}

		if _, err := s.db.CreateEvent(context.Background(), event); err != nil {
			t.Fatalf("CreateEvent(%s): %s", id, err)
		}
	}
}

// TestLinkMemories_RPC covers the happy path end to end: the link is written, and reading it back
// reports the far end, its weight, the outbound direction, and the summed significance.
func TestLinkMemories_RPC(t *testing.T) {
	s := newTestServer(t)
	seedLinkMemories(t, s, "m1", "m2")

	req := &contract.LinkMemoriesRequest{
		Id:    "m1",
		Links: []*contract.Link{{Id: "m2", Significance: 7}},
	}

	res, err := s.LinkMemories(context.Background(), req)
	if err != nil {
		t.Fatalf("LinkMemories: %s", err)
	}

	if !res.GetOk() {
		t.Error("expected Ok: true from a successful link")
	}

	read, err := s.GetMemoryLinks(context.Background(), &contract.GetMemoryLinksRequest{Id: "m1"})
	if err != nil {
		t.Fatalf("GetMemoryLinks: %s", err)
	}

	if len(read.GetLinks()) != 1 {
		t.Fatalf("expected 1 link, got %d", len(read.GetLinks()))
	}

	edge := read.GetLinks()[0]
	if edge.GetId() != "m2" || edge.GetSignificance() != 7 {
		t.Errorf("unexpected edge: %+v", edge)
	}

	if edge.GetDirection() != contract.LinkDirection_LINK_DIRECTION_OUTBOUND {
		t.Errorf("expected an outbound edge, got %s", edge.GetDirection())
	}

	if read.GetLinkSignificance() != 7 {
		t.Errorf("expected a summed link significance of 7, got %d", read.GetLinkSignificance())
	}
}

// TestLinkEvents_RPC is TestLinkMemories_RPC for events, confirming the shared implementation is
// wired to the event store methods rather than the memory ones.
func TestLinkEvents_RPC(t *testing.T) {
	s := newTestServer(t)
	seedLinkEvents(t, s, "e1", "e2")

	req := &contract.LinkEventsRequest{
		Id:    "e1",
		Links: []*contract.Link{{Id: "e2", Significance: 3}},
	}

	if _, err := s.LinkEvents(context.Background(), req); err != nil {
		t.Fatalf("LinkEvents: %s", err)
	}

	read, err := s.GetEventLinks(context.Background(), &contract.GetEventLinksRequest{Id: "e1"})
	if err != nil {
		t.Fatalf("GetEventLinks: %s", err)
	}

	if len(read.GetLinks()) != 1 || read.GetLinks()[0].GetId() != "e2" {
		t.Fatalf("unexpected event links: %+v", read.GetLinks())
	}

	if read.GetLinkSignificance() != 3 {
		t.Errorf("expected a summed link significance of 3, got %d", read.GetLinkSignificance())
	}
}

// TestLinkMemories_Rejections covers every guard createLinks applies before it writes, in the order
// it applies them.
func TestLinkMemories_Rejections(t *testing.T) {
	tests := []struct {
		name    string
		id      string
		links   []*contract.Link
		code    codes.Code
		message string
	}{
		{
			name:    "no id",
			id:      "",
			links:   []*contract.Link{{Id: "m2", Significance: 1}},
			code:    codes.InvalidArgument,
			message: "memory id must be provided",
		},
		{
			name:    "no links",
			id:      "m1",
			links:   nil,
			code:    codes.InvalidArgument,
			message: "no links provided",
		},
		{
			name:    "self link",
			id:      "m1",
			links:   []*contract.Link{{Id: "m1", Significance: 1}},
			code:    codes.InvalidArgument,
			message: "links memory to itself",
		},
		{
			name:    "unknown target",
			id:      "m1",
			links:   []*contract.Link{{Id: "nope", Significance: 1}},
			code:    codes.NotFound,
			message: "no such memory: nope",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s := newTestServer(t)
			seedLinkMemories(t, s, "m1", "m2")

			req := &contract.LinkMemoriesRequest{Id: test.id, Links: test.links}

			res, err := s.LinkMemories(context.Background(), req)
			if err == nil {
				t.Fatal("expected the request to be rejected")
			}

			if got := status.Code(err); got != test.code {
				t.Errorf("expected %s, got %s (%v)", test.code, got, err)
			}

			if !strings.Contains(status.Convert(err).Message(), test.message) {
				t.Errorf("expected the message to mention %q, got %q", test.message, status.Convert(err).Message())
			}

			if res.GetOk() {
				t.Error("expected Ok: false on a rejected request")
			}
		})
	}
}

// TestLinkMemories_UnknownIdsAreReportedTogether pins the reason the near end and every far end are
// checked in one call: a request naming several unknown ids reports all of them, rather than making
// the caller discover them one round trip at a time.
func TestLinkMemories_UnknownIdsAreReportedTogether(t *testing.T) {
	s := newTestServer(t)
	seedLinkMemories(t, s, "m1")

	req := &contract.LinkMemoriesRequest{
		Id: "m1",
		Links: []*contract.Link{
			{Id: "ghost-a", Significance: 1},
			{Id: "ghost-b", Significance: 1},
		},
	}

	_, err := s.LinkMemories(context.Background(), req)
	if err == nil {
		t.Fatal("expected unknown targets to be rejected")
	}

	message := status.Convert(err).Message()
	if !strings.Contains(message, "ghost-a") || !strings.Contains(message, "ghost-b") {
		t.Errorf("expected both unknown ids in the message, got %q", message)
	}
}

// TestLinkEvents_NoIdRejected covers the event half of createLinks' first guard, which names the
// kind in its message.
func TestLinkEvents_NoIdRejected(t *testing.T) {
	s := newTestServer(t)

	req := &contract.LinkEventsRequest{Links: []*contract.Link{{Id: "e2", Significance: 1}}}

	_, err := s.LinkEvents(context.Background(), req)
	if err == nil {
		t.Fatal("expected a missing event id to be rejected")
	}

	if got := status.Convert(err).Message(); !strings.Contains(got, "event id must be provided") {
		t.Errorf("expected the message to name the event kind, got %q", got)
	}
}

// TestLinkMemories_CapacityIsCheckedAgainstTheTotalHeld pins the reason the cap is applied to what
// the item would end up holding rather than to the request alone: a caller adding a few links at a
// time must not be able to walk past the bound indefinitely.
func TestLinkMemories_CapacityIsCheckedAgainstTheTotalHeld(t *testing.T) {
	s := newTestServer(t)

	ids := make([]string, 0, types.MaxLinks+2)
	ids = append(ids, "hub")

	for i := 0; i < types.MaxLinks+1; i++ {
		ids = append(ids, "t"+strconv.Itoa(i))
	}

	seedLinkMemories(t, s, ids...)

	// Fill the hub to exactly the cap in one request, which must be accepted.
	full := make([]*contract.Link, 0, types.MaxLinks)
	for i := 0; i < types.MaxLinks; i++ {
		full = append(full, &contract.Link{Id: "t" + strconv.Itoa(i), Significance: 1})
	}

	if _, err := s.LinkMemories(context.Background(), &contract.LinkMemoriesRequest{Id: "hub", Links: full}); err != nil {
		t.Fatalf("expected exactly MaxLinks to be accepted: %s", err)
	}

	// One more, in a separate request, takes the total past the cap.
	over := &contract.LinkMemoriesRequest{
		Id:    "hub",
		Links: []*contract.Link{{Id: "t" + strconv.Itoa(types.MaxLinks), Significance: 1}},
	}

	_, err := s.LinkMemories(context.Background(), over)
	if err == nil {
		t.Fatal("expected the link that would exceed the cap to be rejected")
	}

	if got := status.Code(err); got != codes.InvalidArgument {
		t.Errorf("expected codes.InvalidArgument, got %s (%v)", got, err)
	}

	if got := status.Convert(err).Message(); !strings.Contains(got, "exceeding the maximum") {
		t.Errorf("expected the message to name the cap, got %q", got)
	}
}

// TestLinkMemories_RelinkingAtCapacityIsAnUpdate pins the other half of the capacity rule:
// re-weighting a link an item already holds must not count as an addition, or an item at the cap
// could never have its existing links adjusted.
func TestLinkMemories_RelinkingAtCapacityIsAnUpdate(t *testing.T) {
	s := newTestServer(t)

	ids := make([]string, 0, types.MaxLinks+1)
	ids = append(ids, "hub")

	for i := 0; i < types.MaxLinks; i++ {
		ids = append(ids, "t"+strconv.Itoa(i))
	}

	seedLinkMemories(t, s, ids...)

	full := make([]*contract.Link, 0, types.MaxLinks)
	for i := 0; i < types.MaxLinks; i++ {
		full = append(full, &contract.Link{Id: "t" + strconv.Itoa(i), Significance: 1})
	}

	if _, err := s.LinkMemories(context.Background(), &contract.LinkMemoriesRequest{Id: "hub", Links: full}); err != nil {
		t.Fatalf("filling to the cap: %s", err)
	}

	relink := &contract.LinkMemoriesRequest{
		Id:    "hub",
		Links: []*contract.Link{{Id: "t0", Significance: 99}},
	}

	if _, err := s.LinkMemories(context.Background(), relink); err != nil {
		t.Fatalf("expected re-weighting an existing link at the cap to be accepted: %s", err)
	}

	read, err := s.GetMemoryLinks(context.Background(), &contract.GetMemoryLinksRequest{Id: "hub"})
	if err != nil {
		t.Fatalf("GetMemoryLinks: %s", err)
	}

	if len(read.GetLinks()) != types.MaxLinks {
		t.Errorf("expected the link count to stay at the cap, got %d", len(read.GetLinks()))
	}

	for _, e := range read.GetLinks() {
		if e.GetId() != "t0" {
			continue
		}

		if e.GetSignificance() != 99 {
			t.Errorf("expected the re-link to re-weight t0 to 99, got %d", e.GetSignificance())
		}

		break
	}
}

// TestLinkMemories_StoreFailuresMapToInternal walks the three store calls createLinks makes and
// confirms each failure reaches the caller as Internal rather than as a raw driver error.
func TestLinkMemories_StoreFailuresMapToInternal(t *testing.T) {
	tests := []struct {
		name  string
		wrap  func(db.Store) db.Store
		event bool
	}{
		{
			name: "existence check fails",
			wrap: func(inner db.Store) db.Store { return missingErrStore{Store: inner} },
		},
		{
			name: "capacity read fails",
			wrap: func(inner db.Store) db.Store { return getLinksErrStore{Store: inner} },
		},
		{
			name: "write fails",
			wrap: func(inner db.Store) db.Store { return writeLinksErrStore{Store: inner} },
		},
		{
			name:  "event existence check fails",
			wrap:  func(inner db.Store) db.Store { return missingErrStore{Store: inner} },
			event: true,
		},
		{
			name:  "event capacity read fails",
			wrap:  func(inner db.Store) db.Store { return getLinksErrStore{Store: inner} },
			event: true,
		},
		{
			name:  "event write fails",
			wrap:  func(inner db.Store) db.Store { return writeLinksErrStore{Store: inner} },
			event: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s := newTestServer(t)
			seedLinkMemories(t, s, "m1", "m2")
			seedLinkEvents(t, s, "e1", "e2")
			s.db = test.wrap(s.db)

			var err error

			if test.event {
				req := &contract.LinkEventsRequest{Id: "e1", Links: []*contract.Link{{Id: "e2", Significance: 1}}}
				_, err = s.LinkEvents(context.Background(), req)
			} else {
				req := &contract.LinkMemoriesRequest{Id: "m1", Links: []*contract.Link{{Id: "m2", Significance: 1}}}
				_, err = s.LinkMemories(context.Background(), req)
			}

			if err == nil {
				t.Fatal("expected the store failure to reach the caller")
			}

			if got := status.Code(err); got != codes.Internal {
				t.Errorf("expected codes.Internal, got %s (%v)", got, err)
			}
		})
	}
}

// TestUnlinkMemories_RPC covers the happy path, including the deduplication of repeated targets.
func TestUnlinkMemories_RPC(t *testing.T) {
	s := newTestServer(t)
	seedLinkMemories(t, s, "m1", "m2", "m3")

	req := &contract.LinkMemoriesRequest{
		Id: "m1",
		Links: []*contract.Link{
			{Id: "m2", Significance: 1},
			{Id: "m3", Significance: 1},
		},
	}

	if _, err := s.LinkMemories(context.Background(), req); err != nil {
		t.Fatalf("LinkMemories: %s", err)
	}

	// The repeated id exercises DedupeLinkIds on the way through.
	unlink := &contract.UnlinkMemoriesRequest{Id: "m1", Ids: []string{"m2", "m2"}}

	res, err := s.UnlinkMemories(context.Background(), unlink)
	if err != nil {
		t.Fatalf("UnlinkMemories: %s", err)
	}

	if !res.GetOk() {
		t.Error("expected Ok: true from a successful unlink")
	}

	read, err := s.GetMemoryLinks(context.Background(), &contract.GetMemoryLinksRequest{Id: "m1"})
	if err != nil {
		t.Fatalf("GetMemoryLinks: %s", err)
	}

	if len(read.GetLinks()) != 1 || read.GetLinks()[0].GetId() != "m3" {
		t.Errorf("expected only the link to m3 to survive, got %+v", read.GetLinks())
	}
}

// TestUnlinkMemories_UnknownTargetIsNotAnError pins the deliberate asymmetry with linking: only the
// near end has to exist, because an unknown target is simply a link that is not there — which is the
// state the caller asked for.
func TestUnlinkMemories_UnknownTargetIsNotAnError(t *testing.T) {
	s := newTestServer(t)
	seedLinkMemories(t, s, "m1")

	req := &contract.UnlinkMemoriesRequest{Id: "m1", Ids: []string{"never-existed"}}

	res, err := s.UnlinkMemories(context.Background(), req)
	if err != nil {
		t.Fatalf("expected unlinking an unknown target to succeed: %s", err)
	}

	if !res.GetOk() {
		t.Error("expected Ok: true when the requested state already holds")
	}
}

// TestUnlinkEvents_RPC covers the event half of removeLinks.
func TestUnlinkEvents_RPC(t *testing.T) {
	s := newTestServer(t)
	seedLinkEvents(t, s, "e1", "e2")

	link := &contract.LinkEventsRequest{Id: "e1", Links: []*contract.Link{{Id: "e2", Significance: 4}}}
	if _, err := s.LinkEvents(context.Background(), link); err != nil {
		t.Fatalf("LinkEvents: %s", err)
	}

	if _, err := s.UnlinkEvents(context.Background(), &contract.UnlinkEventsRequest{Id: "e1", Ids: []string{"e2"}}); err != nil {
		t.Fatalf("UnlinkEvents: %s", err)
	}

	read, err := s.GetEventLinks(context.Background(), &contract.GetEventLinksRequest{Id: "e1"})
	if err != nil {
		t.Fatalf("GetEventLinks: %s", err)
	}

	if len(read.GetLinks()) != 0 {
		t.Errorf("expected the event link to be removed, got %+v", read.GetLinks())
	}
}

// TestUnlinkMemories_Rejections covers removeLinks' guards.
func TestUnlinkMemories_Rejections(t *testing.T) {
	tests := []struct {
		name    string
		id      string
		ids     []string
		code    codes.Code
		message string
	}{
		{
			name:    "no id",
			ids:     []string{"m2"},
			code:    codes.InvalidArgument,
			message: "memory id must be provided",
		},
		{
			name:    "no targets",
			id:      "m1",
			code:    codes.InvalidArgument,
			message: "no ids provided",
		},
		{
			name:    "unknown near end",
			id:      "ghost",
			ids:     []string{"m2"},
			code:    codes.NotFound,
			message: "no such memory: ghost",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s := newTestServer(t)
			seedLinkMemories(t, s, "m1", "m2")

			_, err := s.UnlinkMemories(context.Background(), &contract.UnlinkMemoriesRequest{Id: test.id, Ids: test.ids})
			if err == nil {
				t.Fatal("expected the request to be rejected")
			}

			if got := status.Code(err); got != test.code {
				t.Errorf("expected %s, got %s (%v)", test.code, got, err)
			}

			if !strings.Contains(status.Convert(err).Message(), test.message) {
				t.Errorf("expected the message to mention %q, got %q", test.message, status.Convert(err).Message())
			}
		})
	}
}

// TestUnlinkEvents_NoIdRejected covers the event half of removeLinks' first guard.
func TestUnlinkEvents_NoIdRejected(t *testing.T) {
	s := newTestServer(t)

	_, err := s.UnlinkEvents(context.Background(), &contract.UnlinkEventsRequest{Ids: []string{"e2"}})
	if err == nil {
		t.Fatal("expected a missing event id to be rejected")
	}

	if got := status.Convert(err).Message(); !strings.Contains(got, "event id must be provided") {
		t.Errorf("expected the message to name the event kind, got %q", got)
	}
}

// TestUnlinkMemories_StoreFailuresMapToInternal walks removeLinks' two store calls.
func TestUnlinkMemories_StoreFailuresMapToInternal(t *testing.T) {
	tests := []struct {
		name string
		wrap func(db.Store) db.Store
	}{
		{
			name: "existence check fails",
			wrap: func(inner db.Store) db.Store { return missingErrStore{Store: inner} },
		},
		{
			name: "unlink fails",
			wrap: func(inner db.Store) db.Store { return writeLinksErrStore{Store: inner} },
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s := newTestServer(t)
			seedLinkMemories(t, s, "m1", "m2")
			s.db = test.wrap(s.db)

			_, err := s.UnlinkMemories(context.Background(), &contract.UnlinkMemoriesRequest{Id: "m1", Ids: []string{"m2"}})
			if err == nil {
				t.Fatal("expected the store failure to reach the caller")
			}

			if got := status.Code(err); got != codes.Internal {
				t.Errorf("expected codes.Internal, got %s (%v)", got, err)
			}
		})
	}
}

// TestGetMemoryLinks_Direction confirms the requested direction reaches the store, and that an
// inbound read reports the edge as inbound relative to the item asked about.
func TestGetMemoryLinks_Direction(t *testing.T) {
	s := newTestServer(t)
	seedLinkMemories(t, s, "m1", "m2")

	link := &contract.LinkMemoriesRequest{Id: "m1", Links: []*contract.Link{{Id: "m2", Significance: 5}}}
	if _, err := s.LinkMemories(context.Background(), link); err != nil {
		t.Fatalf("LinkMemories: %s", err)
	}

	outbound, err := s.GetMemoryLinks(context.Background(), &contract.GetMemoryLinksRequest{
		Id:        "m1",
		Direction: contract.LinkDirection_LINK_DIRECTION_OUTBOUND,
	})
	if err != nil {
		t.Fatalf("GetMemoryLinks(outbound): %s", err)
	}

	if len(outbound.GetLinks()) != 1 {
		t.Fatalf("expected m1 to hold 1 outbound link, got %d", len(outbound.GetLinks()))
	}

	// m2 never declared a link, so it has none outbound but one inbound.
	none, err := s.GetMemoryLinks(context.Background(), &contract.GetMemoryLinksRequest{
		Id:        "m2",
		Direction: contract.LinkDirection_LINK_DIRECTION_OUTBOUND,
	})
	if err != nil {
		t.Fatalf("GetMemoryLinks(m2 outbound): %s", err)
	}

	if len(none.GetLinks()) != 0 {
		t.Errorf("expected m2 to hold no outbound links, got %+v", none.GetLinks())
	}

	inbound, err := s.GetMemoryLinks(context.Background(), &contract.GetMemoryLinksRequest{
		Id:        "m2",
		Direction: contract.LinkDirection_LINK_DIRECTION_INBOUND,
	})
	if err != nil {
		t.Fatalf("GetMemoryLinks(m2 inbound): %s", err)
	}

	if len(inbound.GetLinks()) != 1 || inbound.GetLinks()[0].GetId() != "m1" {
		t.Fatalf("expected m2 to hold 1 inbound link from m1, got %+v", inbound.GetLinks())
	}

	if got := inbound.GetLinks()[0].GetDirection(); got != contract.LinkDirection_LINK_DIRECTION_INBOUND {
		t.Errorf("expected the edge to report as inbound, got %s", got)
	}

	// The value the decay maths reads is symmetric, so m2 carries the weight too.
	if inbound.GetLinkSignificance() != 5 {
		t.Errorf("expected link significance to count on both ends, got %d", inbound.GetLinkSignificance())
	}
}

// TestGetLinks_Rejections covers readLinks' guards on both halves of the graph.
func TestGetLinks_Rejections(t *testing.T) {
	s := newTestServer(t)

	if _, err := s.GetMemoryLinks(context.Background(), &contract.GetMemoryLinksRequest{}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("expected InvalidArgument for a missing memory id, got %v", err)
	}

	if _, err := s.GetEventLinks(context.Background(), &contract.GetEventLinksRequest{}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("expected InvalidArgument for a missing event id, got %v", err)
	}

	_, err := s.GetMemoryLinks(context.Background(), &contract.GetMemoryLinksRequest{Id: "ghost"})
	if status.Code(err) != codes.NotFound {
		t.Errorf("expected NotFound for an unknown memory, got %v", err)
	}

	_, err = s.GetEventLinks(context.Background(), &contract.GetEventLinksRequest{Id: "ghost"})
	if status.Code(err) != codes.NotFound {
		t.Errorf("expected NotFound for an unknown event, got %v", err)
	}
}

// TestGetMemoryLinks_StoreFailuresMapToInternal walks readLinks' two store calls.
func TestGetMemoryLinks_StoreFailuresMapToInternal(t *testing.T) {
	tests := []struct {
		name string
		wrap func(db.Store) db.Store
	}{
		{
			name: "existence check fails",
			wrap: func(inner db.Store) db.Store { return missingErrStore{Store: inner} },
		},
		{
			name: "link read fails",
			wrap: func(inner db.Store) db.Store { return getLinksErrStore{Store: inner} },
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s := newTestServer(t)
			seedLinkMemories(t, s, "m1")
			s.db = test.wrap(s.db)

			_, err := s.GetMemoryLinks(context.Background(), &contract.GetMemoryLinksRequest{Id: "m1"})
			if err == nil {
				t.Fatal("expected the store failure to reach the caller")
			}

			if got := status.Code(err); got != codes.Internal {
				t.Errorf("expected codes.Internal, got %s (%v)", got, err)
			}
		})
	}
}

// TestStoreMemory_WithLinks covers checkLinkTargets and storeLinks on the create path: a memory
// created with links has them written once it exists.
func TestStoreMemory_WithLinks(t *testing.T) {
	s := newTestServer(t)
	seedLinkMemories(t, s, "existing")

	memory := &contract.Memory{
		Id:           "fresh",
		Significance: 5,
		Body:         "body",
		Links:        []*contract.Link{{Id: "existing", Significance: 2}},
	}

	if _, err := s.StoreMemory(context.Background(), memory); err != nil {
		t.Fatalf("StoreMemory: %s", err)
	}

	read, err := s.GetMemoryLinks(context.Background(), &contract.GetMemoryLinksRequest{Id: "fresh"})
	if err != nil {
		t.Fatalf("GetMemoryLinks: %s", err)
	}

	if len(read.GetLinks()) != 1 || read.GetLinks()[0].GetId() != "existing" {
		t.Errorf("expected the create's link to be stored, got %+v", read.GetLinks())
	}
}

// TestStoreMemory_UnknownLinkTargetRejectedBeforeCreate pins checkLinkTargets running before the
// item is written: a bad link set must not leave a half-created memory behind.
func TestStoreMemory_UnknownLinkTargetRejectedBeforeCreate(t *testing.T) {
	s := newTestServer(t)

	memory := &contract.Memory{
		Id:           "fresh",
		Significance: 5,
		Body:         "body",
		Links:        []*contract.Link{{Id: "ghost", Significance: 1}},
	}

	_, err := s.StoreMemory(context.Background(), memory)
	if err == nil {
		t.Fatal("expected an unknown link target to be rejected")
	}

	if got := status.Code(err); got != codes.NotFound {
		t.Errorf("expected codes.NotFound, got %s (%v)", got, err)
	}

	memories, err := s.db.GetMemoriesByIds(context.Background(), []string{"fresh"})
	if err != nil {
		t.Fatalf("GetMemoriesByIds: %s", err)
	}

	if len(*memories) != 0 {
		t.Error("expected no memory to be created when its link set was rejected")
	}
}

// TestStoreMemory_InvalidLinkSetRejected covers checkLinkTargets' validation branch, which runs
// before the existence check.
func TestStoreMemory_InvalidLinkSetRejected(t *testing.T) {
	s := newTestServer(t)
	seedLinkMemories(t, s, "target")

	memory := &contract.Memory{
		Id:           "fresh",
		Significance: 5,
		Body:         "body",
		Links: []*contract.Link{
			{Id: "target", Significance: 1},
			{Id: "target", Significance: 2},
		},
	}

	_, err := s.StoreMemory(context.Background(), memory)
	if err == nil {
		t.Fatal("expected a duplicated link target to be rejected")
	}

	if got := status.Code(err); got != codes.InvalidArgument {
		t.Errorf("expected codes.InvalidArgument, got %s (%v)", got, err)
	}
}

// TestCheckLinkTargets_StoreFailureMapsToInternal covers checkLinkTargets' store-error branch.
func TestCheckLinkTargets_StoreFailureMapsToInternal(t *testing.T) {
	s := newTestServer(t)
	seedLinkMemories(t, s, "target")
	s.db = missingErrStore{Store: s.db}

	links := []types.Link{{Id: "target", Significance: 1}}

	err := s.checkLinkTargets(context.Background(), s.memoryLinks(), "fresh", links)
	if err == nil {
		t.Fatal("expected the existence-check failure to be returned")
	}

	if got := status.Code(err); got != codes.Internal {
		t.Errorf("expected codes.Internal, got %s (%v)", got, err)
	}
}

// TestCheckLinkTargets_NoLinksIsNoOp covers the early return, which must not touch the store at all.
func TestCheckLinkTargets_NoLinksIsNoOp(t *testing.T) {
	s := newTestServer(t)
	s.db = missingErrStore{Store: s.db}

	if err := s.checkLinkTargets(context.Background(), s.memoryLinks(), "fresh", nil); err != nil {
		t.Errorf("expected an empty link set to be accepted without a store call, got %s", err)
	}
}

// TestStoreLinks_BestEffort pins storeLinks' deliberate contract: the item is already stored and
// acknowledged, so a link write that fails is logged and swallowed rather than losing the write.
func TestStoreLinks_BestEffort(t *testing.T) {
	s := newTestServer(t)
	seedLinkMemories(t, s, "m1", "m2")
	s.db = writeLinksErrStore{Store: s.db}

	// No panic, no propagation: the function returns nothing to propagate with.
	s.storeLinks(context.Background(), s.memoryLinks(), "m1", []types.Link{{Id: "m2", Significance: 1}})
}

// TestStoreLinks_NoLinksIsNoOp covers the early return.
func TestStoreLinks_NoLinksIsNoOp(t *testing.T) {
	s := newTestServer(t)
	s.db = writeLinksErrStore{Store: s.db}

	s.storeLinks(context.Background(), s.memoryLinks(), "m1", nil)
}

// TestStoreEvent_WithLinks covers the event create path's use of checkLinkTargets and storeLinks.
func TestStoreEvent_WithLinks(t *testing.T) {
	s := newTestServer(t)
	seedLinkEvents(t, s, "existing")

	event := &contract.Event{
		Id:           "fresh",
		Name:         "fresh",
		TimeStart:    100,
		Significance: 5,
		Links:        []*contract.Link{{Id: "existing", Significance: 2}},
	}

	if _, err := s.StoreEvent(context.Background(), event); err != nil {
		t.Fatalf("StoreEvent: %s", err)
	}

	read, err := s.GetEventLinks(context.Background(), &contract.GetEventLinksRequest{Id: "fresh"})
	if err != nil {
		t.Fatalf("GetEventLinks: %s", err)
	}

	if len(read.GetLinks()) != 1 || read.GetLinks()[0].GetId() != "existing" {
		t.Errorf("expected the create's link to be stored, got %+v", read.GetLinks())
	}
}

// TestStoreEvent_UnknownLinkTargetRejected covers the event half of the create-path existence check.
func TestStoreEvent_UnknownLinkTargetRejected(t *testing.T) {
	s := newTestServer(t)

	event := &contract.Event{
		Id:           "fresh",
		Name:         "fresh",
		TimeStart:    100,
		Significance: 5,
		Links:        []*contract.Link{{Id: "ghost", Significance: 1}},
	}

	_, err := s.StoreEvent(context.Background(), event)
	if err == nil {
		t.Fatal("expected an unknown link target to be rejected")
	}

	if got := status.Code(err); got != codes.NotFound {
		t.Errorf("expected codes.NotFound, got %s (%v)", got, err)
	}
}

// TestAttachMemoryLinks populates a read's memories with their links.
func TestAttachMemoryLinks(t *testing.T) {
	s := newTestServer(t)
	seedLinkMemories(t, s, "m1", "m2")

	link := &contract.LinkMemoriesRequest{Id: "m1", Links: []*contract.Link{{Id: "m2", Significance: 6}}}
	if _, err := s.LinkMemories(context.Background(), link); err != nil {
		t.Fatalf("LinkMemories: %s", err)
	}

	memories := []types.Memory{{Id: "m1"}, {Id: "m2"}}
	s.attachMemoryLinks(context.Background(), memories)

	// Links are outbound only, which is what keeps an export/import round trip from doubling edges.
	if len(memories[0].Links) != 1 || memories[0].Links[0].Id != "m2" {
		t.Errorf("expected m1 to carry its outbound link, got %+v", memories[0].Links)
	}

	if len(memories[1].Links) != 0 {
		t.Errorf("expected m2 to carry no outbound links, got %+v", memories[1].Links)
	}
}

// TestAttachMemoryLinks_BestEffort pins the contract: the memories are the answer, so a link read
// that fails must leave them intact rather than failing the whole read.
func TestAttachMemoryLinks_BestEffort(t *testing.T) {
	s := newTestServer(t)
	s.db = linksForErrStore{Store: s.db}

	memories := []types.Memory{{Id: "m1", Body: "kept"}}
	s.attachMemoryLinks(context.Background(), memories)

	if memories[0].Body != "kept" || memories[0].Links != nil {
		t.Errorf("expected the memory to survive the link failure unlinked, got %+v", memories[0])
	}
}

// TestAttachMemoryLinks_EmptyIsNoOp covers the early return.
func TestAttachMemoryLinks_EmptyIsNoOp(t *testing.T) {
	s := newTestServer(t)
	s.db = linksForErrStore{Store: s.db}

	s.attachMemoryLinks(context.Background(), nil)
}

// TestAttachEventLinks is TestAttachMemoryLinks for the event half, which the archive walk uses so
// an export carries the event graph.
func TestAttachEventLinks(t *testing.T) {
	s := newTestServer(t)
	seedLinkEvents(t, s, "e1", "e2")

	link := &contract.LinkEventsRequest{Id: "e1", Links: []*contract.Link{{Id: "e2", Significance: 6}}}
	if _, err := s.LinkEvents(context.Background(), link); err != nil {
		t.Fatalf("LinkEvents: %s", err)
	}

	events := []types.Event{{Id: "e1"}, {Id: "e2"}}
	s.attachEventLinks(context.Background(), events)

	if len(events[0].Links) != 1 || events[0].Links[0].Id != "e2" {
		t.Errorf("expected e1 to carry its outbound link, got %+v", events[0].Links)
	}

	if len(events[1].Links) != 0 {
		t.Errorf("expected e2 to carry no outbound links, got %+v", events[1].Links)
	}
}

// TestAttachEventLinks_BestEffort covers the event half's failure branch.
func TestAttachEventLinks_BestEffort(t *testing.T) {
	s := newTestServer(t)
	s.db = linksForErrStore{Store: s.db}

	events := []types.Event{{Id: "e1", Name: "kept"}}
	s.attachEventLinks(context.Background(), events)

	if events[0].Name != "kept" || events[0].Links != nil {
		t.Errorf("expected the event to survive the link failure unlinked, got %+v", events[0])
	}
}

// TestAttachEventLinks_EmptyIsNoOp covers the early return.
func TestAttachEventLinks_EmptyIsNoOp(t *testing.T) {
	s := newTestServer(t)
	s.db = linksForErrStore{Store: s.db}

	s.attachEventLinks(context.Background(), nil)
}

// TestReinforceLinked_Disabled pins the default: with linkRecallPropagation at 0 spreading
// activation is off, so the store is never consulted at all.
func TestReinforceLinked_Disabled(t *testing.T) {
	s := newTestServer(t)
	s.db = linkedIdsErrStore{Store: s.db}

	// A store call would fail; the early return means none is made.
	s.reinforceLinked(context.Background(), []string{"m1"})
}

// TestReinforceLinked_NoIds covers the other half of the early return.
func TestReinforceLinked_NoIds(t *testing.T) {
	s := newTestServer(t)
	s.consolidation.linkRecallPropagation = 0.5
	s.db = linkedIdsErrStore{Store: s.db}

	s.reinforceLinked(context.Background(), nil)
}

// TestReinforceLinked_AdvancesNeighbourDecayClocks covers the happy path: a recalled memory's
// direct neighbours have their decay clocks advanced a fraction of the way toward now, without
// their recall counts moving.
func TestReinforceLinked_AdvancesNeighbourDecayClocks(t *testing.T) {
	s := newTestServer(t)
	s.consolidation.linkRecallPropagation = 0.5
	seedLinkMemories(t, s, "m1", "m2")

	link := &contract.LinkMemoriesRequest{Id: "m1", Links: []*contract.Link{{Id: "m2", Significance: 1}}}
	if _, err := s.LinkMemories(context.Background(), link); err != nil {
		t.Fatalf("LinkMemories: %s", err)
	}

	before, err := s.db.GetMemoriesByIds(context.Background(), []string{"m2"})
	if err != nil {
		t.Fatalf("GetMemoriesByIds: %s", err)
	}

	s.reinforceLinked(context.Background(), []string{"m1"})

	after, err := s.db.GetMemoriesByIds(context.Background(), []string{"m2"})
	if err != nil {
		t.Fatalf("GetMemoriesByIds: %s", err)
	}

	if (*after)[0].TimeRecalled <= (*before)[0].TimeRecalled {
		t.Errorf(
			"expected the neighbour's decay clock to advance, went from %d to %d",
			(*before)[0].TimeRecalled,
			(*after)[0].TimeRecalled,
		)
	}

	if (*after)[0].RecallCount != (*before)[0].RecallCount {
		t.Errorf(
			"spreading activation must not move recall counts, went from %d to %d",
			(*before)[0].RecallCount,
			(*after)[0].RecallCount,
		)
	}
}

// TestReinforceLinked_NoNeighbours covers the branch where the lookup succeeds but finds nothing.
func TestReinforceLinked_NoNeighbours(t *testing.T) {
	s := newTestServer(t)
	s.consolidation.linkRecallPropagation = 0.5
	seedLinkMemories(t, s, "m1")

	s.reinforceLinked(context.Background(), []string{"m1"})
}

// TestReinforceLinked_StoreFailuresAreSwallowed pins the best-effort contract on both store calls:
// a failure here must not fail a recall that already succeeded.
func TestReinforceLinked_StoreFailuresAreSwallowed(t *testing.T) {
	t.Run("neighbour lookup fails", func(t *testing.T) {
		s := newTestServer(t)
		s.consolidation.linkRecallPropagation = 0.5
		seedLinkMemories(t, s, "m1")
		s.db = linkedIdsErrStore{Store: s.db}

		s.reinforceLinked(context.Background(), []string{"m1"})
	})

	t.Run("reinforcement fails", func(t *testing.T) {
		s := newTestServer(t)
		s.consolidation.linkRecallPropagation = 0.5
		seedLinkMemories(t, s, "m1", "m2")

		link := &contract.LinkMemoriesRequest{Id: "m1", Links: []*contract.Link{{Id: "m2", Significance: 1}}}
		if _, err := s.LinkMemories(context.Background(), link); err != nil {
			t.Fatalf("LinkMemories: %s", err)
		}

		s.db = reinforceErrStore{Store: s.db}

		s.reinforceLinked(context.Background(), []string{"m1"})
	})
}

// TestLinkedMemories covers associative retrieval's happy path.
func TestLinkedMemories(t *testing.T) {
	s := newTestServer(t)
	seedLinkMemories(t, s, "m1", "m2")

	link := &contract.LinkMemoriesRequest{Id: "m1", Links: []*contract.Link{{Id: "m2", Significance: 1}}}
	if _, err := s.LinkMemories(context.Background(), link); err != nil {
		t.Fatalf("LinkMemories: %s", err)
	}

	linked := s.linkedMemories(context.Background(), []string{"m1"})
	if len(linked) != 1 || linked[0].Id != "m2" {
		t.Fatalf("expected the one-hop neighbour, got %+v", linked)
	}
}

// TestLinkedMemories_IsAPlainRead pins the deliberate decision that surfacing a linked memory does
// not reinforce it: an association is not a retrieval, and spreading activation is the separately
// configured mechanism that may move those clocks.
func TestLinkedMemories_IsAPlainRead(t *testing.T) {
	s := newTestServer(t)
	seedLinkMemories(t, s, "m1", "m2")

	link := &contract.LinkMemoriesRequest{Id: "m1", Links: []*contract.Link{{Id: "m2", Significance: 1}}}
	if _, err := s.LinkMemories(context.Background(), link); err != nil {
		t.Fatalf("LinkMemories: %s", err)
	}

	before, err := s.db.GetMemoriesByIds(context.Background(), []string{"m2"})
	if err != nil {
		t.Fatalf("GetMemoriesByIds: %s", err)
	}

	s.linkedMemories(context.Background(), []string{"m1"})

	after, err := s.db.GetMemoriesByIds(context.Background(), []string{"m2"})
	if err != nil {
		t.Fatalf("GetMemoriesByIds: %s", err)
	}

	if (*after)[0].TimeRecalled != (*before)[0].TimeRecalled || (*after)[0].RecallCount != (*before)[0].RecallCount {
		t.Error("expected surfacing a linked memory to leave its recall state untouched")
	}
}

// TestLinkedMemories_EmptyAndFailureCases covers the three branches that return nil.
func TestLinkedMemories_EmptyAndFailureCases(t *testing.T) {
	t.Run("no ids", func(t *testing.T) {
		s := newTestServer(t)

		if got := s.linkedMemories(context.Background(), nil); got != nil {
			t.Errorf("expected nil for an empty id set, got %+v", got)
		}
	})

	t.Run("no neighbours", func(t *testing.T) {
		s := newTestServer(t)
		seedLinkMemories(t, s, "m1")

		if got := s.linkedMemories(context.Background(), []string{"m1"}); got != nil {
			t.Errorf("expected nil when nothing is linked, got %+v", got)
		}
	})

	t.Run("neighbour lookup fails", func(t *testing.T) {
		s := newTestServer(t)
		s.db = linkedIdsErrStore{Store: s.db}

		if got := s.linkedMemories(context.Background(), []string{"m1"}); got != nil {
			t.Errorf("expected nil when the lookup fails, got %+v", got)
		}
	})

	t.Run("neighbour read fails", func(t *testing.T) {
		s := newTestServer(t)
		seedLinkMemories(t, s, "m1", "m2")

		link := &contract.LinkMemoriesRequest{Id: "m1", Links: []*contract.Link{{Id: "m2", Significance: 1}}}
		if _, err := s.LinkMemories(context.Background(), link); err != nil {
			t.Fatalf("LinkMemories: %s", err)
		}

		s.db = getByIdsErrStore{Store: s.db}

		if got := s.linkedMemories(context.Background(), []string{"m1"}); got != nil {
			t.Errorf("expected nil when the read fails, got %+v", got)
		}
	})
}

// TestIdsOfMemories covers the projection helper, including the empty case.
func TestIdsOfMemories(t *testing.T) {
	got := idsOfMemories([]types.Memory{{Id: "a"}, {Id: "b"}})
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("unexpected ids: %+v", got)
	}

	if len(idsOfMemories(nil)) != 0 {
		t.Error("expected no ids from an empty slice")
	}
}

// TestRecallMemories_IncludeLinked covers associative recall: the neighbours are appended after the
// memories actually asked for, and are not themselves counted as recalled.
func TestRecallMemories_IncludeLinked(t *testing.T) {
	s := newTestServer(t)
	seedLinkMemories(t, s, "m1", "m2")

	link := &contract.LinkMemoriesRequest{Id: "m1", Links: []*contract.Link{{Id: "m2", Significance: 1}}}
	if _, err := s.LinkMemories(context.Background(), link); err != nil {
		t.Fatalf("LinkMemories: %s", err)
	}

	res, err := s.RecallMemories(context.Background(), &contract.RecallMemoriesRequest{
		Ids:           []string{"m1"},
		IncludeLinked: true,
	})
	if err != nil {
		t.Fatalf("RecallMemories: %s", err)
	}

	if len(res.GetMemories()) != 2 {
		t.Fatalf("expected the recalled memory plus its neighbour, got %d", len(res.GetMemories()))
	}

	if res.GetMemories()[0].GetId() != "m1" || res.GetMemories()[1].GetId() != "m2" {
		t.Errorf("expected the neighbour appended after the memory asked for, got %+v", res.GetMemories())
	}

	// The neighbour was surfaced, not retrieved: its recall count must not have moved.
	if got := res.GetMemories()[1].GetRecallCount(); got != 0 {
		t.Errorf("expected the linked memory's recall count to stay 0, got %d", got)
	}
}

// TestGetMemories_LinkedTo covers the listing filter's happy path: it resolves to the neighbours'
// ids and composes with the ordinary filters and pagination.
func TestGetMemories_LinkedTo(t *testing.T) {
	s := newTestServer(t)
	seedLinkMemories(t, s, "hub", "n1", "n2", "unrelated")

	link := &contract.LinkMemoriesRequest{
		Id: "hub",
		Links: []*contract.Link{
			{Id: "n1", Significance: 1},
			{Id: "n2", Significance: 1},
		},
	}

	if _, err := s.LinkMemories(context.Background(), link); err != nil {
		t.Fatalf("LinkMemories: %s", err)
	}

	res, err := s.GetMemories(context.Background(), &contract.GetMemoriesRequest{LinkedTo: "hub"})
	if err != nil {
		t.Fatalf("GetMemories: %s", err)
	}

	if len(res.GetMemories()) != 2 {
		t.Fatalf("expected the hub's two neighbours, got %d", len(res.GetMemories()))
	}

	for _, m := range res.GetMemories() {
		if m.GetId() != "n1" && m.GetId() != "n2" {
			t.Errorf("unexpected memory in a linked_to listing: %s", m.GetId())
		}
	}

	if res.GetTotalCount() != 2 {
		t.Errorf("expected a total count of 2, got %d", res.GetTotalCount())
	}
}

// TestGetMemories_LinkedToUnknownMemory pins the documented NotFound, which distinguishes "that
// memory does not exist" from "that memory has no neighbours".
func TestGetMemories_LinkedToUnknownMemory(t *testing.T) {
	s := newTestServer(t)

	_, err := s.GetMemories(context.Background(), &contract.GetMemoriesRequest{LinkedTo: "ghost"})
	if err == nil {
		t.Fatal("expected an unknown linked_to memory to be rejected")
	}

	if got := status.Code(err); got != codes.NotFound {
		t.Errorf("expected codes.NotFound, got %s (%v)", got, err)
	}
}

// TestGetMemories_LinkedToWithNoNeighbours pins the comment's warning: no neighbours must be an
// empty page, not an unrestricted one — an empty id set left on the filter would read as "no id
// restriction" and return the whole store.
func TestGetMemories_LinkedToWithNoNeighbours(t *testing.T) {
	s := newTestServer(t)
	seedLinkMemories(t, s, "lonely", "other-a", "other-b")

	res, err := s.GetMemories(context.Background(), &contract.GetMemoriesRequest{LinkedTo: "lonely"})
	if err != nil {
		t.Fatalf("GetMemories: %s", err)
	}

	if len(res.GetMemories()) != 0 {
		t.Errorf("expected an empty page, got %d memories", len(res.GetMemories()))
	}
}

// TestGetMemories_LinkedToStoreFailures walks the two store calls the filter makes.
func TestGetMemories_LinkedToStoreFailures(t *testing.T) {
	tests := []struct {
		name string
		wrap func(db.Store) db.Store
	}{
		{
			name: "existence check fails",
			wrap: func(inner db.Store) db.Store { return missingErrStore{Store: inner} },
		},
		{
			name: "neighbour lookup fails",
			wrap: func(inner db.Store) db.Store { return linkedIdsErrStore{Store: inner} },
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s := newTestServer(t)
			seedLinkMemories(t, s, "hub")
			s.db = test.wrap(s.db)

			_, err := s.GetMemories(context.Background(), &contract.GetMemoriesRequest{LinkedTo: "hub"})
			if err == nil {
				t.Fatal("expected the store failure to reach the caller")
			}

			if got := status.Code(err); got != codes.Internal {
				t.Errorf("expected codes.Internal, got %s (%v)", got, err)
			}
		})
	}
}

// TestGetMemories_Links covers the links option, which populates each returned memory's links.
func TestGetMemories_Links(t *testing.T) {
	s := newTestServer(t)
	seedLinkMemories(t, s, "m1", "m2")

	link := &contract.LinkMemoriesRequest{Id: "m1", Links: []*contract.Link{{Id: "m2", Significance: 8}}}
	if _, err := s.LinkMemories(context.Background(), link); err != nil {
		t.Fatalf("LinkMemories: %s", err)
	}

	res, err := s.GetMemories(context.Background(), &contract.GetMemoriesRequest{Links: true})
	if err != nil {
		t.Fatalf("GetMemories: %s", err)
	}

	var found bool

	for _, m := range res.GetMemories() {
		if m.GetId() != "m1" {
			continue
		}

		found = true

		if len(m.GetLinks()) != 1 || m.GetLinks()[0].GetId() != "m2" || m.GetLinks()[0].GetSignificance() != 8 {
			t.Errorf("expected m1 to carry its link, got %+v", m.GetLinks())
		}

		break
	}

	if !found {
		t.Fatal("expected m1 in the listing")
	}
}

// TestImportBatch_AppliesLinks covers the import's second pass: the link sets carried by the
// imported rows are applied once every row in the batch exists, which is what lets an archive carry
// a link whose target appears later in the same batch.
func TestImportBatch_AppliesLinks(t *testing.T) {
	s := newTestServer(t)

	req := &contract.ImportBatchRequest{
		Events: []*contract.Event{
			{Id: "e1", Name: "first", TimeStart: 100, Significance: 5, Links: []*contract.Link{{Id: "e2", Significance: 2}}},
			{Id: "e2", Name: "second", TimeStart: 200, Significance: 5},
		},
		Memories: []*contract.Memory{
			{Id: "m1", TimeStamp: 100, Significance: 5, Body: "one", Links: []*contract.Link{{Id: "m2", Significance: 3}}},
			{Id: "m2", TimeStamp: 200, Significance: 5, Body: "two"},
		},
	}

	res, err := s.ImportBatch(context.Background(), req)
	if err != nil {
		t.Fatalf("ImportBatch: %s", err)
	}

	if res.GetEventsImported() != 2 || res.GetMemoriesImported() != 2 {
		t.Fatalf("unexpected import counts: %+v", res)
	}

	memoryLinks, err := s.GetMemoryLinks(context.Background(), &contract.GetMemoryLinksRequest{Id: "m1"})
	if err != nil {
		t.Fatalf("GetMemoryLinks: %s", err)
	}

	if len(memoryLinks.GetLinks()) != 1 || memoryLinks.GetLinks()[0].GetId() != "m2" {
		t.Errorf("expected the imported memory link to be applied, got %+v", memoryLinks.GetLinks())
	}

	eventLinks, err := s.GetEventLinks(context.Background(), &contract.GetEventLinksRequest{Id: "e1"})
	if err != nil {
		t.Fatalf("GetEventLinks: %s", err)
	}

	if len(eventLinks.GetLinks()) != 1 || eventLinks.GetLinks()[0].GetId() != "e2" {
		t.Errorf("expected the imported event link to be applied, got %+v", eventLinks.GetLinks())
	}
}

// TestImportBatch_DropsDanglingLinks pins the best-effort half of the second pass: a link whose far
// end is in neither the batch nor the store is dropped by the store rather than failing the import,
// because a partial archive is exactly what that looks like.
func TestImportBatch_DropsDanglingLinks(t *testing.T) {
	s := newTestServer(t)

	req := &contract.ImportBatchRequest{
		Events: []*contract.Event{
			{Id: "e1", Name: "first", TimeStart: 100, Significance: 5, Links: []*contract.Link{{Id: "absent", Significance: 2}}},
		},
		Memories: []*contract.Memory{
			{Id: "m1", TimeStamp: 100, Significance: 5, Body: "one", Links: []*contract.Link{{Id: "absent", Significance: 3}}},
		},
	}

	res, err := s.ImportBatch(context.Background(), req)
	if err != nil {
		t.Fatalf("expected a dangling link not to fail the import: %s", err)
	}

	if res.GetEventsImported() != 1 || res.GetMemoriesImported() != 1 {
		t.Fatalf("expected the rows themselves to import: %+v", res)
	}

	memoryLinks, err := s.GetMemoryLinks(context.Background(), &contract.GetMemoryLinksRequest{Id: "m1"})
	if err != nil {
		t.Fatalf("GetMemoryLinks: %s", err)
	}

	if len(memoryLinks.GetLinks()) != 0 {
		t.Errorf("expected the dangling memory link to be dropped, got %+v", memoryLinks.GetLinks())
	}

	eventLinks, err := s.GetEventLinks(context.Background(), &contract.GetEventLinksRequest{Id: "e1"})
	if err != nil {
		t.Fatalf("GetEventLinks: %s", err)
	}

	if len(eventLinks.GetLinks()) != 0 {
		t.Errorf("expected the dangling event link to be dropped, got %+v", eventLinks.GetLinks())
	}
}

// importLinksErrStore fails both halves of the import's link pass.
type importLinksErrStore struct {
	db.Store
}

func (importLinksErrStore) ImportEventLinks(ctx context.Context, links map[string][]types.Link) (int, int, error) {
	return 0, 0, errLink
}

func (importLinksErrStore) ImportMemoryLinks(ctx context.Context, links map[string][]types.Link) (int, int, error) {
	return 0, 0, errLink
}

// TestImportBatch_LinkFailureDoesNotFailTheImport pins applyImportedLinks' best-effort contract: the
// rows are already committed and counted, so failing the call after that point would tell the caller
// nothing useful about what to retry.
func TestImportBatch_LinkFailureDoesNotFailTheImport(t *testing.T) {
	s := newTestServer(t)
	s.db = importLinksErrStore{Store: s.db}

	req := &contract.ImportBatchRequest{
		Events: []*contract.Event{
			{Id: "e1", Name: "first", TimeStart: 100, Significance: 5, Links: []*contract.Link{{Id: "e2", Significance: 2}}},
			{Id: "e2", Name: "second", TimeStart: 200, Significance: 5},
		},
		Memories: []*contract.Memory{
			{Id: "m1", TimeStamp: 100, Significance: 5, Body: "one", Links: []*contract.Link{{Id: "m2", Significance: 3}}},
			{Id: "m2", TimeStamp: 200, Significance: 5, Body: "two"},
		},
	}

	res, err := s.ImportBatch(context.Background(), req)
	if err != nil {
		t.Fatalf("expected the link failure to be swallowed: %s", err)
	}

	if res.GetEventsImported() != 2 || res.GetMemoriesImported() != 2 {
		t.Errorf("expected the rows to import regardless: %+v", res)
	}
}

// TestImportBatch_RejectsInvalidMetadata covers the metadata validation both ingest paths apply to
// imported rows: an archive is not a trusted input.
func TestImportBatch_RejectsInvalidMetadata(t *testing.T) {
	oversized := strings.Repeat("x", types.MaxMetadataValueLength+1)

	t.Run("event", func(t *testing.T) {
		s := newTestServer(t)

		req := &contract.ImportBatchRequest{
			Events: []*contract.Event{
				{Id: "e1", Name: "first", TimeStart: 100, Significance: 5, Metadata: map[string]string{"k": oversized}},
			},
		}

		if _, err := s.ImportBatch(context.Background(), req); err == nil {
			t.Fatal("expected invalid event metadata to be rejected")
		}
	})

	t.Run("memory", func(t *testing.T) {
		s := newTestServer(t)

		req := &contract.ImportBatchRequest{
			Memories: []*contract.Memory{
				{Id: "m1", TimeStamp: 100, Significance: 5, Body: "one", Metadata: map[string]string{"k": oversized}},
			},
		}

		if _, err := s.ImportBatch(context.Background(), req); err == nil {
			t.Fatal("expected invalid memory metadata to be rejected")
		}
	})
}
