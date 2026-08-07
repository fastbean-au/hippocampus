package types

import (
	"fmt"

	"github.com/fastbean-au/hippocampus/contract"
)

// Link validation bounds. Links are a client-supplied input into the decay maths: an item's summed
// link significance is damped and weighted into its value, and for a memory its event's is too. An
// uncapped count would let one item's links dominate every scan that reads them, and an uncapped
// per-link significance would push the damped contribution up without limit, so both are bounded
// here. The damping (see hippocampus.linkContribution) is what stops a merely large number of links
// making an item unforgettable; these bounds are what stop the graph itself growing without limit.
//
// The same two bounds apply to memory links and event links: they are one mechanism, and an
// asymmetry between them would only ever be a trap.
const (
	MaxLinks            = 128
	MaxLinkSignificance = 1_000_000
	maxLinkIdLength     = 128
)

// Link is one directed edge from the item carrying it to the item it names. The near end is
// implied by whatever holds the link, so only the far end is recorded here; LinkEdge is the read
// form that says which way a link points.
type Link struct {
	Id           string
	Significance int32
}

// LinkDirection mirrors contract.LinkDirection without the db package having to depend on the
// contract package, and is what a read uses to say which way a returned link points.
type LinkDirection int

const (
	LinkDirectionBoth LinkDirection = iota
	LinkDirectionOutbound
	LinkDirectionInbound
)

// LinkEdge is a stored link as read back: the far end, the weight, and which way it points
// relative to the item that was asked about.
type LinkEdge struct {
	Id           string
	Significance int32
	Direction    LinkDirection
	Created      int64
}

func (l *Link) ToProto() *contract.Link {
	return &contract.Link{
		Id:           l.Id,
		Significance: l.Significance,
	}
}

func LinksFromProto(links []*contract.Link) []Link {
	ls := make([]Link, len(links))

	for i, l := range links {
		ls[i] = Link{
			Id:           l.GetId(),
			Significance: l.GetSignificance(),
		}
	}

	return ls
}

func LinksToProto(links []Link) []*contract.Link {
	ls := make([]*contract.Link, len(links))

	for i, l := range links {
		ls[i] = l.ToProto()
	}

	return ls
}

func (e *LinkEdge) ToProto() *contract.LinkEdge {
	return &contract.LinkEdge{
		Id:           e.Id,
		Significance: e.Significance,
		Direction:    e.Direction.ToProto(),
		Created:      e.Created,
	}
}

func (d LinkDirection) ToProto() contract.LinkDirection {
	switch d {

	case LinkDirectionOutbound:
		return contract.LinkDirection_LINK_DIRECTION_OUTBOUND

	case LinkDirectionInbound:
		return contract.LinkDirection_LINK_DIRECTION_INBOUND

	default:
		return contract.LinkDirection_LINK_DIRECTION_BOTH

	}
}

// LinkDirectionFromProto resolves a requested direction, defaulting UNSPECIFIED to BOTH: the graph
// is stored directed but valued symmetrically, so both directions are what a caller asking about an
// item's links almost always means.
func LinkDirectionFromProto(d contract.LinkDirection) LinkDirection {
	switch d {

	case contract.LinkDirection_LINK_DIRECTION_OUTBOUND:
		return LinkDirectionOutbound

	case contract.LinkDirection_LINK_DIRECTION_INBOUND:
		return LinkDirectionInbound

	default:
		return LinkDirectionBoth

	}
}

// DedupeLinkIds returns ids with duplicates removed, preserving first-occurrence order. Unlinking
// the same target twice is harmless but pointless work, and the deduplicated list is what the
// aggregate recalculation should be handed.
func DedupeLinkIds(ids []string) []string {
	seen := make(map[string]bool, len(ids))
	out := make([]string, 0, len(ids))

	for _, id := range ids {
		if seen[id] {
			continue
		}

		seen[id] = true
		out = append(out, id)
	}

	return out
}

// ValidateLinks checks a set of links declared by one item. owner is the id of the item declaring
// them, so a self-link can be rejected: an item linked to itself would count its own significance
// twice in its own value, and means nothing as an association. kind names the item type for the
// error message ("memory"/"event").
//
// It does NOT check that the targets exist - that needs the store, so the RPC layer does it and
// returns NotFound. Duplicate targets are rejected rather than silently collapsed: the store upserts
// per pair, so a duplicate in one request would otherwise mean the last one silently wins.
func ValidateLinks(links []Link, owner string, kind string) error {
	if len(links) > MaxLinks {
		return fmt.Errorf("%s not valid - too many links (max %d)", kind, MaxLinks)
	}

	seen := make(map[string]bool, len(links))

	for i, l := range links {
		switch {

		case len(l.Id) == 0:
			return fmt.Errorf("%s not valid - link %d has no id", kind, i)

		case len(l.Id) > maxLinkIdLength:
			return fmt.Errorf("%s not valid - link %d id too long", kind, i)

		case owner != "" && l.Id == owner:
			return fmt.Errorf("%s not valid - link %d links %s to itself", kind, i, kind)

		case seen[l.Id]:
			return fmt.Errorf("%s not valid - link %d duplicates an earlier link to '%s'", kind, i, l.Id)

		case l.Significance < 0:
			return fmt.Errorf("%s not valid - link %d significance must not be < 0", kind, i)

		case l.Significance > MaxLinkSignificance:
			return fmt.Errorf("%s not valid - link %d significance must not exceed %d", kind, i, MaxLinkSignificance)

		}

		seen[l.Id] = true
	}

	return nil
}
