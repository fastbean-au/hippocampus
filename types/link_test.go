package types

import (
	"reflect"
	"strings"
	"testing"

	"github.com/fastbean-au/hippocampus/contract"
)

// TestLinkEdgeToProto verifies a read-back link converts to its proto form, direction included -
// the direction is the one field LinkEdge adds over Link, so it is the one worth pinning.
func TestLinkEdgeToProto(t *testing.T) {
	e := LinkEdge{Id: "m2", Significance: 9, Direction: LinkDirectionInbound, Created: 42}

	p := e.ToProto()

	if p.GetId() != "m2" || p.GetSignificance() != 9 || p.GetCreated() != 42 {
		t.Errorf("unexpected proto edge: %+v", p)
	}

	if p.GetDirection() != contract.LinkDirection_LINK_DIRECTION_INBOUND {
		t.Errorf("direction not carried through: %v", p.GetDirection())
	}
}

// TestLinkDirectionRoundTrip verifies each direction survives both conversions, and that the
// unspecified value resolves to BOTH rather than to whichever direction happens to be zero. That
// default is load-bearing: the graph is stored directed but valued symmetrically, so a caller
// asking about an item's links almost always means both.
func TestLinkDirectionRoundTrip(t *testing.T) {
	cases := []struct {
		direction LinkDirection
		proto     contract.LinkDirection
	}{
		{LinkDirectionBoth, contract.LinkDirection_LINK_DIRECTION_BOTH},
		{LinkDirectionOutbound, contract.LinkDirection_LINK_DIRECTION_OUTBOUND},
		{LinkDirectionInbound, contract.LinkDirection_LINK_DIRECTION_INBOUND},
	}

	for _, c := range cases {
		if got := c.direction.ToProto(); got != c.proto {
			t.Errorf("ToProto(%v) = %v, want %v", c.direction, got, c.proto)
		}

		if got := LinkDirectionFromProto(c.proto); got != c.direction {
			t.Errorf("FromProto(%v) = %v, want %v", c.proto, got, c.direction)
		}
	}

	if got := LinkDirectionFromProto(contract.LinkDirection_LINK_DIRECTION_UNSPECIFIED); got != LinkDirectionBoth {
		t.Errorf("an unspecified direction should resolve to both, got %v", got)
	}

	// An out-of-range value (a client on a newer contract) degrades to both rather than to an
	// unrepresentable direction.
	if got := LinkDirectionFromProto(contract.LinkDirection(99)); got != LinkDirectionBoth {
		t.Errorf("an unknown direction should degrade to both, got %v", got)
	}

	if got := LinkDirection(99).ToProto(); got != contract.LinkDirection_LINK_DIRECTION_BOTH {
		t.Errorf("an unknown direction should marshal as both, got %v", got)
	}
}

// TestDedupeLinkIds verifies duplicates collapse while first-occurrence order is kept.
func TestDedupeLinkIds(t *testing.T) {
	got := DedupeLinkIds([]string{"b", "a", "b", "c", "a"})

	if want := []string{"b", "a", "c"}; !reflect.DeepEqual(got, want) {
		t.Errorf("DedupeLinkIds = %v, want %v", got, want)
	}

	if got := DedupeLinkIds(nil); len(got) != 0 {
		t.Errorf("DedupeLinkIds(nil) = %v, want empty", got)
	}
}

// TestValidateLinksSelfLinkOnlyWhenOwnerKnown pins that the self-link check is skipped when the
// owner is unknown. A create validates its links before the item has an id, so there is nothing to
// compare against yet - the RPC layer re-checks once the id exists.
func TestValidateLinksSelfLinkOnlyWhenOwnerKnown(t *testing.T) {
	links := []Link{{Id: "m1", Significance: 1}}

	if err := ValidateLinks(links, "m1", "memory"); err == nil {
		t.Error("expected a self-link to be rejected when the owner is known")
	}

	if err := ValidateLinks(links, "", "memory"); err != nil {
		t.Errorf("an unknown owner cannot self-link, got %q", err.Error())
	}
}

// TestValidateLinksKindIsNamed verifies the item type reaches the message, so an event's error does
// not talk about memories.
func TestValidateLinksKindIsNamed(t *testing.T) {
	err := ValidateLinks([]Link{{}}, "e1", "event")
	if err == nil {
		t.Fatal("expected an error")
	}

	if !strings.Contains(err.Error(), "event not valid") {
		t.Errorf("expected the error to name the kind, got %q", err.Error())
	}
}
