package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func stubHTTPHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
}

// TestHTTPMiddleware_ValidToken verifies that a request carrying a valid bearer token in the
// Authorization header reaches the wrapped handler.
func TestHTTPMiddleware_ValidToken(t *testing.T) {
	v, err := NewHMACVerifier(HMACConfig{LegacySecret: "test-secret"})
	if err != nil {
		t.Fatalf("NewHMACVerifier: %s", err)
	}

	token, err := MintToken(MintRequest{Secret: "test-secret", ClientID: "client-1", TTL: time.Hour})
	if err != nil {
		t.Fatalf("MintToken: %s", err)
	}

	handler := HTTPMiddleware(v, stubHTTPHandler(), nil)

	req := httptest.NewRequest(http.MethodGet, "/v1/events", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
}

// TestHTTPMiddleware_MissingToken verifies that a request with no Authorization header is
// rejected with 401 and a WWW-Authenticate header.
func TestHTTPMiddleware_MissingToken(t *testing.T) {
	v, err := NewHMACVerifier(HMACConfig{LegacySecret: "test-secret"})
	if err != nil {
		t.Fatalf("NewHMACVerifier: %s", err)
	}

	handler := HTTPMiddleware(v, stubHTTPHandler(), nil)

	req := httptest.NewRequest(http.MethodGet, "/v1/events", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rec.Code)
	}

	if rec.Header().Get("WWW-Authenticate") == "" {
		t.Error("expected a WWW-Authenticate header on a 401 response")
	}
}

// TestHTTPMiddleware_MalformedToken verifies that a malformed Authorization header (wrong scheme)
// is rejected with 401.
func TestHTTPMiddleware_MalformedToken(t *testing.T) {
	v, err := NewHMACVerifier(HMACConfig{LegacySecret: "test-secret"})
	if err != nil {
		t.Fatalf("NewHMACVerifier: %s", err)
	}

	handler := HTTPMiddleware(v, stubHTTPHandler(), nil)

	req := httptest.NewRequest(http.MethodGet, "/v1/events", nil)
	req.Header.Set("Authorization", "Basic dXNlcjpwYXNz")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rec.Code)
	}
}

// TestHTTPMiddleware_InvalidToken verifies that a well-formed "Bearer <token>" header carrying a
// token that fails verification (as opposed to a malformed header/scheme) is rejected with 401 -
// the Verify error branch, distinct from ExtractBearerToken's.
func TestHTTPMiddleware_InvalidToken(t *testing.T) {
	v, err := NewHMACVerifier(HMACConfig{LegacySecret: "test-secret"})
	if err != nil {
		t.Fatalf("NewHMACVerifier: %s", err)
	}

	handler := HTTPMiddleware(v, stubHTTPHandler(), nil)

	req := httptest.NewRequest(http.MethodGet, "/v1/events", nil)
	req.Header.Set("Authorization", "Bearer not-a-real-token")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for a well-formed but invalid token, got %d", rec.Code)
	}
}

// TestHTTPMiddleware_OpenPathBypassesAuth verifies the critical scoping guarantee on the HTTP
// side: a path listed in openPaths (e.g. /healthz) is reachable with zero Authorization header,
// mirroring the gRPC health-check bypass.
func TestHTTPMiddleware_OpenPathBypassesAuth(t *testing.T) {
	v, err := NewHMACVerifier(HMACConfig{LegacySecret: "test-secret"})
	if err != nil {
		t.Fatalf("NewHMACVerifier: %s", err)
	}

	handler := HTTPMiddleware(v, stubHTTPHandler(), []string{"/healthz"})

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected the open path to bypass auth entirely, got %d", rec.Code)
	}
}
