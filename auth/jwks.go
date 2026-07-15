package auth

import (
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	log "github.com/sirupsen/logrus"
)

// maxJWKSBytes caps how much of a JWKS or OIDC-discovery response is read before decoding. Both
// documents are small (a handful of keys, or one URL), so a response larger than this is either
// misconfiguration or a hostile/compromised endpoint trying to exhaust memory while a request waits
// on the fetch - either way it must not be read unbounded. It is a var, not a const, only so tests
// can shrink it. 1 MiB is far above any legitimate document.
var maxJWKSBytes = int64(1 << 20)

// discoveryPath is where OpenID Connect Discovery 1.0 places the provider metadata document,
// relative to the issuer URL.
const discoveryPath = "/.well-known/openid-configuration"

// refreshCooldown bounds how often an unknown-kid miss may force an out-of-cycle JWKS re-fetch.
// One forced fetch is what makes key rotation seamless (a token signed by a just-rotated-in key
// arrives before the periodic refresh has seen it), but without a floor between fetches a stream
// of requests bearing fabricated kids could be used to hammer the identity provider.
const refreshCooldown = 30 * time.Second

// httpTimeout caps each JWKS/discovery fetch. A fetch runs while request verification waits on
// it, so an unresponsive identity provider must fail the fetch rather than stall requests
// indefinitely.
const httpTimeout = 10 * time.Second

// JWKSConfig carries the identity-provider settings for NewJWKSVerifier. At least one of JWKSURL
// and Issuer must be set: JWKSURL names the key-set endpoint directly, while Issuer alone
// resolves it via OIDC discovery. When Issuer is set it is also enforced against every token's
// iss claim, and Audience, when set, against aud.
type JWKSConfig struct {
	JWKSURL         string
	Issuer          string
	Audience        string
	RefreshInterval time.Duration
}

// JWKSVerifier verifies RS256 tokens against the RSA keys published at an identity provider's
// JWKS endpoint. Keys are cached by kid and re-fetched lazily: on the first Verify after
// RefreshInterval elapses, and (cooldown-limited) when a token presents a kid the cache doesn't
// hold, which is how a provider-side key rotation is picked up without restarting the service.
type JWKSVerifier struct {
	cfg     JWKSConfig
	jwksURL string
	client  *http.Client

	mu          sync.Mutex
	keys        map[string]*rsa.PublicKey
	lastRefresh time.Time
}

// NewJWKSVerifier builds a JWKSVerifier, resolving the JWKS URL via OIDC discovery when only an
// issuer is configured, and performs the initial key fetch. An unreachable provider fails
// construction rather than starting a service that would reject every request.
func NewJWKSVerifier(cfg JWKSConfig) (*JWKSVerifier, error) {
	log.Trace("func() auth.NewJWKSVerifier")

	if cfg.JWKSURL == "" && cfg.Issuer == "" {
		return nil, fmt.Errorf("auth: idp verification requires auth.jwksUrl or auth.issuer")
	}

	if cfg.RefreshInterval <= 0 {
		return nil, fmt.Errorf("auth: JWKS refresh interval must be positive")
	}

	v := &JWKSVerifier{
		cfg:    cfg,
		client: &http.Client{Timeout: httpTimeout},
	}

	v.jwksURL = cfg.JWKSURL

	if v.jwksURL == "" {
		jwksURL, err := v.discoverJWKSURL()
		if err != nil {
			return nil, err
		}

		v.jwksURL = jwksURL
	}

	if err := v.refresh(false); err != nil {
		return nil, err
	}

	return v, nil
}

// Verify parses and validates token, returning its claims. The parser is restricted to RS256 via
// jwt.WithValidMethods, mirroring how HMACVerifier pins HS256 - a token can never select its own
// verification algorithm. The configured issuer and audience are enforced by the parser itself,
// so an otherwise-valid token minted by a different tenant of the same provider, or for a
// different service, is rejected. As with HMACVerifier, the specific parsing failure is logged
// for operator debugging but not returned to the caller.
func (v *JWKSVerifier) Verify(token string) (*Claims, error) {
	log.Trace("func() auth.JWKSVerifier.Verify")

	if err := v.refresh(false); err != nil {
		log.Debugf("jwks refresh failed, verifying against cached keys: %s", err.Error())
	}

	opts := []jwt.ParserOption{jwt.WithValidMethods([]string{"RS256"})}

	if v.cfg.Issuer != "" {
		opts = append(opts, jwt.WithIssuer(v.cfg.Issuer))
	}

	if v.cfg.Audience != "" {
		opts = append(opts, jwt.WithAudience(v.cfg.Audience))
	}

	var claims Claims

	parsed, err := jwt.ParseWithClaims(token, &claims, v.keyForToken, opts...)
	if err != nil {
		log.Debugf("token rejected: %s", err.Error())

		return nil, fmt.Errorf("auth: token invalid")
	}

	if !parsed.Valid {
		return nil, fmt.Errorf("auth: token invalid")
	}

	return &claims, nil
}

// keyForToken is the parser's key lookup: it selects the cached RSA key matching the token's kid
// header. A kid the cache doesn't hold triggers one cooldown-limited forced refresh before giving
// up, which is what lets a token signed by a freshly rotated key verify on first sight. A token
// with no kid at all is accepted only when the provider publishes exactly one key, since only
// then is the choice unambiguous.
func (v *JWKSVerifier) keyForToken(t *jwt.Token) (any, error) {
	kid, _ := t.Header["kid"].(string)

	if key := v.lookup(kid); key != nil {
		return key, nil
	}

	if err := v.refresh(true); err != nil {
		log.Debugf("jwks refresh failed: %s", err.Error())
	}

	if key := v.lookup(kid); key != nil {
		return key, nil
	}

	return nil, fmt.Errorf("auth: no key matches token")
}

// lookup returns the cached key for kid, the sole cached key when the token carried no kid and
// exactly one key is published, or nil.
func (v *JWKSVerifier) lookup(kid string) *rsa.PublicKey {
	v.mu.Lock()
	defer v.mu.Unlock()

	if kid != "" {
		return v.keys[kid]
	}

	if len(v.keys) == 1 {
		for _, key := range v.keys {
			return key
		}
	}

	return nil
}

// refresh re-fetches the key set if it is due: past RefreshInterval normally, or past
// refreshCooldown when forced by an unknown kid. lastRefresh is advanced before fetching, under
// the lock, so concurrent verifications never stack duplicate fetches - and a failed fetch still
// counts against the cooldown, keeping a provider outage from turning every request into a fetch
// attempt. On failure the previously cached keys stay in service.
func (v *JWKSVerifier) refresh(force bool) error {
	v.mu.Lock()

	window := v.cfg.RefreshInterval
	if force {
		window = refreshCooldown
	}

	if time.Since(v.lastRefresh) < window {
		v.mu.Unlock()

		return nil
	}

	v.lastRefresh = time.Now()
	v.mu.Unlock()

	keys, err := v.fetchKeys()
	if err != nil {
		return err
	}

	v.mu.Lock()
	v.keys = keys
	v.mu.Unlock()

	log.Debugf("jwks refreshed: %d signing keys cached", len(keys))

	return nil
}

// fetchKeys downloads and parses the JWKS document, returning the usable RSA signing keys by
// kid. Non-RSA keys and keys marked for a non-signature use are skipped; a document yielding no
// usable keys is an error, since accepting it would empty the cache and reject every token.
func (v *JWKSVerifier) fetchKeys() (map[string]*rsa.PublicKey, error) {
	log.Trace("func() auth.JWKSVerifier.fetchKeys")

	resp, err := v.client.Get(v.jwksURL)
	if err != nil {
		return nil, fmt.Errorf("auth: failed to fetch JWKS from %s: %s", v.jwksURL, err.Error())
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("auth: JWKS endpoint %s returned status %d", v.jwksURL, resp.StatusCode)
	}

	var doc struct {
		Keys []struct {
			Kty string `json:"kty"`
			Kid string `json:"kid"`
			Use string `json:"use"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}

	if err := json.NewDecoder(io.LimitReader(resp.Body, maxJWKSBytes)).Decode(&doc); err != nil {
		return nil, fmt.Errorf("auth: failed to parse JWKS document: %s", err.Error())
	}

	keys := make(map[string]*rsa.PublicKey, len(doc.Keys))

	for _, jwk := range doc.Keys {
		if jwk.Kty != "RSA" || (jwk.Use != "" && jwk.Use != "sig") {
			continue
		}

		key, err := rsaKeyFromJWK(jwk.N, jwk.E)
		if err != nil {
			log.Warnf("skipping unparseable JWKS key '%s': %s", jwk.Kid, err.Error())

			continue
		}

		keys[jwk.Kid] = key
	}

	if len(keys) == 0 {
		return nil, fmt.Errorf("auth: JWKS document contains no usable RSA signing keys")
	}

	return keys, nil
}

// rsaKeyFromJWK builds an RSA public key from a JWK's base64url-encoded modulus and exponent.
func rsaKeyFromJWK(n string, e string) (*rsa.PublicKey, error) {
	nBytes, err := base64.RawURLEncoding.DecodeString(n)
	if err != nil {
		return nil, fmt.Errorf("invalid modulus: %s", err.Error())
	}

	eBytes, err := base64.RawURLEncoding.DecodeString(e)
	if err != nil {
		return nil, fmt.Errorf("invalid exponent: %s", err.Error())
	}

	if len(nBytes) == 0 || len(eBytes) == 0 {
		return nil, fmt.Errorf("empty modulus or exponent")
	}

	return &rsa.PublicKey{
		N: new(big.Int).SetBytes(nBytes),
		E: int(new(big.Int).SetBytes(eBytes).Int64()),
	}, nil
}

// discoverJWKSURL resolves the provider's JWKS endpoint from its OIDC discovery document,
// for configurations that name only the issuer.
func (v *JWKSVerifier) discoverJWKSURL() (string, error) {
	log.Trace("func() auth.JWKSVerifier.discoverJWKSURL")

	discoveryURL := strings.TrimSuffix(v.cfg.Issuer, "/") + discoveryPath

	resp, err := v.client.Get(discoveryURL)
	if err != nil {
		return "", fmt.Errorf("auth: failed to fetch OIDC discovery document from %s: %s", discoveryURL, err.Error())
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("auth: OIDC discovery endpoint %s returned status %d", discoveryURL, resp.StatusCode)
	}

	var doc struct {
		JWKSURI string `json:"jwks_uri"`
	}

	if err := json.NewDecoder(io.LimitReader(resp.Body, maxJWKSBytes)).Decode(&doc); err != nil {
		return "", fmt.Errorf("auth: failed to parse OIDC discovery document: %s", err.Error())
	}

	if doc.JWKSURI == "" {
		return "", fmt.Errorf("auth: OIDC discovery document from %s has no jwks_uri", discoveryURL)
	}

	return doc.JWKSURI, nil
}
