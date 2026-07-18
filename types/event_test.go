package types

import (
	"math"
	"strings"
	"testing"

	"github.com/fastbean-au/hippocampus/contract"
)

// TestRelationshipsFromProto verifies relationships map field-for-field and an empty input yields an
// empty (non-nil) slice.
func TestRelationshipsFromProto(t *testing.T) {
	in := []*contract.Relationship{
		{EventId: "a", Significance: 1},
		{EventId: "b", Significance: 2},
	}

	out := RelationshipsFromProto(in)

	if len(out) != 2 || out[0].EventId != "a" || out[0].Significance != 1 ||
		out[1].EventId != "b" || out[1].Significance != 2 {
		t.Errorf("unexpected relationships: %+v", out)
	}

	if RelationshipsFromProto(nil) == nil {
		t.Error("expected a non-nil empty slice for nil input")
	}
}

// TestRelationshipToProto verifies a single relationship converts to its proto form.
func TestRelationshipToProto(t *testing.T) {
	p := (&Relationship{EventId: "x", Significance: 9}).ToProto()

	if p.GetEventId() != "x" || p.GetSignificance() != 9 {
		t.Errorf("unexpected proto relationship: %+v", p)
	}
}

// TestEventFromProto_RoundTrip verifies an event, including its nested relationships, survives a
// proto->struct->proto trip.
func TestEventFromProto_RoundTrip(t *testing.T) {
	p := &contract.Event{
		Id:                       "e1",
		TimeStart:                10,
		TimeEnd:                  20,
		Significance:             5,
		Name:                     "n",
		Description:              "d",
		MemoriesConsolidated:     true,
		RelationshipSignificance: 3,
		Relationships:            []*contract.Relationship{{EventId: "r1", Significance: 3}},
		Group:                    "g",
	}

	e := EventFromProto(p)

	if e.Id != "e1" || e.TimeStart != 10 || e.TimeEnd != 20 || e.Significance != 5 ||
		e.Name != "n" || e.Description != "d" || e.Group != "g" || len(e.Relationships) != 1 {
		t.Errorf("field mismatch after EventFromProto: %+v", e)
	}

	// RelationshipSignificance and MemoriesConsolidated are populated by the store, not FromProto,
	// so set them to assert ToProto carries them through.
	e.RelationshipSignificance = 3
	e.MemoriesConsolidated = true

	back := e.ToProto()

	if back.GetId() != "e1" || back.GetTimeStart() != 10 || back.GetTimeEnd() != 20 ||
		back.GetSignificance() != 5 || back.GetName() != "n" || back.GetDescription() != "d" ||
		back.GetGroup() != "g" || !back.GetMemoriesConsolidated() ||
		back.GetRelationshipSignificance() != 3 || len(back.GetRelationships()) != 1 {
		t.Errorf("field mismatch after ToProto: %+v", back)
	}
}

// TestEventValidate_UpdateEmptyIdMessage is a regression test: the update-arm
// empty-id case returned the significance error message by copy/paste; it must name the id.
func TestEventValidate_UpdateEmptyIdMessage(t *testing.T) {
	e := Event{}

	err := e.Validate(true)
	if err == nil {
		t.Fatal("expected an error for an empty id on update")
	}

	if !strings.Contains(err.Error(), "id must be provided") {
		t.Errorf("expected the id-required message, got %q", err.Error())
	}
}

// TestEventValidate_TimeEndBeforeTimeStart is a regression test: an event whose
// time_end precedes its time_start (both supplied) is invalid on both the create and update arms,
// while an unset (zero) time_end - the common "still open" case - stays valid.
func TestEventValidate_TimeEndBeforeTimeStart(t *testing.T) {
	backwards := Event{Id: "e1", Name: "backwards", Significance: 5, TimeStart: 200, TimeEnd: 100}

	for _, update := range []bool{false, true} {
		if err := backwards.Validate(update); err == nil {
			t.Errorf("Validate(update=%v) accepted time_end before time_start", update)
		} else if !strings.Contains(err.Error(), "TimeEnd must not be before TimeStart") {
			t.Errorf("Validate(update=%v) returned the wrong message: %q", update, err.Error())
		}
	}

	// A zero time_end (event still open) must remain valid.
	open := Event{Id: "e2", Name: "open", Significance: 5, TimeStart: 200}
	if err := open.Validate(false); err != nil {
		t.Errorf("an event with an unset time_end should be valid, got: %s", err)
	}

	// A well-ordered pair is valid.
	ordered := Event{Id: "e3", Name: "ordered", Significance: 5, TimeStart: 100, TimeEnd: 200}
	if err := ordered.Validate(false); err != nil {
		t.Errorf("a well-ordered event should be valid, got: %s", err)
	}
}

// TestEventValidate covers each validation branch across the create and update arms.
func TestEventValidate(t *testing.T) {
	longId := strings.Repeat("i", 129)
	longName := strings.Repeat("n", 257)
	longDesc := strings.Repeat("d", 1025)
	longGroup := strings.Repeat("g", 129)

	cases := []struct {
		name    string
		e       Event
		update  bool
		wantErr string // empty means expect no error
	}{
		{"insert valid", Event{Name: "n", Significance: 1, TimeStart: 1}, false, ""},
		{"insert unranked significance", Event{Name: "n", TimeStart: 1}, false, ""},
		{"insert negative significance", Event{Name: "n", Significance: -1, TimeStart: 1}, false, "significance must not be < 0"},
		{"insert no name", Event{Significance: 1, TimeStart: 1}, false, "no name provided"},
		{"insert bad timestart", Event{Name: "n", Significance: 1, TimeStart: 0}, false, "TimeStart must be > 0"},
		{"insert negative timeend", Event{Name: "n", Significance: 1, TimeStart: 1, TimeEnd: -1}, false, "TimeEnd must be > 0"},
		{"id too long", Event{Id: longId, Name: "n", Significance: 1, TimeStart: 1}, false, "id too long"},
		{"name too long", Event{Name: longName, Significance: 1, TimeStart: 1}, false, "name too long"},
		{"description too long", Event{Name: "n", Description: longDesc, Significance: 1, TimeStart: 1}, false, "description too long"},
		{"group too long", Event{Name: "n", Group: longGroup, Significance: 1, TimeStart: 1}, false, "group too long"},
		{"update valid", Event{Id: "e1", Significance: 0}, true, ""},
		{"update no id", Event{Significance: 1}, true, "id must be provided"},
		{"update negative significance", Event{Id: "e1", Significance: -1}, true, "significance must not be < 0"},
		{"update negative timestart", Event{Id: "e1", TimeStart: -1}, true, "TimeStart must not be < 0"},
		{"update negative timeend", Event{Id: "e1", TimeEnd: -1}, true, "TimeEnd must not be < 0"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.e.Validate(c.update)

			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("expected no error, got %q", err.Error())
				}

				return
			}

			if err == nil {
				t.Fatalf("expected error containing %q, got nil", c.wantErr)
			}

			if !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("expected error containing %q, got %q", c.wantErr, err.Error())
			}
		})
	}
}

// TestEventSetDefaults verifies an id and time_start are filled when absent and preserved otherwise.
func TestEventSetDefaults(t *testing.T) {
	e := Event{}
	e.SetDefaults()

	if e.Id == "" {
		t.Error("expected a generated id")
	}

	if e.TimeStart == 0 {
		t.Error("expected a generated time_start")
	}

	fixed := Event{Id: "keep", TimeStart: 42}
	fixed.SetDefaults()

	if fixed.Id != "keep" || fixed.TimeStart != 42 {
		t.Errorf("SetDefaults overwrote supplied values: %+v", fixed)
	}
}

// TestCalculateRelationshipSignificance_NoInt32Overflow verifies the sum accumulates in int64:
// relationships whose int32 significances sum past math.MaxInt32 must not wrap negative.
// Under the previous int32 accumulator this wrapped.
func TestCalculateRelationshipSignificance_NoInt32Overflow(t *testing.T) {
	e := Event{
		Relationships: []Relationship{
			{EventId: "a", Significance: math.MaxInt32},
			{EventId: "b", Significance: math.MaxInt32},
			{EventId: "c", Significance: math.MaxInt32},
		},
	}

	got := e.CalculateRelationshipSignificance()
	want := int64(math.MaxInt32) * 3

	if got != want {
		t.Errorf("expected %d, got %d (int32 overflow wrap?)", want, got)
	}
}
