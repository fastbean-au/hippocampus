package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/spf13/pflag"
	"github.com/spf13/viper"
)

// resetViper clears and restores the global viper between tests, matching the pattern the main
// package's own run tests use. run() and the credential/transport helpers read straight from viper.
func resetViper(t *testing.T) {
	t.Helper()

	viper.Reset()
	t.Cleanup(viper.Reset)
}

// The connection is the shared dial package's (TODO-3 item 172), which tests the TLS block and the
// token itself. What is pinned here is this command's half: the flags reach it, and a bad TLS block
// still fails before anything is served.

func TestDialConfig_MapsTheFlags(t *testing.T) {
	resetViper(t)
	viper.Set("address", "svc:1")
	viper.Set("token", "secret-token")
	viper.Set("tls", true)
	viper.Set("tls-ca-cert", "/ca.pem")
	viper.Set("tls-cert", "/c.pem")
	viper.Set("tls-key", "/k.pem")
	viper.Set("tls-insecure-skip-verify", true)

	cfg := dialConfig()

	if cfg.Address != "svc:1" || cfg.Token != "secret-token" || !cfg.TLS || cfg.TLSCACertFile != "/ca.pem" ||
		cfg.TLSCertFile != "/c.pem" || cfg.TLSKeyFile != "/k.pem" || !cfg.TLSInsecureSkipVerify {
		t.Errorf("dialConfig = %+v, want every connection flag carried", cfg)
	}

	if cfg.ClientVersion != "hippocampus-mcp/"+version {
		t.Errorf("client version = %q", cfg.ClientVersion)
	}
}

func TestDialConfig_PlaintextByDefault(t *testing.T) {
	resetViper(t)

	if dialConfig().TLS {
		t.Error("TLS is on with no flag asking for it")
	}
}

func TestRun_BadTLSBlockFailsBeforeServing(t *testing.T) {
	cases := map[string]func(t *testing.T){
		"half a client certificate": func(t *testing.T) { viper.Set("tls-cert", "/only/a/cert") },
		"an unreadable CA file": func(t *testing.T) {
			viper.Set("tls-ca-cert", filepath.Join(t.TempDir(), "does-not-exist.pem"))
		},
		"a CA file with no certificates": func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "not-a-cert.pem")
			if err := os.WriteFile(path, []byte("this is not a certificate"), 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}

			viper.Set("tls-ca-cert", path)
		},
	}

	for name, configure := range cases {
		t.Run(name, func(t *testing.T) {
			resetViper(t)
			viper.Set("address", "localhost:50051")
			viper.Set("tls", true)
			configure(t)

			if err := run(context.Background()); err == nil {
				t.Fatal("run served with a broken TLS block")
			}
		})
	}
}

func TestServe_VersionShortCircuits(t *testing.T) {
	resetViper(t)
	viper.Set("version", true)

	// With --version set, serve must not touch run (it would try to dial); it prints and returns.
	if err := serve(context.Background()); err != nil {
		t.Fatalf("serve --version returned error: %v", err)
	}
}

func TestServe_InvalidLogLevelFails(t *testing.T) {
	resetViper(t)
	viper.Set("log-level", "not-a-level")

	if err := serve(context.Background()); err == nil {
		t.Fatal("expected serve to fail on an invalid log level")
	}
}

func TestServe_RunsThroughToTransport(t *testing.T) {
	resetViper(t)
	viper.Set("log-level", "info")
	viper.Set("address", "localhost:50051")
	viper.Set("transport", "http")
	viper.Set("http-address", "127.0.0.1:0")

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)

	go func() {
		done <- serve(ctx)
	}()

	time.Sleep(200 * time.Millisecond)
	cancel()

	select {

	case err := <-done:
		if err != nil {
			t.Fatalf("serve returned error on clean shutdown: %v", err)
		}

	case <-time.After(5 * time.Second):
		t.Fatal("serve did not return after context cancellation")
	}
}

func TestRun_UnknownTransportFails(t *testing.T) {
	resetViper(t)
	viper.Set("address", "localhost:50051")
	viper.Set("transport", "carrier-pigeon")

	if err := run(context.Background()); err == nil {
		t.Fatal("expected an error for an unknown transport")
	}
}

func TestRun_BadTLSCredentialsFail(t *testing.T) {
	resetViper(t)
	viper.Set("address", "localhost:50051")
	viper.Set("tls", true)
	viper.Set("tls-cert", "/only/a/cert")

	if err := run(context.Background()); err == nil {
		t.Fatal("expected run to fail when credentials cannot be built")
	}
}

func TestRun_HTTPTransportServesAndShutsDown(t *testing.T) {
	resetViper(t)
	viper.Set("address", "localhost:50051")
	viper.Set("token", "a-token") // exercises the interceptor-append branch
	viper.Set("transport", "http")
	viper.Set("http-address", "127.0.0.1:0")
	viper.Set("call-timeout-seconds", 5)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)

	go func() {
		done <- run(ctx)
	}()

	// Give the listener a moment to come up, then cancel so serveHTTP's ctx.Done branch runs the
	// graceful shutdown.
	time.Sleep(200 * time.Millisecond)
	cancel()

	select {

	case err := <-done:
		if err != nil {
			t.Fatalf("run returned error on clean shutdown: %v", err)
		}

	case <-time.After(5 * time.Second):
		t.Fatal("run did not return after context cancellation")
	}
}

func TestRegisterFlags_BindsOntoViper(t *testing.T) {
	resetViper(t)

	fs := pflag.NewFlagSet("test", pflag.ContinueOnError)

	if err := registerFlags(fs, []string{"--address", "svc:1234", "--transport", "http", "--tls"}); err != nil {
		t.Fatalf("registerFlags returned error: %v", err)
	}

	if got := viper.GetString("address"); got != "svc:1234" {
		t.Errorf("address = %q, want svc:1234", got)
	}

	if got := viper.GetString("transport"); got != "http" {
		t.Errorf("transport = %q, want http", got)
	}

	if !viper.GetBool("tls") {
		t.Error("tls should be true")
	}

	// A default that was not overridden should still be readable through viper.
	if got := viper.GetInt("call-timeout-seconds"); got != 30 {
		t.Errorf("call-timeout-seconds default = %d, want 30", got)
	}
}

func TestRegisterFlags_ParseErrorReturns(t *testing.T) {
	resetViper(t)

	fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
	fs.SetOutput(io.Discard)

	if err := registerFlags(fs, []string{"--not-a-flag"}); err == nil {
		t.Fatal("expected a parse error for an unknown flag")
	}
}

func TestRun_StdioTransportReturnsOnClosedStdin(t *testing.T) {
	resetViper(t)
	viper.Set("address", "localhost:50051")
	viper.Set("transport", "stdio")

	// The SDK's StdioTransport reads the os.Stdin variable at connect time. Point it at the read end
	// of a pipe whose write end is already closed, so the server sees EOF immediately and run's
	// stdio branch returns instead of blocking on a real terminal.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}

	_ = w.Close()

	oldStdin := os.Stdin
	os.Stdin = r

	t.Cleanup(func() {
		os.Stdin = oldStdin
		_ = r.Close()
	})

	done := make(chan struct{})

	go func() {
		// Either a nil (clean) or non-nil (EOF) return is acceptable; the point is that the stdio
		// branch runs and returns rather than hanging.
		_ = run(context.Background())
		close(done)
	}()

	select {

	case <-done:

	case <-time.After(5 * time.Second):
		t.Fatal("run did not return over the stdio transport with a closed stdin")
	}
}

func TestServeHTTP_BindErrorReturns(t *testing.T) {
	server := newServer(newBridge(&fakeClient{}), "test")

	// Port 99999 is out of range, so ListenAndServe fails immediately and serveHTTP returns via its
	// serveErr branch rather than blocking on ctx.
	if err := serveHTTP(context.Background(), server, httpConfig{address: "127.0.0.1:99999"}); err == nil {
		t.Fatal("expected serveHTTP to return the listener bind error")
	}
}
