package search

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// blueskyMemoryId is a real-shaped Bluesky memory id: the post's at:// URI, which the eventsource
// bridge uses verbatim as the memory id so engagement can be matched back to a post without any
// correlation state. It carries the characters that make a document id unsafe to interpolate into
// a URL path.
const blueskyMemoryId = "at://did:plc:z72i7hdynmk6r22z27h6tvur/app.bsky.feed.post/3lk2mnopqrs2t"

// pathCapturingTransport records the request path AS IT GOES ON THE WIRE (the escaped form),
// which is the only form that shows whether a document id survived into a single path segment.
// fakeTransport deliberately records the decoded url.URL.Path, which cannot tell the two apart.
type pathCapturingTransport struct {
	mu     sync.Mutex
	paths  []string
	bodies []string
}

func (p *pathCapturingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	body := ""

	if req.Body != nil {
		b, _ := io.ReadAll(req.Body)
		body = string(b)
	}

	p.mu.Lock()
	p.paths = append(p.paths, req.URL.EscapedPath())
	p.bodies = append(p.bodies, body)
	p.mu.Unlock()

	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{}`)),
		Request:    req,
	}, nil
}

// lastBody returns the body of the most recent request, for the assertions that are about what
// travels in a bulk action line rather than in a path.
func (p *pathCapturingTransport) lastBody() string {
	p.mu.Lock()
	defer p.mu.Unlock()

	if len(p.bodies) == 0 {

		return ""
	}

	return p.bodies[len(p.bodies)-1]
}

func (p *pathCapturingTransport) recordedPaths() []string {
	p.mu.Lock()
	defer p.mu.Unlock()

	out := make([]string, len(p.paths))
	copy(out, p.paths)

	return out
}

// TestWarnOnPathPrefixedAddresses covers the address shapes that do and do not defeat the document
// id escaping. It asserts nothing beyond not panicking - the point is the warning, which only a log
// hook could observe - but it pins which addresses reach it.
func TestWarnOnPathPrefixedAddresses(t *testing.T) {
	warnOnPathPrefixedAddresses([]string{
		"http://opensearch.invalid:9200",       // no path at all
		"http://opensearch.invalid:9200/",      // a bare root, not a prefix
		"http://opensearch.invalid:9200/proxy", // a prefix: warned about
		"://not a url",                         // unparseable: skipped, the client already rejected it
	})
}

// TestOpenSearch_DocumentIdIsPathEscaped covers the document id reaching the cluster intact when it
// contains characters that are structural in a URL path.
//
// The Bluesky bridge ids every memory by the post's at:// URI, so most of a real index's ids carry
// slashes. Interpolated raw, "at://did:plc:.../app.bsky.feed.post/3l..." turns one path segment
// into six: the cluster sees a route that is not /<index>/_doc/<id> at all, so the index write is
// rejected and the delete of the same id silently addresses nothing. Nothing in the service fails
// visibly - the memory is simply never searchable.
//
// apply is called directly (as the integration tests do) so the assertion does not depend on
// worker timing.
func TestOpenSearch_DocumentIdIsPathEscaped(t *testing.T) {
	transport := &pathCapturingTransport{}

	idx, err := NewOpenSearch(Config{
		Addresses: []string{"http://opensearch.invalid:9200"},
		Index:     "test-index",
		QueueSize: 16,
		Transport: transport,
	})
	if err != nil {
		t.Fatalf("NewOpenSearch: %s", err)
	}
	defer func() { _ = idx.Close() }()

	// The two paths carry the id differently, and the difference is the point. An index write puts
	// it in the request PATH, where it must be escaped into a single segment. A delete now travels
	// in a _bulk BODY, where it must be RAW - the server percent-decodes a path before storing the
	// _id, so the stored id is the raw string, and a URL-escaped id in a JSON body addresses a
	// document that does not exist. Escaping in the wrong place fails exactly as silently as not
	// escaping in the right one.

	t.Run("index escapes the id into one path segment", func(t *testing.T) {
		before := len(transport.recordedPaths())

		if err := idx.apply(context.Background(), op{kind: opIndex, doc: Doc{Id: blueskyMemoryId, Body: "a post"}}); err != nil {
			t.Fatalf("apply: %s", err)
		}

		paths := transport.recordedPaths()[before:]
		if len(paths) != 1 {
			t.Fatalf("expected exactly one request, got %d: %v", len(paths), paths)
		}

		prefix := "/test-index/_doc/"

		if !strings.HasPrefix(paths[0], prefix) {
			t.Fatalf("expected a document path under %q, got %q", prefix, paths[0])
		}

		segment := strings.TrimPrefix(paths[0], prefix)

		if strings.Contains(segment, "/") {
			t.Fatalf(
				"the document id was not escaped into a single path segment: %q - the cluster sees a different route and the operation is lost",
				paths[0],
			)
		}

		id, err := url.PathUnescape(segment)
		if err != nil {
			t.Fatalf("the document id segment %q is not a valid path escaping: %s", segment, err)
		}

		if id != blueskyMemoryId {
			t.Errorf("expected the escaped segment to decode back to %q, got %q", blueskyMemoryId, id)
		}
	})

	t.Run("delete carries the raw id in the bulk body", func(t *testing.T) {
		before := len(transport.recordedPaths())

		if err := idx.apply(context.Background(), op{kind: opDeleteIds, ids: []string{blueskyMemoryId}}); err != nil {
			t.Fatalf("apply: %s", err)
		}

		paths := transport.recordedPaths()[before:]
		if len(paths) != 1 {
			t.Fatalf("expected exactly one request, got %d: %v", len(paths), paths)
		}

		if paths[0] != "/_bulk" {
			t.Fatalf("expected the delete to go through /_bulk, got %q", paths[0])
		}

		body := transport.lastBody()

		var action struct {
			Delete struct {
				Index string `json:"_index"`
				ID    string `json:"_id"`
			} `json:"delete"`
		}

		if err := json.Unmarshal([]byte(strings.TrimSpace(body)), &action); err != nil {
			t.Fatalf("the bulk body is not a valid action line (%q): %s", body, err)
		}

		if action.Delete.ID != blueskyMemoryId {
			t.Errorf("the bulk delete addresses %q, want the raw id %q - a URL-escaped id here "+
				"targets a document that was never stored under that name",
				action.Delete.ID, blueskyMemoryId)
		}
	})
}
