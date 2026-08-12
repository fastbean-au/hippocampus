package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"

	"github.com/fastbean-au/hippocampus/contract"
)

// slashyID is an id of the shape the Bluesky bridge writes: an at:// URI, whose id therefore
// contains the two characters a URL path is built out of - "/" and ":". Ids are caller-chosen, so
// nothing stops any client writing one, and the event-source bridges make it the common case.
const slashyID = "at://did:plc:jbvnehrrdqoulco4rf5gxg5r/app.bsky.feed.post/3msuaulefjg22"

// escapePathID escapes an id for a single path segment the way a client must, and the way the
// console's encodeURIComponent does: every character that means something to the router - "/", and
// ":" (which grpc-gateway reads as the start of a custom verb in the final component) - is
// percent-encoded, so the whole id occupies exactly one segment.
func escapePathID(id string) string {
	return strings.ReplaceAll(url.PathEscape(id), ":", "%3A")
}

// TestGatewayRoutesAnEscapedIdInEveryPath is the regression guard for ids containing a slash. The
// gateway defaulted to runtime.UnescapingModeLegacy, which routes on r.URL.Path - already fully
// unescaped by net/url, so a %2F in an id had by then become a real "/" and split the id across
// path segments. Every route addressing a record by id therefore 404'd for any at:// id, which is
// most of what the console's row actions do (edit, recall, delete, links) and every by-id call the
// CLI and the Obsidian plugin make.
//
// It is driven off the published OpenAPI description rather than a hand-written path list, like
// TestHTTPMetricsMiddleware_NamesEveryPublishedRoute, so a route added later is covered without
// anyone remembering to add it here. Any status but 404 means the request was routed: the handlers
// all answer Unimplemented (501), which is enough - what is being asserted is the matching.
func TestGatewayRoutesAnEscapedIdInEveryPath(t *testing.T) {
	var spec struct {
		Paths map[string]map[string]struct {
			OperationID string `json:"operationId"`
		} `json:"paths"`
	}

	if err := json.Unmarshal(contract.SwaggerJSON, &spec); err != nil {
		t.Fatalf("failed to read the embedded OpenAPI description: %s", err.Error())
	}

	gwMux := runtime.NewServeMux(gatewayMuxOptions()...)

	if err := contract.RegisterHippocampusHandlerServer(context.Background(), gwMux, contract.UnimplementedHippocampusServer{}); err != nil {
		t.Fatalf("failed to register the gateway: %s", err.Error())
	}

	escaped := escapePathID(slashyID)
	tested := 0

	for path, operations := range spec.Paths {
		if !captureSegment.MatchString(path) {
			continue
		}

		tested++

		for verb := range operations {
			t.Run(strings.ToUpper(verb)+" "+path, func(t *testing.T) {
				concrete := captureSegment.ReplaceAllString(path, escaped)

				request := httptest.NewRequest(strings.ToUpper(verb), concrete, strings.NewReader("{}"))
				request.Header.Set("Content-Type", "application/json")

				recorder := httptest.NewRecorder()

				gwMux.ServeHTTP(recorder, request)

				if recorder.Code == http.StatusNotFound {
					t.Errorf("%s %s did not route an escaped id: got 404\nbody: %s",
						strings.ToUpper(verb), concrete, recorder.Body.String())
				}
			})
		}
	}

	if tested == 0 {
		t.Fatal("the embedded OpenAPI description declares no path carrying an id")
	}
}

// recordingLinksServer answers one RPC and records the id the gateway decoded out of the path.
type recordingLinksServer struct {
	contract.UnimplementedHippocampusServer

	id string
}

func (s *recordingLinksServer) GetMemoryLinks(_ context.Context, req *contract.GetMemoryLinksRequest) (*contract.GetLinksResponse, error) {
	s.id = req.GetId()

	return &contract.GetLinksResponse{}, nil
}

// TestGatewayDecodesAnEscapedIdExactly is the other half: routing the request is worth nothing if
// the handler is then handed a mangled id. The id must arrive byte-for-byte as it was written -
// ids are compared exactly by every store driver - so a still-escaped or double-unescaped one
// would simply be a NotFound against a record that exists.
func TestGatewayDecodesAnEscapedIdExactly(t *testing.T) {
	recorder := &recordingLinksServer{}

	gwMux := runtime.NewServeMux(gatewayMuxOptions()...)

	if err := contract.RegisterHippocampusHandlerServer(context.Background(), gwMux, recorder); err != nil {
		t.Fatalf("failed to register the gateway: %s", err.Error())
	}

	// A percent sign in the id proves the unescaping runs exactly once: written as %25, it must
	// arrive as "%", not stay "%25" (no unescaping) and not become a malformed sequence (twice).
	id := slashyID + "?x=1%y"

	request := httptest.NewRequest(http.MethodGet, "/v1/memories/"+escapePathID(id)+"/links", nil)
	response := httptest.NewRecorder()

	gwMux.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("expected the request to be served, got %d\nbody: %s", response.Code, response.Body.String())
	}

	if recorder.id != id {
		t.Errorf("the handler received %q, want %q", recorder.id, id)
	}
}
