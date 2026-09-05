package auth

import (
	"fmt"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// TestGroupInScope covers the membership test's two contracts: an empty scope means unscoped and
// matches everything (NOT "scoped to nothing"), and a non-empty scope matches byte-for-byte so it
// agrees with how the store compares group_name.
func TestGroupInScope(t *testing.T) {
	tests := []struct {
		name   string
		groups []string
		group  string
		want   bool
	}{
		{name: "an empty scope matches everything", groups: nil, group: "sales", want: true},
		{name: "an empty slice matches everything", groups: []string{}, group: "sales", want: true},
		{name: "an empty scope matches the empty group", groups: nil, group: "", want: true},
		{name: "a member of the scope matches", groups: []string{"sales", "support"}, group: "support", want: true},
		{name: "a non-member does not match", groups: []string{"sales", "support"}, group: "engineering", want: false},
		{name: "the empty group is not in a non-empty scope", groups: []string{"sales"}, group: "", want: false},
		{name: "the comparison does not fold case", groups: []string{"Sales"}, group: "sales", want: false},
		{name: "the comparison does not trim space", groups: []string{"sales"}, group: " sales", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := GroupInScope(tt.groups, tt.group); got != tt.want {
				t.Errorf("GroupInScope(%v, %q) = %v, want %v", tt.groups, tt.group, got, tt.want)
			}
		})
	}
}

// TestNewGroupScopedVerifier_NotRequired verifies the decorator returns the inner verifier
// unchanged when the requirement is off, which is what lets main.go wrap unconditionally.
func TestNewGroupScopedVerifier_NotRequired(t *testing.T) {
	inner := &stubVerifier{claims: &Claims{ClientID: "client-1"}}

	if got := NewGroupScopedVerifier(inner, false); got != Verifier(inner) {
		t.Fatalf("expected the inner verifier back unchanged, got %T", got)
	}
}

// TestGroupScopedVerifier_RejectsUnscopedToken verifies the failure this decorator exists for: a
// token that verifies perfectly but carries no group scope is refused, rather than silently
// granting the whole store.
func TestGroupScopedVerifier_RejectsUnscopedToken(t *testing.T) {
	tests := []struct {
		name   string
		groups []string
	}{
		{name: "no groups claim at all", groups: nil},
		{name: "an empty groups claim", groups: []string{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inner := &stubVerifier{claims: &Claims{
				RegisteredClaims: jwt.RegisteredClaims{ID: "jti-1"},
				ClientID:         "client-1",
				Groups:           tt.groups,
			}}

			claims, err := NewGroupScopedVerifier(inner, true).Verify("any-token")
			if err == nil {
				t.Fatalf("expected an unscoped token to be rejected, got claims %+v", claims)
			}

			// The rejection must be the same generic error an invalid token gets, so a caller cannot
			// probe the deployment's scoping policy by watching which of its tokens are refused.
			if err.Error() != "auth: token invalid" {
				t.Errorf("expected the generic invalid-token error, got %q", err.Error())
			}

			if claims != nil {
				t.Errorf("expected no claims alongside the rejection, got %+v", claims)
			}
		})
	}
}

// TestGroupScopedVerifier_AcceptsScopedToken verifies a token carrying a scope passes through with
// its claims intact.
func TestGroupScopedVerifier_AcceptsScopedToken(t *testing.T) {
	inner := &stubVerifier{claims: &Claims{ClientID: "client-1", Groups: []string{"sales"}}}

	claims, err := NewGroupScopedVerifier(inner, true).Verify("any-token")
	if err != nil {
		t.Fatalf("Verify: %s", err)
	}

	if len(claims.Groups) != 1 || claims.Groups[0] != "sales" {
		t.Errorf("expected the scope to survive the decorator, got %v", claims.Groups)
	}
}

// TestGroupScopedVerifier_PropagatesInnerFailure verifies the decorator returns the inner
// verifier's error untouched rather than replacing it with its own, so a genuinely invalid token is
// still reported by whichever scheme rejected it.
func TestGroupScopedVerifier_PropagatesInnerFailure(t *testing.T) {
	wantErr := fmt.Errorf("inner refused")

	claims, err := NewGroupScopedVerifier(&stubVerifier{err: wantErr}, true).Verify("any-token")
	if err != wantErr {
		t.Fatalf("expected the inner error back unchanged, got %v", err)
	}

	if claims != nil {
		t.Errorf("expected no claims alongside the rejection, got %+v", claims)
	}
}

// TestGroupScopedVerifier_OverHMAC verifies the requirement composes with a real verification
// scheme rather than only with a stub - the property that makes it a decorator at all.
func TestGroupScopedVerifier_OverHMAC(t *testing.T) {
	const secret = "test-secret"

	inner, err := NewHMACVerifier(HMACConfig{LegacySecret: secret})
	if err != nil {
		t.Fatalf("NewHMACVerifier: %s", err)
	}

	verifier := NewGroupScopedVerifier(inner, true)

	unscoped, err := MintToken(MintRequest{Secret: secret, ClientID: "client-1", TTL: time.Hour})
	if err != nil {
		t.Fatalf("MintToken: %s", err)
	}

	if _, err := verifier.Verify(unscoped); err == nil {
		t.Error("expected an unscoped HMAC token to be rejected")
	}

	scoped, err := MintToken(MintRequest{
		Secret:   secret,
		ClientID: "client-1",
		TTL:      time.Hour,
		Groups:   []string{"sales"},
	})
	if err != nil {
		t.Fatalf("MintToken: %s", err)
	}

	claims, err := verifier.Verify(scoped)
	if err != nil {
		t.Fatalf("Verify: %s", err)
	}

	if len(claims.Groups) != 1 || claims.Groups[0] != "sales" {
		t.Errorf("expected the scope [sales], got %v", claims.Groups)
	}
}
