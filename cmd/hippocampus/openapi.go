package main

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"

	"github.com/fastbean-au/hippocampus/contract"
)

// openAPIPath is the gateway route the OpenAPI document is served at. It is a constant because four
// separate things have to agree on it - the route, the auth open-list, the purge open-list, and the
// two path predicates that decide whether it is metered and whether it is throttled - and a literal
// repeated five times is a literal that will eventually differ in one of them.
const openAPIPath = "/v1/openapi.json"

// openAPIHandler serves the embedded OpenAPI description of the /v1 surface.
//
// It is served WITHOUT authentication, which is a deliberate reversal. The document is generated
// from contract/hippocampus.proto and checked into a public repository, so requiring a token to
// read it protected a file anybody can fetch from GitHub - while breaking every standard OpenAPI
// tool, none of which can attach a token to the initial spec fetch. Confidentiality was never the
// property being defended here; gateway.openapi.enabled is for the deployment that wants the
// endpoint gone entirely, which is a different and honest request.
//
// The ETag is computed once, over the embedded bytes, because they cannot change while the process
// is running - the document is only ever a different one in a different build. A repeat fetch from
// a well-behaved client therefore costs a 304 rather than 148 KB. That is a courtesy to real
// clients and NOT a defence: an attacker simply omits If-None-Match. What bounds the amplification
// is isRateLimitedPath putting this route under the arrival limiter.
func openAPIHandler() http.Handler {
	sum := sha256.Sum256(contract.SwaggerJSON)
	etag := `"` + hex.EncodeToString(sum[:]) + `"`

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", etag)

		// no-cache rather than a max-age: the document changes with the binary, and a deployment
		// that rolls forward while a browser holds an unexpired copy would show the previous
		// version's schema with nothing to indicate it. Revalidation costs one conditional request
		// and answers 304.
		w.Header().Set("Cache-Control", "no-cache")

		if matchesETag(r.Header.Get("If-None-Match"), etag) {
			w.WriteHeader(http.StatusNotModified)

			return
		}

		_, _ = w.Write(contract.SwaggerJSON)
	})
}

// matchesETag reports whether an If-None-Match header names the document's current tag. The header
// is a comma-separated list and may be "*"; a weak validator ("W/" prefixed) is treated as matching
// because for this resource the weak and strong comparisons cannot differ - the bytes are fixed for
// the life of the process.
func matchesETag(header string, etag string) bool {
	if header == "" {
		return false
	}

	if strings.TrimSpace(header) == "*" {
		return true
	}

	for _, v := range strings.Split(header, ",") {
		candidate := strings.TrimSpace(v)
		candidate = strings.TrimPrefix(candidate, "W/")

		if candidate == etag {
			return true
		}
	}

	return false
}
