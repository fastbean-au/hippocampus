package objects

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeS3 is the smallest server the SDK will talk to: enough of ListObjectsV2, GetObject,
// DeleteObject and HeadBucket to drive S3Store for real rather than through a hand-written double.
// It is what proves the paginator, the range plumbing and the not-found mapping actually work
// against the wire protocol, which no fake implementing our own interface can say anything about.
func fakeS3(t *testing.T, objects map[string]string) (*httptest.Server, *[]string) {
	t.Helper()

	deleted := []string{}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/payloads")
		key := strings.TrimPrefix(path, "/")

		switch {

		case r.Method == http.MethodHead && key == "":
			w.WriteHeader(http.StatusOK)

		case r.Method == http.MethodGet && r.URL.Query().Get("list-type") == "2":
			writeListing(w, objects, r.URL.Query().Get("prefix"), r.URL.Query().Get("continuation-token"))

		case r.Method == http.MethodGet:
			body, ok := objects[key]
			if !ok {
				w.WriteHeader(http.StatusNotFound)

				_, _ = w.Write([]byte(`<Error><Code>NoSuchKey</Code><Message>no</Message></Error>`))

				return
			}

			if rng := r.Header.Get("Range"); rng != "" {
				w.Header().Set("Content-Range", fmt.Sprintf("bytes 1-%d/%d", len(body)-1, len(body)))
				w.WriteHeader(http.StatusPartialContent)

				_, _ = w.Write([]byte(body[1:]))

				return
			}

			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))

		case r.Method == http.MethodDelete:
			deleted = append(deleted, key)

			delete(objects, key)

			w.WriteHeader(http.StatusNoContent)

		default:
			w.WriteHeader(http.StatusBadRequest)

		}
	}))

	t.Cleanup(server.Close)

	return server, &deleted
}

// writeListing answers one page, so the paginator is exercised rather than short-circuited: the
// first call returns one key and a continuation token, the second returns the rest.
func writeListing(w http.ResponseWriter, objects map[string]string, prefix string, token string) {
	keys := []string{}

	for k := range objects {
		if prefix != "" && !strings.HasPrefix(k, prefix) {
			continue
		}

		keys = append(keys, k)
	}

	// A deterministic order without pulling in sort: the tests only ever use a handful of keys.
	for i := range keys {
		for j := i + 1; j < len(keys); j++ {
			if keys[j] < keys[i] {
				keys[i], keys[j] = keys[j], keys[i]
			}
		}
	}

	truncated := false

	if token == "" && len(keys) > 1 {
		truncated = true
		keys = keys[:1]
	} else if token != "" {
		keys = keys[1:]
	}

	body := strings.Builder{}

	body.WriteString(`<?xml version="1.0" encoding="UTF-8"?><ListBucketResult>`)
	fmt.Fprintf(&body, "<IsTruncated>%t</IsTruncated>", truncated)

	if truncated {
		body.WriteString("<NextContinuationToken>more</NextContinuationToken>")
	}

	for _, k := range keys {
		fmt.Fprintf(&body,
			"<Contents><Key>%s</Key><Size>%d</Size><LastModified>2026-01-01T00:00:00.000Z</LastModified></Contents>",
			k, len(objects[k]),
		)
	}

	body.WriteString("</ListBucketResult>")

	w.Header().Set("Content-Type", "application/xml")
	_, _ = w.Write([]byte(body.String()))
}

func newS3(t *testing.T, endpoint string) *S3Store {
	t.Helper()

	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("AWS_REGION", "us-east-1")

	store, err := NewS3Store(context.Background(), Config{
		Bucket:       "payloads",
		Endpoint:     endpoint,
		Region:       "us-east-1",
		UsePathStyle: true,
	})
	if err != nil {
		t.Fatalf("NewS3Store failed: %s", err.Error())
	}

	return store
}

func TestS3StoreReadsAnObject(t *testing.T) {
	server, _ := fakeS3(t, map[string]string{"traces/one.json": `{"trace":1}`})
	store := newS3(t, server.URL)

	reader, err := store.Get(context.Background(), "traces/one.json", GetOptions{})
	if err != nil {
		t.Fatalf("Get failed: %s", err.Error())
	}

	defer func() { _ = reader.Body.Close() }()

	body, err := io.ReadAll(reader.Body)
	if err != nil {
		t.Fatalf("reading failed: %s", err.Error())
	}

	if string(body) != `{"trace":1}` {
		t.Errorf("expected the object, got %q", body)
	}

	if reader.ContentType != "application/json" {
		t.Errorf("expected the content type to be carried, got %q", reader.ContentType)
	}
}

func TestS3StoreForwardsARange(t *testing.T) {
	server, _ := fakeS3(t, map[string]string{"traces/one.json": `{"trace":1}`})
	store := newS3(t, server.URL)

	reader, err := store.Get(context.Background(), "traces/one.json", GetOptions{Range: "bytes=1-"})
	if err != nil {
		t.Fatalf("Get failed: %s", err.Error())
	}

	defer func() { _ = reader.Body.Close() }()

	if reader.ContentRange == "" {
		t.Error("expected the content range to be carried back")
	}
}

func TestS3StoreReportsAMissingObject(t *testing.T) {
	server, _ := fakeS3(t, map[string]string{})
	store := newS3(t, server.URL)

	_, err := store.Get(context.Background(), "absent", GetOptions{})
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestS3StoreDeletes(t *testing.T) {
	server, deleted := fakeS3(t, map[string]string{"traces/one.json": "one"})
	store := newS3(t, server.URL)

	if err := store.Delete(context.Background(), "traces/one.json"); err != nil {
		t.Fatalf("Delete failed: %s", err.Error())
	}

	if len(*deleted) != 1 || (*deleted)[0] != "traces/one.json" {
		t.Errorf("expected the key to be deleted, got %v", *deleted)
	}

	// And again, which is the property at-least-once delivery rests on.
	if err := store.Delete(context.Background(), "traces/one.json"); err != nil {
		t.Errorf("expected a repeated delete to succeed, got %s", err.Error())
	}
}

func TestS3StoreListsEveryPage(t *testing.T) {
	server, _ := fakeS3(t, map[string]string{
		"traces/one.json":   "one",
		"traces/two.json":   "two",
		"traces/three.json": "three",
	})

	store := newS3(t, server.URL)

	seen := []string{}

	if err := store.List(context.Background(), "traces/", func(object Object) error {
		seen = append(seen, object.Key)

		if object.Modified.IsZero() {
			t.Error("expected the last-modified time to be carried")
		}

		return nil
	}); err != nil {
		t.Fatalf("List failed: %s", err.Error())
	}

	if len(seen) != 3 {
		t.Errorf("expected the paginator to walk every page, got %v", seen)
	}
}

func TestS3StoreListStopsOnTheCallersError(t *testing.T) {
	server, _ := fakeS3(t, map[string]string{"a": "1", "b": "2"})
	store := newS3(t, server.URL)

	err := store.List(context.Background(), "", func(Object) error {
		return fmt.Errorf("enough")
	})
	if err == nil {
		t.Error("expected the caller's error to stop the walk")
	}
}

func TestS3StorePings(t *testing.T) {
	server, _ := fakeS3(t, map[string]string{})
	store := newS3(t, server.URL)

	if err := store.Ping(context.Background()); err != nil {
		t.Errorf("expected the ping to succeed, got %s", err.Error())
	}
}

func TestS3StorePresigns(t *testing.T) {
	server, _ := fakeS3(t, map[string]string{})
	store := newS3(t, server.URL)

	url, err := store.Presign(context.Background(), "traces/one.json", 5*time.Minute)
	if err != nil {
		t.Fatalf("Presign failed: %s", err.Error())
	}

	if !strings.Contains(url, "traces/one.json") {
		t.Errorf("expected the URL to name the object, got %q", url)
	}

	if !strings.Contains(url, "X-Amz-Signature") {
		t.Errorf("expected a signed URL, got %q", url)
	}
}

func TestS3StoreReportsAnUnreachableBucket(t *testing.T) {
	store := newS3(t, "http://127.0.0.1:1")

	// Bounded, because the SDK's own retries would otherwise make this the slowest test in the
	// module by an order of magnitude. What is being checked is that a failure surfaces rather than
	// being swallowed, and a deadline is as good a failure as a refused connection.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	if err := store.Ping(ctx); err == nil {
		t.Error("expected an unreachable bucket to fail the ping")
	}

	if _, err := store.Get(ctx, "a", GetOptions{}); err == nil {
		t.Error("expected an unreachable bucket to fail a read")
	}

	if err := store.Delete(ctx, "a"); err == nil {
		t.Error("expected an unreachable bucket to fail a delete")
	}

	if err := store.List(ctx, "", func(Object) error { return nil }); err == nil {
		t.Error("expected an unreachable bucket to fail a listing")
	}
}
