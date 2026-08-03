package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/credentials/insecure"

	"github.com/fastbean-au/hippocampus/contract"
)

// writeSelfSignedCert generates a throwaway ECDSA self-signed certificate and writes the cert and
// key PEM files, returning their paths. It lets the TLS config paths be exercised without external
// fixtures.
func writeSelfSignedCert(t *testing.T) (certPath string, keyPath string) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "hippo-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}

	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	certPath = filepath.Join(dir, "cert.pem")
	keyPath = filepath.Join(dir, "key.pem")

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}

	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}

	return certPath, keyPath
}

func TestTLSClientConfigFull(t *testing.T) {
	certPath, keyPath := writeSelfSignedCert(t)

	conf, err := tlsClientConfig(TLSConfig{
		Enabled:            true,
		CACert:             certPath,
		Cert:               certPath,
		Key:                keyPath,
		InsecureSkipVerify: true,
	})
	if err != nil {
		t.Fatalf("tlsClientConfig: %v", err)
	}

	if conf.RootCAs == nil {
		t.Fatal("RootCAs should have been populated from the CA file")
	}

	if len(conf.Certificates) != 1 {
		t.Fatalf("expected one client certificate, got %d", len(conf.Certificates))
	}

	if !conf.InsecureSkipVerify {
		t.Fatal("InsecureSkipVerify should be set")
	}
}

func TestTLSClientConfigBadCA(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.pem")

	if err := os.WriteFile(bad, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := tlsClientConfig(TLSConfig{Enabled: true, CACert: bad})
	if err == nil || !strings.Contains(err.Error(), "no valid certificates") {
		t.Fatalf("err = %v", err)
	}
}

func TestTLSClientConfigMissingCA(t *testing.T) {
	_, err := tlsClientConfig(TLSConfig{Enabled: true, CACert: filepath.Join(t.TempDir(), "absent.pem")})
	if err == nil || !strings.Contains(err.Error(), "reading CA cert file") {
		t.Fatalf("err = %v", err)
	}
}

func TestTransportCredentialsPlaintext(t *testing.T) {
	creds, err := transportCredentials(TLSConfig{Enabled: false})
	if err != nil {
		t.Fatalf("transportCredentials: %v", err)
	}

	if creds.Info().SecurityProtocol != insecure.NewCredentials().Info().SecurityProtocol {
		t.Fatal("disabled TLS should yield insecure credentials")
	}
}

func TestTransportCredentialsTLS(t *testing.T) {
	if _, err := transportCredentials(TLSConfig{Enabled: true}); err != nil {
		t.Fatalf("transportCredentials with TLS: %v", err)
	}
}

func TestHTTPTransportTLS(t *testing.T) {
	if _, err := httpTransport(TLSConfig{Enabled: true}); err != nil {
		t.Fatalf("httpTransport with TLS: %v", err)
	}

	if _, err := httpTransport(TLSConfig{Enabled: true, Cert: "only.pem"}); err == nil {
		t.Fatal("expected an error when only a cert is supplied")
	}
}

// TestHandlerPropagatesClientError confirms handlers surface an RPC error rather than swallow it.
func TestHandlerPropagatesClientError(t *testing.T) {
	wantErr := errors.New("boom")

	for _, key := range []string{"whoami", "sleep", "memory list"} {
		_, _, err := runCommand(t, key, nil, &fakeClient{err: wantErr})
		if !errors.Is(err, wantErr) {
			t.Errorf("%s: err = %v, want boom", key, err)
		}
	}
}

func TestRenderEventFullyPopulated(t *testing.T) {
	var buf bytes.Buffer

	r := &renderer{out: &buf}

	resp := &contract.GetEventResponse{
		Event: &contract.Event{
			Id:           "e1",
			Name:         "deploy",
			Description:  "a release",
			Significance: 9,
			TimeStart:    time.Now().UnixNano(),
			TimeEnd:      time.Now().Add(time.Hour).UnixNano(),
			Group:        "svc",
			Memories: []*contract.Memory{
				{Id: "m1", Body: "note", EventId: "e1", Group: "svc", RecallCount: 3, TimeRecalled: time.Now().UnixNano()},
			},
		},
	}

	if err := r.render(resp); err != nil {
		t.Fatalf("render: %v", err)
	}

	out := buf.String()

	for _, want := range []string{"a release", "time_end:", "group:        svc", "recall_count: 3", "note"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q: %q", want, out)
		}
	}
}

func TestRenderNilEvent(t *testing.T) {
	var buf bytes.Buffer

	r := &renderer{out: &buf}

	if err := r.render(&contract.GetEventResponse{}); err != nil {
		t.Fatalf("render: %v", err)
	}

	if !strings.Contains(buf.String(), "(no event)") {
		t.Fatalf("output = %q", buf.String())
	}
}

// TestRenderCandidatesAndData covers the remaining bespoke text branches.
func TestRenderCandidatesAndData(t *testing.T) {
	var buf bytes.Buffer

	r := &renderer{out: &buf}

	_ = r.render(&contract.GetSummarisationCandidatesResponse{
		Candidates: []*contract.SummarisationCandidate{{EventId: "e1", EventName: "ev", MemoryCount: 12}},
	})
	_ = r.render(&contract.ImportBatchResponse{EventsImported: 2, MemoriesImported: 5})
	_ = r.render(&contract.TransferResponse{ManifestId: "m", MemoriesTransferred: 7})
	_ = r.render(&contract.ClearResponse{MemoriesCleared: 4})
	_ = r.render(&contract.SummariseMemoriesResponse{Id: "s", MemoriesReplaced: 3, Summary: "digest"})

	out := buf.String()

	for _, want := range []string{"ev", "memories_imported: 5", "memories_transferred: 7", "memories_cleared: 4", "digest"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q: %q", want, out)
		}
	}
}

func TestRenderStdinBody(t *testing.T) {
	// Exercise readFileOrStdin's '-' branch by swapping os.Stdin for a pipe.
	old := os.Stdin

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}

	os.Stdin = r

	defer func() { os.Stdin = old }()

	go func() {
		_, _ = w.WriteString("piped body")
		_ = w.Close()
	}()

	data, err := readFileOrStdin("-")
	if err != nil {
		t.Fatalf("readFileOrStdin('-'): %v", err)
	}

	if string(data) != "piped body" {
		t.Fatalf("stdin body = %q", string(data))
	}
}
