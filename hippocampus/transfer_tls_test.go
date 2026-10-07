package hippocampus

import (
	"testing"
)

// TestTransferClientCredentials covers the credential building: plaintext when disabled, TLS when
// enabled, and the two validation failures the trust options add.
func TestTransferClientCredentials(t *testing.T) {
	t.Parallel()

	insecureCreds, err := Transfer{tls: false}.clientCredentials()
	if err != nil {
		t.Fatalf("disabled: unexpected error: %s", err)
	}

	if proto := insecureCreds.Info().SecurityProtocol; proto != "insecure" {
		t.Errorf("disabled: expected insecure credentials, got %q", proto)
	}

	tlsCreds, err := Transfer{tls: true}.clientCredentials()
	if err != nil {
		t.Fatalf("enabled: unexpected error: %s", err)
	}

	if proto := tlsCreds.Info().SecurityProtocol; proto != "tls" {
		t.Errorf("enabled: expected tls credentials, got %q", proto)
	}

	if _, err := (Transfer{tls: true, tlsCertFile: "cert.pem"}).clientCredentials(); err == nil {
		t.Error("half-configured client certificate pair: expected an error, got nil")
	}

	if _, err := (Transfer{tls: true, tlsCACertFile: "/nonexistent/ca.pem"}).clientCredentials(); err == nil {
		t.Error("unreadable CA bundle: expected an error, got nil")
	}
}
