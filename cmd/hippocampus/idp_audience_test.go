package main

import (
	"strings"
	"testing"

	"github.com/spf13/viper"

	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

// TestWarnOnUnconstrainedIdP covers the case an idp deployment is most likely to get wrong without
// noticing: verifying a token's signature and nothing else.
//
// Neither claim is checked unless it is configured (auth/jwks.go only appends jwt.WithAudience and
// jwt.WithIssuer for a non-empty value), so a deployment that names only a JWKS URL accepts every
// token that provider signed - which, behind one corporate IdP, is every application's tokens. The
// warning is the only thing that says so; it is asserted here because a warning nobody emits is
// indistinguishable from a warning nobody needed.
func TestWarnOnUnconstrainedIdP(t *testing.T) {
	cases := []struct {
		name     string
		audience string
		issuer   string
		wantAud  bool
		wantIss  bool
	}{
		{
			name:    "neither set warns about both",
			wantAud: true,
			wantIss: true,
		},
		{
			name:     "audience alone still warns about the issuer",
			audience: "https://hippocampus.example/api",
			wantIss:  true,
		},
		{
			name:    "issuer alone still warns about the audience",
			issuer:  "https://idp.example/realms/x",
			wantAud: true,
		},
		{
			name:     "both set warns about neither",
			audience: "https://hippocampus.example/api",
			issuer:   "https://idp.example/realms/x",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			viper.Reset()
			t.Cleanup(viper.Reset)

			viper.Set("auth.audience", c.audience)
			viper.Set("auth.issuer", c.issuer)

			hook := logtest.NewGlobal()
			t.Cleanup(hook.Reset)

			log.SetLevel(log.WarnLevel)

			warnOnUnconstrainedIdP()

			var gotAud, gotIss bool

			for _, entry := range hook.AllEntries() {
				switch {

				case strings.Contains(entry.Message, "auth.audience is not set"):
					gotAud = true

				case strings.Contains(entry.Message, "auth.issuer is not set"):
					gotIss = true

				}
			}

			if gotAud != c.wantAud {
				t.Errorf("audience warning: got %t, want %t", gotAud, c.wantAud)
			}

			if gotIss != c.wantIss {
				t.Errorf("issuer warning: got %t, want %t", gotIss, c.wantIss)
			}
		})
	}
}
