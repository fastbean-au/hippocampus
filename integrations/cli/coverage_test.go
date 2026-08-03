package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"

	"github.com/fastbean-au/hippocampus/contract"
)

// echoGatewayClient returns an httpClient pointed at a gateway that replies with an empty JSON
// object to every request, which is enough for every RPC's response to unmarshal.
func echoGatewayClient(t *testing.T) *httpClient {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{}"))
	}))

	t.Cleanup(server.Close)

	return &httpClient{baseURL: server.URL, http: &http.Client{Timeout: 5 * time.Second}}
}

// TestHTTPClientAllMethods exercises every RPC method on the HTTP transport so the whole mapping
// table (and the shared do path) is covered.
func TestHTTPClientAllMethods(t *testing.T) {
	c := echoGatewayClient(t)
	ctx := context.Background()

	calls := []struct {
		name string
		call func() error
	}{
		{"Purge", func() error { _, err := c.Purge(ctx, &contract.EmptyRequest{}); return err }},
		{"Sleep", func() error { _, err := c.Sleep(ctx, &contract.EmptyRequest{}); return err }},
		{"WhoAmI", func() error { _, err := c.WhoAmI(ctx, &contract.EmptyRequest{}); return err }},
		{"StoreEvent", func() error { _, err := c.StoreEvent(ctx, &contract.Event{Name: "n"}); return err }},
		{"EndEvent", func() error { _, err := c.EndEvent(ctx, &contract.EndEventRequest{Id: "e"}); return err }},
		{"UpdateEventSignificance", func() error {
			_, err := c.UpdateEventSignificance(ctx, &contract.UpdateEventSignificanceRequest{Id: "e", Significance: 2})

			return err
		}},
		{"MergeEvents", func() error {
			_, err := c.MergeEvents(ctx, &contract.MergeEventsRequest{MergeFrom: "a", MergeTo: "b"})

			return err
		}},
		{"DeleteEvent", func() error { _, err := c.DeleteEvent(ctx, &contract.DeleteEventRequest{Id: "e"}); return err }},
		{"GetEventById", func() error { _, err := c.GetEventById(ctx, &contract.GetEventByIdRequest{Id: "e"}); return err }},
		{"GetEvents", func() error { _, err := c.GetEvents(ctx, &contract.GetEventsRequest{Limit: 5}); return err }},
		{"StoreMemory", func() error { _, err := c.StoreMemory(ctx, &contract.Memory{Body: "b"}); return err }},
		{"UpdateMemory", func() error { _, err := c.UpdateMemory(ctx, &contract.Memory{Id: "m", Body: "b"}); return err }},
		{"DeleteMemories", func() error {
			_, err := c.DeleteMemories(ctx, &contract.DeleteMemoriesRequest{Ids: []string{"m"}})

			return err
		}},
		{"GetMemories", func() error { _, err := c.GetMemories(ctx, &contract.GetMemoriesRequest{Limit: 5}); return err }},
		{"RecallMemories", func() error {
			_, err := c.RecallMemories(ctx, &contract.RecallMemoriesRequest{Ids: []string{"m"}})

			return err
		}},
		{"SearchMemories", func() error {
			_, err := c.SearchMemories(ctx, &contract.SearchMemoriesRequest{Query: "q"})

			return err
		}},
		{"ReplaceMemoriesWithSummary", func() error {
			_, err := c.ReplaceMemoriesWithSummary(ctx, &contract.ReplaceMemoriesWithSummaryRequest{EventId: "e", Summary: &contract.Memory{Body: "s"}})

			return err
		}},
		{"GetSummarisationCandidates", func() error {
			_, err := c.GetSummarisationCandidates(ctx, &contract.EmptyRequest{})

			return err
		}},
		{"SummariseMemories", func() error {
			_, err := c.SummariseMemories(ctx, &contract.SummariseMemoriesRequest{EventId: "e"})

			return err
		}},
		{"Export", func() error { _, err := c.Export(ctx, &contract.ExportRequest{Clear: true}); return err }},
		{"Import", func() error { _, err := c.Import(ctx, &contract.ImportRequest{ObjectKey: "k"}); return err }},
		{"ImportBatch", func() error { _, err := c.ImportBatch(ctx, &contract.ImportBatchRequest{}); return err }},
		{"Transfer", func() error { _, err := c.Transfer(ctx, &contract.TransferRequest{}); return err }},
		{"Clear", func() error { _, err := c.Clear(ctx, &contract.ClearRequest{ManifestId: "m"}); return err }},
	}

	for _, tc := range calls {
		if err := tc.call(); err != nil {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
}

// TestAllHandlersSucceed runs every command handler with minimal valid arguments against the fake
// client, covering the handlers not individually asserted elsewhere.
func TestAllHandlersSucceed(t *testing.T) {
	batchFile := filepath.Join(t.TempDir(), "batch.json")
	if err := os.WriteFile(batchFile, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		key  string
		args []string
	}{
		{"memory store", []string{"--body", "b"}},
		{"memory update", []string{"--id", "m", "--body", "b"}},
		{"memory delete", []string{"m"}},
		{"memory list", nil},
		{"memory recall", []string{"m"}},
		{"memory search", []string{"--query", "q"}},
		{"event create", []string{"--name", "n"}},
		{"event end", []string{"--id", "e"}},
		{"event significance", []string{"--id", "e", "--significance", "2"}},
		{"event merge", []string{"--from", "a", "--to", "b"}},
		{"event delete", []string{"--id", "e"}},
		{"event get", []string{"--id", "e"}},
		{"event list", []string{"--memories"}},
		{"whoami", nil},
		{"sleep", nil},
		{"purge", []string{"--yes"}},
		{"summary candidates", nil},
		{"summary replace", []string{"--event-id", "e", "--body", "s"}},
		{"summary summarise", []string{"--event-id", "e"}},
		{"export", nil},
		{"import", []string{"--object-key", "k"}},
		{"import-batch", []string{"--file", batchFile}},
		{"transfer", nil},
		{"clear", []string{"--manifest-id", "m"}},
	}

	for _, tc := range cases {
		if _, _, err := runCommand(t, tc.key, tc.args, &fakeClient{}); err != nil {
			t.Errorf("%s: %v", tc.key, err)
		}
	}
}

// TestRunOverHTTP drives the full run() path over the HTTP transport against a fake gateway,
// covering client construction, dispatch, and rendering end to end.
func TestRunOverHTTP(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if r.URL.Path == "/v1/whoami" {
			_, _ = w.Write([]byte(`{"clientId":"c1","role":"admin","authEnabled":true}`))

			return
		}

		_, _ = w.Write([]byte("{}"))
	}))

	t.Cleanup(server.Close)

	var out bytes.Buffer

	args := []string{"--transport", "http", "--address", server.URL, "--output", "json", "whoami"}

	if err := run(context.Background(), args, &out, &out); err != nil {
		t.Fatalf("run: %v", err)
	}

	if !strings.Contains(out.String(), "admin") {
		t.Fatalf("output = %q", out.String())
	}
}

// TestRunCommandHelp covers the per-command usage path (commandUsage/localFlagUsages).
func TestRunCommandHelp(t *testing.T) {
	var out bytes.Buffer

	if err := run(context.Background(), []string{"memory", "store", "--help"}, &out, &out); err != nil {
		t.Fatalf("run: %v", err)
	}

	if !strings.Contains(out.String(), "memory store") || !strings.Contains(out.String(), "--body") {
		t.Fatalf("command help = %q", out.String())
	}
}

func TestBearerTokenInterceptor(t *testing.T) {
	interceptor := bearerTokenInterceptor("secret")

	var seen string

	invoker := func(ctx context.Context, _ string, _, _ any, _ *grpc.ClientConn, _ ...grpc.CallOption) error {
		md, _ := metadata.FromOutgoingContext(ctx)

		if values := md.Get("authorization"); len(values) > 0 {
			seen = values[0]
		}

		return nil
	}

	if err := interceptor(context.Background(), "/proto.Hippocampus/WhoAmI", nil, nil, nil, invoker); err != nil {
		t.Fatalf("interceptor: %v", err)
	}

	if seen != "Bearer secret" {
		t.Fatalf("authorization = %q, want 'Bearer secret'", seen)
	}
}

func TestHTTPStatusToCode(t *testing.T) {
	cases := map[int]codes.Code{
		http.StatusBadRequest:          codes.InvalidArgument,
		http.StatusUnauthorized:        codes.Unauthenticated,
		http.StatusForbidden:           codes.PermissionDenied,
		http.StatusNotFound:            codes.NotFound,
		http.StatusServiceUnavailable:  codes.Unavailable,
		http.StatusInternalServerError: codes.Unknown,
	}

	for statusCode, want := range cases {
		if got := httpStatusToCode(statusCode); got != want {
			t.Errorf("httpStatusToCode(%d) = %v, want %v", statusCode, got, want)
		}
	}
}

func TestReadFileOrStdinMissing(t *testing.T) {
	if _, err := readFileOrStdin(filepath.Join(t.TempDir(), "nope.json")); err == nil {
		t.Fatal("expected an error for a missing file")
	}
}
