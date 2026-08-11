package client

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/pflag"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// writeSelfSignedCert generates a self-signed CA certificate and its key, writing both to temp
// files, so the TLS-building branches can be exercised without external fixtures.
func writeSelfSignedCert(t *testing.T) (certFile string, keyFile string) {
	t.Helper()

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}

	tmpl := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}

	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("creating certificate: %v", err)
	}

	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatalf("marshalling key: %v", err)
	}

	dir := t.TempDir()
	certFile = filepath.Join(dir, "cert.pem")
	keyFile = filepath.Join(dir, "key.pem")

	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatalf("writing cert: %v", err)
	}

	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatalf("writing key: %v", err)
	}

	return certFile, keyFile
}

func TestTransportCredentials_PlaintextWhenTLSOff(t *testing.T) {
	creds, err := transportCredentials(Config{})
	if err != nil {
		t.Fatalf("transportCredentials: %v", err)
	}

	if got := creds.Info().SecurityProtocol; got != "insecure" {
		t.Errorf("expected insecure credentials, got %q", got)
	}
}

func TestTransportCredentials_TLSWithCAAndMutualCert(t *testing.T) {
	certFile, keyFile := writeSelfSignedCert(t)

	creds, err := transportCredentials(Config{
		TLS:           true,
		TLSCACertFile: certFile,
		TLSCertFile:   certFile,
		TLSKeyFile:    keyFile,
	})
	if err != nil {
		t.Fatalf("transportCredentials: %v", err)
	}

	if got := creds.Info().SecurityProtocol; got != "tls" {
		t.Errorf("expected tls credentials, got %q", got)
	}
}

// TestTransportCredentials_Failures covers each way the TLS block can be wrong. All of them fail
// startup rather than quietly falling back to a weaker connection.
func TestTransportCredentials_Failures(t *testing.T) {
	certFile, keyFile := writeSelfSignedCert(t)

	empty := filepath.Join(t.TempDir(), "empty.pem")
	if err := os.WriteFile(empty, []byte("not a certificate"), 0o600); err != nil {
		t.Fatalf("writing the bad CA file: %v", err)
	}

	cases := []struct {
		name string
		cfg  Config
		want string
	}{
		{
			"a certificate with no key",
			Config{TLS: true, TLSCertFile: certFile},
			"both a certificate and a key",
		},
		{
			"a key with no certificate",
			Config{TLS: true, TLSKeyFile: keyFile},
			"both a certificate and a key",
		},
		{
			"an unreadable CA bundle",
			Config{TLS: true, TLSCACertFile: filepath.Join(t.TempDir(), "absent.pem")},
			"reading CA cert file",
		},
		{
			"a CA bundle holding no certificates",
			Config{TLS: true, TLSCACertFile: empty},
			"no valid certificates",
		},
		{
			"a key that does not match the certificate",
			Config{TLS: true, TLSCertFile: certFile, TLSKeyFile: certFile},
			"loading client certificate",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := transportCredentials(c.cfg)
			if err == nil {
				t.Fatal("expected an error")
			}

			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("expected an error mentioning %q, got %q", c.want, err.Error())
			}
		})
	}
}

func TestDial(t *testing.T) {
	conn, client, err := Dial(Config{Address: "localhost:50051"})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}

	defer func() { _ = conn.Close() }()

	if client == nil {
		t.Error("expected a client")
	}

	// With a token, the bearer interceptor is installed; the dial itself is lazy so no server is
	// needed.
	tokenConn, _, err := Dial(Config{Address: "localhost:50051", Token: "t"})
	if err != nil {
		t.Fatalf("Dial with a token: %v", err)
	}

	_ = tokenConn.Close()
}

func TestDial_CredentialsError(t *testing.T) {
	if _, _, err := Dial(Config{TLS: true, TLSCertFile: "only-a-cert"}); err == nil {
		t.Error("expected the credentials failure to surface")
	}
}

func TestBearerTokenInterceptor(t *testing.T) {
	interceptor := bearerTokenInterceptor("secret")

	var seen metadata.MD

	invoker := func(ctx context.Context, _ string, _ any, _ any, _ *grpc.ClientConn, _ ...grpc.CallOption) error {
		seen, _ = metadata.FromOutgoingContext(ctx)

		return nil
	}

	if err := interceptor(context.Background(), "/svc/Method", nil, nil, nil, invoker); err != nil {
		t.Fatalf("interceptor: %v", err)
	}

	if got := seen.Get("authorization"); len(got) != 1 || got[0] != "Bearer secret" {
		t.Errorf("expected a bearer authorization header, got %v", got)
	}
}

// TestRegisterFlagsIsPrefixed pins the property the two endpoints depend on: one registration per
// endpoint, with no shared flag between them. A source and a target sharing a --token would send the
// edge's credentials to the central instance.
func TestRegisterFlagsIsPrefixed(t *testing.T) {
	fs := pflag.NewFlagSet("test", pflag.ContinueOnError)

	RegisterFlags(fs, "source", "localhost:50051", "edge")
	RegisterFlags(fs, "target", "", "central")

	for _, name := range []string{
		"source-address", "source-token", "source-tls", "source-tls-ca-cert", "source-tls-cert",
		"source-tls-key", "source-tls-insecure-skip-verify",
		"target-address", "target-token", "target-tls",
	} {
		if fs.Lookup(name) == nil {
			t.Errorf("expected a --%s flag", name)
		}
	}

	// Nothing unprefixed, so neither endpoint can read the other's value by accident.
	for _, name := range []string{"address", "token", "tls"} {
		if fs.Lookup(name) != nil {
			t.Errorf("did not expect an unprefixed --%s flag", name)
		}
	}

	if got := fs.Lookup("source-address").DefValue; got != "localhost:50051" {
		t.Errorf("expected the source default address, got %q", got)
	}

	if got := fs.Lookup("target-address").DefValue; got != "" {
		t.Errorf("expected the target address to have no default, got %q", got)
	}
}

func TestKey(t *testing.T) {
	if got := Key("source", "tls-ca-cert"); got != "source-tls-ca-cert" {
		t.Errorf("unexpected key %q", got)
	}
}
