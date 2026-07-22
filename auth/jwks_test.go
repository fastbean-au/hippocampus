package auth

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// testKeySet is a mutable JWKS document served over httptest, so a test can rotate the published
// keys under a live verifier.
type testKeySet struct {
	mu   sync.Mutex
	keys []map[string]string
}

func (s *testKeySet) set(keys ...map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.keys = keys
}

func (s *testKeySet) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"keys": s.keys})
}

// testJWK renders an RSA public key as the JWK map testKeySet serves.
func testJWK(kid string, pub *rsa.PublicKey) map[string]string {
	return map[string]string{
		"kty": "RSA",
		"use": "sig",
		"alg": "RS256",
		"kid": kid,
		"n":   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
		"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
	}
}

func testRSAKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %s", err)
	}

	return key
}

// mintRS256 signs a token locally the way an identity provider would, with the kid in the header
// (or omitted when kid is empty).
func mintRS256(t *testing.T,
	key *rsa.PrivateKey,
	kid string,
	claims Claims,
) string {
	t.Helper()

	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)

	if kid != "" {
		token.Header["kid"] = kid
	}

	signed, err := token.SignedString(key)
	if err != nil {
		t.Fatalf("SignedString: %s", err)
	}

	return signed
}

func testClaims(issuer string, audience string) Claims {
	now := time.Now()

	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour)),
			Issuer:    issuer,
		},
		ClientID: "client-1",
	}

	if audience != "" {
		claims.Audience = jwt.ClaimStrings{audience}
	}

	return claims
}

// backdateRefresh rewinds the verifier's refresh clock so the next fetch attempt (periodic or
// unknown-kid forced) is due immediately, without the test sleeping through a real interval.
func backdateRefresh(v *JWKSVerifier) {
	backdateRefreshBy(v, time.Hour)
}

// backdateRefreshBy rewinds the verifier's refresh clock by exactly d, so a test can put it past
// one window (e.g. the unknown-kid forced-refresh cooldown) while staying short of another (e.g.
// the periodic RefreshInterval), isolating which of refresh's two due-checks fires.
func backdateRefreshBy(v *JWKSVerifier, d time.Duration) {
	v.mu.Lock()
	defer v.mu.Unlock()

	v.lastRefresh = time.Now().Add(-d)
}

// TestNewJWKSVerifier_RequiresConfig verifies that a configuration naming neither a JWKS URL nor
// an issuer is rejected at construction.
func TestNewJWKSVerifier_RequiresConfig(t *testing.T) {
	if _, err := NewJWKSVerifier(JWKSConfig{RefreshInterval: time.Minute}); err == nil {
		t.Error("expected an error constructing a verifier with neither a JWKS URL nor an issuer")
	}
}

// TestNewJWKSVerifier_UnreachableProvider verifies that construction fails fast when the initial
// key fetch fails, rather than starting a verifier that would reject every request.
func TestNewJWKSVerifier_UnreachableProvider(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	if _, err := NewJWKSVerifier(JWKSConfig{JWKSURL: srv.URL, RefreshInterval: time.Minute}); err == nil {
		t.Error("expected an error when the initial JWKS fetch fails")
	}
}

// TestNewJWKSVerifier_OversizedResponseRejected verifies that a JWKS response larger than
// maxJWKSBytes is not read unbounded: with the cap shrunk below the document size, the truncated
// read fails to decode and construction fails, rather than the whole (potentially hostile) body
// being buffered into memory.
func TestNewJWKSVerifier_OversizedResponseRejected(t *testing.T) {
	original := maxJWKSBytes
	maxJWKSBytes = 8
	defer func() { maxJWKSBytes = original }()

	key := testRSAKey(t)

	keySet := &testKeySet{}
	keySet.set(testJWK("key-1", &key.PublicKey))

	srv := httptest.NewServer(http.HandlerFunc(keySet.serve))
	defer srv.Close()

	if _, err := NewJWKSVerifier(JWKSConfig{JWKSURL: srv.URL, RefreshInterval: time.Minute}); err == nil {
		t.Error("expected construction to fail when the JWKS response exceeds the read cap")
	}
}

// TestJWKSVerifier_ValidToken verifies the happy path: an RS256 token signed by a key the JWKS
// endpoint publishes is accepted and its claims returned.
func TestJWKSVerifier_ValidToken(t *testing.T) {
	key := testRSAKey(t)

	keySet := &testKeySet{}
	keySet.set(testJWK("key-1", &key.PublicKey))

	srv := httptest.NewServer(http.HandlerFunc(keySet.serve))
	defer srv.Close()

	v, err := NewJWKSVerifier(JWKSConfig{JWKSURL: srv.URL, RefreshInterval: time.Minute})
	if err != nil {
		t.Fatalf("NewJWKSVerifier: %s", err)
	}

	claims, err := v.Verify(mintRS256(t, key, "key-1", testClaims("", "")))
	if err != nil {
		t.Fatalf("Verify: %s", err)
	}

	if claims.ClientID != "client-1" {
		t.Errorf("expected client_id 'client-1', got '%s'", claims.ClientID)
	}
}

// TestJWKSVerifier_RejectsHS256 verifies the algorithm pin: a token declaring HS256 must be
// rejected outright, closing the classic key-confusion attack where an HMAC token signed with the
// public key's own bytes would otherwise verify against it.
func TestJWKSVerifier_RejectsHS256(t *testing.T) {
	key := testRSAKey(t)

	keySet := &testKeySet{}
	keySet.set(testJWK("key-1", &key.PublicKey))

	srv := httptest.NewServer(http.HandlerFunc(keySet.serve))
	defer srv.Close()

	v, err := NewJWKSVerifier(JWKSConfig{JWKSURL: srv.URL, RefreshInterval: time.Minute})
	if err != nil {
		t.Fatalf("NewJWKSVerifier: %s", err)
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, testClaims("", ""))
	token.Header["kid"] = "key-1"

	signed, err := token.SignedString([]byte("shared-secret"))
	if err != nil {
		t.Fatalf("SignedString: %s", err)
	}

	if _, err := v.Verify(signed); err == nil {
		t.Error("expected an HS256 token to be rejected by an RS256-pinned verifier")
	}
}

// TestJWKSVerifier_RejectsWrongKey verifies that a token signed by a key the provider never
// published is rejected, even when it claims a published kid.
func TestJWKSVerifier_RejectsWrongKey(t *testing.T) {
	key := testRSAKey(t)
	rogueKey := testRSAKey(t)

	keySet := &testKeySet{}
	keySet.set(testJWK("key-1", &key.PublicKey))

	srv := httptest.NewServer(http.HandlerFunc(keySet.serve))
	defer srv.Close()

	v, err := NewJWKSVerifier(JWKSConfig{JWKSURL: srv.URL, RefreshInterval: time.Minute})
	if err != nil {
		t.Fatalf("NewJWKSVerifier: %s", err)
	}

	if _, err := v.Verify(mintRS256(t, rogueKey, "key-1", testClaims("", ""))); err == nil {
		t.Error("expected a token signed by an unpublished key to be rejected")
	}
}

// TestJWKSVerifier_RejectsExpired verifies that a token whose exp claim has passed is rejected.
func TestJWKSVerifier_RejectsExpired(t *testing.T) {
	key := testRSAKey(t)

	keySet := &testKeySet{}
	keySet.set(testJWK("key-1", &key.PublicKey))

	srv := httptest.NewServer(http.HandlerFunc(keySet.serve))
	defer srv.Close()

	v, err := NewJWKSVerifier(JWKSConfig{JWKSURL: srv.URL, RefreshInterval: time.Minute})
	if err != nil {
		t.Fatalf("NewJWKSVerifier: %s", err)
	}

	claims := testClaims("", "")
	claims.ExpiresAt = jwt.NewNumericDate(time.Now().Add(-time.Hour))

	if _, err := v.Verify(mintRS256(t, key, "key-1", claims)); err == nil {
		t.Error("expected an expired token to be rejected")
	}
}

// TestJWKSVerifier_EnforcesIssuerAndAudience verifies that the configured issuer and audience are
// enforced against the token's iss and aud claims.
func TestJWKSVerifier_EnforcesIssuerAndAudience(t *testing.T) {
	key := testRSAKey(t)

	keySet := &testKeySet{}
	keySet.set(testJWK("key-1", &key.PublicKey))

	srv := httptest.NewServer(http.HandlerFunc(keySet.serve))
	defer srv.Close()

	v, err := NewJWKSVerifier(JWKSConfig{
		JWKSURL:         srv.URL,
		Issuer:          "https://idp.example.com",
		Audience:        "hippocampus",
		RefreshInterval: time.Minute,
	})
	if err != nil {
		t.Fatalf("NewJWKSVerifier: %s", err)
	}

	if _, err := v.Verify(mintRS256(t, key, "key-1", testClaims("https://idp.example.com", "hippocampus"))); err != nil {
		t.Errorf("expected a token with the configured issuer and audience to verify: %s", err)
	}

	if _, err := v.Verify(mintRS256(t, key, "key-1", testClaims("https://other.example.com", "hippocampus"))); err == nil {
		t.Error("expected a token from a different issuer to be rejected")
	}

	if _, err := v.Verify(mintRS256(t, key, "key-1", testClaims("https://idp.example.com", "other-service"))); err == nil {
		t.Error("expected a token for a different audience to be rejected")
	}

	if _, err := v.Verify(mintRS256(t, key, "key-1", testClaims("", ""))); err == nil {
		t.Error("expected a token missing iss and aud to be rejected")
	}
}

// TestJWKSVerifier_KeyRotation verifies that a token signed by a freshly rotated-in key is
// accepted on first sight (the unknown kid forces a re-fetch) and that a token signed by the
// rotated-out key is rejected once the cache has moved on.
func TestJWKSVerifier_KeyRotation(t *testing.T) {
	oldKey := testRSAKey(t)
	newKey := testRSAKey(t)

	keySet := &testKeySet{}
	keySet.set(testJWK("key-old", &oldKey.PublicKey))

	srv := httptest.NewServer(http.HandlerFunc(keySet.serve))
	defer srv.Close()

	v, err := NewJWKSVerifier(JWKSConfig{JWKSURL: srv.URL, RefreshInterval: time.Hour})
	if err != nil {
		t.Fatalf("NewJWKSVerifier: %s", err)
	}

	oldToken := mintRS256(t, oldKey, "key-old", testClaims("", ""))

	if _, err := v.Verify(oldToken); err != nil {
		t.Fatalf("Verify before rotation: %s", err)
	}

	keySet.set(testJWK("key-new", &newKey.PublicKey))
	backdateRefresh(v)

	if _, err := v.Verify(mintRS256(t, newKey, "key-new", testClaims("", ""))); err != nil {
		t.Errorf("expected a token signed by the rotated-in key to verify: %s", err)
	}

	if _, err := v.Verify(oldToken); err == nil {
		t.Error("expected a token signed by the rotated-out key to be rejected")
	}
}

// TestJWKSVerifier_NoKidSingleKey verifies the single-key fallback: a token without a kid header
// verifies when the provider publishes exactly one key, since the choice is unambiguous.
func TestJWKSVerifier_NoKidSingleKey(t *testing.T) {
	key := testRSAKey(t)

	keySet := &testKeySet{}
	keySet.set(testJWK("key-1", &key.PublicKey))

	srv := httptest.NewServer(http.HandlerFunc(keySet.serve))
	defer srv.Close()

	v, err := NewJWKSVerifier(JWKSConfig{JWKSURL: srv.URL, RefreshInterval: time.Minute})
	if err != nil {
		t.Fatalf("NewJWKSVerifier: %s", err)
	}

	if _, err := v.Verify(mintRS256(t, key, "", testClaims("", ""))); err != nil {
		t.Errorf("expected a kid-less token to verify against a single published key: %s", err)
	}
}

// TestJWKSVerifier_NoKidMultipleKeysRejected verifies the complement of the single-key fallback: a
// kid-less token is rejected when the provider publishes more than one key, since which one it was
// meant for is ambiguous.
func TestJWKSVerifier_NoKidMultipleKeysRejected(t *testing.T) {
	key1 := testRSAKey(t)
	key2 := testRSAKey(t)

	keySet := &testKeySet{}
	keySet.set(testJWK("key-1", &key1.PublicKey), testJWK("key-2", &key2.PublicKey))

	srv := httptest.NewServer(http.HandlerFunc(keySet.serve))
	defer srv.Close()

	v, err := NewJWKSVerifier(JWKSConfig{JWKSURL: srv.URL, RefreshInterval: time.Minute})
	if err != nil {
		t.Fatalf("NewJWKSVerifier: %s", err)
	}

	if _, err := v.Verify(mintRS256(t, key1, "", testClaims("", ""))); err == nil {
		t.Error("expected a kid-less token to be rejected when multiple keys are published")
	}
}

// TestJWKSVerifier_OIDCDiscovery verifies that an issuer-only configuration resolves the JWKS
// endpoint through the OIDC discovery document.
func TestJWKSVerifier_OIDCDiscovery(t *testing.T) {
	key := testRSAKey(t)

	keySet := &testKeySet{}
	keySet.set(testJWK("key-1", &key.PublicKey))

	mux := http.NewServeMux()
	mux.HandleFunc("/jwks", keySet.serve)

	srv := httptest.NewServer(mux)
	defer srv.Close()

	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"jwks_uri": srv.URL + "/jwks"})
	})

	v, err := NewJWKSVerifier(JWKSConfig{Issuer: srv.URL, RefreshInterval: time.Minute})
	if err != nil {
		t.Fatalf("NewJWKSVerifier: %s", err)
	}

	if _, err := v.Verify(mintRS256(t, key, "key-1", testClaims(srv.URL, ""))); err != nil {
		t.Errorf("expected a token to verify via a discovery-resolved JWKS endpoint: %s", err)
	}
}

// TestNewJWKSVerifier_NonPositiveRefreshInterval verifies that construction rejects a zero or
// negative refresh interval rather than silently never refreshing (or refreshing on every call).
func TestNewJWKSVerifier_NonPositiveRefreshInterval(t *testing.T) {
	if _, err := NewJWKSVerifier(JWKSConfig{JWKSURL: "http://example.invalid/jwks"}); err == nil {
		t.Error("expected an error constructing a verifier with a zero refresh interval")
	}

	if _, err := NewJWKSVerifier(JWKSConfig{JWKSURL: "http://example.invalid/jwks", RefreshInterval: -time.Second}); err == nil {
		t.Error("expected an error constructing a verifier with a negative refresh interval")
	}
}

// TestNewJWKSVerifier_DiscoveryFails verifies that construction fails when only an issuer is
// configured and its OIDC discovery document cannot be fetched, rather than starting a verifier
// with no JWKS URL at all.
func TestNewJWKSVerifier_DiscoveryFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	if _, err := NewJWKSVerifier(JWKSConfig{Issuer: srv.URL, RefreshInterval: time.Minute}); err == nil {
		t.Error("expected construction to fail when OIDC discovery fails")
	}
}

// TestDiscoverJWKSURL_Unreachable verifies discoverJWKSURL's network-error branch: an issuer whose
// discovery endpoint cannot be reached at all (not merely a non-200 status) is reported as an
// error.
func TestDiscoverJWKSURL_Unreachable(t *testing.T) {
	v := &JWKSVerifier{
		cfg:    JWKSConfig{Issuer: "http://127.0.0.1:1"},
		client: &http.Client{Timeout: time.Second},
	}

	if _, err := v.discoverJWKSURL(); err == nil {
		t.Error("expected an error when the discovery endpoint is unreachable")
	}
}

// TestDiscoverJWKSURL_NonOKStatus verifies that a discovery endpoint responding with a non-200
// status is reported as an error rather than parsed as a document.
func TestDiscoverJWKSURL_NonOKStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	v := &JWKSVerifier{
		cfg:    JWKSConfig{Issuer: srv.URL},
		client: &http.Client{Timeout: time.Second},
	}

	if _, err := v.discoverJWKSURL(); err == nil {
		t.Error("expected an error for a non-200 discovery response")
	}
}

// TestDiscoverJWKSURL_MalformedJSON verifies that an unparseable discovery document body is
// reported as an error.
func TestDiscoverJWKSURL_MalformedJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{not json"))
	}))
	defer srv.Close()

	v := &JWKSVerifier{
		cfg:    JWKSConfig{Issuer: srv.URL},
		client: &http.Client{Timeout: time.Second},
	}

	if _, err := v.discoverJWKSURL(); err == nil {
		t.Error("expected an error for a malformed discovery document")
	}
}

// TestDiscoverJWKSURL_MissingJWKSURI verifies that a discovery document with no jwks_uri field is
// reported as an error, rather than resolving to an empty JWKS endpoint.
func TestDiscoverJWKSURL_MissingJWKSURI(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{})
	}))
	defer srv.Close()

	v := &JWKSVerifier{
		cfg:    JWKSConfig{Issuer: srv.URL},
		client: &http.Client{Timeout: time.Second},
	}

	if _, err := v.discoverJWKSURL(); err == nil {
		t.Error("expected an error for a discovery document missing jwks_uri")
	}
}

// TestFetchKeys_Unreachable verifies fetchKeys' network-error branch.
func TestFetchKeys_Unreachable(t *testing.T) {
	v := &JWKSVerifier{
		jwksURL: "http://127.0.0.1:1",
		client:  &http.Client{Timeout: time.Second},
	}

	if _, err := v.fetchKeys(); err == nil {
		t.Error("expected an error when the JWKS endpoint is unreachable")
	}
}

// TestFetchKeys_NonOKStatus verifies that a non-200 JWKS response is reported as an error.
func TestFetchKeys_NonOKStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	v := &JWKSVerifier{jwksURL: srv.URL, client: &http.Client{Timeout: time.Second}}

	if _, err := v.fetchKeys(); err == nil {
		t.Error("expected an error for a non-200 JWKS response")
	}
}

// TestFetchKeys_MalformedJSON verifies that an unparseable JWKS document body is reported as an
// error.
func TestFetchKeys_MalformedJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{not json"))
	}))
	defer srv.Close()

	v := &JWKSVerifier{jwksURL: srv.URL, client: &http.Client{Timeout: time.Second}}

	if _, err := v.fetchKeys(); err == nil {
		t.Error("expected an error for a malformed JWKS document")
	}
}

// TestFetchKeys_SkipsNonSigningKeys verifies that a published key of a non-RSA kty, or an RSA key
// whose use is set to something other than "sig", is skipped rather than causing an error, so long
// as at least one usable key remains.
func TestFetchKeys_SkipsNonSigningKeys(t *testing.T) {
	key := testRSAKey(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		ecKey := map[string]string{"kty": "EC", "kid": "ec-1", "use": "sig"}
		encKey := testJWK("enc-1", &key.PublicKey)
		encKey["use"] = "enc"
		sigKey := testJWK("sig-1", &key.PublicKey)

		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{ecKey, encKey, sigKey}})
	}))
	defer srv.Close()

	v := &JWKSVerifier{jwksURL: srv.URL, client: &http.Client{Timeout: time.Second}}

	keys, err := v.fetchKeys()
	if err != nil {
		t.Fatalf("fetchKeys: %s", err)
	}

	if _, ok := keys["ec-1"]; ok {
		t.Error("expected a non-RSA key to be skipped")
	}

	if _, ok := keys["enc-1"]; ok {
		t.Error("expected a non-signing-use RSA key to be skipped")
	}

	if _, ok := keys["sig-1"]; !ok {
		t.Error("expected the usable signing key to be kept")
	}
}

// TestFetchKeys_SkipsUnparseableKey verifies that an RSA/sig key with an unparseable modulus or
// exponent is skipped (logged, not fatal) so a single malformed key in the set doesn't take down
// verification for every other published key.
func TestFetchKeys_SkipsUnparseableKey(t *testing.T) {
	key := testRSAKey(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		bad := map[string]string{"kty": "RSA", "use": "sig", "kid": "bad-1", "n": "not-base64!!!", "e": "AQAB"}
		good := testJWK("good-1", &key.PublicKey)

		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{bad, good}})
	}))
	defer srv.Close()

	v := &JWKSVerifier{jwksURL: srv.URL, client: &http.Client{Timeout: time.Second}}

	keys, err := v.fetchKeys()
	if err != nil {
		t.Fatalf("fetchKeys: %s", err)
	}

	if _, ok := keys["bad-1"]; ok {
		t.Error("expected the unparseable key to be skipped")
	}

	if _, ok := keys["good-1"]; !ok {
		t.Error("expected the well-formed key to still be kept")
	}
}

// TestFetchKeys_NoUsableKeys verifies that a JWKS document yielding no usable RSA signing keys is
// an error - accepting it would empty the cache and reject every subsequent token.
func TestFetchKeys_NoUsableKeys(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{}})
	}))
	defer srv.Close()

	v := &JWKSVerifier{jwksURL: srv.URL, client: &http.Client{Timeout: time.Second}}

	if _, err := v.fetchKeys(); err == nil {
		t.Error("expected an error for a JWKS document with no usable keys")
	}
}

// TestRSAKeyFromJWK_Errors verifies the modulus/exponent validation errors: an invalid base64url
// modulus, an invalid base64url exponent, and an empty (but well-formed base64) value are all
// rejected.
func TestRSAKeyFromJWK_Errors(t *testing.T) {
	tests := []struct {
		name string
		n    string
		e    string
	}{
		{name: "invalid modulus", n: "not-base64!!!", e: "AQAB"},
		{name: "invalid exponent", n: base64.RawURLEncoding.EncodeToString([]byte{1, 2, 3}), e: "not-base64!!!"},
		{name: "empty modulus", n: "", e: "AQAB"},
		{name: "empty exponent", n: base64.RawURLEncoding.EncodeToString([]byte{1, 2, 3}), e: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := rsaKeyFromJWK(tt.n, tt.e); err == nil {
				t.Errorf("expected an error for %s", tt.name)
			}
		})
	}
}

// TestRSAKeyFromJWK_Valid verifies the success path directly: a well-formed modulus and exponent
// build a matching RSA public key.
func TestRSAKeyFromJWK_Valid(t *testing.T) {
	key := testRSAKey(t)

	n := base64.RawURLEncoding.EncodeToString(key.N.Bytes())
	e := base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes())

	got, err := rsaKeyFromJWK(n, e)
	if err != nil {
		t.Fatalf("rsaKeyFromJWK: %s", err)
	}

	if got.E != key.E || got.N.Cmp(key.N) != 0 {
		t.Error("expected the parsed key to match the source key's modulus and exponent")
	}
}

// TestJWKSVerifier_VerifyRefreshFailsKeepsCachedKeys verifies that when Verify's own due periodic
// refresh fails (the identity provider has gone down), verification still proceeds against the
// last successfully cached keys rather than failing outright.
func TestJWKSVerifier_VerifyRefreshFailsKeepsCachedKeys(t *testing.T) {
	key := testRSAKey(t)

	keySet := &testKeySet{}
	keySet.set(testJWK("key-1", &key.PublicKey))

	var down atomic.Bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if down.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)

			return
		}

		keySet.serve(w, r)
	}))
	defer srv.Close()

	v, err := NewJWKSVerifier(JWKSConfig{JWKSURL: srv.URL, RefreshInterval: time.Minute})
	if err != nil {
		t.Fatalf("NewJWKSVerifier: %s", err)
	}

	token := mintRS256(t, key, "key-1", testClaims("", ""))

	// Make the endpoint fail and put the refresh clock past RefreshInterval, so Verify's own
	// due-refresh attempt fails; the previously cached key must still serve the request.
	down.Store(true)
	backdateRefresh(v)

	if _, err := v.Verify(token); err != nil {
		t.Errorf("expected verification to succeed against cached keys despite a failed refresh: %s", err)
	}
}

// TestJWKSVerifier_KeyForToken_ForcedRefreshFails verifies that when an unknown kid forces a
// re-fetch and that re-fetch itself fails (provider down), the token is rejected with the generic
// "no key matches" error rather than panicking or wedging.
func TestJWKSVerifier_KeyForToken_ForcedRefreshFails(t *testing.T) {
	key := testRSAKey(t)
	otherKey := testRSAKey(t)

	keySet := &testKeySet{}
	keySet.set(testJWK("key-1", &key.PublicKey))

	var down atomic.Bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if down.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)

			return
		}

		keySet.serve(w, r)
	}))
	defer srv.Close()

	// RefreshInterval long enough that Verify's own outer refresh(false) is never due within this
	// test, isolating keyForToken's own forced refresh.
	v, err := NewJWKSVerifier(JWKSConfig{JWKSURL: srv.URL, RefreshInterval: time.Minute})
	if err != nil {
		t.Fatalf("NewJWKSVerifier: %s", err)
	}

	// Past the forced-refresh cooldown (30s) but short of RefreshInterval (60s), so only the
	// keyForToken forced refresh is due, not Verify's own.
	backdateRefreshBy(v, 40*time.Second)
	down.Store(true)

	unknownKidToken := mintRS256(t, otherKey, "key-unknown", testClaims("", ""))

	if _, err := v.Verify(unknownKidToken); err == nil {
		t.Error("expected an unknown-kid token to be rejected when the forced refresh itself fails")
	}
}

// TestJWKSVerifier_KeyForToken_ForcedRefreshFindsRotatedKey verifies keyForToken's own successful
// forced-refresh path in isolation from Verify's outer refresh: with the outer refresh not yet
// due, an unknown kid still forces a re-fetch that picks up a freshly rotated-in key.
func TestJWKSVerifier_KeyForToken_ForcedRefreshFindsRotatedKey(t *testing.T) {
	oldKey := testRSAKey(t)
	newKey := testRSAKey(t)

	keySet := &testKeySet{}
	keySet.set(testJWK("key-old", &oldKey.PublicKey))

	srv := httptest.NewServer(http.HandlerFunc(keySet.serve))
	defer srv.Close()

	v, err := NewJWKSVerifier(JWKSConfig{JWKSURL: srv.URL, RefreshInterval: time.Minute})
	if err != nil {
		t.Fatalf("NewJWKSVerifier: %s", err)
	}

	keySet.set(testJWK("key-new", &newKey.PublicKey))

	// Past the forced-refresh cooldown (30s) but short of RefreshInterval (60s): Verify's own
	// outer refresh is not due, so only keyForToken's own forced refresh runs.
	backdateRefreshBy(v, 40*time.Second)

	if _, err := v.Verify(mintRS256(t, newKey, "key-new", testClaims("", ""))); err != nil {
		t.Errorf("expected the rotated-in key to verify via keyForToken's own forced refresh: %s", err)
	}
}
