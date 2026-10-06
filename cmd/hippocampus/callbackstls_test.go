package main

import (
	"testing"

	"github.com/spf13/viper"
)

// callbacks.tls.enabled was documented and offered by the wizard, and never read (TODO-3 item 163):
// an operator turning the block off with insecureSkipVerify still set kept skipping verification.
func TestCallbacksTLSHonoursEnabled(t *testing.T) {
	cases := []struct {
		name       string
		settings   map[string]any
		wantSkip   bool
		wantCAFile string
	}{
		{
			"explicitly disabled ignores the block",
			map[string]any{"callbacks.tls.enabled": false, "callbacks.tls.insecureSkipVerify": true, "callbacks.tls.caCertFile": "/ca.pem"},
			false,
			"",
		},
		{
			"explicitly enabled applies it",
			map[string]any{"callbacks.tls.enabled": true, "callbacks.tls.insecureSkipVerify": true, "callbacks.tls.caCertFile": "/ca.pem"},
			true,
			"/ca.pem",
		},
		{
			// Every configuration written before the key was read set the options without it, and
			// dropping them would silently stop a private-CA receiver verifying.
			"unset applies the options it carries, as before",
			map[string]any{"callbacks.tls.caCertFile": "/ca.pem"},
			false,
			"/ca.pem",
		},
		{"nothing set is no customisation", map[string]any{}, false, ""},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			viper.Reset()
			t.Cleanup(viper.Reset)

			for k, v := range c.settings {
				viper.Set(k, v)
			}

			got := callbacksTLSFromViper()

			if got.InsecureSkipVerify != c.wantSkip || got.CACertFile != c.wantCAFile {
				t.Errorf("callbacksTLSFromViper() = %+v, want skip %v and CA %q", got, c.wantSkip, c.wantCAFile)
			}
		})
	}
}
