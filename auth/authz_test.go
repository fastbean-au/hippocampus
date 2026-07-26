package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/fastbean-au/hippocampus/contract"
)

// TestPoliciesCoverEveryRPC is the drift guard: every RPC in the generated service descriptor must
// have an authorization policy, so a newly added RPC without a tier fails the build's tests rather
// than silently defaulting to deny (or, worse, slipping through).
func TestPoliciesCoverEveryRPC(t *testing.T) {
	for _, m := range contract.Hippocampus_ServiceDesc.Methods {
		if _, ok := policies[m.MethodName]; !ok {
			t.Errorf("RPC %q has no authorization policy in policies", m.MethodName)
		}
	}

	if len(policies) != len(contract.Hippocampus_ServiceDesc.Methods) {
		t.Errorf("policies has %d entries but the service descriptor has %d methods - a policy names a non-existent RPC",
			len(policies), len(contract.Hippocampus_ServiceDesc.Methods))
	}
}

func TestParseTier(t *testing.T) {
	cases := map[string]struct {
		want Tier
		ok   bool
	}{
		"reader":  {TierReader, true},
		"WRITER":  {TierWriter, true},
		" admin ": {TierAdmin, true},
		"root":    {tierNone, false},
		"":        {tierNone, false},
	}

	for in, want := range cases {
		got, ok := parseTier(in)

		if got != want.want || ok != want.ok {
			t.Errorf("parseTier(%q) = (%v, %v), want (%v, %v)", in, got, ok, want.want, want.ok)
		}
	}
}

func TestEffectiveTier(t *testing.T) {
	a, err := NewAuthorizer(map[string]string{"hippo-ops": "admin"})
	if err != nil {
		t.Fatalf("NewAuthorizer: %s", err)
	}

	cases := []struct {
		name  string
		roles []string
		want  Tier
		found bool
	}{
		{"single reader", []string{"reader"}, TierReader, true},
		{"highest of several", []string{"reader", "admin", "writer"}, TierAdmin, true},
		{"mapped group", []string{"hippo-ops"}, TierAdmin, true},
		{"unknown only", []string{"root"}, tierNone, false},
		{"empty", nil, tierNone, false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, found := a.effectiveTier(&Claims{Roles: c.roles})

			if got != c.want || found != c.found {
				t.Fatalf("effectiveTier(%v) = (%v, %v), want (%v, %v)", c.roles, got, found, c.want, c.found)
			}
		})
	}

	if _, found := a.effectiveTier(nil); found {
		t.Fatalf("effectiveTier(nil) should not resolve a tier")
	}
}

func TestNewAuthorizerRejectsUnknownMappingTier(t *testing.T) {
	if _, err := NewAuthorizer(map[string]string{"group": "superuser"}); err == nil {
		t.Fatalf("expected an error for an unknown mapped tier")
	}
}

// TestInterceptorTierMatrix drives the gRPC authorization interceptor with a representative RPC of
// each tier under each role, asserting the reader<writer<admin nesting is enforced.
func TestInterceptorTierMatrix(t *testing.T) {
	a, err := NewAuthorizer(nil)
	if err != nil {
		t.Fatalf("NewAuthorizer: %s", err)
	}

	interceptor := a.UnaryServerInterceptor()

	reached := false
	handler := func(ctx context.Context, req any) (any, error) {
		reached = true

		return "ok", nil
	}

	// method -> its required tier, one representative per tier.
	methods := map[string]Tier{
		"/proto.Hippocampus/GetMemories": TierReader,
		"/proto.Hippocampus/StoreMemory": TierWriter,
		"/proto.Hippocampus/Purge":       TierAdmin,
	}

	roleTiers := map[string]Tier{"reader": TierReader, "writer": TierWriter, "admin": TierAdmin}

	for role, held := range roleTiers {
		for method, required := range methods {
			reached = false

			ctx := ContextWithClaims(context.Background(), &Claims{Roles: []string{role}})
			info := &grpc.UnaryServerInfo{FullMethod: method}

			_, err := interceptor(ctx, nil, info, handler)

			wantAllowed := held >= required

			if wantAllowed && err != nil {
				t.Errorf("role %s calling %s: expected allow, got %v", role, method, err)
			}

			if !wantAllowed {
				if status.Code(err) != codes.PermissionDenied {
					t.Errorf("role %s calling %s: expected PermissionDenied, got %v", role, method, err)
				}

				if reached {
					t.Errorf("role %s calling %s: handler ran despite deny", role, method)
				}
			}
		}
	}
}

// TestInterceptorStashesTier confirms a successful authorization puts the resolved tier on the
// context, which the reinforcement gate downstream relies on.
func TestInterceptorStashesTier(t *testing.T) {
	a, err := NewAuthorizer(nil)
	if err != nil {
		t.Fatalf("NewAuthorizer: %s", err)
	}

	var seen Tier
	var ok bool

	handler := func(ctx context.Context, req any) (any, error) {
		seen, ok = TierFromContext(ctx)

		return nil, nil
	}

	ctx := ContextWithClaims(context.Background(), &Claims{Roles: []string{"writer"}})
	info := &grpc.UnaryServerInfo{FullMethod: "/proto.Hippocampus/StoreMemory"}

	if _, err := a.UnaryServerInterceptor()(ctx, nil, info, handler); err != nil {
		t.Fatalf("interceptor: %s", err)
	}

	if !ok || seen != TierWriter {
		t.Fatalf("expected TierWriter stashed on context, got (%v, %v)", seen, ok)
	}
}

// TestInterceptorIgnoresNonHippocampus confirms the interceptor leaves the health service (and
// anything outside the Hippocampus prefix) untouched, so probes need no role.
func TestInterceptorIgnoresNonHippocampus(t *testing.T) {
	a, err := NewAuthorizer(nil)
	if err != nil {
		t.Fatalf("NewAuthorizer: %s", err)
	}

	reached := false
	handler := func(ctx context.Context, req any) (any, error) {
		reached = true

		return "ok", nil
	}

	info := &grpc.UnaryServerInfo{FullMethod: "/grpc.health.v1.Health/Check"}

	if _, err := a.UnaryServerInterceptor()(context.Background(), nil, info, handler); err != nil {
		t.Fatalf("expected the health check to pass through, got %v", err)
	}

	if !reached {
		t.Fatalf("health check did not reach the handler")
	}
}

// stubHippocampusServer satisfies contract.HippocampusServer with every method returning
// Unimplemented, so the gateway routes resolve without needing real handlers. A request that
// passes authorization reaches such a handler and returns 501; a denied one is stopped at 403.
type stubHippocampusServer struct {
	contract.UnimplementedHippocampusServer
}

// TestGatewayMiddlewareEndToEnd registers the real gateway (so the real google.api.http patterns
// drive routing) with the authorization middleware installed, and asserts each representative route
// is allowed or denied per the caller's role. This also validates the core assumption that
// runtime.HTTPPattern is populated at middleware time and that normalizePattern matches the real
// route templates.
func TestGatewayMiddlewareEndToEnd(t *testing.T) {
	a, err := NewAuthorizer(nil)
	if err != nil {
		t.Fatalf("NewAuthorizer: %s", err)
	}

	mux := runtime.NewServeMux(runtime.WithMiddlewares(a.GatewayMiddleware()))
	if err := contract.RegisterHippocampusHandlerServer(context.Background(), mux, &stubHippocampusServer{}); err != nil {
		t.Fatalf("RegisterHippocampusHandlerServer: %s", err)
	}

	type req struct {
		method string
		path   string
	}

	// One representative route per tier, including a capture-bearing path to exercise
	// normalizePattern ("/v1/events/{id}").
	reads := req{http.MethodGet, "/v1/memories"}
	writes := req{http.MethodPost, "/v1/memories"}
	admin := req{http.MethodPost, "/v1/purge"}
	capture := req{http.MethodGet, "/v1/events/some-id"} // GetEventById, reader

	cases := []struct {
		role       string
		r          req
		wantForbid bool
	}{
		{"reader", reads, false},
		{"reader", capture, false},
		{"reader", writes, true},
		{"reader", admin, true},
		{"writer", writes, false},
		{"writer", admin, true},
		{"admin", admin, false},
		{"root", reads, true}, // unknown role resolves to no tier -> denied
	}

	for _, c := range cases {
		name := c.role + " " + c.r.method + " " + c.r.path
		t.Run(name, func(t *testing.T) {
			// Inject verified claims the way auth.HTTPMiddleware would, then route through the mux.
			claims := &Claims{Roles: []string{c.role}}

			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mux.ServeHTTP(w, r.WithContext(ContextWithClaims(r.Context(), claims)))
			})

			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, httptest.NewRequest(c.r.method, c.r.path, nil))

			forbidden := rr.Code == http.StatusForbidden

			if forbidden != c.wantForbid {
				t.Fatalf("%s: got status %d (forbidden=%v), want forbidden=%v", name, rr.Code, forbidden, c.wantForbid)
			}

			// An allowed request must actually reach the (unimplemented) handler, i.e. not be a 403.
			if !c.wantForbid && rr.Code == http.StatusForbidden {
				t.Fatalf("%s: allowed request was forbidden", name)
			}
		})
	}
}

func TestTierString(t *testing.T) {
	cases := map[Tier]string{TierReader: "reader", TierWriter: "writer", TierAdmin: "admin", tierNone: "none", Tier(99): "none"}

	for tier, want := range cases {
		if got := tier.String(); got != want {
			t.Errorf("Tier(%d).String() = %q, want %q", tier, got, want)
		}
	}
}

// TestGatewayMiddlewareMissingPattern covers the fail-closed branch: a matched-looking request that
// somehow carries no route pattern on its context is forbidden rather than allowed by default.
func TestGatewayMiddlewareMissingPattern(t *testing.T) {
	a, err := NewAuthorizer(nil)
	if err != nil {
		t.Fatalf("NewAuthorizer: %s", err)
	}

	reached := false
	next := func(w http.ResponseWriter, r *http.Request, pathParams map[string]string) {
		reached = true
	}

	rr := httptest.NewRecorder()
	// A request with valid admin claims but no runtime.HTTPPattern on the context.
	r := httptest.NewRequest(http.MethodPost, "/v1/purge", nil)
	r = r.WithContext(ContextWithClaims(r.Context(), &Claims{Roles: []string{"admin"}}))

	a.GatewayMiddleware()(next)(rr, r, nil)

	if rr.Code != http.StatusForbidden {
		t.Errorf("expected 403 when the route pattern is absent, got %d", rr.Code)
	}

	if reached {
		t.Errorf("handler ran despite the missing pattern")
	}
}

// TestRolesFromClaim covers the non-standard role-claim extraction used for identity providers that
// do not publish roles under "roles".
func TestRolesFromClaim(t *testing.T) {
	// Mint a token carrying roles under a custom top-level claim via a raw HS256 signing, reusing
	// MintToken's standard path is not possible (it only writes "roles"), so build a token whose
	// "groups" claim we then read back.
	token, err := MintToken(MintRequest{Secret: strings.Repeat("x", 32), ClientID: "c", Roles: []string{"ignored"}, TTL: 3600_000_000_000})
	if err != nil {
		t.Fatalf("MintToken: %s", err)
	}

	// The standard claim is "roles"; asking for a different, absent claim yields no roles.
	if got := rolesFromClaim(token, "groups"); got != nil {
		t.Fatalf("expected no roles for absent claim, got %v", got)
	}

	// Reading the present "roles" claim back returns it.
	if got := rolesFromClaim(token, "roles"); len(got) != 1 || got[0] != "ignored" {
		t.Fatalf("expected [ignored] from roles claim, got %v", got)
	}

	// A claim of an unexpected type (number) yields no roles rather than erroring, and a mixed array
	// keeps only the string members. Build these directly since MintToken only writes "roles".
	raw := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"exp":       time.Now().Add(time.Hour).Unix(),
		"num_claim": 42,
		"mixed":     []any{"reader", 7, "writer"},
	})

	signed, err := raw.SignedString([]byte(strings.Repeat("x", 32)))
	if err != nil {
		t.Fatalf("SignedString: %s", err)
	}

	if got := rolesFromClaim(signed, "num_claim"); got != nil {
		t.Fatalf("expected no roles for a non-string/array claim, got %v", got)
	}

	if got := rolesFromClaim(signed, "mixed"); len(got) != 2 || got[0] != "reader" || got[1] != "writer" {
		t.Fatalf("expected [reader writer] from a mixed array, got %v", got)
	}

	// A token that does not parse yields no roles rather than panicking.
	if got := rolesFromClaim("not-a-jwt", "roles"); got != nil {
		t.Fatalf("expected no roles from an unparseable token, got %v", got)
	}
}

// TestRolesFromClaimProviderShapes covers the two real-world provider shapes: Auth0 namespaces its
// roles under a URI-keyed top-level claim (dots and slashes in the key, matched literally), and
// Keycloak nests them under realm_access.roles (matched as a dotted path when no literal key
// exists).
func TestRolesFromClaimProviderShapes(t *testing.T) {
	raw := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"exp": time.Now().Add(time.Hour).Unix(),

		// Auth0: a URI-namespaced top-level claim. The key itself contains dots and slashes, so it
		// must match literally rather than being split into a path.
		"https://hippocampus.demo/roles": []any{"writer", "admin"},

		// Keycloak: roles nested one level down under realm_access.
		"realm_access": map[string]any{
			"roles": []any{"reader", "writer"},
		},
	})

	signed, err := raw.SignedString([]byte(strings.Repeat("x", 32)))
	if err != nil {
		t.Fatalf("SignedString: %s", err)
	}

	if got := rolesFromClaim(signed, "https://hippocampus.demo/roles"); len(got) != 2 || got[0] != "writer" || got[1] != "admin" {
		t.Fatalf("expected [writer admin] from the Auth0 namespaced claim, got %v", got)
	}

	if got := rolesFromClaim(signed, "realm_access.roles"); len(got) != 2 || got[0] != "reader" || got[1] != "writer" {
		t.Fatalf("expected [reader writer] from the Keycloak nested claim, got %v", got)
	}

	// A dotted path whose intermediate segment is missing yields no roles rather than erroring.
	if got := rolesFromClaim(signed, "resource_access.roles"); got != nil {
		t.Fatalf("expected no roles for an absent nested path, got %v", got)
	}

	// A dotted path that bottoms out on a non-object before its final segment also yields nothing.
	if got := rolesFromClaim(signed, "realm_access.roles.extra"); got != nil {
		t.Fatalf("expected no roles when the path walks past a leaf, got %v", got)
	}

	// A provider that publishes a single role as a bare string (rather than a one-element array) is
	// accepted, at a top-level key and down a nested path alike.
	scalar := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"exp":  time.Now().Add(time.Hour).Unix(),
		"role": "admin",
		"realm_access": map[string]any{
			"role": "writer",
		},
	})

	signedScalar, err := scalar.SignedString([]byte(strings.Repeat("x", 32)))
	if err != nil {
		t.Fatalf("SignedString: %s", err)
	}

	if got := rolesFromClaim(signedScalar, "role"); len(got) != 1 || got[0] != "admin" {
		t.Fatalf("expected [admin] from a scalar top-level claim, got %v", got)
	}

	if got := rolesFromClaim(signedScalar, "realm_access.role"); len(got) != 1 || got[0] != "writer" {
		t.Fatalf("expected [writer] from a scalar nested claim, got %v", got)
	}
}
