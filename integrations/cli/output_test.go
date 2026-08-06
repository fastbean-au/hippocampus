package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/fastbean-au/hippocampus/contract"
)

func TestRenderJSON(t *testing.T) {
	var buf bytes.Buffer

	r := &renderer{out: &buf, json: true}

	if err := r.render(&contract.StoreMemoryResponse{Id: "m1"}); err != nil {
		t.Fatalf("render: %v", err)
	}

	// protojson intentionally varies the space after the colon, so match the key and value
	// independently rather than the exact spacing.
	if !strings.Contains(buf.String(), `"id"`) || !strings.Contains(buf.String(), `"m1"`) {
		t.Fatalf("json output = %q", buf.String())
	}
}

func TestRenderTextStoreMemoryRejected(t *testing.T) {
	var buf bytes.Buffer

	r := &renderer{out: &buf}

	if err := r.render(&contract.StoreMemoryResponse{Rejected: true}); err != nil {
		t.Fatalf("render: %v", err)
	}

	if !strings.Contains(buf.String(), "rejected") {
		t.Fatalf("output = %q", buf.String())
	}
}

func TestRenderTextMemories(t *testing.T) {
	var buf bytes.Buffer

	r := &renderer{out: &buf}

	resp := &contract.GetMemoriesResponse{
		TotalCount: 2,
		Memories: []*contract.Memory{
			{Id: "m1", Body: "visible text", Significance: 3},
			{Id: "m2", Body: "rawbytes", IsBinary: contract.Bool_TRUE},
		},
	}

	if err := r.render(resp); err != nil {
		t.Fatalf("render: %v", err)
	}

	out := buf.String()

	if !strings.Contains(out, "visible text") {
		t.Fatalf("expected the text body, got %q", out)
	}

	if strings.Contains(out, "rawbytes") {
		t.Fatalf("a binary body must not be printed verbatim: %q", out)
	}

	if !strings.Contains(out, "<binary,") {
		t.Fatalf("expected a binary placeholder, got %q", out)
	}
}

// TestRenderTextPreview covers the dry run's text form: the counts, the two rules named in terms
// an operator can act on, and the truncation note that stops a short sample being read as the whole
// of what would be forgotten.
func TestRenderTextPreview(t *testing.T) {
	var buf bytes.Buffer

	r := &renderer{out: &buf}

	preview := &contract.PreviewConsolidationResponse{
		MemoriesConsolidated: 3,
		MemoriesEvicted:      2,
		EventsDeleted:        1,
		BytesFreed:           4096,
		MemoriesRetained:     7,
		RetainedBytes:        8192,
		CapacityPressure:     1.25,
		DeletionThreshold:    12.5,
		UsedBytes:            1000,
		CapacityBytes:        2000,
		Truncated:            true,
		Candidates: []*contract.ForgetCandidate{
			{Id: "m1", Value: 0.5, Significance: 1, Rule: contract.ForgetRule_FORGET_RULE_CONSOLIDATION, Group: "logs"},
			{Id: "m2", Value: 9.5, Significance: 8, Rule: contract.ForgetRule_FORGET_RULE_EVICTION},
		},
	}

	if err := r.render(preview); err != nil {
		t.Fatalf("render: %v", err)
	}

	out := buf.String()

	for _, want := range []string{
		"would forget 5 memory/memories and 1 event(s)",
		"consolidated (decayed below the threshold): 3",
		"evicted (over the byte capacity):           2",
		"retained by the minimum retention floor:    7 (8192 bytes)",
		"used / capacity:    1000 / 2000 bytes",
		"truncated",
		"m1",
		"decayed",
		"m2",
		"capacity",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

// TestRenderTextPreviewWithoutCapacity covers the other branch of the byte-capacity line, which
// has to explain why nothing is evicted rather than print a bare zero.
func TestRenderTextPreviewWithoutCapacity(t *testing.T) {
	var buf bytes.Buffer

	r := &renderer{out: &buf}

	if err := r.render(&contract.PreviewConsolidationResponse{UsedBytes: 500}); err != nil {
		t.Fatalf("render: %v", err)
	}

	out := buf.String()

	if !strings.Contains(out, "no byte capacity configured") {
		t.Errorf("output = %q", out)
	}

	// Nothing to show, so no sample header either.
	if strings.Contains(out, "least valuable first") {
		t.Errorf("empty candidate list produced a sample header: %q", out)
	}
}

func TestRenderTextWhoAmI(t *testing.T) {
	var buf bytes.Buffer

	r := &renderer{out: &buf}

	if err := r.render(&contract.WhoAmIResponse{ClientId: "c1", Role: "admin", AuthEnabled: true}); err != nil {
		t.Fatalf("render: %v", err)
	}

	out := buf.String()

	if !strings.Contains(out, "client_id:    c1") || !strings.Contains(out, "role:         admin") {
		t.Fatalf("output = %q", out)
	}
}

func TestFormatNanos(t *testing.T) {
	if got := formatNanos(0); got != "-" {
		t.Fatalf("formatNanos(0) = %q, want -", got)
	}

	if got := formatNanos(1_000_000_000); got == "-" {
		t.Fatalf("formatNanos of a real timestamp should not be a dash")
	}
}

// TestRenderTextExplanation covers the explanation's text form: the two sides of the comparison, the
// projection, the override that would otherwise make a low value look like a contradiction, and the
// curve's crossing.
func TestRenderTextExplanation(t *testing.T) {
	var buf bytes.Buffer

	r := &renderer{out: &buf}

	explanation := &contract.ExplainConsolidationResponse{
		CapacityPressure:  1.5,
		DeletionThreshold: 15,
		UsedBytes:         1000,
		CapacityBytes:     2000,
		Method:            1,
		Aggressiveness:    1,
		UnitsOfAgeInDays:  1,
		Valuations: []*contract.MemoryValuation{
			{Id: "m1", Significance: 3, EffectiveSignificance: 4.5, Value: 0.5, Threshold: 15, AgeDays: 9, DaysUntilForgotten: 0, WouldConsolidate: true},
			{Id: "m2", Significance: 8, EffectiveSignificance: 8, Value: 40, Threshold: 15, AgeDays: 0.2, DaysUntilForgotten: 3.5, Retained: true},
		},
		Curve: &contract.DecayCurve{
			Significance:    10,
			MaxAgeDays:      15,
			CrossingAgeDays: 10,
			Points:          []*contract.DecayPoint{{AgeDays: 1, Value: 10}, {AgeDays: 15, Value: 0.66}},
		},
	}

	if err := r.render(explanation); err != nil {
		t.Fatalf("render: %v", err)
	}

	out := buf.String()

	for _, want := range []string{
		"memory m1",
		"value / threshold:  0.5 / 15",
		"a cycle running now would forget it",
		"forgotten in:       due now",
		"3 stored, 4.5 effective",
		"retained: inside the minimum retention window",
		"~3.50 day(s)",
		"capacity pressure:  1.500",
		"used / capacity:    1000 / 2000 bytes",
		"crosses the threshold at 10.00 day(s)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

// TestRenderTextExplanationEdges covers the branches the happy path does not reach: no byte
// capacity, a curve that never crosses, and the deferred-by-minimum-age note.
func TestRenderTextExplanationEdges(t *testing.T) {
	var buf bytes.Buffer

	r := &renderer{out: &buf}

	explanation := &contract.ExplainConsolidationResponse{
		UsedBytes: 500,
		Valuations: []*contract.MemoryValuation{
			{Id: "m1", Value: 0.1, Threshold: 1, BelowMinimumAge: true, DaysUntilForgotten: -1},
		},
		Curve: &contract.DecayCurve{Significance: 1e6, MaxAgeDays: 30, CrossingAgeDays: -1},
	}

	if err := r.render(explanation); err != nil {
		t.Fatalf("render: %v", err)
	}

	out := buf.String()

	for _, want := range []string{
		"below the minimum age",
		"not within any projected timeframe",
		"no byte capacity configured",
		"does not cross the threshold",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}
