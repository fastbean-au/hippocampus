package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/fastbean-au/hippocampus/auth"
	"github.com/fastbean-au/hippocampus/contract"
	"github.com/fastbean-au/hippocampus/hippocampus"
	"github.com/fastbean-au/hippocampus/observability"
	"github.com/fastbean-au/hippocampus/ratelimit"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"github.com/spf13/viper"
)

// openAPIEnabled reports whether the OpenAPI route should be registered.
//
// An UNSET key means enabled. That is deliberately not left to viper.SetDefault alone: the zero
// value of a bool is the opposite of every release before this key existed, so any path that reaches
// run() without having seeded the defaults would silently stop serving an endpoint the documentation
// tells clients to generate from. The default is still declared in setStartupDefaults, because that
// is the one place the configuration wizard's cross-check reads it from; this makes the behaviour
// independent of whether it was applied.
func openAPIEnabled() bool {
	if viper.IsSet("gateway.openapi.enabled") {
		return viper.GetBool("gateway.openapi.enabled")
	}

	return true
}

// Gateway HTTP server hardening timeouts. ReadHeaderTimeout bounds slow-header (slowloris) clients,
// ReadTimeout bounds reading the whole request including its body (the same attack one step later:
// a header sent promptly and a body trickled), and IdleTimeout bounds idle keep-alive connections.
// ReadTimeout is generous because it is a ceiling on the slowest legitimate client, not a target:
// a body at the default cap fits inside it at well under 100 KB/s. There is deliberately no
// WriteTimeout - Export/Import/Transfer responses can legitimately run long, and a write deadline
// would abort them mid-stream.
const (
	gatewayReadHeaderTimeout = 10 * time.Second
	gatewayReadTimeout       = 2 * time.Minute
	gatewayIdleTimeout       = 120 * time.Second
)

// grpcDefaultMaxRecvMsgBytes is grpc-go's own receive limit, which applies when maxRecvMsgBytes is
// left at 0.
const grpcDefaultMaxRecvMsgBytes = 4 << 20

// gatewayBodyLimit resolves the gateway's request-body ceiling (TODO-3 item 161). An explicit
// gateway.maxRequestBytes wins, and an explicit value of 0 or less is the deliberate "unbounded".
// Unset, the ceiling follows the gRPC transport's effective receive limit, so one request is bounded
// alike on both transports - doubled, because the gateway reads JSON, which carries every field name
// on every record and so runs larger than the protobuf encoding of the same request. Equal limits
// would refuse over HTTP a batch the gRPC transport accepts.
func gatewayBodyLimit(set bool, configured int64, maxRecvMsgBytes int) int64 {
	if set {
		return max(configured, 0)
	}

	grpcLimit := int64(grpcDefaultMaxRecvMsgBytes)

	if maxRecvMsgBytes > 0 {
		grpcLimit = int64(maxRecvMsgBytes)
	}

	return 2 * grpcLimit
}

// gatewayMuxOptions returns the ServeMux options every gateway wiring needs, whatever the
// configuration adds on top (the authoriser and the rate limiter are appended by the caller when
// they are enabled). It is a function rather than a literal at the call site so a test can build a
// mux routed exactly as the service's is.
//
// The unescaping mode is load-bearing. Ids here are caller-chosen and routinely contain a "/" - an
// at:// URI is what the event-source bridges write - and the default (UnescapingModeLegacy) routes
// on r.URL.Path, which net/url has by then fully unescaped: a %2F inside an id has become a real
// separator and split the id across two path segments, so no by-id route matched at all and every
// such request 404'd. Every other mode routes on r.URL.RawPath and splits it before unescaping, so
// an escaped id stays in one segment; a single-segment capture is then always unescaped in full
// (grpc-gateway forces UnescapingModeAllCharacters for those regardless of the mode), which is what
// hands the handler the id byte-for-byte as it was written. AllExceptReserved is the mode
// grpc-gateway intends to default to in v3, so this is a step towards that default rather than away
// from it, and it is inert for a request carrying nothing escaped: RawPath is empty then, and the
// legacy path is taken.
func gatewayMuxOptions() []runtime.ServeMuxOption {
	return []runtime.ServeMuxOption{
		runtime.WithUnescapingMode(runtime.UnescapingModeAllExceptReserved),

		// The route-capture middleware is registered first so it runs outermost of the gateway
		// middlewares: it names the RPC for httpMetricsMiddleware, and running ahead of the
		// authoriser means a request the authoriser rejects is still counted against the RPC it was
		// aimed at.
		runtime.WithMiddlewares(gatewayRouteMiddleware()),
	}
}

// newGatewayServer builds the HTTP gateway server with the hardening timeouts above. It is a
// separate function so the timeout policy can be unit-tested without standing up the whole service.
// bindAddress (empty by default) restricts the interface the listener binds to; empty binds all
// interfaces, mirroring the gRPC listener's bindAddress.
func newGatewayServer(bindAddress string, port int, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              bindAddress + ":" + strconv.Itoa(port),
		Handler:           handler,
		ReadHeaderTimeout: gatewayReadHeaderTimeout,
		ReadTimeout:       gatewayReadTimeout,
		IdleTimeout:       gatewayIdleTimeout,
	}
}

// maxRequestBytesMiddleware caps the request body the gateway will read, so an oversized (or
// deliberately huge) body is rejected by the transport before a handler buffers it into memory. GET
// requests (health, list) carry no body and are unaffected.
//
// An oversized body is answered 413, which takes two routes because the gateway turns any failed
// body read into InvalidArgument and so would answer 400 - telling the client its request was
// malformed rather than too large (TODO-3 item 161). A body declaring its length is refused before
// the handler runs at all; one that does not (chunked) is refused when the read overflows, by
// replacing the status the handler then writes.
func maxRequestBytesMiddleware(next http.Handler, maxBytes int64) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body == nil || r.Body == http.NoBody {
			next.ServeHTTP(w, r)

			return
		}

		if r.ContentLength > maxBytes {
			http.Error(w, fmt.Sprintf("request body exceeds the %d-byte limit", maxBytes), http.StatusRequestEntityTooLarge)

			return
		}

		limited := &overflowRecorder{ResponseWriter: w}
		r.Body = &overflowReader{ReadCloser: http.MaxBytesReader(w, r.Body, maxBytes), overflowed: &limited.overflowed}

		next.ServeHTTP(limited, r)
	})
}

// overflowReader notes when the body read has hit the limit, for overflowRecorder to act on.
type overflowReader struct {
	io.ReadCloser
	overflowed *bool
}

func (o *overflowReader) Read(p []byte) (int, error) {
	n, err := o.ReadCloser.Read(p)

	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		*o.overflowed = true
	}

	return n, err
}

// overflowRecorder answers 413 in place of whatever error status the handler writes once the body
// has overflowed. The read and the write happen on the handler's goroutine, so the flag needs no
// lock.
type overflowRecorder struct {
	http.ResponseWriter
	overflowed bool
}

func (o *overflowRecorder) WriteHeader(code int) {
	if o.overflowed && code >= http.StatusBadRequest {
		code = http.StatusRequestEntityTooLarge
	}

	o.ResponseWriter.WriteHeader(code)
}

// Unwrap lets http.ResponseController reach the underlying writer's flusher and deadlines.
func (o *overflowRecorder) Unwrap() http.ResponseWriter {
	return o.ResponseWriter
}

// gatewayConfig is what newGatewayHandler needs to build the HTTP gateway's handler chain.
type gatewayConfig struct {
	// Port is gateway.port; a non-positive one disables the gateway.
	Port        int
	BindAddress string

	OpenAPI bool

	// MaxRequestBytes is gatewayBodyLimit's resolution; 0 or less is unbounded.
	MaxRequestBytes int64

	CORSOrigins       []string
	TrustForwardedFor bool

	// UI is the console's front-channel config, less LoginMode, which follows what initAuth built.
	UI UIConfig
}

// gatewayDeps is what the gateway serves and guards it with.
type gatewayDeps struct {
	Hippocampus *hippocampus.Server

	// Served is the value the gateway calls: Hippocampus wrapped for the audit log.
	Served contract.HippocampusServer

	Auth      authSetup
	Limiter   *ratelimit.Limiter
	Readiness *readinessProbe
	Version   versionInfo
}

// setGatewayDefaults applies the gateway.* defaults.
func setGatewayDefaults() {
	// The OpenAPI document is served by default, which is what every version before the key existed
	// did. It is defaulted rather than left to read as a zero bool precisely because the zero value
	// is the opposite of the existing behaviour, and an unset key must not silently remove an
	// endpoint the documentation tells clients to generate from.
	viper.SetDefault("gateway.openapi.enabled", true)
}

// gatewayProblems checks the gateway.* keys.
func gatewayProblems() []error {
	var problems []error

	// Cross-origin access. An entry that does not match what a browser actually sends in the Origin
	// header - a trailing slash being the usual way to get it wrong - presents as CORS simply not
	// working rather than as a configuration error, so refuse it at startup instead.
	if err := validateCORSOrigins(parseCORSOrigins(viper.Get("gateway.corsOrigins"))); err != nil {
		problems = append(problems, err)
	}

	return problems
}

// gatewayConfigFromViper reads the gateway's keys and the console's front-channel OIDC config.
func gatewayConfigFromViper(authMethod string) gatewayConfig {
	log.Trace("func() gatewayConfigFromViper")

	// The front-channel OIDC config the console reads to start a login. The issuer defaults to
	// auth.issuer (the same one the verifier discovers its JWKS from); scopes default to a plain
	// openid/profile request when unset.
	uiIssuer := viper.GetString("auth.ui.issuer")

	if uiIssuer == "" {
		uiIssuer = viper.GetString("auth.issuer")
	}

	uiScopes := viper.GetString("auth.ui.scopes")

	if uiScopes == "" {
		uiScopes = "openid profile"
	}

	return gatewayConfig{
		Port:        viper.GetInt("gateway.port"),
		BindAddress: viper.GetString("gateway.bindAddress"),
		OpenAPI:     openAPIEnabled(),

		// Unset, the cap follows the gRPC transport's own (gatewayBodyLimit); an explicit 0 is the
		// deliberate "unbounded".
		MaxRequestBytes: gatewayBodyLimit(
			viper.IsSet("gateway.maxRequestBytes"),
			viper.GetInt64("gateway.maxRequestBytes"),
			viper.GetInt("maxRecvMsgBytes"),
		),

		CORSOrigins:       parseCORSOrigins(viper.Get("gateway.corsOrigins")),
		TrustForwardedFor: viper.GetBool("rateLimit.trustForwardedFor"),
		UI: UIConfig{
			AuthMethod: authMethod,
			Issuer:     uiIssuer,
			ClientID:   viper.GetString("auth.ui.clientId"),
			Scopes:     uiScopes,
			Audience:   viper.GetString("auth.ui.audience"),
		},
	}
}

// newGatewayHandler builds the HTTP gateway's handler: the /v1 JSON API, the lifecycle probes, the
// OpenAPI document, the console and the login endpoints, wrapped in the same order of checks as the
// gRPC interceptor chain. The gateway calls straight into the server rather than dialing back to
// the gRPC listener, so there is no extra network hop or serialisation round trip.
func newGatewayHandler(cfg gatewayConfig, deps gatewayDeps) (http.Handler, error) {
	log.Trace("func() newGatewayHandler")

	authEnabled := deps.Auth.enabled()
	oauth2Login := deps.Auth.OAuth2Login

	// Authorisation on the gateway runs as a post-routing middleware (so the matched route - hence
	// the RPC's required tier - is known) rather than an outer HTTP wrapper. It reads the claims
	// stashed by auth.HTTPMiddleware below, so it enforces the same policy as the gRPC interceptor
	// from the same table. Added only when auth is enabled, mirroring the gRPC side.
	muxOpts := gatewayMuxOptions()

	if authEnabled {
		muxOpts = append(muxOpts, runtime.WithMiddlewares(deps.Auth.Authoriser.GatewayMiddleware()))
	}

	// The per-client/per-tier limit is a gateway middleware rather than an outer HTTP wrapper, and
	// is registered after the authoriser, because the tier it reads is stashed on the request
	// context by that middleware - an outer wrapper would run before the tier existed.
	if deps.Limiter.PrincipalActive() {
		muxOpts = append(muxOpts, runtime.WithMiddlewares(rateLimitGatewayMiddleware(deps.Limiter, cfg.TrustForwardedFor)))
	}

	gwMux := runtime.NewServeMux(muxOpts...)
	if err := contract.RegisterHippocampusHandlerServer(context.Background(), gwMux, deps.Served); err != nil {
		return nil, fmt.Errorf("failed to register HTTP gateway: %w", err)
	}

	// The Kubernetes lifecycle endpoints: /livez (liveness and startup), /readyz (readiness,
	// database-aware and failing from the start of shutdown), and /healthz, the older name for
	// /livez that every probe written before it existed still points at.
	liveness := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "version": deps.Version})
	}

	httpMux := http.NewServeMux()
	httpMux.HandleFunc(observability.LivenessPath, liveness)
	httpMux.HandleFunc(observability.LegacyHealthPath, liveness)
	httpMux.HandleFunc(observability.ReadinessPath, deps.Readiness.handler())

	// The OpenAPI document describing /v1. Enabled by default and served without a token - see
	// openAPIHandler for why that reversed - with gateway.openapi.enabled available to remove the
	// route outright for a deployment that wants nothing served there.
	if cfg.OpenAPI {
		httpMux.Handle(openAPIPath, openAPIHandler())
	} else {
		log.Info("OpenAPI document disabled (gateway.openapi.enabled is false): /v1/openapi.json is not served, so a client must generate from contract/hippocampus.proto or a descriptor set built from it")
	}

	// The front-channel OIDC config the console reads to start a login. The browser fetches it
	// before it has a token, so it must stay unauthenticated and reachable during a purge, like the
	// page itself.
	//
	// LoginMode tells the console how sign-in works: server-hosted (/auth/login) when the oauth2
	// login is built, otherwise the in-browser PKCE flow under idp, and empty otherwise.
	uiConfig := cfg.UI

	if uiConfig.AuthMethod == "idp" {
		uiConfig.LoginMode = "browser"

		if oauth2Login != nil {
			uiConfig.LoginMode = "server"
		}
	}

	httpMux.Handle("/ui/config", uiConfigHandler(uiConfig))

	// The console's own routes. Registered here rather than beside the probes above because the
	// security headers need uiConfig: under an in-browser OIDC login the policy has to permit
	// connections to the identity provider, which is not known until the block above has run.
	console := map[string]http.Handler{
		"/ui":            webUIHandler(),
		"/ui/app.js":     webUIAssetHandler("app.js", "text/javascript; charset=utf-8"),
		"/ui/lib.js":     webUIAssetHandler("lib.js", "text/javascript; charset=utf-8"),
		"/ui/styles.css": webUIAssetHandler("styles.css", "text/css; charset=utf-8"),
	}

	for path, handler := range console {
		httpMux.Handle(path, webUISecurityHeaders(uiConfig, handler))
	}

	// When the server-side login is enabled, register its handlers; middlewareOpenPaths marks them
	// open alongside the probes and the console (they run before, or in place of, holding a token -
	// /auth/refresh authenticates itself via the refresh cookie).
	sessionCookie := ""
	authOpenPaths, purgeOpenPaths := middlewareOpenPaths(oauth2Login != nil)

	if oauth2Login != nil {
		httpMux.Handle("/auth/login", http.HandlerFunc(oauth2Login.Login))
		httpMux.Handle("/auth/callback", http.HandlerFunc(oauth2Login.Callback))
		httpMux.Handle("/auth/refresh", http.HandlerFunc(oauth2Login.Refresh))
		httpMux.Handle("/auth/logout", http.HandlerFunc(oauth2Login.Logout))

		sessionCookie = auth.SessionCookieName
	}

	httpMux.Handle("/", gwMux)

	// The gateway calls the server directly, bypassing the gRPC interceptor chain, so the purge gate
	// must be re-applied here or /v1/... requests would run during a purge. It is applied
	// unconditionally (independent of auth); /healthz, /readyz, the static OpenAPI doc, the
	// console, its front-channel config, and the login endpoints stay reachable while a purge runs.
	handler := deps.Hippocampus.HTTPMiddlewareBlockWhenPurgeInProgress(httpMux, purgeOpenPaths)

	// Per-request logging: the gateway never runs the gRPC interceptor chain, so without this its
	// traffic is invisible in logs. Positioned inside auth (so unauthenticated requests are still
	// rejected first) but around the purge gate and handler, capturing status and duration.
	handler = httpLoggingMiddleware(handler)

	// /healthz, /readyz and the OpenAPI document stay open; everything else requires a token when
	// auth is enabled. Auth wraps the purge gate so an unauthenticated request is rejected before
	// any other check, mirroring the gRPC chain order.
	if authEnabled {
		// Inside auth, so it sees the verified claims that middleware stashes, and outside
		// everything else for the same reason its gRPC counterpart runs before the authoriser.
		// The open paths never reach the verifier, so they carry no claims and record nothing.
		handler = deps.Hippocampus.HTTPMiddlewareObserveCaller(handler)

		handler = auth.HTTPMiddleware(deps.Auth.Verifier, handler, authOpenPaths, sessionCookie)
	}

	// The instance-wide ceiling wraps auth here exactly as it precedes it in the gRPC chain, so a
	// flood is bounded before the gateway verifies the tokens in it. It is scoped to the /v1 RPC
	// surface, so the probes, the console, and the login endpoints stay reachable while it is
	// rejecting - throttling a probe would turn a busy instance into a restarted one.
	if deps.Limiter.ArrivalActive() {
		handler = rateLimitArrivalMiddleware(deps.Limiter, handler)
	}

	// Cap the request body the gateway will read. Outermost so an oversized body is rejected before
	// auth or any handler buffers it.
	if cfg.MaxRequestBytes > 0 {
		handler = maxRequestBytesMiddleware(handler, cfg.MaxRequestBytes)
	}

	// RPC metrics wrap auth and the body-size cap so a request rejected by either is still counted in
	// the rate and error figures, matching the gRPC chain, where the metrics interceptor also sits
	// outside authentication.
	handler = httpMetricsMiddleware(handler)

	// Cross-origin access for browser clients that are not served by this gateway - swagger-ui, or
	// a console hosted elsewhere. Empty by default, so a deployment that has not asked for this is
	// byte-for-byte unchanged. corsMiddleware documents why it wraps the metrics middleware rather
	// than sitting anywhere lower in the chain.
	if len(cfg.CORSOrigins) > 0 {
		handler = corsMiddleware(cfg.CORSOrigins, handler)

		log.Infof("gateway CORS enabled for %s: a browser page on those origins may call /v1. Credentials are never allowed, so a session cookie cannot be used cross-origin", strings.Join(cfg.CORSOrigins, ", "))
	}

	// Panic recovery wraps everything (outermost) so a panic in any handler or middleware becomes a
	// clean 500 rather than a dropped connection.
	return recoverMiddleware(handler), nil
}
