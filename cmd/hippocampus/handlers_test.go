package main

import (
	"context"
	"encoding/json"
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

// TestUIConfigHandler verifies the front-channel config is served as no-store JSON carrying the
// configured provider parameters, so the console can start an OIDC login from it.
func TestUIConfigHandler(t *testing.T) {
	cfg := UIConfig{
		AuthMethod: "idp",
		Issuer:     "https://issuer.example/",
		ClientID:   "spa-client",
		Scopes:     "openid profile",
		Audience:   "https://api.example",
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/ui/config", nil)

	uiConfigHandler(cfg).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("unexpected Content-Type %q", ct)
	}

	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("expected Cache-Control no-store, got %q", cc)
	}

	var got UIConfig

	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("body is not valid JSON: %s", err)
	}

	if got != cfg {
		t.Errorf("round-tripped config mismatch: got %+v want %+v", got, cfg)
	}
}

// TestUIConfigHandlerNoAuth verifies the secret-free shape under auth.method none: authMethod is
// reported and the omitempty fields drop out, so the page learns auth is off without a login block.
func TestUIConfigHandlerNoAuth(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/ui/config", nil)

	uiConfigHandler(UIConfig{AuthMethod: "none"}).ServeHTTP(rec, req)

	if body := rec.Body.String(); body != `{"authMethod":"none"}` {
		t.Errorf("unexpected no-auth config body %q", body)
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
