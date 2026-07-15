package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"google.golang.org/grpc"
)

// TestWebUIHandler verifies the embedded console is served as no-store HTML with a non-empty body,
// so a browser always fetches the current build rather than a cached one.
func TestWebUIHandler(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/ui", nil)

	webUIHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}

	if ct := rec.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Errorf("unexpected Content-Type %q", ct)
	}

	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("expected Cache-Control no-store, got %q", cc)
	}

	if rec.Body.Len() == 0 {
		t.Error("expected a non-empty console body")
	}
}

// TestInterceptorLogger_PassesThrough verifies the logging interceptor returns the wrapped
// handler's response and error unchanged.
func TestInterceptorLogger_PassesThrough(t *testing.T) {
	info := &grpc.UnaryServerInfo{FullMethod: "/proto.Hippocampus/Ping"}

	// Success path: the handler's response is returned verbatim.
	resp, err := InterceptorLogger(context.Background(), "req", info, func(ctx context.Context, req any) (any, error) {
		return "ok", nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	if resp != "ok" {
		t.Errorf("expected response 'ok', got %v", resp)
	}

	// Error path: the handler's error propagates.
	wantErr := errors.New("boom")

	if _, err := InterceptorLogger(context.Background(), "req", info, func(ctx context.Context, req any) (any, error) {
		return nil, wantErr
	}); !errors.Is(err, wantErr) {
		t.Errorf("expected the handler's error to propagate, got %v", err)
	}
}
