package auth

import (
	"fmt"
	"net/http"
	"strings"
)

// bearerPrefix is matched case-insensitively, per RFC 6750 (the scheme itself is case-sensitive
// in the RFC, but real clients disagree often enough - curl's -H, various SDKs - that a strict
// match rejects legitimate requests for no security benefit).
const bearerPrefix = "bearer "

// ExtractBearerToken pulls the token out of an Authorization header value (or the equivalent gRPC
// metadata value) of the form "Bearer <token>". It is shared by the gRPC interceptor and the HTTP
// middleware so a malformed header is rejected identically on both transports.
func ExtractBearerToken(headerValue string) (string, error) {
	if headerValue == "" {
		return "", fmt.Errorf("auth: missing authorization header")
	}

	if len(headerValue) <= len(bearerPrefix) || !strings.EqualFold(headerValue[:len(bearerPrefix)], bearerPrefix) {
		return "", fmt.Errorf("auth: authorization header must be of the form 'Bearer <token>'")
	}

	token := strings.TrimSpace(headerValue[len(bearerPrefix):])
	if token == "" {
		return "", fmt.Errorf("auth: authorization header must be of the form 'Bearer <token>'")
	}

	return token, nil
}

// tokenFromRequest pulls the bearer token from an HTTP request, preferring the Authorization header
// and falling back to the named session cookie when the header is absent and a cookie name is
// configured. The header always wins, so an explicit Authorization header (an API client, or a
// grpc-gateway caller) is never overridden by a stale cookie. It is shared by the HTTP auth
// middleware so header and cookie callers authenticate through the identical Verify path.
func tokenFromRequest(r *http.Request, sessionCookie string) (string, error) {
	if header := r.Header.Get("Authorization"); header != "" {
		return ExtractBearerToken(header)
	}

	if sessionCookie != "" {
		if cookie, err := r.Cookie(sessionCookie); err == nil && cookie.Value != "" {
			return cookie.Value, nil
		}
	}

	return "", fmt.Errorf("auth: missing bearer token")
}
