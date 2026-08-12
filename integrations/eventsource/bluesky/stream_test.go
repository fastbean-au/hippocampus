package bluesky

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// These exercise the real dial and read path against a local websocket server, so the one file in
// the package that opens a socket is covered without depending on Bluesky's infrastructure. The
// env-gated test in integration_test.go covers the real endpoint.

// jetstreamServer starts a websocket server that sends frames and records the query it was called
// with. It returns the ws:// URL.
func jetstreamServer(t *testing.T, frames []string, query *string) string {
	t.Helper()

	upgrader := websocket.Upgrader{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if query != nil {
			*query = r.URL.RawQuery
		}

		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}

		defer func() { _ = conn.Close() }()

		for _, v := range frames {
			if err := conn.WriteMessage(websocket.TextMessage, []byte(v)); err != nil {
				return
			}
		}

		// Hold the connection open so a reader blocking for the next frame is exercised.
		time.Sleep(2 * time.Second)
	}))

	t.Cleanup(srv.Close)

	return "ws" + strings.TrimPrefix(srv.URL, "http")
}

func TestDefaultDialReadsFrames(t *testing.T) {
	var query string

	url := jetstreamServer(t, []string{string(postJSON(10, "a", "hello", ""))}, &query)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	s, err := defaultDial(ctx, Config{
		URL:         url,
		Collections: []string{CollectionPost},
		ReadTimeout: 2 * time.Second,
	}, 4210)
	if err != nil {
		t.Fatalf("defaultDial: %s", err)
	}

	defer func() { _ = s.Close() }()

	// The subscription parameters really did reach the server.
	if !strings.Contains(query, "wantedCollections=app.bsky.feed.post") {
		t.Errorf("query = %q, want the collection", query)
	}

	if !strings.Contains(query, "cursor=4210") {
		t.Errorf("query = %q, want the cursor", query)
	}

	frame, err := s.Next(ctx)
	if err != nil {
		t.Fatalf("Next: %s", err)
	}

	if !strings.Contains(string(frame), "hello") {
		t.Errorf("frame = %q", frame)
	}
}

// TestDefaultDialReportsAConnectionFailure covers the error path a reconnect loop depends on.
func TestDefaultDialReportsAConnectionFailure(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := defaultDial(ctx, Config{URL: "ws://127.0.0.1:1"}, 0); err == nil {
		t.Error("expected dialling a dead port to fail")
	}
}

func TestDefaultDialRejectsAMalformedURL(t *testing.T) {
	if _, err := defaultDial(context.Background(), Config{URL: "://not a url"}, 0); err == nil {
		t.Error("expected a malformed URL to be reported")
	}
}

// TestDefaultDialReportsAnHTTPStatus: a plain HTTP handler that never upgrades is what a
// misconfigured URL looks like, and the status is the useful part of the message.
func TestDefaultDialReportsAnHTTPStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))

	defer srv.Close()

	_, err := defaultDial(context.Background(), Config{URL: "ws" + strings.TrimPrefix(srv.URL, "http")}, 0)
	if err == nil {
		t.Fatal("expected the failed upgrade to be reported")
	}

	if !strings.Contains(err.Error(), "418") {
		t.Errorf("error %q does not name the HTTP status", err)
	}
}

// TestStreamNextReturnsWhenTheContextIsCancelled is the reason Next takes a context at all: the
// underlying ReadMessage takes only a deadline, so a blocked read would otherwise wait out the full
// read timeout after a shutdown.
func TestStreamNextReturnsWhenTheContextIsCancelled(t *testing.T) {
	url := jetstreamServer(t, nil, nil)

	s, err := defaultDial(context.Background(), Config{URL: url, ReadTimeout: time.Hour}, 0)
	if err != nil {
		t.Fatalf("defaultDial: %s", err)
	}

	defer func() { _ = s.Close() }()

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)

	go func() {
		_, nextErr := s.Next(ctx)
		done <- nextErr
	}()

	// Let the read block, then cancel.
	time.Sleep(100 * time.Millisecond)
	cancel()

	select {

	case err := <-done:
		if err == nil {
			t.Error("expected Next to report the cancellation")
		}

	case <-time.After(3 * time.Second):
		t.Fatal("Next did not return promptly after its context was cancelled")
	}
}

// TestStreamNextTimesOutOnSilence: the firehose is never idle, so a connection with nothing on it is
// black-holed rather than quiet, and the read deadline is what turns that into a reconnect.
func TestStreamNextTimesOutOnSilence(t *testing.T) {
	url := jetstreamServer(t, nil, nil)

	s, err := defaultDial(context.Background(), Config{URL: url, ReadTimeout: 200 * time.Millisecond}, 0)
	if err != nil {
		t.Fatalf("defaultDial: %s", err)
	}

	defer func() { _ = s.Close() }()

	start := time.Now()

	if _, err := s.Next(context.Background()); err == nil {
		t.Fatal("expected the read deadline to fire on a silent connection")
	}

	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("Next blocked for %s, want the read timeout to have fired", elapsed)
	}
}

// TestStreamNextFailsOnAClosedConnection covers the read-error path after Close.
func TestStreamNextFailsOnAClosedConnection(t *testing.T) {
	url := jetstreamServer(t, nil, nil)

	s, err := defaultDial(context.Background(), Config{URL: url, ReadTimeout: time.Second}, 0)
	if err != nil {
		t.Fatalf("defaultDial: %s", err)
	}

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %s", err)
	}

	if _, err := s.Next(context.Background()); err == nil {
		t.Error("expected reading a closed connection to fail")
	}
}
