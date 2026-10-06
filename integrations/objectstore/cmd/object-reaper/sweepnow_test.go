package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc"

	"github.com/fastbean-au/hippocampus/contract"
)

// --sweep-now driven through run (TODO-3 item 166): the bucket is a fake speaking enough of the S3
// wire protocol for the SDK, and the service is a stub answering ExplainConsolidation. What is pinned
// is the whole chain from the flags to a DELETE on the wire - shadow mode sends none, and an armed
// reaper deletes only the object whose memory the store no longer holds.

// sweepBucket serves a listing of objects (all old enough to be judged) and records DELETEs.
type sweepBucket struct {
	mu      sync.Mutex
	objects map[string]bool
	deleted []string
}

func newSweepBucket(t *testing.T, keys ...string) (*sweepBucket, string) {
	t.Helper()

	bucket := &sweepBucket{objects: map[string]bool{}}

	for _, k := range keys {
		bucket.objects[k] = true
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/payloads"), "/")

		bucket.mu.Lock()
		defer bucket.mu.Unlock()

		switch {

		case r.Method == http.MethodHead:
			w.WriteHeader(http.StatusOK)

		case r.Method == http.MethodGet && r.URL.Query().Get("list-type") == "2":
			keys := make([]string, 0, len(bucket.objects))

			for k := range bucket.objects {
				keys = append(keys, k)
			}

			sort.Strings(keys)

			var body strings.Builder

			body.WriteString(`<?xml version="1.0" encoding="UTF-8"?><ListBucketResult><IsTruncated>false</IsTruncated>`)

			for _, k := range keys {
				fmt.Fprintf(&body, "<Contents><Key>%s</Key><Size>1</Size><LastModified>2026-01-01T00:00:00.000Z</LastModified></Contents>", k)
			}

			body.WriteString("</ListBucketResult>")

			w.Header().Set("Content-Type", "application/xml")
			_, _ = w.Write([]byte(body.String()))

		case r.Method == http.MethodDelete:
			bucket.deleted = append(bucket.deleted, key)
			delete(bucket.objects, key)

			w.WriteHeader(http.StatusNoContent)

		default:
			w.WriteHeader(http.StatusBadRequest)

		}
	}))

	t.Cleanup(server.Close)

	return bucket, server.URL
}

func (b *sweepBucket) deletions() []string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return append([]string{}, b.deleted...)
}

// heldService answers ExplainConsolidation for the ids it holds and omits the rest, which is how the
// real service answers about an id it does not have.
type heldService struct {
	contract.UnimplementedHippocampusServer

	held map[string]bool
}

func (h *heldService) ExplainConsolidation(
	_ context.Context,
	in *contract.ExplainConsolidationRequest,
) (*contract.ExplainConsolidationResponse, error) {
	res := &contract.ExplainConsolidationResponse{}

	for _, id := range in.GetMemoryIds() {
		if h.held[id] {
			res.Valuations = append(res.Valuations, &contract.MemoryValuation{Id: id})
		}
	}

	return res, nil
}

func startHeldService(t *testing.T, held ...string) string {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %s", err)
	}

	service := &heldService{held: map[string]bool{}}

	for _, id := range held {
		service.held[id] = true
	}

	server := grpc.NewServer()
	contract.RegisterHippocampusServer(server, service)

	go func() { _ = server.Serve(listener) }()

	t.Cleanup(server.Stop)

	return listener.Addr().String()
}

func sweepNow(t *testing.T, extra ...string) *sweepBucket {
	t.Helper()

	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")

	bucket, endpoint := newSweepBucket(t, "kept.json", "orphan.json")
	address := startHeldService(t, "payloads/kept.json")

	args := append([]string{
		"--bucket", "payloads",
		"--s3-endpoint", endpoint,
		"--s3-region", "us-east-1",
		"--s3-path-style",
		"--address", address,
		"--sweep-now",
		"--listen-port", "0",
		"--health-port", "0",
	}, extra...)

	setupFlags(t, args)

	if err := run(context.Background()); err != nil {
		t.Fatalf("run --sweep-now: %s", err)
	}

	return bucket
}

func TestSweepNowInShadowModeDeletesNothing(t *testing.T) {
	bucket := sweepNow(t)

	if got := bucket.deletions(); len(got) != 0 {
		t.Errorf("a shadow-mode sweep deleted %v", got)
	}
}

func TestSweepNowArmedDeletesOnlyTheOrphan(t *testing.T) {
	bucket := sweepNow(t, "--delete")

	if got := bucket.deletions(); len(got) != 1 || got[0] != "orphan.json" {
		t.Errorf("an armed sweep deleted %v, want only orphan.json, whose memory the store does not hold", got)
	}
}
