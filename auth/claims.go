package auth

import (
	"github.com/golang-jwt/jwt/v5"
)

// Claims embeds the standard registered claims (exp, iat, ...) plus a client identifier and the
// bearer's roles. ClientID identifies the bearer for request logging and per-client revocation.
// Roles carries the authorisation tiers (reader/writer/admin) the token grants; it is resolved to
// an effective tier by the Authoriser (see authz.go). Both verifiers parse straight into this
// struct, so a token carrying a top-level "roles" claim populates Roles without any extra work;
// an identity provider that publishes roles under a differently-named claim is handled by
// auth.roleClaim (see JWKSConfig.RoleClaim).
type Claims struct {
	jwt.RegisteredClaims

	ClientID string   `json:"client_id"`
	Roles    []string `json:"roles"`
}
