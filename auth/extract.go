package auth

import (
	"fmt"
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
