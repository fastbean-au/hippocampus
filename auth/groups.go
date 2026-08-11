package auth

import (
	"fmt"
	"slices"

	log "github.com/sirupsen/logrus"
)

// Group scoping.
//
// A token may carry a set of group labels (Claims.Groups) naming the slice of the store it is
// allowed to see. This file holds the two pieces that decision needs everywhere it is made: the
// membership test, and the verifier decorator that enforces every token carry a scope at all.
//
// The enforcement itself is not here. It cannot be: a read filtered by group and a read of named
// ids need different treatment (a predicate versus a check on what came back), so it lives at the
// RPC layer beside each handler, declared and drift-guarded in hippocampus/scope.go.

// GroupInScope reports whether group falls inside the scope named by groups.
//
// An empty scope means unscoped and matches everything, so a caller must NOT use this function to
// decide whether a request is scoped at all - GroupsFromContext's bool answers that. Passing an
// unscoped caller's (empty) groups here returns true for every group, which is the correct answer
// once it is established that the caller is unscoped and the wrong one if it has not been.
//
// The comparison is exact and byte-for-byte, deliberately: it must agree with how the store
// compares group_name, which is byte-for-byte on all three dialects (MySQL needs an explicit
// COLLATE utf8mb4_bin for that, precisely because its default collation would otherwise fold case).
// Folding case here would let a token scoped to "Sales" read rows written under "sales" that no
// query built from the same scope would return - the scope and the predicate would disagree, which
// is the one property this whole mechanism cannot afford to lose.
func GroupInScope(groups []string, group string) bool {
	if len(groups) == 0 {
		return true
	}

	return slices.Contains(groups, group)
}

// groupScopedVerifier decorates an inner Verifier with the requirement that a token name a group
// scope, so the requirement composes with any verification scheme (hmac or idp) exactly as
// revokingVerifier does for revocation.
type groupScopedVerifier struct {
	inner Verifier
}

// NewGroupScopedVerifier wraps inner so a token that verifies but carries no group scope is
// rejected (auth.requireGroupScope). It returns inner unchanged when require is false, so callers
// can wrap unconditionally.
//
// It exists because an unscoped token is not a lesser privilege but the greatest one - it sees the
// whole store - so a deployment that has partitioned its store wants the absence of a scope to be
// an error rather than a silent grant of everything. That is the failure mode this guards: an
// identity provider misconfigured to stop emitting the groups claim would otherwise hand every
// caller the entire store, and every request would succeed while doing it.
func NewGroupScopedVerifier(inner Verifier, require bool) Verifier {
	if !require {
		return inner
	}

	return &groupScopedVerifier{inner: inner}
}

// Verify runs the inner verification, then rejects a token carrying no group scope. The rejection
// reuses the same generic error an invalid token gets, so a caller cannot probe the deployment's
// scoping policy by watching which of its tokens are refused; the specific reason is logged at
// Debug for operators, matching revokingVerifier.
func (v *groupScopedVerifier) Verify(token string) (*Claims, error) {
	claims, err := v.inner.Verify(token)
	if err != nil {
		return nil, err
	}

	if len(claims.Groups) == 0 {
		log.Debugf("token rejected: no group scope and auth.requireGroupScope is set (jti %q, client %q)", claims.ID, claims.ClientID)

		return nil, fmt.Errorf("auth: token invalid")
	}

	return claims, nil
}
