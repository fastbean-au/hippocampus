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
