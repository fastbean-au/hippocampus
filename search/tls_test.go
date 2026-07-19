package search

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeSelfSignedPair generates a throwaway self-signed certificate and writes the cert and key
// PEM files into dir, returning their paths. Used to exercise the CA-bundle and client-certificate
// paths of TLSConfig.build without needing a real cluster or fixture files.
func writeSelfSignedPair(t *testing.T, dir string) (string, string) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %s", err.Error())
	}

	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "hippocampus-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		IsCA:         true,
	}

	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %s", err.Error())
	}

	certPath := filepath.Join(dir, "cert.pem")
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		t.Fatalf("write cert: %s", err.Error())
	}

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %s", err.Error())
	}

	keyPath := filepath.Join(dir, "key.pem")
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatalf("write key: %s", err.Error())
	}

	return certPath, keyPath
}

// TestTLSConfigBuild_EmptyReturnsNil confirms the default (nothing configured) yields a nil
// *tls.Config, so the client keeps its own default behaviour unchanged.
func TestTLSConfigBuild_EmptyReturnsNil(t *testing.T) {
	out, err := TLSConfig{}.build()
	if err != nil {
		t.Fatalf("unexpected error: %s", err.Error())
	}

	if out != nil {
		t.Fatalf("expected nil tls.Config for empty TLSConfig, got %+v", out)
	}
}

// TestTLSConfigBuild_InsecureSkipVerify confirms the escape hatch produces a config with
// verification disabled even when no CA or client certificate is supplied.
func TestTLSConfigBuild_InsecureSkipVerify(t *testing.T) {
	out, err := TLSConfig{InsecureSkipVerify: true}.build()
	if err != nil {
		t.Fatalf("unexpected error: %s", err.Error())
	}

	if out == nil || !out.InsecureSkipVerify {
		t.Fatalf("expected InsecureSkipVerify to be set, got %+v", out)
	}

	if out.MinVersion != tls.VersionTLS12 {
		t.Fatalf("expected MinVersion TLS 1.2, got %d", out.MinVersion)
	}
}

// TestTLSConfigBuild_CACert confirms a valid CA bundle populates RootCAs.
func TestTLSConfigBuild_CACert(t *testing.T) {
	certPath, _ := writeSelfSignedPair(t, t.TempDir())

	out, err := TLSConfig{CACertFile: certPath}.build()
	if err != nil {
		t.Fatalf("unexpected error: %s", err.Error())
	}

	if out == nil || out.RootCAs == nil {
		t.Fatalf("expected RootCAs to be populated, got %+v", out)
	}
}

// TestTLSConfigBuild_CACertMissingFile confirms an unreadable CA file fails construction rather
// than silently falling back to the system pool.
func TestTLSConfigBuild_CACertMissingFile(t *testing.T) {
	_, err := TLSConfig{CACertFile: filepath.Join(t.TempDir(), "nope.pem")}.build()
	if err == nil {
		t.Fatal("expected an error for a missing CA cert file")
	}
}

// TestTLSConfigBuild_CACertNotPEM confirms a file containing no certificates is rejected.
func TestTLSConfigBuild_CACertNotPEM(t *testing.T) {
	path := filepath.Join(t.TempDir(), "garbage.pem")
	if err := os.WriteFile(path, []byte("not a certificate"), 0o600); err != nil {
		t.Fatalf("write file: %s", err.Error())
	}

	_, err := TLSConfig{CACertFile: path}.build()
	if err == nil {
		t.Fatal("expected an error for a CA file with no certificates")
	}
}

// TestTLSConfigBuild_ClientCert confirms a matching cert/key pair loads into Certificates.
func TestTLSConfigBuild_ClientCert(t *testing.T) {
	certPath, keyPath := writeSelfSignedPair(t, t.TempDir())

	out, err := TLSConfig{CertFile: certPath, KeyFile: keyPath}.build()
	if err != nil {
		t.Fatalf("unexpected error: %s", err.Error())
	}

	if out == nil || len(out.Certificates) != 1 {
		t.Fatalf("expected one client certificate, got %+v", out)
	}
}

// TestTLSConfigBuild_HalfClientCert confirms a cert without its key (or vice versa) is rejected
// rather than silently ignored.
func TestTLSConfigBuild_HalfClientCert(t *testing.T) {
	certPath, keyPath := writeSelfSignedPair(t, t.TempDir())

	if _, err := (TLSConfig{CertFile: certPath}).build(); err == nil {
		t.Fatal("expected an error when only certFile is set")
	}

	if _, err := (TLSConfig{KeyFile: keyPath}).build(); err == nil {
		t.Fatal("expected an error when only keyFile is set")
	}
}

// TestBuildTransport_TestTransportWins confirms a caller-supplied transport (the fake cluster in
// unit tests) takes precedence over the TLS block and is returned untouched.
func TestBuildTransport_TestTransportWins(t *testing.T) {
	sentinel := http.DefaultTransport

	out, err := buildTransport(Config{
		Transport: sentinel,
		TLS:       TLSConfig{InsecureSkipVerify: true},
	})
	if err != nil {
		t.Fatalf("unexpected error: %s", err.Error())
	}

	if out == nil {
		t.Fatal("expected the supplied transport to be returned")
	}
}

// TestBuildTransport_NoTLSReturnsNil confirms that, with no TLS customisation and no test
// transport, the transport is nil so the client keeps its own default.
func TestBuildTransport_NoTLSReturnsNil(t *testing.T) {
	out, err := buildTransport(Config{})
	if err != nil {
		t.Fatalf("unexpected error: %s", err.Error())
	}

	if out != nil {
		t.Fatalf("expected a nil transport with no TLS config, got %T", out)
	}
}

// TestBuildTransport_TLSInstalled confirms a TLS block produces an *http.Transport carrying the
// assembled *tls.Config.
func TestBuildTransport_TLSInstalled(t *testing.T) {
	out, err := buildTransport(Config{TLS: TLSConfig{InsecureSkipVerify: true}})
	if err != nil {
		t.Fatalf("unexpected error: %s", err.Error())
	}

	transport, ok := out.(*http.Transport)
	if !ok {
		t.Fatalf("expected *http.Transport, got %T", out)
	}

	if transport.TLSClientConfig == nil || !transport.TLSClientConfig.InsecureSkipVerify {
		t.Fatalf("expected the tls config to be installed on the transport, got %+v", transport.TLSClientConfig)
	}
}

// writeCert writes a single certificate's PEM to a file in dir and returns its path.
func writeCert(t *testing.T, dir string, cert *x509.Certificate) string {
	t.Helper()

	path := filepath.Join(dir, "server-ca.pem")
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})
	if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
		t.Fatalf("write cert: %s", err.Error())
	}

	return path
}

// TestBuildTransport_TrustsConfiguredCA drives the transport built from caCertFile against a real
// HTTPS server whose certificate is signed by that CA, proving the CA bundle is genuinely honoured
// (not just stored) and that verified TLS round-trips succeed.
func TestBuildTransport_TrustsConfiguredCA(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	caPath := writeCert(t, t.TempDir(), server.Certificate())

	transport, err := buildTransport(Config{TLS: TLSConfig{CACertFile: caPath}})
	if err != nil {
		t.Fatalf("buildTransport: %s", err.Error())
	}

	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}

	resp, err := client.Get(server.URL)
	if err != nil {
		t.Fatalf("expected a verified TLS request to succeed, got: %s", err.Error())
	}

	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
}

// TestBuildTransport_RejectsUntrustedCA proves the CA bundle actually restricts trust: pointed at
// an unrelated CA, the same HTTPS server's certificate must fail verification rather than connect.
func TestBuildTransport_RejectsUntrustedCA(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	// A CA file for a certificate unrelated to the server's.
	unrelatedCA, _ := writeSelfSignedPair(t, t.TempDir())

	transport, err := buildTransport(Config{TLS: TLSConfig{CACertFile: unrelatedCA}})
	if err != nil {
		t.Fatalf("buildTransport: %s", err.Error())
	}

	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}

	resp, err := client.Get(server.URL)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("expected TLS verification to fail against an untrusted CA, but the request succeeded")
	}
}
