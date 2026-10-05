package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The security pages are the ones an operator or an assessor scopes compensating controls from, and
// their "what the service does not do" list is read more literally than anything else in the docs:
// a claimed absence sends a reader to build the missing thing themselves. Two of those claims went
// false when the feature shipped - server-side mutual TLS (item 101) and gRPC reflection - and the
// pages kept denying them, in the same file that documented both elsewhere (TODO-3 item 146).
//
// No guard can read prose for meaning, so this one is deliberately narrow: each entry is a sentence
// that once denied a feature, paired with the configuration key that proves the feature exists. It
// fails only while both hold - the denial is on the page AND the service still reads the key - so a
// feature genuinely removed later makes the sentence true again and the guard steps aside.
var deniedFeatures = []struct {
	file   string
	phrase string
	key    string
}{
	{"docs/security.md", "There is no server-side mutual TLS", "tls.clientCaFile"},
	{"docs/security.md", "It registers no gRPC reflection service", "reflection.enabled"},
	{"SECURITY.md", "verify client certificates on its listeners", "tls.clientCaFile"},
}

var whitespace = regexp.MustCompile(`\s+`)

func TestSecurityDocsDoNotDenyShippedFeatures(t *testing.T) {
	root := filepath.Join("..", "..")

	source, err := os.ReadFile(filepath.Join(root, "cmd", "hippocampus", "main.go"))
	if err != nil {
		t.Fatalf("reading main.go: %s", err)
	}

	for _, v := range deniedFeatures {
		page, err := os.ReadFile(filepath.Join(root, v.file))
		if err != nil {
			t.Fatalf("reading %s: %s", v.file, err)
		}

		// Prose wraps at 100 columns, so a phrase can straddle a line break.
		flattened := whitespace.ReplaceAllString(string(page), " ")

		if !strings.Contains(flattened, v.phrase) {
			continue
		}

		if strings.Contains(string(source), `"`+v.key+`"`) {
			t.Errorf("%s says %q, but the service reads %s - the feature exists", v.file, v.phrase, v.key)
		}
	}
}
