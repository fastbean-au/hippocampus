package hippocampus

import (
	"context"
	"testing"

	"github.com/fastbean-au/hippocampus/auth"
	"github.com/fastbean-au/hippocampus/contract"
)

// TestWhoAmI_AuthDisabled reports an unrestricted admin tier when no tier is on the context, which
// is how a request looks when authorization never ran (authentication disabled).
func TestWhoAmI_AuthDisabled(t *testing.T) {
	s := newTestServer(t)

	res, err := s.WhoAmI(context.Background(), &contract.EmptyRequest{})
	if err != nil {
		t.Fatalf("WhoAmI: %s", err)
	}

	if res.GetAuthEnabled() || res.GetRole() != "admin" || res.GetClientId() != "" {
		t.Fatalf("expected auth_enabled=false role=admin client_id='', got %+v", res)
	}
}

// TestWhoAmI_Authenticated reports the tier and client id the authorization layer stashed.
func TestWhoAmI_Authenticated(t *testing.T) {
	s := newTestServer(t)

	ctx := auth.ContextWithClaims(context.Background(), &auth.Claims{ClientID: "console-1"})
	ctx = auth.ContextWithTier(ctx, auth.TierReader)

	res, err := s.WhoAmI(ctx, &contract.EmptyRequest{})
	if err != nil {
		t.Fatalf("WhoAmI: %s", err)
	}

	if !res.GetAuthEnabled() || res.GetRole() != "reader" || res.GetClientId() != "console-1" {
		t.Fatalf("expected auth_enabled=true role=reader client_id=console-1, got %+v", res)
	}
}
