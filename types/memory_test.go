package types

import (
	"strings"
	"testing"
	"time"

	"github.com/fastbean-au/hippocampus/contract"
)

// TestMemoryFromProto_RoundTrip verifies that a proto memory maps field-for-field onto the
// internal struct and that the tri-state is_binary enum collapses to the bool correctly.
func TestMemoryFromProto_RoundTrip(t *testing.T) {
	p := &contract.Memory{
		Id:           "m1",
		TimeStamp:    123,
		Significance: 7,
		EventId:      "e1",
		Body:         "body",
		IsBinary:     contract.Bool_TRUE,
		TimeRecalled: 456,
		RecallCount:  3,
		IsSummary:    true,
		Group:        "g1",
	}

	m := MemoryFromProto(p)

	if m.Id != "m1" || m.TimeStamp != 123 || m.Significance != 7 || m.EventId != "e1" ||
		m.Body != "body" || m.TimeRecalled != 456 || m.RecallCount != 3 || m.Group != "g1" {
		t.Errorf("field mismatch after MemoryFromProto: %+v", m)
	}

	if !m.IsBinary {
		t.Error("expected IsBinary true for Bool_TRUE")
	}

	if !m.IsSummary {
		t.Error("expected IsSummary true")
	}
}

// TestMemoryFromProto_NonBinary verifies that Bool_FALSE (and the zero value) map to IsBinary false.
func TestMemoryFromProto_NonBinary(t *testing.T) {
	if MemoryFromProto(&contract.Memory{IsBinary: contract.Bool_FALSE}).IsBinary {
		t.Error("Bool_FALSE should map to IsBinary false")
	}

	if MemoryFromProto(&contract.Memory{}).IsBinary {
		t.Error("unset is_binary should map to IsBinary false")
	}
}

// TestMemoryToProto_Binary checks the bool is expanded back to the correct tri-state enum in both
// directions.
func TestMemoryToProto_Binary(t *testing.T) {
	if got := (&Memory{IsBinary: true}).ToProto().IsBinary; got != contract.Bool_TRUE {
		t.Errorf("expected Bool_TRUE, got %v", got)
	}

	if got := (&Memory{IsBinary: false}).ToProto().IsBinary; got != contract.Bool_FALSE {
		t.Errorf("expected Bool_FALSE, got %v", got)
	}
}

// TestMemoryToProto_RoundTrip confirms every scalar field survives a struct->proto->struct trip.
func TestMemoryToProto_RoundTrip(t *testing.T) {
	m := Memory{
		Id:           "m2",
		TimeStamp:    99,
		Significance: 4,
		EventId:      "e2",
		Body:         "content",
		IsBinary:     true,
		TimeRecalled: 88,
		RecallCount:  2,
		IsSummary:    true,
		Group:        "g2",
	}

	back := MemoryFromProto(m.ToProto())

	if back != m {
		t.Errorf("round-trip mismatch:\n got %+v\nwant %+v", back, m)
	}
}

// TestMemoryValidateInsert covers each validation branch for both the insert and update arms.
func TestMemoryValidateInsert(t *testing.T) {
	future := time.Now().UnixNano() + (time.Hour).Nanoseconds()
	longBody := strings.Repeat("x", 11)
	longId := strings.Repeat("i", 129)
	longGroup := strings.Repeat("g", 129)

	cases := []struct {
		name    string
		m       Memory
		maxLen  int
		update  bool
		wantErr string // empty means expect no error
	}{
		{"insert valid", Memory{Significance: 1, Body: "b"}, 0, false, ""},
		{"insert no significance", Memory{Body: "b"}, 0, false, "significance must be > 0"},
		{"insert no body", Memory{Significance: 1}, 0, false, "no body provided"},
		{"insert body too long", Memory{Significance: 1, Body: longBody}, 10, false, "body too long"},
		{"insert id too long", Memory{Significance: 1, Body: "b", Id: longId}, 0, false, "id too long"},
		{"insert group too long", Memory{Significance: 1, Body: "b", Group: longGroup}, 0, false, "group too long"},
		{"insert negative timestamp", Memory{Significance: 1, Body: "b", TimeStamp: -1}, 0, false, "timestamp must not be < 0"},
		{"insert future timestamp", Memory{Significance: 1, Body: "b", TimeStamp: future}, 0, false, "too far in the future"},
		{"update valid", Memory{Id: "m1", Significance: 0}, 0, true, ""},
		{"update no id", Memory{Significance: 1}, 0, true, "id must be provided"},
		{"update negative significance", Memory{Id: "m1", Significance: -1}, 0, true, "significance must not be < 0"},
		{"update future timestamp", Memory{Id: "m1", TimeStamp: future}, 0, true, "too far in the future"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.m.ValidateInsert(c.maxLen, c.update)

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

// TestMemoryValidateInsert_ClockSkewBoundary verifies a timestamp within the allowed skew window is
// accepted while one just beyond it is rejected.
func TestMemoryValidateInsert_ClockSkewBoundary(t *testing.T) {
	withinSkew := Memory{Significance: 1, Body: "b", TimeStamp: time.Now().Add(time.Minute).UnixNano()}
	if err := withinSkew.ValidateInsert(0, false); err != nil {
		t.Errorf("timestamp within clock skew should be valid, got %q", err.Error())
	}

	beyondSkew := Memory{Significance: 1, Body: "b", TimeStamp: time.Now().Add(10 * time.Minute).UnixNano()}
	if err := beyondSkew.ValidateInsert(0, false); err == nil {
		t.Error("timestamp beyond clock skew should be rejected")
	}
}

// TestMemorySetDefaults verifies an id and timestamp are filled when absent and preserved otherwise.
func TestMemorySetDefaults(t *testing.T) {
	m := Memory{}
	m.SetDefaults()

	if m.Id == "" {
		t.Error("expected a generated id")
	}

	if m.TimeStamp == 0 {
		t.Error("expected a generated timestamp")
	}

	fixed := Memory{Id: "keep", TimeStamp: 42}
	fixed.SetDefaults()

	if fixed.Id != "keep" || fixed.TimeStamp != 42 {
		t.Errorf("SetDefaults overwrote supplied values: %+v", fixed)
	}
}
