package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/golang-jwt/jwt/v5"
	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Tier is an ordered authorisation level. The tiers nest - writer includes every reader-level RPC,
// admin includes every writer-level RPC - so a required tier is satisfied by any tier at or above
// it (tier >= required). Zero is deliberately not a valid tier: a token whose roles resolve to no
// known tier has tierNone and is denied every Hippocampus RPC (default-closed).
type Tier int

const (
	tierNone Tier = iota
	// TierReader may read: the GET RPCs, plus recall/search (whose reinforcement side effect is
	// separately gated - see the hippocampus package's reinforcement gate).
	TierReader
	// TierWriter may mutate: store/update/delete/import/summary, and everything TierReader may do.
	TierWriter
	// TierAdmin may run administrative/destructive RPCs (Purge, Sleep, Clear, Transfer, Export),
	// and everything TierWriter may do.
	TierAdmin
)

// String renders a tier as the role name used in tokens and config, so it can go straight into a
// log field. An out-of-range value renders as "none".
func (t Tier) String() string {
	switch t {

	case TierReader:
		return "reader"

	case TierWriter:
		return "writer"

	case TierAdmin:
		return "admin"

	default:
		return "none"
	}
}

// TierNames returns every tier name, lowest first, as it is written in a token's roles claim and in
// configuration. It exists so configuration that is itself keyed by tier - the per-tier rate limits
// are the first - can enumerate and validate against the same set the authoriser resolves from,
// rather than against a second copy of the names that could fall out of step with it.
func TierNames() []string {
	return []string{TierReader.String(), TierWriter.String(), TierAdmin.String()}
}

// parseTier maps a role/tier name (as written in a token's roles claim or in auth.roleMapping) to a
// Tier. The bool is false for any unrecognised name, so an unknown role never silently grants
// access.
func parseTier(name string) (Tier, bool) {
	switch strings.ToLower(strings.TrimSpace(name)) {

	case "reader":
		return TierReader, true

	case "writer":
		return TierWriter, true

	case "admin":
		return TierAdmin, true

	default:
		return tierNone, false
	}
}

// rpcPolicy is the authorisation policy for a single RPC: the minimum tier a caller needs, plus the
// HTTP verb and (capture-normalised) path the gateway exposes it as. It is the one place a new RPC
// is assigned a tier; both the gRPC method map and the gateway route map are derived from it, so
// the two transports can never enforce different policies.
type rpcPolicy struct {
	tier       Tier
	httpMethod string
	httpPath   string
}

// policies is the authoritative per-RPC authorisation table, keyed by bare gRPC method name (as it
// appears in contract.Hippocampus_ServiceDesc). httpPath is the google.api.http path with every
// capture segment normalised to "*" (see normalisePattern), so it matches what the gateway
// middleware computes from the matched pattern at request time. A drift-guard test asserts every
// RPC in the service descriptor has an entry here.
var policies = map[string]rpcPolicy{
	// reads
	"WhoAmI":                     {TierReader, http.MethodGet, "/v1/whoami"},
	"GetEvents":                  {TierReader, http.MethodGet, "/v1/events"},
	"GetEventById":               {TierReader, http.MethodGet, "/v1/events/*"},
	"GetMemories":                {TierReader, http.MethodGet, "/v1/memories"},
	"RecallMemories":             {TierReader, http.MethodPost, "/v1/memories/recall"},
	"SearchMemories":             {TierReader, http.MethodPost, "/v1/memories/search"},
	"GetSummarisationCandidates": {TierReader, http.MethodGet, "/v1/summarisation/candidates"},

	// Explaining a memory is a read of that memory, not of the store: unlike the preview below it
	// enumerates nothing, answering only about ids the caller already holds and could read in full
	// through GetMemories at this same tier. What it adds beyond them is operational rather than
	// content - the capacity pressure and the threshold it scales - and withholding those would
	// leave a reader shown a value with nothing to measure it against, which is the whole point of
	// asking.
	"ExplainConsolidation": {TierReader, http.MethodPost, "/v1/consolidation/explain"},

	// writes
	"StoreEvent":                 {TierWriter, http.MethodPost, "/v1/events"},
	"EndEvent":                   {TierWriter, http.MethodPost, "/v1/events/*/end"},
	"UpdateEventSignificance":    {TierWriter, http.MethodPatch, "/v1/events/*/significance"},
	"MergeEvents":                {TierWriter, http.MethodPost, "/v1/events/merge"},
	"DeleteEvent":                {TierWriter, http.MethodDelete, "/v1/events/*"},
	"StoreMemory":                {TierWriter, http.MethodPost, "/v1/memories"},
	"UpdateMemory":               {TierWriter, http.MethodPatch, "/v1/memories/*"},
	"DeleteMemories":             {TierWriter, http.MethodPost, "/v1/memories/delete"},
	"ReplaceMemoriesWithSummary": {TierWriter, http.MethodPost, "/v1/events/*/summary"},
	"SummariseMemories":          {TierWriter, http.MethodPost, "/v1/events/*/summarise"},
	"Import":                     {TierWriter, http.MethodPost, "/v1/import"},
	"ImportBatch":                {TierWriter, http.MethodPost, "/v1/import/batch"},

	// admin
	"Purge": {TierAdmin, http.MethodPost, "/v1/purge"},
	"Sleep": {TierAdmin, http.MethodPost, "/v1/sleep"},

	// A dry run deletes nothing, but it enumerates ids, groups and significances from across the
	// whole store - more than a reader is otherwise shown by any single call - so it is assigned
	// the admin tier alongside the cycle it describes rather than the reader tier its read-only
	// nature would suggest. Being a separate RPC from Sleep is what leaves that free to be
	// reconsidered: the preview can be lowered a tier without also handing out the ability to
	// trigger a real cycle.
	"PreviewConsolidation": {TierAdmin, http.MethodGet, "/v1/sleep/preview"},

	"Export":   {TierAdmin, http.MethodPost, "/v1/export"},
	"Transfer": {TierAdmin, http.MethodPost, "/v1/transfer"},
	"Clear":    {TierAdmin, http.MethodPost, "/v1/clear"},
}

// routeRPCs inverts the policy table: "<VERB> <normalised path>" -> bare gRPC method name. It is
// derived at init from the same entries the tier maps are, so it can never name a different set of
// RPCs than the service actually serves.
var routeRPCs = func() map[string]string {
	out := make(map[string]string, len(policies))

	for method, p := range policies {
		out[p.httpMethod+" "+p.httpPath] = method
	}

	return out
}()

// RouteRPC maps a matched gateway route - an HTTP verb plus the raw path template of the pattern the
// mux matched - back to the bare gRPC method name serving it. It exists so per-request metrics can
// label a gateway request with the same RPC name the gRPC interceptor uses, letting one dashboard
// panel sum a call across both transports; the policy table is already the single, drift-guarded
// source for the RPC <-> route correspondence, so deriving the name from it beats maintaining a
// second copy that could disagree. Unlike the tier maps this says nothing about authorisation and is
// safe to call whether or not auth is enabled. The bool is false for a route with no policy entry,
// which callers must treat as an unknown RPC rather than inventing a label from the path (a raw path
// carries ids, and so unbounded metric cardinality).
func RouteRPC(httpMethod string, pattern string) (string, bool) {
	rpc, ok := routeRPCs[httpMethod+" "+normalisePattern(pattern)]

	return rpc, ok
}

// captureSegment matches a grpc-gateway path capture as Pattern.String renders it (e.g. "{id=*}"),
// so normalisePattern can reduce it to the placeholder the policy table uses.
var captureSegment = regexp.MustCompile(`\{[^}]*\}`)

// normalisePattern reduces a google.api.http path template to the form used as a gateway policy
// key: every capture segment ("{id=*}", "{event_id=*}") becomes "*". This keeps the policy keys
// independent of capture variable names and of grpc-gateway's exact rendering, while staying unique
// per route (no two RPCs share a verb and normalised path).
func normalisePattern(pattern string) string {
	return captureSegment.ReplaceAllString(pattern, "*")
}

// Authoriser decides whether an authenticated caller may invoke a given RPC. It holds the derived
// per-method and per-route tier maps plus the role-name -> tier resolution, and exposes the two
// enforcement adapters (gRPC interceptor, gateway middleware). It is immutable after construction,
// so a single instance is shared by both transports.
type Authoriser struct {
	methodTiers  map[string]Tier // "/hippocampus.v1.Hippocampus/<Method>" -> required tier
	gatewayTiers map[string]Tier // "<VERB> <normalised path>"    -> required tier
	roleTiers    map[string]Tier // role name (lower-cased)        -> granted tier
}

// NewAuthoriser builds an Authoriser. The role->tier resolution starts from the identity mapping
// (reader/writer/admin name themselves), which roleMapping then extends: an identity provider that
// tags tokens with its own group names (e.g. "hippo-ops": "admin") maps them onto tiers here. A
// mapping whose target is not a known tier fails construction rather than silently denying every
// bearer of that group.
func NewAuthoriser(roleMapping map[string]string) (*Authoriser, error) {
	log.Trace("func() auth.NewAuthoriser")

	roleTiers := map[string]Tier{
		"reader": TierReader,
		"writer": TierWriter,
		"admin":  TierAdmin,
	}

	for role, tierName := range roleMapping {
		tier, ok := parseTier(tierName)
		if !ok {
			return nil, fmt.Errorf("auth: roleMapping[%q] names unknown tier %q (expected reader, writer, or admin)", role, tierName)
		}

		roleTiers[strings.ToLower(strings.TrimSpace(role))] = tier
	}

	methodTiers := make(map[string]Tier, len(policies))
	gatewayTiers := make(map[string]Tier, len(policies))

	for method, p := range policies {
		methodTiers[hippocampusServicePrefix+method] = p.tier
		gatewayTiers[p.httpMethod+" "+p.httpPath] = p.tier
	}

	return &Authoriser{
		methodTiers:  methodTiers,
		gatewayTiers: gatewayTiers,
		roleTiers:    roleTiers,
	}, nil
}

// effectiveTier resolves a token's roles to the highest tier they grant. The bool is false when no
// role resolves to a known tier (including a nil claims or an empty roles list), which the callers
// treat as denied - the default-closed posture.
func (a *Authoriser) effectiveTier(claims *Claims) (Tier, bool) {
	if claims == nil {
		return tierNone, false
	}

	best := tierNone
	found := false

	for _, role := range claims.Roles {
		tier, ok := a.roleTiers[strings.ToLower(strings.TrimSpace(role))]
		if !ok {
			continue
		}

		found = true

		if tier > best {
			best = tier
		}
	}

	return best, found
}

// UnaryServerInterceptor returns the gRPC authorisation interceptor. It must run after
// UnaryServerInterceptor (authentication) so the verified claims are on the context; it scopes
// itself to Hippocampus RPCs exactly as the auth interceptor does, leaving the health service
// reachable without a role. On success it stashes the resolved tier on the context so the
// reinforcement gate downstream can read it without re-resolving roles.
func (a *Authoriser) UnaryServerInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context,
		req any,
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (any, error) {
		if !strings.HasPrefix(info.FullMethod, hippocampusServicePrefix) {
			return handler(ctx, req)
		}

		tier, allowed := a.allow(a.methodTiers[info.FullMethod], hasEntry(a.methodTiers, info.FullMethod), ClaimsFromContext(ctx))
		if !allowed {
			log.Tracef("rejecting request - tier %s insufficient for %s", tier, info.FullMethod)

			return nil, status.Error(codes.PermissionDenied, "insufficient role")
		}

		return handler(ContextWithTier(ctx, tier), req)
	}
}

// GatewayMiddleware returns the grpc-gateway authorisation middleware. It runs after routing (so
// runtime.HTTPPattern is populated) and inside HTTPMiddleware (so the verified claims are on the
// request context). The matched pattern plus the HTTP verb identify the RPC, which the derived
// gatewayTiers map turns into a required tier. On success it stashes the resolved tier on the
// request context for the reinforcement gate, mirroring the gRPC interceptor.
func (a *Authoriser) GatewayMiddleware() runtime.Middleware {
	return func(next runtime.HandlerFunc) runtime.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request, pathParams map[string]string) {
			pattern, ok := runtime.HTTPPattern(r.Context())
			if !ok {
				// A matched Hippocampus route always carries its pattern; its absence is anomalous,
				// so fail closed rather than guess a tier.
				log.Trace("rejecting request - no route pattern on context")
				forbidden(w)

				return
			}

			key := r.Method + " " + normalisePattern(pattern.String())

			required, known := a.gatewayTiers[key]

			tier, allowed := a.allow(required, known, ClaimsFromContext(r.Context()))
			if !allowed {
				log.Tracef("rejecting request - tier %s insufficient for %s", tier, key)
				forbidden(w)

				return
			}

			next(w, r.WithContext(ContextWithTier(r.Context(), tier)), pathParams)
		}
	}
}

// allow is the shared decision both adapters use: the caller is allowed only when the route is a
// known Hippocampus RPC (known), the token resolves to some tier (found), and that tier meets the
// route's requirement. It returns the resolved tier so a successful caller's tier can be stashed on
// the context.
func (a *Authoriser) allow(required Tier, known bool, claims *Claims) (Tier, bool) {
	tier, found := a.effectiveTier(claims)

	if !known || !found || tier < required {
		return tier, false
	}

	return tier, true
}

// hasEntry reports whether m holds key, used so the interceptor can tell "unmapped RPC" (deny)
// apart from a genuine tierNone requirement (which no policy uses).
func hasEntry(m map[string]Tier, key string) bool {
	_, ok := m[key]

	return ok
}

// forbidden writes a 403 response in the same small JSON style as the authentication middleware's
// 401, so a caller that authenticated but lacks the role gets a clear, distinct status.
func forbidden(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": "forbidden"})
}

// rolesFromClaim extracts a role list from a differently-named claim, for identity providers that
// do not publish roles under the standard top-level "roles" (auth.roleClaim). It parses the token
// unverified - the signature was already checked by the caller, and this only reads a claim value,
// never makes a trust decision - and accepts either a JSON array of strings or a single string.
//
// The claim name is resolved literally first, so a provider whose key itself contains dots or
// slashes (Auth0 namespaces its roles under a URI such as "https://example.com/roles") matches as a
// plain top-level key. Only when no literal key matches is the name treated as a dotted path and
// walked through nested objects, so a nested claim (Keycloak's "realm_access.roles") also resolves.
func rolesFromClaim(token string, claimName string) []string {
	var raw jwt.MapClaims

	if _, _, err := jwt.NewParser().ParseUnverified(token, &raw); err != nil {
		return nil
	}

	return coerceRoles(resolveClaim(raw, claimName))
}

// resolveClaim looks up claimName in the parsed claims, preferring a literal top-level key and
// falling back to a dotted-path walk through nested objects when no literal key is present. It
// returns nil when neither resolves.
func resolveClaim(raw jwt.MapClaims, claimName string) any {
	if v, ok := raw[claimName]; ok {
		return v
	}

	if !strings.Contains(claimName, ".") {
		return nil
	}

	var current any = map[string]any(raw)

	for segment := range strings.SplitSeq(claimName, ".") {
		object, ok := current.(map[string]any)
		if !ok {
			return nil
		}

		current, ok = object[segment]

		if !ok {
			return nil
		}
	}

	return current
}

// coerceRoles turns a resolved claim value into a role list, accepting a single string or a JSON
// array and keeping only the string members of an array. Any other type yields no roles.
func coerceRoles(value any) []string {
	switch v := value.(type) {

	case string:
		return []string{v}

	case []any:
		roles := make([]string, 0, len(v))

		for _, item := range v {
			if s, ok := item.(string); ok {
				roles = append(roles, s)
			}
		}

		return roles

	default:
		return nil
	}
}
