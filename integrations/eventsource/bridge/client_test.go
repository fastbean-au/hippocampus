package bridge

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
	"testing"
	"time"

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
	creds, err := transportCredentials(ClientConfig{TLS: false})
	if err != nil {
		t.Fatalf("transportCredentials: %v", err)
	}

	if creds.Info().SecurityProtocol != "insecure" {
		t.Errorf("security protocol = %q, want insecure", creds.Info().SecurityProtocol)
	}
}

func TestTransportCredentials_TLSWithCAAndMutualCert(t *testing.T) {
	certFile, keyFile := writeSelfSignedCert(t)

	creds, err := transportCredentials(ClientConfig{
		TLS:           true,
		TLSCACertFile: certFile,
		TLSCertFile:   certFile,
		TLSKeyFile:    keyFile,
	})
	if err != nil {
		t.Fatalf("transportCredentials: %v", err)
	}

	if creds.Info().SecurityProtocol != "tls" {
		t.Errorf("security protocol = %q, want tls", creds.Info().SecurityProtocol)
	}
}

func TestTransportCredentials_MismatchedCertKey(t *testing.T) {
	if _, err := transportCredentials(ClientConfig{TLS: true, TLSCertFile: "only-cert"}); err == nil {
		t.Errorf("want error when only the certificate is set")
	}

	if _, err := transportCredentials(ClientConfig{TLS: true, TLSKeyFile: "only-key"}); err == nil {
		t.Errorf("want error when only the key is set")
	}
}

func TestTransportCredentials_MissingCAFile(t *testing.T) {
	if _, err := transportCredentials(ClientConfig{TLS: true, TLSCACertFile: "/no/such/ca.pem"}); err == nil {
		t.Errorf("want error when the CA file cannot be read")
	}
}

func TestTransportCredentials_InvalidCAFile(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.pem")

	if err := os.WriteFile(bad, []byte("not a certificate"), 0o600); err != nil {
		t.Fatalf("writing bad CA: %v", err)
	}

	if _, err := transportCredentials(ClientConfig{TLS: true, TLSCACertFile: bad}); err == nil {
		t.Errorf("want error when the CA file has no valid certificates")
	}
}

func TestTransportCredentials_BadClientCert(t *testing.T) {
	dir := t.TempDir()
	cert := filepath.Join(dir, "c.pem")
	key := filepath.Join(dir, "k.pem")

	_ = os.WriteFile(cert, []byte("nope"), 0o600)
	_ = os.WriteFile(key, []byte("nope"), 0o600)

	if _, err := transportCredentials(ClientConfig{TLS: true, TLSCertFile: cert, TLSKeyFile: key}); err == nil {
		t.Errorf("want error when the client keypair cannot be loaded")
	}
}

func TestDial_PlaintextReturnsClient(t *testing.T) {
	conn, client, err := Dial(ClientConfig{Address: "localhost:50051"})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}

	defer func() { _ = conn.Close() }()

	if conn == nil || client == nil {
		t.Fatalf("Dial returned nil conn/client")
	}
}

func TestDial_WithTokenReturnsClient(t *testing.T) {
	conn, client, err := Dial(ClientConfig{Address: "localhost:50051", Token: "tok"})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}

	defer func() { _ = conn.Close() }()

	if conn == nil || client == nil {
		t.Fatalf("Dial returned nil conn/client")
	}
}

func TestDial_CredentialsError(t *testing.T) {
	if _, _, err := Dial(ClientConfig{Address: "localhost:50051", TLS: true, TLSCACertFile: "/no/such/ca.pem"}); err == nil {
		t.Errorf("Dial should return an error when the credentials cannot be built")
	}
}

func TestBearerTokenInterceptor(t *testing.T) {
	var seen metadata.MD

	invoker := func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, opts ...grpc.CallOption) error {
		seen, _ = metadata.FromOutgoingContext(ctx)

		return nil
	}

	if err := bearerTokenInterceptor("secret")(context.Background(), "/svc/M", nil, nil, nil, invoker); err != nil {
		t.Fatalf("interceptor: %v", err)
	}

	if got := seen.Get("authorization"); len(got) != 1 || got[0] != "Bearer secret" {
		t.Errorf("authorization metadata = %v, want [Bearer secret]", got)
	}
}
