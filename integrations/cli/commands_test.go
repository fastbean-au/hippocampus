package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/pflag"
	"google.golang.org/protobuf/proto"

	"github.com/fastbean-au/hippocampus/contract"
)

// runCommand builds the named command's flag set, parses args into it, and runs its handler against
// fake, returning the request the fake captured plus the rendered text output.
func runCommand(t *testing.T, key string, args []string, fake *fakeClient) (proto.Message, string, error) {
	t.Helper()

	cmd, ok := commands()[key]
	if !ok {
		t.Fatalf("no such command %q", key)
	}

	fs := pflag.NewFlagSet(key, pflag.ContinueOnError)
	registerGlobalFlags(fs)
	cmd.flags(fs)

	if err := fs.Parse(args); err != nil {
		return nil, "", err
	}

	var buf bytes.Buffer

	err := cmd.run(context.Background(), fake, fs, &renderer{out: &buf})

	return fake.req, buf.String(), err
}

func TestMemoryStore(t *testing.T) {
	req, out, err := runCommand(t, "memory store", []string{"--body", "hi", "--significance", "5", "--group", "svc", "--binary"}, &fakeClient{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	memory, ok := req.(*contract.Memory)
	if !ok {
		t.Fatalf("captured %T, want *contract.Memory", req)
	}

	if memory.GetBody() != "hi" || memory.GetSignificance() != 5 || memory.GetGroup() != "svc" {
		t.Fatalf("unexpected memory: %+v", memory)
	}

	if memory.GetIsBinary() != contract.Bool_TRUE {
		t.Fatalf("is_binary = %v, want TRUE", memory.GetIsBinary())
	}

	if !strings.Contains(out, "stored memory m-new") {
		t.Fatalf("output = %q", out)
	}
}

func TestMemoryStoreRequiresBody(t *testing.T) {
	_, _, err := runCommand(t, "memory store", []string{"--significance", "5"}, &fakeClient{})
	if err == nil || !strings.Contains(err.Error(), "body is required") {
		t.Fatalf("err = %v, want a body-required error", err)
	}
}

func TestMemoryStoreBodyFromFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "body.txt")

	if err := os.WriteFile(path, []byte("from a file"), 0o600); err != nil {
		t.Fatal(err)
	}

	req, _, err := runCommand(t, "memory store", []string{"--body-file", path}, &fakeClient{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	if req.(*contract.Memory).GetBody() != "from a file" {
		t.Fatalf("body = %q", req.(*contract.Memory).GetBody())
	}
}

func TestMemoryListFiltersAndExtremum(t *testing.T) {
	req, _, err := runCommand(t, "memory list", []string{"--significance-min", "3", "--group", "g", "--limit", "50", "--extremum", "lowest"}, &fakeClient{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	got := req.(*contract.GetMemoriesRequest)

	if got.GetSignificanceMin() != 3 || got.GetGroup() != "g" || got.GetLimit() != 50 {
		t.Fatalf("unexpected request: %+v", got)
	}

	if got.GetSignificanceExtremum() != contract.SignificanceExtremum_SIGNIFICANCE_EXTREMUM_LOWEST {
		t.Fatalf("extremum = %v", got.GetSignificanceExtremum())
	}
}

func TestMemoryListBadExtremum(t *testing.T) {
	_, _, err := runCommand(t, "memory list", []string{"--extremum", "sideways"}, &fakeClient{})
	if err == nil || !strings.Contains(err.Error(), "invalid --extremum") {
		t.Fatalf("err = %v", err)
	}
}

func TestMemoryDeleteIdsFromFlagAndArgs(t *testing.T) {
	req, _, err := runCommand(t, "memory delete", []string{"--id", "a", "--id", "b", "c"}, &fakeClient{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	ids := req.(*contract.DeleteMemoriesRequest).GetIds()
	if strings.Join(ids, ",") != "a,b,c" {
		t.Fatalf("ids = %v, want [a b c]", ids)
	}
}

func TestMemoryDeleteRequiresIds(t *testing.T) {
	_, _, err := runCommand(t, "memory delete", nil, &fakeClient{})
	if err == nil || !strings.Contains(err.Error(), "at least one memory id") {
		t.Fatalf("err = %v", err)
	}
}

func TestMemorySearchRequiresQuery(t *testing.T) {
	_, _, err := runCommand(t, "memory search", nil, &fakeClient{})
	if err == nil || !strings.Contains(err.Error(), "--query is required") {
		t.Fatalf("err = %v", err)
	}
}

func TestMemoryUpdateWithPlacement(t *testing.T) {
	args := []string{"--id", "m1", "--body", "new", "--place-mode", "between", "--place-anchor", "5", "--place-upper", "6"}

	req, _, err := runCommand(t, "memory update", args, &fakeClient{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	memory := req.(*contract.Memory)

	if memory.GetId() != "m1" || memory.GetBody() != "new" {
		t.Fatalf("unexpected memory: %+v", memory)
	}

	place := memory.GetPlacement()
	if place.GetMode() != contract.SignificancePlacement_BETWEEN || place.GetAnchor() != 5 || place.GetUpper() != 6 {
		t.Fatalf("unexpected placement: %+v", place)
	}
}

func TestPlacementBadMode(t *testing.T) {
	_, _, err := runCommand(t, "memory store", []string{"--body", "x", "--place-mode", "diagonal"}, &fakeClient{})
	if err == nil || !strings.Contains(err.Error(), "invalid --place-mode") {
		t.Fatalf("err = %v", err)
	}
}

func TestEventCreate(t *testing.T) {
	args := []string{"--name", "deploy", "--significance", "8", "--relationship", "e2:3", "--relationship", "e3:1", "--time-start", "2026-01-02T15:04:05Z"}

	req, out, err := runCommand(t, "event create", args, &fakeClient{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	event := req.(*contract.Event)

	if event.GetName() != "deploy" || event.GetSignificance() != 8 {
		t.Fatalf("unexpected event: %+v", event)
	}

	if len(event.GetRelationships()) != 2 || event.GetRelationships()[0].GetEventId() != "e2" || event.GetRelationships()[0].GetSignificance() != 3 {
		t.Fatalf("unexpected relationships: %+v", event.GetRelationships())
	}

	if event.GetTimeStart() == 0 {
		t.Fatal("time_start should have been parsed from RFC3339")
	}

	if !strings.Contains(out, "stored event e-new") {
		t.Fatalf("output = %q", out)
	}
}

func TestEventCreateRequiresName(t *testing.T) {
	_, _, err := runCommand(t, "event create", nil, &fakeClient{})
	if err == nil || !strings.Contains(err.Error(), "--name is required") {
		t.Fatalf("err = %v", err)
	}
}

func TestEventCreateBadRelationship(t *testing.T) {
	_, _, err := runCommand(t, "event create", []string{"--name", "x", "--relationship", "no-colon"}, &fakeClient{})
	if err == nil || !strings.Contains(err.Error(), "invalid --relationship") {
		t.Fatalf("err = %v", err)
	}
}

func TestEventCreateBadTime(t *testing.T) {
	_, _, err := runCommand(t, "event create", []string{"--name", "x", "--time-start", "yesterday"}, &fakeClient{})
	if err == nil || !strings.Contains(err.Error(), "want RFC3339") {
		t.Fatalf("err = %v", err)
	}
}

func TestEventMergeRequiresBoth(t *testing.T) {
	_, _, err := runCommand(t, "event merge", []string{"--from", "a"}, &fakeClient{})
	if err == nil || !strings.Contains(err.Error(), "both --from and --to") {
		t.Fatalf("err = %v", err)
	}
}

func TestEventGet(t *testing.T) {
	req, out, err := runCommand(t, "event get", []string{"--id", "e1", "--memories"}, &fakeClient{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	if got := req.(*contract.GetEventByIdRequest); got.GetId() != "e1" || !got.GetMemories() {
		t.Fatalf("unexpected request: %+v", got)
	}

	if !strings.Contains(out, "event e1") {
		t.Fatalf("output = %q", out)
	}
}

func TestPurgeNeedsConfirmation(t *testing.T) {
	_, _, err := runCommand(t, "purge", nil, &fakeClient{})
	if err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("err = %v", err)
	}

	req, out, err := runCommand(t, "purge", []string{"--yes"}, &fakeClient{})
	if err != nil {
		t.Fatalf("run with --yes: %v", err)
	}

	if _, ok := req.(*contract.EmptyRequest); !ok {
		t.Fatalf("captured %T", req)
	}

	if !strings.Contains(out, "ok: true") {
		t.Fatalf("output = %q", out)
	}
}

// TestSleepDryRunUsesThePreviewRPC pins the routing: --dry-run must reach PreviewConsolidation and
// must NOT reach Sleep, because a dry run that triggered a real cycle would delete the very
// memories the operator asked only to be shown.
func TestSleepDryRunUsesThePreviewRPC(t *testing.T) {
	req, _, err := runCommand(t, "sleep", []string{"--dry-run", "--limit", "25"}, &fakeClient{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	preview, ok := req.(*contract.PreviewConsolidationRequest)
	if !ok {
		t.Fatalf("captured %T, want a PreviewConsolidationRequest", req)
	}

	if preview.GetLimit() != 25 {
		t.Errorf("limit: got %d, want 25", preview.GetLimit())
	}
}

// TestSleepWithoutDryRunTriggersACycle is the other half: the default is unchanged.
func TestSleepWithoutDryRunTriggersACycle(t *testing.T) {
	req, out, err := runCommand(t, "sleep", nil, &fakeClient{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	if _, ok := req.(*contract.EmptyRequest); !ok {
		t.Fatalf("captured %T, want an EmptyRequest", req)
	}

	if !strings.Contains(out, "ok: true") {
		t.Fatalf("output = %q", out)
	}
}

func TestSummaryReplace(t *testing.T) {
	req, _, err := runCommand(t, "summary replace", []string{"--event-id", "e9", "--body", "sum", "--significance", "4"}, &fakeClient{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	got := req.(*contract.ReplaceMemoriesWithSummaryRequest)

	if got.GetEventId() != "e9" || got.GetSummary().GetBody() != "sum" || got.GetSummary().GetSignificance() != 4 {
		t.Fatalf("unexpected request: %+v", got)
	}
}

func TestSummaryReplaceRequiresEventID(t *testing.T) {
	_, _, err := runCommand(t, "summary replace", []string{"--body", "x"}, &fakeClient{})
	if err == nil || !strings.Contains(err.Error(), "--event-id is required") {
		t.Fatalf("err = %v", err)
	}
}

func TestImportBatchFromFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "batch.json")

	batch := `{"memories":[{"id":"m1","body":"hello","significance":3}],"events":[{"id":"e1","name":"ev"}]}`

	if err := os.WriteFile(path, []byte(batch), 0o600); err != nil {
		t.Fatal(err)
	}

	req, _, err := runCommand(t, "import-batch", []string{"--file", path}, &fakeClient{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	got := req.(*contract.ImportBatchRequest)

	if len(got.GetMemories()) != 1 || got.GetMemories()[0].GetId() != "m1" {
		t.Fatalf("memories = %+v", got.GetMemories())
	}

	if len(got.GetEvents()) != 1 || got.GetEvents()[0].GetName() != "ev" {
		t.Fatalf("events = %+v", got.GetEvents())
	}
}

func TestImportBatchRequiresFile(t *testing.T) {
	_, _, err := runCommand(t, "import-batch", nil, &fakeClient{})
	if err == nil || !strings.Contains(err.Error(), "--file is required") {
		t.Fatalf("err = %v", err)
	}
}

func TestExportClearFlag(t *testing.T) {
	req, out, err := runCommand(t, "export", []string{"--clear"}, &fakeClient{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	if !req.(*contract.ExportRequest).GetClear() {
		t.Fatal("clear should be true")
	}

	if !strings.Contains(out, "man-1") {
		t.Fatalf("output = %q", out)
	}
}
