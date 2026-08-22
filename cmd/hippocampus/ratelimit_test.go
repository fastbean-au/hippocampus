package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
	"github.com/spf13/viper"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric/noop"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"github.com/fastbean-au/hippocampus/auth"
	"github.com/fastbean-au/hippocampus/ratelimit"
)

const testRPCMethod = "/hippocampus.v1.Hippocampus/GetMemories"

func testLimiter(t *testing.T, cfg ratelimit.Config) *ratelimit.Limiter {
	t.Helper()

	limiter, err := ratelimit.New(cfg)
	if err != nil {
		t.Fatalf("failed to build the limiter: %s", err.Error())
	}

	return limiter
}

func countingHandler(calls *int) grpc.UnaryHandler {
	return func(ctx context.Context, req any) (any, error) {
		*calls++

		return "ok", nil
	}
}

// TestInterceptorRateLimitArrivalRefusesOverTheCeiling covers the edge check: the burst is served,
// the next call is refused with ResourceExhausted, and the handler is never reached for it.
func TestInterceptorRateLimitArrivalRefusesOverTheCeiling(t *testing.T) {
	limiter := testLimiter(t, ratelimit.Config{Global: ratelimit.Rule{RequestsPerSecond: 0.001, Burst: 1}})
	interceptor := InterceptorRateLimitArrival(limiter)

	calls := 0
	info := &grpc.UnaryServerInfo{FullMethod: testRPCMethod}

	if _, err := interceptor(context.Background(), nil, info, countingHandler(&calls)); err != nil {
		t.Fatalf("the first request was refused: %s", err.Error())
	}

	_, err := interceptor(context.Background(), nil, info, countingHandler(&calls))
	if status.Code(err) != codes.ResourceExhausted {
		t.Errorf("the second request returned %s, expected ResourceExhausted", status.Code(err))
	}

	if calls != 1 {
		t.Errorf("the handler ran %d times, expected only for the admitted request", calls)
	}
}

// The health service is a probe surface, not an RPC. Throttling it would turn a busy instance into
// a restarted one, so it must bypass both interceptors entirely.
func TestRateLimitInterceptorsIgnoreNonHippocampusMethods(t *testing.T) {
	limiter := testLimiter(t, ratelimit.Config{
		Global:    ratelimit.Rule{RequestsPerSecond: 0.001, Burst: 1},
		PerClient: ratelimit.Rule{RequestsPerSecond: 0.001, Burst: 1},
	})

	info := &grpc.UnaryServerInfo{FullMethod: "/grpc.health.v1.Health/Check"}

	for _, tc := range []struct {
		name        string
		interceptor grpc.UnaryServerInterceptor
	}{
		{"arrival", InterceptorRateLimitArrival(limiter)},
		{"principal", InterceptorRateLimitPrincipal(limiter)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0

			for i := range 5 {
				if _, err := tc.interceptor(context.Background(), nil, info, countingHandler(&calls)); err != nil {
					t.Fatalf("health check %d was refused: %s", i+1, err.Error())
				}
			}

			if calls != 5 {
				t.Errorf("the handler ran %d times, expected all 5 health checks through", calls)
			}
		})
	}
}

// TestInterceptorRateLimitPrincipalKeysOnTheVerifiedClient checks that the bucket follows the
// authenticated identity rather than the connection: two clients on one peer address get their own
// allowances.
func TestInterceptorRateLimitPrincipalKeysOnTheVerifiedClient(t *testing.T) {
	limiter := testLimiter(t, ratelimit.Config{PerClient: ratelimit.Rule{RequestsPerSecond: 0.001, Burst: 1}})
	interceptor := InterceptorRateLimitPrincipal(limiter)

	info := &grpc.UnaryServerInfo{FullMethod: testRPCMethod}
	calls := 0

	ctxFor := func(clientID string) context.Context {
		return auth.ContextWithClaims(context.Background(), &auth.Claims{ClientID: clientID})
	}

	if _, err := interceptor(ctxFor("alice"), nil, info, countingHandler(&calls)); err != nil {
		t.Fatalf("alice's first request was refused: %s", err.Error())
	}

	if _, err := interceptor(ctxFor("alice"), nil, info, countingHandler(&calls)); status.Code(err) != codes.ResourceExhausted {
		t.Errorf("alice's second request returned %s, expected ResourceExhausted", status.Code(err))
	}

	if _, err := interceptor(ctxFor("bob"), nil, info, countingHandler(&calls)); err != nil {
		t.Errorf("bob was refused because alice had exhausted her own bucket: %s", err.Error())
	}
}

// TestInterceptorRateLimitPrincipalAppliesTheTier covers the level that only exists once
// authorisation has run: the tier is read from the context the authoriser stashed it on.
func TestInterceptorRateLimitPrincipalAppliesTheTier(t *testing.T) {
	limiter := testLimiter(t, ratelimit.Config{
		Tiers: map[string]ratelimit.Rule{"reader": {RequestsPerSecond: 0.001, Burst: 1}},
	})
	interceptor := InterceptorRateLimitPrincipal(limiter)

	info := &grpc.UnaryServerInfo{FullMethod: testRPCMethod}
	calls := 0

	reader := auth.ContextWithTier(context.Background(), auth.TierReader)
	writer := auth.ContextWithTier(context.Background(), auth.TierWriter)

	if _, err := interceptor(reader, nil, info, countingHandler(&calls)); err != nil {
		t.Fatalf("the first reader request was refused: %s", err.Error())
	}

	if _, err := interceptor(reader, nil, info, countingHandler(&calls)); status.Code(err) != codes.ResourceExhausted {
		t.Errorf("the second reader request returned %s, expected ResourceExhausted", status.Code(err))
	}

	// A tier with no rule is unlimited, and an unauthorised context (no tier at all) matches no
	// tier bucket.
	if _, err := interceptor(writer, nil, info, countingHandler(&calls)); err != nil {
		t.Errorf("a writer was refused by the reader tier's rule: %s", err.Error())
	}

	if _, err := interceptor(context.Background(), nil, info, countingHandler(&calls)); err != nil {
		t.Errorf("an unauthorised caller was refused by a tier rule: %s", err.Error())
	}
}

// TestGRPCPrincipalKeyPrefersTheVerifiedIdentity pins the preference order and, more importantly,
// the prefixes: without them a client id that reads as an address would share a bucket with that
// address, and the address is the part of the key a caller can influence.
func TestGRPCPrincipalKeyPrefersTheVerifiedIdentity(t *testing.T) {
	withPeer := peer.NewContext(context.Background(), &peer.Peer{
		Addr: &net.TCPAddr{IP: net.ParseIP("198.51.100.7"), Port: 44312},
	})

	for _, tc := range []struct {
		name     string
		ctx      context.Context
		expected string
	}{
		{
			name:     "verified client id",
			ctx:      auth.ContextWithClaims(withPeer, &auth.Claims{ClientID: "alice"}),
			expected: "client:alice",
		},
		{
			name: "subject when the token carries no client id",
			ctx: auth.ContextWithClaims(withPeer, &auth.Claims{
				RegisteredClaims: jwt.RegisteredClaims{Subject: "user-1"},
			}),
			expected: "sub:user-1",
		},
		{
			// The port is stripped so a caller's every connection shares one bucket; keeping it
			// would hand out a fresh allowance per ephemeral source port.
			name:     "peer address without its port",
			ctx:      withPeer,
			expected: "ip:198.51.100.7",
		},
		{
			name:     "nothing identifies the caller",
			ctx:      context.Background(),
			expected: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if key := grpcPrincipalKey(tc.ctx); key != tc.expected {
				t.Errorf("key is '%s', expected '%s'", key, tc.expected)
			}
		})
	}
}

func TestRateLimitArrivalMiddlewareRefusesWith429(t *testing.T) {
	limiter := testLimiter(t, ratelimit.Config{Global: ratelimit.Rule{RequestsPerSecond: 0.001, Burst: 1}})

	served := 0
	handler := rateLimitArrivalMiddleware(limiter, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served++

		w.WriteHeader(http.StatusOK)
	}))

	first := httptest.NewRecorder()
	handler.ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/v1/memories", nil))

	if first.Code != http.StatusOK {
		t.Fatalf("the first request returned %d, expected 200", first.Code)
	}

	second := httptest.NewRecorder()
	handler.ServeHTTP(second, httptest.NewRequest(http.MethodGet, "/v1/memories", nil))

	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("the second request returned %d, expected 429", second.Code)
	}

	// RFC 9110's Retry-After, and never 0 - a "come back immediately" would invite exactly the
	// retry the limit exists to prevent.
	if retry := second.Header().Get("Retry-After"); retry == "" || retry == "0" {
		t.Errorf("Retry-After is '%s', expected a positive whole number of seconds", retry)
	}

	var body map[string]string

	if err := json.Unmarshal(second.Body.Bytes(), &body); err != nil {
		t.Fatalf("the 429 body is not the JSON error shape the other rejections use: %s", err.Error())
	}

	if body["scope"] != string(ratelimit.ScopeGlobal) {
		t.Errorf("the body names scope '%s', expected '%s'", body["scope"], ratelimit.ScopeGlobal)
	}

	if served != 1 {
		t.Errorf("the handler served %d requests, expected only the admitted one", served)
	}
}

// The probes, the console, and the login endpoints are not the RPC surface, and a limit that could
// reject a readiness probe would be a limit that gets the instance restarted under load.
func TestRateLimitArrivalMiddlewareIgnoresSupportPaths(t *testing.T) {
	limiter := testLimiter(t, ratelimit.Config{Global: ratelimit.Rule{RequestsPerSecond: 0.001, Burst: 1}})

	handler := rateLimitArrivalMiddleware(limiter, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	paths := []string{
		"/healthz",
		"/readyz",
		"/ui",
		"/ui/app.js",
		"/ui/styles.css",
		"/ui/config",
		"/auth/login",
		// NOTE: /v1/openapi.json is deliberately absent - it IS rate limited now. See
		// isRateLimitedPath and TestRateLimitArrivalMiddlewareThrottlesTheOpenAPIDocument.
	}

	for _, path := range paths {
		for i := range 3 {
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))

			if recorder.Code != http.StatusOK {
				t.Errorf("request %d to %s returned %d, expected it to bypass the limit", i+1, path, recorder.Code)
			}
		}
	}
}

// The OpenAPI document is the one support-surface path that IS throttled, and the split between
// isRPCPath and isRateLimitedPath exists for it alone. It is served without a token and is the
// largest single response the gateway produces, so leaving it outside the ceiling made it the
// cheapest bandwidth amplifier on the surface - while still, correctly, staying out of the RPC
// metrics, which describe the service's work rather than its documentation.
func TestRateLimitArrivalMiddlewareThrottlesTheOpenAPIDocument(t *testing.T) {
	limiter := testLimiter(t, ratelimit.Config{Global: ratelimit.Rule{RequestsPerSecond: 0.001, Burst: 1}})

	handler := rateLimitArrivalMiddleware(limiter, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	first := httptest.NewRecorder()
	handler.ServeHTTP(first, httptest.NewRequest(http.MethodGet, openAPIPath, nil))

	if first.Code != http.StatusOK {
		t.Fatalf("first request to %s returned %d, expected the burst to allow it", openAPIPath, first.Code)
	}

	second := httptest.NewRecorder()
	handler.ServeHTTP(second, httptest.NewRequest(http.MethodGet, openAPIPath, nil))

	if second.Code != http.StatusTooManyRequests {
		t.Errorf("second request to %s returned %d, expected %d - the document must be under the arrival ceiling", openAPIPath, second.Code, http.StatusTooManyRequests)
	}
}

// The two predicates differ on exactly one path, in opposite directions. Pinning both here is what
// stops a later tidy-up "simplifying" them back into one.
func TestOpenAPIDocumentIsRateLimitedButNotMetered(t *testing.T) {
	if isRPCPath(openAPIPath) {
		t.Errorf("isRPCPath(%q) = true, expected the document to stay out of the RPC metrics", openAPIPath)
	}

	if !isRateLimitedPath(openAPIPath) {
		t.Errorf("isRateLimitedPath(%q) = false, expected the document to be under the arrival ceiling", openAPIPath)
	}

	for _, path := range []string{"/healthz", "/readyz", "/ui", "/ui/config"} {
		if isRateLimitedPath(path) {
			t.Errorf("isRateLimitedPath(%q) = true, expected support paths to stay unthrottled", path)
		}
	}

	if !isRPCPath("/v1/memories") || !isRateLimitedPath("/v1/memories") {
		t.Error("an ordinary RPC path must be both metered and rate limited")
	}
}

// gatewayHandler drives a runtime.Middleware the way the mux does, so the per-tier/per-client
// middleware can be exercised without standing up a whole gateway.
func gatewayHandler(middleware runtime.Middleware, served *int) http.Handler {
	handler := middleware(func(w http.ResponseWriter, r *http.Request, pathParams map[string]string) {
		*served++

		w.WriteHeader(http.StatusOK)
	})

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler(w, r, nil)
	})
}

func TestRateLimitGatewayMiddlewareKeysOnTheRequest(t *testing.T) {
	limiter := testLimiter(t, ratelimit.Config{PerClient: ratelimit.Rule{RequestsPerSecond: 0.001, Burst: 1}})

	served := 0
	handler := gatewayHandler(rateLimitGatewayMiddleware(limiter, false), &served)

	request := func(remote string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/v1/memories", nil)
		r.RemoteAddr = remote

		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, r)

		return recorder
	}

	if code := request("198.51.100.7:5000").Code; code != http.StatusOK {
		t.Fatalf("the first request returned %d, expected 200", code)
	}

	// A different source port is the same caller, so it must not buy a fresh allowance.
	if code := request("198.51.100.7:5001").Code; code != http.StatusTooManyRequests {
		t.Errorf("a second request from the same address on a new port returned %d, expected 429", code)
	}

	if code := request("203.0.113.9:5000").Code; code != http.StatusOK {
		t.Errorf("a different address was refused with %d", code)
	}
}

// X-Forwarded-For is caller-supplied. Believing it by default would let any caller pick its own
// bucket, and therefore an unlimited number of them - so the trust is opt-in, and only then does a
// forwarded address separate two callers arriving on one connection.
func TestHTTPPrincipalKeyTrustsForwardedForOnlyWhenTold(t *testing.T) {
	forwarded := httptest.NewRequest(http.MethodGet, "/v1/memories", nil)
	forwarded.RemoteAddr = "10.0.0.1:9999"
	forwarded.Header.Set("X-Forwarded-For", "198.51.100.7, 10.0.0.1")

	if key := httpPrincipalKey(forwarded, false); key != "ip:10.0.0.1" {
		t.Errorf("untrusted key is '%s', expected the connection's own address", key)
	}

	if key := httpPrincipalKey(forwarded, true); key != "ip:198.51.100.7" {
		t.Errorf("trusted key is '%s', expected the leftmost forwarded address", key)
	}

	// A verified identity always wins, so turning the trust on cannot let a header displace a token.
	authenticated := forwarded.WithContext(auth.ContextWithClaims(forwarded.Context(), &auth.Claims{ClientID: "alice"}))

	if key := httpPrincipalKey(authenticated, true); key != "client:alice" {
		t.Errorf("key is '%s', expected the verified client id to win over the header", key)
	}

	// An empty header falls back rather than producing a key of "ip:".
	blank := httptest.NewRequest(http.MethodGet, "/v1/memories", nil)
	blank.RemoteAddr = "10.0.0.1:9999"
	blank.Header.Set("X-Forwarded-For", " , ")

	if key := httpPrincipalKey(blank, true); key != "ip:10.0.0.1" {
		t.Errorf("key from an empty forwarded header is '%s', expected the connection's address", key)
	}
}

func TestRetryAfterSecondsIsNeverZero(t *testing.T) {
	for _, tc := range []struct {
		wait     time.Duration
		expected string
	}{
		{0, "1"},
		{time.Millisecond, "1"},
		{1500 * time.Millisecond, "2"},
		{30 * time.Second, "30"},
	} {
		if actual := retryAfterSeconds(ratelimit.Decision{RetryAfter: tc.wait}); actual != tc.expected {
			t.Errorf("a wait of %s rendered as '%s', expected '%s'", tc.wait, actual, tc.expected)
		}
	}
}

// hostOnly has to cope with an address that carries no port at all (a unix socket, or a
// RemoteAddr a test set by hand), rather than returning "".
func TestHostOnlyPassesThroughAnAddressWithNoPort(t *testing.T) {
	if host := hostOnly("/tmp/hippocampus.sock"); host != "/tmp/hippocampus.sock" {
		t.Errorf("host is '%s', expected the address unchanged", host)
	}
}

func TestRateLimiterFromViperDisabledYieldsNoLimiter(t *testing.T) {
	viper.Reset()
	defer viper.Reset()

	// Set a rate anyway: the enabled flag is the switch, so a config left behind from an experiment
	// must not quietly start throttling.
	viper.Set("rateLimit.global.requestsPerSecond", 100)

	limiter, err := rateLimiterFromViper()
	if err != nil {
		t.Fatalf("unexpected error: %s", err.Error())
	}

	if limiter.Active() {
		t.Error("a limiter was built with rateLimit.enabled unset")
	}
}

func TestRateLimiterFromViperReadsEveryLevel(t *testing.T) {
	viper.Reset()
	defer viper.Reset()

	viper.Set("rateLimit.enabled", true)
	viper.Set("rateLimit.global.requestsPerSecond", 100)
	viper.Set("rateLimit.perClient.requestsPerSecond", 1)
	viper.Set("rateLimit.perClient.burst", 1)
	viper.Set("rateLimit.tiers.reader.requestsPerSecond", 5)

	limiter, err := rateLimiterFromViper()
	if err != nil {
		t.Fatalf("unexpected error: %s", err.Error())
	}

	if !limiter.ArrivalActive() || !limiter.PrincipalActive() {
		t.Fatalf("limiter reports arrival %v / principal %v, expected both active", limiter.ArrivalActive(), limiter.PrincipalActive())
	}

	if !limiter.Principal("client:a", "reader").Allowed {
		t.Fatal("the first request was refused")
	}

	if decision := limiter.Principal("client:a", "reader"); decision.Scope != ratelimit.ScopeClient {
		t.Errorf("the per-client rule was refused with scope '%s', expected '%s'", decision.Scope, ratelimit.ScopeClient)
	}
}

// A misspelled tier is a rule that never applies to anything - a limit that reads as configured and
// is not. It is worth failing startup over, since nothing else would ever reveal it.
func TestRateLimiterFromViperRejectsAnUnknownTier(t *testing.T) {
	viper.Reset()
	defer viper.Reset()

	viper.Set("rateLimit.enabled", true)
	viper.Set("rateLimit.tiers.readers.requestsPerSecond", 5)

	_, err := rateLimiterFromViper()
	if err == nil {
		t.Fatal("expected an error for an unknown tier name")
	}

	if !strings.Contains(err.Error(), "readers") {
		t.Errorf("error '%s' does not name the offending tier", err.Error())
	}
}

func TestRateLimiterFromViperPassesValidationFailuresUp(t *testing.T) {
	viper.Reset()
	defer viper.Reset()

	viper.Set("rateLimit.enabled", true)
	viper.Set("rateLimit.global.requestsPerSecond", -1)

	if _, err := rateLimiterFromViper(); err == nil {
		t.Fatal("expected a negative rate to be rejected")
	}
}

// Enabled with nothing set is a config that reads as protected and is not. It warns rather than
// failing, because it is also the shape of a config part-way through being written.
func TestRateLimiterFromViperWarnsWhenNothingIsLimited(t *testing.T) {
	viper.Reset()
	defer viper.Reset()

	viper.Set("rateLimit.enabled", true)

	hook := logtest.NewGlobal()
	defer hook.Reset()

	previous := log.GetLevel()
	log.SetLevel(log.WarnLevel)

	defer log.SetLevel(previous)

	limiter, err := rateLimiterFromViper()
	if err != nil {
		t.Fatalf("unexpected error: %s", err.Error())
	}

	if limiter.Active() {
		t.Error("a limiter with no rule reports itself active")
	}

	warned := false

	for _, entry := range hook.AllEntries() {
		if entry.Level == log.WarnLevel && strings.Contains(entry.Message, "rateLimit.enabled") {
			warned = true
		}
	}

	if !warned {
		t.Error("no warning was logged for an enabled limiter with nothing configured")
	}
}

// Both enforcement points sit ahead of the two request loggers, so without a line of their own a
// throttled request is invisible in the log at any level. It is at Debug rather than Info because
// a flood would otherwise write a line per rejected request.
func TestARefusalIsLoggedAtDebugWithItsScopeAndPrincipal(t *testing.T) {
	limiter := testLimiter(t, ratelimit.Config{PerClient: ratelimit.Rule{RequestsPerSecond: 0.001, Burst: 1}})
	interceptor := InterceptorRateLimitPrincipal(limiter)

	info := &grpc.UnaryServerInfo{FullMethod: testRPCMethod}
	calls := 0
	ctx := auth.ContextWithClaims(context.Background(), &auth.Claims{ClientID: "alice"})

	previous := log.GetLevel()

	defer log.SetLevel(previous)

	// At the default level the rejection must stay quiet.
	log.SetLevel(log.InfoLevel)

	quiet := logtest.NewGlobal()

	if _, err := interceptor(ctx, nil, info, countingHandler(&calls)); err != nil {
		t.Fatalf("the first request was refused: %s", err.Error())
	}

	if _, err := interceptor(ctx, nil, info, countingHandler(&calls)); err == nil {
		t.Fatal("the second request was admitted")
	}

	for _, entry := range quiet.AllEntries() {
		if strings.Contains(entry.Message, "rate limit") {
			t.Errorf("a rejection was logged at %s, expected nothing above Debug", entry.Level)
		}
	}

	quiet.Reset()

	log.SetLevel(log.DebugLevel)

	hook := logtest.NewGlobal()
	defer hook.Reset()

	if _, err := interceptor(ctx, nil, info, countingHandler(&calls)); err == nil {
		t.Fatal("the third request was admitted")
	}

	entry := hook.LastEntry()
	if entry == nil {
		t.Fatal("nothing was logged at Debug for a refused request")
	}

	if entry.Data["scope"] != string(ratelimit.ScopeClient) {
		t.Errorf("logged scope is %v, expected '%s'", entry.Data["scope"], ratelimit.ScopeClient)
	}

	if entry.Data["principal"] != "client:alice" {
		t.Errorf("logged principal is %v, expected 'client:alice'", entry.Data["principal"])
	}

	if entry.Data["route"] != testRPCMethod {
		t.Errorf("logged route is %v, expected '%s'", entry.Data["route"], testRPCMethod)
	}
}

// The global ceiling does not know who is calling, so it must not log an empty principal field that
// reads as "an unidentified caller was throttled".
func TestAGlobalRefusalLogsNoPrincipal(t *testing.T) {
	limiter := testLimiter(t, ratelimit.Config{Global: ratelimit.Rule{RequestsPerSecond: 0.001, Burst: 1}})
	interceptor := InterceptorRateLimitArrival(limiter)

	info := &grpc.UnaryServerInfo{FullMethod: testRPCMethod}
	calls := 0

	previous := log.GetLevel()
	log.SetLevel(log.DebugLevel)

	defer log.SetLevel(previous)

	hook := logtest.NewGlobal()
	defer hook.Reset()

	if _, err := interceptor(context.Background(), nil, info, countingHandler(&calls)); err != nil {
		t.Fatalf("the first request was refused: %s", err.Error())
	}

	if _, err := interceptor(context.Background(), nil, info, countingHandler(&calls)); err == nil {
		t.Fatal("the second request was admitted")
	}

	entry := hook.LastEntry()
	if entry == nil {
		t.Fatal("nothing was logged at Debug for a refused request")
	}

	if _, present := entry.Data["principal"]; present {
		t.Errorf("a global refusal logged a principal: %v", entry.Data["principal"])
	}
}

// registerRateLimitGauge is called once at wiring time; the observable it registers must survive a
// provider that refuses the instrument rather than taking the process down.
func TestRegisterRateLimitGaugeIsSafe(t *testing.T) {
	limiter := testLimiter(t, ratelimit.Config{PerClient: ratelimit.Rule{RequestsPerSecond: 1}})

	previous := otel.GetMeterProvider()
	defer otel.SetMeterProvider(previous)

	otel.SetMeterProvider(noop.NewMeterProvider())
	registerRateLimitGauge(limiter)

	otel.SetMeterProvider(erroringMeterProvider{})
	registerRateLimitGauge(limiter)
}

// A rate-limited request is the caller sending too much, not the service failing. Classifying it as
// a server fault would page an operator with the error-rate alert every time their own protection
// did its job.
func TestRateLimitRejectionIsAClientFault(t *testing.T) {
	if !isClientFaultCode(codes.ResourceExhausted) {
		t.Error("ResourceExhausted is classified as a server fault")
	}

	if outcome := outcomeForCode(codes.ResourceExhausted); outcome != outcomeClientError {
		t.Errorf("ResourceExhausted counts as '%s', expected '%s'", outcome, outcomeClientError)
	}
}
