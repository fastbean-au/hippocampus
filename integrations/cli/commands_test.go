package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
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
	args := []string{"--name", "deploy", "--significance", "8", "--link", "e2:3", "--link", "e3:1", "--time-start", "2026-01-02T15:04:05Z"}

	req, out, err := runCommand(t, "event create", args, &fakeClient{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	event := req.(*contract.Event)

	if event.GetName() != "deploy" || event.GetSignificance() != 8 {
		t.Fatalf("unexpected event: %+v", event)
	}

	if len(event.GetLinks()) != 2 || event.GetLinks()[0].GetId() != "e2" || event.GetLinks()[0].GetSignificance() != 3 {
		t.Fatalf("unexpected links: %+v", event.GetLinks())
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

func TestEventCreateBadLink(t *testing.T) {
	_, _, err := runCommand(t, "event create", []string{"--name", "x", "--link", "no-colon"}, &fakeClient{})
	if err == nil || !strings.Contains(err.Error(), "invalid --link") {
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

// TestMemoryExplain covers the request shaping: ids from --id and positional args alike, and the
// curve attached only when a significance was asked for.
func TestMemoryExplain(t *testing.T) {
	req, _, err := runCommand(t, "memory explain",
		[]string{"--id", "m1", "--curve-significance", "40", "--curve-days", "90", "--curve-points", "20", "m2"},
		&fakeClient{},
	)
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	explain, ok := req.(*contract.ExplainConsolidationRequest)
	if !ok {
		t.Fatalf("captured %T, want an ExplainConsolidationRequest", req)
	}

	if len(explain.GetMemoryIds()) != 2 || explain.GetMemoryIds()[1] != "m2" {
		t.Errorf("memory_ids = %v", explain.GetMemoryIds())
	}

	curve := explain.GetCurve()

	if curve.GetSignificance() != 40 || curve.GetMaxAgeDays() != 90 || curve.GetPoints() != 20 {
		t.Errorf("curve = %+v", curve)
	}
}

// TestMemoryExplainCurveOnly covers the other supported shape: no ids at all, asking only what the
// current configuration does.
func TestMemoryExplainCurveOnly(t *testing.T) {
	req, _, err := runCommand(t, "memory explain", []string{"--curve-significance", "10"}, &fakeClient{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	if len(req.(*contract.ExplainConsolidationRequest).GetMemoryIds()) != 0 {
		t.Error("expected no memory ids")
	}
}

// TestMemoryExplainNeedsSomethingToExplain covers the one input rule: a call with neither ids nor a
// curve has nothing to answer.
func TestMemoryExplainNeedsSomethingToExplain(t *testing.T) {
	_, _, err := runCommand(t, "memory explain", nil, &fakeClient{})
	if err == nil || !strings.Contains(err.Error(), "curve-significance") {
		t.Fatalf("err = %v", err)
	}
}

// TestMemoryLink verifies the near end and the parsed link set reach LinkMemories.
func TestMemoryLink(t *testing.T) {
	args := []string{"--id", "m1", "--link", "m2:5", "--link", "m3:7"}

	req, _, err := runCommand(t, "memory link", args, &fakeClient{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	in := req.(*contract.LinkMemoriesRequest)

	if in.GetId() != "m1" || len(in.GetLinks()) != 2 {
		t.Fatalf("unexpected request: %+v", in)
	}

	if in.GetLinks()[1].GetId() != "m3" || in.GetLinks()[1].GetSignificance() != 7 {
		t.Fatalf("unexpected links: %+v", in.GetLinks())
	}
}

func TestMemoryLinkRequiresId(t *testing.T) {
	_, _, err := runCommand(t, "memory link", []string{"--link", "m2:5"}, &fakeClient{})
	if err == nil || !strings.Contains(err.Error(), "--id is required") {
		t.Fatalf("err = %v", err)
	}
}

func TestMemoryLinkRequiresLinks(t *testing.T) {
	_, _, err := runCommand(t, "memory link", []string{"--id", "m1"}, &fakeClient{})
	if err == nil || !strings.Contains(err.Error(), "at least one --link") {
		t.Fatalf("err = %v", err)
	}
}

// TestMemoryUnlinkPositionalTargets pins that targets may be positional, matching how the delete
// commands take ids.
func TestMemoryUnlinkPositionalTargets(t *testing.T) {
	req, _, err := runCommand(t, "memory unlink", []string{"--id", "m1", "m2", "m3"}, &fakeClient{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	in := req.(*contract.UnlinkMemoriesRequest)

	if in.GetId() != "m1" || len(in.GetIds()) != 2 || in.GetIds()[0] != "m2" {
		t.Fatalf("unexpected request: %+v", in)
	}
}

func TestMemoryLinksRendersEdges(t *testing.T) {
	fake := &fakeClient{linksResp: &contract.GetLinksResponse{
		LinkSignificance: 12,
		Links: []*contract.LinkEdge{
			{Id: "m2", Significance: 5, Direction: contract.LinkDirection_LINK_DIRECTION_OUTBOUND},
			{Id: "m3", Significance: 7, Direction: contract.LinkDirection_LINK_DIRECTION_INBOUND},
		},
	}}

	req, out, err := runCommand(t, "memory links", []string{"--id", "m1", "--direction", "both"}, fake)
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	if in := req.(*contract.GetMemoryLinksRequest); in.GetDirection() != contract.LinkDirection_LINK_DIRECTION_BOTH {
		t.Fatalf("unexpected direction: %v", in.GetDirection())
	}

	for _, want := range []string{"2 link(s)", "12 total link significance", "outbound", "inbound"} {
		if !strings.Contains(out, want) {
			t.Errorf("output %q missing %q", out, want)
		}
	}
}

func TestLinksBadDirection(t *testing.T) {
	_, _, err := runCommand(t, "event links", []string{"--id", "e1", "--direction", "sideways"}, &fakeClient{})
	if err == nil || !strings.Contains(err.Error(), "invalid --direction") {
		t.Fatalf("err = %v", err)
	}
}

// TestEventLink covers the event half, which shares its implementation with the memory half.
func TestEventLink(t *testing.T) {
	req, _, err := runCommand(t, "event link", []string{"--id", "e1", "--link", "e2:3"}, &fakeClient{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	in := req.(*contract.LinkEventsRequest)

	if in.GetId() != "e1" || len(in.GetLinks()) != 1 || in.GetLinks()[0].GetId() != "e2" {
		t.Fatalf("unexpected request: %+v", in)
	}
}

func TestEventUnlink(t *testing.T) {
	req, _, err := runCommand(t, "event unlink", []string{"--id", "e1", "--target", "e2"}, &fakeClient{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	if in := req.(*contract.UnlinkEventsRequest); in.GetId() != "e1" || len(in.GetIds()) != 1 {
		t.Fatalf("unexpected request: %+v", in)
	}
}

func TestEventUnlinkRequiresTargets(t *testing.T) {
	_, _, err := runCommand(t, "event unlink", []string{"--id", "e1"}, &fakeClient{})
	if err == nil || !strings.Contains(err.Error(), "at least one event id to unlink") {
		t.Fatalf("err = %v", err)
	}
}

// TestParseMetadata covers the write-side flag parsing. The split is on the FIRST '=', so a value
// may contain one; a key may not, which is what makes the packing unambiguous.
func TestParseMetadata(t *testing.T) {
	cases := []struct {
		name    string
		raw     []string
		want    map[string]string
		wantErr bool
	}{
		{"none", nil, nil, false},
		{"one pair", []string{"source=slack"}, map[string]string{"source": "slack"}, false},
		{
			"two pairs",
			[]string{"source=slack", "project=apollo"},
			map[string]string{"source": "slack", "project": "apollo"},
			false,
		},
		{"value containing the separator", []string{"q=a=b"}, map[string]string{"q": "a=b"}, false},
		{"empty value", []string{"source="}, map[string]string{"source": ""}, false},
		{"repeated key, same value", []string{"a=1", "a=1"}, map[string]string{"a": "1"}, false},

		{"no separator", []string{"novalue"}, nil, true},
		{"no key", []string{"=v"}, nil, true},
		{"repeated key, different values", []string{"a=1", "a=2"}, nil, true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseMetadata(c.raw)

			if c.wantErr {
				if err == nil {
					t.Fatalf("expected an error for %v", c.raw)
				}

				return
			}

			if err != nil {
				t.Fatalf("parseMetadata(%v): %s", c.raw, err)
			}

			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("expected %#v, got %#v", c.want, got)
			}
		})
	}
}

// TestTriStateFromFlag pins why these filters are string flags rather than pflag bools: an omitted
// bool and an explicit --summary=false are the same value, so "only the non-summaries" would be
// unaskable.
func TestTriStateFromFlag(t *testing.T) {
	cases := []struct {
		value   string
		want    contract.Bool
		wantErr bool
	}{
		{"", contract.Bool_UNSPECIFIED, false},
		{"true", contract.Bool_TRUE, false},
		{"false", contract.Bool_FALSE, false},
		{"TRUE", contract.Bool_TRUE, false},
		{" false ", contract.Bool_FALSE, false},
		{"maybe", contract.Bool_UNSPECIFIED, true},
	}

	for _, c := range cases {
		fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
		fs.String("recalled", "", "")

		if err := fs.Set("recalled", c.value); err != nil {
			t.Fatalf("failed to set the flag: %s", err)
		}

		got, err := triStateFromFlag(fs, "recalled")

		if c.wantErr {
			if err == nil {
				t.Errorf("expected an error for %q", c.value)
			}

			continue
		}

		if err != nil {
			t.Errorf("triStateFromFlag(%q): %s", c.value, err)

			continue
		}

		if got != c.want {
			t.Errorf("triStateFromFlag(%q) = %v, want %v", c.value, got, c.want)
		}
	}
}

func TestListSortFlags(t *testing.T) {
	req, _, err := runCommand(t, "memory list",
		[]string{"--order-by", "recall_count", "--order-dir", "asc"}, &fakeClient{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	memories := req.(*contract.GetMemoriesRequest)

	if memories.GetOrderBy() != "recall_count" ||
		memories.GetOrderDir() != contract.SortDirection_SORT_DIRECTION_ASC {
		t.Fatalf("unexpected memory sort: order_by=%q order_dir=%v",
			memories.GetOrderBy(), memories.GetOrderDir())
	}

	req, _, err = runCommand(t, "event list",
		[]string{"--order-by", "name", "--order-dir", "desc"}, &fakeClient{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	events := req.(*contract.GetEventsRequest)

	if events.GetOrderBy() != "name" ||
		events.GetOrderDir() != contract.SortDirection_SORT_DIRECTION_DESC {
		t.Fatalf("unexpected event sort: order_by=%q order_dir=%v",
			events.GetOrderBy(), events.GetOrderDir())
	}

	// An omitted direction stays UNSPECIFIED rather than defaulting to one, so the service applies
	// the sort field's natural direction.
	req, _, err = runCommand(t, "memory list", []string{"--order-by", "id"}, &fakeClient{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	if dir := req.(*contract.GetMemoriesRequest).GetOrderDir(); dir != contract.SortDirection_SORT_DIRECTION_UNSPECIFIED {
		t.Fatalf("omitted --order-dir = %v, want UNSPECIFIED", dir)
	}
}

func TestListBadSortDirection(t *testing.T) {
	for _, key := range []string{"memory list", "event list"} {
		if _, _, err := runCommand(t, key, []string{"--order-dir", "sideways"}, &fakeClient{}); err == nil ||
			!strings.Contains(err.Error(), "invalid --order-dir") {
			t.Errorf("%s: err = %v", key, err)
		}
	}
}
