package main

import (
	"context"
	"net/http"
	"testing"

	"github.com/fastbean-au/hippocampus/contract"
)

// TestHTTPClientLinkRPCs pins each link RPC's method, path and body binding against the fake
// gateway. They are one-line methods, but the mapping is exactly what a typo silently breaks: the
// generated gRPC client would still compile, and a wrong path only shows up as a 404 at runtime.
func TestHTTPClientLinkRPCs(t *testing.T) {
	tests := []struct {
		name       string
		call       func(*httpClient) error
		wantMethod string
		wantPath   string
		// wantBody is a substring the request body must carry, empty when the RPC sends none.
		wantBody string
		// wantQuery is a query parameter that must be present, empty when none is expected.
		wantQuery string
		wantValue string
	}{
		{
			name: "LinkMemories",
			call: func(c *httpClient) error {
				_, err := c.LinkMemories(context.Background(), &contract.LinkMemoriesRequest{
					Id:    "m1",
					Links: []*contract.Link{{Id: "m2", Significance: 5}},
				})

				return err
			},
			wantMethod: http.MethodPost,
			wantPath:   "/v1/memories/m1/links",
			wantBody:   `"m2"`,
		},
		{
			name: "UnlinkMemories",
			call: func(c *httpClient) error {
				_, err := c.UnlinkMemories(context.Background(), &contract.UnlinkMemoriesRequest{
					Id:  "m1",
					Ids: []string{"m2"},
				})

				return err
			},
			wantMethod: http.MethodPost,
			wantPath:   "/v1/memories/m1/links/delete",
			wantBody:   `"m2"`,
		},
		{
			name: "GetMemoryLinks",
			call: func(c *httpClient) error {
				_, err := c.GetMemoryLinks(context.Background(), &contract.GetMemoryLinksRequest{
					Id:        "m1",
					Direction: contract.LinkDirection_LINK_DIRECTION_INBOUND,
				})

				return err
			},
			wantMethod: http.MethodGet,
			wantPath:   "/v1/memories/m1/links",
			wantQuery:  "direction",
			wantValue:  "LINK_DIRECTION_INBOUND",
		},
		{
			name: "LinkEvents",
			call: func(c *httpClient) error {
				_, err := c.LinkEvents(context.Background(), &contract.LinkEventsRequest{
					Id:    "e1",
					Links: []*contract.Link{{Id: "e2", Significance: 3}},
				})

				return err
			},
			wantMethod: http.MethodPost,
			wantPath:   "/v1/events/e1/links",
			wantBody:   `"e2"`,
		},
		{
			name: "UnlinkEvents",
			call: func(c *httpClient) error {
				_, err := c.UnlinkEvents(context.Background(), &contract.UnlinkEventsRequest{
					Id:  "e1",
					Ids: []string{"e2"},
				})

				return err
			},
			wantMethod: http.MethodPost,
			wantPath:   "/v1/events/e1/links/delete",
			wantBody:   `"e2"`,
		},
		{
			name: "GetEventLinks",
			call: func(c *httpClient) error {
				_, err := c.GetEventLinks(context.Background(), &contract.GetEventLinksRequest{
					Id:        "e1",
					Direction: contract.LinkDirection_LINK_DIRECTION_OUTBOUND,
				})

				return err
			},
			wantMethod: http.MethodGet,
			wantPath:   "/v1/events/e1/links",
			wantQuery:  "direction",
			wantValue:  "LINK_DIRECTION_OUTBOUND",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client, captured := newTestHTTPClient(t, http.StatusOK, &contract.GeneralResponse{Ok: true})

			if err := test.call(client); err != nil {
				t.Fatalf("%s: %s", test.name, err)
			}

			if captured.method != test.wantMethod {
				t.Errorf("method = %q, want %q", captured.method, test.wantMethod)
			}

			if captured.path != test.wantPath {
				t.Errorf("path = %q, want %q", captured.path, test.wantPath)
			}

			if test.wantBody != "" && !containsString(string(captured.body), test.wantBody) {
				t.Errorf("body = %q, want it to carry %q", captured.body, test.wantBody)
			}

			if test.wantQuery != "" {
				if got := captured.query.Get(test.wantQuery); got != test.wantValue {
					t.Errorf("query %q = %q, want %q", test.wantQuery, got, test.wantValue)
				}
			}

			// The id belongs in the path, never repeated as a query parameter.
			if got := captured.query.Get("id"); got != "" {
				t.Errorf("expected the id to stay in the path, found it in the query as %q", got)
			}
		})
	}
}

// TestHTTPClientPreviewConsolidation pins the dry run's binding. It is a GET with its arguments in
// the query, since it changes nothing.
func TestHTTPClientPreviewConsolidation(t *testing.T) {
	client, captured := newTestHTTPClient(t, http.StatusOK, &contract.PreviewConsolidationResponse{})

	if _, err := client.PreviewConsolidation(context.Background(), &contract.PreviewConsolidationRequest{Limit: 25}); err != nil {
		t.Fatalf("PreviewConsolidation: %s", err)
	}

	if captured.method != http.MethodGet {
		t.Errorf("method = %q, want GET", captured.method)
	}

	if captured.path != "/v1/sleep/preview" {
		t.Errorf("path = %q, want /v1/sleep/preview", captured.path)
	}

	if got := captured.query.Get("limit"); got != "25" {
		t.Errorf("query limit = %q, want 25", got)
	}
}

// containsString avoids a strings import for the body assertions above.
func containsString(haystack string, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}

	return false
}
