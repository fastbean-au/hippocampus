package auth

import (
	"encoding/json"
	"net/http"

	log "github.com/sirupsen/logrus"
)

// HTTPMiddleware wraps next so every request requires a valid bearer token in the Authorization
// header, except for the paths listed in openPaths (an exact match against the request URL path -
// used for liveness endpoints like /healthz that orchestrators must be able to reach without a
// token). Unlike UnaryServerInterceptor's prefix-based scoping, this is closed by default: any
// path not in openPaths requires a token, including new endpoints added later without remembering
// to update an allow-list of what's protected.
//
// sessionCookie, when non-empty, names a cookie the middleware falls back to for the token when the
// request carries no Authorization header - the seam the server-side OIDC login uses so a browser
// that signed in via /auth/login is authenticated by its HttpOnly session cookie alone, without the
// token ever living in page-readable storage. An empty name disables the fallback, preserving the
// header-only behaviour for hmac and header-bearing clients.
func HTTPMiddleware(v Verifier, next http.Handler, openPaths []string, sessionCookie string) http.Handler {
	open := make(map[string]bool, len(openPaths))
	for _, p := range openPaths {
		open[p] = true
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if open[r.URL.Path] {
			next.ServeHTTP(w, r)

			return
		}

		token, fromCookie, err := tokenFromRequest(r, sessionCookie)
		if err != nil {
			log.Trace("rejecting request - no bearer token in header or session cookie")
			unauthorized(w)

			return
		}

		// A cookie is ambient authority: the browser attaches it to whatever request a page makes,
		// and SameSite=Lax stops that only across SITES - a page on another port of this host, or on
		// a sibling subdomain, is same-site, and its plain-text POST arrives with the cookie and is
		// decoded as JSON. So a cookie-borne write must come from this origin (TODO-3 item 145).
		// CrossOriginProtection reads the Sec-Fetch-Site the browser sets (else Origin against Host),
		// passes safe methods, and passes a request carrying neither header, which no browser sends.
		if fromCookie {
			if err := crossOrigin.Check(r); err != nil {
				log.WithField("path", r.URL.Path).
					Warn("refused a cross-origin write authenticated by the session cookie")
				forbiddenCrossOrigin(w)

				return
			}
		}

		claims, err := v.Verify(token)
		if err != nil {
			log.Trace("rejecting request - invalid token")
			unauthorized(w)

			return
		}

		// Stash the verified claims on the request context so the logging middleware inside this one
		// can attribute the request to the authenticated client.
		next.ServeHTTP(w, r.WithContext(ContextWithClaims(r.Context(), claims)))
	})
}

// crossOrigin is the CSRF check applied to cookie-authenticated requests. It is stateless and safe
// for concurrent use, so one instance serves every middleware.
var crossOrigin = http.NewCrossOriginProtection()

// forbiddenCrossOrigin writes the 403 a refused cross-origin cookie write gets, in the same JSON
// shape as unauthorized.
func forbiddenCrossOrigin(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": "cross-origin request refused"})
}

// unauthorized writes a 401 response carrying the WWW-Authenticate header RFC 6750 requires for
// bearer-token schemes, plus a small JSON body matching the gateway's existing JSON error style.
func unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", "Bearer")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized"})
}
