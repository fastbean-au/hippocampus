package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/spf13/viper"

	"github.com/fastbean-au/hippocampus/contract"
)

// An unset key must mean ENABLED. The zero value of a bool is the opposite of what every release
// before this key existed did, so a path reaching run() without seeded defaults would otherwise
// silently stop serving an endpoint the documentation tells clients to generate from.
func TestOpenAPIEnabled(t *testing.T) {
	cases := []struct {
		name  string
		set   bool
		value bool
		want  bool
	}{
		{name: "unset means served", want: true},
		{name: "explicitly on", set: true, value: true, want: true},
		{name: "explicitly off", set: true, value: false, want: false},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			viper.Reset()
			t.Cleanup(viper.Reset)

			if testCase.set {
				viper.Set("gateway.openapi.enabled", testCase.value)
			}

			if got := openAPIEnabled(); got != testCase.want {
				t.Errorf("openAPIEnabled() = %v, want %v", got, testCase.want)
			}
		})
	}
}

// setStartupDefaults must declare the same default the function falls back to. They are two
// mechanisms for one decision, and the configuration wizard's cross-check reads the SetDefault one.
func TestOpenAPIDefaultMatchesTheFallback(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)

	setStartupDefaults()

	if !viper.GetBool("gateway.openapi.enabled") {
		t.Error("setStartupDefaults leaves gateway.openapi.enabled false, which contradicts openAPIEnabled's unset behaviour")
	}
}

func TestOpenAPIHandler(t *testing.T) {
	handler := openAPIHandler()

	t.Run("serves the embedded document", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, openAPIPath, nil))

		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
		}

		if got := recorder.Header().Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", got)
		}

		if !bytes.Equal(recorder.Body.Bytes(), contract.SwaggerJSON) {
			t.Error("the body is not the embedded document")
		}

		if recorder.Header().Get("ETag") == "" {
			t.Error("no ETag, so a repeat fetch cannot be answered with a 304")
		}

		if got := recorder.Header().Get("Cache-Control"); got != "no-cache" {
			t.Errorf("Cache-Control = %q, want no-cache - the document changes with the binary", got)
		}
	})

	t.Run("a matching If-None-Match is answered 304", func(t *testing.T) {
		first := httptest.NewRecorder()
		handler.ServeHTTP(first, httptest.NewRequest(http.MethodGet, openAPIPath, nil))

		etag := first.Header().Get("ETag")

		for _, header := range []string{etag, "*", "W/" + etag, `"other", ` + etag} {
			request := httptest.NewRequest(http.MethodGet, openAPIPath, nil)
			request.Header.Set("If-None-Match", header)

			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)

			if recorder.Code != http.StatusNotModified {
				t.Errorf("If-None-Match %q: status = %d, want %d", header, recorder.Code, http.StatusNotModified)
			}

			if recorder.Body.Len() != 0 {
				t.Errorf("If-None-Match %q: a 304 carried a %d-byte body", header, recorder.Body.Len())
			}
		}
	})

	t.Run("a stale If-None-Match is answered in full", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodGet, openAPIPath, nil)
		request.Header.Set("If-None-Match", `"a-previous-build"`)

		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)

		if recorder.Code != http.StatusOK {
			t.Errorf("status = %d, want %d - a client holding an old build's document must be refreshed", recorder.Code, http.StatusOK)
		}
	})
}

// The document is on BOTH middleware open-lists. The purge one is long-standing; the auth one is the
// reversal - requiring a token protected a file published with the source, while breaking every
// OpenAPI tool, none of which can authenticate the initial spec fetch.
func TestOpenAPIDocumentIsOpenToUnauthenticatedCallers(t *testing.T) {
	authPaths, purgePaths := middlewareOpenPaths(false)

	if !slices.Contains(authPaths, openAPIPath) {
		t.Errorf("%q is not on the auth open-list, so a browser API explorer cannot load the spec", openAPIPath)
	}

	if !slices.Contains(purgePaths, openAPIPath) {
		t.Errorf("%q is not on the purge open-list", openAPIPath)
	}
}

// The document declares how to authenticate, which is what gives a browser API explorer an Authorize
// box. Without it the explorer can read the schema and can never call anything on a secured
// instance, which presents as the tool being broken rather than as a missing annotation.
func TestSwaggerDocumentDeclaresBearerAuth(t *testing.T) {
	if !bytes.Contains(contract.SwaggerJSON, []byte(`"securityDefinitions"`)) {
		t.Fatal("the generated OpenAPI document declares no securityDefinitions - regenerate with go generate ./contract")
	}

	if !bytes.Contains(contract.SwaggerJSON, []byte(`"Authorization"`)) {
		t.Error("the securityDefinition does not name the Authorization header")
	}
}
