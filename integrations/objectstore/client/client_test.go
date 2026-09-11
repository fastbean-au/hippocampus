package client

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/pflag"
)

func TestDialBuildsAClient(t *testing.T) {
	conn, hippo, err := Dial(Config{Address: "localhost:50051", Token: "sekrit", Endpoint: "hippocampus"})
	if err != nil {
		t.Fatalf("Dial failed: %s", err.Error())
	}

	defer func() { _ = conn.Close() }()

	if hippo == nil {
		t.Error("expected a client")
	}
}

func TestPlaintextIsTheDefault(t *testing.T) {
	creds, err := transportCredentials(Config{})
	if err != nil {
		t.Fatalf("transportCredentials failed: %s", err.Error())
	}

	if got := creds.Info().SecurityProtocol; got != "insecure" {
		t.Errorf("expected insecure credentials, got %q", got)
	}
}

func TestTLSUsesTheSystemPoolByDefault(t *testing.T) {
	creds, err := transportCredentials(Config{TLS: true})
	if err != nil {
		t.Fatalf("transportCredentials failed: %s", err.Error())
	}

	if got := creds.Info().SecurityProtocol; got != "tls" {
		t.Errorf("expected tls credentials, got %q", got)
	}
}

// Half a client certificate is a configuration mistake that would otherwise present as a handshake
// failure against the far end.
func TestMutualTLSNeedsBothHalves(t *testing.T) {
	if _, err := transportCredentials(Config{TLS: true, TLSCertFile: "cert.pem"}); err == nil {
		t.Error("expected a certificate without a key to be refused")
	}

	if _, err := transportCredentials(Config{TLS: true, TLSKeyFile: "key.pem"}); err == nil {
		t.Error("expected a key without a certificate to be refused")
	}
}

func TestABadCABundleIsReported(t *testing.T) {
	if _, err := transportCredentials(Config{TLS: true, TLSCACertFile: filepath.Join(t.TempDir(), "absent.pem")}); err == nil {
		t.Error("expected a missing CA file to be refused")
	}

	path := filepath.Join(t.TempDir(), "empty.pem")

	if err := os.WriteFile(path, []byte("not a certificate"), 0o600); err != nil {
		t.Fatalf("could not write the file: %s", err.Error())
	}

	if _, err := transportCredentials(Config{TLS: true, TLSCACertFile: path}); err == nil {
		t.Error("expected a CA file with no certificates in it to be refused")
	}
}

func TestABadClientCertificateIsReported(t *testing.T) {
	dir := t.TempDir()

	cert := filepath.Join(dir, "cert.pem")
	key := filepath.Join(dir, "key.pem")

	for _, path := range []string{cert, key} {
		if err := os.WriteFile(path, []byte("rubbish"), 0o600); err != nil {
			t.Fatalf("could not write the file: %s", err.Error())
		}
	}

	if _, err := transportCredentials(Config{TLS: true, TLSCertFile: cert, TLSKeyFile: key}); err == nil {
		t.Error("expected an unloadable certificate to be refused")
	}
}

// The two commands read these keys back by name, so the registration has to define every one of
// them - a typo here is a flag that silently reads as its zero value.
func TestCommonFlagsAreRegistered(t *testing.T) {
	fs := pflag.NewFlagSet("test", pflag.ContinueOnError)

	RegisterCommonFlags(fs, 8090)

	expected := []string{
		"address", "token", "tls", "tls-ca-cert", "tls-cert", "tls-key", "tls-insecure-skip-verify",
		"call-timeout-seconds", "bucket", "s3-endpoint", "s3-region", "s3-path-style", "log-level",
		"version", "metrics", "tracing", "tracing-sampling-ratio", "otlp-endpoint", "otlp-insecure",
		"metrics-interval-seconds", "metrics-group", "health-port", "health-bind-address", "prometheus",
	}

	for _, name := range expected {
		if fs.Lookup(name) == nil {
			t.Errorf("expected --%s to be registered", name)
		}
	}

	if got := fs.Lookup("health-port").DefValue; got != "8090" {
		t.Errorf("expected the health port default to be the one passed, got %s", got)
	}
}

// Two processes defaulting to one probe port means whichever starts second fails for a reason that
// has nothing to do with its job.
func TestTheHealthPortDefaultIsPerBinary(t *testing.T) {
	fs := pflag.NewFlagSet("test", pflag.ContinueOnError)

	RegisterCommonFlags(fs, 8091)

	if got := fs.Lookup("health-port").DefValue; got != "8091" {
		t.Errorf("expected 8091, got %s", got)
	}
}

func TestTheBucketFlagSaysItIsRequired(t *testing.T) {
	fs := pflag.NewFlagSet("test", pflag.ContinueOnError)

	RegisterCommonFlags(fs, 8090)

	if usage := fs.Lookup("bucket").Usage; !strings.Contains(usage, "required") {
		t.Errorf("expected the usage to say the bucket is required, got %q", usage)
	}
}
