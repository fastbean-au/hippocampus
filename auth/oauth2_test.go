package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// fakeIDP is a minimal OpenID Connect provider over httptest: it serves the discovery document, a
// JWKS, and a token endpoint that mints id_tokens signed with its own key. Its per-response
// behaviour (the nonce it embeds, the tokens it returns, whether it fails) is settable so one server
// drives every case.
type fakeIDP struct {
	server   *httptest.Server
	kid      string
	clientID string

	mu           sync.Mutex
	nonce        string
	accessToken  string
	refreshToken string
	failToken    bool
	omitIDToken  bool
	noEndSession bool
}

// newFakeIDP starts a provider for clientID. The caller stops it via t.Cleanup. The signing key is
// captured by the handler closures, reusing testRSAKey/testJWK from jwks_test.go.
func newFakeIDP(t *testing.T, clientID string) *fakeIDP {
	t.Helper()

	key := testRSAKey(t)
	kid := "idp-key-1"

	idp := &fakeIDP{
		kid:          kid,
		clientID:     clientID,
		accessToken:  "test-access-token",
		refreshToken: "test-refresh-token",
	}

	mux := http.NewServeMux()

	mux.HandleFunc(discoveryPath, func(w http.ResponseWriter, r *http.Request) {
		base := idp.server.URL

		doc := map[string]string{
			"issuer":                 base,
			"authorization_endpoint": base + "/authorize",
			"token_endpoint":         base + "/token",
			"jwks_uri":               base + "/jwks",
			"end_session_endpoint":   base + "/logout",
		}

		idp.mu.Lock()
		if idp.noEndSession {
			delete(doc, "end_session_endpoint")
		}
		idp.mu.Unlock()

		_ = json.NewEncoder(w).Encode(doc)
	})

	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{testJWK(kid, &key.PublicKey)}})
	})

	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		idp.mu.Lock()
		defer idp.mu.Unlock()

		if idp.failToken {
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)

			return
		}

		claims := jwt.MapClaims{
			"iss":   idp.server.URL,
			"aud":   idp.clientID,
			"sub":   "user-1",
			"iat":   time.Now().Unix(),
			"exp":   time.Now().Add(time.Hour).Unix(),
			"nonce": idp.nonce,
		}

		tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
		tok.Header["kid"] = kid

		idToken, err := tok.SignedString(key)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)

			return
		}

		resp := map[string]any{
			"access_token":  idp.accessToken,
			"token_type":    "Bearer",
			"expires_in":    3600,
			"refresh_token": idp.refreshToken,
		}

		if !idp.omitIDToken {
			resp["id_token"] = idToken
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})

	idp.server = httptest.NewServer(mux)
	t.Cleanup(idp.server.Close)

	return idp
}

func (i *fakeIDP) configure(nonce string) {
	i.mu.Lock()
	defer i.mu.Unlock()

	i.nonce = nonce
}

func (i *fakeIDP) setFailToken(fail bool) {
	i.mu.Lock()
	defer i.mu.Unlock()

	i.failToken = fail
}

func (i *fakeIDP) setOmitIDToken(omit bool) {
	i.mu.Lock()
	defer i.mu.Unlock()

	i.omitIDToken = omit
}

func (i *fakeIDP) setNoEndSession(no bool) {
	i.mu.Lock()
	defer i.mu.Unlock()

	i.noEndSession = no
}

// newTestLogin builds an OAuth2Login (and its id_token verifier) pointed at idp.
func newTestLogin(t *testing.T, idp *fakeIDP) *OAuth2Login {
	t.Helper()

	idv, err := NewJWKSVerifier(JWKSConfig{
		Issuer:          idp.server.URL,
		Audience:        idp.clientID,
		RefreshInterval: time.Minute,
	})
	if err != nil {
		t.Fatalf("NewJWKSVerifier: %s", err)
	}

	login, err := NewOAuth2Login(OAuth2Config{
		Issuer:          idp.server.URL,
		ClientID:        idp.clientID,
		ClientSecret:    "test-secret",
		RedirectURL:     "https://app.example/auth/callback",
		Scopes:          []string{"openid", "profile"},
		IDTokenVerifier: idv,
		CookieSecure:    false,
	})
	if err != nil {
		t.Fatalf("NewOAuth2Login: %s", err)
	}

	return login
}

// cookiesByName collects Set-Cookie cookies from a recorder into a name→cookie map.
func cookiesByName(rec *httptest.ResponseRecorder) map[string]*http.Cookie {
	out := map[string]*http.Cookie{}

	for _, c := range rec.Result().Cookies() {
		out[c.Name] = c
	}

	return out
}

// TestNewOAuth2Login_RequiresFields verifies construction rejects a config missing a required field.
func TestNewOAuth2Login_RequiresFields(t *testing.T) {
	if _, err := NewOAuth2Login(OAuth2Config{}); err == nil {
		t.Error("expected an error for an empty oauth2 config")
	}
}

// TestOAuth2Login_Login checks the authorization redirect carries the PKCE challenge, state, and
// nonce, and that the transient cookies are set for the return leg.
func TestOAuth2Login_Login(t *testing.T) {
	idp := newFakeIDP(t, "console-client")
	login := newTestLogin(t, idp)

	req := httptest.NewRequest(http.MethodGet, "/auth/login", nil)
	rec := httptest.NewRecorder()

	login.Login(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("expected 302, got %d", rec.Code)
	}

	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse Location: %s", err)
	}

	q := loc.Query()

	for _, key := range []string{"response_type", "client_id", "state", "code_challenge"} {
		if q.Get(key) == "" {
			t.Errorf("authorization URL missing %q", key)
		}
	}

	if q.Get("code_challenge_method") != "S256" {
		t.Errorf("expected S256 challenge method, got %q", q.Get("code_challenge_method"))
	}

	if q.Get("nonce") == "" {
		t.Error("authorization URL missing nonce")
	}

	cookies := cookiesByName(rec)

	for _, name := range []string{stateCookieName, nonceCookieName, pkceCookieName} {
		if cookies[name] == nil || cookies[name].Value == "" {
			t.Errorf("expected transient cookie %q to be set", name)
		}
	}

	if cookies[stateCookieName].Value != q.Get("state") {
		t.Error("state cookie must match the state in the authorization URL")
	}
}

// TestOAuth2Login_CallbackHappyPath drives a valid callback and asserts the session cookie is set to
// the provider's access token and the transient cookies are cleared.
func TestOAuth2Login_CallbackHappyPath(t *testing.T) {
	idp := newFakeIDP(t, "console-client")
	login := newTestLogin(t, idp)

	idp.configure("nonce-abc")

	req := httptest.NewRequest(http.MethodGet, "/auth/callback?code=auth-code&state=state-xyz", nil)
	req.AddCookie(&http.Cookie{Name: stateCookieName, Value: "state-xyz"})
	req.AddCookie(&http.Cookie{Name: nonceCookieName, Value: "nonce-abc"})
	req.AddCookie(&http.Cookie{Name: pkceCookieName, Value: "pkce-verifier"})
	rec := httptest.NewRecorder()

	login.Callback(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("expected 302 to the console, got %d (%s)", rec.Code, rec.Body.String())
	}

	if rec.Header().Get("Location") != "/ui" {
		t.Errorf("expected redirect to /ui, got %q", rec.Header().Get("Location"))
	}

	cookies := cookiesByName(rec)

	if cookies[SessionCookieName] == nil || cookies[SessionCookieName].Value != idp.accessToken {
		t.Errorf("expected session cookie set to the access token, got %+v", cookies[SessionCookieName])
	}

	if cookies[refreshCookieName] == nil || cookies[refreshCookieName].Value != idp.refreshToken {
		t.Errorf("expected refresh cookie set to the refresh token, got %+v", cookies[refreshCookieName])
	}

	if c := cookies[stateCookieName]; c == nil || c.MaxAge >= 0 {
		t.Error("expected the state cookie to be cleared")
	}
}

// TestOAuth2Login_CallbackStateMismatch verifies a callback whose state does not match the cookie is
// rejected before any token exchange.
func TestOAuth2Login_CallbackStateMismatch(t *testing.T) {
	idp := newFakeIDP(t, "console-client")
	login := newTestLogin(t, idp)

	req := httptest.NewRequest(http.MethodGet, "/auth/callback?code=auth-code&state=attacker", nil)
	req.AddCookie(&http.Cookie{Name: stateCookieName, Value: "genuine"})
	req.AddCookie(&http.Cookie{Name: nonceCookieName, Value: "nonce-abc"})
	req.AddCookie(&http.Cookie{Name: pkceCookieName, Value: "pkce-verifier"})
	rec := httptest.NewRecorder()

	login.Callback(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 on state mismatch, got %d", rec.Code)
	}

	if cookiesByName(rec)[SessionCookieName] != nil && cookiesByName(rec)[SessionCookieName].Value != "" {
		t.Error("no session cookie should be set on a rejected callback")
	}
}

// TestOAuth2Login_CallbackNonceMismatch verifies that an id_token whose nonce does not match the
// session's nonce is rejected even though state, code, and signature are all valid - the check that
// binds the id_token to this login.
func TestOAuth2Login_CallbackNonceMismatch(t *testing.T) {
	idp := newFakeIDP(t, "console-client")
	login := newTestLogin(t, idp)

	idp.configure("nonce-from-provider")

	req := httptest.NewRequest(http.MethodGet, "/auth/callback?code=auth-code&state=state-xyz", nil)
	req.AddCookie(&http.Cookie{Name: stateCookieName, Value: "state-xyz"})
	req.AddCookie(&http.Cookie{Name: nonceCookieName, Value: "different-nonce"})
	req.AddCookie(&http.Cookie{Name: pkceCookieName, Value: "pkce-verifier"})
	rec := httptest.NewRecorder()

	login.Callback(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 on nonce mismatch, got %d", rec.Code)
	}
}

// TestOAuth2Login_CallbackProviderError verifies that an error parameter from the provider is
// surfaced without attempting an exchange.
func TestOAuth2Login_CallbackProviderError(t *testing.T) {
	idp := newFakeIDP(t, "console-client")
	login := newTestLogin(t, idp)

	req := httptest.NewRequest(http.MethodGet, "/auth/callback?error=access_denied&error_description=nope", nil)
	req.AddCookie(&http.Cookie{Name: stateCookieName, Value: "state-xyz"})
	rec := httptest.NewRecorder()

	login.Callback(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 on a provider error, got %d", rec.Code)
	}
}

// TestOAuth2Login_Refresh verifies the refresh endpoint swaps the refresh cookie for a fresh session
// cookie and returns 204.
func TestOAuth2Login_Refresh(t *testing.T) {
	idp := newFakeIDP(t, "console-client")
	login := newTestLogin(t, idp)

	req := httptest.NewRequest(http.MethodPost, "/auth/refresh", nil)
	req.AddCookie(&http.Cookie{Name: refreshCookieName, Value: "test-refresh-token"})
	rec := httptest.NewRecorder()

	login.Refresh(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d (%s)", rec.Code, rec.Body.String())
	}

	if c := cookiesByName(rec)[SessionCookieName]; c == nil || c.Value != idp.accessToken {
		t.Errorf("expected a refreshed session cookie, got %+v", c)
	}
}

// TestOAuth2Login_RefreshNoCookie verifies a refresh without the refresh cookie is a 401.
func TestOAuth2Login_RefreshNoCookie(t *testing.T) {
	idp := newFakeIDP(t, "console-client")
	login := newTestLogin(t, idp)

	req := httptest.NewRequest(http.MethodPost, "/auth/refresh", nil)
	rec := httptest.NewRecorder()

	login.Refresh(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without a refresh cookie, got %d", rec.Code)
	}
}

// TestOAuth2Login_RefreshProviderFailureClearsSession verifies that when the provider rejects the
// refresh, the session cookies are cleared and a 401 is returned so the console re-prompts.
func TestOAuth2Login_RefreshProviderFailureClearsSession(t *testing.T) {
	idp := newFakeIDP(t, "console-client")
	login := newTestLogin(t, idp)

	idp.setFailToken(true)

	req := httptest.NewRequest(http.MethodPost, "/auth/refresh", nil)
	req.AddCookie(&http.Cookie{Name: refreshCookieName, Value: "stale-refresh-token"})
	rec := httptest.NewRecorder()

	login.Refresh(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 on refresh failure, got %d", rec.Code)
	}

	if c := cookiesByName(rec)[SessionCookieName]; c == nil || c.MaxAge >= 0 {
		t.Error("expected the session cookie to be cleared on refresh failure")
	}
}

// TestOAuth2Login_Logout verifies logout clears the cookies and redirects through the provider's
// end-session endpoint.
func TestOAuth2Login_Logout(t *testing.T) {
	idp := newFakeIDP(t, "console-client")
	login := newTestLogin(t, idp)

	req := httptest.NewRequest(http.MethodGet, "/auth/logout", nil)
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: "some-token"})
	rec := httptest.NewRecorder()

	login.Logout(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("expected 302, got %d", rec.Code)
	}

	if !strings.HasPrefix(rec.Header().Get("Location"), idp.server.URL+"/logout") {
		t.Errorf("expected redirect to the end-session endpoint, got %q", rec.Header().Get("Location"))
	}

	if c := cookiesByName(rec)[SessionCookieName]; c == nil || c.MaxAge >= 0 {
		t.Error("expected the session cookie to be cleared on logout")
	}
}

// TestNewOAuth2Login_MissingFields verifies each individually-missing required field is rejected at
// construction, not deferred to the first sign-in.
func TestNewOAuth2Login_MissingFields(t *testing.T) {
	idp := newFakeIDP(t, "console-client")

	idv, err := NewJWKSVerifier(JWKSConfig{Issuer: idp.server.URL, Audience: "console-client", RefreshInterval: time.Minute})
	if err != nil {
		t.Fatalf("NewJWKSVerifier: %s", err)
	}

	full := OAuth2Config{
		Issuer:          idp.server.URL,
		ClientID:        "console-client",
		ClientSecret:    "secret",
		RedirectURL:     "https://app.example/auth/callback",
		IDTokenVerifier: idv,
	}

	cases := map[string]func(*OAuth2Config){
		"issuer":       func(c *OAuth2Config) { c.Issuer = "" },
		"clientId":     func(c *OAuth2Config) { c.ClientID = "" },
		"clientSecret": func(c *OAuth2Config) { c.ClientSecret = "" },
		"redirectUrl":  func(c *OAuth2Config) { c.RedirectURL = "" },
		"verifier":     func(c *OAuth2Config) { c.IDTokenVerifier = nil },
	}

	for name, mangle := range cases {
		cfg := full
		mangle(&cfg)

		if _, err := NewOAuth2Login(cfg); err == nil {
			t.Errorf("expected an error when %s is missing", name)
		}
	}
}

// TestNewOAuth2Login_DiscoveryFailure verifies construction fails when the issuer's discovery
// document is unreachable, rather than starting a login that can never complete.
func TestNewOAuth2Login_DiscoveryFailure(t *testing.T) {
	idp := newFakeIDP(t, "console-client")

	idv, err := NewJWKSVerifier(JWKSConfig{Issuer: idp.server.URL, Audience: "console-client", RefreshInterval: time.Minute})
	if err != nil {
		t.Fatalf("NewJWKSVerifier: %s", err)
	}

	_, err = NewOAuth2Login(OAuth2Config{
		Issuer:          "http://127.0.0.1:1/nothing-listens-here",
		ClientID:        "console-client",
		ClientSecret:    "secret",
		RedirectURL:     "https://app.example/auth/callback",
		IDTokenVerifier: idv,
	})

	if err == nil {
		t.Error("expected construction to fail when discovery is unreachable")
	}
}

// TestOAuth2Login_LoginWithAudience verifies the optional audience parameter is added to the
// authorization redirect (the Auth0 path that yields a verifiable JWT access token).
func TestOAuth2Login_LoginWithAudience(t *testing.T) {
	idp := newFakeIDP(t, "console-client")

	idv, err := NewJWKSVerifier(JWKSConfig{Issuer: idp.server.URL, Audience: "console-client", RefreshInterval: time.Minute})
	if err != nil {
		t.Fatalf("NewJWKSVerifier: %s", err)
	}

	login, err := NewOAuth2Login(OAuth2Config{
		Issuer:          idp.server.URL,
		ClientID:        "console-client",
		ClientSecret:    "secret",
		RedirectURL:     "https://app.example/auth/callback",
		Audience:        "hippocampus-api",
		IDTokenVerifier: idv,
	})
	if err != nil {
		t.Fatalf("NewOAuth2Login: %s", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/auth/login", nil)
	rec := httptest.NewRecorder()

	login.Login(rec, req)

	loc, _ := url.Parse(rec.Header().Get("Location"))

	if loc.Query().Get("audience") != "hippocampus-api" {
		t.Errorf("expected the audience parameter on the authorization URL, got %q", loc.Query().Get("audience"))
	}
}

// TestOAuth2Login_CallbackMissingCode verifies a callback with a valid state but no code is rejected.
func TestOAuth2Login_CallbackMissingCode(t *testing.T) {
	idp := newFakeIDP(t, "console-client")
	login := newTestLogin(t, idp)

	req := httptest.NewRequest(http.MethodGet, "/auth/callback?state=state-xyz", nil)
	req.AddCookie(&http.Cookie{Name: stateCookieName, Value: "state-xyz"})
	rec := httptest.NewRecorder()

	login.Callback(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 with no code, got %d", rec.Code)
	}
}

// TestOAuth2Login_CallbackMissingPKCE verifies a callback missing the PKCE cookie is rejected before
// the exchange (the verifier is gone, so the exchange could never succeed).
func TestOAuth2Login_CallbackMissingPKCE(t *testing.T) {
	idp := newFakeIDP(t, "console-client")
	login := newTestLogin(t, idp)

	req := httptest.NewRequest(http.MethodGet, "/auth/callback?code=auth-code&state=state-xyz", nil)
	req.AddCookie(&http.Cookie{Name: stateCookieName, Value: "state-xyz"})
	req.AddCookie(&http.Cookie{Name: nonceCookieName, Value: "nonce-abc"})
	rec := httptest.NewRecorder()

	login.Callback(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 with no PKCE cookie, got %d", rec.Code)
	}
}

// TestOAuth2Login_CallbackExchangeFailure verifies that a token endpoint rejection surfaces as a 400
// with no session established.
func TestOAuth2Login_CallbackExchangeFailure(t *testing.T) {
	idp := newFakeIDP(t, "console-client")
	login := newTestLogin(t, idp)

	idp.setFailToken(true)

	req := httptest.NewRequest(http.MethodGet, "/auth/callback?code=auth-code&state=state-xyz", nil)
	req.AddCookie(&http.Cookie{Name: stateCookieName, Value: "state-xyz"})
	req.AddCookie(&http.Cookie{Name: nonceCookieName, Value: "nonce-abc"})
	req.AddCookie(&http.Cookie{Name: pkceCookieName, Value: "pkce-verifier"})
	rec := httptest.NewRecorder()

	login.Callback(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 on exchange failure, got %d", rec.Code)
	}

	if c := cookiesByName(rec)[SessionCookieName]; c != nil && c.Value != "" {
		t.Error("no session cookie should be set on a failed exchange")
	}
}

// TestOAuth2Login_CallbackNoIDToken verifies that a token response omitting the id_token is rejected
// (the id_token is what authenticates the user and carries the nonce).
func TestOAuth2Login_CallbackNoIDToken(t *testing.T) {
	idp := newFakeIDP(t, "console-client")
	login := newTestLogin(t, idp)

	idp.setOmitIDToken(true)

	req := httptest.NewRequest(http.MethodGet, "/auth/callback?code=auth-code&state=state-xyz", nil)
	req.AddCookie(&http.Cookie{Name: stateCookieName, Value: "state-xyz"})
	req.AddCookie(&http.Cookie{Name: nonceCookieName, Value: "nonce-abc"})
	req.AddCookie(&http.Cookie{Name: pkceCookieName, Value: "pkce-verifier"})
	rec := httptest.NewRecorder()

	login.Callback(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 when the id_token is absent, got %d", rec.Code)
	}
}

// TestOAuth2Login_CallbackMissingNonceCookie verifies that a valid id_token with no session nonce
// cookie to compare against is rejected - the nonce binding must be present.
func TestOAuth2Login_CallbackMissingNonceCookie(t *testing.T) {
	idp := newFakeIDP(t, "console-client")
	login := newTestLogin(t, idp)

	idp.configure("nonce-abc")

	req := httptest.NewRequest(http.MethodGet, "/auth/callback?code=auth-code&state=state-xyz", nil)
	req.AddCookie(&http.Cookie{Name: stateCookieName, Value: "state-xyz"})
	req.AddCookie(&http.Cookie{Name: pkceCookieName, Value: "pkce-verifier"})
	rec := httptest.NewRecorder()

	login.Callback(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 with no nonce cookie, got %d", rec.Code)
	}
}

// TestOAuth2Login_LogoutNoEndSession verifies logout falls back to the console redirect when the
// provider advertises no end-session endpoint.
func TestOAuth2Login_LogoutNoEndSession(t *testing.T) {
	idp := newFakeIDP(t, "console-client")
	idp.setNoEndSession(true)

	login := newTestLogin(t, idp)

	req := httptest.NewRequest(http.MethodGet, "/auth/logout", nil)
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: "some-token"})
	rec := httptest.NewRecorder()

	login.Logout(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("expected 302, got %d", rec.Code)
	}

	if rec.Header().Get("Location") != "/ui" {
		t.Errorf("expected fallback redirect to /ui, got %q", rec.Header().Get("Location"))
	}
}

// TestNonceFromToken_Malformed verifies the unverified nonce read returns empty for a non-JWT string
// rather than panicking.
func TestNonceFromToken_Malformed(t *testing.T) {
	if got := nonceFromToken("not-a-jwt"); got != "" {
		t.Errorf("expected empty nonce for a malformed token, got %q", got)
	}
}

// TestSubtleMismatch covers the constant-time comparison's branches: empty inputs mismatch, equal
// non-empty match, differing non-empty mismatch.
func TestSubtleMismatch(t *testing.T) {
	if !subtleMismatch("", "x") {
		t.Error("empty expectation should mismatch")
	}

	if !subtleMismatch("x", "") {
		t.Error("empty candidate should mismatch")
	}

	if subtleMismatch("same", "same") {
		t.Error("equal values should match")
	}

	if !subtleMismatch("a", "b") {
		t.Error("differing values should mismatch")
	}
}
