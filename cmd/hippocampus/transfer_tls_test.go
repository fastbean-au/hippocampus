package main

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
