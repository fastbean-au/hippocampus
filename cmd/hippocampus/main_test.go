package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/viper"

	"github.com/fastbean-au/hippocampus/auth"
)

// TestNewGatewayServer_HardeningTimeouts is a regression test: the gateway HTTP
// server must bound slow-header and idle-keepalive clients (ReadHeaderTimeout / IdleTimeout) while
// leaving WriteTimeout unset so long Export/Import/Transfer responses are not aborted.
func TestNewGatewayServer_HardeningTimeouts(t *testing.T) {
	s := newGatewayServer(8080, http.NotFoundHandler())

	if s.ReadHeaderTimeout <= 0 {
		t.Error("gateway server must set a positive ReadHeaderTimeout (slowloris protection)")
	}

	if s.IdleTimeout <= 0 {
		t.Error("gateway server must set a positive IdleTimeout")
	}

	if s.WriteTimeout != 0 {
		t.Errorf("gateway server must leave WriteTimeout unset for long streaming responses, got %s", s.WriteTimeout)
	}

	if s.Addr != ":8080" {
		t.Errorf("expected addr ':8080', got %q", s.Addr)
	}

	if s.Handler == nil {
		t.Error("gateway server must carry the provided handler")
	}
}

// writeSelfSignedCert writes a throwaway self-signed certificate/key pair to two files in a temp
// directory and returns their paths, so loadServerTLS can be exercised without a real certificate.
func writeSelfSignedCert(t *testing.T) (string, string) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %s", err)
	}

	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "hippocampus-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}

	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("CreateCertificate: %s", err)
	}

	dir := t.TempDir()
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		t.Fatalf("write cert: %s", err)
	}

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("MarshalECPrivateKey: %s", err)
	}

	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatalf("write key: %s", err)
	}

	return certPath, keyPath
}

// TestLoadServerTLS_PinsMinVersion is a regression test: the shared TLS config must pin a TLS 1.2
// floor (so a future change to Go's default can't quietly admit a weaker protocol) and carry the
// loaded certificate.
func TestLoadServerTLS_PinsMinVersion(t *testing.T) {
	certPath, keyPath := writeSelfSignedCert(t)

	cfg, err := loadServerTLS(certPath, keyPath)
	if err != nil {
		t.Fatalf("loadServerTLS: %s", err)
	}

	if cfg.MinVersion != tls.VersionTLS12 {
		t.Errorf("expected MinVersion TLS 1.2 (%d), got %d", tls.VersionTLS12, cfg.MinVersion)
	}

	if len(cfg.Certificates) != 1 {
		t.Errorf("expected the loaded certificate to be present, got %d certificates", len(cfg.Certificates))
	}
}

// TestLoadServerTLS_BadPairFails confirms a missing or unreadable certificate/key pair fails fast
// rather than returning a usable-looking config.
func TestLoadServerTLS_BadPairFails(t *testing.T) {
	if _, err := loadServerTLS("/nonexistent/cert.pem", "/nonexistent/key.pem"); err == nil {
		t.Error("expected loadServerTLS to fail on a missing certificate/key pair")
	}
}

// TestMaxRequestBytesMiddleware verifies the gateway body cap: a body within the limit reaches the
// handler intact, while a body over the limit makes the handler's read fail (the transport-level
// protection against an oversized body being buffered whole).
func TestMaxRequestBytesMiddleware(t *testing.T) {
	const limit = 16

	var readErr error

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, readErr = io.ReadAll(r.Body)
	})

	handler := maxRequestBytesMiddleware(next, limit)

	// Within the limit: the read succeeds.
	readErr = nil
	req := httptest.NewRequest(http.MethodPost, "/v1/memories", strings.NewReader(strings.Repeat("a", limit)))
	handler.ServeHTTP(httptest.NewRecorder(), req)

	if readErr != nil {
		t.Errorf("expected a body within the limit to read cleanly, got: %s", readErr)
	}

	// Over the limit: the read fails.
	readErr = nil
	req = httptest.NewRequest(http.MethodPost, "/v1/memories", strings.NewReader(strings.Repeat("a", limit+1)))
	handler.ServeHTTP(httptest.NewRecorder(), req)

	if readErr == nil {
		t.Error("expected a body over the limit to fail the handler's read")
	}
}

// validConsolidationConfig sets every key validateConfig checks to a known-good value, so each
// test case can flip a single key to the value under test and know that value is what fails (or
// passes) the validation.
func validConsolidationConfig() {
	viper.Reset()
	viper.Set("consolidation.unitsOfAgeInDays", 1.0)
	viper.Set("consolidation.method", 1)
	viper.Set("consolidation.aggressiveness", 1.0)
	viper.Set("sleep.periodSeconds", 60)
	viper.Set("storage.driver", "sqlite")
	viper.Set("storage.directory", "/var/lib/hippocampus")
}

// TestValidateConfigRejectsMissingUnitsOfAgeInDays is a regression test: when
// consolidation.unitsOfAgeInDays is absent, viper returns 0, the age term becomes +Inf, and the
// first sleep cycle deletes every memory and event past the minimum age. Startup must reject it
// rather than run.
func TestValidateConfigRejectsMissingUnitsOfAgeInDays(t *testing.T) {
	validConsolidationConfig()
	viper.Set("consolidation.unitsOfAgeInDays", 0.0)

	if err := validateConfig(); err == nil {
		t.Fatal("validateConfig accepted consolidation.unitsOfAgeInDays of 0; expected rejection")
	}
}

// TestValidateConfigEmptyDirectoryPostgres confirms the empty-directory guard is scoped to the
// sqlite driver: postgres (and mysql) address their store via a DSN, not storage.directory, so an
// empty directory must not fail their validation.
func TestValidateConfigEmptyDirectoryPostgres(t *testing.T) {
	validConsolidationConfig()
	viper.Set("storage.driver", "postgres")
	viper.Set("storage.directory", "")

	if err := validateConfig(); err != nil {
		t.Fatalf("validateConfig rejected an empty storage.directory for the postgres driver: %v", err)
	}
}

func TestValidateConfig(t *testing.T) {
	cases := []struct {
		name    string
		key     string
		value   interface{}
		wantErr bool
	}{
		{name: "valid baseline", key: "", value: nil, wantErr: false},

		{name: "unitsOfAgeInDays zero", key: "consolidation.unitsOfAgeInDays", value: 0.0, wantErr: true},
		{name: "unitsOfAgeInDays negative", key: "consolidation.unitsOfAgeInDays", value: -1.0, wantErr: true},

		{name: "method zero", key: "consolidation.method", value: 0, wantErr: true},
		{name: "method too high", key: "consolidation.method", value: 7, wantErr: true},
		{name: "method lower bound", key: "consolidation.method", value: 1, wantErr: false},
		{name: "method upper bound", key: "consolidation.method", value: 6, wantErr: false},

		{name: "aggressiveness zero", key: "consolidation.aggressiveness", value: 0.0, wantErr: true},
		{name: "aggressiveness negative", key: "consolidation.aggressiveness", value: -0.5, wantErr: true},

		// A non-positive sleep.periodSeconds is a supported "disable automatic sleep" mode, not a
		// config error - autoSleep drops the timed case.
		{name: "periodSeconds zero", key: "sleep.periodSeconds", value: 0, wantErr: false},
		{name: "periodSeconds negative", key: "sleep.periodSeconds", value: -5, wantErr: false},

		{name: "sqlite empty directory", key: "storage.directory", value: "", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			validConsolidationConfig()

			if tc.key != "" {
				viper.Set(tc.key, tc.value)
			}

			err := validateConfig()

			if tc.wantErr && err == nil {
				t.Fatalf("validateConfig(%s=%v) = nil, want error", tc.key, tc.value)
			}

			if !tc.wantErr && err != nil {
				t.Fatalf("validateConfig(%s=%v) = %v, want nil", tc.key, tc.value, err)
			}
		})
	}
}

// TestConfigureEnvOverrides verifies a config key resolves from its HIPPOCAMPUS_-prefixed
// environment variable, so secrets can be injected as env vars instead of living in config.json.
func TestConfigureEnvOverrides(t *testing.T) {
	viper.Reset()
	defer viper.Reset()

	t.Setenv("HIPPOCAMPUS_AUTH_SIGNINGSECRET", "from-env")

	configureEnvOverrides()

	if got := viper.GetString("auth.signingSecret"); got != "from-env" {
		t.Errorf("expected auth.signingSecret to resolve from the environment, got %q", got)
	}
}

// TestConfigureEnvOverrides_EnvBeatsFile verifies an environment variable overrides a value set in
// the config file - the precedence the container secret pattern relies on.
func TestConfigureEnvOverrides_EnvBeatsFile(t *testing.T) {
	viper.Reset()
	defer viper.Reset()

	viper.SetConfigType("json")

	if err := viper.ReadConfig(strings.NewReader(`{"storage":{"postgres":{"dsn":"from-file"}}}`)); err != nil {
		t.Fatalf("ReadConfig: %s", err)
	}

	t.Setenv("HIPPOCAMPUS_STORAGE_POSTGRES_DSN", "from-env")

	configureEnvOverrides()

	if got := viper.GetString("storage.postgres.dsn"); got != "from-env" {
		t.Errorf("expected the environment to override the config file, got %q", got)
	}
}

// TestHmacConfigFromViper verifies the config's signing secret, keyed rotation keys, and active kid
// are all read into the auth.HMACConfig the running verifier and --mint-token share.
func TestHmacConfigFromViper(t *testing.T) {
	viper.Reset()
	defer viper.Reset()

	viper.Set("auth.signingSecret", "legacy-secret")
	viper.Set("auth.activeKid", "k2")
	viper.Set("auth.signingKeys", []map[string]string{
		{"kid": "k1", "secret": "secret-one"},
		{"kid": "k2", "secret": "secret-two"},
	})

	got := hmacConfigFromViper()

	if got.LegacySecret != "legacy-secret" {
		t.Errorf("LegacySecret = %q, want %q", got.LegacySecret, "legacy-secret")
	}

	if got.ActiveKid != "k2" {
		t.Errorf("ActiveKid = %q, want %q", got.ActiveKid, "k2")
	}

	want := []auth.SigningKey{{Kid: "k1", Secret: "secret-one"}, {Kid: "k2", Secret: "secret-two"}}
	if len(got.Keys) != len(want) {
		t.Fatalf("Keys = %+v, want %+v", got.Keys, want)
	}

	for i, v := range want {
		if got.Keys[i] != v {
			t.Errorf("Keys[%d] = %+v, want %+v", i, got.Keys[i], v)
		}
	}
}

// TestHmacConfigFromViper_Empty verifies an unconfigured auth section yields a zero-value
// HMACConfig rather than erroring, since auth is optional.
func TestHmacConfigFromViper_Empty(t *testing.T) {
	viper.Reset()
	defer viper.Reset()

	got := hmacConfigFromViper()

	if got.LegacySecret != "" || got.ActiveKid != "" || len(got.Keys) != 0 {
		t.Errorf("expected a zero-value HMACConfig, got %+v", got)
	}
}

// TestResolveMintKey covers --mint-token's key-selection precedence: an explicit --signing-secret
// override, an explicit --kid, falling back to auth.activeKid, falling back to the first configured
// key, falling back to the legacy secret with no keys at all, and an unknown --kid failing fast with
// an empty secret rather than silently signing with the wrong key.
func TestResolveMintKey(t *testing.T) {
	cfg := auth.HMACConfig{
		LegacySecret: "legacy-secret",
		ActiveKid:    "k2",
		Keys: []auth.SigningKey{
			{Kid: "k1", Secret: "secret-one"},
			{Kid: "k2", Secret: "secret-two"},
		},
	}

	tests := []struct {
		name       string
		override   string
		kid        string
		cfg        auth.HMACConfig
		wantSecret string
		wantKid    string
	}{
		{
			name:       "signing-secret override wins, kid-less without --kid",
			override:   "cli-secret",
			cfg:        cfg,
			wantSecret: "cli-secret",
			wantKid:    "",
		},
		{
			name:       "signing-secret override with explicit kid",
			override:   "cli-secret",
			kid:        "k1",
			cfg:        cfg,
			wantSecret: "cli-secret",
			wantKid:    "k1",
		},
		{
			name:       "explicit kid selects matching key",
			kid:        "k1",
			cfg:        cfg,
			wantSecret: "secret-one",
			wantKid:    "k1",
		},
		{
			name:       "no kid falls back to active kid",
			cfg:        cfg,
			wantSecret: "secret-two",
			wantKid:    "k2",
		},
		{
			name: "no kid, no active kid falls back to first key",
			cfg: auth.HMACConfig{
				LegacySecret: "legacy-secret",
				Keys:         []auth.SigningKey{{Kid: "k1", Secret: "secret-one"}, {Kid: "k2", Secret: "secret-two"}},
			},
			wantSecret: "secret-one",
			wantKid:    "k1",
		},
		{
			name:       "no keys at all falls back to legacy secret with no kid",
			cfg:        auth.HMACConfig{LegacySecret: "legacy-secret"},
			wantSecret: "legacy-secret",
			wantKid:    "",
		},
		{
			name:       "unknown kid fails fast with an empty secret",
			kid:        "does-not-exist",
			cfg:        cfg,
			wantSecret: "",
			wantKid:    "does-not-exist",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			viper.Reset()
			defer viper.Reset()

			viper.Set("signing-secret", tt.override)
			viper.Set("kid", tt.kid)

			secret, kid := resolveMintKey(tt.cfg)
			if secret != tt.wantSecret || kid != tt.wantKid {
				t.Errorf("resolveMintKey() = (%q, %q), want (%q, %q)", secret, kid, tt.wantSecret, tt.wantKid)
			}
		})
	}
}
