package auth

import (
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	log "github.com/sirupsen/logrus"
)

// MintToken signs and returns a new bearer token for clientID, valid for ttl from now. Minting
// only needs the shared secret - it has no dependency on a Verifier - so callers that only need
// to issue tokens (the --mint-token CLI mode) don't need to construct one.
func MintToken(secret string, clientID string, ttl time.Duration) (string, error) {
	log.Trace("func() auth.MintToken")

	if secret == "" {
		return "", fmt.Errorf("auth: signing secret must not be empty")
	}

	now := time.Now()

	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
		},
		ClientID: clientID,
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)

	signed, err := token.SignedString([]byte(secret))
	if err != nil {
		return "", fmt.Errorf("auth: failed to sign token")
	}

	return signed, nil
}
