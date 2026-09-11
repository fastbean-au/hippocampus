package objects

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

func TestNewS3StoreRequiresABucket(t *testing.T) {
	if _, err := NewS3Store(context.Background(), Config{}); err == nil {
		t.Error("expected a store with no bucket to be refused")
	}
}

func TestNewS3StoreBuildsAClient(t *testing.T) {
	store, err := NewS3Store(context.Background(), Config{
		Bucket:       "payloads",
		Region:       "us-east-1",
		Endpoint:     "http://localhost:9000",
		UsePathStyle: true,
	})
	if err != nil {
		t.Fatalf("NewS3Store failed: %s", err.Error())
	}

	if store.Bucket() != "payloads" {
		t.Errorf("expected the bucket to be reported, got %q", store.Bucket())
	}
}

// absent() is what makes Delete idempotent against the S3-compatible stores that answer a missing
// key with an error rather than a 204.
func TestAbsentRecognisesTheStoreSayingNo(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		expected bool
	}{
		{name: "no such key", err: &types.NoSuchKey{}, expected: true},
		{name: "not found", err: &types.NotFound{}, expected: true},
		{name: "wrapped", err: fmt.Errorf("reading: %w", &types.NoSuchKey{}), expected: true},
		{name: "anything else", err: fmt.Errorf("access denied"), expected: false},
	}

	for _, v := range cases {
		t.Run(v.name, func(t *testing.T) {
			if got := absent(v.err); got != v.expected {
				t.Errorf("expected %v, got %v", v.expected, got)
			}
		})
	}
}

// The in-memory store is a statement of what the interface promises, so its behaviour is worth
// pinning - above all the one that the whole delivery model rests on.
func TestDeletingAnAbsentObjectIsASuccess(t *testing.T) {
	store := NewMemory("payloads")

	if err := store.Delete(context.Background(), "never-existed"); err != nil {
		t.Errorf("expected deleting an absent object to succeed, got %s", err.Error())
	}
}

func TestMemoryGetReportsAMissingObject(t *testing.T) {
	store := NewMemory("payloads")

	_, err := store.Get(context.Background(), "absent", GetOptions{})
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestMemoryReadsAndLists(t *testing.T) {
	store := NewMemory("payloads")
	store.Put("a/one", []byte("hello"), time.Now())
	store.Put("b/two", []byte("world"), time.Now())

	reader, err := store.Get(context.Background(), "a/one", GetOptions{})
	if err != nil {
		t.Fatalf("Get failed: %s", err.Error())
	}

	body, err := io.ReadAll(reader.Body)
	if err != nil {
		t.Fatalf("reading the body failed: %s", err.Error())
	}

	if string(body) != "hello" {
		t.Errorf("expected the body, got %q", body)
	}

	seen := 0

	if err := store.List(context.Background(), "a/", func(Object) error {
		seen++

		return nil
	}); err != nil {
		t.Fatalf("List failed: %s", err.Error())
	}

	if seen != 1 {
		t.Errorf("expected the prefix to narrow the listing, got %d", seen)
	}

	if err := store.Ping(context.Background()); err != nil {
		t.Errorf("expected the in-memory store to answer a ping, got %s", err.Error())
	}
}

func TestMemoryServesARange(t *testing.T) {
	store := NewMemory("payloads")
	store.Put("a/one", []byte("hello"), time.Now())

	reader, err := store.Get(context.Background(), "a/one", GetOptions{Range: "bytes=2-"})
	if err != nil {
		t.Fatalf("Get failed: %s", err.Error())
	}

	if reader.ContentRange == "" {
		t.Error("expected a content range")
	}

	body, err := io.ReadAll(reader.Body)
	if err != nil {
		t.Fatalf("reading the body failed: %s", err.Error())
	}

	if string(body) != "llo" {
		t.Errorf("expected the range, got %q", body)
	}
}

func TestMemoryPropagatesInjectedFailures(t *testing.T) {
	store := NewMemory("payloads")
	store.Put("a/one", []byte("hello"), time.Now())

	store.PresignErr = fmt.Errorf("no")
	store.GetErr = fmt.Errorf("no")
	store.DeleteErr = fmt.Errorf("no")
	store.ListErr = fmt.Errorf("no")

	if _, err := store.Presign(context.Background(), "a/one", time.Minute); err == nil {
		t.Error("expected the injected presign failure")
	}

	if _, err := store.Get(context.Background(), "a/one", GetOptions{}); err == nil {
		t.Error("expected the injected get failure")
	}

	if err := store.Delete(context.Background(), "a/one"); err == nil {
		t.Error("expected the injected delete failure")
	}

	if err := store.List(context.Background(), "", func(Object) error { return nil }); err == nil {
		t.Error("expected the injected list failure")
	}
}

func TestMemoryPresignNamesTheObject(t *testing.T) {
	store := NewMemory("payloads")

	url, err := store.Presign(context.Background(), "a/one", time.Minute)
	if err != nil {
		t.Fatalf("Presign failed: %s", err.Error())
	}

	if url == "" {
		t.Error("expected a URL")
	}
}
