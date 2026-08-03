package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/fastbean-au/hippocampus/contract"
)

// TestHandlerValidationErrors drives each handler down its input-validation error branch (bad
// RFC3339 time, bad placement mode, or an unreadable body/file) so those per-handler branches are
// covered.
func TestHandlerValidationErrors(t *testing.T) {
	cases := []struct {
		key    string
		args   []string
		substr string
	}{
		{"memory store", []string{"--body", "b", "--timestamp", "nope"}, "want RFC3339"},
		{"memory store", []string{"--body", "b", "--place-mode", "sideways"}, "invalid --place-mode"},
		{"memory store", []string{"--body-file", "/no/such/file"}, "failed to read"},
		{"memory update", []string{"--id", "m", "--timestamp", "nope"}, "want RFC3339"},
		{"event end", []string{"--id", "e", "--time-end", "nope"}, "want RFC3339"},
		{"event create", []string{"--name", "n", "--time-start", "nope"}, "want RFC3339"},
		{"event create", []string{"--name", "n", "--time-end", "nope"}, "want RFC3339"},
		{"event significance", []string{"--id", "e", "--place-mode", "sideways"}, "invalid --place-mode"},
		{"event list", []string{"--time-start-min", "nope"}, "want RFC3339"},
		{"event list", []string{"--time-start-max", "nope"}, "want RFC3339"},
		{"event list", []string{"--time-end-min", "nope"}, "want RFC3339"},
		{"event list", []string{"--time-end-max", "nope"}, "want RFC3339"},
		{"event list", []string{"--extremum", "sideways"}, "invalid --extremum"},
		{"memory list", []string{"--timestamp-min", "nope"}, "want RFC3339"},
		{"memory list", []string{"--timestamp-max", "nope"}, "want RFC3339"},
		{"summary summarise", []string{"--event-id", "e", "--place-mode", "sideways"}, "invalid --place-mode"},
		{"summary replace", []string{"--event-id", "e", "--body", "s", "--place-mode", "sideways"}, "invalid --place-mode"},
		{"import-batch", []string{"--file", "/no/such/file"}, "failed to read"},
	}

	for _, tc := range cases {
		_, _, err := runCommand(t, tc.key, tc.args, &fakeClient{})
		if err == nil || !strings.Contains(err.Error(), tc.substr) {
			t.Errorf("%s %v: err = %v, want %q", tc.key, tc.args, err, tc.substr)
		}
	}
}

// TestHandlerRequiredFlags covers the remaining "required flag missing" branches.
func TestHandlerRequiredFlags(t *testing.T) {
	cases := []struct {
		key    string
		args   []string
		substr string
	}{
		{"memory recall", nil, "at least one memory id"},
		{"event delete", nil, "--id is required"},
		{"event get", nil, "--id is required"},
		{"import", nil, "--object-key is required"},
		{"clear", nil, "--manifest-id is required"},
		{"event significance", nil, "--id is required"},
		{"summary summarise", nil, "--event-id is required"},
	}

	for _, tc := range cases {
		_, _, err := runCommand(t, tc.key, tc.args, &fakeClient{})
		if err == nil || !strings.Contains(err.Error(), tc.substr) {
			t.Errorf("%s: err = %v, want %q", tc.key, err, tc.substr)
		}
	}
}

// TestPlacementBelowAndExtremumHighest covers the placement `below` arm and the `highest` extremum
// arm not hit elsewhere.
func TestPlacementBelowAndExtremumHighest(t *testing.T) {
	req, _, err := runCommand(t, "memory store", []string{"--body", "b", "--place-mode", "below", "--place-anchor", "3"}, &fakeClient{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	if req.(*contract.Memory).GetPlacement().GetMode() != contract.SignificancePlacement_BELOW {
		t.Fatalf("placement mode = %v", req.(*contract.Memory).GetPlacement().GetMode())
	}

	req, _, err = runCommand(t, "memory list", []string{"--extremum", "highest"}, &fakeClient{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	if got := req.(*contract.GetMemoriesRequest).GetSignificanceExtremum(); got != contract.SignificanceExtremum_SIGNIFICANCE_EXTREMUM_HIGHEST {
		t.Fatalf("extremum = %v", got)
	}
}

// TestRenderStoreEventRejectedAndEventsList covers renderText's StoreEventResponse-rejected branch
// and the GetEventsResponse list-loop body.
func TestRenderStoreEventRejectedAndEventsList(t *testing.T) {
	var sb strings.Builder

	r := &renderer{out: &sb}

	if err := r.render(&contract.StoreEventResponse{Rejected: true}); err != nil {
		t.Fatalf("render: %v", err)
	}

	if err := r.render(&contract.GetEventsResponse{
		TotalCount: 1,
		Events:     []*contract.Event{{Id: "e1", Name: "deploy", Significance: 4}},
	}); err != nil {
		t.Fatalf("render: %v", err)
	}

	out := sb.String()

	if !strings.Contains(out, "rejected") {
		t.Fatalf("expected the rejected line: %q", out)
	}

	if !strings.Contains(out, "event e1") || !strings.Contains(out, "1 event(s)") {
		t.Fatalf("expected the events list: %q", out)
	}
}

// TestRenderNilMemory covers renderMemory's nil guard.
func TestRenderNilMemory(t *testing.T) {
	var sb strings.Builder

	r := &renderer{out: &sb}

	if err := r.render(&contract.GetMemoriesResponse{Memories: []*contract.Memory{nil}}); err != nil {
		t.Fatalf("render: %v", err)
	}

	if !strings.Contains(sb.String(), "(no memory)") {
		t.Fatalf("output = %q", sb.String())
	}
}

// TestImportBatchBadJSON covers runImportBatch's parse-error branch.
func TestImportBatchBadJSON(t *testing.T) {
	path := writeTempFile(t, "{ not valid json")

	_, _, err := runCommand(t, "import-batch", []string{"--file", path}, &fakeClient{})
	if err == nil || !strings.Contains(err.Error(), "ImportBatchRequest") {
		t.Fatalf("err = %v", err)
	}
}

// TestGatewayErrorEmptyBody covers gatewayError's non-JSON, empty-body branch (falls back to the
// HTTP status text under a code derived from the status).
func TestGatewayErrorEmptyBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))

	t.Cleanup(server.Close)

	client := &httpClient{baseURL: server.URL, http: &http.Client{Timeout: 5 * time.Second}}

	_, err := client.WhoAmI(context.Background(), &contract.EmptyRequest{})
	if err == nil {
		t.Fatal("expected an error")
	}

	st, _ := status.FromError(err)

	if st.Code() != codes.Unknown {
		t.Fatalf("code = %v, want Unknown", st.Code())
	}

	if st.Message() != http.StatusText(http.StatusInternalServerError) {
		t.Fatalf("message = %q, want the HTTP status text", st.Message())
	}
}
