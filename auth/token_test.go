package auth

import (
	"testing"
	"time"
)

// TestMintToken_RoundTrip verifies that a minted token verifies successfully and carries the
// client id it was minted with.
func TestMintToken_RoundTrip(t *testing.T) {
	v, err := NewHMACVerifier("test-secret")
	if err != nil {
		t.Fatalf("NewHMACVerifier: %s", err)
	}

	token, err := MintToken("test-secret", "client-1", time.Hour)
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
	if _, err := MintToken("", "client-1", time.Hour); err == nil {
		t.Error("expected an error minting with an empty secret")
	}
}
