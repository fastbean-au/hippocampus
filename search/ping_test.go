package search

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

// healthTransport answers cluster-health requests with a chosen body and lets every other request
// (ensureIndex's, during construction) succeed. The shared fakeTransport cannot do this: it returns
// a fixed "{}" body, and what this test is about is the field inside the body.
type healthTransport struct {
	body string
	err  error

	// path records what the probe actually asked for, which is half the point: a whole-cluster
	// health check would report an unrelated red index as this one's problem, and would miss a red
	// index in an otherwise green cluster.
	path string
}

func (h *healthTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !strings.HasPrefix(req.URL.Path, "/_cluster/health") {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{}`)),
			Request:    req,
		}, nil
	}

	h.path = req.URL.Path

	if h.err != nil {
		return nil, h.err
	}

	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(h.body)),
		Request:    req,
	}, nil
}

func newPingIndex(t *testing.T, transport http.RoundTripper) *OpenSearch {
	t.Helper()

	idx, err := NewOpenSearch(Config{
		Addresses: []string{"http://opensearch.invalid:9200"},
		Index:     "test-index",
		QueueSize: 4,
		Transport: transport,
	})
	if err != nil {
		t.Fatalf("NewOpenSearch: %s", err)
	}

	t.Cleanup(func() { _ = idx.Close() })

	return idx
}

// TestOpenSearch_Ping covers the deployment-topology probe.
//
// Yellow is deliberately healthy. Every single-node deployment - all of the compose stacks here,
// and plenty of real ones - is permanently yellow, because its replica shards have nowhere to be
// assigned; treating that as a fault would leave the view permanently amber and teach an operator
// to ignore the colour, which is worse than not showing it. Red is degraded rather than
// unreachable, because the cluster answered and search may still be serving from the shards that
// are assigned.
func TestOpenSearch_Ping(t *testing.T) {
	for name, tc := range map[string]struct {
		body         string
		transportErr error
		wantErr      bool
		wantDegraded bool
	}{
		"green":  {body: `{"status":"green"}`},
		"yellow": {body: `{"status":"yellow"}`},
		"red": {
			body:         `{"status":"red","unassigned_shards":3}`,
			wantErr:      true,
			wantDegraded: true,
		},
		"unreachable": {
			transportErr: errors.New("connection refused"),
			wantErr:      true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			transport := &healthTransport{body: tc.body, err: tc.transportErr}
			idx := newPingIndex(t, transport)

			err := idx.Ping(context.Background())

			if tc.wantErr && err == nil {
				t.Fatal("Ping reported success")
			}

			if !tc.wantErr && err != nil {
				t.Fatalf("Ping: %s", err)
			}

			if degraded := errors.Is(err, ErrDegraded); degraded != tc.wantDegraded {
				t.Errorf("degraded = %v, want %v (err: %v)", degraded, tc.wantDegraded, err)
			}

			if !strings.HasSuffix(transport.path, "/test-index") {
				t.Errorf("Ping asked for %q; it must scope the health check to this index", transport.path)
			}
		})
	}
}

// TestOpenSearch_PingNamesTheIndex checks that a failure says which index is unhappy. A probe whose
// message is just "degraded" sends an operator to the cluster with nothing to look for.
func TestOpenSearch_PingNamesTheIndex(t *testing.T) {
	idx := newPingIndex(t, &healthTransport{body: `{"status":"red","unassigned_shards":2}`})

	err := idx.Ping(context.Background())
	if err == nil {
		t.Fatal("Ping reported success on a red index")
	}

	if !strings.Contains(err.Error(), "test-index") {
		t.Errorf("the error does not name the index: %s", err)
	}

	if !strings.Contains(err.Error(), fmt.Sprint(2)) {
		t.Errorf("the error does not report the unassigned shards: %s", err)
	}
}
