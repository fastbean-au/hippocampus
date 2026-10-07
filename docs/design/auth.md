# Authentication and authorisation

This is the design record moved out of CLAUDE.md verbatim (TODO-3 item 176): why the code is the way it
is, the alternatives that were rejected, and the incidents behind the guards. CLAUDE.md keeps the short
version and links here.

## `auth/`

`auth/` — JWT bearer-token support, self-contained (no `*hippocampus.Server`, no DB). `Verifier`
is an interface (`Verify(token string) (*Claims, error)`) with two implementations, both
restricted to a single algorithm via `jwt.WithValidMethods` (so a token can never select its own)
and both requiring an `exp` claim via `jwt.WithExpirationRequired` (golang-jwt only validates `exp`
when present, so this stops an expiry-less token verifying forever): `HMACVerifier` (HS256; built from an `HMACConfig` of a legacy single `signingSecret` plus
any number of `kid`-tagged `signingKeys` — every key verifies, so a new secret rotates in while
old tokens still verify; a kid-less token uses the legacy secret, an unknown kid is rejected) and
`JWKSVerifier` (`jwks.go`; RS256 against an
identity provider's JWKS — endpoint from `auth.jwksUrl` or OIDC discovery via `auth.issuer`,
keys cached by kid, re-fetched lazily on `auth.jwksRefreshIntervalSeconds` plus one
cooldown-limited forced re-fetch on an unknown kid so IdP key rotation verifies on first
sight; `iss`/`aud` enforced when configured; the initial fetch failing fails construction,
later outages leave cached keys serving). `Claims` embeds `jwt.RegisteredClaims` (including the
`jti`) plus `ClientID` and `Roles`. `MintToken` (taking a `MintRequest`) is a plain function, not part of
`Verifier`, used by both the `--mint-token` CLI mode and tests; it is HMAC-only (an IdP mints
its own tokens), always stamps a random `jti`, and sets a `kid` header when minting under a
keyed secret. `revocation.go` adds `RevocationList` (a JSON file of revoked `jti`s and
`client_id`s — the latter optionally only before an `issuedBefore` cutoff — reloaded on the
file's mtime every `auth.revocationRefreshSeconds`, failing startup on a bad initial load but
keeping the last good set on a bad reload) and `NewRevokingVerifier`, a decorator that checks
the list after any inner `Verifier` succeeds, so revocation composes with `idp` as well as
`hmac`. All viper reads stay in main.go: `hmacConfigFromViper`/`resolveMintKey` there build the
`HMACConfig` and pick the minting key.
`UnaryServerInterceptor` and `HTTPMiddleware` are the two enforcement adapters — both are
needed because the HTTP gateway calls `hipo` directly and never passes through the gRPC
interceptor chain. Both scope themselves so Hippocampus RPCs require a token but health surfaces
(`grpc.health.v1.Health`, `/healthz`) never do — the gRPC side by a `/hippocampus.v1.Hippocampus/` prefix
check (mirroring `InterceptorBlockWhenPurgeInProgress`), the HTTP side by an explicit open-path
allow-list (closed by default, so newly added endpoints are protected without remembering to
update anything). On a successful verify both adapters stash the `*Claims` in the request context
(`context.go`: `ContextWithClaims`/`ClaimsFromContext`/`ClientIDFromContext`), which the two
loggers read to attach a `client_id` to request logs (a per-client audit trail).
Authorisation (`authz.go`) layers roles on top of that authentication: a `Tier` hierarchy
(`reader` ⊂ `writer` ⊂ `admin`) and a single `policies` table assigning every RPC a minimum
tier, from which `NewAuthorizer` derives both a gRPC method map and a gateway verb+path map — so
the two transports enforce one policy from one source (a drift-guard test asserts every RPC in
the service descriptor has a policy). `Authorizer.UnaryServerInterceptor` (chained right after
the auth interceptor) and `Authorizer.GatewayMiddleware` (a grpc-gateway `runtime.WithMiddlewares`
middleware keyed on the matched `runtime.HTTPPattern`, normalised — `RPCMethod` is not yet set
pre-handler) are the two enforcement adapters; both resolve the highest tier the verified
`Claims.Roles` grant (default-closed: a token resolving to no known tier is denied every RPC) and
stash it via `ContextWithTier`/`TierFromContext`. Roles come from the `roles` claim (or
`auth.roleClaim` for an IdP that names it differently), mapped to tiers by `auth.roleMapping`;
`--mint-token --role` stamps them. The authorizer is built (in main.go) only when auth is enabled.
The stashed tier drives two things: `hippocampus.Server.mayReinforce` (the reader-recall gate —
`auth.readerRecallReinforces` decides whether a reader's `RecallMemories`/reinforcing
`SearchMemories` actually reinforces or is downgraded to a plain read) and the `WhoAmI` RPC, which
reports the caller's effective tier so the web console can hide the write controls it may not use.
**Group scoping** (`groups.go`) is the orthogonal axis: a tier says what a caller may _do_, a scope
says which records they may do it _to_. `Claims.Groups` (from the `groups` claim, or
`auth.groupsClaim` for an IdP naming it differently) names the `group` labels a token may reach;
`GroupsFromContext` returns `(groups, bound)` and **callers must branch on the bool, never on the
slice's length** — an empty scope means the _whole store_, so reading it as "scoped to nothing"
empties every read on an unauthenticated instance and reading the reverse hands a bound token
everything. `GroupInScope` is the membership test, exact and byte-for-byte so it agrees with how
the store compares `group_name` (MySQL needs its explicit `COLLATE utf8mb4_bin` for that).
`NewGroupScopedVerifier` (`auth.requireGroupScope`) is a decorator in the `NewRevokingVerifier`
mould, refusing a token that carries no scope — an unscoped token being the _most_ privileged
shape there is, not the least. Enforcement is not here; see `hippocampus/scope.go`.
