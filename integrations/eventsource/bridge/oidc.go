package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// Bearer-token sources for the bridges.
//
// A static --token is fine for a hand-run bridge, but it is the wrong shape for a long-running one
// against an IdP: the token expires and the bridge starts failing every write with Unauthenticated,
// silently, for as long as it is left running. The OIDC client-credentials grant is the machine-to-
// machine answer - the bridge holds a client id and secret, mints its own access tokens, and
// refreshes them before they expire.
//
// This mirrors the generators' implementation in the hippocampus-gen repo, deliberately: that is
// what the hosted showcase already authenticates with, so a bridge added to that stack is configured
// with the same three flags and against the same realm. It could not simply be imported - it lives
// under internal/ in a different module - so behaviour parity is maintained by matching it, down to
// the Auth0 audience quirk below.

// refreshSkew is how far ahead of expiry a token is refreshed, so a long RPC never travels with an
// about-to-expire token.
const refreshSkew = 30 * time.Second

// defaultTokenTTL is assumed when a token response omits expires_in, so the source still refreshes
// rather than caching one token forever.
const defaultTokenTTL = 5 * time.Minute

// oidcHTTPTimeout bounds a single call to the discovery or token endpoint.
const oidcHTTPTimeout = 15 * time.Second

// oidcBodyLimit caps how much of a token/discovery response is read, so a misbehaving endpoint
// cannot stream an unbounded body into memory.
const oidcBodyLimit = 1 << 20

// TokenSource yields a currently-valid bearer token. Token may block to fetch or refresh one.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
}

// staticSource returns a fixed token supplied out of band (--token).
type staticSource struct {
	token string
}

func (s staticSource) Token(_ context.Context) (string, error) {
	return s.token, nil
}

// OIDCConfig configures a machine-to-machine client-credentials grant.
//
// Either TokenURL (the token endpoint directly) or Issuer (from which it is discovered via
// <issuer>/.well-known/openid-configuration) must be set, along with a client id and secret.
type OIDCConfig struct {
	// Issuer is the OIDC issuer to discover the token endpoint from.
	Issuer string

	// TokenURL is the token endpoint, used in place of discovery when set.
	TokenURL string

	// ClientID and ClientSecret are the machine client's credentials. A non-empty ClientID is what
	// selects this grant over a static token.
	ClientID     string
	ClientSecret string

	// Scope is an optional space-separated scope list.
	Scope string

	// Audience is required by providers whose access token is opaque without one (Auth0's API
	// identifier); Keycloak ignores it.
	Audience string

	// HTTPClient is optional and defaults to one with a sane timeout.
	HTTPClient *http.Client
}

// clientCredentialsSource fetches an access token via the client-credentials grant and caches it
// until shortly before it expires. It is safe for concurrent use.
//
// Endpoint discovery is LAZY - resolved on the first token fetch rather than at construction - so a
// bridge does not fail to start because its IdP happened to be unreachable at that moment. A
// supervised daemon that retries is the better failure mode than one that exits. What IS checked
// eagerly is the configuration itself (newOIDCSource below), so a typo'd flag fails immediately
// rather than at whatever hour the first message arrives.
type clientCredentialsSource struct {
	cfg        OIDCConfig
	httpClient *http.Client

	mu       sync.Mutex
	tokenURL string
	token    string
	expiry   time.Time
}

// newOIDCSource validates the configuration and returns a source. It performs no I/O.
func newOIDCSource(cfg OIDCConfig) (*clientCredentialsSource, error) {
	if cfg.ClientID == "" || cfg.ClientSecret == "" {
		return nil, fmt.Errorf("client-credentials auth requires both a client id and a secret")
	}

	if cfg.TokenURL == "" && cfg.Issuer == "" {
		return nil, fmt.Errorf("client-credentials auth requires either a token url or an issuer")
	}

	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: oidcHTTPTimeout}
	}

	return &clientCredentialsSource{
		cfg:        cfg,
		httpClient: httpClient,
		tokenURL:   cfg.TokenURL,
	}, nil
}

// Token returns a cached token while one is comfortably valid, otherwise fetches a fresh one.
func (s *clientCredentialsSource) Token(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.token != "" && time.Now().Add(refreshSkew).Before(s.expiry) {
		return s.token, nil
	}

	if s.tokenURL == "" {
		discovered, err := discoverTokenURL(ctx, s.httpClient, s.cfg.Issuer)
		if err != nil {
			return "", err
		}

		s.tokenURL = discovered
	}

	if err := s.fetch(ctx); err != nil {
		return "", err
	}

	return s.token, nil
}

// fetch posts the client-credentials grant and stores the resulting token and its expiry. The caller
// holds s.mu.
func (s *clientCredentialsSource) fetch(ctx context.Context) error {
	form := url.Values{}
	form.Set("grant_type", "client_credentials")
	form.Set("client_id", s.cfg.ClientID)
	form.Set("client_secret", s.cfg.ClientSecret)

	if s.cfg.Scope != "" {
		form.Set("scope", s.cfg.Scope)
	}

	// Auth0 returns an opaque token unless an API audience is requested; Keycloak ignores it.
	if s.cfg.Audience != "" {
		form.Set("audience", s.cfg.Audience)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("building token request: %w", err)
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("requesting token: %w", err)
	}

	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, oidcBodyLimit))
	if err != nil {
		return fmt.Errorf("reading token response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		// The body is included because a client-credentials rejection is almost always a readable
		// "invalid_client" or "unauthorized_client" that names the actual problem.
		return fmt.Errorf("token endpoint returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var tr struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}

	if err := json.Unmarshal(body, &tr); err != nil {
		return fmt.Errorf("decoding token response: %w", err)
	}

	if tr.AccessToken == "" {
		return fmt.Errorf("token response carried no access_token")
	}

	ttl := time.Duration(tr.ExpiresIn) * time.Second
	if ttl <= 0 {
		ttl = defaultTokenTTL
	}

	s.token = tr.AccessToken
	s.expiry = time.Now().Add(ttl)

	return nil
}

// discoverTokenURL reads the provider's OIDC metadata and returns its token endpoint.
func discoverTokenURL(ctx context.Context, httpClient *http.Client, issuer string) (string, error) {
	metaURL := strings.TrimRight(issuer, "/") + "/.well-known/openid-configuration"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, metaURL, nil)
	if err != nil {
		return "", fmt.Errorf("building discovery request: %w", err)
	}

	req.Header.Set("Accept", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetching OIDC discovery document: %w", err)
	}

	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, oidcBodyLimit))
	if err != nil {
		return "", fmt.Errorf("reading OIDC discovery document: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("OIDC discovery returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var meta struct {
		TokenEndpoint string `json:"token_endpoint"`
	}

	if err := json.Unmarshal(body, &meta); err != nil {
		return "", fmt.Errorf("decoding OIDC discovery document: %w", err)
	}

	if meta.TokenEndpoint == "" {
		return "", fmt.Errorf("OIDC discovery document carried no token_endpoint")
	}

	return meta.TokenEndpoint, nil
}

// tokenSource picks the auth a ClientConfig describes: a set OIDC client id selects the
// client-credentials grant, otherwise a non-empty Token selects the static one, otherwise there is
// no auth to configure and the interceptor is left off entirely.
//
// The client id wins over a static token deliberately rather than erroring: a deployment moving from
// one to the other will pass both for a while (an env file still carrying HIPPOCAMPUS_<BROKER>_TOKEN
// beside the new client credentials), and the refreshing source is unambiguously the one it meant.
func tokenSource(cfg ClientConfig) (TokenSource, error) {
	if cfg.OIDC.ClientID != "" {
		return newOIDCSource(cfg.OIDC)
	}

	if cfg.Token != "" {
		return staticSource{token: cfg.Token}, nil
	}

	return nil, nil
}

// bearerInterceptor stamps "authorization: Bearer <token>" onto every RPC's outgoing metadata,
// resolving the token from src each call (which is a cached read except when a refresh is due).
//
// A token that cannot be obtained fails the RPC rather than sending none: an unauthenticated call to
// a service that requires auth would be rejected anyway, and reporting "obtaining bearer token" says
// what actually went wrong. For a bridge that means the message is not acked and is redelivered,
// which is the right outcome for a transient IdP outage.
func bearerInterceptor(src TokenSource) grpc.UnaryClientInterceptor {
	return func(
		ctx context.Context,
		method string,
		req any,
		reply any,
		cc *grpc.ClientConn,
		invoker grpc.UnaryInvoker,
		opts ...grpc.CallOption,
	) error {
		token, err := src.Token(ctx)
		if err != nil {
			return fmt.Errorf("obtaining bearer token: %w", err)
		}

		// An empty token is left off rather than sent as a bare "Bearer ", so a misconfigured static
		// source reaches the service as an unauthenticated call - which reports the real problem.
		if token != "" {
			ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
		}

		return invoker(ctx, method, req, reply, cc, opts...)
	}
}
