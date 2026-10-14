package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Check the client-visible tools/list response, including explicit false values. The hints
// describe every supported argument and deployment, not just the default inputs or token role.
func TestServer_ToolAnnotations(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	server := newServer(newBridge(&fakeClient{}), "test")
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return server
	}, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})

	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/", strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`,
	))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", "2025-11-25")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("tools/list status = %d: %s", rec.Code, rec.Body.String())
	}

	var response struct {
		Result struct {
			Tools []struct {
				Name        string          `json:"name"`
				Annotations map[string]bool `json:"annotations"`
			} `json:"tools"`
		} `json:"result"`
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode tools/list: %v", err)
	}
	if response.Error != nil {
		t.Fatalf("tools/list error: %s", response.Error)
	}
	tools := response.Result

	type hints struct {
		readOnly, destructive, idempotent bool
	}
	want := map[string]hints{
		"store_memory": {false, false, false},
		// Repeating a memory update can enqueue another write callback.
		"update_memory": {false, true, false},
		// Repeating a deletion can enqueue another search-outbox entry, even for absent ids.
		"delete_memories": {false, true, false},
		"recall_memories": {false, false, false},
		// reinforce=true mutates recall state, even though the default is a plain search.
		"search_memories": {false, false, false},
		"list_memories":   {true, false, true},
		// Re-linking replaces the existing weight, but preserves the creation time.
		"link_memories":    {false, true, true},
		"unlink_memories":  {false, true, true},
		"get_memory_links": {true, false, true},
		"create_event":     {false, false, false},
		// Omitted time_end uses a new server timestamp on each call.
		"end_event":                    {false, true, false},
		"update_event":                 {false, true, true},
		"get_event":                    {true, false, true},
		"list_events":                  {true, false, true},
		"link_events":                  {false, true, true},
		"unlink_events":                {false, true, true},
		"get_event_links":              {true, false, true},
		"get_summarisation_candidates": {true, false, true},
		"significance_levels":          {true, false, true},
		"explain_consolidation":        {true, false, true},
		"consolidation_status":         {true, false, true},
		"whoami":                       {true, false, true},
	}

	if len(tools.Tools) != len(want) {
		t.Errorf("tool count = %d, want %d", len(tools.Tools), len(want))
	}
	for _, tool := range tools.Tools {
		expected, ok := want[tool.Name]
		if !ok {
			t.Errorf("unexpected tool %q", tool.Name)
			continue
		}
		delete(want, tool.Name)
		t.Run(tool.Name, func(t *testing.T) {
			if tool.Annotations == nil {
				t.Fatal("tools/list omitted annotations")
			}
			got := tool.Annotations
			for key, value := range map[string]bool{
				"readOnlyHint":    expected.readOnly,
				"destructiveHint": expected.destructive,
				"idempotentHint":  expected.idempotent,
			} {
				if actual, present := got[key]; !present || actual != value {
					t.Errorf("%s = %v (present=%v), want explicit %v", key, actual, present, value)
				}
			}
		})
	}
	for name := range want {
		t.Errorf("missing tool %q", name)
	}
}
