package main

import (
	"bytes"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/fastbean-au/hippocampus/contract"
)

// renderToString runs the text renderer over one message, for the two rendering cases below.
func renderToString(t *testing.T, msg proto.Message) string {
	t.Helper()

	var buf bytes.Buffer

	if err := (&renderer{out: &buf}).render(msg); err != nil {
		t.Fatalf("render: %s", err)
	}

	return buf.String()
}

// The client half of TODO-2 item 94, plus the significance registry: the commands and flags that
// reach the RPCs Batch A added.

func TestEventUpdate(t *testing.T) {
	req, _, err := runCommand(t, "event update", []string{
		"--id", "e1",
		"--name", "corrected",
		"--description", "why",
		"--group", "news",
		"--metadata", "outlet=abc",
		"--significance", "7",
	}, &fakeClient{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	event, ok := req.(*contract.Event)
	if !ok {
		t.Fatalf("captured %T, want *contract.Event", req)
	}

	switch {

	case event.GetId() != "e1":
		t.Errorf("id = %q, want e1", event.GetId())

	case event.GetName() != "corrected":
		t.Errorf("name = %q, want corrected", event.GetName())

	case event.GetDescription() != "why":
		t.Errorf("description = %q, want why", event.GetDescription())

	case event.GetGroup() != "news":
		t.Errorf("group = %q, want news", event.GetGroup())

	case event.GetSignificance() != 7:
		t.Errorf("significance = %d, want 7", event.GetSignificance())

	case event.GetMetadata()["outlet"] != "abc":
		t.Errorf("metadata = %v, want outlet=abc", event.GetMetadata())

	}
}

// The two clearing flags are the whole reason an empty --group cannot mean "unset it", so they get
// their own case: without them the update would read as "leave unchanged" and silently do nothing.
func TestEventUpdate_Clears(t *testing.T) {
	req, _, err := runCommand(t, "event update", []string{
		"--id", "e1", "--clear-group", "--clear-metadata",
	}, &fakeClient{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	event := req.(*contract.Event)

	if !event.GetClearGroup() || !event.GetClearMetadata() {
		t.Errorf("clear_group/clear_metadata = %v/%v, want both true", event.GetClearGroup(), event.GetClearMetadata())
	}
}

func TestEventUpdate_RequiresId(t *testing.T) {
	_, _, err := runCommand(t, "event update", []string{"--name", "no id"}, &fakeClient{})

	if err == nil || !strings.Contains(err.Error(), "--id") {
		t.Fatalf("err = %v, want a complaint about --id", err)
	}
}

// A create must still refuse a missing name after the flag sets were shared with the update, where
// name is optional.
func TestEventCreate_StillRequiresName(t *testing.T) {
	_, _, err := runCommand(t, "event create", []string{"--significance", "5"}, &fakeClient{})

	if err == nil || !strings.Contains(err.Error(), "--name") {
		t.Fatalf("err = %v, want a complaint about --name", err)
	}
}

func TestEventList_NewFilters(t *testing.T) {
	req, _, err := runCommand(t, "event list", []string{
		"--ended", "false",
		"--name-contains", "election",
		"--linked-to", "e-open",
		"--links",
	}, &fakeClient{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	list, ok := req.(*contract.GetEventsRequest)
	if !ok {
		t.Fatalf("captured %T, want *contract.GetEventsRequest", req)
	}

	switch {

	case list.GetEnded() != contract.Bool_FALSE:
		t.Errorf("ended = %v, want FALSE - the tri-state is a string flag precisely so false is expressible", list.GetEnded())

	case list.GetNameContains() != "election":
		t.Errorf("name_contains = %q, want election", list.GetNameContains())

	case list.GetLinkedTo() != "e-open":
		t.Errorf("linked_to = %q, want e-open", list.GetLinkedTo())

	case !list.GetLinks():
		t.Error("links = false, want true")

	}
}

func TestEventList_EndedUnsetStaysUnspecified(t *testing.T) {
	req, _, err := runCommand(t, "event list", nil, &fakeClient{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	if got := req.(*contract.GetEventsRequest).GetEnded(); got != contract.Bool_UNSPECIFIED {
		t.Errorf("ended = %v, want UNSPECIFIED when the flag is absent", got)
	}
}

func TestEventGet_Links(t *testing.T) {
	req, _, err := runCommand(t, "event get", []string{"--id", "e1", "--links"}, &fakeClient{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	if !req.(*contract.GetEventByIdRequest).GetLinks() {
		t.Error("links = false, want true")
	}
}

func TestSignificanceLevels(t *testing.T) {
	fake := &fakeClient{}

	req, out, err := runCommand(t, "significance levels", []string{
		"--significance-min", "3", "--significance-max", "9", "--limit", "50",
	}, fake)
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	levels, ok := req.(*contract.GetSignificanceLevelsRequest)
	if !ok {
		t.Fatalf("captured %T, want *contract.GetSignificanceLevelsRequest", req)
	}

	if levels.GetSignificanceMin() != 3 || levels.GetSignificanceMax() != 9 || levels.GetLimit() != 50 {
		t.Errorf("unexpected request: %+v", levels)
	}

	if out == "" {
		t.Error("nothing rendered")
	}
}

// The renderer puts the values on one line, which is the point: adjacent values - the situation a
// placement opens a gap between - are only visible beside one another.
func TestRenderSignificanceLevels(t *testing.T) {
	out := renderToString(t, &contract.GetSignificanceLevelsResponse{
		Significances: []int32{3, 4, 9},
		TotalCount:    3,
	})

	if !strings.Contains(out, "3, 4, 9") {
		t.Errorf("rendered %q, want the values on one line", out)
	}
}

func TestRenderWhoAmI_VersionAndCallbacks(t *testing.T) {
	out := renderToString(t, &contract.WhoAmIResponse{
		Role:             "admin",
		Version:          "v1.2.3",
		CallbacksEnabled: true,
	})

	for _, want := range []string{"v1.2.3", "callbacks:"} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered %q, want it to carry %q", out, want)
		}
	}
}
