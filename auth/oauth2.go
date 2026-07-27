package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	log "github.com/sirupsen/logrus"
	"golang.org/x/oauth2"
)

// SessionCookieName is the cookie the server-side login flow sets on a successful sign-in, carrying
// the identity provider's access token. HTTPMiddleware reads it as a fallback when a request omits
// the Authorization header, so a browser that logged in via /auth/login is authenticated by the
// cookie alone - the token never has to live in page-readable storage. It is exported so the
// middleware and the wiring in main.go name the same cookie.
const SessionCookieName = "hippo_session"

// The remaining cookies are internal to the flow. The refresh cookie is scoped to /auth so it is
// only ever sent to the refresh/logout endpoints, never to the /v1 API. The three transient cookies
// carry the CSRF state, the OIDC nonce, and the PKCE verifier across the provider redirect and are
// cleared the moment the callback consumes them.
const (
	refreshCookieName = "hippo_refresh"
	stateCookieName   = "hippo_oauth_state"
	nonceCookieName   = "hippo_oauth_nonce"
	pkceCookieName    = "hippo_oauth_pkce"
)

// transientCookieTTL bounds how long the state/nonce/PKCE cookies live: long enough for a human to
// complete an interactive sign-in at the provider, short enough that a stale in-flight login cannot
// be resumed much later. The cookies are cleared on callback regardless, so this only caps an
// abandoned attempt.
const transientCookieTTL = 10 * time.Minute

// oauth2HTTPTimeout caps the discovery fetch and every back-channel token exchange, mirroring the
// JWKS fetch timeout - a hung identity provider must fail the request rather than stall it.
const oauth2HTTPTimeout = 15 * time.Second

// defaultSessionTTL is the access-token cookie lifetime used when the token itself carries no
// expiry. Any real OIDC access token has an exp, so this is only a floor.
const defaultSessionTTL = time.Hour

// defaultRefreshTTL is how long the browser keeps the refresh cookie. The refresh token's own
// lifetime is enforced by the provider; this only bounds how long the browser will offer it.
const defaultRefreshTTL = 7 * 24 * time.Hour

// OAuth2Config configures a server-side OIDC relying-party login - the confidential-client
// Authorization Code flow. Unlike the console's in-browser PKCE flow (a public client with no
// secret), this runs the code exchange on the server with a client secret, so it supports providers
// and deployments that require a confidential client, and keeps the token out of browser storage by
// setting it as an HttpOnly cookie.
type OAuth2Config struct {
	// Issuer is the OIDC issuer URL; the authorization, token, and end-session endpoints are
	// resolved from its discovery document.
	Issuer string

	// ClientID and ClientSecret are the confidential client's credentials, used on the back-channel
	// token exchange.
	ClientID     string
	ClientSecret string

	// RedirectURL must exactly match the callback registered on the provider's client (the service's
	// own .../auth/callback URL).
	RedirectURL string

	// Scopes are requested at the authorization endpoint. "openid" is always needed; "offline_access"
	// (or the provider's equivalent) is what yields a refresh token.
	Scopes []string

	// Audience, when set, is passed as the audience parameter so providers that otherwise issue an
	// opaque access token (Auth0) mint a verifiable JWT for the API instead. Providers that ignore it
	// (Keycloak) are unaffected.
	Audience string

	// IDTokenVerifier verifies the id_token returned by the exchange. It must enforce an audience of
	// ClientID (the id_token's aud), distinct from the access token's API audience - main.go builds a
	// dedicated JWKS verifier for it, reusing the same key machinery.
	IDTokenVerifier Verifier

	// CookieSecure sets the Secure attribute on every cookie. It should track whether the deployment
	// is served over HTTPS; a Secure cookie is dropped by the browser over plain HTTP, which would
	// silently break login on a local http test rig.
	CookieSecure bool

	// CookieDomain, when set, scopes the cookies to a domain (for a console reachable at a different
	// host than the token exchange). Empty leaves them host-only.
	CookieDomain string

	// PostLogoutRedirectURL is where the provider returns the browser after an RP-initiated logout.
	PostLogoutRedirectURL string

	// SuccessRedirectURL is where the browser lands after a successful login. Defaults to /ui.
	SuccessRedirectURL string

	// SessionTTL is the access-token cookie lifetime when the token carries no expiry; RefreshTTL is
	// the refresh cookie's lifetime. Both fall back to package defaults when zero.
	SessionTTL time.Duration
	RefreshTTL time.Duration
}

// OAuth2Login owns the server-side login flow: it holds the resolved provider endpoints and the
// oauth2 client config, and exposes the four HTTP handlers (Login, Callback, Refresh, Logout) that
// main.go registers under /auth.
type OAuth2Login struct {
	cfg           OAuth2Config
	oauth         *oauth2.Config
	endSessionURL string
	client        *http.Client
}

// oidcDiscovery is the subset of the OIDC discovery document this flow needs: the two endpoints the
// oauth2 config drives, plus the optional end-session endpoint for RP-initiated logout.
type oidcDiscovery struct {
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	EndSessionEndpoint    string `json:"end_session_endpoint"`
}

// NewOAuth2Login validates cfg, discovers the provider endpoints, and builds the login handler. A
// missing required field or an unreachable/incomplete discovery document fails construction, so a
// misconfigured login surfaces at startup rather than on the first sign-in.
func NewOAuth2Login(cfg OAuth2Config) (*OAuth2Login, error) {
	log.Trace("func() auth.NewOAuth2Login")

	if cfg.Issuer == "" {
		return nil, fmt.Errorf("auth: oauth2 login requires auth.oauth2.issuer (or auth.issuer)")
	}

	if cfg.ClientID == "" || cfg.ClientSecret == "" {
		return nil, fmt.Errorf("auth: oauth2 login requires auth.oauth2.clientId and clientSecret")
	}

	if cfg.RedirectURL == "" {
		return nil, fmt.Errorf("auth: oauth2 login requires auth.oauth2.redirectUrl")
	}

	if cfg.IDTokenVerifier == nil {
		return nil, fmt.Errorf("auth: oauth2 login requires an id_token verifier")
	}

	if cfg.SuccessRedirectURL == "" {
		cfg.SuccessRedirectURL = "/ui"
	}

	if cfg.SessionTTL <= 0 {
		cfg.SessionTTL = defaultSessionTTL
	}

	if cfg.RefreshTTL <= 0 {
		cfg.RefreshTTL = defaultRefreshTTL
	}

	client := &http.Client{Timeout: oauth2HTTPTimeout}

	disco, err := discoverOAuth2Endpoints(client, cfg.Issuer)
	if err != nil {
		return nil, err
	}

	l := &OAuth2Login{
		cfg:           cfg,
		endSessionURL: disco.EndSessionEndpoint,
		client:        client,
		oauth: &oauth2.Config{
			ClientID:     cfg.ClientID,
			ClientSecret: cfg.ClientSecret,
			RedirectURL:  cfg.RedirectURL,
			Scopes:       cfg.Scopes,
			Endpoint: oauth2.Endpoint{
				AuthURL:  disco.AuthorizationEndpoint,
				TokenURL: disco.TokenEndpoint,
			},
		},
	}

	return l, nil
}

// discoverOAuth2Endpoints fetches the provider's OIDC discovery document and returns the endpoints
// the login flow drives. It reuses the bounded-read discipline of the JWKS fetch: the response is
// read through a limit reader so a hostile or misconfigured endpoint cannot exhaust memory.
func discoverOAuth2Endpoints(client *http.Client, issuer string) (oidcDiscovery, error) {
	log.Trace("func() auth.discoverOAuth2Endpoints")

	var disco oidcDiscovery

	discoveryURL := strings.TrimSuffix(issuer, "/") + discoveryPath

	resp, err := client.Get(discoveryURL)
	if err != nil {
		return disco, fmt.Errorf("auth: failed to fetch OIDC discovery document from %s: %s", discoveryURL, err.Error())
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return disco, fmt.Errorf("auth: OIDC discovery endpoint %s returned status %d", discoveryURL, resp.StatusCode)
	}

	if err := json.NewDecoder(io.LimitReader(resp.Body, maxJWKSBytes)).Decode(&disco); err != nil {
		return disco, fmt.Errorf("auth: failed to parse OIDC discovery document: %s", err.Error())
	}

	if disco.AuthorizationEndpoint == "" || disco.TokenEndpoint == "" {
		return disco, fmt.Errorf("auth: OIDC discovery document from %s is missing the authorization or token endpoint", discoveryURL)
	}

	return disco, nil
}

// Login starts the flow. It mints a fresh CSRF state, an OIDC nonce, and a PKCE verifier, stashes
// them in short-lived cookies for the return leg, and redirects the browser to the provider's
// authorization endpoint. PKCE is included even though this is a confidential client - it is cheap
// belt-and-suspenders against code interception, and modern providers expect it.
func (l *OAuth2Login) Login(w http.ResponseWriter, r *http.Request) {
	log.Trace("func() auth.OAuth2Login.Login")

	state, err := randomToken()
	if err != nil {
		l.serverError(w, err)

		return
	}

	nonce, err := randomToken()
	if err != nil {
		l.serverError(w, err)

		return
	}

	verifier := oauth2.GenerateVerifier()

	l.setCookie(w, stateCookieName, state, "/auth", transientCookieTTL)
	l.setCookie(w, nonceCookieName, nonce, "/auth", transientCookieTTL)
	l.setCookie(w, pkceCookieName, verifier, "/auth", transientCookieTTL)

	opts := []oauth2.AuthCodeOption{
		oauth2.S256ChallengeOption(verifier),
		oauth2.SetAuthURLParam("nonce", nonce),
	}

	if l.cfg.Audience != "" {
		opts = append(opts, oauth2.SetAuthURLParam("audience", l.cfg.Audience))
	}

	http.Redirect(w, r, l.oauth.AuthCodeURL(state, opts...), http.StatusFound)
}

// Callback completes the flow. It validates the returned state against the stashed cookie, exchanges
// the code for tokens on the back channel (with the client secret and PKCE verifier), verifies the
// id_token's signature and nonce, and sets the session (and refresh) cookies before redirecting to
// the console. Any failure clears the transient cookies and returns an error page rather than a
// half-established session.
func (l *OAuth2Login) Callback(w http.ResponseWriter, r *http.Request) {
	log.Trace("func() auth.OAuth2Login.Callback")

	query := r.URL.Query()

	// Clear the transient cookies up front - queued on the response before any status is written, so
	// the deletions survive whichever branch (success redirect or error) writes the headers. The flow
	// either completes here or is abandoned, and a leftover state/nonce/verifier must never be
	// replayable against a later callback.
	l.clearTransientCookies(w)

	if provErr := query.Get("error"); provErr != "" {
		l.loginError(w, fmt.Errorf("provider returned %q: %s", provErr, query.Get("error_description")))

		return
	}

	state := query.Get("state")

	stateCookie, err := r.Cookie(stateCookieName)
	if err != nil || stateCookie.Value == "" || subtleMismatch(stateCookie.Value, state) {
		l.loginError(w, fmt.Errorf("state mismatch - the login could not be verified, please try again"))

		return
	}

	code := query.Get("code")
	if code == "" {
		l.loginError(w, fmt.Errorf("no authorization code returned"))

		return
	}

	verifierCookie, err := r.Cookie(pkceCookieName)
	if err != nil || verifierCookie.Value == "" {
		l.loginError(w, fmt.Errorf("missing PKCE verifier - please start the login again"))

		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), oauth2HTTPTimeout)
	defer cancel()

	ctx = context.WithValue(ctx, oauth2.HTTPClient, l.client)

	token, err := l.oauth.Exchange(ctx, code, oauth2.VerifierOption(verifierCookie.Value))
	if err != nil {
		log.Debugf("oauth2 code exchange failed: %s", err.Error())
		l.loginError(w, fmt.Errorf("token exchange failed"))

		return
	}

	if err := l.verifyIDToken(r, token); err != nil {
		log.Debugf("oauth2 id_token verification failed: %s", err.Error())
		l.loginError(w, fmt.Errorf("the identity provider's response could not be verified"))

		return
	}

	l.setSessionCookies(w, token)

	http.Redirect(w, r, l.cfg.SuccessRedirectURL, http.StatusFound)
}

// verifyIDToken checks the id_token the exchange returned: it must be present, verify against the
// provider's keys (signature, issuer, audience=ClientID, expiry - all enforced by the injected
// verifier), and carry the nonce this session issued. The nonce binds the id_token to this login,
// closing token-injection/replay even though the access token is separately verified on every API
// call.
func (l *OAuth2Login) verifyIDToken(r *http.Request, token *oauth2.Token) error {
	raw, ok := token.Extra("id_token").(string)
	if !ok || raw == "" {
		return fmt.Errorf("no id_token in token response")
	}

	if _, err := l.cfg.IDTokenVerifier.Verify(raw); err != nil {
		return err
	}

	nonceCookie, err := r.Cookie(nonceCookieName)
	if err != nil || nonceCookie.Value == "" {
		return fmt.Errorf("missing nonce cookie")
	}

	if subtleMismatch(nonceFromToken(raw), nonceCookie.Value) {
		return fmt.Errorf("nonce mismatch")
	}

	return nil
}

// Refresh silently swaps the refresh cookie for a new access token on the back channel and resets
// the session cookies. It is what lets a session outlive the (short) access-token lifetime without
// the browser holding any token. On any failure it clears the cookies and returns 401 so the
// console falls back to a fresh sign-in.
func (l *OAuth2Login) Refresh(w http.ResponseWriter, r *http.Request) {
	log.Trace("func() auth.OAuth2Login.Refresh")

	refreshCookie, err := r.Cookie(refreshCookieName)
	if err != nil || refreshCookie.Value == "" {
		unauthorized(w)

		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), oauth2HTTPTimeout)
	defer cancel()

	ctx = context.WithValue(ctx, oauth2.HTTPClient, l.client)

	token, err := l.oauth.TokenSource(ctx, &oauth2.Token{RefreshToken: refreshCookie.Value}).Token()
	if err != nil {
		log.Debugf("oauth2 token refresh failed: %s", err.Error())
		l.clearSessionCookies(w)
		unauthorized(w)

		return
	}

	l.setSessionCookies(w, token)

	w.WriteHeader(http.StatusNoContent)
}

// Logout clears the local session and, when the provider advertises an end-session endpoint,
// redirects the browser through an RP-initiated logout so the provider's own session is dropped too;
// otherwise it returns the browser to the console.
func (l *OAuth2Login) Logout(w http.ResponseWriter, r *http.Request) {
	log.Trace("func() auth.OAuth2Login.Logout")

	l.clearSessionCookies(w)

	if l.endSessionURL != "" {
		p := url.Values{"client_id": {l.cfg.ClientID}}

		if l.cfg.PostLogoutRedirectURL != "" {
			p.Set("post_logout_redirect_uri", l.cfg.PostLogoutRedirectURL)
		}

		http.Redirect(w, r, l.endSessionURL+"?"+p.Encode(), http.StatusFound)

		return
	}

	http.Redirect(w, r, l.cfg.SuccessRedirectURL, http.StatusFound)
}

// setSessionCookies records the access token as the session cookie and, when the exchange returned
// one, the refresh token as the /auth-scoped refresh cookie. The session cookie's lifetime tracks
// the access token's expiry so the browser drops it around the same time the token stops verifying.
func (l *OAuth2Login) setSessionCookies(w http.ResponseWriter, token *oauth2.Token) {
	sessionTTL := l.cfg.SessionTTL

	if !token.Expiry.IsZero() {
		if ttl := time.Until(token.Expiry); ttl > 0 {
			sessionTTL = ttl
		}
	}

	l.setCookie(w, SessionCookieName, token.AccessToken, "/", sessionTTL)

	if token.RefreshToken != "" {
		l.setCookie(w, refreshCookieName, token.RefreshToken, "/auth", l.cfg.RefreshTTL)
	}
}

// clearSessionCookies expires the session and refresh cookies.
func (l *OAuth2Login) clearSessionCookies(w http.ResponseWriter) {
	l.clearCookie(w, SessionCookieName, "/")
	l.clearCookie(w, refreshCookieName, "/auth")
}

// clearTransientCookies expires the state, nonce, and PKCE cookies used only during a single flow.
func (l *OAuth2Login) clearTransientCookies(w http.ResponseWriter) {
	l.clearCookie(w, stateCookieName, "/auth")
	l.clearCookie(w, nonceCookieName, "/auth")
	l.clearCookie(w, pkceCookieName, "/auth")
}

// setCookie writes an HttpOnly, SameSite=Lax cookie. HttpOnly keeps the token out of page-readable
// script (mitigating token theft via XSS - the reason to prefer this over browser storage), and
// SameSite=Lax lets the cookie ride the top-level redirect back from the provider while blocking it
// on cross-site subrequests, which is the CSRF mitigation for the cookie-authenticated API.
func (l *OAuth2Login) setCookie(w http.ResponseWriter, name string, value string, path string, ttl time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     path,
		Domain:   l.cfg.CookieDomain,
		MaxAge:   int(ttl.Seconds()),
		HttpOnly: true,
		Secure:   l.cfg.CookieSecure,
		SameSite: http.SameSiteLaxMode,
	})
}

// clearCookie expires a cookie by setting it empty with a negative max-age at the same path/domain.
func (l *OAuth2Login) clearCookie(w http.ResponseWriter, name string, path string) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    "",
		Path:     path,
		Domain:   l.cfg.CookieDomain,
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   l.cfg.CookieSecure,
		SameSite: http.SameSiteLaxMode,
	})
}

// serverError logs and returns a generic 500 for an internal failure (e.g. entropy exhaustion) that
// is not the caller's fault.
func (l *OAuth2Login) serverError(w http.ResponseWriter, err error) {
	log.Errorf("oauth2 login internal error: %s", err.Error())
	http.Error(w, "internal error", http.StatusInternalServerError)
}

// loginError returns a plain 400 describing a failed sign-in. The specific cause is kept terse to
// avoid leaking provider internals; the detail is logged, not returned.
func (l *OAuth2Login) loginError(w http.ResponseWriter, err error) {
	log.Infof("oauth2 login rejected: %s", err.Error())
	http.Error(w, "sign in failed: "+err.Error(), http.StatusBadRequest)
}

// nonceFromToken reads the nonce claim from an id_token without verifying it - the signature is
// checked separately by the id_token verifier; this only reads a value to compare, never makes a
// trust decision, mirroring rolesFromClaim.
func nonceFromToken(token string) string {
	var raw jwt.MapClaims

	if _, _, err := jwt.NewParser().ParseUnverified(token, &raw); err != nil {
		return ""
	}

	nonce, _ := raw["nonce"].(string)

	return nonce
}

// randomToken returns 32 bytes of cryptographic randomness, base64url-encoded, for the CSRF state
// and OIDC nonce.
func randomToken() (string, error) {
	buf := make([]byte, 32)

	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("auth: failed to read random bytes: %s", err.Error())
	}

	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// subtleMismatch reports whether a and b differ, in constant time, so a state/nonce comparison does
// not leak a timing side channel. It treats an empty expectation as a mismatch.
func subtleMismatch(a string, b string) bool {
	if a == "" || b == "" {
		return true
	}

	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) != 1
}
