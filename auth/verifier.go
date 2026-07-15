package auth

import (
	"fmt"

	"github.com/golang-jwt/jwt/v5"
	log "github.com/sirupsen/logrus"
)

// Verifier checks a bearer token string and returns the claims it carries. It is the seam that
// lets the signing mechanism change - HMACVerifier today, a JWKS-backed RS256 verifier against a
// real identity provider later - without touching the interceptor/middleware call sites.
type Verifier interface {
	Verify(token string) (*Claims, error)
}

// HMACVerifier verifies tokens signed with a single shared HS256 secret.
type HMACVerifier struct {
	secret []byte
}

// minHMACSecretBytes is the shortest secret NewHMACVerifier accepts without warning. HS256 keys
// its HMAC with the raw secret, so a secret shorter than the 256-bit hash output is the weakest
// link and is realistically brute-forceable; 32 bytes matches the algorithm's security level.
const minHMACSecretBytes = 32

// NewHMACVerifier builds an HMACVerifier from a shared secret. An empty secret is rejected here
// rather than left to fail on the first Verify call, since a verifier constructed with an empty
// secret would otherwise accept unsigned or trivially-forged tokens. A short-but-non-empty secret
// is accepted (so an existing deployment keeps working) but warned about, since a weak HS256 secret
// undermines the whole token scheme.
func NewHMACVerifier(secret string) (*HMACVerifier, error) {
	log.Trace("func() auth.NewHMACVerifier")

	if secret == "" {
		return nil, fmt.Errorf("auth: signing secret must not be empty")
	}

	if len(secret) < minHMACSecretBytes {
		log.Warnf("auth: signing secret is only %d bytes - use at least %d for HS256 to resist brute-force", len(secret), minHMACSecretBytes)
	}

	return &HMACVerifier{secret: []byte(secret)}, nil
}

// Verify parses and validates token, returning its claims. The parser is restricted to HS256 via
// jwt.WithValidMethods so a token cannot select its own verification algorithm - a token claiming
// alg "none", or any algorithm this verifier never intended to trust, is rejected outright rather
// than trusted because the token said so. The specific parsing failure is logged for operator
// debugging but not returned to the caller, so a rejected request doesn't leak internal token
// parsing details.
func (v *HMACVerifier) Verify(token string) (*Claims, error) {
	log.Trace("func() auth.HMACVerifier.Verify")

	var claims Claims

	parsed, err := jwt.ParseWithClaims(token, &claims, func(t *jwt.Token) (any, error) {
		return v.secret, nil
	}, jwt.WithValidMethods([]string{"HS256"}))
	if err != nil {
		log.Debugf("token rejected: %s", err.Error())

		return nil, fmt.Errorf("auth: token invalid")
	}

	if !parsed.Valid {
		return nil, fmt.Errorf("auth: token invalid")
	}

	return &claims, nil
}
