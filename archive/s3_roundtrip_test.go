package archive

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
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

	case http.MethodDelete:
		delete(f.objects, r.URL.Path)
		w.WriteHeader(http.StatusNoContent)

	case http.MethodGet:
		// ListObjectsV2 is a GET on the bucket with list-type=2; anything else is a GetObject.
		if r.URL.Query().Get("list-type") == "2" {
			f.list(w, r)

			return
		}

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

// list answers ListObjectsV2 for the bucket in the request path, filtered by prefix, in one page.
func (f *fakeS3) list(w http.ResponseWriter, r *http.Request) {
	bucket := "/" + strings.Trim(r.URL.Path, "/") + "/"
	prefix := r.URL.Query().Get("prefix")

	var keys []string

	for path := range f.objects {
		key, ok := strings.CutPrefix(path, bucket)
		if ok && strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
	}

	sort.Strings(keys)

	var b strings.Builder

	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?><ListBucketResult><IsTruncated>false</IsTruncated>`)

	for _, key := range keys {
		b.WriteString("<Contents><Key>" + key + "</Key></Contents>")
	}

	b.WriteString(`</ListBucketResult>`)

	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(b.String()))
}

// TestS3Store_ListAndDelete: the scheduled export's pruning lists a prefix and deletes what it
// no longer keeps (TODO-3 item 158).
func TestS3Store_ListAndDelete(t *testing.T) {
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

	ctx := context.Background()

	for _, key := range []string{"scheduled/b.archive.gz", "scheduled/a.archive.gz", "manual.archive.gz"} {
		if err := store.Put(ctx, key, bytes.NewReader([]byte("x"))); err != nil {
			t.Fatalf("Put(%s): %s", key, err)
		}
	}

	keys, err := store.List(ctx, "scheduled/")
	if err != nil {
		t.Fatalf("List: %s", err)
	}

	if strings.Join(keys, ",") != "scheduled/a.archive.gz,scheduled/b.archive.gz" {
		t.Errorf("List = %v, want the two scheduled keys, sorted", keys)
	}

	if err := store.Delete(ctx, "scheduled/a.archive.gz"); err != nil {
		t.Fatalf("Delete: %s", err)
	}

	keys, err = store.List(ctx, "scheduled/")
	if err != nil || strings.Join(keys, ",") != "scheduled/b.archive.gz" {
		t.Errorf("after Delete, List = %v (%v), want only scheduled/b", keys, err)
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

// TestS3Store_ListAndDeleteSurfaceErrors: a bucket that refuses is an error to the caller, never an
// empty listing - which the rotation would read as "nothing to keep or prune".
func TestS3Store_ListAndDeleteSurfaceErrors(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("AWS_MAX_ATTEMPTS", "1")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
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

	if _, err := store.List(context.Background(), "scheduled/"); err == nil {
		t.Error("List against a refusing bucket reported no error")
	}

	if err := store.Delete(context.Background(), "scheduled/a.archive.gz"); err == nil {
		t.Error("Delete against a refusing bucket reported no error")
	}
}
