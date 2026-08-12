package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// fakeIdP is a minimal OIDC provider: a discovery document pointing at its own token endpoint, and a
// client-credentials grant that hands out a numbered token so a refresh is visible.
type fakeIdP struct {
	server *httptest.Server

	mu        sync.Mutex
	issued    int
	discovery int
	forms     []map[string]string

	expiresIn  int64
	status     int
	body       string
	noDiscover bool
}

func newFakeIdP(t *testing.T) *fakeIdP {
	t.Helper()

	f := &fakeIdP{expiresIn: 3600}

	mux := http.NewServeMux()

	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.discovery++
		missing := f.noDiscover
		f.mu.Unlock()

		if missing {
			w.WriteHeader(http.StatusNotFound)

			return
		}

		_ = json.NewEncoder(w).Encode(map[string]string{"token_endpoint": f.server.URL + "/token"})
	})

	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()

		form := make(map[string]string, len(r.PostForm))
		for k := range r.PostForm {
			form[k] = r.PostForm.Get(k)
		}

		f.mu.Lock()
		f.issued++
		n := f.issued
		f.forms = append(f.forms, form)
		status, body, expires := f.status, f.body, f.expiresIn
		f.mu.Unlock()

		if status != 0 {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))

			return
		}

		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": fmt.Sprintf("token-%d", n),
			"expires_in":   expires,
		})
	})

	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)

	return f
}

func (f *fakeIdP) counts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.issued, f.discovery
}

func (f *fakeIdP) lastForm() map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()

	if len(f.forms) == 0 {
		return nil
	}

	return f.forms[len(f.forms)-1]
}

func TestClientCredentialsMintsAndCachesAToken(t *testing.T) {
	idp := newFakeIdP(t)

	src, err := newOIDCSource(OIDCConfig{
		Issuer:       idp.server.URL,
		ClientID:     "hippocampus-gen",
		ClientSecret: "s3cret",
	})
	if err != nil {
		t.Fatalf("newOIDCSource: %s", err)
	}

	ctx := context.Background()

	first, err := src.Token(ctx)
	if err != nil {
		t.Fatalf("Token: %s", err)
	}

	if first != "token-1" {
		t.Errorf("token = %q, want token-1", first)
	}

	// A second call inside the validity window must not go back to the IdP.
	second, err := src.Token(ctx)
	if err != nil {
		t.Fatalf("Token: %s", err)
	}

	if second != first {
		t.Errorf("token = %q, want the cached %q", second, first)
	}

	issued, discovered := idp.counts()

	if issued != 1 {
		t.Errorf("token endpoint called %d times, want 1", issued)
	}

	if discovered != 1 {
		t.Errorf("discovery called %d times, want 1", discovered)
	}

	form := idp.lastForm()

	if form["grant_type"] != "client_credentials" {
		t.Errorf("grant_type = %q", form["grant_type"])
	}

	if form["client_id"] != "hippocampus-gen" || form["client_secret"] != "s3cret" {
		t.Errorf("credentials not sent: %v", form)
	}
}

// TestClientCredentialsRefreshesBeforeExpiry is the whole reason this exists: a static token
// eventually expires and the bridge then fails every write, silently, for as long as it runs.
func TestClientCredentialsRefreshesBeforeExpiry(t *testing.T) {
	idp := newFakeIdP(t)

	// Shorter than refreshSkew, so the cached token is never considered usable and every call
	// refreshes - which is what a token approaching expiry looks like.
	idp.expiresIn = 1

	src, err := newOIDCSource(OIDCConfig{
		Issuer:       idp.server.URL,
		ClientID:     "id",
		ClientSecret: "secret",
	})
	if err != nil {
		t.Fatalf("newOIDCSource: %s", err)
	}

	ctx := context.Background()

	first, _ := src.Token(ctx)
	second, _ := src.Token(ctx)

	if first == second {
		t.Errorf("token %q was reused despite being about to expire", first)
	}

	if issued, _ := idp.counts(); issued != 2 {
		t.Errorf("token endpoint called %d times, want 2", issued)
	}
}

// TestClientCredentialsAssumesATTLWhenNoneIsGiven: a response without expires_in must not be cached
// forever, or the refresh never happens and the failure returns.
func TestClientCredentialsAssumesATTLWhenNoneIsGiven(t *testing.T) {
	idp := newFakeIdP(t)
	idp.expiresIn = 0

	src, _ := newOIDCSource(OIDCConfig{Issuer: idp.server.URL, ClientID: "id", ClientSecret: "s"})

	if _, err := src.Token(context.Background()); err != nil {
		t.Fatalf("Token: %s", err)
	}

	if src.expiry.IsZero() || time.Until(src.expiry) > defaultTokenTTL+time.Second {
		t.Errorf("expiry = %s, want roughly the default TTL from now", src.expiry)
	}
}

func TestClientCredentialsSendsScopeAndAudience(t *testing.T) {
	idp := newFakeIdP(t)

	src, _ := newOIDCSource(OIDCConfig{
		Issuer:       idp.server.URL,
		ClientID:     "id",
		ClientSecret: "s",
		Scope:        "memories:write",
		Audience:     "https://api.example",
	})

	if _, err := src.Token(context.Background()); err != nil {
		t.Fatalf("Token: %s", err)
	}

	form := idp.lastForm()

	if form["scope"] != "memories:write" {
		t.Errorf("scope = %q", form["scope"])
	}

	// Auth0 returns an opaque token without this; Keycloak ignores it.
	if form["audience"] != "https://api.example" {
		t.Errorf("audience = %q", form["audience"])
	}
}

func TestClientCredentialsOmitsUnsetOptionalFields(t *testing.T) {
	idp := newFakeIdP(t)

	src, _ := newOIDCSource(OIDCConfig{Issuer: idp.server.URL, ClientID: "id", ClientSecret: "s"})

	if _, err := src.Token(context.Background()); err != nil {
		t.Fatalf("Token: %s", err)
	}

	form := idp.lastForm()

	if _, ok := form["scope"]; ok {
		t.Error("an unset scope should not be sent at all")
	}

	if _, ok := form["audience"]; ok {
		t.Error("an unset audience should not be sent at all")
	}
}

func TestClientCredentialsTokenURLSkipsDiscovery(t *testing.T) {
	idp := newFakeIdP(t)

	src, _ := newOIDCSource(OIDCConfig{
		TokenURL:     idp.server.URL + "/token",
		ClientID:     "id",
		ClientSecret: "s",
	})

	if _, err := src.Token(context.Background()); err != nil {
		t.Fatalf("Token: %s", err)
	}

	if _, discovered := idp.counts(); discovered != 0 {
		t.Errorf("discovery called %d times, want 0 when a token url is given", discovered)
	}
}

func TestClientCredentialsErrors(t *testing.T) {
	t.Run("a rejected grant reports the provider's message", func(t *testing.T) {
		idp := newFakeIdP(t)
		idp.status = http.StatusUnauthorized
		idp.body = `{"error":"invalid_client"}`

		src, _ := newOIDCSource(OIDCConfig{Issuer: idp.server.URL, ClientID: "id", ClientSecret: "wrong"})

		_, err := src.Token(context.Background())
		if err == nil {
			t.Fatal("expected the rejection to be reported")
		}

		// The provider's own message is the useful part: it names the actual problem.
		if !strings.Contains(err.Error(), "invalid_client") {
			t.Errorf("error %q does not carry the provider's message", err)
		}
	})

	t.Run("a failed discovery is reported", func(t *testing.T) {
		idp := newFakeIdP(t)
		idp.noDiscover = true

		src, _ := newOIDCSource(OIDCConfig{Issuer: idp.server.URL, ClientID: "id", ClientSecret: "s"})

		if _, err := src.Token(context.Background()); err == nil {
			t.Error("expected the discovery failure to be reported")
		}
	})

	t.Run("an unreachable issuer is reported", func(t *testing.T) {
		src, _ := newOIDCSource(OIDCConfig{Issuer: "http://127.0.0.1:1", ClientID: "id", ClientSecret: "s"})

		if _, err := src.Token(context.Background()); err == nil {
			t.Error("expected the connection failure to be reported")
		}
	})

	t.Run("a token response with no access_token is reported", func(t *testing.T) {
		idp := newFakeIdP(t)
		idp.status = http.StatusOK
		idp.body = `{"expires_in":300}`

		src, _ := newOIDCSource(OIDCConfig{Issuer: idp.server.URL, ClientID: "id", ClientSecret: "s"})

		if _, err := src.Token(context.Background()); err == nil {
			t.Error("expected a missing access_token to be reported")
		}
	})

	t.Run("an undecodable token response is reported", func(t *testing.T) {
		idp := newFakeIdP(t)
		idp.status = http.StatusOK
		idp.body = `not json`

		src, _ := newOIDCSource(OIDCConfig{Issuer: idp.server.URL, ClientID: "id", ClientSecret: "s"})

		if _, err := src.Token(context.Background()); err == nil {
			t.Error("expected an undecodable response to be reported")
		}
	})
}

// TestNewOIDCSourceValidatesEagerly: configuration is checked without I/O, so a typo'd flag fails at
// startup rather than at whatever hour the first message happens to arrive.
func TestNewOIDCSourceValidatesEagerly(t *testing.T) {
	cases := []struct {
		name string
		cfg  OIDCConfig
	}{
		{name: "no client id", cfg: OIDCConfig{Issuer: "https://idp", ClientSecret: "s"}},
		{name: "no client secret", cfg: OIDCConfig{Issuer: "https://idp", ClientID: "id"}},
		{name: "neither issuer nor token url", cfg: OIDCConfig{ClientID: "id", ClientSecret: "s"}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := newOIDCSource(c.cfg); err == nil {
				t.Error("expected the configuration to be rejected")
			}
		})
	}
}

func TestTokenSourceSelection(t *testing.T) {
	t.Run("a client id selects the refreshing grant over a static token", func(t *testing.T) {
		// Both are set, which is what a deployment mid-migration looks like: an env file still
		// carrying a token beside the new client credentials. The refreshing source is the one it
		// meant.
		src, err := tokenSource(ClientConfig{
			Token: "static",
			OIDC:  OIDCConfig{Issuer: "https://idp", ClientID: "id", ClientSecret: "s"},
		})
		if err != nil {
			t.Fatalf("tokenSource: %s", err)
		}

		if _, ok := src.(*clientCredentialsSource); !ok {
			t.Errorf("source = %T, want the client-credentials source", src)
		}
	})

	t.Run("a token alone selects the static source", func(t *testing.T) {
		src, err := tokenSource(ClientConfig{Token: "static"})
		if err != nil {
			t.Fatalf("tokenSource: %s", err)
		}

		got, err := src.Token(context.Background())
		if err != nil || got != "static" {
			t.Errorf("Token = %q, %v", got, err)
		}
	})

	t.Run("no auth configured yields no source", func(t *testing.T) {
		src, err := tokenSource(ClientConfig{})
		if err != nil {
			t.Fatalf("tokenSource: %s", err)
		}

		if src != nil {
			t.Errorf("source = %v, want nil so the interceptor is left off entirely", src)
		}
	})

	t.Run("a malformed OIDC config is reported", func(t *testing.T) {
		if _, err := tokenSource(ClientConfig{OIDC: OIDCConfig{ClientID: "id"}}); err == nil {
			t.Error("expected the incomplete OIDC config to be rejected")
		}
	})
}

// TestDialRejectsAMalformedOIDCConfig pins that the validation actually reaches Dial, so a
// misconfigured bridge exits at startup instead of running and failing every write.
func TestDialRejectsAMalformedOIDCConfig(t *testing.T) {
	_, _, err := Dial(ClientConfig{
		Address: "localhost:50051",
		OIDC:    OIDCConfig{ClientID: "id"}, // no secret, no issuer
	})

	if err == nil {
		t.Fatal("expected Dial to reject the incomplete OIDC config")
	}

	if !strings.Contains(err.Error(), "bearer-token auth") {
		t.Errorf("error %q does not name the auth configuration", err)
	}
}

// TestBearerInterceptorMintsThroughTheGrant covers the seam end to end: the interceptor stamps a
// token the IdP actually issued.
func TestBearerInterceptorMintsThroughTheGrant(t *testing.T) {
	idp := newFakeIdP(t)

	src, _ := newOIDCSource(OIDCConfig{Issuer: idp.server.URL, ClientID: "id", ClientSecret: "s"})

	var seen metadata.MD

	invoker := func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, opts ...grpc.CallOption) error {
		seen, _ = metadata.FromOutgoingContext(ctx)

		return nil
	}

	if err := bearerInterceptor(src)(context.Background(), "/svc/M", nil, nil, nil, invoker); err != nil {
		t.Fatalf("interceptor: %s", err)
	}

	if got := seen.Get("authorization"); len(got) != 1 || got[0] != "Bearer token-1" {
		t.Errorf("authorization = %v, want [Bearer token-1]", got)
	}
}

// TestBearerInterceptorFailsTheRPCWhenNoTokenCanBeObtained: an unauthenticated call to a service
// that requires auth would be rejected anyway, and "obtaining bearer token" says what actually went
// wrong. For a bridge that means the message is not acked and is redelivered - the right outcome for
// a transient IdP outage.
func TestBearerInterceptorFailsTheRPCWhenNoTokenCanBeObtained(t *testing.T) {
	src, _ := newOIDCSource(OIDCConfig{Issuer: "http://127.0.0.1:1", ClientID: "id", ClientSecret: "s"})

	called := false

	invoker := func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, opts ...grpc.CallOption) error {
		called = true

		return nil
	}

	err := bearerInterceptor(src)(context.Background(), "/svc/M", nil, nil, nil, invoker)
	if err == nil {
		t.Fatal("expected the RPC to fail when no token could be obtained")
	}

	if called {
		t.Error("the RPC was invoked without a token")
	}
}

// TestBearerInterceptorOmitsAnEmptyToken: a misconfigured static source should reach the service as
// an unauthenticated call, which reports the real problem, rather than as a bare "Bearer ".
func TestBearerInterceptorOmitsAnEmptyToken(t *testing.T) {
	var seen metadata.MD

	invoker := func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, opts ...grpc.CallOption) error {
		seen, _ = metadata.FromOutgoingContext(ctx)

		return nil
	}

	if err := bearerInterceptor(staticSource{})(context.Background(), "/svc/M", nil, nil, nil, invoker); err != nil {
		t.Fatalf("interceptor: %s", err)
	}

	if got := seen.Get("authorization"); len(got) != 0 {
		t.Errorf("authorization = %v, want none", got)
	}
}

// TestClientCredentialsIsConcurrencySafe: several adapters' RPCs resolve a token at once, and the
// race detector is the point of this test.
func TestClientCredentialsIsConcurrencySafe(t *testing.T) {
	idp := newFakeIdP(t)

	src, _ := newOIDCSource(OIDCConfig{Issuer: idp.server.URL, ClientID: "id", ClientSecret: "s"})

	var wg sync.WaitGroup

	for range 16 {
		wg.Add(1)

		go func() {
			defer wg.Done()

			if _, err := src.Token(context.Background()); err != nil {
				t.Errorf("Token: %s", err)
			}
		}()
	}

	wg.Wait()

	// One fetch, not sixteen: the mutex serialises the first refresh and the rest read the cache.
	if issued, _ := idp.counts(); issued != 1 {
		t.Errorf("token endpoint called %d times, want 1", issued)
	}
}
