package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/spf13/pflag"
	"github.com/spf13/viper"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// resetViper clears and restores the global viper between tests, matching the pattern the main
// package's own run tests use. run() and the credential/transport helpers read straight from viper.
func resetViper(t *testing.T) {
	t.Helper()

	viper.Reset()
	t.Cleanup(viper.Reset)
}

func TestTransportCredentials_InsecureByDefault(t *testing.T) {
	resetViper(t)

	creds, err := transportCredentials()
	if err != nil {
		t.Fatalf("transportCredentials returned error: %v", err)
	}

	if got := creds.Info().SecurityProtocol; got != "insecure" {
		t.Fatalf("expected insecure credentials, got %q", got)
	}
}

func TestTransportCredentials_TLSSystemPool(t *testing.T) {
	resetViper(t)
	viper.Set("tls", true)

	creds, err := transportCredentials()
	if err != nil {
		t.Fatalf("transportCredentials returned error: %v", err)
	}

	if got := creds.Info().SecurityProtocol; got != "tls" {
		t.Fatalf("expected tls credentials, got %q", got)
	}
}

func TestTransportCredentials_HalfConfiguredClientCertFails(t *testing.T) {
	resetViper(t)
	viper.Set("tls", true)
	viper.Set("tls-cert", "/only/a/cert")

	if _, err := transportCredentials(); err == nil {
		t.Fatal("expected an error when only --tls-cert is set")
	}
}

func TestTransportCredentials_BadCAFileFails(t *testing.T) {
	resetViper(t)

	path := filepath.Join(t.TempDir(), "not-a-cert.pem")
	if err := os.WriteFile(path, []byte("this is not a certificate"), 0o600); err != nil {
		t.Fatalf("write temp file: %v", err)
	}

	viper.Set("tls", true)
	viper.Set("tls-ca-cert", path)

	if _, err := transportCredentials(); err == nil {
		t.Fatal("expected an error for a CA file with no valid certificates")
	}
}

func TestTransportCredentials_MissingCAFileFails(t *testing.T) {
	resetViper(t)
	viper.Set("tls", true)
	viper.Set("tls-ca-cert", filepath.Join(t.TempDir(), "does-not-exist.pem"))

	if _, err := transportCredentials(); err == nil {
		t.Fatal("expected an error for an unreadable CA file")
	}
}

func TestTransportCredentials_CAAndClientCert(t *testing.T) {
	resetViper(t)

	certPath, keyPath := writeSelfSignedCert(t)

	viper.Set("tls", true)
	viper.Set("tls-ca-cert", certPath)
	viper.Set("tls-cert", certPath)
	viper.Set("tls-key", keyPath)

	creds, err := transportCredentials()
	if err != nil {
		t.Fatalf("transportCredentials returned error: %v", err)
	}

	if got := creds.Info().SecurityProtocol; got != "tls" {
		t.Fatalf("expected tls credentials, got %q", got)
	}
}

func TestTransportCredentials_BadClientCertPairFails(t *testing.T) {
	resetViper(t)

	certPath, _ := writeSelfSignedCert(t)

	// A key file that is not a valid key for the cert makes LoadX509KeyPair fail.
	badKey := filepath.Join(t.TempDir(), "bad.key")
	if err := os.WriteFile(badKey, []byte("-----BEGIN EC PRIVATE KEY-----\nnope\n-----END EC PRIVATE KEY-----\n"), 0o600); err != nil {
		t.Fatalf("write bad key: %v", err)
	}

	viper.Set("tls", true)
	viper.Set("tls-cert", certPath)
	viper.Set("tls-key", badKey)

	if _, err := transportCredentials(); err == nil {
		t.Fatal("expected an error loading a mismatched client certificate")
	}
}

func TestBearerTokenInterceptor_AttachesAuthorization(t *testing.T) {
	interceptor := bearerTokenInterceptor("secret-token")

	var seen metadata.MD

	invoker := func(ctx context.Context, _ string, _, _ any, _ *grpc.ClientConn, _ ...grpc.CallOption) error {
		md, _ := metadata.FromOutgoingContext(ctx)
		seen = md

		return nil
	}

	if err := interceptor(context.Background(), "/proto.Hippocampus/StoreMemory", nil, nil, nil, invoker); err != nil {
		t.Fatalf("interceptor returned error: %v", err)
	}

	got := seen.Get("authorization")
	if len(got) != 1 || got[0] != "Bearer secret-token" {
		t.Fatalf("authorization metadata not set correctly: %v", got)
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
	if err := serveHTTP(context.Background(), server, "127.0.0.1:99999"); err == nil {
		t.Fatal("expected serveHTTP to return the listener bind error")
	}
}

// writeSelfSignedCert writes a self-signed ECDSA certificate and its key to temp files and returns
// their paths, for exercising the TLS trust-option branches of transportCredentials.
func writeSelfSignedCert(t *testing.T) (string, string) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "hippocampus-mcp-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IsCA:         true,
	}

	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}

	dir := t.TempDir()
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")

	certPEM, err := os.Create(certPath)
	if err != nil {
		t.Fatalf("create cert file: %v", err)
	}
	defer func() { _ = certPEM.Close() }()

	if err := pem.Encode(certPEM, &pem.Block{Type: "CERTIFICATE", Bytes: der}); err != nil {
		t.Fatalf("encode cert: %v", err)
	}

	keyBytes, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}

	keyPEM, err := os.Create(keyPath)
	if err != nil {
		t.Fatalf("create key file: %v", err)
	}
	defer func() { _ = keyPEM.Close() }()

	if err := pem.Encode(keyPEM, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes}); err != nil {
		t.Fatalf("encode key: %v", err)
	}

	return certPath, keyPath
}
