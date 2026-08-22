package main

import (
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// UIConfig is the front-channel configuration the embedded console needs to start an OIDC login. It
// is served, unauthenticated, at /ui/config so the browser can learn - before it holds any token -
// whether auth is on and, under idp, which provider parameters to begin the authorization-code flow
// with. It carries no secret: only the public SPA client id, the scopes to request, the issuer the
// browser runs OIDC discovery against, and the optional audience some providers (Auth0) require to
// mint a JWT access token rather than an opaque one.
type UIConfig struct {
	AuthMethod string `json:"authMethod"`
	Issuer     string `json:"issuer,omitempty"`
	ClientID   string `json:"clientId,omitempty"`
	Scopes     string `json:"scopes,omitempty"`
	Audience   string `json:"audience,omitempty"`

	// LoginMode tells the console how sign-in works under idp: "browser" (the default) runs the
	// in-page Authorization Code + PKCE flow using the fields above, while "server" means the service
	// hosts the flow at /auth/login and the console only links to it - the browser holds no token, the
	// session rides an HttpOnly cookie. Empty in non-idp modes, where the manual token box is used.
	LoginMode string `json:"loginMode,omitempty"`
}

// uiConfigHandler serves the front-channel OIDC configuration as JSON at /ui/config. Like the
// console page itself it is unauthenticated - the browser reads it before it has a token - and it
// never caches, so a config change takes effect on the next load. The payload is marshalled once at
// construction since it is fixed for the process's lifetime.
func uiConfigHandler(cfg UIConfig) http.Handler {
	body, _ := json.Marshal(cfg)

	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")

		_, _ = w.Write(body)
	})
}

// webUIAssets holds the embedded console: index.html plus the stylesheet and script it references.
// The three were one self-contained file until the script outgrew being reviewable inside the
// markup; they are still built by nothing (no bundler, no build step) and fetched from nowhere but
// this binary.
//
// The directory form is deliberate. Naming each file individually means a new asset is embedded
// only if someone remembers to add it, whereas a directory embed makes forgetting impossible - and
// TestEveryEmbeddedAssetIsServedAndOpen then requires every file in it to be both routed and listed
// among the open paths, which is the part that actually fails closed if missed.
//
//go:embed webui
var webUIAssets embed.FS

// webUIAssetPaths are the console's request paths. main.go registers exactly these and adds them to
// both middleware allow-lists; the drift guard reads this list rather than restating it, so an asset
// added to webui/ without being routed and opened fails the test instead of 401ing in a browser.
//
// They are exact paths rather than a /ui/ prefix served by an http.FileServer for three reasons: the
// auth allow-list is exact-match and closed by default (a prefix would silently open whatever landed
// in the directory), a prefix under /ui/ would shadow /ui/config, and three files are not a tree.
var webUIAssetPaths = []string{"/ui", "/ui/app.js", "/ui/lib.js", "/ui/styles.css"}

// webUISecurityHeaders wraps a console route with the response headers a deployment reachable from
// a browser wants. The Content-Security-Policy is the point of it.
//
// This became possible only once the console carried no inline script or style: `script-src 'self'`
// without `unsafe-inline` blocks an onclick= attribute and a style= attribute outright, and blocks
// them SILENTLY - a CSP violation is a console warning, not an exception - so a page with either
// would half-work in a way no test would notice. That is why the drift guard in webuitest asserts
// neither exists, and why this and that assertion have to move together.
//
// It deliberately differs from the config-wizard's otherwise-identical policy
// (cmd/config-wizard/main.go, securityHeaders) in one respect: connect-src. The wizard talks to
// nothing and denies every connection; this page IS the client of /v1, so it must permit 'self' -
// and, under an in-browser OIDC login, the identity provider it runs discovery and the token
// exchange against.
//
// Keep it in step with the markup. Adding an external font, a CDN script, or a fetch to a new origin
// needs a matching relaxation here or the browser will quietly refuse it.
func webUISecurityHeaders(cfg UIConfig, next http.Handler) http.Handler {
	// The browser-side PKCE flow fetches the issuer's discovery document and posts to its token
	// endpoint, both cross-origin. The server-hosted flow (loginMode "server") does neither - the
	// service performs the exchange and the browser only follows a same-origin redirect - so the
	// issuer is added for the browser flow alone rather than whenever an IdP is configured.
	//
	// The origin is derived from the issuer. A provider serving its token endpoint from a different
	// host than its issuer would need that host added too; none of the providers this has been run
	// against does.
	connect := "'self'"

	if cfg.AuthMethod == "idp" && cfg.LoginMode == "browser" && cfg.Issuer != "" {
		if u, err := url.Parse(cfg.Issuer); err == nil && u.Scheme != "" && u.Host != "" {
			connect += " " + u.Scheme + "://" + u.Host
		}
	}

	policy := strings.Join([]string{
		"default-src 'none'",
		"script-src 'self'",
		"style-src 'self'",
		// The favicon is an inline SVG data URI, which img-src governs.
		"img-src 'self' data:",
		"connect-src " + connect,
		// The sign-in form's submit is preventDefault-ed and the server-login path is a navigation,
		// which form-action does not cover - so nothing on this page ever posts a form.
		"form-action 'none'",
		"base-uri 'none'",
		"frame-ancestors 'none'",
	}, "; ")

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", policy)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")

		next.ServeHTTP(w, r)
	})
}

// middlewareOpenPaths builds the exact-match allow-lists for the two HTTP middlewares: the paths
// auth.HTTPMiddleware serves without a token, and the paths
// Server.HTTPMiddlewareBlockWhenPurgeInProgress serves while a purge runs. serverLogin adds the
// server-hosted OIDC endpoints, which run before (or in place of) holding a token.
//
// The OpenAPI document is on BOTH lists. It was on the purge list alone until the schema-secrecy
// argument was examined and found not to hold - it is generated from a proto checked into a public
// repository - and requiring a token for it broke every OpenAPI tool, none of which can authenticate
// the initial spec fetch. openAPIHandler carries the full reasoning.
//
// It is a function rather than two literals inside main() so a test can assert what it returns.
// The auth list is closed by default, which is the design - a new endpoint is protected without
// anyone remembering to do anything - but it means the console's own assets must be listed or the
// page 401s its own stylesheet, and that presents as a broken console rather than as an auth error.
// TestEveryEmbeddedAssetIsServedAndOpen is what makes that impossible to get wrong.
func middlewareOpenPaths(serverLogin bool) (authPaths []string, purgePaths []string) {
	authPaths = append([]string{"/healthz", "/readyz", openAPIPath, "/ui/config"}, webUIAssetPaths...)
	purgePaths = append(
		[]string{"/healthz", "/readyz", openAPIPath, "/ui/config"},
		webUIAssetPaths...,
	)

	if serverLogin {
		login := []string{"/auth/login", "/auth/callback", "/auth/refresh", "/auth/logout"}
		authPaths = append(authPaths, login...)
		purgePaths = append(purgePaths, login...)
	}

	return authPaths, purgePaths
}

// webUIHandler serves the console's entry document at the exact path /ui. It is listed among the
// open paths of both the purge-in-progress and auth middleware, so the static page always loads; the
// API calls it makes still travel through /v1 and are subject to auth/purge like any other request
// (the page carries the bearer token itself).
func webUIHandler() http.Handler {
	body, err := webUIAssets.ReadFile("webui/index.html")
	if err != nil {
		panic(fmt.Sprintf("embedded console missing webui/index.html: %s", err.Error()))
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")

		// The entry document is embedded in the binary and changes with every build/deploy, so tell
		// the browser never to reuse a cached copy — otherwise a stale console lingers after an
		// upgrade. Its two assets are revalidated by ETag instead; see webUIAssetHandler.
		w.Header().Set("Cache-Control", "no-store")

		_, _ = w.Write(body)
	})
}

// webUIAssetHandler serves one of the console's static assets. Unlike the entry document these are
// revalidated rather than never cached: the bytes are embedded and so cannot change for this
// process's lifetime, which is what makes an ETag computed once at construction safe - it can go
// stale only by the process being replaced, and a replaced process serves a different one. A
// navigation therefore costs two 304s rather than re-transferring ~110 KiB, while an upgrade is
// still picked up immediately because no-cache forces the revalidation.
func webUIAssetHandler(name string, contentType string) http.Handler {
	body, err := webUIAssets.ReadFile("webui/" + name)
	if err != nil {
		panic(fmt.Sprintf("embedded console missing webui/%s: %s", name, err.Error()))
	}

	sum := sha256.Sum256(body)
	etag := `"` + base64.RawURLEncoding.EncodeToString(sum[:16]) + `"`

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("ETag", etag)

		if match := r.Header.Get("If-None-Match"); match == etag {
			w.WriteHeader(http.StatusNotModified)

			return
		}

		_, _ = w.Write(body)
	})
}
