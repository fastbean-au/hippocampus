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

// secretKeyPattern matches a configuration key read through viper whose name says it holds a
// credential. It is a heuristic over names, deliberately broad: a key it catches that is not really
// secret costs one table row, while a secret it misses is one an operator is never told to keep out
// of a committed config file.
var secretKeyPattern = regexp.MustCompile(`viper\.Get[A-Za-z]*\("([A-Za-z0-9.]*(?:[Ss]ecret|[Pp]assword|[Tt]oken|[Aa]piKey|[Dd]sn)[A-Za-z0-9.]*)"\)`)

// TestEverySecretIsInTheSecretsTable holds docs/security.md's secrets table to the code. The table
// is the page's answer to "what must I inject rather than commit", and it had fallen four keys
// behind - both model API keys and both callback credentials (TODO-3 item 155) - because nothing
// compared it with what the service reads. Each secret-like key read in the service's own packages
// must appear in the table as its HIPPOCAMPUS_* environment override.
func TestEverySecretIsInTheSecretsTable(t *testing.T) {
	root := filepath.Join("..", "..")

	page, err := os.ReadFile(filepath.Join(root, "docs", "security.md"))
	if err != nil {
		t.Fatalf("reading docs/security.md: %s", err)
	}

	start := strings.Index(string(page), "## Secrets")
	if start < 0 {
		t.Fatal("docs/security.md has no Secrets section")
	}

	section := string(page)[start:]
	if end := strings.Index(section[len("## Secrets"):], "\n## "); end >= 0 {
		section = section[:len("## Secrets")+end]
	}

	keys := map[string]bool{}

	for _, dir := range []string{"cmd/hippocampus", "hippocampus", "auth"} {
		files, err := filepath.Glob(filepath.Join(root, dir, "*.go"))
		if err != nil {
			t.Fatalf("listing %s: %s", dir, err)
		}

		for _, file := range files {
			if strings.HasSuffix(file, "_test.go") {
				continue
			}

			source, err := os.ReadFile(file)
			if err != nil {
				t.Fatalf("reading %s: %s", file, err)
			}

			for _, match := range secretKeyPattern.FindAllStringSubmatch(string(source), -1) {
				keys[match[1]] = true
			}
		}
	}

	if len(keys) == 0 {
		t.Fatal("found no secret-like keys at all, so the scan is broken rather than the table complete")
	}

	for key := range keys {
		env := "HIPPOCAMPUS_" + strings.ToUpper(strings.ReplaceAll(key, ".", "_"))

		if !strings.Contains(section, "`"+env+"`") {
			t.Errorf("the service reads %s, but the secrets table does not list %s", key, env)
		}
	}
}
