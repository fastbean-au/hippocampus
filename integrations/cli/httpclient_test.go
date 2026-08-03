package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/fastbean-au/hippocampus/contract"
)

// capturedRequest records what the fake gateway received so a test can assert the httpClient built
// the right HTTP method, path, query, and body for an RPC.
type capturedRequest struct {
	method string
	path   string
	query  url.Values
	body   []byte
	auth   string
}

// newTestHTTPClient stands up a fake gateway that records each request and replies with respBody
// (protojson) at respStatus, returning an httpClient pointed at it plus a pointer to the capture.
func newTestHTTPClient(t *testing.T, respStatus int, respBody proto.Message) (*httpClient, *capturedRequest) {
	t.Helper()

	captured := &capturedRequest{}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)

		captured.method = r.Method
		captured.path = r.URL.Path
		captured.query = r.URL.Query()
		captured.body = body
		captured.auth = r.Header.Get("Authorization")

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(respStatus)

		if respBody != nil {
			data, _ := protojson.Marshal(respBody)
			_, _ = w.Write(data)
		}
	}))

	t.Cleanup(server.Close)

	client := &httpClient{
		baseURL: server.URL,
		token:   "test-token",
		http:    &http.Client{Timeout: 5 * time.Second},
	}

	return client, captured
}

func TestHTTPClientStoreMemory(t *testing.T) {
	client, captured := newTestHTTPClient(t, http.StatusOK, &contract.StoreMemoryResponse{Id: "m1"})

	resp, err := client.StoreMemory(context.Background(), &contract.Memory{Body: "hello", Significance: 5})
	if err != nil {
		t.Fatalf("StoreMemory: %v", err)
	}

	if resp.GetId() != "m1" {
		t.Fatalf("id = %q, want m1", resp.GetId())
	}

	if captured.method != http.MethodPost || captured.path != "/v1/memories" {
		t.Fatalf("got %s %s, want POST /v1/memories", captured.method, captured.path)
	}

	if captured.auth != "Bearer test-token" {
		t.Fatalf("auth header = %q", captured.auth)
	}

	sent := &contract.Memory{}
	if err := protojson.Unmarshal(captured.body, sent); err != nil {
		t.Fatalf("body was not valid protojson: %v", err)
	}

	if sent.GetBody() != "hello" || sent.GetSignificance() != 5 {
		t.Fatalf("body round-trip mismatch: %+v", sent)
	}
}

func TestHTTPClientGetMemoriesQuery(t *testing.T) {
	client, captured := newTestHTTPClient(t, http.StatusOK, &contract.GetMemoriesResponse{TotalCount: 3})

	req := &contract.GetMemoriesRequest{
		SignificanceMin:      2,
		Group:                "svc-a",
		Limit:                10,
		SignificanceExtremum: contract.SignificanceExtremum_SIGNIFICANCE_EXTREMUM_HIGHEST,
	}

	if _, err := client.GetMemories(context.Background(), req); err != nil {
		t.Fatalf("GetMemories: %v", err)
	}

	if captured.method != http.MethodGet || captured.path != "/v1/memories" {
		t.Fatalf("got %s %s, want GET /v1/memories", captured.method, captured.path)
	}

	if captured.query.Get("significanceMin") != "2" {
		t.Fatalf("significanceMin = %q, want 2", captured.query.Get("significanceMin"))
	}

	if captured.query.Get("group") != "svc-a" {
		t.Fatalf("group = %q, want svc-a", captured.query.Get("group"))
	}

	if captured.query.Get("limit") != "10" {
		t.Fatalf("limit = %q, want 10", captured.query.Get("limit"))
	}

	if got := captured.query.Get("significanceExtremum"); got != "SIGNIFICANCE_EXTREMUM_HIGHEST" {
		t.Fatalf("significanceExtremum = %q", got)
	}

	// A zero/unset field must never be sent as a query parameter.
	if _, ok := captured.query["offset"]; ok {
		t.Fatalf("offset should be omitted when unset, got %q", captured.query.Get("offset"))
	}
}

func TestHTTPClientGetEventByIdPathAndExclusion(t *testing.T) {
	client, captured := newTestHTTPClient(t, http.StatusOK, &contract.GetEventResponse{})

	req := &contract.GetEventByIdRequest{Id: "ev 1", Memories: true}

	if _, err := client.GetEventById(context.Background(), req); err != nil {
		t.Fatalf("GetEventById: %v", err)
	}

	if captured.path != "/v1/events/ev 1" {
		t.Fatalf("path = %q, want /v1/events/ev 1 (escaped id decoded by the server)", captured.path)
	}

	if captured.query.Get("memories") != "true" {
		t.Fatalf("memories = %q, want true", captured.query.Get("memories"))
	}

	// id is bound to the path; it must not be duplicated into the query string.
	if _, ok := captured.query["id"]; ok {
		t.Fatalf("id should be excluded from the query, got %q", captured.query.Get("id"))
	}
}

func TestHTTPClientDeleteEvent(t *testing.T) {
	client, captured := newTestHTTPClient(t, http.StatusOK, &contract.GeneralResponse{Ok: true})

	if _, err := client.DeleteEvent(context.Background(), &contract.DeleteEventRequest{Id: "e1", Memories: true}); err != nil {
		t.Fatalf("DeleteEvent: %v", err)
	}

	if captured.method != http.MethodDelete || captured.path != "/v1/events/e1" {
		t.Fatalf("got %s %s, want DELETE /v1/events/e1", captured.method, captured.path)
	}

	if captured.query.Get("memories") != "true" {
		t.Fatalf("memories = %q, want true", captured.query.Get("memories"))
	}
}

func TestHTTPClientReplaceSummaryBindsSummaryBody(t *testing.T) {
	client, captured := newTestHTTPClient(t, http.StatusOK, &contract.ReplaceMemoriesWithSummaryResponse{Id: "s1", MemoriesReplaced: 4})

	req := &contract.ReplaceMemoriesWithSummaryRequest{
		EventId: "e9",
		Summary: &contract.Memory{Body: "the summary", Significance: 7},
	}

	if _, err := client.ReplaceMemoriesWithSummary(context.Background(), req); err != nil {
		t.Fatalf("ReplaceMemoriesWithSummary: %v", err)
	}

	if captured.path != "/v1/events/e9/summary" {
		t.Fatalf("path = %q, want /v1/events/e9/summary", captured.path)
	}

	// Only the summary Memory is bound to the body (body: "summary"), not the whole request.
	sent := &contract.Memory{}
	if err := protojson.Unmarshal(captured.body, sent); err != nil {
		t.Fatalf("body was not a Memory: %v", err)
	}

	if sent.GetBody() != "the summary" {
		t.Fatalf("summary body = %q", sent.GetBody())
	}
}

func TestHTTPClientErrorMapsToStatus(t *testing.T) {
	client, _ := newTestHTTPClient(t, http.StatusNotFound, nil)

	// Override the handler by pointing at a server that returns a gateway-style error body.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"code":5,"message":"event not found"}`))
	}))

	t.Cleanup(server.Close)

	client.baseURL = server.URL

	_, err := client.GetEventById(context.Background(), &contract.GetEventByIdRequest{Id: "nope"})
	if err == nil {
		t.Fatal("expected an error")
	}

	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("error was not a status: %v", err)
	}

	if st.Code() != codes.NotFound {
		t.Fatalf("code = %v, want NotFound", st.Code())
	}

	if st.Message() != "event not found" {
		t.Fatalf("message = %q", st.Message())
	}
}

func TestHTTPClientErrorNonJSONBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("purge in progress"))
	}))

	t.Cleanup(server.Close)

	client := &httpClient{baseURL: server.URL, http: &http.Client{Timeout: 5 * time.Second}}

	_, err := client.Sleep(context.Background(), &contract.EmptyRequest{})
	if err == nil {
		t.Fatal("expected an error")
	}

	st, _ := status.FromError(err)
	if st.Code() != codes.Unavailable {
		t.Fatalf("code = %v, want Unavailable", st.Code())
	}

	if st.Message() != "purge in progress" {
		t.Fatalf("message = %q", st.Message())
	}
}

func TestHTTPClientImplementsInterface(t *testing.T) {
	// Compile-time guarantee restated as a test hook so the assertion is visible in coverage.
	var _ contract.HippocampusClient = (*httpClient)(nil)
}
