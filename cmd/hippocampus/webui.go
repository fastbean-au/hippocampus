package main

import (
	_ "embed"
	"net/http"
)

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
