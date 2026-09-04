package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The service's configuration surface is every viper key it reads. Two other files claim to
// describe that surface in full - the documentation under docs/, and the configuration wizard's
// schema - and neither is executed by anything, so both drift silently. The failure is not
// cosmetic: a key nobody documented is a feature an operator cannot find (ollama.embedding
// truncation), and a key the wizard does not offer is one a generated config omits, leaving the
// service to read it as its zero value.
//
// The guards below make both claims mechanical. They also catch the shape that prompted them: a
// key read under a name nothing else in the repo uses - `observability.traces.enabled`, where the
// service reads and documents `observability.tracing.enabled` - which presents as a setting that
// does nothing rather than as an error. A misspelling has no documentation entry by construction,
// so requiring one catches it.
//
// Both exception lists take a reason per key, and TestNoStaleConfigKeyExceptions rejects an
// exception naming a key the service no longer reads, so the lists cannot outlive their subjects.

var (
	// The reads that make up the surface. SetDefault is included: defaulting a key is as much a
	// declaration that it exists as reading one.
	configKeyPattern = regexp.MustCompile(`viper\.(?:Get[A-Za-z]*|IsSet|Sub|UnmarshalKey|SetDefault)\("([^"]+)"`)

	// Command-line flags reach viper through BindPFlags, so they answer to GetString like a config
	// key while belonging to the CLI rather than to config.json.
	flagNamePattern = regexp.MustCompile(`flags\.[A-Za-z]+P?\("([^"]+)"`)

	// A dotted key written as inline code in the documentation.
	documentedKeyPattern = regexp.MustCompile("`([a-z][A-Za-z0-9]*(?:\\.[A-Za-z0-9]+)+)`")

	// A key in one of the documentation's JSON examples, which is how most of them are shown.
	jsonKeyPattern = regexp.MustCompile(`^"([A-Za-z_][A-Za-z0-9_]*)"\s*:\s*(.*)`)

	wizardFieldPattern = regexp.MustCompile(`key: "([^"]+)"`)
)

// undocumentedConfigKeys are read but deliberately absent from docs/, with the reason.
var undocumentedConfigKeys = map[string]string{
	"rateLimit.tiers": "a prefix, not a key: the documented keys are rateLimit.tiers.<tier>.requestsPerSecond/burst, read through this one with viper.Sub",
	"transfer.tls":    "a prefix read with IsSet to accept the legacy scalar form; the documented key is transfer.tls.enabled",
}

// unmanagedConfigKeys are read but deliberately not offered by the wizard, with the reason. The
// wizard builds a first configuration; a key belonging to an operational task performed later, or
// one a form cannot usefully collect, does not belong in it.
var unmanagedConfigKeys = map[string]string{
	"auth.enabled":                           "the deprecated boolean alias for auth.method, consulted only when auth.method is unset; the wizard offers auth.method",
	"auth.signingKeys":                       "a list of kid-tagged secrets: rotation is an operational task, not a first-configuration one",
	"auth.activeKid":                         "only meaningful alongside auth.signingKeys",
	"auth.oauth2.issuer":                     "defaults to auth.issuer, which the wizard offers; overriding it is for a provider whose login issuer differs from its API issuer",
	"auth.oauth2.audience":                   "defaults to auth.audience, as above",
	"auth.ui.issuer":                         "defaults to auth.issuer, as above",
	"auth.ui.audience":                       "defaults to auth.audience, as above",
	"auth.oauth2.cookieSecure":               "follows the redirectUrl scheme, which the wizard collects; setting it is for a TLS-terminating proxy that the URL does not describe",
	"auth.oauth2.cookieDomain":               "an override for a console served across subdomains",
	"auth.oauth2.successRedirectUrl":         "defaults to /ui, the console the wizard is configuring",
	"auth.oauth2.postLogoutRedirectUrl":      "provider-side RP-initiated logout target, meaningful only where the provider advertises end_session_endpoint",
	"auth.oauth2.sessionTTLSeconds":          "session lifetime tuning, defaulted and rarely a first-configuration decision",
	"auth.oauth2.refreshTTLSeconds":          "the same, for the refresh cookie",
	"opensearch.applyTimeoutSeconds":         "index-worker tuning: the four apply*/closeDrain* knobs bound retries on a best-effort secondary index and are defaulted",
	"opensearch.applyMaxAttempts":            "as above",
	"opensearch.applyRetryBaseBackoffMillis": "as above",
	"opensearch.closeDrainTimeoutSeconds":    "as above",
	"opensearch.reconcileBatchSize":          "page size for the self-healing sweep; defaulted, and tuned only against a measured store",
	"opensearch.staleSweep":                  "the reverse half of that sweep, which defaults on and is turned off only to diagnose it; offering a checkbox for a correctness backstop invites turning it off",
	"opensearch.outbox.maxRows":              "a bound on the delete queue, reached only when the index cannot keep up; defaulted, and a number to raise against a measured backlog rather than to guess at up front",
	"opensearch.outbox.maxAgeHours":          "as above",
	"reflection.enabled":                     "the service derives its default from auth.method (on when auth is off, off otherwise), which is right for both configurations the wizard produces; a wizard field's default is a static literal, so offering it would mean picking one of the two and writing the other out wrongly",
	"callbacks.batchSize":                    "dispatcher tuning: how many deliveries one pass claims, defaulted and tuned only against a measured backlog rather than guessed at up front",
	"callbacks.retryBaseBackoffSeconds":      "as above, for the retry curve against a failing receiver",
	"callbacks.retryMaxBackoffSeconds":       "as above",
	"callbacks.tls.certFile":                 "mutual TLS to the callback receiver; the wizard offers the trust half (caCertFile), which is what a first configuration needs",
	"callbacks.tls.keyFile":                  "as above",
	"callbacks.tls.insecureSkipVerify":       "a dev-only escape hatch the wizard should not encourage",
	"topology.components":                    "a list of declared inbound components, each a name/kind/healthUrl triple: a deployment inventory rather than a setting, and one the wizard cannot know",
	"transfer.tls.certFile":                  "mutual TLS to the transfer target; the wizard offers the trust half (caCertFile), which is what a first configuration needs",
	"transfer.tls.keyFile":                   "as above",
	"transfer.tls.insecureSkipVerify":        "a dev-only escape hatch the wizard should not encourage",
	"opensearch.tls.insecureSkipVerify":      "as above, for the index connection",
	"rateLimit.tiers":                        "a prefix, not a field: the wizard offers rateLimit.tiers.<tier>.requestsPerSecond/burst, which the service reads through this one with viper.Sub",
	"transfer.tls":                           "a prefix read with IsSet to accept the legacy scalar form; the wizard offers the block form, transfer.tls.enabled",
}

// serviceConfigKeys is every viper key the service reads, from the root module's non-test sources.
//
// The integration modules are excluded because they configure themselves through their own flags,
// demo/ because the load generator has its own unrelated key space, and .claude/ because agent
// worktrees hold copies of this repo at other revisions - one of which still carried the American
// spellings from before the rename, which is a good illustration of why the walk names what it
// skips.
func serviceConfigKeys(t *testing.T) map[string]bool {
	t.Helper()

	skipped := map[string]bool{".git": true, ".claude": true, "integrations": true, "demo": true, "node_modules": true, ".trunk": true}
	keys := make(map[string]bool)

	root := filepath.Join("..", "..")

	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if entry.IsDir() {
			if skipped[entry.Name()] {
				return filepath.SkipDir
			}

			return nil
		}

		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}

		source, err := os.ReadFile(path)
		if err != nil {
			return err
		}

		for _, match := range configKeyPattern.FindAllStringSubmatch(string(source), -1) {
			keys[match[1]] = true
		}

		return nil
	})
	if err != nil {
		t.Fatalf("failed to walk the module for viper reads: %s", err.Error())
	}

	if len(keys) == 0 {
		t.Fatal("found no viper reads - the pattern no longer matches the service")
	}

	for name := range commandLineFlags(t) {
		delete(keys, name)
	}

	return keys
}

// commandLineFlags reads the flag names registered in execute(), which BindPFlags makes readable
// through viper without their being configuration.
func commandLineFlags(t *testing.T) map[string]bool {
	t.Helper()

	source, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("failed to read main.go: %s", err.Error())
	}

	names := make(map[string]bool)

	for _, match := range flagNamePattern.FindAllStringSubmatch(string(source), -1) {
		names[match[1]] = true
	}

	if len(names) == 0 {
		t.Fatal("found no flag registrations - the pattern no longer matches main.go")
	}

	return names
}

// documentedConfigKeys is every key the documentation names, in either of the two forms it uses:
// inline code (`storage.driver`), and a JSON example, which is nested and therefore has to be
// flattened back to the dotted form viper uses.
func documentedConfigKeys(t *testing.T) map[string]bool {
	t.Helper()

	paths, err := filepath.Glob(filepath.Join("..", "..", "docs", "*.md"))
	if err != nil {
		t.Fatalf("failed to list the documentation: %s", err.Error())
	}

	paths = append(paths, filepath.Join("..", "..", "README.md"))
	keys := make(map[string]bool)

	for _, path := range paths {
		source, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("failed to read %s: %s", path, err.Error())
		}

		for _, match := range documentedKeyPattern.FindAllStringSubmatch(string(source), -1) {
			keys[match[1]] = true
		}

		for key := range jsonExampleKeys(string(source)) {
			keys[key] = true
		}
	}

	if len(keys) == 0 {
		t.Fatal("found no configuration keys in the documentation - the patterns no longer match it")
	}

	return keys
}

// jsonExampleKeys flattens the JSON configuration examples in one document to dotted keys.
//
// The examples are annotated JSON rather than JSON - they carry // comments and elisions - so this
// tracks nesting by line rather than parsing. That is enough for what it is asked: a key appearing
// at the right depth under the right parents. Comments are stripped first so a brace inside one
// cannot unbalance the stack, and the stack is reset at each fence so an example that elides its
// closing braces cannot leak into the next.
func jsonExampleKeys(document string) map[string]bool {
	keys := make(map[string]bool)
	stack := []string{}
	inFence := false

	for _, line := range strings.Split(document, "\n") {
		trimmed := strings.TrimSpace(line)

		if strings.HasPrefix(trimmed, "```") {
			inFence = !inFence
			stack = stack[:0]

			continue
		}

		if !inFence {
			continue
		}

		if index := strings.Index(trimmed, "//"); index >= 0 {
			trimmed = strings.TrimSpace(trimmed[:index])
		}

		if match := jsonKeyPattern.FindStringSubmatch(trimmed); match != nil {
			if strings.HasPrefix(match[2], "{") {
				stack = append(stack, match[1])

				continue
			}

			keys[strings.Join(append(append([]string{}, stack...), match[1]), ".")] = true

			continue
		}

		for _, character := range trimmed {
			if character == '}' && len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		}
	}

	return keys
}

// wizardConfigKeys is every key the configuration wizard can set.
func wizardConfigKeys(t *testing.T) map[string]bool {
	t.Helper()

	source, err := os.ReadFile(filepath.Join("..", "config-wizard", "wizard", "app.js"))
	if err != nil {
		t.Fatalf("failed to read the wizard schema: %s", err.Error())
	}

	keys := make(map[string]bool)

	for _, match := range wizardFieldPattern.FindAllStringSubmatch(string(source), -1) {
		keys[match[1]] = true
	}

	if len(keys) == 0 {
		t.Fatal("found no fields in the wizard schema - the pattern no longer matches it")
	}

	return keys
}

// TestEveryConfigKeyIsDocumented fails when the service reads a key that appears nowhere in the
// documentation. A key nobody wrote down is one an operator can only find by reading the source,
// and a key whose name is a typo is one that can never be found at all.
func TestEveryConfigKeyIsDocumented(t *testing.T) {
	documented := documentedConfigKeys(t)

	for key := range serviceConfigKeys(t) {
		if documented[key] {
			continue
		}

		if _, expected := undocumentedConfigKeys[key]; expected {
			continue
		}

		t.Errorf("the service reads %q, which appears in no document - document it, or add it to undocumentedConfigKeys with the reason", key)
	}
}

// TestWizardOffersEveryConfigKey fails when the service reads a key the wizard cannot set. The
// wizard is how a configuration is meant to be produced, so a key it does not offer is one that
// silently reads as its zero value in every config built with it.
func TestWizardOffersEveryConfigKey(t *testing.T) {
	offered := wizardConfigKeys(t)

	for key := range serviceConfigKeys(t) {
		if offered[key] {
			continue
		}

		if _, expected := unmanagedConfigKeys[key]; expected {
			continue
		}

		t.Errorf("the service reads %q, which the wizard does not offer - add a field for it, or add it to unmanagedConfigKeys with the reason", key)
	}
}

// TestNoStaleConfigKeyExceptions is what keeps the two lists above honest: an exception naming a
// key the service no longer reads is an exemption granted to nothing, and the next key to take
// that name inherits it silently.
func TestNoStaleConfigKeyExceptions(t *testing.T) {
	keys := serviceConfigKeys(t)

	for _, exceptions := range []map[string]string{undocumentedConfigKeys, unmanagedConfigKeys} {
		for key, reason := range exceptions {
			if !keys[key] {
				t.Errorf("%q is excepted (%q) but the service no longer reads it - remove the exception", key, reason)
			}
		}
	}
}
