package auth

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	log "github.com/sirupsen/logrus"
)

// MintRequest carries everything MintToken needs to issue a token. It is a struct rather than a
// positional parameter list so the signing key can be identified by kid without pushing the
// signature past the project's parameter-count limit.
type MintRequest struct {
	// Secret is the raw HS256 signing secret. Required.
	Secret string

	// Kid, when non-empty, is written as the token's "kid" header so a multi-key verifier can
	// select the matching secret. Empty mints a header-less token, verified by a verifier's
	// legacy single secret - the pre-rotation behaviour.
	Kid string

	// ClientID is stamped into the token's client_id claim, identifying the bearer for logging
	// and per-client revocation.
	ClientID string

	// TTL is how long the token stays valid from the moment it is minted.
	TTL time.Duration
}

// MintToken signs and returns a new bearer token described by req, valid for req.TTL from now.
// Minting only needs the signing secret - it has no dependency on a Verifier - so callers that only
// need to issue tokens (the --mint-token CLI mode) don't need to construct one. Every token is
// given a random jti so an individual token can later be revoked by id.
func MintToken(req MintRequest) (string, error) {
	log.Trace("func() auth.MintToken")

	if req.Secret == "" {
		return "", fmt.Errorf("auth: signing secret must not be empty")
	}

	jti, err := newTokenID()
	if err != nil {
		return "", err
	}

	now := time.Now()

	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			ID:        jti,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(req.TTL)),
		},
		ClientID: req.ClientID,
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)

	if req.Kid != "" {
		token.Header["kid"] = req.Kid
	}

	signed, err := token.SignedString([]byte(req.Secret))
	if err != nil {
		return "", fmt.Errorf("auth: failed to sign token")
	}

	return signed, nil
}

// TokenID reports the jti of a minted token by parsing it without verifying its signature. The
// --mint-token CLI uses it to print the jti alongside the token so an operator can record it for
// later revocation. It never trusts the token - the id is opaque metadata, not an authorization
// decision - so skipping verification here is safe.
func TokenID(token string) (string, error) {
	var claims Claims

	parser := jwt.NewParser()

	if _, _, err := parser.ParseUnverified(token, &claims); err != nil {
		return "", fmt.Errorf("auth: could not parse token")
	}

	return claims.ID, nil
}

// newTokenID returns a random 128-bit identifier, hex-encoded, for use as a token's jti.
func newTokenID() (string, error) {
	buf := make([]byte, 16)

	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("auth: failed to generate token id")
	}

	return hex.EncodeToString(buf), nil
}
