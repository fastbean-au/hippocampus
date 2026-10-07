package client

import (
	"strings"
	"testing"

	"github.com/spf13/pflag"
)

func TestDialBuildsAClient(t *testing.T) {
	conn, hippo, err := Dial(Config{Address: "localhost:50051", Token: "sekrit"})
	if err != nil {
		t.Fatalf("Dial failed: %s", err.Error())
	}

	defer func() { _ = conn.Close() }()

	if hippo == nil {
		t.Error("expected a client")
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
