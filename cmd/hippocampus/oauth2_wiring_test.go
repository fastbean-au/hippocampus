package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/spf13/viper"
)

// startFakeIDPForWiring stands up the minimum an OIDC provider must expose for run to build the
// oauth2 login at startup: a discovery document and a JWKS (both id_token and access-token verifiers
// fetch keys during construction). No token endpoint is needed - this test exercises only the
// wiring, /ui/config, and the /auth/login redirect, none of which perform a code exchange.
func startFakeIDPForWiring(t *testing.T) *httptest.Server {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}

	mux := http.NewServeMux()
	var base string

	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"issuer":                 base,
			"authorization_endpoint": base + "/authorize",
			"token_endpoint":         base + "/token",
			"jwks_uri":               base + "/jwks",
			"end_session_endpoint":   base + "/logout",
		})
	})

	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "RSA",
			"use": "sig",
			"alg": "RS256",
			"kid": "idp-key-1",
			"n":   base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
			"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
		}}})
	})

	srv := httptest.NewServer(mux)
	base = srv.URL
	t.Cleanup(srv.Close)

	return srv
}

// TestRun_OAuth2Login covers the server-side OIDC login wiring end to end: run builds the oauth2
// login from an auth.oauth2 config (which discovers the provider and fetches its keys at startup),
// registers the /auth/* handlers, and advertises server mode to the console. It asserts /ui/config
// reports loginMode "server" and GET /auth/login redirects to the provider's authorization endpoint.
func TestRun_OAuth2Login(t *testing.T) {
	idp := startFakeIDPForWiring(t)

	_, gwBase := baseRunConfig(t)

	viper.Set("auth.method", "idp")
	viper.Set("auth.issuer", idp.URL)
	viper.Set("auth.audience", "hippocampus-api")
	viper.Set("auth.oauth2.enabled", true)
	viper.Set("auth.oauth2.clientId", "hippocampus-console")
	viper.Set("auth.oauth2.clientSecret", "test-secret")
	viper.Set("auth.oauth2.redirectUrl", gwBase+"/auth/callback")

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- run(ctx, versionInfo{}) }()

	waitForOK(t, http.DefaultClient, gwBase+"/healthz")

	// /ui/config must report server mode so the console links to /auth/login rather than running the
	// in-browser PKCE flow.
	resp, err := http.DefaultClient.Get(gwBase + "/ui/config")
	if err != nil {
		t.Fatalf("GET /ui/config: %v", err)
	}

	var cfg struct {
		AuthMethod string `json:"authMethod"`
		LoginMode  string `json:"loginMode"`
	}

	_ = json.NewDecoder(resp.Body).Decode(&cfg)
	_ = resp.Body.Close()

	if cfg.AuthMethod != "idp" || cfg.LoginMode != "server" {
		t.Fatalf("expected idp/server in /ui/config, got %+v", cfg)
	}

	// GET /auth/login must be reachable without a token and redirect to the provider's authorization
	// endpoint carrying the flow parameters.
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}

	loginResp, err := noRedirect.Get(gwBase + "/auth/login")
	if err != nil {
		t.Fatalf("GET /auth/login: %v", err)
	}
	_ = loginResp.Body.Close()

	if loginResp.StatusCode != http.StatusFound {
		t.Fatalf("expected 302 from /auth/login, got %d", loginResp.StatusCode)
	}

	loc, err := url.Parse(loginResp.Header.Get("Location"))
	if err != nil {
		t.Fatalf("parse Location: %v", err)
	}

	idpURL, _ := url.Parse(idp.URL)

	if loc.Host != idpURL.Host || loc.Path != "/authorize" {
		t.Fatalf("expected redirect to the provider /authorize, got %s", loc.String())
	}

	if loc.Query().Get("code_challenge") == "" || loc.Query().Get("state") == "" {
		t.Errorf("authorization redirect missing PKCE challenge or state: %s", loc.RawQuery)
	}

	cancel()

	select {

	case err := <-done:
		if err != nil {
			t.Fatalf("run returned an error on clean shutdown: %v", err)
		}

	case <-time.After(20 * time.Second):
		t.Fatal("run did not return after context cancellation")

	}
}
