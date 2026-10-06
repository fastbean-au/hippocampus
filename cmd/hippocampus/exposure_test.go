package main

import (
	"strings"
	"testing"
)

// TestUnauthenticatedExposure (TODO-3 item 168): auth.method none on a listener bound beyond
// loopback is the whole store open to whoever can route to it, and nothing at startup said so.
func TestUnauthenticatedExposure(t *testing.T) {
	cases := []struct {
		name        string
		auth        string
		grpcBind    string
		gatewayBind string
		gatewayPort int
		want        []string
	}{
		{"auth on is never a warning", "hmac", "", "", 8080, nil},
		{"both listeners on loopback", "none", "127.0.0.1", "::1", 8080, nil},
		{"localhost counts as loopback", "none", "localhost", "localhost", 8080, nil},
		{"gRPC on every interface", "none", "", "127.0.0.1", 8080, []string{"gRPC"}},
		{"gateway on every interface", "none", "127.0.0.1", "", 8080, []string{"HTTP gateway"}},
		{"a gateway that is off is not exposed", "none", "127.0.0.1", "", 0, nil},
		{"a routable address", "none", "10.0.0.5", "0.0.0.0", 8080, []string{"gRPC", "HTTP gateway"}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := unauthenticatedExposure(c.auth, c.grpcBind, c.gatewayBind, c.gatewayPort)

			if len(c.want) == 0 {
				if got != "" {
					t.Errorf("warned %q, want nothing", got)
				}

				return
			}

			for _, listener := range c.want {
				if !strings.Contains(got, listener) {
					t.Errorf("warning %q does not name the %s listener", got, listener)
				}
			}
		})
	}
}
