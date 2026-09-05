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

// TestGroupsFromContext covers the contract callers must branch on: the bool, never the slice's
// length. "No scope" and "scoped to nothing" are indistinguishable from the slice alone, and
// defaulting either way is a bug - one empties every read on an unauthenticated instance, the other
// hands a bound token the whole store.
func TestGroupsFromContext(t *testing.T) {
	tests := []struct {
		name       string
		ctx        context.Context
		wantGroups []string
		wantBound  bool
	}{
		{
			name:      "no claims at all reports unbound",
			ctx:       context.Background(),
			wantBound: false,
		},
		{
			name:      "verified claims carrying no groups report unbound",
			ctx:       ContextWithClaims(context.Background(), &Claims{ClientID: "client-1"}),
			wantBound: false,
		},
		{
			name:      "an empty groups slice reports unbound",
			ctx:       ContextWithClaims(context.Background(), &Claims{ClientID: "client-1", Groups: []string{}}),
			wantBound: false,
		},
		{
			name:       "a scoped token reports its groups and bound",
			ctx:        ContextWithClaims(context.Background(), &Claims{ClientID: "client-1", Groups: []string{"sales", "support"}}),
			wantGroups: []string{"sales", "support"},
			wantBound:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			groups, bound := GroupsFromContext(tt.ctx)

			if bound != tt.wantBound {
				t.Fatalf("bound = %v, want %v", bound, tt.wantBound)
			}

			if len(groups) != len(tt.wantGroups) {
				t.Fatalf("groups = %v, want %v", groups, tt.wantGroups)
			}

			for i, v := range tt.wantGroups {
				if groups[i] != v {
					t.Errorf("groups[%d] = %q, want %q", i, groups[i], v)
				}
			}
		})
	}
}
