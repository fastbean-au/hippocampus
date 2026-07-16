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

// SigningKey is one kid-identified HS256 secret. A verifier trusts every configured key, so a new
// key can be introduced (and start minting) while tokens signed by the previous key still verify -
// the basis of secret rotation without a flag day.
type SigningKey struct {
	Kid    string
	Secret string
}

// HMACConfig configures an HMACVerifier. LegacySecret and Keys are complementary, not exclusive:
// Keys verify tokens carrying a matching kid header, while LegacySecret verifies tokens with no kid
// header (every token minted before keyed rotation existed). A config with only LegacySecret set
// reproduces the original single-secret behaviour exactly.
type HMACConfig struct {
	// LegacySecret verifies (and, absent any Keys, mints) tokens that carry no kid header.
	LegacySecret string

	// Keys are the kid-identified signing secrets. Every one verifies; minting selects ActiveKid.
	Keys []SigningKey

	// ActiveKid names the key in Keys used for minting. It is validated (must name a real key) but
	// not otherwise consumed by the verifier - the caller reads it to decide which key to mint with.
	ActiveKid string
}

// HMACVerifier verifies tokens signed with one or more shared HS256 secrets. Tokens carrying a kid
// header are verified against the matching keyed secret; tokens with no kid are verified against
// the legacy secret when one is configured.
type HMACVerifier struct {
	legacySecret []byte
	keys         map[string][]byte
}

// minHMACSecretBytes is the shortest secret NewHMACVerifier accepts without warning. HS256 keys
// its HMAC with the raw secret, so a secret shorter than the 256-bit hash output is the weakest
// link and is realistically brute-forceable; 32 bytes matches the algorithm's security level.
const minHMACSecretBytes = 32

// NewHMACVerifier builds an HMACVerifier from cfg. At least one secret must be present across
// LegacySecret and Keys, else the verifier would accept unsigned or trivially-forged tokens; this
// is rejected here rather than left to fail on the first Verify call. Each key's kid must be
// non-empty and unique (an empty kid is what LegacySecret is for), and ActiveKid, when set, must
// name a configured key. A short-but-non-empty secret is accepted (so an existing deployment keeps
// working) but warned about, since a weak HS256 secret undermines the whole token scheme.
func NewHMACVerifier(cfg HMACConfig) (*HMACVerifier, error) {
	log.Trace("func() auth.NewHMACVerifier")

	if cfg.LegacySecret == "" && len(cfg.Keys) == 0 {
		return nil, fmt.Errorf("auth: at least one signing secret must be configured")
	}

	v := &HMACVerifier{
		keys: make(map[string][]byte, len(cfg.Keys)),
	}

	if cfg.LegacySecret != "" {
		warnIfShortSecret("signingSecret", cfg.LegacySecret)

		v.legacySecret = []byte(cfg.LegacySecret)
	}

	for i, k := range cfg.Keys {
		if k.Kid == "" {
			return nil, fmt.Errorf("auth: signingKeys[%d] has an empty kid - use signingSecret for a key without a kid", i)
		}

		if k.Secret == "" {
			return nil, fmt.Errorf("auth: signingKeys[%d] (kid %q) has an empty secret", i, k.Kid)
		}

		if _, exists := v.keys[k.Kid]; exists {
			return nil, fmt.Errorf("auth: duplicate signing key kid %q", k.Kid)
		}

		warnIfShortSecret(fmt.Sprintf("signingKeys kid %q", k.Kid), k.Secret)

		v.keys[k.Kid] = []byte(k.Secret)
	}

	if cfg.ActiveKid != "" {
		if _, ok := v.keys[cfg.ActiveKid]; !ok {
			return nil, fmt.Errorf("auth: activeKid %q does not name a configured signing key", cfg.ActiveKid)
		}
	}

	return v, nil
}

// warnIfShortSecret logs a warning when secret is below the HS256 security level. label names the
// source (signingSecret, or a specific kid) so the operator can tell which key to strengthen.
func warnIfShortSecret(label string, secret string) {
	if len(secret) < minHMACSecretBytes {
		log.Warnf("auth: %s is only %d bytes - use at least %d for HS256 to resist brute-force", label, len(secret), minHMACSecretBytes)
	}
}

// Verify parses and validates token, returning its claims. The parser is restricted to HS256 via
// jwt.WithValidMethods so a token cannot select its own verification algorithm - a token claiming
// alg "none", or any algorithm this verifier never intended to trust, is rejected outright rather
// than trusted because the token said so. The signing key is chosen by the token's kid header (or
// the legacy secret when there is none); an unknown kid is rejected. The specific parsing failure
// is logged for operator debugging but not returned to the caller, so a rejected request doesn't
// leak internal token parsing details.
func (v *HMACVerifier) Verify(token string) (*Claims, error) {
	log.Trace("func() auth.HMACVerifier.Verify")

	var claims Claims

	parsed, err := jwt.ParseWithClaims(token, &claims, v.keyForToken, jwt.WithValidMethods([]string{"HS256"}))
	if err != nil {
		log.Debugf("token rejected: %s", err.Error())

		return nil, fmt.Errorf("auth: token invalid")
	}

	if !parsed.Valid {
		return nil, fmt.Errorf("auth: token invalid")
	}

	return &claims, nil
}

// keyForToken is the jwt keyfunc: it selects the HS256 secret to verify t against, based on t's kid
// header. A token with no kid uses the legacy secret; a token with a kid uses the matching keyed
// secret. A kid that names no configured key, or a missing legacy secret, is an error - the
// verifier never falls back to a secret the token didn't ask for.
func (v *HMACVerifier) keyForToken(t *jwt.Token) (any, error) {
	raw, ok := t.Header["kid"]

	if !ok {
		if v.legacySecret == nil {
			return nil, fmt.Errorf("auth: token has no kid and no legacy signing secret is configured")
		}

		return v.legacySecret, nil
	}

	kid, ok := raw.(string)
	if !ok {
		return nil, fmt.Errorf("auth: token kid header is not a string")
	}

	secret, ok := v.keys[kid]
	if !ok {
		return nil, fmt.Errorf("auth: token kid %q names no configured signing key", kid)
	}

	return secret, nil
}
