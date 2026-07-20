package contract_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"github.com/spf13/viper"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/fastbean-au/hippocampus/contract"
	"github.com/fastbean-au/hippocampus/db"
	"github.com/fastbean-au/hippocampus/hippocampus"
)

// newGatewayTestServer wires up exactly what cmd/hippocampus/main.go does to serve the HTTP
// gateway: a runtime.NewServeMux() with contract.RegisterHippocampusHandlerServer registering every
// generated request_*/response_* handler against a real *hippocampus.Server, calling straight into
// it (no gRPC dial, no interceptor chain - the same shape main.go uses). It is wrapped in
// httptest.NewServer so this test drives real HTTP requests end to end, exercising the gateway's
// request-decode and response-encode paths for essentially every RPC in hippocampus.pb.gw.go.
//
// The underlying *hippocampus.Server runs over an in-memory SQLite db.DB (db.New("")), matching the
// pattern hippocampus/memory_test.go's newTestServer helper uses, and consolidation.enabled is set
// so the Sleep RPC's happy path (not just its replica-rejection branch) is reachable too.
func newGatewayTestServer(t *testing.T) (*httptest.Server, *hippocampus.Server) {
	t.Helper()

	viper.Reset()
	viper.Set("consolidation.enabled", true)
	viper.Set("sleep.periodSeconds", 0)

	database, err := db.New("")
	if err != nil {
		t.Fatalf("failed to create in-memory DB: %s", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	hipo := hippocampus.New(database, nil, nil)
	t.Cleanup(hipo.Stop)

	gwMux := runtime.NewServeMux()
	if err := contract.RegisterHippocampusHandlerServer(context.Background(), gwMux, hipo); err != nil {
		t.Fatalf("failed to register HTTP gateway: %s", err)
	}

	server := httptest.NewServer(gwMux)
	t.Cleanup(server.Close)

	return server, hipo
}

// doJSON issues an HTTP request against the gateway test server and decodes a JSON response body
// into out (when out is non-nil) via protojson - not encoding/json - because the gateway's
// generated response marshaler (runtime.JSONPb, over protojson) writes response fields under their
// lowerCamelCase JSON name (e.g. "totalCount"), not the snake_case name protoc-gen-go's `json:"..."`
// struct tags carry (those exist for encoding/json compatibility on the request side, where
// protojson.Unmarshal is deliberately lenient about accepting either form - it's only marshaling
// that is strict about the camelCase name). It returns the raw status code and body so callers can
// also assert on error responses.
func doJSON(t *testing.T, server *httptest.Server, method string, path string, body any, out proto.Message) (int, []byte) {
	t.Helper()

	var reader io.Reader

	switch b := body.(type) {

	case nil:
		reader = nil

	case []byte:
		reader = bytes.NewReader(b)

	default:
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request body: %s", err)
		}

		reader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequest(method, server.URL+path, reader)
	if err != nil {
		t.Fatalf("build request %s %s: %s", method, path, err)
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request %s %s: %s", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body for %s %s: %s", method, path, err)
	}

	if out != nil && resp.StatusCode == http.StatusOK {
		if err := protojson.Unmarshal(respBody, out); err != nil {
			t.Fatalf("unmarshal response body for %s %s: %s\nbody: %s", method, path, err, respBody)
		}
	}

	return resp.StatusCode, respBody
}

// TestGatewayEndToEnd drives real HTTP requests at every RPC's REST path through a real
// *hippocampus.Server, in dependency order (create an event before recalling one of its memories,
// and so on), so the whole exercise doubles as a smoke test of the gateway wiring itself, not just
// isolated per-RPC calls.
func TestGatewayEndToEnd(t *testing.T) {
	server, _ := newGatewayTestServer(t)
	exerciseGatewayRPCs(t, server)
}

// exerciseGatewayRPCs drives every RPC's REST path against server, in dependency order (create an
// event before recalling one of its memories, and so on). It is shared by two gateway wirings:
// TestGatewayEndToEnd's contract.RegisterHippocampusHandlerServer (calls straight into the
// *hippocampus.Server, as main.go does) and TestGatewayClientForwarding's
// contract.RegisterHippocampusHandlerClient (forwards over a real gRPC connection to a real
// grpc.Server) - between them exercising both the "local_request_" and "request_" halves of every
// generated handler in hippocampus.pb.gw.go.
func exerciseGatewayRPCs(t *testing.T, server *httptest.Server) {
	t.Helper()

	// --- Events ---

	var storeEventRes contract.StoreEventResponse

	status, _ := doJSON(t, server, http.MethodPost, "/v1/events", map[string]any{
		"name":         "event one",
		"significance": 5,
		"group":        "test-group",
	}, &storeEventRes)

	if status != http.StatusOK {
		t.Fatalf("StoreEvent: status = %d", status)
	}

	eventID := storeEventRes.GetId()
	if eventID == "" {
		t.Fatal("StoreEvent: expected a non-empty id")
	}

	var secondEventRes contract.StoreEventResponse

	if status, _ := doJSON(t, server, http.MethodPost, "/v1/events", map[string]any{
		"name":         "event two",
		"significance": 3,
	}, &secondEventRes); status != http.StatusOK {
		t.Fatalf("StoreEvent (second): status = %d", status)
	}

	secondEventID := secondEventRes.GetId()

	var getEventRes contract.GetEventResponse

	if status, _ := doJSON(t, server, http.MethodGet, "/v1/events/"+eventID+"?memories=true", nil, &getEventRes); status != http.StatusOK {
		t.Fatalf("GetEventById: status = %d", status)
	}

	if got := getEventRes.GetEvent().GetId(); got != eventID {
		t.Errorf("GetEventById: got id %q, want %q", got, eventID)
	}

	var getEventsRes contract.GetEventsResponse

	if status, _ := doJSON(t, server, http.MethodGet, "/v1/events?limit=10&order_by=significance", nil, &getEventsRes); status != http.StatusOK {
		t.Fatalf("GetEvents: status = %d", status)
	}

	if got := getEventsRes.GetTotalCount(); got < 2 {
		t.Errorf("GetEvents: total_count = %d, want >= 2", got)
	}

	var endEventRes contract.GeneralResponse

	if status, _ := doJSON(t, server, http.MethodPost, "/v1/events/"+eventID+"/end", map[string]any{}, &endEventRes); status != http.StatusOK {
		t.Fatalf("EndEvent: status = %d", status)
	}

	if !endEventRes.GetOk() {
		t.Error("EndEvent: expected ok=true")
	}

	var updateSigRes contract.GeneralResponse

	if status, _ := doJSON(t, server, http.MethodPatch, "/v1/events/"+eventID+"/significance", map[string]any{
		"significance": 7,
	}, &updateSigRes); status != http.StatusOK {
		t.Fatalf("UpdateEventSignificance: status = %d", status)
	}

	var mergeRes contract.GeneralResponse

	if status, _ := doJSON(t, server, http.MethodPost, "/v1/events/merge", map[string]any{
		"merge_to":   eventID,
		"merge_from": secondEventID,
	}, &mergeRes); status != http.StatusOK {
		t.Fatalf("MergeEvents: status = %d", status)
	}

	// --- Memories ---

	var storeMemRes contract.StoreMemoryResponse

	if status, _ := doJSON(t, server, http.MethodPost, "/v1/memories", map[string]any{
		"significance": 4,
		"body":         "first memory",
		"event_id":     eventID,
		"group":        "test-group",
	}, &storeMemRes); status != http.StatusOK {
		t.Fatalf("StoreMemory: status = %d", status)
	}

	memoryID := storeMemRes.GetId()
	if memoryID == "" {
		t.Fatal("StoreMemory: expected a non-empty id")
	}

	var secondMemRes contract.StoreMemoryResponse

	if status, _ := doJSON(t, server, http.MethodPost, "/v1/memories", map[string]any{
		"significance": 2,
		"body":         "second memory",
		"event_id":     eventID,
	}, &secondMemRes); status != http.StatusOK {
		t.Fatalf("StoreMemory (second): status = %d", status)
	}

	secondMemoryID := secondMemRes.GetId()

	var updateMemRes contract.GeneralResponse

	if status, _ := doJSON(t, server, http.MethodPatch, "/v1/memories/"+memoryID, map[string]any{
		"body": "first memory, updated",
	}, &updateMemRes); status != http.StatusOK {
		t.Fatalf("UpdateMemory: status = %d", status)
	}

	var getMemsRes contract.GetMemoriesResponse

	if status, _ := doJSON(t, server, http.MethodGet, "/v1/memories?limit=10", nil, &getMemsRes); status != http.StatusOK {
		t.Fatalf("GetMemories: status = %d", status)
	}

	if got := getMemsRes.GetTotalCount(); got < 2 {
		t.Errorf("GetMemories: total_count = %d, want >= 2", got)
	}

	var recallRes contract.GetMemoriesResponse

	if status, _ := doJSON(t, server, http.MethodPost, "/v1/memories/recall", map[string]any{
		"ids": []string{memoryID},
	}, &recallRes); status != http.StatusOK {
		t.Fatalf("RecallMemories: status = %d", status)
	}

	if len(recallRes.GetMemories()) != 1 {
		t.Errorf("RecallMemories: got %d memories, want 1", len(recallRes.GetMemories()))
	}

	// SearchMemories requires opensearch.enabled; this server has no search index configured, so
	// the expected outcome is the FailedPrecondition branch, not a successful search - still real
	// gateway/handler code, just its error-mapping path.
	if status, body := doJSON(t, server, http.MethodPost, "/v1/memories/search", map[string]any{
		"query": "memory",
	}, nil); status == http.StatusOK {
		t.Errorf("SearchMemories: expected an error status (search index not configured), got 200: %s", body)
	}

	// --- Summarization ---

	var summaryRes contract.ReplaceMemoriesWithSummaryResponse

	if status, _ := doJSON(t, server, http.MethodPost, "/v1/events/"+eventID+"/summary", map[string]any{
		"significance": 9,
		"body":         "condensed summary",
	}, &summaryRes); status != http.StatusOK {
		t.Fatalf("ReplaceMemoriesWithSummary: status = %d", status)
	}

	if summaryRes.GetId() == "" {
		t.Error("ReplaceMemoriesWithSummary: expected a non-empty summary memory id")
	}

	var candidatesRes contract.GetSummarizationCandidatesResponse

	if status, _ := doJSON(t, server, http.MethodGet, "/v1/summarization/candidates", nil, &candidatesRes); status != http.StatusOK {
		t.Fatalf("GetSummarizationCandidates: status = %d", status)
	}

	// --- Delete / cleanup RPCs ---

	var deleteMemsRes contract.GeneralResponse

	if status, _ := doJSON(t, server, http.MethodPost, "/v1/memories/delete", map[string]any{
		"ids": []string{secondMemoryID},
	}, &deleteMemsRes); status != http.StatusOK {
		t.Fatalf("DeleteMemories: status = %d", status)
	}

	var deleteEventRes contract.GeneralResponse

	if status, body := doJSON(t, server, http.MethodDelete, "/v1/events/"+secondEventID+"?memories=true", nil, &deleteEventRes); status != http.StatusOK {
		t.Fatalf("DeleteEvent: status = %d, body = %s", status, body)
	}

	// --- Sleep / Purge ---

	var sleepRes contract.GeneralResponse

	if status, _ := doJSON(t, server, http.MethodPost, "/v1/sleep", map[string]any{}, &sleepRes); status != http.StatusOK {
		t.Fatalf("Sleep: status = %d", status)
	}

	if !sleepRes.GetOk() {
		t.Error("Sleep: expected ok=true")
	}

	// --- Transfer/archive surface: exercised for their (real, reachable-without-infra) error
	// branches, since none of S3/a transfer target is configured for this test server. ---

	if status, body := doJSON(t, server, http.MethodPost, "/v1/export", map[string]any{}, nil); status == http.StatusOK {
		t.Errorf("Export: expected an error status (no object store configured), got 200: %s", body)
	}

	if status, body := doJSON(t, server, http.MethodPost, "/v1/import", map[string]any{"object_key": "x"}, nil); status == http.StatusOK {
		t.Errorf("Import: expected an error status (no object store configured), got 200: %s", body)
	}

	var importBatchRes contract.ImportBatchResponse

	if status, _ := doJSON(t, server, http.MethodPost, "/v1/import/batch", map[string]any{
		"events": []map[string]any{
			{"id": "imported-event", "name": "imported", "time_start": 1, "significance": 1},
		},
		"memories": []map[string]any{
			{"id": "imported-memory", "event_id": "imported-event", "time_stamp": 1, "significance": 1, "body": "x"},
		},
	}, &importBatchRes); status != http.StatusOK {
		t.Fatalf("ImportBatch: status = %d", status)
	}

	if importBatchRes.GetEventsImported() != 1 || importBatchRes.GetMemoriesImported() != 1 {
		t.Errorf("ImportBatch: got events=%d memories=%d, want 1/1", importBatchRes.GetEventsImported(), importBatchRes.GetMemoriesImported())
	}

	if status, body := doJSON(t, server, http.MethodPost, "/v1/transfer", map[string]any{}, nil); status == http.StatusOK {
		t.Errorf("Transfer: expected an error status (no transfer target configured), got 200: %s", body)
	}

	if status, body := doJSON(t, server, http.MethodPost, "/v1/clear", map[string]any{"manifest_id": "does-not-exist"}, nil); status == http.StatusOK {
		t.Errorf("Clear: expected an error status (unknown manifest), got 200: %s", body)
	}

	// --- Purge last: it wipes everything the rest of the test relies on. ---

	var purgeRes contract.GeneralResponse

	if status, _ := doJSON(t, server, http.MethodPost, "/v1/purge", map[string]any{}, &purgeRes); status != http.StatusOK {
		t.Fatalf("Purge: status = %d", status)
	}

	if !purgeRes.GetOk() {
		t.Error("Purge: expected ok=true")
	}
}

// TestGatewayMalformedRequests exercises the gateway's error-mapping branches for input the
// generated request_* decoders themselves reject, before ever reaching the *hippocampus.Server
// handler: an unparsable JSON body and a request missing a required path parameter.
func TestGatewayMalformedRequests(t *testing.T) {
	server, _ := newGatewayTestServer(t)

	t.Run("malformed JSON body", func(t *testing.T) {
		status, body := doJSON(t, server, http.MethodPost, "/v1/events", []byte("{not valid json"), nil)

		if status == http.StatusOK {
			t.Errorf("expected a non-200 status for malformed JSON, got 200: %s", body)
		}
	})

	t.Run("missing required path parameter falls through to 404", func(t *testing.T) {
		// /v1/events/{id}/end with an empty {id} segment does not match the registered pattern at
		// all, so the gateway mux itself reports it as not found rather than routing into EndEvent.
		status, body := doJSON(t, server, http.MethodPost, "/v1/events//end", map[string]any{}, nil)

		if status == http.StatusOK {
			t.Errorf("expected a non-200 status for a missing path parameter, got 200: %s", body)
		}
	})

	t.Run("wrong HTTP method", func(t *testing.T) {
		status, body := doJSON(t, server, http.MethodGet, "/v1/purge", nil, nil)

		if status == http.StatusOK {
			t.Errorf("expected a non-200 status for GET on a POST-only route, got 200: %s", body)
		}
	})

	t.Run("unknown route", func(t *testing.T) {
		status, body := doJSON(t, server, http.MethodGet, "/v1/does-not-exist", nil, nil)

		if status != http.StatusNotFound {
			t.Errorf("expected 404 for an unregistered route, got %d: %s", status, body)
		}
	})

	exerciseGatewayErrorPaths(t, server)
}

// exerciseGatewayErrorPaths drives the request-decode error branches of hippocampus.pb.gw.go that
// TestGatewayEndToEnd's happy-path walk never reaches: a malformed JSON body on every route whose
// generated handler decodes one, and an unparsable query parameter on every route whose generated
// handler populates one via runtime.PopulateQueryParameters. It is shared by
// TestGatewayMalformedRequests (server, the local_request_* / direct-to-*hippocampus.Server
// gateway) and TestGatewayClientForwardingMalformedRequests (the request_* / client-forwarding
// gateway) - between them exercising both halves of every generated decode-error branch, mirroring
// how exerciseGatewayRPCs covers both halves of the happy path.
func exerciseGatewayErrorPaths(t *testing.T, server *httptest.Server) {
	t.Helper()

	badJSON := []byte("{not valid json")

	jsonRoutes := []struct {
		name   string
		method string
		path   string
	}{
		{"EndEvent", http.MethodPost, "/v1/events/does-not-exist/end"},
		{"UpdateEventSignificance", http.MethodPatch, "/v1/events/does-not-exist/significance"},
		{"MergeEvents", http.MethodPost, "/v1/events/merge"},
		{"StoreMemory", http.MethodPost, "/v1/memories"},
		{"UpdateMemory", http.MethodPatch, "/v1/memories/does-not-exist"},
		{"DeleteMemories", http.MethodPost, "/v1/memories/delete"},
		{"RecallMemories", http.MethodPost, "/v1/memories/recall"},
		{"SearchMemories", http.MethodPost, "/v1/memories/search"},
		{"ReplaceMemoriesWithSummary", http.MethodPost, "/v1/events/does-not-exist/summary"},
		{"Export", http.MethodPost, "/v1/export"},
		{"Import", http.MethodPost, "/v1/import"},
		{"ImportBatch", http.MethodPost, "/v1/import/batch"},
		{"Transfer", http.MethodPost, "/v1/transfer"},
		{"Clear", http.MethodPost, "/v1/clear"},
	}

	for _, route := range jsonRoutes {
		t.Run(route.name+" malformed JSON body", func(t *testing.T) {
			status, body := doJSON(t, server, route.method, route.path, badJSON, nil)

			if status == http.StatusOK {
				t.Errorf("%s: expected a non-200 status for malformed JSON, got 200: %s", route.name, body)
			}
		})
	}

	queryRoutes := []struct {
		name   string
		method string
		path   string
	}{
		{"GetEventById", http.MethodGet, "/v1/events/does-not-exist?memories=not-a-bool"},
		{"DeleteEvent", http.MethodDelete, "/v1/events/does-not-exist?memories=not-a-bool"},
		{"GetEvents", http.MethodGet, "/v1/events?limit=not-a-number"},
		{"GetMemories", http.MethodGet, "/v1/memories?limit=not-a-number"},
		// Invalid percent-encoding (a lone "%zz") fails req.ParseForm() itself, a branch distinct
		// from - and evaluated before - runtime.PopulateQueryParameters's type-mismatch branch above.
		{"GetEventById unparsable query string", http.MethodGet, "/v1/events/does-not-exist?%zz"},
		{"DeleteEvent unparsable query string", http.MethodDelete, "/v1/events/does-not-exist?%zz"},
		{"GetEvents unparsable query string", http.MethodGet, "/v1/events?%zz"},
		{"GetMemories unparsable query string", http.MethodGet, "/v1/memories?%zz"},
	}

	for _, route := range queryRoutes {
		t.Run(route.name+" malformed query parameter", func(t *testing.T) {
			status, body := doJSON(t, server, route.method, route.path, nil, nil)

			if status == http.StatusOK {
				t.Errorf("%s: expected a non-200 status for a malformed query parameter, got 200: %s", route.name, body)
			}
		})
	}
}

// TestGatewayGetEventByIdNotFound exercises GetEventById's NotFound path through the gateway, a
// distinct branch from the malformed-request cases above (well-formed request, valid path
// parameter, but no such event).
func TestGatewayGetEventByIdNotFound(t *testing.T) {
	server, _ := newGatewayTestServer(t)

	status, body := doJSON(t, server, http.MethodGet, "/v1/events/does-not-exist", nil, nil)
	if status == http.StatusOK {
		t.Fatalf("expected an error status for an unknown event id, got 200: %s", body)
	}
}

// TestGatewaySleepRejectedOnReplica exercises Sleep's replica-rejection branch (consolidation.
// enabled: false), the one Sleep outcome TestGatewayEndToEnd's consolidation.enabled: true server
// cannot reach.
func TestGatewaySleepRejectedOnReplica(t *testing.T) {
	viper.Reset()
	viper.Set("consolidation.enabled", false)
	viper.Set("sleep.periodSeconds", 0)

	database, err := db.New("")
	if err != nil {
		t.Fatalf("failed to create in-memory DB: %s", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	hipo := hippocampus.New(database, nil, nil)
	t.Cleanup(hipo.Stop)

	gwMux := runtime.NewServeMux()
	if err := contract.RegisterHippocampusHandlerServer(context.Background(), gwMux, hipo); err != nil {
		t.Fatalf("failed to register HTTP gateway: %s", err)
	}

	server := httptest.NewServer(gwMux)
	t.Cleanup(server.Close)

	status, body := doJSON(t, server, http.MethodPost, "/v1/sleep", map[string]any{}, nil)
	if status == http.StatusOK {
		t.Fatalf("expected Sleep to be rejected on a read/write replica, got 200: %s", body)
	}
}
