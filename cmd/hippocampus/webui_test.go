package main

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

// consoleMux builds the console's routes exactly as main.go does, so the tests below exercise the
// real registration rather than a restatement of it.
func consoleMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("/ui", webUIHandler())
	mux.Handle("/ui/app.js", webUIAssetHandler("app.js", "text/javascript; charset=utf-8"))
	mux.Handle("/ui/lib.js", webUIAssetHandler("lib.js", "text/javascript; charset=utf-8"))
	mux.Handle("/ui/styles.css", webUIAssetHandler("styles.css", "text/css; charset=utf-8"))

	return mux
}

// TestEveryEmbeddedAssetIsServedAndOpen is the drift guard for the console's split into three
// files. Every file under webui/ must be routed and must appear in BOTH middleware allow-lists,
// because the two fail in opposite and equally confusing ways: an asset missing from the auth list
// makes an authenticated console 401 its own script, which presents as a blank page rather than as
// an auth failure, and one missing from the purge list makes the console unreadable exactly while
// an operator is trying to watch a purge.
//
// It walks the embedded filesystem rather than a hand-written list so that adding a fourth asset
// cannot be half-done: the file exists, therefore the test demands a route and two entries for it.
func TestEveryEmbeddedAssetIsServedAndOpen(t *testing.T) {
	authOpen, purgeOpen := middlewareOpenPaths(false)
	mux := consoleMux()

	var found int

	err := fs.WalkDir(webUIAssets, "webui", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() {
			return nil
		}

		found++

		// index.html is served at /ui, not /ui/index.html: it is the console's entry document and
		// the path predates the split.
		name := strings.TrimPrefix(path, "webui/")
		want := "/ui/" + name

		if name == "index.html" {
			want = "/ui"
		}

		if !slices.Contains(webUIAssetPaths, want) {
			t.Errorf("%s is embedded but %q is not in webUIAssetPaths", path, want)

			return nil
		}

		if !slices.Contains(authOpen, want) {
			t.Errorf("%s is embedded but %q is not open to the auth middleware", path, want)
		}

		if !slices.Contains(purgeOpen, want) {
			t.Errorf("%s is embedded but %q is not open to the purge middleware", path, want)
		}

		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, want, nil))

		if rec.Code != http.StatusOK {
			t.Errorf("%s: expected 200 at %q, got %d", path, want, rec.Code)
		}

		if rec.Body.Len() == 0 {
			t.Errorf("%s: served an empty body at %q", path, want)
		}

		return nil
	})
	if err != nil {
		t.Fatalf("walking the embedded console: %s", err.Error())
	}

	if found != len(webUIAssetPaths) {
		t.Errorf("embedded %d console files but webUIAssetPaths lists %d", found, len(webUIAssetPaths))
	}
}

// TestWebUIAssetRevalidates pins the caching contract the split introduced. The two assets are
// embedded, so their bytes cannot change for this process's lifetime and the ETag computed once at
// construction cannot go stale - but no-cache must still be sent, or a browser would be entitled to
// reuse them across an upgrade without asking.
func TestWebUIAssetRevalidates(t *testing.T) {
	handler := webUIAssetHandler("app.js", "text/javascript; charset=utf-8")

	first := httptest.NewRecorder()
	handler.ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/ui/app.js", nil))

	if first.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", first.Code)
	}

	if ct := first.Header().Get("Content-Type"); ct != "text/javascript; charset=utf-8" {
		t.Errorf("unexpected Content-Type %q", ct)
	}

	if cc := first.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("expected Cache-Control no-cache, got %q", cc)
	}

	etag := first.Header().Get("ETag")

	if etag == "" {
		t.Fatal("expected an ETag on a console asset")
	}

	req := httptest.NewRequest(http.MethodGet, "/ui/app.js", nil)
	req.Header.Set("If-None-Match", etag)

	second := httptest.NewRecorder()
	handler.ServeHTTP(second, req)

	if second.Code != http.StatusNotModified {
		t.Errorf("expected 304 for a matching ETag, got %d", second.Code)
	}

	if second.Body.Len() != 0 {
		t.Errorf("expected an empty 304 body, got %d bytes", second.Body.Len())
	}
}

// TestConsoleReferencesItsAssets pins the other half of the split: the entry document must actually
// ask for the two files, by absolute path. A relative "app.js" would resolve to /app.js, because the
// console is registered at the exact path /ui with no trailing slash - so the page would load, look
// intact in a directory listing, and be inert in a browser.
func TestConsoleReferencesItsAssets(t *testing.T) {
	rec := httptest.NewRecorder()
	webUIHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ui", nil))

	body := rec.Body.String()

	for _, want := range []string{`href="/ui/styles.css"`, `src="/ui/app.js"`} {
		if !strings.Contains(body, want) {
			t.Errorf("expected the console to reference %s", want)
		}
	}

	// The whole point of the split is that neither block is inline any more; a stray one would mean
	// the page carries two copies, and would defeat the CSP the split exists to make possible.
	for _, unwanted := range []string{"<style>", "<script>"} {
		if strings.Contains(body, unwanted) {
			t.Errorf("expected no inline %s block in the console", unwanted)
		}
	}
}

// csp returns the console's Content-Security-Policy under a given UI configuration.
func csp(t *testing.T, cfg UIConfig) string {
	t.Helper()

	rec := httptest.NewRecorder()
	handler := webUISecurityHeaders(cfg, webUIHandler())

	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ui", nil))

	return rec.Header().Get("Content-Security-Policy")
}

// TestConsoleCSPDeniesInlineCode pins the policy the split into three files exists to make possible.
// Without it the console would still work, so nothing else would notice it had been relaxed - and a
// relaxation is exactly what an XSS needs, this console having already had one (item 24.1).
func TestConsoleCSPDeniesInlineCode(t *testing.T) {
	policy := csp(t, UIConfig{AuthMethod: "none"})

	for _, want := range []string{
		"default-src 'none'",
		"script-src 'self'",
		"style-src 'self'",
		"img-src 'self' data:",
		"form-action 'none'",
		"base-uri 'none'",
		"frame-ancestors 'none'",
	} {
		if !strings.Contains(policy, want) {
			t.Errorf("policy is missing %q: %s", want, policy)
		}
	}

	// The whole point: an onclick= or style= attribute is blocked without these, and blocked
	// silently. If either ever appears here, the drift guard in webuitest asserting the markup
	// carries no inline code has been defeated rather than satisfied.
	for _, unwanted := range []string{"unsafe-inline", "unsafe-eval"} {
		if strings.Contains(policy, unwanted) {
			t.Errorf("policy permits %s, which defeats it: %s", unwanted, policy)
		}
	}
}

// The console IS the client of /v1, unlike the config-wizard whose otherwise-identical policy denies
// every connection. A connect-src that forgot 'self' would leave a console that renders perfectly
// and can call nothing.
func TestConsoleCSPPermitsItsOwnAPI(t *testing.T) {
	if policy := csp(t, UIConfig{AuthMethod: "none"}); !strings.Contains(policy, "connect-src 'self'") {
		t.Errorf("policy does not permit same-origin requests: %s", policy)
	}
}

// Under the in-browser PKCE flow the page fetches the issuer's discovery document and posts to its
// token endpoint, both cross-origin. Omitting the issuer would block sign-in on exactly the
// deployments that authenticate.
func TestConsoleCSPPermitsTheIssuerUnderBrowserLogin(t *testing.T) {
	policy := csp(t, UIConfig{
		AuthMethod: "idp",
		LoginMode:  "browser",
		Issuer:     "https://example.eu.auth0.com/",
	})

	if !strings.Contains(policy, "connect-src 'self' https://example.eu.auth0.com") {
		t.Errorf("policy does not permit the issuer: %s", policy)
	}

	// The origin only - a path from the issuer URL would not match a connect-src source anyway, and
	// carrying one would suggest it narrowed the permission when it does not.
	if strings.Contains(policy, "auth0.com/;") || strings.Contains(policy, "auth0.com/ ") {
		t.Errorf("policy carries the issuer's path rather than its origin: %s", policy)
	}
}

// The server-hosted flow performs the exchange service-side; the browser only follows a same-origin
// redirect. Widening connect-src there would grant a permission nothing uses.
func TestConsoleCSPWithholdsTheIssuerUnderServerLogin(t *testing.T) {
	for _, cfg := range []UIConfig{
		{AuthMethod: "idp", LoginMode: "server", Issuer: "https://example.eu.auth0.com/"},
		{AuthMethod: "hmac", Issuer: "https://example.eu.auth0.com/"},
		{AuthMethod: "none"},
	} {
		if policy := csp(t, cfg); strings.Contains(policy, "auth0.com") {
			t.Errorf("%s/%s: policy names the issuer needlessly: %s", cfg.AuthMethod, cfg.LoginMode, policy)
		}
	}
}

// A malformed issuer must not produce a malformed policy - a broken directive can invalidate the
// whole header on some browsers, which would silently turn the protection off.
func TestConsoleCSPSurvivesAMalformedIssuer(t *testing.T) {
	for _, issuer := range []string{"", "not a url", "://missing-scheme", "ftp:"} {
		policy := csp(t, UIConfig{AuthMethod: "idp", LoginMode: "browser", Issuer: issuer})

		if !strings.Contains(policy, "connect-src 'self'") {
			t.Errorf("issuer %q broke the policy: %s", issuer, policy)
		}

		if strings.Contains(policy, "connect-src 'self' ;") || strings.Contains(policy, "  ") {
			t.Errorf("issuer %q produced a malformed policy: %s", issuer, policy)
		}
	}
}

// Every console route carries the headers, not only the entry document: the assets are served from
// the same origin and a policy applied to one response and not another is the kind of gap that
// looks fine until someone reaches an asset directly.
func TestConsoleSecurityHeadersOnEveryRoute(t *testing.T) {
	cfg := UIConfig{AuthMethod: "none"}
	mux := http.NewServeMux()

	for path, handler := range map[string]http.Handler{
		"/ui":            webUIHandler(),
		"/ui/app.js":     webUIAssetHandler("app.js", "text/javascript; charset=utf-8"),
		"/ui/lib.js":     webUIAssetHandler("lib.js", "text/javascript; charset=utf-8"),
		"/ui/styles.css": webUIAssetHandler("styles.css", "text/css; charset=utf-8"),
	} {
		mux.Handle(path, webUISecurityHeaders(cfg, handler))
	}

	for _, path := range webUIAssetPaths {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))

		for _, header := range []string{"Content-Security-Policy", "X-Content-Type-Options", "Referrer-Policy"} {
			if rec.Header().Get(header) == "" {
				t.Errorf("%s: missing %s", path, header)
			}
		}
	}
}
