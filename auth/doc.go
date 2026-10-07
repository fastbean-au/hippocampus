// Package auth authenticates and authorises callers of the Hippocampus service.
//
// Authentication verifies a JWT bearer token, either HS256 against shared secrets (HMACVerifier) or
// RS256 against an identity provider's JWKS (JWKSVerifier), optionally decorated with a revocation
// list or a requirement for group scope. Authorisation maps the token's roles to a tier - reader,
// writer or admin - and checks it against one policy table (authz.go) that both transports, gRPC
// and the HTTP gateway, enforce. Group scope (groups.go) is the second axis: which records a token
// may reach.
//
// The package holds no reference to the server or the store. Enforcement on stored records - what a
// group scope means for each RPC - is the hippocampus package's.
package auth
