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
