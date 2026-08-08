package archive

import (
	"context"
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/fastbean-au/hippocampus/contract"
)

// TestWriterWriteSurfacesUnderlyingError covers the write path's own error return, as distinct from
// Close's. A record large enough to push past the buffered writer and the compressor reaches the
// destination during the write rather than only at Close, which is what makes the branch reachable.
func TestWriterWriteSurfacesUnderlyingError(t *testing.T) {
	w := NewWriter(&failWriter{})

	// Random bytes so the body cannot be compressed away to nothing before it reaches the
	// destination.
	body := make([]byte, 1<<20)
	if _, err := rand.Read(body); err != nil {
		t.Fatalf("rand.Read: %s", err)
	}

	if err := w.WriteHeader(&contract.ArchiveHeader{Version: Version}); err != nil {
		t.Fatalf("WriteHeader: %s", err)
	}

	// Bodies are proto3 strings, so the random bytes go through as a (lossy but valid) string; only
	// their incompressibility matters here.
	memory := &contract.Memory{Id: "m1", Significance: 5, Body: string(body)}

	if err := w.WriteMemory(memory); err == nil {
		t.Error("expected the write to surface the destination's error")
	}
}

// TestS3Store_PutSurfacesUploadFailure covers the upload error path against an endpoint that
// refuses every request.
func TestS3Store_PutSurfacesUploadFailure(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("AWS_REGION", "us-east-1")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)

	store, err := NewS3Store(context.Background(), S3Config{
		Bucket:       "bucket",
		Endpoint:     server.URL,
		UsePathStyle: true,
		Region:       "us-east-1",
	})
	if err != nil {
		t.Fatalf("NewS3Store: %s", err)
	}

	if err := store.Put(context.Background(), "key", strings.NewReader("body")); err == nil {
		t.Error("expected an upload against a failing endpoint to be reported")
	}
}

// TestNewS3Store_ConfigLoadFailure covers the AWS configuration chain failing, which is what a
// malformed AWS setting on the host produces. Worth pinning because the wrapped message is the only
// thing that tells an operator the fault is in their AWS environment rather than in the bucket
// settings they just edited.
func TestNewS3Store_ConfigLoadFailure(t *testing.T) {
	t.Setenv("AWS_MAX_ATTEMPTS", "not-a-number")

	_, err := NewS3Store(context.Background(), S3Config{Bucket: "bucket"})
	if err == nil {
		t.Fatal("expected a malformed AWS configuration to be reported")
	}

	if !strings.Contains(err.Error(), "AWS configuration") {
		t.Errorf("expected the message to name the AWS configuration, got %q", err)
	}
}
