package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/viper"
)

// issuedPorts records every port freeTCPPort has handed out in this process, so no two tests in a
// run can be given the same one.
var (
	issuedPortsMu sync.Mutex
	issuedPorts   = map[int]struct{}{}
)

// The window freeTCPPort draws from. It sits below the ephemeral range every platform this builds
// for allocates from (Linux defaults to 32768-60999, macOS to 49152-65535), which is the whole
// point: see freeTCPPort.
const (
	testPortRangeStart = 20000
	testPortRangeEnd   = 32000
	testPortAttempts   = 200
)

// freeTCPPort returns a localhost port for the server under test to bind, having confirmed it is
// bindable and remembered it so no later call in this process returns it again.
//
// It deliberately does NOT ask the kernel for an ephemeral port ("127.0.0.1:0") and then release
// it. run binds the port itself, so any reservation has to be released before the server starts,
// and go test runs this package's binary beside other packages' - several of which hold ephemeral
// listeners of their own. The kernel is free to hand one of them the very port just released, which
// surfaces here as run failing with "bind: address already in use" and a test timing out on a
// /healthz that never comes up. Drawing from a range nothing is allocated from automatically leaves
// only a deliberate collision to lose to, and the probe below catches that.
func freeTCPPort(t *testing.T) int {
	t.Helper()

	for range testPortAttempts {
		port := testPortRangeStart + rand.IntN(testPortRangeEnd-testPortRangeStart)

		if !claimPort(port) {
			continue
		}

		l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			// Something outside this process holds it; it stays claimed so it is not retried.
			continue
		}

		_ = l.Close()

		return port
	}

	t.Fatalf("no free port found in %d-%d after %d attempts", testPortRangeStart, testPortRangeEnd, testPortAttempts)

	return 0
}

// claimPort records a port as issued, reporting whether this call is the one that claimed it.
func claimPort(port int) bool {
	issuedPortsMu.Lock()
	defer issuedPortsMu.Unlock()

	if _, taken := issuedPorts[port]; taken {
		return false
	}

	issuedPorts[port] = struct{}{}

	return true
}

// baseRunConfig sets the minimum viper keys run needs (main sets some of these as defaults before
// calling run, so a direct run test must supply them itself), using a free gRPC and gateway port
// and a temporary sqlite database. It returns the gateway base URL.
func baseRunConfig(t *testing.T) (grpcPort int, gatewayBase string) {
	t.Helper()

	viper.Reset()
	t.Cleanup(viper.Reset)

	grpcPort = freeTCPPort(t)
	gwPort := freeTCPPort(t)

	viper.Set("storage.driver", "sqlite")
	viper.Set("storage.directory", t.TempDir())
	viper.Set("bindAddress", "127.0.0.1")
	viper.Set("port", grpcPort)
	viper.Set("gateway.bindAddress", "127.0.0.1")
	viper.Set("gateway.port", gwPort)

	viper.Set("consolidation.unitsOfAgeInDays", 1.0)
	viper.Set("consolidation.method", 1)
	viper.Set("consolidation.aggressiveness", 1.0)
	viper.Set("sleep.periodSeconds", 0) // no timed sleep cycle during the test

	// Defaults main sets before run; supply them here since the test calls run directly.
	viper.Set("storage.pool.maxOpenConns", 25)
	viper.Set("storage.queryTimeoutSeconds", 60)
	viper.Set("shutdown.timeoutSeconds", 10)
	viper.Set("stats.intervalSeconds", 0)

	return grpcPort, fmt.Sprintf("http://127.0.0.1:%d", gwPort)
}

// waitForOK polls url with the given client until it returns 200 or the deadline passes.
func waitForOK(t *testing.T, client *http.Client, url string) {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)

	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()

			if resp.StatusCode == http.StatusOK {
				return
			}
		}

		time.Sleep(50 * time.Millisecond)
	}

	t.Fatalf("timed out waiting for %s to return 200", url)
}

// TestRun_ServesAndShutsDown starts the full server via run on ephemeral ports, confirms the gRPC
// listener and HTTP gateway (including its /healthz and /readyz endpoints) come up, then cancels
// the context and asserts run drains and returns nil - covering the whole bootstrap, serve, and
// graceful-shutdown lifecycle in-process.
func TestRun_ServesAndShutsDown(t *testing.T) {
	grpcPort, gwBase := baseRunConfig(t)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- run(ctx, versionInfo{Version: "test"}) }()

	// The gateway's /healthz proves the HTTP listener is up; /readyz proves the DB-aware probe runs.
	waitForOK(t, http.DefaultClient, gwBase+"/healthz")
	waitForOK(t, http.DefaultClient, gwBase+"/readyz")

	// The gRPC listener should be accepting connections on its ephemeral port.
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", grpcPort), 2*time.Second)
	if err != nil {
		t.Fatalf("gRPC port not accepting connections: %v", err)
	}
	_ = conn.Close()

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

// TestRun_NoGateway covers the gateway-disabled branch (gateway.port 0): the gRPC server still
// serves and run still shuts down cleanly.
func TestRun_NoGateway(t *testing.T) {
	grpcPort, _ := baseRunConfig(t)
	viper.Set("gateway.port", 0)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- run(ctx, versionInfo{}) }()

	// Wait for the gRPC listener to accept before shutting down.
	deadline := time.Now().Add(10 * time.Second)

	for {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", grpcPort), 500*time.Millisecond)
		if err == nil {
			_ = conn.Close()

			break
		}

		if time.Now().After(deadline) {
			t.Fatal("gRPC listener never came up")
		}

		time.Sleep(50 * time.Millisecond)
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

// TestRun_UnknownDriver covers run's bootstrap error path: an unrecognised storage.driver returns
// an error rather than starting the server.
func TestRun_UnknownDriver(t *testing.T) {
	baseRunConfig(t)
	viper.Set("storage.driver", "cassandra")

	if err := run(context.Background(), versionInfo{}); err == nil {
		t.Fatal("expected run to fail for an unknown storage.driver")
	}
}

// TestRun_AuthAndHardening covers the auth-enabled path (hmac verifier + gateway auth middleware)
// together with the gRPC hardening options and the gateway request-size cap, all of which are
// gated branches skipped by the default config.
func TestRun_AuthAndHardening(t *testing.T) {
	_, gwBase := baseRunConfig(t)

	viper.Set("auth.method", "hmac")
	viper.Set("auth.signingSecret", "test-secret")
	viper.Set("maxRecvMsgBytes", 8*1024*1024)
	viper.Set("maxConcurrentStreams", 100)
	viper.Set("keepalive.minTimeSeconds", 5)
	viper.Set("keepalive.permitWithoutStream", true)
	viper.Set("gateway.maxRequestBytes", 1024*1024)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- run(ctx, versionInfo{}) }()

	// /healthz stays open even with auth enabled, so it still proves the listener is up.
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

// TestRun_TLS covers the TLS-enabled branches: the gRPC server gets TLS credentials and the gateway
// serves HTTPS. The self-signed cert has no matching SAN, so the probe client skips verification.
func TestRun_TLS(t *testing.T) {
	_, gwBase := baseRunConfig(t)

	cert, key := writeSelfSignedCert(t)
	viper.Set("tls.enabled", true)
	viper.Set("tls.certFile", cert)
	viper.Set("tls.keyFile", key)

	client := &http.Client{
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
	}

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- run(ctx, versionInfo{}) }()

	httpsBase := strings.Replace(gwBase, "http://", "https://", 1)
	waitForOK(t, client, httpsBase+"/healthz")

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

// TestRun_ListenErrorReturned covers the serve-error path: with the gRPC port already occupied, the
// listener goroutine reports the bind failure through serveErr and run returns it after shutting
// down, rather than blocking forever.
func TestRun_ListenErrorReturned(t *testing.T) {
	baseRunConfig(t)

	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("occupy port: %v", err)
	}
	defer func() { _ = occupied.Close() }()

	viper.Set("port", occupied.Addr().(*net.TCPAddr).Port)
	viper.Set("gateway.port", 0)

	if err := run(context.Background(), versionInfo{}); err == nil {
		t.Fatal("expected run to return the gRPC listen error")
	}
}
