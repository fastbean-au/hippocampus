package hippocampus

import (
	"context"
	"sort"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/fastbean-au/hippocampus/contract"
	"github.com/fastbean-au/hippocampus/types"
)

// The RPC half of TODO-2 item 94: UpdateEvent, and the four GetEvents/GetEventById additions that
// bring the event surface level with the memory one.

// seedEventRPCs builds a small store with two open events, one ended one, and a link between two of
// them - enough for every filter below to have something it must NOT return.
func seedEventRPCs(t *testing.T) *Server {
	t.Helper()

	s := newTestServer(t)
	ctx := context.Background()

	events := []types.Event{
		{Id: "e-open", Name: "Election night", TimeStart: 100, Significance: 3, Group: "news"},
		{Id: "e-other", Name: "budget thread", TimeStart: 200, Significance: 4},
		{Id: "e-ended", Name: "The election recount", TimeStart: 300, TimeEnd: 400, Significance: 5},
	}

	for _, e := range events {
		if _, err := s.db.CreateEvent(ctx, e); err != nil {
			t.Fatalf("CreateEvent(%s): %s", e.Id, err)
		}
	}

	if err := s.db.LinkEvents(ctx, "e-open", []types.Link{{Id: "e-ended", Significance: 7}}); err != nil {
		t.Fatalf("LinkEvents: %s", err)
	}

	return s
}

func eventIds(res *contract.GetEventsResponse) []string {
	out := make([]string, 0, len(res.GetEvents()))

	for _, e := range res.GetEvents() {
		out = append(out, e.GetId())
	}

	sort.Strings(out)

	return out
}

func TestUpdateEvent(t *testing.T) {
	ctx := context.Background()

	t.Run("updates the five fields no RPC could reach", func(t *testing.T) {
		s := seedEventRPCs(t)

		res, err := s.UpdateEvent(ctx, &contract.Event{
			Id:          "e-open",
			Name:        "Election night, corrected",
			Description: "a description",
			Group:       "politics",
			Metadata:    map[string]string{"outlet": "abc"},
			TimeEnd:     999,
		})
		if err != nil {
			t.Fatalf("UpdateEvent: %s", err)
		}

		if !res.GetOk() {
			t.Error("UpdateEvent reported not ok")
		}

		stored, err := s.db.GetEvent(ctx, "e-open")
		if err != nil {
			t.Fatalf("GetEvent: %s", err)
		}

		switch {

		case stored.Name != "Election night, corrected":
			t.Errorf("name = %q, want the corrected one", stored.Name)

		case stored.Description != "a description":
			t.Errorf("description = %q, want it set", stored.Description)

		case stored.Group != "politics":
			t.Errorf("group = %q, want politics", stored.Group)

		case stored.Metadata["outlet"] != "abc":
			t.Errorf("metadata = %v, want outlet=abc", stored.Metadata)

		case stored.TimeEnd != 999:
			t.Errorf("time_end = %d, want 999", stored.TimeEnd)

		}

		// Untouched fields keep their stored values - this is a partial update, not a replace.
		if stored.TimeStart != 100 || stored.Significance != 3 {
			t.Errorf("time_start/significance = %d/%d, want 100/3 - an absent field must leave the row alone",
				stored.TimeStart, stored.Significance)
		}
	})

	t.Run("clears group and metadata", func(t *testing.T) {
		s := seedEventRPCs(t)

		if _, err := s.UpdateEvent(ctx, &contract.Event{Id: "e-open", Metadata: map[string]string{"k": "v"}}); err != nil {
			t.Fatalf("UpdateEvent(set metadata): %s", err)
		}

		if _, err := s.UpdateEvent(ctx, &contract.Event{Id: "e-open", ClearGroup: true, ClearMetadata: true}); err != nil {
			t.Fatalf("UpdateEvent(clear): %s", err)
		}

		stored, err := s.db.GetEvent(ctx, "e-open")
		if err != nil {
			t.Fatalf("GetEvent: %s", err)
		}

		if stored.Group != "" || len(stored.Metadata) != 0 {
			t.Errorf("group/metadata = %q/%v, want both cleared", stored.Group, stored.Metadata)
		}
	})

	t.Run("changes significance, absolutely and by placement", func(t *testing.T) {
		s := seedEventRPCs(t)

		if _, err := s.UpdateEvent(ctx, &contract.Event{Id: "e-open", Significance: 9}); err != nil {
			t.Fatalf("UpdateEvent(significance): %s", err)
		}

		stored, err := s.db.GetEvent(ctx, "e-open")
		if err != nil {
			t.Fatalf("GetEvent: %s", err)
		}

		if stored.Significance != 9 {
			t.Fatalf("significance = %d, want 9", stored.Significance)
		}

		if _, err := s.UpdateEvent(ctx, &contract.Event{
			Id: "e-other",
			Placement: &contract.SignificancePlacement{
				Mode:   contract.SignificancePlacement_ABOVE,
				Anchor: 9,
			},
		}); err != nil {
			t.Fatalf("UpdateEvent(placement): %s", err)
		}

		stored, err = s.db.GetEvent(ctx, "e-other")
		if err != nil {
			t.Fatalf("GetEvent: %s", err)
		}

		if stored.Significance <= 9 {
			t.Errorf("significance = %d, want a rank above 9", stored.Significance)
		}
	})

	t.Run("rejects an empty id", func(t *testing.T) {
		s := seedEventRPCs(t)

		_, err := s.UpdateEvent(ctx, &contract.Event{Name: "no id"})

		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("UpdateEvent(no id) = %v, want InvalidArgument", err)
		}
	})

	t.Run("rejects an invalid field", func(t *testing.T) {
		s := seedEventRPCs(t)

		_, err := s.UpdateEvent(ctx, &contract.Event{Id: "e-open", Significance: -1})

		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("UpdateEvent(significance=-1) = %v, want InvalidArgument", err)
		}
	})

	t.Run("an unknown id is NotFound, not a create", func(t *testing.T) {
		s := seedEventRPCs(t)

		_, err := s.UpdateEvent(ctx, &contract.Event{Id: "nope", Name: "phantom"})

		if status.Code(err) != codes.NotFound {
			t.Fatalf("UpdateEvent(unknown) = %v, want NotFound", err)
		}

		if exists, err := s.db.EventExists(ctx, "nope"); err != nil || exists {
			t.Error("UpdateEvent created an event for an unknown id")
		}
	})

	t.Run("does not replace the link graph", func(t *testing.T) {
		s := seedEventRPCs(t)

		if _, err := s.UpdateEvent(ctx, &contract.Event{Id: "e-open", Name: "renamed"}); err != nil {
			t.Fatalf("UpdateEvent: %s", err)
		}

		links, _, err := s.db.GetEventLinks(ctx, "e-open", types.LinkDirectionOutbound)
		if err != nil {
			t.Fatalf("GetEventLinks: %s", err)
		}

		if len(links) != 1 {
			t.Errorf("an update carrying no links left %d links, want the existing 1 untouched", len(links))
		}
	})
}

func TestGetEvents_Ended(t *testing.T) {
	s := seedEventRPCs(t)
	ctx := context.Background()

	open, err := s.GetEvents(ctx, &contract.GetEventsRequest{Ended: contract.Bool_FALSE})
	if err != nil {
		t.Fatalf("GetEvents(ended=false): %s", err)
	}

	if got := eventIds(open); len(got) != 2 || got[0] != "e-open" || got[1] != "e-other" {
		t.Errorf("GetEvents(ended=false) = %v, want the two open events", got)
	}

	ended, err := s.GetEvents(ctx, &contract.GetEventsRequest{Ended: contract.Bool_TRUE})
	if err != nil {
		t.Fatalf("GetEvents(ended=true): %s", err)
	}

	if got := eventIds(ended); len(got) != 1 || got[0] != "e-ended" {
		t.Errorf("GetEvents(ended=true) = %v, want [e-ended]", got)
	}
}

// TestGetEvents_TimeEndMaxExcludesOpenEvents is item 94.2's behaviour change at the RPC layer: the
// bound now asks only about events that ended, so "ended before X" no longer returns everything
// still running.
func TestGetEvents_TimeEndMaxExcludesOpenEvents(t *testing.T) {
	s := seedEventRPCs(t)

	res, err := s.GetEvents(context.Background(), &contract.GetEventsRequest{TimeEndMax: 500})
	if err != nil {
		t.Fatalf("GetEvents: %s", err)
	}

	if got := eventIds(res); len(got) != 1 || got[0] != "e-ended" {
		t.Errorf("GetEvents(time_end_max=500) = %v, want [e-ended]", got)
	}

	if res.GetTotalCount() != 1 {
		t.Errorf("total_count = %d, want 1", res.GetTotalCount())
	}
}

func TestGetEvents_NameContains(t *testing.T) {
	s := seedEventRPCs(t)
	ctx := context.Background()

	res, err := s.GetEvents(ctx, &contract.GetEventsRequest{NameContains: "ELECTION"})
	if err != nil {
		t.Fatalf("GetEvents: %s", err)
	}

	if got := eventIds(res); len(got) != 2 || got[0] != "e-ended" || got[1] != "e-open" {
		t.Errorf("GetEvents(name_contains=ELECTION) = %v, want both election events", got)
	}

	// It composes with the other predicates rather than replacing them.
	res, err = s.GetEvents(ctx, &contract.GetEventsRequest{NameContains: "election", Ended: contract.Bool_TRUE})
	if err != nil {
		t.Fatalf("GetEvents: %s", err)
	}

	if got := eventIds(res); len(got) != 1 || got[0] != "e-ended" {
		t.Errorf("GetEvents(name_contains + ended) = %v, want [e-ended]", got)
	}
}

func TestGetEvents_LinkedTo(t *testing.T) {
	s := seedEventRPCs(t)
	ctx := context.Background()

	res, err := s.GetEvents(ctx, &contract.GetEventsRequest{LinkedTo: "e-open"})
	if err != nil {
		t.Fatalf("GetEvents(linked_to): %s", err)
	}

	if got := eventIds(res); len(got) != 1 || got[0] != "e-ended" {
		t.Errorf("GetEvents(linked_to=e-open) = %v, want [e-ended]", got)
	}

	// The edge is directed but the traversal is not - the far end sees it too.
	res, err = s.GetEvents(ctx, &contract.GetEventsRequest{LinkedTo: "e-ended"})
	if err != nil {
		t.Fatalf("GetEvents(linked_to, inbound): %s", err)
	}

	if got := eventIds(res); len(got) != 1 || got[0] != "e-open" {
		t.Errorf("GetEvents(linked_to=e-ended) = %v, want [e-open]", got)
	}

	// An event with no neighbours is an empty page, never an unrestricted one.
	res, err = s.GetEvents(ctx, &contract.GetEventsRequest{LinkedTo: "e-other"})
	if err != nil {
		t.Fatalf("GetEvents(linked_to, no neighbours): %s", err)
	}

	if len(res.GetEvents()) != 0 {
		t.Errorf("GetEvents(linked_to=e-other) = %v, want an empty page", eventIds(res))
	}

	_, err = s.GetEvents(ctx, &contract.GetEventsRequest{LinkedTo: "nope"})

	if status.Code(err) != codes.NotFound {
		t.Errorf("GetEvents(linked_to=nope) = %v, want NotFound", err)
	}
}

func TestGetEvents_Links(t *testing.T) {
	s := seedEventRPCs(t)
	ctx := context.Background()

	res, err := s.GetEvents(ctx, &contract.GetEventsRequest{Links: true})
	if err != nil {
		t.Fatalf("GetEvents(links): %s", err)
	}

	found := false

	for _, e := range res.GetEvents() {
		if e.GetId() != "e-open" {
			continue
		}

		found = true

		if len(e.GetLinks()) != 1 || e.GetLinks()[0].GetId() != "e-ended" {
			t.Errorf("e-open carried links %v, want the one edge to e-ended", e.GetLinks())
		}

		break
	}

	if !found {
		t.Fatal("e-open was not in the page")
	}

	// Without the flag nothing is attached, which is what keeps the listing off the link tables.
	res, err = s.GetEvents(ctx, &contract.GetEventsRequest{})
	if err != nil {
		t.Fatalf("GetEvents: %s", err)
	}

	for _, e := range res.GetEvents() {
		if len(e.GetLinks()) != 0 {
			t.Errorf("%s carried links without being asked for them", e.GetId())
		}
	}
}

func TestGetEventById_Links(t *testing.T) {
	s := seedEventRPCs(t)
	ctx := context.Background()

	res, err := s.GetEventById(ctx, &contract.GetEventByIdRequest{Id: "e-open", Links: true})
	if err != nil {
		t.Fatalf("GetEventById(links): %s", err)
	}

	if len(res.GetEvent().GetLinks()) != 1 || res.GetEvent().GetLinks()[0].GetId() != "e-ended" {
		t.Errorf("links = %v, want the one edge to e-ended", res.GetEvent().GetLinks())
	}

	res, err = s.GetEventById(ctx, &contract.GetEventByIdRequest{Id: "e-open"})
	if err != nil {
		t.Fatalf("GetEventById: %s", err)
	}

	if len(res.GetEvent().GetLinks()) != 0 {
		t.Errorf("links = %v, want none without the flag", res.GetEvent().GetLinks())
	}
}

func TestGetSignificanceLevels(t *testing.T) {
	s := seedEventRPCs(t)
	ctx := context.Background()

	res, err := s.GetSignificanceLevels(ctx, &contract.GetSignificanceLevelsRequest{})
	if err != nil {
		t.Fatalf("GetSignificanceLevels: %s", err)
	}

	want := []int32{3, 4, 5}

	if len(res.GetSignificances()) != len(want) {
		t.Fatalf("significances = %v, want %v", res.GetSignificances(), want)
	}

	for i, rank := range want {
		if res.GetSignificances()[i] != rank {
			t.Fatalf("significances = %v, want %v (ascending)", res.GetSignificances(), want)
		}
	}

	if res.GetTotalCount() != int32(len(want)) {
		t.Errorf("total_count = %d, want %d", res.GetTotalCount(), len(want))
	}

	bounded, err := s.GetSignificanceLevels(ctx, &contract.GetSignificanceLevelsRequest{SignificanceMin: 4, SignificanceMax: 4})
	if err != nil {
		t.Fatalf("GetSignificanceLevels(bounded): %s", err)
	}

	if len(bounded.GetSignificances()) != 1 || bounded.GetSignificances()[0] != 4 {
		t.Errorf("significances = %v, want [4]", bounded.GetSignificances())
	}

	// A page that ran off the end answers its own total, exactly as the two listings do.
	paged, err := s.GetSignificanceLevels(ctx, &contract.GetSignificanceLevelsRequest{Limit: 2, Offset: 2})
	if err != nil {
		t.Fatalf("GetSignificanceLevels(paged): %s", err)
	}

	if len(paged.GetSignificances()) != 1 || paged.GetTotalCount() != 3 {
		t.Errorf("paged = %v, total_count = %d; want one value and a total of 3",
			paged.GetSignificances(), paged.GetTotalCount())
	}

	_, err = s.GetSignificanceLevels(ctx, &contract.GetSignificanceLevelsRequest{SignificanceMin: 9, SignificanceMax: 2})

	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("GetSignificanceLevels(max < min) = %v, want InvalidArgument", err)
	}
}
