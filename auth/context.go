package auth

import (
	"context"
)

// claimsContextKey is the unexported key type under which verified Claims are stashed in a request
// context. An unexported type guarantees no other package can collide with (or read) the key.
type claimsContextKey struct{}

// ContextWithClaims returns a copy of ctx carrying the verified claims, so downstream code (logging
// middleware, audit) can attribute the request to the authenticated client. Both enforcement
// adapters call it after a successful Verify.
func ContextWithClaims(ctx context.Context, claims *Claims) context.Context {
	return context.WithValue(ctx, claimsContextKey{}, claims)
}

// ClaimsFromContext returns the verified claims stashed by ContextWithClaims, or nil when the
// request was not authenticated (auth disabled, or an open path that never runs the verifier).
func ClaimsFromContext(ctx context.Context) *Claims {
	claims, _ := ctx.Value(claimsContextKey{}).(*Claims)

	return claims
}

// ClientIDFromContext returns the authenticated client id for the request, or "" when the request
// carried no verified claims. It is the convenience the logging middleware uses to add a client_id
// field without reaching into Claims itself.
func ClientIDFromContext(ctx context.Context) string {
	if claims := ClaimsFromContext(ctx); claims != nil {
		return claims.ClientID
	}

	return ""
}

// GroupsFromContext returns the group scope carried by the request's verified claims, and whether
// the caller is bound to one at all. It reads through ClaimsFromContext rather than a context key of
// its own - the scope arrives as a claim, so stashing it separately would be a second copy of a
// value that is already on the context, free to fall out of step with it.
//
// The bool is what callers must branch on, not len(groups): false means the request carries no
// scope and may see the whole store (authentication is disabled, or the token is unscoped), while
// true with a non-empty slice means the request is restricted to exactly those groups. The two
// cannot be told apart from the slice alone, and defaulting the wrong way in either direction is a
// bug - "no scope" read as "scope to nothing" makes an unauthenticated instance return nothing, and
// "scope to nothing" read as "no scope" hands a bound token the whole store.
//
// A verified token whose scope is empty is reported as unbound. A deployment that considers that a
// misconfiguration rejects it at verification time instead (NewGroupScopedVerifier), which keeps
// this function's contract simple: by the time it is called, an empty scope has already been
// allowed by policy.
func GroupsFromContext(ctx context.Context) ([]string, bool) {
	claims := ClaimsFromContext(ctx)

	if claims == nil || len(claims.Groups) == 0 {
		return nil, false
	}

	return claims.Groups, true
}

// tierContextKey is the unexported key type under which the caller's resolved authorisation tier is
// stashed. Like claimsContextKey, an unexported type prevents collisions from other packages.
type tierContextKey struct{}

// ContextWithTier returns a copy of ctx carrying the caller's resolved authorisation tier. Both
// authorisation adapters (the gRPC interceptor and the gateway middleware) call it once they have
// decided the request is allowed, so downstream handlers can consult the tier without re-resolving
// roles - the recall/search reinforcement gate in the hippocampus package reads it this way.
func ContextWithTier(ctx context.Context, tier Tier) context.Context {
	return context.WithValue(ctx, tierContextKey{}, tier)
}

// TierFromContext returns the resolved authorisation tier stashed by ContextWithTier. The bool is
// false when no tier is present - authorisation was not run (auth disabled), so callers should
// treat that as "unconstrained" rather than "denied", matching the behaviour of a service with no
// authentication configured.
func TierFromContext(ctx context.Context) (Tier, bool) {
	tier, ok := ctx.Value(tierContextKey{}).(Tier)

	return tier, ok
}
