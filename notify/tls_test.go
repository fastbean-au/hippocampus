package notify

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeSelfSignedPair generates a throwaway self-signed certificate valid for 127.0.0.1 and writes
// the cert and key PEM files into dir, returning their paths. It exercises the CA-bundle and
// client-certificate paths of TLSConfig.build without needing a fixture file.
func writeSelfSignedPair(t *testing.T, dir string) (string, string) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %s", err.Error())
	}

	template := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "hippocampus-test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:              []string{"localhost"},
		IsCA:                  true,
		BasicConstraintsValid: true,
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

// TestTLSConfigBuildEmptyReturnsNil confirms the default (nothing configured) yields a nil
// *tls.Config, so the client keeps its own default behaviour unchanged.
func TestTLSConfigBuildEmptyReturnsNil(t *testing.T) {
	out, err := TLSConfig{}.build()
	if err != nil {
		t.Fatalf("unexpected error: %s", err.Error())
	}

	if out != nil {
		t.Error("an empty TLS block produced a *tls.Config - the client's default would be replaced")
	}
}

func TestTLSConfigBuild(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := writeSelfSignedPair(t, dir)

	empty := filepath.Join(dir, "empty.pem")
	if err := os.WriteFile(empty, []byte("not a certificate"), 0o600); err != nil {
		t.Fatalf("write empty bundle: %s", err.Error())
	}

	cases := []struct {
		name    string
		config  TLSConfig
		wantErr string
		check   func(*testing.T, *tls.Config)
	}{
		{
			name:   "insecure alone still builds",
			config: TLSConfig{InsecureSkipVerify: true},
			check: func(t *testing.T, c *tls.Config) {
				if !c.InsecureSkipVerify {
					t.Error("insecureSkipVerify was not carried")
				}
			},
		},
		{
			name:   "ca bundle",
			config: TLSConfig{CACertFile: certPath},
			check: func(t *testing.T, c *tls.Config) {
				if c.RootCAs == nil {
					t.Error("the CA bundle produced no root pool")
				}
			},
		},
		{
			name:   "client certificate",
			config: TLSConfig{CertFile: certPath, KeyFile: keyPath},
			check: func(t *testing.T, c *tls.Config) {
				if len(c.Certificates) != 1 {
					t.Errorf("got %d client certificates, want 1", len(c.Certificates))
				}
			},
		},
		{
			name:    "cert without key",
			config:  TLSConfig{CertFile: certPath},
			wantErr: "certFile and keyFile",
		},
		{
			name:    "key without cert",
			config:  TLSConfig{KeyFile: keyPath},
			wantErr: "certFile and keyFile",
		},
		{
			name:    "missing ca file",
			config:  TLSConfig{CACertFile: filepath.Join(dir, "absent.pem")},
			wantErr: "reading callbacks CA cert file",
		},
		{
			name:    "ca file with no certificates",
			config:  TLSConfig{CACertFile: empty},
			wantErr: "no valid certificates",
		},
		{
			name:    "unloadable key pair",
			config:  TLSConfig{CertFile: certPath, KeyFile: empty},
			wantErr: "client certificate",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, err := c.config.build()

			if c.wantErr != "" {
				if err == nil {
					t.Fatalf("expected an error containing %q, got none", c.wantErr)
				}

				if !strings.Contains(err.Error(), c.wantErr) {
					t.Errorf("error was %q, want it to contain %q", err.Error(), c.wantErr)
				}

				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %s", err.Error())
			}

			if out.MinVersion != tls.VersionTLS12 {
				t.Errorf("MinVersion is %d, want TLS 1.2", out.MinVersion)
			}

			c.check(t, out)
		})
	}
}

// TestBuildTransportPrefersTheSuppliedTransport pins that an injected transport wins outright, even
// over a TLS block that would otherwise fail to build. That is what makes the sink fakeable.
func TestBuildTransportPrefersTheSuppliedTransport(t *testing.T) {
	supplied := http.DefaultTransport

	out, err := buildTransport(Config{Transport: supplied, TLS: TLSConfig{CertFile: "only-half"}})
	if err != nil {
		t.Fatalf("unexpected error: %s", err.Error())
	}

	if out == nil {
		t.Fatal("the supplied transport was discarded")
	}
}

// TestBuildTransportReturnsNilWithoutCustomisation confirms the default transport is left alone,
// which is what keeps its pooling and proxy behaviour.
func TestBuildTransportReturnsNilWithoutCustomisation(t *testing.T) {
	out, err := buildTransport(Config{URL: "https://example.com/hook"})
	if err != nil {
		t.Fatalf("unexpected error: %s", err.Error())
	}

	if out != nil {
		t.Error("an empty TLS block replaced the default transport")
	}
}

// TestWebhookTrustsAPrivateCA is the end-to-end half: a TLS receiver whose certificate no system
// pool knows is verified through caCertFile, and refused without it.
func TestWebhookTrustsAPrivateCA(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := writeSelfSignedPair(t, dir)

	pair, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		t.Fatalf("load key pair: %s", err.Error())
	}

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12}
	srv.StartTLS()

	defer srv.Close()

	trusting, err := NewWebhook(Config{URL: srv.URL, TLS: TLSConfig{CACertFile: certPath}})
	if err != nil {
		t.Fatalf("NewWebhook: %s", err.Error())
	}

	if err := trusting.Deliver(context.Background(), sampleDelivery()); err != nil {
		t.Errorf("a receiver signed by the configured CA was refused: %s", err.Error())
	}

	untrusting, err := NewWebhook(Config{URL: srv.URL})
	if err != nil {
		t.Fatalf("NewWebhook: %s", err.Error())
	}

	if err := untrusting.Deliver(context.Background(), sampleDelivery()); err == nil {
		t.Error("a certificate signed by an unknown CA was accepted without caCertFile")
	}

	skipping, err := NewWebhook(Config{URL: srv.URL, TLS: TLSConfig{InsecureSkipVerify: true}})
	if err != nil {
		t.Fatalf("NewWebhook: %s", err.Error())
	}

	if err := skipping.Deliver(context.Background(), sampleDelivery()); err != nil {
		t.Errorf("insecureSkipVerify did not disable verification: %s", err.Error())
	}
}
