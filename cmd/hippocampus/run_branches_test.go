package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
	"github.com/spf13/viper"
)

// TestRun_ConsolidationDisabledSQLiteWarns covers the consolidation.enabled=false branch: with the
// sqlite driver this cannot be a horizontal-scaling replica (the store is an embedded file), so run
// must log a warning while still starting and serving normally.
func TestRun_ConsolidationDisabledSQLiteWarns(t *testing.T) {
	_, gwBase := baseRunConfig(t)
	viper.Set("consolidation.enabled", false)

	hook := logtest.NewGlobal()
	log.SetLevel(log.InfoLevel)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- run(ctx, versionInfo{}) }()

	waitForOK(t, http.DefaultClient, gwBase+"/healthz")

	found := false

	for _, entry := range hook.AllEntries() {
		if entry.Level == log.WarnLevel {
			found = true

			break
		}
	}

	if !found {
		t.Error("expected a warning log entry for consolidation.enabled=false with storage.driver 'sqlite'")
	}

	cancel()

	select {

	case err := <-done:
		if err != nil {
			t.Fatalf("run returned an error: %v", err)
		}

	case <-time.After(20 * time.Second):
		t.Fatal("run did not return after cancellation")

	}
}

// TestRun_PostgresWalTriggerRejected and TestRun_MySQLWalTriggerRejected cover run's early rejection
// of consolidation.walTriggerBytes on the postgres/mysql drivers: WAL-triggered sleep is
// SQLite-specific (there is no client-visible WAL file to measure on either server driver), so this
// must fail fast, before any connection is attempted.
func TestRun_PostgresWalTriggerRejected(t *testing.T) {
	baseRunConfig(t)
	viper.Set("storage.driver", "postgres")
	viper.Set("consolidation.walTriggerBytes", 1024)

	if err := run(context.Background(), versionInfo{}); err == nil {
		t.Fatal("expected run to reject consolidation.walTriggerBytes with storage.driver 'postgres'")
	}
}

func TestRun_MySQLWalTriggerRejected(t *testing.T) {
	baseRunConfig(t)
	viper.Set("storage.driver", "mysql")
	viper.Set("consolidation.walTriggerBytes", 1024)

	if err := run(context.Background(), versionInfo{}); err == nil {
		t.Fatal("expected run to reject consolidation.walTriggerBytes with storage.driver 'mysql'")
	}
}

// TestRun_UnreachablePostgresErrors and TestRun_UnreachableMySQLErrors cover run's database-open
// error path for the postgres/mysql drivers: dialing a DSN nothing listens on fails fast (connection
// refused) and run must return that error rather than starting the server.
func TestRun_UnreachablePostgresErrors(t *testing.T) {
	baseRunConfig(t)
	viper.Set("storage.driver", "postgres")
	viper.Set("storage.postgres.dsn", "postgres://bogus:bogus@127.0.0.1:1/bogus?sslmode=disable&connect_timeout=1")

	if err := run(context.Background(), versionInfo{}); err == nil {
		t.Fatal("expected run to fail opening an unreachable postgres database")
	}
}

func TestRun_UnreachableMySQLErrors(t *testing.T) {
	baseRunConfig(t)
	viper.Set("storage.driver", "mysql")
	viper.Set("storage.mysql.dsn", "bogus:bogus@tcp(127.0.0.1:1)/bogus?timeout=1s")

	if err := run(context.Background(), versionInfo{}); err == nil {
		t.Fatal("expected run to fail opening an unreachable mysql database")
	}
}

// TestRun_OpenSearchEnabled covers the opensearch.enabled success branch: construction only needs a
// syntactically valid address, not a reachable cluster (ensureIndex's failure is logged and
// best-effort), so pointing at an address nothing listens on still lets run start and serve
// normally with the search index wired in.
func TestRun_OpenSearchEnabled(t *testing.T) {
	_, gwBase := baseRunConfig(t)
	viper.Set("opensearch.enabled", true)
	viper.Set("opensearch.addresses", []string{"http://127.0.0.1:1"})
	viper.Set("opensearch.index", "test-index")

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- run(ctx, versionInfo{}) }()

	waitForOK(t, http.DefaultClient, gwBase+"/healthz")

	cancel()

	select {

	case err := <-done:
		if err != nil {
			t.Fatalf("run returned an error: %v", err)
		}

	case <-time.After(20 * time.Second):
		t.Fatal("run did not return after cancellation")

	}
}

// TestRun_OpenSearchInitError covers the opensearch.enabled error branch: a malformed TLS block
// (only one of certFile/keyFile set) fails search.NewOpenSearch's construction synchronously, so run
// must return that error before starting the server.
func TestRun_OpenSearchInitError(t *testing.T) {
	baseRunConfig(t)
	viper.Set("opensearch.enabled", true)
	viper.Set("opensearch.tls.certFile", "/cert-with-no-matching-key.pem")

	if err := run(context.Background(), versionInfo{}); err == nil {
		t.Fatal("expected run to fail on a malformed opensearch tls configuration")
	}
}

// TestRun_S3Configured covers the s3.bucket success branch: constructing the S3 object store only
// resolves the AWS SDK's default credential/config chain, which succeeds without a reachable AWS
// endpoint, so run must start and serve normally with the archive object store wired in.
func TestRun_S3Configured(t *testing.T) {
	_, gwBase := baseRunConfig(t)
	viper.Set("s3.bucket", "test-bucket")
	viper.Set("s3.region", "us-east-1")

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- run(ctx, versionInfo{}) }()

	waitForOK(t, http.DefaultClient, gwBase+"/healthz")

	cancel()

	select {

	case err := <-done:
		if err != nil {
			t.Fatalf("run returned an error: %v", err)
		}

	case <-time.After(20 * time.Second):
		t.Fatal("run did not return after cancellation")

	}
}

// TestRun_S3ConfigError covers the s3.bucket error branch: pointing AWS_CONFIG_FILE at a directory
// (instead of a file) makes the AWS SDK's default config load fail synchronously, so run must return
// that error before starting the server.
func TestRun_S3ConfigError(t *testing.T) {
	baseRunConfig(t)
	viper.Set("s3.bucket", "test-bucket")

	badConfigDir := t.TempDir()
	t.Setenv("AWS_CONFIG_FILE", badConfigDir)

	if err := run(context.Background(), versionInfo{}); err == nil {
		t.Fatal("expected run to fail when the AWS default config cannot be loaded")
	}
}

// TestRun_AuthEnabledLegacyAlias covers the deprecated auth.enabled boolean: when auth.method is
// unset, auth.enabled=true must still select the hmac verifier (with a warning logged), preserving
// behaviour for configs written before auth.method existed.
func TestRun_AuthEnabledLegacyAlias(t *testing.T) {
	_, gwBase := baseRunConfig(t)
	viper.Set("auth.enabled", true)
	viper.Set("auth.signingSecret", "test-secret")

	hook := logtest.NewGlobal()
	log.SetLevel(log.InfoLevel)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- run(ctx, versionInfo{}) }()

	waitForOK(t, http.DefaultClient, gwBase+"/healthz")

	found := false

	for _, entry := range hook.AllEntries() {
		if entry.Level == log.WarnLevel {
			found = true

			break
		}
	}

	if !found {
		t.Error("expected a deprecation warning for auth.enabled")
	}

	cancel()

	select {

	case err := <-done:
		if err != nil {
			t.Fatalf("run returned an error: %v", err)
		}

	case <-time.After(20 * time.Second):
		t.Fatal("run did not return after cancellation")

	}
}

// TestRun_TLSLoadError covers the TLS-enabled bootstrap error path: a missing/unreadable
// certificate-key pair must fail run before any listener starts.
func TestRun_TLSLoadError(t *testing.T) {
	baseRunConfig(t)
	viper.Set("tls.enabled", true)
	viper.Set("tls.certFile", "/nonexistent/cert.pem")
	viper.Set("tls.keyFile", "/nonexistent/key.pem")

	if err := run(context.Background(), versionInfo{}); err == nil {
		t.Fatal("expected run to fail loading a nonexistent TLS certificate/key pair")
	}
}

// TestRun_HMACVerifierError covers auth.NewHMACVerifier's construction failure: auth.method 'hmac'
// with neither a legacy signingSecret nor any signingKeys configured must fail run rather than start
// with no usable signing key.
func TestRun_HMACVerifierError(t *testing.T) {
	baseRunConfig(t)
	viper.Set("auth.method", "hmac")

	if err := run(context.Background(), versionInfo{}); err == nil {
		t.Fatal("expected run to fail building the hmac verifier with no signing secret configured")
	}
}

// TestRun_UnknownAuthMethod covers the auth.method default case: an unrecognised value must fail
// run rather than silently falling back to no authentication.
func TestRun_UnknownAuthMethod(t *testing.T) {
	baseRunConfig(t)
	viper.Set("auth.method", "bogus")

	if err := run(context.Background(), versionInfo{}); err == nil {
		t.Fatal("expected run to reject an unknown auth.method")
	}
}

// TestRun_IdpAuthError covers auth.NewJWKSVerifier's construction failure via run's idp branch: with
// neither auth.jwksUrl nor auth.issuer configured, construction fails fast, before any network call.
func TestRun_IdpAuthError(t *testing.T) {
	baseRunConfig(t)
	viper.Set("auth.method", "idp")

	if err := run(context.Background(), versionInfo{}); err == nil {
		t.Fatal("expected run to fail building the idp verifier with neither jwksUrl nor issuer configured")
	}
}

// testRSAJWK renders an RSA public key as a minimal JWK map, the shape a JWKS endpoint serves.
func testRSAJWK(t *testing.T, kid string) map[string]string {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %s", err)
	}

	return map[string]string{
		"kty": "RSA",
		"use": "sig",
		"alg": "RS256",
		"kid": kid,
		"n":   base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
		"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
	}
}

// TestRun_IdpAuthSuccess covers the idp branch's success path: a reachable JWKS endpoint serving at
// least one usable RSA key lets construction succeed, so run starts and serves with the idp verifier
// wired in (and, since tls.enabled is false, logs the plaintext-bearer-token warning too).
func TestRun_IdpAuthSuccess(t *testing.T) {
	_, gwBase := baseRunConfig(t)

	jwk := testRSAJWK(t, "kid-1")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{jwk}})
	}))
	t.Cleanup(srv.Close)

	viper.Set("auth.method", "idp")
	viper.Set("auth.jwksUrl", srv.URL)

	hook := logtest.NewGlobal()
	log.SetLevel(log.InfoLevel)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- run(ctx, versionInfo{}) }()

	waitForOK(t, http.DefaultClient, gwBase+"/healthz")

	found := false

	for _, entry := range hook.AllEntries() {
		if entry.Level == log.WarnLevel {
			found = true

			break
		}
	}

	if !found {
		t.Error("expected the plaintext-bearer-token warning (auth enabled, tls disabled)")
	}

	cancel()

	select {

	case err := <-done:
		if err != nil {
			t.Fatalf("run returned an error: %v", err)
		}

	case <-time.After(20 * time.Second):
		t.Fatal("run did not return after cancellation")

	}
}

// TestRun_RevocationEnabled and TestRun_RevocationError cover the auth.revocationFile branch: a
// valid revocation list file loads successfully and is stopped cleanly on shutdown, while a malformed
// one fails run's bootstrap.
func TestRun_RevocationEnabled(t *testing.T) {
	_, gwBase := baseRunConfig(t)
	viper.Set("auth.method", "hmac")
	viper.Set("auth.signingSecret", "test-secret")

	path := filepath.Join(t.TempDir(), "revoked.json")
	if err := os.WriteFile(path, []byte(`{"jtis":[],"clients":[]}`), 0o600); err != nil {
		t.Fatalf("write revocation file: %s", err)
	}

	viper.Set("auth.revocationFile", path)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- run(ctx, versionInfo{}) }()

	waitForOK(t, http.DefaultClient, gwBase+"/healthz")

	cancel()

	select {

	case err := <-done:
		if err != nil {
			t.Fatalf("run returned an error: %v", err)
		}

	case <-time.After(20 * time.Second):
		t.Fatal("run did not return after cancellation")

	}
}

func TestRun_RevocationError(t *testing.T) {
	baseRunConfig(t)
	viper.Set("auth.method", "hmac")
	viper.Set("auth.signingSecret", "test-secret")

	path := filepath.Join(t.TempDir(), "revoked.json")
	if err := os.WriteFile(path, []byte(`{not valid json`), 0o600); err != nil {
		t.Fatalf("write revocation file: %s", err)
	}

	viper.Set("auth.revocationFile", path)

	if err := run(context.Background(), versionInfo{}); err == nil {
		t.Fatal("expected run to fail loading a malformed revocation file")
	}
}

// TestRun_ObservabilityEnabled covers the branch that installs the otelgrpc stats handler: when
// tracing or metrics is enabled, run must still start and serve normally even against an OTLP
// endpoint nothing listens on, since the exporters build lazily. Shrinking shutdown.timeoutSeconds
// also makes this deterministically hit the shutdownObservability error-logging branch: the flush
// against the unreachable endpoint cannot succeed within the shortened deadline.
func TestRun_ObservabilityEnabled(t *testing.T) {
	_, gwBase := baseRunConfig(t)
	viper.Set("observability.tracing.enabled", true)
	viper.Set("observability.tracing.samplingRatio", 1.0)
	viper.Set("observability.metrics.enabled", true)
	viper.Set("observability.otlp.endpoint", "127.0.0.1:1")
	viper.Set("observability.otlp.insecure", true)
	viper.Set("shutdown.timeoutSeconds", 1)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- run(ctx, versionInfo{}) }()

	waitForOK(t, http.DefaultClient, gwBase+"/healthz")

	cancel()

	select {

	case err := <-done:
		if err != nil {
			t.Fatalf("run returned an error: %v", err)
		}

	case <-time.After(20 * time.Second):
		t.Fatal("run did not return after cancellation")

	}
}

// TestRun_OpenAPIEndpoint covers the /v1/openapi.json handler registered on the gateway mux, which
// no other test exercises: it must serve the embedded swagger document with a 200.
func TestRun_OpenAPIEndpoint(t *testing.T) {
	_, gwBase := baseRunConfig(t)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- run(ctx, versionInfo{}) }()

	waitForOK(t, http.DefaultClient, gwBase+"/healthz")

	resp, err := http.Get(gwBase + "/v1/openapi.json")
	if err != nil {
		t.Fatalf("GET /v1/openapi.json: %s", err)
	}
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 from /v1/openapi.json, got %d", resp.StatusCode)
	}

	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("expected application/json content type, got %q", ct)
	}

	cancel()

	select {

	case err := <-done:
		if err != nil {
			t.Fatalf("run returned an error: %v", err)
		}

	case <-time.After(20 * time.Second):
		t.Fatal("run did not return after cancellation")

	}
}

// TestRun_PostgresIntegration and TestRun_MySQLIntegration exercise run's full postgres/mysql
// bootstrap - including the connection-pool-limit branch that only runs after a successful open -
// against a real disposable database. They skip locally when the corresponding DSN environment
// variable is unset, matching the db package's own integration test convention; CI supplies both via
// service containers (see .github/workflows/ci.yaml).
func TestRun_PostgresIntegration(t *testing.T) {
	dsn := os.Getenv("HIPPOCAMPUS_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("HIPPOCAMPUS_TEST_POSTGRES_DSN not set")
	}

	_, gwBase := baseRunConfig(t)
	viper.Set("storage.driver", "postgres")
	viper.Set("storage.postgres.dsn", dsn)
	viper.Set("storage.pool.maxOpenConns", 5)
	viper.Set("storage.pool.maxIdleConns", 2)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- run(ctx, versionInfo{}) }()

	waitForOK(t, http.DefaultClient, gwBase+"/healthz")
	waitForOK(t, http.DefaultClient, gwBase+"/readyz")

	cancel()

	select {

	case err := <-done:
		if err != nil {
			t.Fatalf("run returned an error: %v", err)
		}

	case <-time.After(20 * time.Second):
		t.Fatal("run did not return after cancellation")

	}
}

// hangingDocIndexServer answers the opensearch index-exists/mapping checks normally (so
// construction and ensureIndex succeed) but never responds to a document index request, so the
// async worker's in-flight apply attempt is still blocked when shutdown runs.
type hangingDocIndexServer struct {
	block chan struct{}
}

func (h *hangingDocIndexServer) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/_doc/") {
			<-h.block

			return
		}

		if r.Body != nil {
			_, _ = io.ReadAll(r.Body)
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}
}

// TestRun_SearchIndexCloseTimeout covers run's searchIndex.Close() error-logging branch: a memory
// write queued into the async opensearch worker whose document-index request never gets a response
// is still in flight when shutdown runs, so Close's bounded drain wait (shortened here via
// opensearch.closeDrainTimeoutSeconds) times out and run must log the failure and still shut down
// cleanly rather than hang or error out.
func TestRun_SearchIndexCloseTimeout(t *testing.T) {
	_, gwBase := baseRunConfig(t)

	fake := &hangingDocIndexServer{block: make(chan struct{})}
	server := httptest.NewServer(fake.handler())
	t.Cleanup(func() {
		close(fake.block)
		server.Close()
	})

	viper.Set("opensearch.enabled", true)
	viper.Set("opensearch.addresses", []string{server.URL})
	viper.Set("opensearch.index", "test-index")
	viper.Set("opensearch.applyTimeoutSeconds", 30)
	viper.Set("opensearch.closeDrainTimeoutSeconds", 1)
	viper.Set("shutdown.timeoutSeconds", 5)

	hook := logtest.NewGlobal()
	log.SetLevel(log.InfoLevel)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- run(ctx, versionInfo{}) }()

	waitForOK(t, http.DefaultClient, gwBase+"/healthz")

	resp, err := http.Post(gwBase+"/v1/memories", "application/json", strings.NewReader(`{"body":"hello world"}`))
	if err != nil {
		t.Fatalf("POST /v1/memories: %s", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 creating the memory, got %d", resp.StatusCode)
	}

	// Give the async worker a moment to pick the write off the queue and land in the hung HTTP call
	// before shutdown begins.
	time.Sleep(100 * time.Millisecond)

	cancel()

	select {

	case err := <-done:
		if err != nil {
			t.Fatalf("run returned an error: %v", err)
		}

	case <-time.After(20 * time.Second):
		t.Fatal("run did not return after cancellation")

	}

	found := false

	for _, entry := range hook.AllEntries() {
		if entry.Level == log.ErrorLevel && strings.Contains(entry.Message, "failed to close search index cleanly") {
			found = true

			break
		}
	}

	if !found {
		t.Error("expected an error log entry for a search index close that timed out draining the queue")
	}
}

func TestRun_MySQLIntegration(t *testing.T) {
	dsn := os.Getenv("HIPPOCAMPUS_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("HIPPOCAMPUS_TEST_MYSQL_DSN not set")
	}

	_, gwBase := baseRunConfig(t)
	viper.Set("storage.driver", "mysql")
	viper.Set("storage.mysql.dsn", dsn)
	viper.Set("storage.pool.maxOpenConns", 5)
	viper.Set("storage.pool.maxIdleConns", 2)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- run(ctx, versionInfo{}) }()

	waitForOK(t, http.DefaultClient, gwBase+"/healthz")
	waitForOK(t, http.DefaultClient, gwBase+"/readyz")

	cancel()

	select {

	case err := <-done:
		if err != nil {
			t.Fatalf("run returned an error: %v", err)
		}

	case <-time.After(20 * time.Second):
		t.Fatal("run did not return after cancellation")

	}
}
