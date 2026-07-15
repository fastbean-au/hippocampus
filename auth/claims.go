package auth

import (
	"github.com/golang-jwt/jwt/v5"
)

// Claims embeds the standard registered claims (exp, iat, ...) plus a client identifier. ClientID
// is not consumed by any request handling today - it exists so tokens already carry a client
// identity claim when multi-tenancy needs one, rather than requiring every issued token to be
// reminted later.
type Claims struct {
	jwt.RegisteredClaims

	ClientID string `json:"client_id"`
}
