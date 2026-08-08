package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spf13/viper"

	"github.com/fastbean-au/hippocampus/auth"
)

// TestRun_EmbeddingWithoutOpenSearchIsRefused pins the startup precondition: the OpenSearch k-NN
// index is the only place this service can put vectors, so enabling the embedder without it would
// cost every write an embedding call whose result nothing could ever search. Failing at startup
// says that once, where a warning would leave it to be inferred from search results that never
// improve.
func TestRun_EmbeddingWithoutOpenSearchIsRefused(t *testing.T) {
	baseRunConfig(t)
	viper.Set("ollama.embedding.enabled", true)
	viper.Set("opensearch.enabled", false)

	err := run(context.Background(), versionInfo{})
	if err == nil {
		t.Fatal("expected the embedder to be refused without opensearch.enabled")
	}

	if !strings.Contains(err.Error(), "requires opensearch.enabled") {
		t.Errorf("expected the message to name the missing prerequisite, got %q", err)
	}
}

// TestRun_EmbeddingInitErrorIsReported covers the embedder's own construction failure. NewOllama
// checks only what it can check without the network - an address and a model name - so this is what
// an embedding block enabled but left half-configured produces.
func TestRun_EmbeddingInitErrorIsReported(t *testing.T) {
	baseRunConfig(t)
	viper.Set("opensearch.enabled", true)
	viper.Set("opensearch.addresses", []string{"http://127.0.0.1:1"})
	viper.Set("ollama.embedding.enabled", true)
	viper.Set("ollama.embedding.address", "http://127.0.0.1:1")
	viper.Set("ollama.embedding.model", "")

	err := run(context.Background(), versionInfo{})
	if err == nil {
		t.Fatal("expected an embedder with no model configured to fail startup")
	}

	if !strings.Contains(err.Error(), "ollama embedder") {
		t.Errorf("expected the message to name the embedder, got %q", err)
	}
}

// TestRun_EmbeddingEnabledSucceeds covers the success arm. Construction needs no reachable model
// server - the dimension is validated against what the model returns on the first Embed, not at
// startup - so run starts and serves with the embedder wired in.
func TestRun_EmbeddingEnabledSucceeds(t *testing.T) {
	_, gwBase := baseRunConfig(t)
	viper.Set("opensearch.enabled", true)
	viper.Set("opensearch.addresses", []string{"http://127.0.0.1:1"})
	viper.Set("ollama.embedding.enabled", true)
	viper.Set("ollama.embedding.address", "http://127.0.0.1:1")
	viper.Set("ollama.embedding.model", "nomic-embed-text")
	viper.Set("ollama.embedding.timeoutSeconds", 5)
	viper.Set("ollama.embedding.dimensions", 8)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- run(ctx, versionInfo{}) }()

	waitForOK(t, http.DefaultClient, gwBase+"/healthz")

	cancel()

	if err := <-done; err != nil {
		t.Errorf("run: %s", err)
	}
}

// TestRun_RoleMappingTypeErrorIsReported covers the auth.roleMapping read failing, which is what a
// mapping written as something other than a string-to-string table produces.
func TestRun_RoleMappingTypeErrorIsReported(t *testing.T) {
	baseRunConfig(t)
	viper.Set("auth.method", "hmac")
	viper.Set("auth.signingSecret", "0123456789abcdef0123456789abcdef")
	viper.Set("auth.roleMapping", []string{"not", "a", "map"})

	err := run(context.Background(), versionInfo{})
	if err == nil {
		t.Fatal("expected a malformed auth.roleMapping to fail startup")
	}

	if !strings.Contains(err.Error(), "roleMapping") {
		t.Errorf("expected the message to name the setting, got %q", err)
	}
}

// TestRun_AuthoriserErrorIsReported covers the authoriser rejecting its mapping - a role mapped to
// a tier that does not exist, which would otherwise silently deny that role every RPC.
func TestRun_AuthoriserErrorIsReported(t *testing.T) {
	baseRunConfig(t)
	viper.Set("auth.method", "hmac")
	viper.Set("auth.signingSecret", "0123456789abcdef0123456789abcdef")
	viper.Set("auth.roleMapping", map[string]string{"team-a": "not-a-tier"})

	if err := run(context.Background(), versionInfo{}); err == nil {
		t.Fatal("expected an unknown tier in auth.roleMapping to fail startup")
	}
}

// TestRun_RateLimitingActive covers the rate-limiting wiring: an active limiter registers its
// gauge and installs both the arrival and per-principal interceptors, and with authentication off
// it says so, since per-client limits then key on the caller's address.
func TestRun_RateLimitingActive(t *testing.T) {
	_, gwBase := baseRunConfig(t)
	viper.Set("rateLimit.enabled", true)
	viper.Set("rateLimit.global.requestsPerSecond", 1000.0)
	viper.Set("rateLimit.global.burst", 1000.0)
	viper.Set("rateLimit.perClient.requestsPerSecond", 1000.0)
	viper.Set("rateLimit.perClient.burst", 1000.0)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- run(ctx, versionInfo{}) }()

	waitForOK(t, http.DefaultClient, gwBase+"/healthz")

	cancel()

	if err := <-done; err != nil {
		t.Errorf("run: %s", err)
	}
}

// TestRun_RateLimitConfigErrorIsReported covers a rate-limit rule the limiter refuses.
func TestRun_RateLimitConfigErrorIsReported(t *testing.T) {
	baseRunConfig(t)
	viper.Set("rateLimit.enabled", true)
	viper.Set("rateLimit.global.requestsPerSecond", -1.0)

	if err := run(context.Background(), versionInfo{}); err == nil {
		t.Fatal("expected a negative rate to fail startup")
	}
}

// TestHTTPPrincipalKey_FallsBackToSubject pins the ordering the per-principal bucket keys on: a
// token's client_id names the caller most precisely, but an IdP-issued token need not carry one,
// and the subject is what identifies the caller there. Falling through to the address instead would
// put every caller behind one proxy in a single bucket.
func TestHTTPPrincipalKey_FallsBackToSubject(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/v1/memories", nil)

	claims := &auth.Claims{}
	claims.Subject = "user-42"

	request = request.WithContext(auth.ContextWithClaims(request.Context(), claims))

	if got := httpPrincipalKey(request, false); got != "sub:user-42" {
		t.Errorf("expected the subject to key the bucket, got %q", got)
	}
}

// TestHTTPPrincipalKey_PrefersClientID is the other half of that ordering.
func TestHTTPPrincipalKey_PrefersClientID(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/v1/memories", nil)

	claims := &auth.Claims{ClientID: "svc-a"}
	claims.Subject = "user-42"

	request = request.WithContext(auth.ContextWithClaims(request.Context(), claims))

	if got := httpPrincipalKey(request, false); got != "client:svc-a" {
		t.Errorf("expected the client id to win over the subject, got %q", got)
	}
}
