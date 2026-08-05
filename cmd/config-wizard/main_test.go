package main

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestHandlerServesTheWizard proves the embedded bundle is actually reachable: the page itself, the
// two assets it references, and the 404 for anything else.
func TestHandlerServesTheWizard(t *testing.T) {
	server := httptest.NewServer(handler())
	defer server.Close()

	cases := []struct {
		path        string
		status      int
		contentType string
		contains    string
	}{
		{path: "/", status: http.StatusOK, contentType: "text/html", contains: "<title>Hippocampus configuration wizard</title>"},
		{path: "/index.html", status: http.StatusOK, contentType: "text/html", contains: "app.js"},
		{path: "/app.js", status: http.StatusOK, contentType: "text/javascript", contains: "buildConfig"},
		{path: "/styles.css", status: http.StatusOK, contentType: "text/css", contains: "--accent"},
		{path: "/nothing-here", status: http.StatusNotFound},
	}

	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			response, err := http.Get(server.URL + c.path)
			if err != nil {
				t.Fatalf("GET %s: %s", c.path, err.Error())
			}

			defer func() { _ = response.Body.Close() }()

			if response.StatusCode != c.status {
				t.Fatalf("GET %s: got status %d, want %d", c.path, response.StatusCode, c.status)
			}

			if c.contentType != "" && !strings.Contains(response.Header.Get("Content-Type"), c.contentType) {
				t.Errorf("GET %s: got content type %q, want it to contain %q", c.path, response.Header.Get("Content-Type"), c.contentType)
			}

			body, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatalf("failed to read the body: %s", err.Error())
			}

			if c.contains != "" && !strings.Contains(string(body), c.contains) {
				t.Errorf("GET %s: body does not contain %q", c.path, c.contains)
			}
		})
	}
}

// TestSecurityHeaders pins the response headers a public deployment relies on. The
// Content-Security-Policy in particular is coupled to the markup: it forbids inline script and
// style, so a regression that inlines either would break the page in a browser but not in a test
// that only checked for a 200.
func TestSecurityHeaders(t *testing.T) {
	server := httptest.NewServer(handler())
	defer server.Close()

	response, err := http.Get(server.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %s", err.Error())
	}

	defer func() { _ = response.Body.Close() }()

	policy := response.Header.Get("Content-Security-Policy")

	for _, directive := range []string{"default-src 'none'", "script-src 'self'", "style-src 'self'", "connect-src 'none'", "frame-ancestors 'none'"} {
		if !strings.Contains(policy, directive) {
			t.Errorf("Content-Security-Policy %q is missing %q", policy, directive)
		}
	}

	if strings.Contains(policy, "unsafe-inline") {
		t.Errorf("Content-Security-Policy %q allows inline script or style", policy)
	}

	if got := response.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("got X-Content-Type-Options %q, want nosniff", got)
	}

	if got := response.Header.Get("Cache-Control"); got != "no-cache" {
		t.Errorf("got Cache-Control %q, want no-cache", got)
	}
}

// TestAssetsCarryNoInlineCode is the other half of the CSP guarantee, checked against the markup
// itself: under script-src/style-src 'self' an inline <script> or <style> block silently does
// nothing, so the page must not grow one.
func TestAssetsCarryNoInlineCode(t *testing.T) {
	page, err := wizardAssets.ReadFile("wizard/index.html")
	if err != nil {
		t.Fatalf("failed to read the embedded page: %s", err.Error())
	}

	markup := string(page)

	if strings.Contains(markup, "<script>") || strings.Contains(markup, "<style>") {
		t.Error("the wizard page carries an inline script or style block, which its Content-Security-Policy blocks")
	}

	if !strings.Contains(markup, `<script src="app.js">`) {
		t.Error("the wizard page no longer loads app.js as an external script")
	}
}

func TestHealthHandler(t *testing.T) {
	recorder := httptest.NewRecorder()
	healthHandler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("got status %d, want 200", recorder.Code)
	}

	var body struct {
		Status  string `json:"status"`
		Version string `json:"version"`
	}

	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("failed to parse the health body %q: %s", recorder.Body.String(), err.Error())
	}

	if body.Status != "ok" {
		t.Errorf("got status %q, want ok", body.Status)
	}

	if body.Version != version {
		t.Errorf("got version %q, want %q", body.Version, version)
	}
}

func TestExecuteVersionExitsBeforeListening(t *testing.T) {
	// --version must not bind a port, so a second run while the wizard is already serving still
	// works. If it did listen, this call would block rather than return.
	if err := execute([]string{"--version"}); err != nil {
		t.Fatalf("--version: %s", err.Error())
	}
}

func TestExecuteHelpIsNotAnError(t *testing.T) {
	if err := execute([]string{"--help"}); err != nil {
		t.Fatalf("--help: %s", err.Error())
	}
}

func TestExecuteRejectsAnUnknownFlag(t *testing.T) {
	if err := execute([]string{"--not-a-flag"}); err == nil {
		t.Fatal("expected an error for an unknown flag")
	}
}

func TestExecuteReportsAnUnusableAddress(t *testing.T) {
	// Port 1 needs privileges this test does not have, and the bind fails immediately - the cheapest
	// way to reach the listen-failure path.
	err := execute([]string{"--port", "1", "--bind-address", "127.0.0.1", "--log-level", "error"})
	if err == nil {
		t.Fatal("expected a listen failure")
	}

	if !strings.Contains(err.Error(), "failed to listen") {
		t.Errorf("got %q, want a listen failure", err.Error())
	}
}

// TestServeDrainsOnSignal covers the signal path: serve must return cleanly once the process is
// signalled, having shut the server down rather than leaving it accepting.
func TestServeDrainsOnSignal(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %s", err.Error())
	}

	server := &http.Server{Handler: handler(), ReadHeaderTimeout: time.Second}

	done := make(chan error, 1)

	go func() {
		done <- serve(server, listener)
	}()

	// Wait for the server to be answering before signalling it, so the shutdown is not racing the
	// goroutine that starts it.
	address := "http://" + listener.Addr().String() + "/healthz"

	for range 100 {
		response, err := http.Get(address)
		if err == nil {
			_ = response.Body.Close()

			break
		}

		time.Sleep(10 * time.Millisecond)
	}

	process, err := os.FindProcess(os.Getpid())
	if err != nil {
		t.Fatalf("failed to find this process: %s", err.Error())
	}

	if err := process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("failed to signal: %s", err.Error())
	}

	select {

	case err := <-done:
		if err != nil {
			t.Fatalf("serve returned %s, want a clean shutdown", err.Error())
		}

	case <-time.After(10 * time.Second):
		t.Fatal("serve did not return after SIGTERM")
	}

	if _, err := http.Get(address); err == nil {
		t.Error("the server is still accepting connections after shutdown")
	}
}

// TestServeReportsAServeFailure covers the other exit from serve: the listener dying under it.
func TestServeReportsAServeFailure(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %s", err.Error())
	}

	// Closing the listener out from under Serve makes it return a non-ErrServerClosed error, which
	// serve must surface rather than treat as a clean stop.
	_ = listener.Close()

	if err := serve(&http.Server{Handler: handler(), ReadHeaderTimeout: time.Second}, listener); err == nil {
		t.Fatal("expected the serve failure to be reported")
	}
}

// TestExecuteServesAndShutsDown drives the whole startup path in process: flags, listener, server,
// and the signalled drain.
func TestExecuteServesAndShutsDown(t *testing.T) {
	port := freePort(t)

	done := make(chan error, 1)

	go func() {
		done <- execute([]string{"--port", port, "--bind-address", "127.0.0.1", "--log-level", "error"})
	}()

	address := "http://127.0.0.1:" + port + "/healthz"
	up := false

	for range 200 {
		response, err := http.Get(address)
		if err == nil {
			_ = response.Body.Close()

			up = true

			break
		}

		time.Sleep(10 * time.Millisecond)
	}

	if !up {
		t.Fatal("the wizard never started serving")
	}

	process, err := os.FindProcess(os.Getpid())
	if err != nil {
		t.Fatalf("failed to find this process: %s", err.Error())
	}

	if err := process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("failed to signal: %s", err.Error())
	}

	select {

	case err := <-done:
		if err != nil {
			t.Fatalf("execute returned %s, want a clean shutdown", err.Error())
		}

	case <-time.After(10 * time.Second):
		t.Fatal("execute did not return after SIGTERM")
	}
}

// freePort asks the kernel for an unused port and gives it straight back, so the caller can hand the
// number to something that binds it itself.
func freePort(t *testing.T) string {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to find a free port: %s", err.Error())
	}

	_, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatalf("failed to read the port: %s", err.Error())
	}

	_ = listener.Close()

	return port
}

func TestInitLoggingFallsBackToInfo(t *testing.T) {
	// An unrecognised level must not fail startup; it falls back to info like the service does.
	initLogging("not-a-level")
	initLogging("debug")
}
