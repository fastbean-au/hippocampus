package client

import (
	"testing"

	"github.com/spf13/pflag"
)

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
