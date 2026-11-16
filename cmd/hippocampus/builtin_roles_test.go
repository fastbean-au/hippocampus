package main

import (
	"strings"
	"testing"

	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
	"github.com/spf13/viper"
)

// TestWarnOnBuiltinRoles covers the warning given when an idp deployment maps its provider's roles
// but leaves reader, writer and admin granting their tiers too, which on a shared role namespace
// (Keycloak's realm_access.roles) makes any holder of an unrelated "admin" role a Hippocampus admin.
//
// Not parallel: it installs a hook on the global logger.
func TestWarnOnBuiltinRoles(t *testing.T) {
	cases := []struct {
		name         string
		roleMapping  map[string]string
		builtinRoles bool
		want         string
	}{
		{
			name:         "a mapping with the built-ins on warns about each",
			roleMapping:  map[string]string{"hippo-ops": "admin"},
			builtinRoles: true,
			want:         "reader, writer, admin",
		},
		{
			name:         "a built-in the mapping names is not warned about",
			roleMapping:  map[string]string{"hippo-ops": "admin", " Admin ": "reader"},
			builtinRoles: true,
			want:         "reader, writer",
		},
		{
			name:         "a mapping naming every built-in warns about none",
			roleMapping:  map[string]string{"reader": "reader", "writer": "writer", "admin": "reader"},
			builtinRoles: true,
		},
		{
			name:         "no mapping warns about none",
			builtinRoles: true,
		},
		{
			name:        "built-ins off warns about none",
			roleMapping: map[string]string{"hippo-ops": "admin"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			hook := logtest.NewGlobal()
			t.Cleanup(hook.Reset)

			previous := log.GetLevel()
			log.SetLevel(log.WarnLevel)
			t.Cleanup(func() { log.SetLevel(previous) })

			warnOnBuiltinRoles(c.roleMapping, c.builtinRoles)

			var got []string

			for _, entry := range hook.AllEntries() {
				if strings.Contains(entry.Message, "auth.builtinRoles") {
					got = append(got, entry.Message)
				}
			}

			if c.want == "" {
				if len(got) != 0 {
					t.Fatalf("expected no warning, got %q", got)
				}

				return
			}

			if len(got) != 1 {
				t.Fatalf("expected one warning, got %q", got)
			}

			if !strings.Contains(got[0], "role named "+c.want+" is granted") {
				t.Fatalf("expected the warning to name %q, got %q", c.want, got[0])
			}
		})
	}
}

// TestAuthConfigFromViper_BuiltinRoles holds auth.builtinRoles to idp: it defaults on, can be turned
// off there, and is ignored under hmac, whose --mint-token stamps the built-in names.
//
// Not parallel: it sets keys on the global viper instance.
func TestAuthConfigFromViper_BuiltinRoles(t *testing.T) {
	cases := []struct {
		name   string
		method string
		set    *bool
		want   bool
	}{
		{name: "idp defaults on", method: "idp", want: true},
		{name: "idp can turn them off", method: "idp", set: new(false), want: false},
		{name: "hmac ignores the key", method: "hmac", set: new(false), want: true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			viper.Reset()
			t.Cleanup(viper.Reset)

			if c.set != nil {
				viper.Set("auth.builtinRoles", *c.set)
			}

			cfg, err := authConfigFromViper(c.method)
			if err != nil {
				t.Fatalf("authConfigFromViper: %s", err)
			}

			if cfg.BuiltinRoles != c.want {
				t.Fatalf("BuiltinRoles = %t, want %t", cfg.BuiltinRoles, c.want)
			}
		})
	}
}
