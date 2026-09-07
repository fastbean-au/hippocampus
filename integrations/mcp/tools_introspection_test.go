package main

import (
	"context"
	"testing"

	"github.com/fastbean-au/hippocampus/contract"
)

// The tools TODO-2 item 100.3 added, plus the event surface item 94 opened. Each asserts the two
// things a bridge handler can get wrong: what it sends, and what it projects back.

func TestUpdateEvent_MapsRequest(t *testing.T) {
	f := &fakeClient{}
	b := newBridge(f)

	_, out, err := b.updateEvent(context.Background(), nil, updateEventInput{
		Id:            "e1",
		Name:          "corrected",
		Description:   "why",
		Significance:  7,
		Group:         "news",
		TimeEnd:       "2026-08-19T14:30:00Z",
		Metadata:      map[string]string{"outlet": "abc"},
		ClearMetadata: true,
	})
	if err != nil {
		t.Fatalf("updateEvent: %s", err)
	}

	if !out.Ok {
		t.Error("ok = false, want true")
	}

	req := f.updateEventReq

	switch {

	case req.GetId() != "e1":
		t.Errorf("id = %q, want e1", req.GetId())

	case req.GetName() != "corrected" || req.GetDescription() != "why":
		t.Errorf("name/description = %q/%q", req.GetName(), req.GetDescription())

	case req.GetSignificance() != 7 || req.GetGroup() != "news":
		t.Errorf("significance/group = %d/%q", req.GetSignificance(), req.GetGroup())

	case req.GetTimeEnd() == 0:
		t.Error("time_end was not parsed from the RFC3339 input")

	case !req.GetClearMetadata():
		t.Error("clear_metadata did not travel")

	}
}

func TestUpdateEvent_RejectsEmptyId(t *testing.T) {
	f := &fakeClient{}

	if _, _, err := newBridge(f).updateEvent(context.Background(), nil, updateEventInput{Name: "x"}); err == nil {
		t.Fatal("expected an error for a missing id")
	}

	if f.updateEventReq != nil {
		t.Error("UpdateEvent was called despite the missing id")
	}
}

func TestUpdateEvent_RejectsUnparseableTime(t *testing.T) {
	f := &fakeClient{}

	if _, _, err := newBridge(f).updateEvent(context.Background(), nil, updateEventInput{Id: "e1", TimeStart: "yesterday"}); err == nil {
		t.Fatal("expected an error for a non-RFC3339 time")
	}

	if f.updateEventReq != nil {
		t.Error("UpdateEvent was called despite the bad timestamp")
	}
}

// get_event asks for the memory count unconditionally and never for the memories: the count is what
// decides whether a model should open the event, and the memories would put every body in its
// context at once.
func TestGetEvent_AsksForCountsNotMemories(t *testing.T) {
	f := &fakeClient{getEventRes: &contract.GetEventResponse{Event: &contract.Event{
		Id:          "e1",
		Name:        "release",
		MemoryCount: 42,
		Links:       []*contract.Link{{Id: "e2", Significance: 5}},
	}}}

	_, out, err := newBridge(f).getEvent(context.Background(), nil, getEventInput{Id: "e1", Links: true})
	if err != nil {
		t.Fatalf("getEvent: %s", err)
	}

	if !f.getEventReq.GetMemoryCounts() {
		t.Error("memory_counts was not requested")
	}

	if f.getEventReq.GetMemories() {
		t.Error("memories was requested - that puts every body of the event into the model's context")
	}

	if out.Event.MemoryCount != 42 || out.Event.Name != "release" {
		t.Errorf("event = %+v, want the name and count projected through", out.Event)
	}

	// Event.links is outbound only, so a projected edge has to say so rather than claim "both".
	if len(out.Links) != 1 || out.Links[0].Direction != "outbound" {
		t.Errorf("links = %+v, want one outbound edge", out.Links)
	}
}

func TestEventLinkTools(t *testing.T) {
	ctx := context.Background()

	t.Run("link", func(t *testing.T) {
		f := &fakeClient{}

		if _, _, err := newBridge(f).linkEvents(ctx, nil, linkEventsInput{
			Id:    "e1",
			Links: []linkViewInput{{Id: "e2", Significance: 5}},
		}); err != nil {
			t.Fatalf("linkEvents: %s", err)
		}

		if f.eventLinkReq.GetId() != "e1" || len(f.eventLinkReq.GetLinks()) != 1 {
			t.Errorf("request = %+v", f.eventLinkReq)
		}
	})

	t.Run("link validates like the memory half", func(t *testing.T) {
		f := &fakeClient{}
		b := newBridge(f)

		for _, in := range []linkEventsInput{
			{Links: []linkViewInput{{Id: "e2"}}},
			{Id: "e1"},
			{Id: "e1", Links: []linkViewInput{{Significance: 5}}},
		} {
			if _, _, err := b.linkEvents(ctx, nil, in); err == nil {
				t.Errorf("linkEvents(%+v) succeeded, want an error", in)
			}
		}

		if f.eventLinkReq != nil {
			t.Error("LinkEvents was called despite invalid input")
		}
	})

	t.Run("unlink", func(t *testing.T) {
		f := &fakeClient{}

		if _, _, err := newBridge(f).unlinkEvents(ctx, nil, unlinkEventsInput{Id: "e1", Ids: []string{"e2"}}); err != nil {
			t.Fatalf("unlinkEvents: %s", err)
		}

		if f.eventUnlinkReq.GetId() != "e1" {
			t.Errorf("request = %+v", f.eventUnlinkReq)
		}

		b := newBridge(&fakeClient{})

		if _, _, err := b.unlinkEvents(ctx, nil, unlinkEventsInput{Id: "e1"}); err == nil {
			t.Error("unlinkEvents with no ids succeeded, want an error")
		}

		if _, _, err := b.unlinkEvents(ctx, nil, unlinkEventsInput{Ids: []string{"e2"}}); err == nil {
			t.Error("unlinkEvents with no id succeeded, want an error")
		}
	})

	t.Run("list", func(t *testing.T) {
		f := &fakeClient{eventLinksRes: &contract.GetLinksResponse{
			Links:            []*contract.LinkEdge{{Id: "e2", Significance: 5, Direction: contract.LinkDirection_LINK_DIRECTION_INBOUND}},
			LinkSignificance: 5,
		}}

		_, out, err := newBridge(f).getEventLinks(ctx, nil, getEventLinksInput{Id: "e1", Direction: "inbound"})
		if err != nil {
			t.Fatalf("getEventLinks: %s", err)
		}

		if f.eventLinksReq.GetDirection() != contract.LinkDirection_LINK_DIRECTION_INBOUND {
			t.Errorf("direction = %v, want INBOUND", f.eventLinksReq.GetDirection())
		}

		if len(out.Links) != 1 || out.Links[0].Direction != "inbound" || out.LinkSignificance != 5 {
			t.Errorf("output = %+v", out)
		}

		if _, _, err := newBridge(&fakeClient{}).getEventLinks(ctx, nil, getEventLinksInput{Id: "e1", Direction: "sideways"}); err == nil {
			t.Error("an unknown direction succeeded; it must be an error rather than a silent fallback")
		}
	})
}

func TestListEvents_NewFilters(t *testing.T) {
	f := &fakeClient{getEventsRes: &contract.GetEventsResponse{}}
	open := false

	if _, _, err := newBridge(f).listEvents(context.Background(), nil, listEventsInput{
		Ended:        &open,
		NameContains: "election",
		LinkedTo:     "e-open",
	}); err != nil {
		t.Fatalf("listEvents: %s", err)
	}

	req := f.getEventsReq

	switch {

	case req.GetEnded() != contract.Bool_FALSE:
		t.Errorf("ended = %v, want FALSE - the input is a *bool precisely so false is expressible", req.GetEnded())

	case req.GetNameContains() != "election":
		t.Errorf("name_contains = %q", req.GetNameContains())

	case req.GetLinkedTo() != "e-open":
		t.Errorf("linked_to = %q", req.GetLinkedTo())

	}

	// Omitted, the tri-state must stay unset rather than becoming "only the ended ones".
	f = &fakeClient{getEventsRes: &contract.GetEventsResponse{}}

	if _, _, err := newBridge(f).listEvents(context.Background(), nil, listEventsInput{}); err != nil {
		t.Fatalf("listEvents: %s", err)
	}

	if f.getEventsReq.GetEnded() != contract.Bool_UNSPECIFIED {
		t.Errorf("ended = %v, want UNSPECIFIED when omitted", f.getEventsReq.GetEnded())
	}
}

func TestSignificanceLevels(t *testing.T) {
	f := &fakeClient{levelsRes: &contract.GetSignificanceLevelsResponse{
		Significances: []int32{3, 5, 9},
		TotalCount:    3,
	}}

	_, out, err := newBridge(f).significanceLevels(context.Background(), nil, significanceLevelsInput{
		SignificanceMin: 3,
		Limit:           50,
	})
	if err != nil {
		t.Fatalf("significanceLevels: %s", err)
	}

	if len(out.Significances) != 3 || out.TotalCount != 3 {
		t.Errorf("output = %+v", out)
	}

	if f.levelsReq.GetSignificanceMin() != 3 || f.levelsReq.GetLimit() != 50 {
		t.Errorf("request = %+v", f.levelsReq)
	}
}

func TestExplainConsolidation(t *testing.T) {
	f := &fakeClient{explainRes: &contract.ExplainConsolidationResponse{
		DeletionThreshold: 0.4,
		CapacityPressure:  1.5,
		MemoryCount:       120,
		Valuations: []*contract.MemoryValuation{{
			Id:                 "m1",
			Value:              0.3,
			Threshold:          0.4,
			WouldConsolidate:   true,
			DaysUntilForgotten: 0,
		}},
	}}

	_, out, err := newBridge(f).explainConsolidation(context.Background(), nil, explainConsolidationInput{Ids: []string{"m1"}})
	if err != nil {
		t.Fatalf("explainConsolidation: %s", err)
	}

	if f.explainReq.GetCurve() != nil {
		t.Error("a curve was requested - it describes the configuration, not any memory, and is up to 500 points of it")
	}

	if len(out.Valuations) != 1 || !out.Valuations[0].WouldConsolidate {
		t.Fatalf("valuations = %+v", out.Valuations)
	}

	if out.DeletionThreshold != 0.4 || out.CapacityPressure != 1.5 || out.MemoryCount != 120 {
		t.Errorf("store-level figures = %+v", out)
	}
}

func TestExplainConsolidation_RequiresIds(t *testing.T) {
	f := &fakeClient{}

	if _, _, err := newBridge(f).explainConsolidation(context.Background(), nil, explainConsolidationInput{}); err == nil {
		t.Fatal("expected an error with no ids")
	}

	if f.explainReq != nil {
		t.Error("ExplainConsolidation was called with no ids")
	}
}

func TestConsolidationStatus(t *testing.T) {
	f := &fakeClient{statusRes: &contract.GetConsolidationStatusResponse{
		ConsolidationEnabled: true,
		PeriodSeconds:        300,
		NextSleepAt:          1234,
		LastCycle: &contract.CycleReport{
			StartedAt:            1000,
			MemoriesConsolidated: 7,
			MemoriesEvicted:      2,
			Success:              true,
		},
	}}

	_, out, err := newBridge(f).consolidationStatus(context.Background(), nil, struct{}{})
	if err != nil {
		t.Fatalf("consolidationStatus: %s", err)
	}

	switch {

	case !out.ConsolidationEnabled || out.PeriodSeconds != 300 || out.NextSleepAt != 1234:
		t.Errorf("schedule = %+v", out)

	case out.MemoriesConsolidated != 7 || out.MemoriesEvicted != 2 || !out.LastCycleSucceeded:
		t.Errorf("last cycle = %+v", out)

	}
}

// A replica has never run a cycle, so last_cycle is nil. The projection must read through that
// rather than panic - which is exactly the deployment an agent is most likely to be pointed at.
func TestConsolidationStatus_NoCycleYet(t *testing.T) {
	f := &fakeClient{statusRes: &contract.GetConsolidationStatusResponse{ConsolidationEnabled: false}}

	_, out, err := newBridge(f).consolidationStatus(context.Background(), nil, struct{}{})
	if err != nil {
		t.Fatalf("consolidationStatus: %s", err)
	}

	if out.ConsolidationEnabled || out.LastCycleAt != 0 {
		t.Errorf("output = %+v, want a replica reporting no cycle", out)
	}
}

func TestWhoAmI(t *testing.T) {
	f := &fakeClient{whoAmIRes: &contract.WhoAmIResponse{
		Role:        "writer",
		ClientId:    "agent-1",
		AuthEnabled: true,
		Groups:      []string{"apollo"},
		GroupScoped: true,
		Version:     "v1.2.3",
		SearchModes: []contract.SearchMode{
			contract.SearchMode_SEARCH_MODE_KEYWORD,
			contract.SearchMode_SEARCH_MODE_HYBRID,
		},
		ConsolidationEnabled: true,
	}}

	_, out, err := newBridge(f).whoAmI(context.Background(), nil, struct{}{})
	if err != nil {
		t.Fatalf("whoAmI: %s", err)
	}

	if out.Role != "writer" || out.ClientId != "agent-1" || out.Version != "v1.2.3" {
		t.Errorf("output = %+v", out)
	}

	if !out.GroupScoped || len(out.Groups) != 1 {
		t.Errorf("scope = %v/%v", out.GroupScoped, out.Groups)
	}

	// The mode names must be the strings search_memories' own mode input takes, or feature detection
	// hands a model a value it cannot pass back.
	if len(out.SearchModes) != 2 || out.SearchModes[0] != "keyword" || out.SearchModes[1] != "hybrid" {
		t.Errorf("search_modes = %v, want the names search_memories accepts", out.SearchModes)
	}
}

// An unset enum value must not become a mode name a model would then pass to search_memories.
func TestSearchModeNames_SkipsUnspecified(t *testing.T) {
	got := searchModeNames([]contract.SearchMode{
		contract.SearchMode_SEARCH_MODE_UNSPECIFIED,
		contract.SearchMode_SEARCH_MODE_SEMANTIC,
	})

	if len(got) != 1 || got[0] != "semantic" {
		t.Errorf("searchModeNames = %v, want [semantic]", got)
	}
}
