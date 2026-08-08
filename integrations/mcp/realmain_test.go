package main

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/spf13/pflag"
	"github.com/spf13/viper"
)

// resetCommandLine gives each test a fresh global flag set, since realMain registers onto
// pflag.CommandLine.
func resetCommandLine(t *testing.T) {
	t.Helper()

	viper.Reset()
	t.Cleanup(viper.Reset)

	previous := pflag.CommandLine
	pflag.CommandLine = pflag.NewFlagSet("hippocampus-mcp", pflag.ContinueOnError)
	pflag.CommandLine.SetOutput(io.Discard)

	t.Cleanup(func() { pflag.CommandLine = previous })
}

// TestRealMain_VersionReturnsZero covers the success path end to end: --version short-circuits
// before anything is dialled, so it is the one invocation that needs no service at all.
func TestRealMain_VersionReturnsZero(t *testing.T) {
	resetCommandLine(t)

	if code := realMain([]string{"--version"}); code != 0 {
		t.Errorf("realMain(--version) = %d, want 0", code)
	}
}

// TestRealMain_FlagErrorReturnsOne covers the flag-registration failure, which must be a non-zero
// exit rather than a panic - an MCP host reads the exit code.
func TestRealMain_FlagErrorReturnsOne(t *testing.T) {
	resetCommandLine(t)

	if code := realMain([]string{"--not-a-flag"}); code != 1 {
		t.Errorf("realMain(--not-a-flag) = %d, want 1", code)
	}
}

// TestRealMain_ServeErrorReturnsOne covers the serve failure arm.
func TestRealMain_ServeErrorReturnsOne(t *testing.T) {
	resetCommandLine(t)

	if code := realMain([]string{"--transport", "not-a-transport"}); code != 1 {
		t.Errorf("realMain(bad transport) = %d, want 1", code)
	}
}

// TestServeHTTP_HandlesARequest covers the per-request server closure. The bridge is stateless, so
// one server instance backs every session - which is exactly what the closure returning the same
// server expresses, and what this exercises by actually issuing a request.
func TestServeHTTP_HandlesARequest(t *testing.T) {
	resetViper(t)
	viper.Set("address", "localhost:50051")
	viper.Set("transport", "http")
	viper.Set("http-address", "127.0.0.1:38091")
	viper.Set("call-timeout-seconds", 5)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- run(ctx) }()

	endpoint := "http://127.0.0.1:38091"
	waitForListener(t, endpoint)

	// An MCP initialize over the streamable-HTTP transport: enough to route through the handler,
	// which is what the closure under test provides.
	body := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{` +
		`"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`)

	request, err := http.NewRequest(http.MethodPost, endpoint, body)
	if err != nil {
		t.Fatalf("NewRequest: %s", err)
	}

	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")

	resp, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("POST: %s", err)
	}

	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	if resp.StatusCode >= 500 {
		t.Errorf("expected the handler to serve the request, got %d", resp.StatusCode)
	}

	cancel()

	select {

	case err := <-done:
		if err != nil {
			t.Errorf("run: %s", err)
		}

	case <-time.After(10 * time.Second):
		t.Fatal("run did not return after its context was cancelled")

	}
}

// waitForListener polls until the address accepts a connection.
func waitForListener(t *testing.T, endpoint string) {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)

	for time.Now().Before(deadline) {
		resp, err := http.Get(endpoint)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()

			return
		}

		time.Sleep(50 * time.Millisecond)
	}

	t.Fatalf("nothing listening on %s", endpoint)
}
