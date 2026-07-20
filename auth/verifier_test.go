package auth

import (
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// longSecret is a >=32-byte secret used where a test needs a key that does not trip the
// short-secret warning path.
const longSecret = "a-sufficiently-long-signing-secret-value"

// TestNewHMACVerifier_NoSecret verifies that constructing a verifier with neither a legacy secret
// nor any keys is rejected up front, rather than silently accepting (or rejecting) every token
// later.
func TestNewHMACVerifier_NoSecret(t *testing.T) {
	if _, err := NewHMACVerifier(HMACConfig{}); err == nil {
		t.Error("expected an error constructing a verifier with no secret at all")
	}
}

// TestNewHMACVerifier_ShortSecretStillConstructs verifies that a short-but-non-empty secret is
// accepted (only warned about, not rejected), so an existing deployment configured with one keeps
// working after the length hardening was added.
func TestNewHMACVerifier_ShortSecretStillConstructs(t *testing.T) {
	v, err := NewHMACVerifier(HMACConfig{LegacySecret: "short"})
	if err != nil {
		t.Fatalf("expected a short secret to construct with only a warning, got error: %s", err)
	}

	if v == nil {
		t.Fatal("expected a non-nil verifier for a short secret")
	}
}

// TestNewHMACVerifier_DuplicateKid verifies that two keys sharing a kid are rejected, since a kid
// must unambiguously select one secret.
func TestNewHMACVerifier_DuplicateKid(t *testing.T) {
	_, err := NewHMACVerifier(HMACConfig{
		Keys: []SigningKey{
			{Kid: "dup", Secret: longSecret},
			{Kid: "dup", Secret: longSecret + "-2"},
		},
	})

	if err == nil {
		t.Error("expected an error for duplicate signing key kids")
	}
}

// TestNewHMACVerifier_EmptyKid verifies that a key with an empty kid is rejected - the legacy
// secret is the mechanism for a key without a kid.
func TestNewHMACVerifier_EmptyKid(t *testing.T) {
	_, err := NewHMACVerifier(HMACConfig{
		Keys: []SigningKey{{Kid: "", Secret: longSecret}},
	})

	if err == nil {
		t.Error("expected an error for a signing key with an empty kid")
	}
}

// TestNewHMACVerifier_UnknownActiveKid verifies that an activeKid naming no configured key is
// rejected, so a mint would never silently fall back to the wrong key.
func TestNewHMACVerifier_UnknownActiveKid(t *testing.T) {
	_, err := NewHMACVerifier(HMACConfig{
		Keys:      []SigningKey{{Kid: "real", Secret: longSecret}},
		ActiveKid: "ghost",
	})

	if err == nil {
		t.Error("expected an error for an activeKid that names no configured key")
	}
}

// TestHMACVerifier_ExpiredToken verifies that a token whose exp claim has already passed is
// rejected.
func TestHMACVerifier_ExpiredToken(t *testing.T) {
	v, err := NewHMACVerifier(HMACConfig{LegacySecret: "test-secret"})
	if err != nil {
		t.Fatalf("NewHMACVerifier: %s", err)
	}

	token, err := MintToken(MintRequest{Secret: "test-secret", ClientID: "client-1", TTL: -time.Hour})
	if err != nil {
		t.Fatalf("MintToken: %s", err)
	}

	if _, err := v.Verify(token); err == nil {
		t.Error("expected an expired token to be rejected")
	}
}

// TestHMACVerifier_NoExpirationRejected verifies that a token carrying no exp claim is rejected
// rather than verifying forever. golang-jwt only validates exp when present, so without
// jwt.WithExpirationRequired an expiry-less token (e.g. minted directly with a leaked secret) would
// be irrevocable-by-time.
func TestHMACVerifier_NoExpirationRejected(t *testing.T) {
	v, err := NewHMACVerifier(HMACConfig{LegacySecret: longSecret})
	if err != nil {
		t.Fatalf("NewHMACVerifier: %s", err)
	}

	// Sign a token by hand with no exp claim - MintToken always stamps one, so this cannot use it.
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, Claims{ClientID: "client-1"})

	signed, err := token.SignedString([]byte(longSecret))
	if err != nil {
		t.Fatalf("SignedString: %s", err)
	}

	if _, err := v.Verify(signed); err == nil {
		t.Error("expected a token without an exp claim to be rejected")
	}
}

// TestHMACVerifier_WrongSecret verifies that a token signed with a different secret is rejected.
func TestHMACVerifier_WrongSecret(t *testing.T) {
	v, err := NewHMACVerifier(HMACConfig{LegacySecret: "correct-secret"})
	if err != nil {
		t.Fatalf("NewHMACVerifier: %s", err)
	}

	token, err := MintToken(MintRequest{Secret: "wrong-secret", ClientID: "client-1", TTL: time.Hour})
	if err != nil {
		t.Fatalf("MintToken: %s", err)
	}

	if _, err := v.Verify(token); err == nil {
		t.Error("expected a token signed with a different secret to be rejected")
	}
}

// TestHMACVerifier_KeySelectedByKid verifies that a token minted under one kid verifies against
// that key and not another, and that the right key produces valid claims.
func TestHMACVerifier_KeySelectedByKid(t *testing.T) {
	v, err := NewHMACVerifier(HMACConfig{
		Keys: []SigningKey{
			{Kid: "old", Secret: longSecret + "-old"},
			{Kid: "new", Secret: longSecret + "-new"},
		},
		ActiveKid: "new",
	})
	if err != nil {
		t.Fatalf("NewHMACVerifier: %s", err)
	}

	oldToken, err := MintToken(MintRequest{Secret: longSecret + "-old", Kid: "old", ClientID: "c", TTL: time.Hour})
	if err != nil {
		t.Fatalf("MintToken old: %s", err)
	}

	newToken, err := MintToken(MintRequest{Secret: longSecret + "-new", Kid: "new", ClientID: "c", TTL: time.Hour})
	if err != nil {
		t.Fatalf("MintToken new: %s", err)
	}

	if _, err := v.Verify(oldToken); err != nil {
		t.Errorf("expected the old-kid token to still verify (rotation overlap), got: %s", err)
	}

	if _, err := v.Verify(newToken); err != nil {
		t.Errorf("expected the new-kid token to verify, got: %s", err)
	}
}

// TestHMACVerifier_UnknownKidRejected verifies that a token whose kid names no configured key is
// rejected, even when its signature is otherwise well-formed.
func TestHMACVerifier_UnknownKidRejected(t *testing.T) {
	v, err := NewHMACVerifier(HMACConfig{
		Keys: []SigningKey{{Kid: "known", Secret: longSecret}},
	})
	if err != nil {
		t.Fatalf("NewHMACVerifier: %s", err)
	}

	token, err := MintToken(MintRequest{Secret: longSecret, Kid: "unknown", ClientID: "c", TTL: time.Hour})
	if err != nil {
		t.Fatalf("MintToken: %s", err)
	}

	if _, err := v.Verify(token); err == nil {
		t.Error("expected a token with an unknown kid to be rejected")
	}
}

// TestHMACVerifier_KidlessTokenUsesLegacySecret verifies that a token with no kid header verifies
// against the legacy secret - the compatibility path for tokens minted before keyed rotation.
func TestHMACVerifier_KidlessTokenUsesLegacySecret(t *testing.T) {
	v, err := NewHMACVerifier(HMACConfig{
		LegacySecret: longSecret,
		Keys:         []SigningKey{{Kid: "k", Secret: longSecret + "-k"}},
	})
	if err != nil {
		t.Fatalf("NewHMACVerifier: %s", err)
	}

	token, err := MintToken(MintRequest{Secret: longSecret, ClientID: "c", TTL: time.Hour})
	if err != nil {
		t.Fatalf("MintToken: %s", err)
	}

	if _, err := v.Verify(token); err != nil {
		t.Errorf("expected a kid-less token to verify against the legacy secret, got: %s", err)
	}
}

// TestHMACVerifier_KidlessTokenRejectedWithoutLegacySecret verifies that when only keyed secrets
// are configured, a kid-less token is rejected rather than falling back to some key it did not
// name.
func TestHMACVerifier_KidlessTokenRejectedWithoutLegacySecret(t *testing.T) {
	v, err := NewHMACVerifier(HMACConfig{
		Keys: []SigningKey{{Kid: "k", Secret: longSecret}},
	})
	if err != nil {
		t.Fatalf("NewHMACVerifier: %s", err)
	}

	token, err := MintToken(MintRequest{Secret: longSecret, ClientID: "c", TTL: time.Hour})
	if err != nil {
		t.Fatalf("MintToken: %s", err)
	}

	if _, err := v.Verify(token); err == nil {
		t.Error("expected a kid-less token to be rejected when no legacy secret is configured")
	}
}

// TestHMACVerifier_RejectsUnexpectedAlgorithm verifies that the verifier refuses to trust a
// token's own declared algorithm: a token signed with a different (but otherwise valid) algorithm
// using the very same secret bytes must still be rejected, since NewHMACVerifier only ever
// intends to trust HS256.
func TestHMACVerifier_RejectsUnexpectedAlgorithm(t *testing.T) {
	v, err := NewHMACVerifier(HMACConfig{LegacySecret: "test-secret"})
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
	v, err := NewHMACVerifier(HMACConfig{LegacySecret: "test-secret"})
	if err != nil {
		t.Fatalf("NewHMACVerifier: %s", err)
	}

	if _, err := v.Verify("not-a-token"); err == nil {
		t.Error("expected a malformed token to be rejected")
	}
}
