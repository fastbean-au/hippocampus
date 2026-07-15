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
	v.mu.Lock()
	defer v.mu.Unlock()

	v.lastRefresh = time.Now().Add(-time.Hour)
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
