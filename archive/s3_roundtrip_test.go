package archive

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeS3 is a minimal S3-compatible object store over HTTP: it keeps PUT bodies in memory keyed by
// request path and serves them back on GET. A tiny body uploads as a single PutObject (well under
// the transfer manager's multipart threshold), so PUT and GET are the only verbs exercised.
type fakeS3 struct {
	mu      sync.Mutex
	objects map[string][]byte
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	switch r.Method {

	case http.MethodPut:
		body, _ := io.ReadAll(r.Body)
		f.objects[r.URL.Path] = body
		w.Header().Set("ETag", `"fakeetag"`)
		w.WriteHeader(http.StatusOK)

	case http.MethodGet:
		body, ok := f.objects[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)

			return
		}

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)

	default:
		w.WriteHeader(http.StatusOK)
	}
}

// TestS3Store_PutGetRoundTrip drives the real S3 client wiring against an in-memory endpoint: a
// stored object reads back byte-for-byte, and Get of a missing key surfaces an error.
func TestS3Store_PutGetRoundTrip(t *testing.T) {
	// Static credentials so the default config chain never reaches the network to resolve any.
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("AWS_REGION", "us-east-1")

	server := httptest.NewServer(&fakeS3{objects: map[string][]byte{}})
	t.Cleanup(server.Close)

	store, err := NewS3Store(context.Background(), S3Config{
		Bucket:       "archive-bucket",
		Region:       "us-east-1",
		Endpoint:     server.URL,
		UsePathStyle: true,
	})
	if err != nil {
		t.Fatalf("NewS3Store: %s", err)
	}

	payload := []byte("archive payload bytes")

	if err := store.Put(context.Background(), "snapshot-1", bytes.NewReader(payload)); err != nil {
		t.Fatalf("Put: %s", err)
	}

	rc, err := store.Get(context.Background(), "snapshot-1")
	if err != nil {
		t.Fatalf("Get: %s", err)
	}
	defer func() { _ = rc.Close() }()

	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("reading object body: %s", err)
	}

	if !bytes.Equal(got, payload) {
		t.Fatalf("round-trip mismatch: put %q, got %q", payload, got)
	}

	// A key that was never stored must surface as an error rather than an empty read.
	if _, err := store.Get(context.Background(), "missing-key"); err == nil {
		t.Error("expected an error fetching a missing object")
	} else if !strings.Contains(err.Error(), "missing-key") {
		t.Errorf("expected the error to name the key, got: %s", err)
	}
}
