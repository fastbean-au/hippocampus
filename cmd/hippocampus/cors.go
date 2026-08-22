package main

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// The CORS response headers and the preflight cache lifetime. The allowed header set is fixed
// rather than echoed back from Access-Control-Request-Headers: the gateway reads exactly these two,
// and reflecting whatever a caller asks for would make the allow-list describe the request instead
// of the server.
const (
	corsAllowedMethods  = "GET, POST, PUT, PATCH, DELETE, OPTIONS"
	corsAllowedHeaders  = "Authorization, Content-Type"
	corsPreflightMaxAge = "600"
)

// parseCORSOrigins normalises whatever gateway.corsOrigins was written as into a list of origins.
//
// It accepts both shapes on purpose. A configuration file (and the wizard) writes a JSON array,
// which viper hands back as a []any; an environment override is necessarily a single string, and
// viper does not split one into a slice - cast.ToStringSlice would break it on whitespace alone, so
// the comma an operator will reach for first would silently become part of an origin. Splitting on
// both here is what makes HIPPOCAMPUS_GATEWAY_CORSORIGINS behave the way its value looks.
func parseCORSOrigins(value any) []string {
	var raw []string

	switch v := value.(type) {

	case nil:
		return nil

	case string:
		raw = strings.FieldsFunc(v, func(r rune) bool {
			return r == ',' || r == ' ' || r == '\t' || r == '\n' || r == '\r'
		})

	case []string:
		raw = v

	case []any:
		for _, item := range v {
			s, ok := item.(string)
			if !ok {
				continue
			}

			raw = append(raw, s)
		}

	default:
		return nil

	}

	origins := make([]string, 0, len(raw))

	for _, v := range raw {
		origin := strings.TrimSpace(v)

		if origin == "" {
			continue
		}

		origins = append(origins, origin)
	}

	return origins
}

// validateCORSOrigins refuses anything that is not an exact origin.
//
// The wildcard is rejected rather than supported. A browser already refuses to combine "*" with
// credentials, so it would not be the danger it looks like - but this gateway fronts Purge, and an
// operator reaching for "*" to make a tool work is not making the decision the wildcard actually
// represents. Naming the origins is cheap, and it is the same default-closed exact-match reasoning
// webUIAssetPaths and the auth open-list already run on.
func validateCORSOrigins(origins []string) error {
	for _, v := range origins {
		if v == "*" {
			return fmt.Errorf("gateway.corsOrigins may not contain \"*\": list the exact origins that may call this gateway (e.g. \"http://localhost:8082\")")
		}

		parsed, err := url.Parse(v)
		if err != nil {
			return fmt.Errorf("gateway.corsOrigins entry %q is not a valid origin: %w", v, err)
		}

		if parsed.Scheme != "http" && parsed.Scheme != "https" {
			return fmt.Errorf("gateway.corsOrigins entry %q must use the http or https scheme", v)
		}

		if parsed.Host == "" {
			return fmt.Errorf("gateway.corsOrigins entry %q names no host", v)
		}

		if parsed.User != nil {
			return fmt.Errorf("gateway.corsOrigins entry %q must not carry credentials", v)
		}

		// An origin is scheme://host[:port] and nothing else. A trailing slash is the common way to
		// get this wrong, and it never matches the Origin header a browser sends, so it would
		// present as CORS simply not working rather than as a configuration error.
		if parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
			return fmt.Errorf("gateway.corsOrigins entry %q must be scheme://host[:port] with no path, query or fragment (no trailing slash)", v)
		}
	}

	return nil
}

// isCORSPath reports whether a gateway request is on the surface CORS applies to. It is the /v1
// prefix whole, unlike isRPCPath: the OpenAPI document is exactly what a browser client fetches
// cross-origin first, and swagger-ui resolves its "Try it out" calls against the origin that served
// that document - so excluding it would leave the page unable to load the spec at all.
//
// The console, the probes and the login endpoints are deliberately outside it. They are same-origin
// surfaces of this service and nothing should be reaching them from another origin.
func isCORSPath(path string) bool {
	return strings.HasPrefix(path, "/v1/")
}

// corsMiddleware answers cross-origin requests for the configured origins, and is installed only
// when at least one is configured - so a deployment that has not asked for this is byte-for-byte
// unchanged.
//
// Its position in the chain is load-bearing, and every layer below it rules out a lower one. It sits
// OUTSIDE authentication, because a preflight carries no Authorization header by construction and
// would be rejected before it could be answered. It sits outside the arrival rate limiter, or a
// throttled preflight becomes an opaque browser failure instead of a legible 429. It sits outside
// httpMetricsMiddleware, because a preflight is not an RPC: gatewayRouteMiddleware resolves no route
// for OPTIONS, so counting them would push a stream of rpc="unknown" into the very series the
// shipped alert rules read. And it sits INSIDE recoverMiddleware, so a panic here is still a clean
// 500.
//
// Access-Control-Allow-Credentials is deliberately never sent. That is what keeps the console's
// HttpOnly session cookie unusable from another origin: without it a browser will not attach
// credentials to a cross-origin request, so enabling this cannot turn a logged-in console session
// into ambient authority for a hostile page. Tokens still work, because a bearer token is sent
// explicitly by the caller rather than attached by the browser.
func corsMiddleware(origins []string, next http.Handler) http.Handler {
	allowed := make(map[string]bool, len(origins))

	for _, v := range origins {
		allowed[v] = true
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isCORSPath(r.URL.Path) {
			next.ServeHTTP(w, r)

			return
		}

		origin := r.Header.Get("Origin")

		// Vary is set whether or not the origin matched, and whether or not there was one: the
		// response body is identical either way but the headers are not, so a cache that ignored
		// Origin could serve one caller's allow header to another.
		w.Header().Add("Vary", "Origin")

		if origin != "" && allowed[origin] {
			w.Header().Set("Access-Control-Allow-Origin", origin)
		}

		// A preflight is answered here and never forwarded. It is identified by the request method
		// the browser is asking about rather than by OPTIONS alone, so a genuine OPTIONS call on the
		// RPC surface would still reach the mux.
		if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
			// The allow headers are only added for an origin on the list. An unlisted origin gets a
			// 204 carrying none of them, which is what makes the browser refuse the real request.
			if origin != "" && allowed[origin] {
				w.Header().Set("Access-Control-Allow-Methods", corsAllowedMethods)
				w.Header().Set("Access-Control-Allow-Headers", corsAllowedHeaders)
				w.Header().Set("Access-Control-Max-Age", corsPreflightMaxAge)
			}

			w.WriteHeader(http.StatusNoContent)

			return
		}

		// The allow header is set BEFORE the request is served, so a 401 from auth, a 429 from the
		// limiter or a 503 from the purge gate still carries it. Without that the browser reports an
		// opaque CORS failure and the caller never sees the status that actually explains it.
		next.ServeHTTP(w, r)
	})
}
