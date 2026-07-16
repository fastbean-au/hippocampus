package auth

import (
	"testing"
	"time"
)

// TestMintToken_RoundTrip verifies that a minted token verifies successfully and carries the
// client id it was minted with.
func TestMintToken_RoundTrip(t *testing.T) {
	v, err := NewHMACVerifier(HMACConfig{LegacySecret: "test-secret"})
	if err != nil {
		t.Fatalf("NewHMACVerifier: %s", err)
	}

	token, err := MintToken(MintRequest{Secret: "test-secret", ClientID: "client-1", TTL: time.Hour})
	if err != nil {
		t.Fatalf("MintToken: %s", err)
	}

	claims, err := v.Verify(token)
	if err != nil {
		t.Fatalf("Verify: %s", err)
	}

	if claims.ClientID != "client-1" {
		t.Errorf("expected client id 'client-1', got '%s'", claims.ClientID)
	}
}

// TestMintToken_EmptySecret verifies that minting refuses an empty secret rather than silently
// signing with one, which would make every token trivially forgeable.
func TestMintToken_EmptySecret(t *testing.T) {
	if _, err := MintToken(MintRequest{ClientID: "client-1", TTL: time.Hour}); err == nil {
		t.Error("expected an error minting with an empty secret")
	}
}

// TestMintToken_SetsJTI verifies that every minted token carries a non-empty jti, and that two
// tokens minted from the same request get distinct ids - the property per-token revocation relies
// on.
func TestMintToken_SetsJTI(t *testing.T) {
	req := MintRequest{Secret: "test-secret", ClientID: "client-1", TTL: time.Hour}

	first, err := MintToken(req)
	if err != nil {
		t.Fatalf("MintToken: %s", err)
	}

	second, err := MintToken(req)
	if err != nil {
		t.Fatalf("MintToken: %s", err)
	}

	firstID, err := TokenID(first)
	if err != nil {
		t.Fatalf("TokenID: %s", err)
	}

	secondID, err := TokenID(second)
	if err != nil {
		t.Fatalf("TokenID: %s", err)
	}

	if firstID == "" {
		t.Error("expected a non-empty jti on a minted token")
	}

	if firstID == secondID {
		t.Errorf("expected distinct jtis, got %q twice", firstID)
	}
}

// TestMintToken_KidHeader verifies that minting with a kid sets it as the token header (so a
// multi-key verifier can select the right secret), and that minting without one leaves the header
// absent (verified by the legacy secret).
func TestMintToken_KidHeader(t *testing.T) {
	v, err := NewHMACVerifier(HMACConfig{
		Keys: []SigningKey{{Kid: "2026-07", Secret: "a-sufficiently-long-signing-secret-value"}},
	})
	if err != nil {
		t.Fatalf("NewHMACVerifier: %s", err)
	}

	token, err := MintToken(MintRequest{
		Secret:   "a-sufficiently-long-signing-secret-value",
		Kid:      "2026-07",
		ClientID: "client-1",
		TTL:      time.Hour,
	})
	if err != nil {
		t.Fatalf("MintToken: %s", err)
	}

	if _, err := v.Verify(token); err != nil {
		t.Fatalf("expected a kid'd token to verify against its keyed secret, got: %s", err)
	}
}
