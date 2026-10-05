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

	handler := HTTPMiddleware(v, stubHTTPHandler(), nil, "")

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

	handler := HTTPMiddleware(v, stubHTTPHandler(), nil, "")

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

	handler := HTTPMiddleware(v, stubHTTPHandler(), nil, "")

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

	handler := HTTPMiddleware(v, stubHTTPHandler(), nil, "")

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

	handler := HTTPMiddleware(v, stubHTTPHandler(), []string{"/healthz"}, "")

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected the open path to bypass auth entirely, got %d", rec.Code)
	}
}

// TestHTTPMiddleware_SessionCookieFallback verifies that when a session cookie name is configured, a
// request carrying no Authorization header but a valid token in that cookie is authenticated - the
// seam the server-side OIDC login relies on.
func TestHTTPMiddleware_SessionCookieFallback(t *testing.T) {
	v, err := NewHMACVerifier(HMACConfig{LegacySecret: "test-secret"})
	if err != nil {
		t.Fatalf("NewHMACVerifier: %s", err)
	}

	token, err := MintToken(MintRequest{Secret: "test-secret", ClientID: "client-1", TTL: time.Hour})
	if err != nil {
		t.Fatalf("MintToken: %s", err)
	}

	handler := HTTPMiddleware(v, stubHTTPHandler(), nil, SessionCookieName)

	req := httptest.NewRequest(http.MethodGet, "/v1/events", nil)
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: token})
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected the session cookie to authenticate the request, got %d", rec.Code)
	}
}

// TestHTTPMiddleware_SessionCookieIgnoredWhenUnconfigured verifies that the cookie fallback is off
// by default: with no session cookie name configured, a token present only in a cookie does not
// authenticate, preserving the header-only behaviour for hmac and API clients.
func TestHTTPMiddleware_SessionCookieIgnoredWhenUnconfigured(t *testing.T) {
	v, err := NewHMACVerifier(HMACConfig{LegacySecret: "test-secret"})
	if err != nil {
		t.Fatalf("NewHMACVerifier: %s", err)
	}

	token, err := MintToken(MintRequest{Secret: "test-secret", ClientID: "client-1", TTL: time.Hour})
	if err != nil {
		t.Fatalf("MintToken: %s", err)
	}

	handler := HTTPMiddleware(v, stubHTTPHandler(), nil, "")

	req := httptest.NewRequest(http.MethodGet, "/v1/events", nil)
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: token})
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 when the cookie fallback is unconfigured, got %d", rec.Code)
	}
}

// TestHTTPMiddleware_HeaderWinsOverCookie verifies that an explicit Authorization header takes
// precedence over the session cookie, so an API client's header is never overridden by a stale
// cookie on the same request.
func TestHTTPMiddleware_HeaderWinsOverCookie(t *testing.T) {
	v, err := NewHMACVerifier(HMACConfig{LegacySecret: "test-secret"})
	if err != nil {
		t.Fatalf("NewHMACVerifier: %s", err)
	}

	good, err := MintToken(MintRequest{Secret: "test-secret", ClientID: "client-1", TTL: time.Hour})
	if err != nil {
		t.Fatalf("MintToken: %s", err)
	}

	handler := HTTPMiddleware(v, stubHTTPHandler(), nil, SessionCookieName)

	req := httptest.NewRequest(http.MethodGet, "/v1/events", nil)
	req.Header.Set("Authorization", "Bearer "+good)
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: "garbage-cookie-token"})
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected the valid header to win over the bad cookie, got %d", rec.Code)
	}
}

// TestHTTPMiddleware_CookieSessionsRefuseCrossOriginWrites pins the CSRF defence on the session
// cookie (TODO-3 item 145). SameSite=Lax stops a cookie riding along on a cross-SITE request, but a
// page on another port of the same host or on a sibling subdomain is same-site, and its "simple"
// POST - text/plain, no preflight - arrives with the cookie attached. The gateway decodes any content
// type as JSON, so that is a Purge from a hostile page in the signed-in admin's name. A browser
// labels every request with where it came from (Sec-Fetch-Site, else Origin), and a cookie-borne
// write from anywhere but the gateway's own origin is refused.
//
// A bearer header is never refused this way: a browser cannot attach one to a cross-origin request
// without a CORS preflight the gateway does not grant, so a header is proof the caller chose to send
// it. Safe methods are never refused either: they change nothing, so there is nothing to forge.
func TestHTTPMiddleware_CookieSessionsRefuseCrossOriginWrites(t *testing.T) {
	v, err := NewHMACVerifier(HMACConfig{LegacySecret: "test-secret"})
	if err != nil {
		t.Fatalf("NewHMACVerifier: %s", err)
	}

	token, err := MintToken(MintRequest{Secret: "test-secret", ClientID: "client-1", TTL: time.Hour})
	if err != nil {
		t.Fatalf("MintToken: %s", err)
	}

	handler := HTTPMiddleware(v, stubHTTPHandler(), nil, SessionCookieName)

	cases := []struct {
		name    string
		method  string
		cookie  bool
		headers map[string]string
		want    int
	}{
		{"cookie POST from a same-site page", http.MethodPost, true, map[string]string{"Sec-Fetch-Site": "same-site"}, http.StatusForbidden},
		{"cookie POST from a cross-site page", http.MethodPost, true, map[string]string{"Sec-Fetch-Site": "cross-site"}, http.StatusForbidden},
		{"cookie DELETE from a same-site page", http.MethodDelete, true, map[string]string{"Sec-Fetch-Site": "same-site"}, http.StatusForbidden},
		{"cookie POST with a foreign Origin and no Sec-Fetch-Site", http.MethodPost, true, map[string]string{"Origin": "https://evil.example.com"}, http.StatusForbidden},
		{"cookie POST from the console's own origin", http.MethodPost, true, map[string]string{"Sec-Fetch-Site": "same-origin"}, http.StatusOK},
		{"cookie POST from a non-browser client", http.MethodPost, true, nil, http.StatusOK},
		{"cookie GET from a same-site page", http.MethodGet, true, map[string]string{"Sec-Fetch-Site": "same-site"}, http.StatusOK},
		{"bearer POST from a cross-site page", http.MethodPost, false, map[string]string{"Sec-Fetch-Site": "cross-site"}, http.StatusOK},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(c.method, "http://hippo.example.com/v1/purge", nil)

			if c.cookie {
				req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: token})
			} else {
				req.Header.Set("Authorization", "Bearer "+token)
			}

			for k, v := range c.headers {
				req.Header.Set(k, v)
			}

			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != c.want {
				t.Errorf("got %d, want %d", rec.Code, c.want)
			}
		})
	}
}
