package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestInterceptorRecoverPanic verifies that a panicking handler is turned into a codes.Internal
// error rather than propagating out and crashing the process, and that a non-panicking handler is
// passed through untouched.
func TestInterceptorRecoverPanic(t *testing.T) {
	panicking := func(ctx context.Context, req interface{}) (interface{}, error) {
		panic("boom")
	}

	resp, err := InterceptorRecoverPanic(context.Background(), nil, &grpc.UnaryServerInfo{FullMethod: "/proto.Hippocampus/Test"}, panicking)
	if resp != nil {
		t.Errorf("expected a nil response after a recovered panic, got %v", resp)
	}

	if status.Code(err) != codes.Internal {
		t.Errorf("expected codes.Internal after a recovered panic, got %v", status.Code(err))
	}

	okHandler := func(ctx context.Context, req interface{}) (interface{}, error) {
		return "ok", nil
	}

	resp, err = InterceptorRecoverPanic(context.Background(), nil, &grpc.UnaryServerInfo{FullMethod: "/proto.Hippocampus/Test"}, okHandler)
	if err != nil {
		t.Errorf("expected no error from a clean handler, got %v", err)
	}

	if resp != "ok" {
		t.Errorf("expected the clean handler's response to pass through, got %v", resp)
	}
}

// TestRecoverMiddleware verifies that a panicking gateway handler is turned into a 500 response
// rather than propagating out, while a clean handler passes through with its own status.
func TestRecoverMiddleware(t *testing.T) {
	panicking := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	})

	rec := httptest.NewRecorder()
	recoverMiddleware(panicking).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/memories", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("expected 500 after a recovered panic, got %d", rec.Code)
	}

	clean := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	rec = httptest.NewRecorder()
	recoverMiddleware(clean).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Errorf("expected a clean handler to pass through with 200, got %d", rec.Code)
	}
}
