package hippocampus

import (
	"context"

	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/fastbean-au/hippocampus/contract"
	"github.com/fastbean-au/hippocampus/db"
	"github.com/fastbean-au/hippocampus/types"
)

// significanceSpecFromProto builds a db.SignificanceSpec from a request's absolute significance and
// optional placement. kind names the table an id-based anchor refers to (memories for memory RPCs,
// events for event RPCs) - the significance scale is shared, but an anchor id is looked up in the
// item's own table. A nil or UNSPECIFIED placement yields an absolute spec (the value as-is).
func significanceSpecFromProto(value int32, placement *contract.SignificancePlacement, kind db.AnchorKind) db.SignificanceSpec {
	spec := db.SignificanceSpec{Value: value, AnchorKind: kind, UpperKind: kind}

	if placement == nil {
		return spec
	}

	switch placement.GetMode() {

	case contract.SignificancePlacement_ABOVE:
		spec.Placement = db.PlacementAbove

	case contract.SignificancePlacement_BELOW:
		spec.Placement = db.PlacementBelow

	case contract.SignificancePlacement_BETWEEN:
		spec.Placement = db.PlacementBetween

	default:
		return spec
	}

	spec.Anchor = placement.GetAnchor()
	spec.AnchorID = placement.GetAnchorId()
	spec.Upper = placement.GetUpper()
	spec.UpperID = placement.GetUpperId()

	return spec
}

// hasPlacement reports whether a request carries a real (non-UNSPECIFIED) placement directive.
func hasPlacement(placement *contract.SignificancePlacement) bool {
	return placement != nil && placement.GetMode() != contract.SignificancePlacement_UNSPECIFIED
}

// resolveSignificance handles the placement case only: it resolves the relative placement to a
// registry level and stamps the resolved level id and rank onto the item (overwriting the anchor
// value the client sent with the true rank, for the search index and response). An absolute
// significance is left to the store layer (CreateMemory/UpdateMemory etc. resolve it), which keeps
// the fast, lock-free path for ordinary writes; here it is a no-op so Significance keeps carrying
// the client's absolute value.
func (s *Server) resolveSignificance(
	ctx context.Context,
	value int32,
	placement *contract.SignificancePlacement,
	kind db.AnchorKind,
	setLevelID func(*int64),
	setRank func(int32),
) error {
	if !hasPlacement(placement) {
		return nil
	}

	spec := significanceSpecFromProto(value, placement, kind)

	levelID, rank, err := s.db.ResolveSignificanceLevel(ctx, spec)
	if err != nil {
		return err
	}

	if levelID.Valid {
		id := levelID.Int64
		setLevelID(&id)
	}

	setRank(rank)

	return nil
}

// resolveMemorySignificance resolves and stamps a placement for a memory create/update.
func (s *Server) resolveMemorySignificance(ctx context.Context, in *contract.Memory, memory *types.Memory) error {
	return s.resolveSignificance(
		ctx,
		in.GetSignificance(),
		in.GetPlacement(),
		db.AnchorMemory,
		func(id *int64) { memory.SignificanceLevelID = id },
		func(rank int32) { memory.Significance = rank },
	)
}

// resolveEventSignificance resolves and stamps a placement for an event create/update, from an
// absolute significance value plus an optional placement (id-anchors resolve within events).
func (s *Server) resolveEventSignificance(ctx context.Context, value int32, placement *contract.SignificancePlacement, event *types.Event) error {
	return s.resolveSignificance(
		ctx,
		value,
		placement,
		db.AnchorEvent,
		func(id *int64) { event.SignificanceLevelID = id },
		func(rank int32) { event.Significance = rank },
	)
}

// Page-size bounds for the significance registry listing, in the same shape as the memory and event
// listings: an unset (0) limit selects the default, and anything larger than the cap is clamped.
//
// The cap is higher than theirs because a level is one int32 rather than a row - a thousand of them
// is four kilobytes, where a thousand memories is a message nothing should be sending - and because
// the whole point of the RPC is to show a client the scale it is ranking against, which a page of
// twenty-five would not.
const (
	defaultSignificanceLevelPageSize = 200
	maxSignificanceLevelPageSize     = 1000
)

// GetSignificanceLevels lists the distinct significance values in use - the registry
// SignificancePlacement positions against.
//
// It names no stored record: one shared registry ranks memories and events alike, so a value says
// nothing about who carries it. That is why it is reader tier and scopeNone, and why a group-scoped
// caller is answered in full rather than refused - there is no per-group significance scale to
// partition, and the decay maths a scoped caller's memories are subject to runs on this one.
func (s *Server) GetSignificanceLevels(ctx context.Context, in *contract.GetSignificanceLevelsRequest) (*contract.GetSignificanceLevelsResponse, error) {
	log.Trace("func() GetSignificanceLevels")

	var res contract.GetSignificanceLevelsResponse

	if in.GetSignificanceMax() > 0 && in.GetSignificanceMin() > 0 && in.GetSignificanceMax() < in.GetSignificanceMin() {
		return &res, status.Error(codes.InvalidArgument, "SignificanceMax must be greater than or equal to SignificanceMin")
	}

	limit := int(in.GetLimit())

	if limit <= 0 {
		limit = defaultSignificanceLevelPageSize
	}

	if limit > maxSignificanceLevelPageSize {
		limit = maxSignificanceLevelPageSize
	}

	offset := int(in.GetOffset())

	if offset < 0 {
		offset = 0
	}

	filter := db.SignificanceLevelFilter{
		SignificanceMin: in.GetSignificanceMin(),
		SignificanceMax: in.GetSignificanceMax(),
		Limit:           limit,
		Offset:          offset,
	}

	levels, err := s.db.SignificanceLevels(ctx, filter)
	if err != nil {
		return &res, mapError(err)
	}

	// The total is derived from a short page exactly as the two listings derive theirs, and for the
	// same reason - it is a second unbounded pass, and a page that ran off the end has already
	// answered it. See GetMemories for why both forms are exact and why the positive-offset one
	// needs a non-empty page.
	total := len(levels)

	switch {

	case len(levels) >= filter.Limit, filter.Offset > 0 && len(levels) == 0:
		total, err = s.db.CountSignificanceLevels(ctx, filter)
		if err != nil {
			return &res, mapError(err)
		}

	case filter.Offset > 0:
		total = filter.Offset + len(levels)

	}

	res.Significances = levels
	res.TotalCount = int32(total)

	return &res, nil
}
