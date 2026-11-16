package main

import (
	"fmt"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/viper"

	"github.com/fastbean-au/hippocampus/auth"
)

// warnOnUnconstrainedIdP warns when an idp deployment verifies nothing but the signature.
//
// A token is only proof of what a provider signed, not of who it was signed FOR. With auth.audience
// unset, jwt.WithAudience is never applied, so EVERY RS256 token the provider's keys signed
// verifies here - including one minted for a different application in the same tenant. In an
// organisation with a single Okta/Entra/Keycloak behind several services, that is any employee's
// token for any of them, with this service's tier then resolved from whatever roles claim it
// happens to carry. Setting auth.audience to this service's own identifier is what makes a token
// mean "for Hippocampus".
//
// The same applies to auth.issuer, one step weaker: configuring jwksUrl alone (rather than letting
// discovery derive it from the issuer) leaves iss unchecked too, so the trust is in the key set
// alone.
//
// A warning rather than a refusal, matching the short-signing-secret and auth-without-TLS
// precedents: a single-purpose IdP whose tokens are only ever this service's is a legitimate
// deployment, and there is no way to tell one from the other here.
func warnOnUnconstrainedIdP(audience string, issuer string) {
	if audience == "" {
		log.Warn("auth.method is 'idp' but auth.audience is not set - every token the provider " +
			"signs will verify, including tokens minted for other applications in the same tenant: " +
			"set auth.audience to this service's identifier")
	}

	if issuer == "" {
		log.Warn("auth.method is 'idp' but auth.issuer is not set - the 'iss' claim is not checked, " +
			"so trust rests entirely on the configured JWKS key set")
	}
}

// warnOnBuiltinRoles warns when an idp deployment maps its provider's roles onto tiers but still
// lets the built-in names reader, writer and admin grant them as well.
//
// A roleMapping says the provider's role names are not the tier names. Many providers share one
// role namespace across applications, though: on Keycloak, auth.roleClaim realm_access.roles holds
// every realm role, and an unrelated one called "admin" is common. While the built-ins are on, its
// holder is a Hippocampus admin, with Purge, Clear and Export. auth.builtinRoles false removes them.
//
// No warning is given for a built-in name the mapping itself names, since the mapping then decides
// what it grants. An empty mapping is not warned about either: there the built-in names are the only
// way to grant a tier, so the provider must be issuing them on purpose.
func warnOnBuiltinRoles(roleMapping map[string]string, builtinRoles bool) {
	if !builtinRoles || len(roleMapping) == 0 {
		return
	}

	mapped := make(map[string]struct{}, len(roleMapping))

	for role := range roleMapping {
		mapped[strings.ToLower(strings.TrimSpace(role))] = struct{}{}
	}

	var active []string

	for _, name := range auth.TierNames() {
		if _, ok := mapped[name]; !ok {
			active = append(active, name)
		}
	}

	if len(active) == 0 {
		return
	}

	log.Warnf("auth.roleMapping is set but auth.builtinRoles is true - a token holding a role named %s "+
		"is granted that tier, even when the provider issues the role for another application: set "+
		"auth.builtinRoles to false to grant tiers through auth.roleMapping alone",
		strings.Join(active, ", "))
}

// topologyTierOverride turns topology.minimumTier into the per-RPC tier override NewAuthoriser
// takes. It returns nil - meaning "leave the declared policy alone" - when the key is unset or the
// RPC is disabled entirely, so a deployment that never touches the key gets exactly the policy
// table's default. An unparseable tier is NOT rejected here: it is passed through so the authoriser
// refuses it by name, keeping one error message for a bad tier wherever it was written.
func topologyTierOverride() map[string]string {
	if !viper.GetBool("topology.enabled") {
		return nil
	}

	tier := strings.TrimSpace(viper.GetString("topology.minimumTier"))
	if tier == "" {
		return nil
	}

	return map[string]string{"GetTopology": tier}
}

// hmacConfigFromViper reads the HMAC signing configuration into an auth.HMACConfig. Centralising the
// viper access here (per the project convention that all viper reads live in cmd/hippocampus) means the
// --mint-token CLI and the running verifier share one interpretation of signingSecret, signingKeys,
// and activeKid.
func hmacConfigFromViper() auth.HMACConfig {
	var keys []auth.SigningKey

	if err := viper.UnmarshalKey("auth.signingKeys", &keys); err != nil {
		log.Fatalf("failed to read auth.signingKeys: %s", err.Error())
	}

	return auth.HMACConfig{
		LegacySecret: viper.GetString("auth.signingSecret"),
		Keys:         keys,
		ActiveKid:    viper.GetString("auth.activeKid"),
	}
}

// oauth2Setup is the server-side OIDC login's configuration: the login itself, less the id_token
// verifier, which newOAuth2Login builds from IDToken.
type oauth2Setup struct {
	Login   auth.OAuth2Config
	IDToken auth.JWKSConfig
}

// oauth2SetupFromViper reads the auth.oauth2 config block. The issuer and audience default to the
// top-level auth.issuer/auth.audience the token verifier already uses, so a single-provider
// deployment only has to set the client credentials and redirect URL. The Secure cookie attribute
// follows the redirect URL's scheme unless auth.oauth2.cookieSecure is set explicitly, so a Secure
// cookie is not silently dropped on a local http rig nor omitted on an https deployment.
func oauth2SetupFromViper(refreshInterval time.Duration) oauth2Setup {
	issuer := viper.GetString("auth.oauth2.issuer")

	if issuer == "" {
		issuer = viper.GetString("auth.issuer")
	}

	audience := viper.GetString("auth.oauth2.audience")

	if audience == "" {
		audience = viper.GetString("auth.audience")
	}

	clientID := viper.GetString("auth.oauth2.clientId")

	scopes := strings.Fields(viper.GetString("auth.oauth2.scopes"))

	if len(scopes) == 0 {
		scopes = []string{"openid", "profile", "email", "offline_access"}
	}

	redirectURL := viper.GetString("auth.oauth2.redirectUrl")

	cookieSecure := strings.HasPrefix(strings.ToLower(redirectURL), "https")

	if viper.IsSet("auth.oauth2.cookieSecure") {
		cookieSecure = viper.GetBool("auth.oauth2.cookieSecure")
	}

	return oauth2Setup{
		Login: auth.OAuth2Config{
			Issuer:                issuer,
			ClientID:              clientID,
			ClientSecret:          viper.GetString("auth.oauth2.clientSecret"),
			RedirectURL:           redirectURL,
			Scopes:                scopes,
			Audience:              audience,
			CookieSecure:          cookieSecure,
			CookieDomain:          viper.GetString("auth.oauth2.cookieDomain"),
			PostLogoutRedirectURL: viper.GetString("auth.oauth2.postLogoutRedirectUrl"),
			SuccessRedirectURL:    viper.GetString("auth.oauth2.successRedirectUrl"),
			SessionTTL:            time.Duration(viper.GetInt("auth.oauth2.sessionTTLSeconds")) * time.Second,
			RefreshTTL:            time.Duration(viper.GetInt("auth.oauth2.refreshTTLSeconds")) * time.Second,
		},

		// The id_token carries the client id as its audience, so it needs its own verifier
		// distinct from the access-token one (which enforces the API audience). Both discover from
		// the same issuer and share the JWKS fetch/cache logic.
		IDToken: auth.JWKSConfig{
			JWKSURL:         viper.GetString("auth.jwksUrl"),
			Issuer:          issuer,
			Audience:        clientID,
			RefreshInterval: refreshInterval,
			RoleClaim:       viper.GetString("auth.roleClaim"),
		},
	}
}

// newOAuth2Login assembles the server-side OIDC login, building the dedicated id_token verifier it
// checks the provider's id_token with.
func newOAuth2Login(setup oauth2Setup) (*auth.OAuth2Login, error) {
	idTokenVerifier, err := auth.NewJWKSVerifier(setup.IDToken)
	if err != nil {
		return nil, fmt.Errorf("failed to build id_token verifier: %w", err)
	}

	cfg := setup.Login
	cfg.IDTokenVerifier = idTokenVerifier

	return auth.NewOAuth2Login(cfg)
}

// authConfig is what initAuth needs to build the verifier, the authoriser and the optional login.
type authConfig struct {
	// Method is the resolved auth.method: "none", "hmac" or "idp".
	Method string

	HMAC auth.HMACConfig
	JWKS auth.JWKSConfig

	// OAuth2 is the server-side login's configuration, set only under idp with auth.oauth2.enabled.
	OAuth2 *oauth2Setup

	RevocationFile    string
	RevocationRefresh time.Duration
	RequireGroupScope bool

	RoleMapping  map[string]string
	TierOverride map[string]string

	// BuiltinRoles is auth.builtinRoles: whether reader, writer and admin grant their own tiers. It
	// is read only under idp; --mint-token stamps those names, so hmac always keeps them.
	BuiltinRoles bool

	// TLSEnabled is read alongside, since authentication without TLS is warned about.
	TLSEnabled bool
}

// authSetup is what initAuth built. Verifier and Authoriser are both nil with auth off.
type authSetup struct {
	Method      string
	Verifier    auth.Verifier
	Authoriser  *auth.Authoriser
	OAuth2Login *auth.OAuth2Login
	Revocations *auth.RevocationList
}

// enabled reports whether requests must carry a verified token.
func (a authSetup) enabled() bool {
	return a.Verifier != nil
}

// stop ends the revocation list's reloader, when there is one.
func (a authSetup) stop() {
	if a.Revocations != nil {
		a.Revocations.Stop()
	}
}

// authConfigFromViper reads the auth.* keys for the scheme auth.method selects. The scheme is
// resolved by the caller (resolveAuthMethod), with the deprecated auth.enabled taken into account.
func authConfigFromViper(method string) (authConfig, error) {
	log.Trace("func() authConfigFromViper")

	cfg := authConfig{
		Method:            method,
		RevocationFile:    viper.GetString("auth.revocationFile"),
		RequireGroupScope: viper.GetBool("auth.requireGroupScope"),
		TierOverride:      topologyTierOverride(),
		TLSEnabled:        viper.GetBool("tls.enabled"),
		BuiltinRoles:      true,
	}

	switch method {

	case "hmac":
		cfg.HMAC = hmacConfigFromViper()

	case "idp":
		viper.SetDefault("auth.jwksRefreshIntervalSeconds", 300)
		viper.SetDefault("auth.roleClaim", "roles")
		viper.SetDefault("auth.groupsClaim", "groups")
		viper.SetDefault("auth.clientIdClaim", "client_id")
		viper.SetDefault("auth.builtinRoles", true)

		refreshInterval := time.Duration(viper.GetInt("auth.jwksRefreshIntervalSeconds")) * time.Second

		cfg.JWKS = auth.JWKSConfig{
			JWKSURL:         viper.GetString("auth.jwksUrl"),
			Issuer:          viper.GetString("auth.issuer"),
			Audience:        viper.GetString("auth.audience"),
			RefreshInterval: refreshInterval,
			RoleClaim:       viper.GetString("auth.roleClaim"),
			GroupsClaim:     viper.GetString("auth.groupsClaim"),
			ClientIDClaim:   viper.GetString("auth.clientIdClaim"),
		}

		cfg.BuiltinRoles = viper.GetBool("auth.builtinRoles")

		if viper.GetBool("auth.oauth2.enabled") {
			setup := oauth2SetupFromViper(refreshInterval)

			cfg.OAuth2 = &setup
		}

	}

	if method == "hmac" || method == "idp" {
		if cfg.RevocationFile != "" {
			viper.SetDefault("auth.revocationRefreshSeconds", 30)

			cfg.RevocationRefresh = time.Duration(viper.GetInt("auth.revocationRefreshSeconds")) * time.Second
		}

		if err := viper.UnmarshalKey("auth.roleMapping", &cfg.RoleMapping); err != nil {
			return authConfig{}, fmt.Errorf("failed to read auth.roleMapping: %w", err)
		}
	}

	return cfg, nil
}

// initAuth builds authentication and authorisation. cfg.Method selects the verification scheme:
// "none" (the default, preserving the no-auth behaviour of every prior release), "hmac"
// (shared-secret HS256, tokens minted by --mint-token), or "idp" (RS256 against an identity
// provider's JWKS endpoint - named directly by auth.jwksUrl or resolved via OIDC discovery from
// auth.issuer).
//
// The caller stops the returned setup when it shuts down.
func initAuth(cfg authConfig) (authSetup, error) {
	log.Trace("func() initAuth")

	setup := authSetup{Method: cfg.Method}

	switch cfg.Method {

	case "none":

	case "hmac":
		verifier, err := auth.NewHMACVerifier(cfg.HMAC)
		if err != nil {
			return authSetup{}, fmt.Errorf("failed to initialise auth: %w", err)
		}

		setup.Verifier = verifier

	case "idp":
		verifier, err := auth.NewJWKSVerifier(cfg.JWKS)
		if err != nil {
			return authSetup{}, fmt.Errorf("failed to initialise auth: %w", err)
		}

		setup.Verifier = verifier

		warnOnUnconstrainedIdP(cfg.JWKS.Audience, cfg.JWKS.Issuer)
		warnOnBuiltinRoles(cfg.RoleMapping, cfg.BuiltinRoles)

		// The server-side OIDC relying-party login (confidential-client Authorization Code flow).
		// It is only available under idp and only when auth.oauth2.enabled is set; it is
		// registered as /auth/* handlers on the gateway and its session cookie is fed to the HTTP
		// auth middleware.
		if cfg.OAuth2 != nil {
			login, err := newOAuth2Login(*cfg.OAuth2)
			if err != nil {
				return authSetup{}, fmt.Errorf("failed to initialise oauth2 login: %w", err)
			}

			setup.OAuth2Login = login
		}

	default:
		return authSetup{}, fmt.Errorf("unknown auth.method '%s' (expected 'none', 'hmac', or 'idp')", cfg.Method)

	}

	if !setup.enabled() {
		return setup, nil
	}

	// A revocation list, when configured, wraps whichever verifier was built (hmac or idp) so a
	// leaked token or decommissioned client can be cut off without rotating the signing secret for
	// everyone. It reloads from disk on the file's mtime, so revocations take effect without a
	// restart; a named-but-broken file fails startup rather than silently revoking nothing.
	if cfg.RevocationFile != "" {
		list, err := auth.NewRevocationList(cfg.RevocationFile, cfg.RevocationRefresh)
		if err != nil {
			return authSetup{}, fmt.Errorf("failed to load revocation list: %w", err)
		}

		setup.Revocations = list
		setup.Verifier = auth.NewRevokingVerifier(setup.Verifier, list)

		log.Infof("token revocation enabled from '%s'", cfg.RevocationFile)
	}

	// Group scoping is inert unless tokens carry a scope, so nothing needs enabling for it to work.
	// What this key adds is the refusal of a token that arrives WITHOUT one: an unscoped token sees
	// the whole store, so on a partitioned deployment the absence of a scope is the most dangerous
	// shape a valid token can have, not the least. Wrapping outside the revocation decorator is
	// deliberate but immaterial - both only inspect claims the inner verifier already produced.
	if cfg.RequireGroupScope {
		setup.Verifier = auth.NewGroupScopedVerifier(setup.Verifier, true)

		log.Info("auth.requireGroupScope is set - tokens carrying no group scope will be rejected")
	}

	if !cfg.TLSEnabled {
		log.Warn("authentication is enabled but tls.enabled is false - bearer tokens will be sent in " +
			"plaintext unless TLS is terminated upstream (e.g. by a proxy or service mesh)")
	}

	// Authorisation rides on top of authentication: it maps each RPC to a required role tier
	// (reader/writer/admin) and rejects an authenticated caller whose token does not grant it. Built
	// only when auth is enabled - with auth off there is no principal to authorise, so the service
	// behaves as it always has. auth.roleMapping translates an identity provider's own group names
	// onto the tiers (empty for hmac, where --mint-token stamps the tier names directly), and
	// auth.builtinRoles false leaves it the only way a role grants a tier.
	authoriser, err := auth.NewAuthoriser(cfg.RoleMapping, cfg.TierOverride, cfg.BuiltinRoles)
	if err != nil {
		setup.stop()

		return authSetup{}, fmt.Errorf("failed to initialise authorisation: %w", err)
	}

	setup.Authoriser = authoriser

	return setup, nil
}
