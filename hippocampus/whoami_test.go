package hippocampus

import (
	"context"
	"testing"

	"github.com/fastbean-au/hippocampus/auth"
	"github.com/fastbean-au/hippocampus/contract"
)

// TestWhoAmI_AuthDisabled reports an unrestricted admin tier when no tier is on the context, which
// is how a request looks when authorisation never ran (authentication disabled).
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

// TestWhoAmI_SummariserEnabled verifies the summariser capability is reported from the deployment's
// configuration rather than assumed, so a client can offer service-authored summarisation only
// where SummariseMemories would actually serve.
func TestWhoAmI_SummariserEnabled(t *testing.T) {
	for _, tc := range []struct {
		name    string
		enabled bool
	}{
		{"configured", true},
		{"absent", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newSummariseTestServer(t, &fakeSummariser{enabled: tc.enabled})

			res, err := s.WhoAmI(context.Background(), &contract.EmptyRequest{})
			if err != nil {
				t.Fatalf("WhoAmI: %s", err)
			}

			if res.GetSummariserEnabled() != tc.enabled {
				t.Errorf("summariser_enabled = %v, want %v", res.GetSummariserEnabled(), tc.enabled)
			}
		})
	}

	// A server with no summariser wired at all must report false rather than panic on the nil - the
	// default shape of every deployment that has not enabled ollama.
	s := newTestServer(t)

	res, err := s.WhoAmI(context.Background(), &contract.EmptyRequest{})
	if err != nil {
		t.Fatalf("WhoAmI: %s", err)
	}

	if res.GetSummariserEnabled() {
		t.Error("expected summariser_enabled=false when no summariser is configured")
	}
}

// TestWhoAmI_ConsolidationCapabilities pins the two consolidation flags to the server's own
// configuration, on both the authenticated and the unauthenticated path.
//
// The pairing with ExplainConsolidation is the point of the test rather than the field's value:
// consolidation_enabled exists so a client can hide what a replica would refuse, so it is only
// correct if it agrees with what that RPC actually does. A flag that said "yes" while the RPC
// answered FAILED_PRECONDITION would be worse than no flag, since the console would then present
// the Decay tab with more confidence than before.
func TestWhoAmI_ConsolidationCapabilities(t *testing.T) {
	for _, tc := range []struct {
		name          string
		consolidating bool
		tombstones    bool
	}{
		{"consolidator with the log on", true, true},
		{"consolidator with the log off", true, false},
		{"replica", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer(t)

			s.consolidationEnabled = tc.consolidating
			s.consolidation.tombstones = tc.tombstones

			for _, ctx := range []context.Context{
				context.Background(),
				auth.ContextWithTier(context.Background(), auth.TierReader),
			} {
				res, err := s.WhoAmI(ctx, &contract.EmptyRequest{})
				if err != nil {
					t.Fatalf("WhoAmI: %s", err)
				}

				if res.GetConsolidationEnabled() != tc.consolidating {
					t.Errorf("consolidation_enabled = %v, want %v", res.GetConsolidationEnabled(), tc.consolidating)
				}

				if res.GetTombstonesEnabled() != tc.tombstones {
					t.Errorf("tombstones_enabled = %v, want %v", res.GetTombstonesEnabled(), tc.tombstones)
				}
			}

			// The flag has to mean what the RPC does, or hiding a control on it is a guess.
			_, err := s.ExplainConsolidation(context.Background(), &contract.ExplainConsolidationRequest{})

			if refused := err != nil; refused == tc.consolidating {
				t.Errorf("ExplainConsolidation refused = %v with consolidation_enabled = %v; the flag must predict the refusal", refused, tc.consolidating)
			}
		})
	}
}

// TestWhoAmI_Authenticated reports the tier and client id the authorisation layer stashed.
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

// TestWhoAmI_VersionAndCallbacks covers the two fields item 103 added: the build, which no other
// gRPC-reachable RPC reports unconditionally, and the callback capability flag, which exists for
// tombstones_enabled's reason - an empty queue and a disabled feature render identically.
//
// Both are properties of the DEPLOYMENT, so both must be reported on the unauthenticated path as
// well; that is the half this asserts twice.
func TestWhoAmI_VersionAndCallbacks(t *testing.T) {
	for _, tc := range []struct {
		name      string
		version   string
		callbacks bool
	}{
		{"a stamped build with callbacks on", "v1.2.3", true},
		{"a stamped build with callbacks off", "v1.2.3", false},
		{"no build information", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer(t)

			s.version = tc.version
			s.callbacksEnabled = tc.callbacks

			for _, ctx := range []context.Context{
				context.Background(),
				auth.ContextWithTier(context.Background(), auth.TierReader),
			} {
				res, err := s.WhoAmI(ctx, &contract.EmptyRequest{})
				if err != nil {
					t.Fatalf("WhoAmI: %s", err)
				}

				if res.GetVersion() != tc.version {
					t.Errorf("version = %q, want %q", res.GetVersion(), tc.version)
				}

				if res.GetCallbacksEnabled() != tc.callbacks {
					t.Errorf("callbacks_enabled = %v, want %v", res.GetCallbacksEnabled(), tc.callbacks)
				}
			}
		})
	}
}
