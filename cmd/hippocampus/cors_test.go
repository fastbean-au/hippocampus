package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// gateway.corsOrigins arrives as a JSON array from a configuration file and as a single string from
// an environment override. Both have to work, and the string form has to split on the comma an
// operator reaches for first - viper would otherwise hand the whole thing back as one origin that
// silently matches nothing.
func TestParseCORSOrigins(t *testing.T) {
	cases := []struct {
		name  string
		value any
		want  []string
	}{
		{name: "nil is empty", value: nil, want: nil},
		{name: "json array", value: []any{"http://a:1", "https://b"}, want: []string{"http://a:1", "https://b"}},
		{name: "string slice", value: []string{"http://a:1"}, want: []string{"http://a:1"}},
		{name: "comma separated", value: "http://a:1,https://b", want: []string{"http://a:1", "https://b"}},
		{name: "comma and space separated", value: "http://a:1, https://b", want: []string{"http://a:1", "https://b"}},
		{name: "space separated", value: "http://a:1 https://b", want: []string{"http://a:1", "https://b"}},
		{name: "surrounding whitespace trimmed", value: "  http://a:1  ", want: []string{"http://a:1"}},
		{name: "empty string", value: "", want: []string{}},
		{name: "non-string members ignored", value: []any{"http://a:1", 7}, want: []string{"http://a:1"}},
		{name: "unexpected type", value: 7, want: nil},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := parseCORSOrigins(testCase.value)

			if len(got) != len(testCase.want) {
				t.Fatalf("parseCORSOrigins(%v) = %q, want %q", testCase.value, got, testCase.want)
			}

			for i, v := range testCase.want {
				if got[i] != v {
					t.Errorf("parseCORSOrigins(%v)[%d] = %q, want %q", testCase.value, i, got[i], v)
				}
			}
		})
	}
}

// Every rejection here is a value that would otherwise present as "CORS does not work" rather than
// as a configuration error - a trailing slash never matches the Origin header a browser sends, and
// a wildcard is a decision rather than a shortcut.
func TestValidateCORSOrigins(t *testing.T) {
	cases := []struct {
		name    string
		origins []string
		wantErr bool
	}{
		{name: "empty is fine", origins: nil},
		{name: "http with port", origins: []string{"http://localhost:8082"}},
		{name: "https without port", origins: []string{"https://console.example.com"}},
		{name: "several", origins: []string{"http://a:1", "https://b"}},
		{name: "wildcard refused", origins: []string{"*"}, wantErr: true},
		{name: "trailing slash refused", origins: []string{"http://localhost:8082/"}, wantErr: true},
		{name: "path refused", origins: []string{"http://localhost:8082/ui"}, wantErr: true},
		{name: "query refused", origins: []string{"http://localhost:8082?a=1"}, wantErr: true},
		{name: "fragment refused", origins: []string{"http://localhost:8082#x"}, wantErr: true},
		{name: "credentials refused", origins: []string{"http://user:pass@localhost:8082"}, wantErr: true},
		{name: "bad scheme refused", origins: []string{"ftp://localhost:8082"}, wantErr: true},
		{name: "scheme-less refused", origins: []string{"localhost:8082"}, wantErr: true},
		{name: "hostless refused", origins: []string{"http://"}, wantErr: true},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			err := validateCORSOrigins(testCase.origins)

			if testCase.wantErr && err == nil {
				t.Errorf("validateCORSOrigins(%q) returned no error, expected one", testCase.origins)
			}

			if !testCase.wantErr && err != nil {
				t.Errorf("validateCORSOrigins(%q) returned %v, expected none", testCase.origins, err)
			}
		})
	}
}

// corsTestHandler is the inner handler the middleware wraps: it records that it ran, and can answer
// with a status other than 200 so the "headers survive a rejection" case can be exercised.
func corsTestHandler(served *bool, status int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*served = true

		w.WriteHeader(status)
	})
}

func TestCORSMiddleware(t *testing.T) {
	const allowedOrigin = "http://localhost:8082"

	t.Run("allowed origin is echoed and the request is served", func(t *testing.T) {
		served := false
		handler := corsMiddleware([]string{allowedOrigin}, corsTestHandler(&served, http.StatusOK))

		request := httptest.NewRequest(http.MethodGet, "/v1/whoami", nil)
		request.Header.Set("Origin", allowedOrigin)

		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)

		if !served {
			t.Error("the inner handler did not run")
		}

		if got := recorder.Header().Get("Access-Control-Allow-Origin"); got != allowedOrigin {
			t.Errorf("Access-Control-Allow-Origin = %q, want %q", got, allowedOrigin)
		}

		if got := recorder.Header().Get("Vary"); !strings.Contains(got, "Origin") {
			t.Errorf("Vary = %q, want it to contain Origin", got)
		}
	})

	t.Run("unlisted origin gets no allow header but is still served", func(t *testing.T) {
		served := false
		handler := corsMiddleware([]string{allowedOrigin}, corsTestHandler(&served, http.StatusOK))

		request := httptest.NewRequest(http.MethodGet, "/v1/whoami", nil)
		request.Header.Set("Origin", "http://evil.example.com")

		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)

		// The request is served: refusing it here would be a server-side access decision, which is
		// not what CORS is. The browser is the one that refuses to hand the response to the page.
		if !served {
			t.Error("the inner handler did not run")
		}

		if got := recorder.Header().Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("Access-Control-Allow-Origin = %q, want it unset for an unlisted origin", got)
		}
	})

	t.Run("preflight is answered and never forwarded", func(t *testing.T) {
		served := false
		handler := corsMiddleware([]string{allowedOrigin}, corsTestHandler(&served, http.StatusOK))

		request := httptest.NewRequest(http.MethodOptions, "/v1/memories", nil)
		request.Header.Set("Origin", allowedOrigin)
		request.Header.Set("Access-Control-Request-Method", "POST")

		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)

		if served {
			t.Error("a preflight reached the inner handler; it must be answered by the middleware")
		}

		if recorder.Code != http.StatusNoContent {
			t.Errorf("preflight status = %d, want %d", recorder.Code, http.StatusNoContent)
		}

		if got := recorder.Header().Get("Access-Control-Allow-Methods"); got == "" {
			t.Error("preflight carried no Access-Control-Allow-Methods")
		}

		if got := recorder.Header().Get("Access-Control-Allow-Headers"); !strings.Contains(got, "Authorization") {
			t.Errorf("Access-Control-Allow-Headers = %q, want it to permit Authorization", got)
		}
	})

	t.Run("preflight from an unlisted origin carries no allow headers", func(t *testing.T) {
		served := false
		handler := corsMiddleware([]string{allowedOrigin}, corsTestHandler(&served, http.StatusOK))

		request := httptest.NewRequest(http.MethodOptions, "/v1/memories", nil)
		request.Header.Set("Origin", "http://evil.example.com")
		request.Header.Set("Access-Control-Request-Method", "POST")

		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)

		if got := recorder.Header().Get("Access-Control-Allow-Methods"); got != "" {
			t.Errorf("Access-Control-Allow-Methods = %q, want it unset for an unlisted origin", got)
		}
	})

	t.Run("allow header survives a rejection from further in", func(t *testing.T) {
		served := false
		handler := corsMiddleware([]string{allowedOrigin}, corsTestHandler(&served, http.StatusUnauthorized))

		request := httptest.NewRequest(http.MethodGet, "/v1/whoami", nil)
		request.Header.Set("Origin", allowedOrigin)

		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)

		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want %d", recorder.Code, http.StatusUnauthorized)
		}

		// Without the header on this response the browser reports an opaque CORS failure and the
		// caller never learns it was a 401. That is the whole reason the header is set before the
		// inner handler runs rather than after.
		if got := recorder.Header().Get("Access-Control-Allow-Origin"); got != allowedOrigin {
			t.Errorf("Access-Control-Allow-Origin = %q on a 401, want %q", got, allowedOrigin)
		}
	})

	t.Run("credentials are never allowed", func(t *testing.T) {
		served := false
		handler := corsMiddleware([]string{allowedOrigin}, corsTestHandler(&served, http.StatusOK))

		for _, method := range []string{http.MethodGet, http.MethodOptions} {
			request := httptest.NewRequest(method, "/v1/whoami", nil)
			request.Header.Set("Origin", allowedOrigin)
			request.Header.Set("Access-Control-Request-Method", "GET")

			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)

			// This is what keeps the console's HttpOnly session cookie unusable cross-origin.
			if got := recorder.Header().Get("Access-Control-Allow-Credentials"); got != "" {
				t.Errorf("%s: Access-Control-Allow-Credentials = %q, want it never set", method, got)
			}
		}
	})

	t.Run("non-v1 paths are untouched", func(t *testing.T) {
		for _, path := range []string{"/healthz", "/readyz", "/ui", "/ui/config", "/auth/login"} {
			served := false
			handler := corsMiddleware([]string{allowedOrigin}, corsTestHandler(&served, http.StatusOK))

			request := httptest.NewRequest(http.MethodGet, path, nil)
			request.Header.Set("Origin", allowedOrigin)

			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)

			if !served {
				t.Errorf("%s: the inner handler did not run", path)
			}

			if got := recorder.Header().Get("Access-Control-Allow-Origin"); got != "" {
				t.Errorf("%s: Access-Control-Allow-Origin = %q, want the console and probes left alone", path, got)
			}
		}
	})

	// The OpenAPI document must be inside the CORS scope even though it is outside the RPC metrics
	// scope: it is the first thing a browser API explorer fetches, and Swagger UI resolves its
	// "Try it out" calls against whichever origin served it.
	t.Run("the openapi document is in scope", func(t *testing.T) {
		if !isCORSPath(openAPIPath) {
			t.Errorf("isCORSPath(%q) = false, want the document reachable cross-origin", openAPIPath)
		}
	})
}
