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
func HTTPMiddleware(v Verifier, next http.Handler, openPaths []string) http.Handler {
	open := make(map[string]bool, len(openPaths))
	for _, p := range openPaths {
		open[p] = true
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if open[r.URL.Path] {
			next.ServeHTTP(w, r)

			return
		}

		token, err := ExtractBearerToken(r.Header.Get("Authorization"))
		if err != nil {
			log.Trace("rejecting request - malformed authorization header")
			unauthorized(w)

			return
		}

		if _, err := v.Verify(token); err != nil {
			log.Trace("rejecting request - invalid token")
			unauthorized(w)

			return
		}

		next.ServeHTTP(w, r)
	})
}

// unauthorized writes a 401 response carrying the WWW-Authenticate header RFC 6750 requires for
// bearer-token schemes, plus a small JSON body matching the gateway's existing JSON error style.
func unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", "Bearer")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized"})
}
