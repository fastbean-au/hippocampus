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
