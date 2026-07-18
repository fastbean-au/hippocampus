package hippocampus

import (
	"context"

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
