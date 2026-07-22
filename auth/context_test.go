package auth

import (
	"context"
	"testing"
)

// TestClaimsFromContext_NoClaims verifies that a context carrying no stashed claims (auth
// disabled, or an open path that never runs a verifier) yields a nil Claims rather than a panic
// or a zero-value struct that would be mistaken for an authenticated request.
func TestClaimsFromContext_NoClaims(t *testing.T) {
	if claims := ClaimsFromContext(context.Background()); claims != nil {
		t.Errorf("expected nil claims from a context with none stashed, got %+v", claims)
	}
}

// TestClientIDFromContext_NoClaims verifies the convenience accessor returns "" - not a panic -
// when the request carried no verified claims, mirroring ClaimsFromContext's nil-safe contract.
func TestClientIDFromContext_NoClaims(t *testing.T) {
	if clientID := ClientIDFromContext(context.Background()); clientID != "" {
		t.Errorf("expected an empty client id from a context with none stashed, got %q", clientID)
	}
}

// TestClientIDFromContext_RoundTrip verifies the happy path end-to-end: claims stashed via
// ContextWithClaims are retrievable by both ClaimsFromContext and the ClientIDFromContext
// convenience wrapper.
func TestClientIDFromContext_RoundTrip(t *testing.T) {
	ctx := ContextWithClaims(context.Background(), &Claims{ClientID: "client-9"})

	if clientID := ClientIDFromContext(ctx); clientID != "client-9" {
		t.Errorf("expected client id 'client-9', got %q", clientID)
	}
}
