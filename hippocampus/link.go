package hippocampus

import (
	"context"
	"strings"

	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/fastbean-au/hippocampus/contract"
	"github.com/fastbean-au/hippocampus/types"
)

// The link RPCs. Memories and events share one implementation here as they do in the store: a link
// is a link, and the only difference between the two graphs is which table the ids have to exist in.
//
// Every write goes through the same three steps - validate the set, confirm both ends exist, then
// write - in that order. The existence check is what this layer adds over the store: links must not
// dangle (the aggregate feeding the decay maths is maintained per item, so an edge to something that
// was never there would count significance for one end forever), and NotFound naming the missing id
// is a far more useful answer than a foreign-key error would be.

// linkKind is one graph as this layer sees it: how to check ids, how to write, and what to call the
// thing in an error message.
type linkKind struct {
	name    string
	missing func(ctx context.Context, ids []string) ([]string, error)
	link    func(ctx context.Context, id string, links []types.Link) error
	unlink  func(ctx context.Context, id string, targets []string) error
	get     func(ctx context.Context, id string, d types.LinkDirection) ([]types.LinkEdge, int64, error)
}

func (s *Server) memoryLinks() linkKind {
	return linkKind{
		name:    "memory",
		missing: s.db.MissingMemoryIds,
		link:    s.db.LinkMemories,
		unlink:  s.db.UnlinkMemories,
		get:     s.db.GetMemoryLinks,
	}
}

func (s *Server) eventLinks() linkKind {
	return linkKind{
		name:    "event",
		missing: s.db.MissingEventIds,
		link:    s.db.LinkEvents,
		unlink:  s.db.UnlinkEvents,
		get:     s.db.GetEventLinks,
	}
}

// LinkMemories adds or updates links from one memory to others.
func (s *Server) LinkMemories(ctx context.Context, in *contract.LinkMemoriesRequest) (*contract.GeneralResponse, error) {
	log.Trace("func() LinkMemories")

	return s.createLinks(ctx, s.memoryLinks(), in.GetId(), types.LinksFromProto(in.GetLinks()))
}

// LinkEvents adds or updates links from one event to others.
func (s *Server) LinkEvents(ctx context.Context, in *contract.LinkEventsRequest) (*contract.GeneralResponse, error) {
	log.Trace("func() LinkEvents")

	return s.createLinks(ctx, s.eventLinks(), in.GetId(), types.LinksFromProto(in.GetLinks()))
}

func (s *Server) createLinks(
	ctx context.Context,
	kind linkKind,
	id string,
	links []types.Link,
) (*contract.GeneralResponse, error) {
	res := contract.GeneralResponse{}

	if id == "" {
		return &res, status.Errorf(codes.InvalidArgument, "%s id must be provided", kind.name)
	}

	if len(links) == 0 {
		return &res, status.Error(codes.InvalidArgument, "no links provided")
	}

	if err := types.ValidateLinks(links, id, kind.name); err != nil {
		return &res, status.Error(codes.InvalidArgument, err.Error())
	}

	// The near end and every far end are checked in one call, so a request naming several unknown
	// ids reports all of them rather than one per round trip.
	ids := make([]string, 0, len(links)+1)
	ids = append(ids, id)

	for _, l := range links {
		ids = append(ids, l.Id)
	}

	missing, err := kind.missing(ctx, ids)
	if err != nil {
		return &res, mapError(err)
	}

	if len(missing) > 0 {
		return &res, status.Errorf(
			codes.NotFound,
			"no such %s: %s",
			kind.name,
			strings.Join(missing, ", "),
		)
	}

	// The per-item cap is checked against what the item would end up holding, not against the
	// request alone: a caller adding two links at a time could otherwise walk past it indefinitely.
	if err := s.checkLinkCapacity(ctx, kind, id, links); err != nil {
		return &res, err
	}

	if err := kind.link(ctx, id, links); err != nil {
		return &res, mapError(err)
	}

	res.Ok = true

	return &res, nil
}

// checkLinkCapacity rejects a write that would take an item past types.MaxLinks. The bound is on
// what an item holds in total, in either direction, which is the figure that matters: the cap exists
// so no single item's links can dominate a scan that reads them, and an inbound link costs exactly
// what an outbound one does.
func (s *Server) checkLinkCapacity(ctx context.Context, kind linkKind, id string, links []types.Link) error {
	existing, _, err := kind.get(ctx, id, types.LinkDirectionBoth)
	if err != nil {
		return mapError(err)
	}

	// Re-linking an existing pair is an update, not an addition, so it must not count toward the
	// cap - otherwise re-weighting an item's links would fail once it was full.
	held := make(map[string]bool, len(existing))
	for _, e := range existing {
		held[e.Id] = true
	}

	total := len(held)

	for _, l := range links {
		if !held[l.Id] {
			total++
		}
	}

	if total > types.MaxLinks {
		return status.Errorf(
			codes.InvalidArgument,
			"%s '%s' would hold %d links, exceeding the maximum of %d",
			kind.name,
			id,
			total,
			types.MaxLinks,
		)
	}

	return nil
}

// UnlinkMemories removes links between one memory and the named memories.
func (s *Server) UnlinkMemories(ctx context.Context, in *contract.UnlinkMemoriesRequest) (*contract.GeneralResponse, error) {
	log.Trace("func() UnlinkMemories")

	return s.removeLinks(ctx, s.memoryLinks(), in.GetId(), in.GetIds())
}

// UnlinkEvents removes links between one event and the named events.
func (s *Server) UnlinkEvents(ctx context.Context, in *contract.UnlinkEventsRequest) (*contract.GeneralResponse, error) {
	log.Trace("func() UnlinkEvents")

	return s.removeLinks(ctx, s.eventLinks(), in.GetId(), in.GetIds())
}

func (s *Server) removeLinks(
	ctx context.Context,
	kind linkKind,
	id string,
	targets []string,
) (*contract.GeneralResponse, error) {
	res := contract.GeneralResponse{}

	if id == "" {
		return &res, status.Errorf(codes.InvalidArgument, "%s id must be provided", kind.name)
	}

	if len(targets) == 0 {
		return &res, status.Error(codes.InvalidArgument, "no ids provided")
	}

	// Only the near end has to exist. An unknown target is simply a link that is not there, which is
	// the state the caller asked for - the same reasoning DeleteMemories applies to unknown ids.
	missing, err := kind.missing(ctx, []string{id})
	if err != nil {
		return &res, mapError(err)
	}

	if len(missing) > 0 {
		return &res, status.Errorf(codes.NotFound, "no such %s: %s", kind.name, id)
	}

	if err := kind.unlink(ctx, id, types.DedupeLinkIds(targets)); err != nil {
		return &res, mapError(err)
	}

	res.Ok = true

	return &res, nil
}

// GetMemoryLinks returns a memory's links.
func (s *Server) GetMemoryLinks(ctx context.Context, in *contract.GetMemoryLinksRequest) (*contract.GetLinksResponse, error) {
	log.Trace("func() GetMemoryLinks")

	return s.readLinks(ctx, s.memoryLinks(), in.GetId(), in.GetDirection())
}

// GetEventLinks returns an event's links.
func (s *Server) GetEventLinks(ctx context.Context, in *contract.GetEventLinksRequest) (*contract.GetLinksResponse, error) {
	log.Trace("func() GetEventLinks")

	return s.readLinks(ctx, s.eventLinks(), in.GetId(), in.GetDirection())
}

func (s *Server) readLinks(
	ctx context.Context,
	kind linkKind,
	id string,
	direction contract.LinkDirection,
) (*contract.GetLinksResponse, error) {
	res := contract.GetLinksResponse{}

	if id == "" {
		return &res, status.Errorf(codes.InvalidArgument, "%s id must be provided", kind.name)
	}

	missing, err := kind.missing(ctx, []string{id})
	if err != nil {
		return &res, mapError(err)
	}

	if len(missing) > 0 {
		return &res, status.Errorf(codes.NotFound, "no such %s: %s", kind.name, id)
	}

	edges, total, err := kind.get(ctx, id, types.LinkDirectionFromProto(direction))
	if err != nil {
		return &res, mapError(err)
	}

	res.Links = make([]*contract.LinkEdge, len(edges))
	for i, e := range edges {
		res.Links[i] = e.ToProto()
	}

	res.LinkSignificance = total

	return &res, nil
}

// storeLinks writes the links a create carried, after the item itself has been written. It is
// best-effort by design: the memory or event is already stored and acknowledged, so failing the
// whole call over a link would lose a write the caller has no way to know succeeded. A failure is
// logged and the item stands without the link, which the caller can add through LinkMemories.
//
// Validation and the existence check still run first, so a bad link set is rejected before the item
// is created - see the callers in StoreMemory/StoreEvent.
func (s *Server) storeLinks(ctx context.Context, kind linkKind, id string, links []types.Link) {
	if len(links) == 0 {
		return
	}

	if err := kind.link(ctx, id, links); err != nil {
		log.Errorf("failed to store links for %s '%s': %s", kind.name, id, err.Error())
	}
}

// checkLinkTargets validates a create's link set and confirms every target exists, before the item
// is written. Returns a status error ready to return to the caller.
func (s *Server) checkLinkTargets(ctx context.Context, kind linkKind, id string, links []types.Link) error {
	if len(links) == 0 {
		return nil
	}

	if err := types.ValidateLinks(links, id, kind.name); err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}

	// Only the targets: the item itself is about to be created, so it is legitimately not there yet.
	targets := make([]string, 0, len(links))
	for _, l := range links {
		targets = append(targets, l.Id)
	}

	missing, err := kind.missing(ctx, targets)
	if err != nil {
		return mapError(err)
	}

	if len(missing) > 0 {
		return status.Errorf(codes.NotFound, "no such %s: %s", kind.name, strings.Join(missing, ", "))
	}

	return nil
}

// attachMemoryLinks populates each memory's Links from the store, for a read that asked for them.
// Best-effort: the memories are the answer, and failing the whole read because the links could not
// be loaded would be the wrong trade.
func (s *Server) attachMemoryLinks(ctx context.Context, memories []types.Memory) {
	if len(memories) == 0 {
		return
	}

	ids := make([]string, 0, len(memories))
	for i := range memories {
		ids = append(ids, memories[i].Id)
	}

	links, err := s.db.LinksForMemories(ctx, ids)
	if err != nil {
		log.Errorf("failed to read links for memories: %s", err.Error())

		return
	}

	for i := range memories {
		memories[i].Links = links[memories[i].Id]
	}
}

// attachEventLinks is attachMemoryLinks for events, used by the archive walk so an export carries
// the event half of the graph.
func (s *Server) attachEventLinks(ctx context.Context, events []types.Event) {
	if len(events) == 0 {
		return
	}

	ids := make([]string, 0, len(events))
	for i := range events {
		ids = append(ids, events[i].Id)
	}

	links, err := s.db.LinksForEvents(ctx, ids)
	if err != nil {
		log.Errorf("failed to read links for events: %s", err.Error())

		return
	}

	for i := range events {
		events[i].Links = links[events[i].Id]
	}
}

// reinforceLinked is spreading activation at the RPC layer: after a recall, the memories one hop
// from those recalled have their decay clocks advanced a fraction of the way to now. Off unless
// consolidation.linkRecallPropagation is positive.
//
// Best-effort throughout - a failure here must not fail a recall that already succeeded - and
// silent when the caller's recall was not itself reinforcing, since a read that deliberately did
// not reset the caller's own decay clock must not reset anyone else's.
func (s *Server) reinforceLinked(ctx context.Context, ids []string) {
	if s.consolidation.linkRecallPropagation <= 0 || len(ids) == 0 {
		return
	}

	linked, err := s.db.LinkedMemoryIds(ctx, ids)
	if err != nil {
		log.Errorf("failed to find linked memories to reinforce: %s", err.Error())

		return
	}

	if len(linked) == 0 {
		return
	}

	if err := s.db.ReinforceLinkedMemories(ctx, linked, s.consolidation.linkRecallPropagation); err != nil {
		log.Errorf("failed to reinforce linked memories: %s", err.Error())
	}
}

// idsOfMemories projects a slice of memories to their ids, for the link helpers that take an id set.
func idsOfMemories(memories []types.Memory) []string {
	ids := make([]string, 0, len(memories))
	for i := range memories {
		ids = append(ids, memories[i].Id)
	}

	return ids
}

// linkedMemories fetches the memories one hop from those given, for the include_linked options on
// RecallMemories and SearchMemories. They are a plain read: an associative recall surfaces what a
// memory is connected to, and reinforcing those as though the caller had asked for them by id would
// reset decay clocks on the strength of a link rather than a retrieval. Spreading activation
// (reinforceLinked) is the deliberate, separately configured version of that, and is what should
// move those clocks if anything does.
func (s *Server) linkedMemories(ctx context.Context, ids []string) []types.Memory {
	if len(ids) == 0 {
		return nil
	}

	linked, err := s.db.LinkedMemoryIds(ctx, ids)
	if err != nil {
		log.Errorf("failed to find linked memories: %s", err.Error())

		return nil
	}

	if len(linked) == 0 {
		return nil
	}

	memories, err := s.db.GetMemoriesByIds(ctx, linked)
	if err != nil {
		log.Errorf("failed to read linked memories: %s", err.Error())

		return nil
	}

	return *memories
}
