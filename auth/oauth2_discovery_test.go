package auth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestDiscoverOAuth2Endpoints_Rejections covers the three ways a discovery document is refused.
// Each is a distinct misconfiguration an operator has to be able to tell apart: the endpoint is not
// there, it is not serving JSON, or it is serving a document that does not describe a login flow.
func TestDiscoverOAuth2Endpoints_Rejections(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
		message string
	}{
		{
			name: "non-200 status",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusNotFound)
			},
			message: "returned status 404",
		},
		{
			name: "unparseable body",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte("this is not json"))
			},
			message: "failed to parse OIDC discovery document",
		},
		{
			name: "missing endpoints",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"issuer":"https://idp.example"}`))
			},
			message: "missing the authorization or token endpoint",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(test.handler)
			t.Cleanup(server.Close)

			_, err := discoverOAuth2Endpoints(server.Client(), server.URL)
			if err == nil {
				t.Fatal("expected the discovery document to be refused")
			}

			if !strings.Contains(err.Error(), test.message) {
				t.Errorf("expected the message to mention %q, got %q", test.message, err)
			}
		})
	}
}

// TestDiscoverOAuth2Endpoints_TrimsTrailingSlash pins that an issuer configured with a trailing
// slash still resolves to the right well-known path rather than one with a doubled separator.
func TestDiscoverOAuth2Endpoints_TrimsTrailingSlash(t *testing.T) {
	var requested string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requested = r.URL.Path

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"authorization_endpoint":"https://idp.example/authorize","token_endpoint":"https://idp.example/token"}`))
	}))
	t.Cleanup(server.Close)

	disco, err := discoverOAuth2Endpoints(server.Client(), server.URL+"/")
	if err != nil {
		t.Fatalf("discoverOAuth2Endpoints: %s", err)
	}

	if requested != discoveryPath {
		t.Errorf("expected the well-known path %q, got %q", discoveryPath, requested)
	}

	if disco.TokenEndpoint != "https://idp.example/token" {
		t.Errorf("unexpected token endpoint: %q", disco.TokenEndpoint)
	}
}

// rejectingVerifier refuses every token, standing in for a provider key rotation the service has
// not seen or an id_token minted for someone else.
type rejectingVerifier struct{}

func (rejectingVerifier) Verify(token string) (*Claims, error) {
	return nil, errRejected
}

var errRejected = &verifyError{}

type verifyError struct{}

func (*verifyError) Error() string { return "auth: id_token rejected" }

// TestOAuth2Login_CallbackRejectsUnverifiableIDToken pins that the id_token is verified against the
// provider's keys and not merely parsed. The access token is separately verified on every API call,
// but the id_token is what this flow trusts to establish who logged in.
func TestOAuth2Login_CallbackRejectsUnverifiableIDToken(t *testing.T) {
	idp := newFakeIDP(t, "test-client")

	login, err := NewOAuth2Login(OAuth2Config{
		Issuer:          idp.server.URL,
		ClientID:        idp.clientID,
		ClientSecret:    "test-secret",
		RedirectURL:     "https://app.example/auth/callback",
		Scopes:          []string{"openid"},
		IDTokenVerifier: rejectingVerifier{},
	})
	if err != nil {
		t.Fatalf("NewOAuth2Login: %s", err)
	}

	idp.configure("nonce-abc")

	request := httptest.NewRequest(http.MethodGet, "/auth/callback?code=auth-code&state=state-xyz", nil)
	request.AddCookie(&http.Cookie{Name: stateCookieName, Value: "state-xyz"})
	request.AddCookie(&http.Cookie{Name: nonceCookieName, Value: "nonce-abc"})
	request.AddCookie(&http.Cookie{Name: pkceCookieName, Value: "pkce-verifier"})

	rec := httptest.NewRecorder()
	login.Callback(rec, request)

	if rec.Code == http.StatusFound {
		t.Errorf("expected an unverifiable id_token to fail the callback, got a redirect")
	}
}

// TestOAuth2Login_LogoutCarriesThePostLogoutRedirect covers the RP-initiated logout carrying the
// configured return URL, so the provider sends the browser back to the console rather than leaving
// it on the provider's own page.
func TestOAuth2Login_LogoutCarriesThePostLogoutRedirect(t *testing.T) {
	idp := newFakeIDP(t, "test-client")

	idv, err := NewJWKSVerifier(JWKSConfig{
		Issuer:          idp.server.URL,
		Audience:        idp.clientID,
		RefreshInterval: time.Minute,
	})
	if err != nil {
		t.Fatalf("NewJWKSVerifier: %s", err)
	}

	login, err := NewOAuth2Login(OAuth2Config{
		Issuer:                idp.server.URL,
		ClientID:              idp.clientID,
		ClientSecret:          "test-secret",
		RedirectURL:           "https://app.example/auth/callback",
		Scopes:                []string{"openid"},
		IDTokenVerifier:       idv,
		PostLogoutRedirectURL: "https://app.example/",
	})
	if err != nil {
		t.Fatalf("NewOAuth2Login: %s", err)
	}

	rec := httptest.NewRecorder()
	login.Logout(rec, httptest.NewRequest(http.MethodGet, "/auth/logout", nil))

	if rec.Code != http.StatusFound {
		t.Fatalf("expected a redirect to the provider's end-session endpoint, got %d", rec.Code)
	}

	location := rec.Header().Get("Location")
	if !strings.Contains(location, "post_logout_redirect_uri=https%3A%2F%2Fapp.example%2F") {
		t.Errorf("expected the post-logout redirect in the location, got %q", location)
	}
}
