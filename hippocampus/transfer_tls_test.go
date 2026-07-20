package hippocampus

import (
	"testing"

	"github.com/spf13/viper"
)

// TestTransferTLSEnabled verifies both the legacy scalar form (transfer.tls: true) and the block
// form (transfer.tls.enabled: true) toggle the Transfer client's TLS, so existing configs keep
// working alongside the new trust options.
func TestTransferTLSEnabled(t *testing.T) {
	tests := []struct {
		name string
		set  func()
		want bool
	}{
		{"unset", func() {}, false},
		{"legacy bool true", func() { viper.Set("transfer.tls", true) }, true},
		{"legacy bool false", func() { viper.Set("transfer.tls", false) }, false},
		{"block enabled true", func() { viper.Set("transfer.tls", map[string]any{"enabled": true}) }, true},
		{"block enabled false", func() { viper.Set("transfer.tls", map[string]any{"enabled": false}) }, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			viper.Reset()
			t.Cleanup(viper.Reset)

			tc.set()

			if got := transferTLSEnabled(); got != tc.want {
				t.Errorf("transferTLSEnabled() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestTransferClientCredentials covers the credential building: plaintext when disabled, TLS when
// enabled, and the two validation failures the trust options add.
func TestTransferClientCredentials(t *testing.T) {
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
