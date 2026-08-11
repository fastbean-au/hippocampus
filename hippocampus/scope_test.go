package hippocampus

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/fastbean-au/hippocampus/auth"
	"github.com/fastbean-au/hippocampus/contract"
)

// TestScopesCoverEveryRPC is the drift guard, mirroring auth's TestPoliciesCoverEveryRPC: every RPC
// in the generated service descriptor must declare how it honours a caller's group scope.
//
// It exists because the alternative failure is silent. An RPC added without a scope predicate does
// not error, does not log, and passes every test written for it - it simply returns other groups'
// records to a caller who should not see them, and looks exactly like a working feature while doing
// so. Forcing a new RPC to name its mode here is what turns that into a build failure.
func TestScopesCoverEveryRPC(t *testing.T) {
	for _, m := range contract.Hippocampus_ServiceDesc.Methods {
		if _, ok := scopes[m.MethodName]; !ok {
			t.Errorf("RPC %q has no entry in scopes - declare how it honours a caller's group scope (see hippocampus/scope.go)", m.MethodName)
		}
	}

	if len(scopes) != len(contract.Hippocampus_ServiceDesc.Methods) {
		t.Errorf("scopes has %d entries but the service descriptor has %d methods - an entry names a non-existent RPC",
			len(scopes), len(contract.Hippocampus_ServiceDesc.Methods))
	}
}

// TestScopesAgreeWithPolicies checks the two tables describe the same service. They are independent
// by design - a tier and a scope answer different questions - but they are keyed identically, so a
// method name misspelled in one and not the other would leave that RPC unguarded in a way each
// table's own coverage test would still pass.
func TestScopesAgreeWithPolicies(t *testing.T) {
	for _, m := range contract.Hippocampus_ServiceDesc.Methods {
		_, hasScope := scopes[m.MethodName]

		if !hasScope {
			continue
		}

		if !auth.HasPolicy(m.MethodName) {
			t.Errorf("RPC %q has a scope entry but no authorisation policy", m.MethodName)
		}
	}
}

// scopedContext returns a context carrying a verified-claims scope, as the auth adapters build it.
func scopedContext(groups ...string) context.Context {
	return auth.ContextWithClaims(context.Background(), &auth.Claims{Groups: groups})
}

// TestScopedGroupsDistinguishesUnscopedFromEmpty pins the distinction the whole mechanism rests on:
// no scope means the whole store, and it must not be confusable with a scope that happens to be
// empty. Getting this backwards in either direction is a security bug or a total outage, and the
// slice alone cannot tell them apart.
func TestScopedGroupsDistinguishesUnscopedFromEmpty(t *testing.T) {
	s := &Server{}

	if _, bound := s.scopedGroups(context.Background()); bound {
		t.Error("a context with no claims must report unbound")
	}

	if _, bound := s.scopedGroups(auth.ContextWithClaims(context.Background(), &auth.Claims{})); bound {
		t.Error("claims carrying no groups must report unbound")
	}

	groups, bound := s.scopedGroups(scopedContext("a"))

	if !bound || len(groups) != 1 || groups[0] != "a" {
		t.Errorf("scopedGroups = %v, %v; want [a], true", groups, bound)
	}
}

func TestRequireUnbound(t *testing.T) {
	s := &Server{}

	if err := s.requireUnbound(context.Background(), "Purge"); err != nil {
		t.Errorf("an unscoped caller must be allowed: %s", err)
	}

	err := s.requireUnbound(scopedContext("a"), "Purge")

	if status.Code(err) != codes.PermissionDenied {
		t.Errorf("a scoped caller must be refused with PermissionDenied, got %v", err)
	}
}

func TestWriteGroup(t *testing.T) {
	s := &Server{}

	tests := []struct {
		name      string
		ctx       context.Context
		requested string
		want      string
		wantCode  codes.Code
	}{
		{
			name:      "unscoped caller keeps what it asked for",
			ctx:       context.Background(),
			requested: "anything",
			want:      "anything",
		},
		{
			name: "unscoped caller may write with no group",
			ctx:  context.Background(),
		},
		{
			name: "sole group is stamped when none is named",
			ctx:  scopedContext("a"),
			want: "a",
		},
		{
			name:      "a group in scope is kept",
			ctx:       scopedContext("a", "b"),
			requested: "b",
			want:      "b",
		},
		{
			name:      "a group outside scope is refused",
			ctx:       scopedContext("a"),
			requested: "b",
			wantCode:  codes.PermissionDenied,
		},
		{
			name:     "several groups and none named is ambiguous",
			ctx:      scopedContext("a", "b"),
			wantCode: codes.InvalidArgument,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := s.writeGroup(test.ctx, test.requested)

			if test.wantCode != codes.OK {
				if status.Code(err) != test.wantCode {
					t.Fatalf("writeGroup error = %v, want code %v", err, test.wantCode)
				}

				return
			}

			if err != nil {
				t.Fatalf("writeGroup returned an unexpected error: %s", err)
			}

			if got != test.want {
				t.Errorf("writeGroup = %q, want %q", got, test.want)
			}
		})
	}
}
