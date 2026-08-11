package auth

import (
	"github.com/golang-jwt/jwt/v5"
)

// Claims embeds the standard registered claims (exp, iat, ...) plus a client identifier, the
// bearer's roles, and the groups the bearer is scoped to. ClientID identifies the bearer for
// request logging and per-client revocation.
// Roles carries the authorisation tiers (reader/writer/admin) the token grants; it is resolved to
// an effective tier by the Authoriser (see authz.go). Both verifiers parse straight into this
// struct, so a token carrying a top-level "roles" claim populates Roles without any extra work;
// an identity provider that publishes roles under a differently-named claim is handled by
// auth.roleClaim (see JWKSConfig.RoleClaim).
type Claims struct {
	jwt.RegisteredClaims

	ClientID string   `json:"client_id"`
	Roles    []string `json:"roles"`

	// Groups scopes the bearer to a subset of the store: it names the group labels
	// (Memory.group / Event.group) whose records this token may read and write. It is orthogonal
	// to Roles - the tier says what kinds of operation are permitted, Groups says which records
	// they may touch - so the two are resolved independently.
	//
	// An EMPTY Groups means unscoped: the token sees the whole store, which is what every token
	// did before group scoping existed and is therefore what an existing deployment keeps doing.
	// A deployment that wants every token bound wraps its verifier with NewGroupScopedVerifier
	// (auth.requireGroupScope), which refuses a token that arrives with none.
	//
	// This is a SOFT partition, not tenancy: the decay dynamics (capacity pressure, eviction
	// ranking, the significance registry, the sleep cadence) remain store-global and shared, so a
	// busy group still influences what another group forgets. Hard isolation is the deployment
	// model's job - one instance and one database per tenant. See docs/operations.md.
	//
	// Like Roles, an identity provider that publishes the scope under a differently-named claim is
	// handled by auth.groupsClaim (see JWKSConfig.GroupsClaim).
	Groups []string `json:"groups"`
}
