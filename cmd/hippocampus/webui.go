package main

import (
	_ "embed"
	"encoding/json"
	"net/http"
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

// webUIHTML is the single-page console served at /ui. It is a self-contained HTML/CSS/JS document
// (no build step, no external assets) that drives the gateway's /v1 JSON endpoints: OpenSearch
// content search plus event/memory create, update, and delete.
//
//go:embed webui/index.html
var webUIHTML []byte

// webUIHandler serves the embedded console. It is registered at the exact path /ui and is listed
// among the open paths of both the purge-in-progress and auth middleware, so the static page always
// loads; the API calls it makes still travel through /v1 and are subject to auth/purge like any
// other request (the page carries the bearer token itself).
func webUIHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")

		// The page is embedded in the binary and changes with every build/deploy, so tell the
		// browser never to reuse a cached copy — otherwise a stale console lingers after an upgrade.
		w.Header().Set("Cache-Control", "no-store")

		_, _ = w.Write(webUIHTML)
	})
}
