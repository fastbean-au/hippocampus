package main

import (
	"context"
	"crypto/tls"
	"net/http"
	"strings"
	"testing"
	"time"

	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
	"github.com/spf13/viper"
)

// Wiring branches in run that only fire under a configuration nothing else here sets: the callback
// sink, mutual TLS, the CORS wrapper, the group-scope decorator, and the OpenAPI opt-out. Each is a
// few lines of construction and a log line, and each is reached only by starting the whole server -
// which is why they were the residue left after the unit-testable halves of main.go were covered.

// runUntilHealthy starts run, waits for the gateway to answer, then cancels and asserts a clean
// return. Every test below has that shape, and what differs is only the configuration set before it.
func runUntilHealthy(t *testing.T, client *http.Client, healthURL string) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- run(ctx, versionInfo{}) }()

	waitForOK(t, client, healthURL)

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

// TestRun_CallbackSinkConfigured covers the sink's construction. It is the one optional dependency
// whose absence is invisible - a store with callbacks enabled and no sink built would queue a
// delivery per forgotten memory and drain none of them - so what this pins is that an enabled
// configuration actually reaches notify.NewWebhook and that the server starts with it.
func TestRun_CallbackSinkConfigured(t *testing.T) {
	_, gwBase := baseRunConfig(t)

	hook := logtest.NewLocal(log.StandardLogger())
	t.Cleanup(hook.Reset)

	viper.Set("callbacks.enabled", true)
	viper.Set("callbacks.url", "https://hooks.internal/forgotten")
	viper.Set("callbacks.timeoutSeconds", 5)

	t.Cleanup(func() {
		viper.Set("callbacks.enabled", nil)
		viper.Set("callbacks.url", nil)
		viper.Set("callbacks.timeoutSeconds", nil)
	})

	runUntilHealthy(t, http.DefaultClient, gwBase+"/healthz")

	if !logContains(hook, "callbacks enabled, delivering to") {
		t.Error("expected the callback sink's startup line")
	}
}

// TestRun_CallbackSinkConstructionError covers the failure arm. A sink that cannot be built is
// fatal rather than degraded, for the reason above: the alternative is a queue nothing drains.
func TestRun_CallbackSinkConstructionError(t *testing.T) {
	baseRunConfig(t)

	viper.Set("callbacks.enabled", true)
	viper.Set("callbacks.url", "https://hooks.internal/forgotten")
	viper.Set("callbacks.tls.caCertFile", "/nonexistent/ca.pem")

	t.Cleanup(func() {
		viper.Set("callbacks.enabled", nil)
		viper.Set("callbacks.url", nil)
		viper.Set("callbacks.tls.caCertFile", nil)
	})

	err := run(context.Background(), versionInfo{})
	if err == nil {
		t.Fatal("expected an unbuildable callback sink to fail startup")
	}

	if !strings.Contains(err.Error(), "callback sink") {
		t.Errorf("the error does not name the callback sink: %s", err)
	}
}

// TestRun_MutualTLS covers both mutual-TLS arms and the warning that goes with the strict one.
//
// The gateway's probes are the reason the warning exists: a required client certificate covers
// /healthz and /readyz too, since the handshake precedes the request, so an orchestrator probing
// without one sees a listener that refuses every check while the service is perfectly healthy.
func TestRun_MutualTLS(t *testing.T) {
	for _, required := range []bool{false, true} {
		name := "verified when offered"
		if required {
			name = "required"
		}

		t.Run(name, func(t *testing.T) {
			_, gwBase := baseRunConfig(t)

			hook := logtest.NewLocal(log.StandardLogger())
			t.Cleanup(hook.Reset)

			cert, key := writeSelfSignedCert(t)

			viper.Set("tls.enabled", true)
			viper.Set("tls.certFile", cert)
			viper.Set("tls.keyFile", key)
			viper.Set("tls.clientCaFile", cert)
			viper.Set("tls.requireClientCert", required)

			t.Cleanup(func() {
				for _, k := range []string{
					"tls.enabled", "tls.certFile", "tls.keyFile",
					"tls.clientCaFile", "tls.requireClientCert",
				} {
					viper.Set(k, nil)
				}
			})

			// The same throwaway pair serves as the client's certificate and as the CA both ends
			// verify against, which is what makes a real handshake testable without a hierarchy.
			pair, err := tls.LoadX509KeyPair(cert, key)
			if err != nil {
				t.Fatalf("LoadX509KeyPair: %s", err)
			}

			client := &http.Client{
				Transport: &http.Transport{
					TLSClientConfig: &tls.Config{
						InsecureSkipVerify: true,
						Certificates:       []tls.Certificate{pair},
					},
				},
			}

			httpsBase := strings.Replace(gwBase, "http://", "https://", 1)

			runUntilHealthy(t, client, httpsBase+"/healthz")

			if !logContains(hook, "mutual TLS enabled on both listeners: client certificates "+name) {
				t.Errorf("expected the mutual-TLS startup line naming %q", name)
			}

			if got := logContains(hook, "applies to the gateway's /healthz"); got != required {
				t.Errorf("the probe warning was logged = %t, want %t", got, required)
			}
		})
	}
}

// TestRun_GatewayCORS covers the CORS wrapper. It is opt-in and off by default, so nothing else
// starting the gateway reaches it - and the middleware is what stands between a browser page on a
// declared origin being able to call /v1 and not.
func TestRun_GatewayCORS(t *testing.T) {
	_, gwBase := baseRunConfig(t)

	hook := logtest.NewLocal(log.StandardLogger())
	t.Cleanup(hook.Reset)

	viper.Set("gateway.corsOrigins", []string{"https://console.internal"})
	t.Cleanup(func() { viper.Set("gateway.corsOrigins", nil) })

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- run(ctx, versionInfo{}) }()

	waitForOK(t, http.DefaultClient, gwBase+"/healthz")

	request, err := http.NewRequest(http.MethodGet, gwBase+"/v1/openapi.json", nil)
	if err != nil {
		t.Fatalf("NewRequest: %s", err)
	}

	request.Header.Set("Origin", "https://console.internal")

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("GET with an Origin: %s", err)
	}

	_ = response.Body.Close()

	if got := response.Header.Get("Access-Control-Allow-Origin"); got != "https://console.internal" {
		t.Errorf("Access-Control-Allow-Origin = %q, want the declared origin", got)
	}

	// Credentials are never allowed, so a session cookie cannot be used cross-origin.
	if got := response.Header.Get("Access-Control-Allow-Credentials"); got != "" {
		t.Errorf("Access-Control-Allow-Credentials = %q, want it absent", got)
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

	if !logContains(hook, "gateway CORS enabled for") {
		t.Error("expected the CORS startup line naming the origins")
	}
}

// TestRun_OpenAPIDisabled covers the opt-out. The document is served by default, so this is the arm
// nothing else takes - and the log line is load-bearing: a client that cannot find the spec needs to
// be told it was switched off rather than left to conclude the gateway is broken.
func TestRun_OpenAPIDisabled(t *testing.T) {
	_, gwBase := baseRunConfig(t)

	hook := logtest.NewLocal(log.StandardLogger())
	t.Cleanup(hook.Reset)

	viper.Set("gateway.openapi.enabled", false)
	t.Cleanup(func() { viper.Set("gateway.openapi.enabled", nil) })

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- run(ctx, versionInfo{}) }()

	waitForOK(t, http.DefaultClient, gwBase+"/healthz")

	response, err := http.Get(gwBase + "/v1/openapi.json")
	if err != nil {
		t.Fatalf("GET /v1/openapi.json: %s", err)
	}

	_ = response.Body.Close()

	if response.StatusCode == http.StatusOK {
		t.Error("the OpenAPI document is still served with gateway.openapi.enabled false")
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

	if !logContains(hook, "OpenAPI document disabled") {
		t.Error("expected the line naming what a client must do instead")
	}
}

// TestRun_RequireGroupScope covers the decorator. An unscoped token is the MOST privileged shape
// there is rather than the least, so a deployment that partitions its store by group needs a way to
// refuse one - and this is the only place that wraps the verifier to do it.
func TestRun_RequireGroupScope(t *testing.T) {
	_, gwBase := baseRunConfig(t)

	hook := logtest.NewLocal(log.StandardLogger())
	t.Cleanup(hook.Reset)

	viper.Set("auth.method", "hmac")
	viper.Set("auth.signingSecret", "a-test-signing-secret")
	viper.Set("auth.requireGroupScope", true)

	t.Cleanup(func() {
		viper.Set("auth.method", nil)
		viper.Set("auth.signingSecret", nil)
		viper.Set("auth.requireGroupScope", nil)
	})

	// /healthz is exempt from authentication, so it still answers on an authenticated instance.
	runUntilHealthy(t, http.DefaultClient, gwBase+"/healthz")

	if !logContains(hook, "auth.requireGroupScope is set") {
		t.Error("expected the line saying unscoped tokens will be rejected")
	}
}

// logContains reports whether any captured entry's message contains substr.
func logContains(hook *logtest.Hook, substr string) bool {
	for _, entry := range hook.AllEntries() {
		if strings.Contains(entry.Message, substr) {
			return true
		}
	}

	return false
}
