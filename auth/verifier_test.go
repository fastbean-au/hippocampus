package auth

import (
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// TestNewHMACVerifier_EmptySecret verifies that constructing a verifier with an empty secret is
// rejected up front, rather than silently accepting (or rejecting) every token later.
func TestNewHMACVerifier_EmptySecret(t *testing.T) {
	if _, err := NewHMACVerifier(""); err == nil {
		t.Error("expected an error constructing a verifier with an empty secret")
	}
}

// TestNewHMACVerifier_ShortSecretStillConstructs verifies that a short-but-non-empty secret is
// accepted (only warned about, not rejected), so an existing deployment configured with one keeps
// working after the length hardening was added.
func TestNewHMACVerifier_ShortSecretStillConstructs(t *testing.T) {
	v, err := NewHMACVerifier("short")
	if err != nil {
		t.Fatalf("expected a short secret to construct with only a warning, got error: %s", err)
	}

	if v == nil {
		t.Fatal("expected a non-nil verifier for a short secret")
	}
}

// TestHMACVerifier_ExpiredToken verifies that a token whose exp claim has already passed is
// rejected.
func TestHMACVerifier_ExpiredToken(t *testing.T) {
	v, err := NewHMACVerifier("test-secret")
	if err != nil {
		t.Fatalf("NewHMACVerifier: %s", err)
	}

	token, err := MintToken("test-secret", "client-1", -time.Hour)
	if err != nil {
		t.Fatalf("MintToken: %s", err)
	}

	if _, err := v.Verify(token); err == nil {
		t.Error("expected an expired token to be rejected")
	}
}

// TestHMACVerifier_WrongSecret verifies that a token signed with a different secret is rejected.
func TestHMACVerifier_WrongSecret(t *testing.T) {
	v, err := NewHMACVerifier("correct-secret")
	if err != nil {
		t.Fatalf("NewHMACVerifier: %s", err)
	}

	token, err := MintToken("wrong-secret", "client-1", time.Hour)
	if err != nil {
		t.Fatalf("MintToken: %s", err)
	}

	if _, err := v.Verify(token); err == nil {
		t.Error("expected a token signed with a different secret to be rejected")
	}
}

// TestHMACVerifier_RejectsUnexpectedAlgorithm verifies that the verifier refuses to trust a
// token's own declared algorithm: a token signed with a different (but otherwise valid) algorithm
// using the very same secret bytes must still be rejected, since NewHMACVerifier only ever
// intends to trust HS256.
func TestHMACVerifier_RejectsUnexpectedAlgorithm(t *testing.T) {
	v, err := NewHMACVerifier("test-secret")
	if err != nil {
		t.Fatalf("NewHMACVerifier: %s", err)
	}

	now := time.Now()
	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour)),
		},
		ClientID: "client-1",
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS384, claims)

	signed, err := token.SignedString([]byte("test-secret"))
	if err != nil {
		t.Fatalf("SignedString: %s", err)
	}

	if _, err := v.Verify(signed); err == nil {
		t.Error("expected a token signed with an unexpected algorithm to be rejected")
	}
}

// TestHMACVerifier_RejectsGarbage verifies that a malformed token string is rejected rather than
// panicking.
func TestHMACVerifier_RejectsGarbage(t *testing.T) {
	v, err := NewHMACVerifier("test-secret")
	if err != nil {
		t.Fatalf("NewHMACVerifier: %s", err)
	}

	if _, err := v.Verify("not-a-token"); err == nil {
		t.Error("expected a malformed token to be rejected")
	}
}
