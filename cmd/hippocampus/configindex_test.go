package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/spf13/viper"
)

// The key index at the top of docs/configuration.md (TODO-3 item 173): every key the service reads,
// its default, one line on what it does, and the section that owns it. The page called itself
// exhaustive while lacking 25 keys - every core decay key among them - because the guard accepted a
// mention on any page. The index is GENERATED, from sources already held to the service by other
// guards, so it cannot drift either:
//
//   - the keys are serviceConfigKeys, plus the few read through a prefix (indexExtraKeys);
//   - the default is what setStartupDefaults installs, or a viper.SetDefault literal elsewhere in
//     main.go, or the wizard's recorded service default (svc, held by defaults_test.go);
//   - the line is the wizard field's label (TestWizardOffersEveryConfigKey), or indexNotes for a key
//     the wizard deliberately does not offer;
//   - the section is the heading that documents the key most - a JSON example outweighs passing
//     mentions, and a tie goes to the earlier page - or the one indexOwners pins.
//
// Regenerate with HIPPOCAMPUS_UPDATE_CONFIG_INDEX=1 go test ./cmd/hippocampus -run TestConfigIndex.

const (
	configIndexBegin = "<!-- config-index:begin -->"
	configIndexEnd   = "<!-- config-index:end -->"
)

// indexExtraKeys are read through a prefix the key scan cannot see - rateLimitRuleFromViper builds
// them by concatenation, and the tiers are read with viper.Sub.
var indexExtraKeys = []string{
	"rateLimit.global.requestsPerSecond",
	"rateLimit.global.burst",
	"rateLimit.perClient.requestsPerSecond",
	"rateLimit.perClient.burst",
	"rateLimit.tiers.<tier>.requestsPerSecond",
	"rateLimit.tiers.<tier>.burst",
}

// indexNotes is the line for a key the wizard does not offer, and therefore has no label for.
var indexNotes = map[string]string{
	"auth.enabled":                                           "Deprecated alias for auth.method hmac, consulted only when auth.method is unset",
	"auth.signingKeys":                                       "kid-tagged HMAC secrets, every one of which verifies, for rotation",
	"auth.activeKid":                                         "Which of auth.signingKeys --mint-token signs with",
	"auth.oauth2.issuer":                                     "The console sign-in's issuer, when it differs from auth.issuer",
	"auth.oauth2.audience":                                   "The console sign-in's audience, when it differs from auth.audience",
	"auth.ui.issuer":                                         "The console's issuer, when it differs from auth.issuer",
	"auth.ui.audience":                                       "The console's audience, when it differs from auth.audience",
	"auth.oauth2.cookieSecure":                               "Force the session cookie's Secure flag, behind a TLS-terminating proxy",
	"auth.oauth2.cookieDomain":                               "The session cookie's domain, for a console served across subdomains",
	"auth.oauth2.successRedirectUrl":                         "Where a successful sign-in lands",
	"auth.oauth2.postLogoutRedirectUrl":                      "Where the provider returns the browser after sign-out",
	"auth.oauth2.sessionTTLSeconds":                          "Console session lifetime",
	"auth.oauth2.refreshTTLSeconds":                          "Refresh cookie lifetime",
	"opensearch.applyTimeoutSeconds":                         "Bound on one index operation before it is retried",
	"opensearch.applyMaxAttempts":                            "Attempts at one index operation before it is dropped",
	"opensearch.applyRetryBaseBackoffMillis":                 "Base of the index worker's jittered retry backoff",
	"opensearch.closeDrainTimeoutSeconds":                    "How long shutdown waits for the index queue to drain",
	"opensearch.reconcileBatchSize":                          "Page size of the self-healing reconcile sweep",
	"opensearch.staleSweep":                                  "Also remove index documents the store no longer holds",
	"consolidation.significanceLevels.unusedRetentionInDays": "How long the significance registry keeps a value nothing carries",
	"opensearch.outbox.maxRows":                              "Row cap on the queued index deletions",
	"opensearch.outbox.maxAgeHours":                          "Age cap on the queued index deletions",
	"opensearch.outbox.maxBytes":                             "Byte cap on the queued index deletions",
	"reflection.enabled":                                     "Register gRPC server reflection; derived from auth.method when unset",
	"callbacks.batchSize":                                    "Deliveries one dispatcher pass claims",
	"callbacks.retryBaseBackoffSeconds":                      "Base of a failing delivery's retry backoff",
	"callbacks.retryMaxBackoffSeconds":                       "Ceiling of a failing delivery's retry backoff",
	"callbacks.tls.certFile":                                 "Client certificate for mutual TLS to the receiver",
	"callbacks.tls.keyFile":                                  "Its key",
	"callbacks.tls.insecureSkipVerify":                       "Skip verifying the receiver's certificate (dev only)",
	"topology.components":                                    "Declared inbound components, each a name, kind and healthUrl",
	"transfer.tls.certFile":                                  "Client certificate for mutual TLS to the transfer target",
	"transfer.tls.keyFile":                                   "Its key",
	"transfer.tls.insecureSkipVerify":                        "Skip verifying the transfer target's certificate (dev only)",
	"opensearch.tls.insecureSkipVerify":                      "Skip verifying the cluster's certificate (dev only)",
	"rateLimit.global.requestsPerSecond":                     "Instance-wide sustained request rate",
	"rateLimit.global.burst":                                 "Instance-wide burst",
	"rateLimit.perClient.requestsPerSecond":                  "Per-caller sustained request rate",
	"rateLimit.perClient.burst":                              "Per-caller burst",
	"rateLimit.tiers.<tier>.requestsPerSecond":               "Sustained rate for callers resolving to this tier (reader, writer, admin)",
	"rateLimit.tiers.<tier>.burst":                           "Burst for callers resolving to this tier",
	"s3.bucket":                                              "The S3 bucket archives are written to; set this or archive.directory, not both",
}

var (
	wizardLabelPattern   = regexp.MustCompile(`key: "([^"]+)",\s*\n\s*label: "([^"]+)"`)
	wizardSvcPattern     = regexp.MustCompile(`(?s)key: "([^"]+)",[^}]*?\bsvc: ([^,\n]+),`)
	setDefaultLiteral    = regexp.MustCompile(`viper\.SetDefault\("([^"]+)", ([^)]+)\)`)
	markdownHeading      = regexp.MustCompile(`^(#{1,4}) (.+)$`)
	slugDropCharacters   = regexp.MustCompile("[^a-z0-9 -]")
	indexDocumentedOrder = []string{"configuration.md", "consolidation.md", "api.md", "operations.md", "security.md"}
)

type indexRow struct {
	key      string
	def      string
	line     string
	section  string
	sectHref string
}

func TestConfigIndex(t *testing.T) {
	rows := buildConfigIndex(t)

	path := filepath.Join("..", "..", "docs", "configuration.md")

	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %s", path, err)
	}

	page := string(source)
	start := strings.Index(page, configIndexBegin)
	end := strings.Index(page, configIndexEnd)

	if start < 0 || end < start {
		t.Fatalf("%s has no %s ... %s markers", path, configIndexBegin, configIndexEnd)
	}

	generated := renderConfigIndex(rows)
	current := page[start+len(configIndexBegin) : end]

	if current == generated {
		return
	}

	if os.Getenv("HIPPOCAMPUS_UPDATE_CONFIG_INDEX") == "1" {
		updated := page[:start+len(configIndexBegin)] + generated + page[end:]

		if err := os.WriteFile(path, []byte(updated), 0o600); err != nil {
			t.Fatalf("writing %s: %s", path, err)
		}

		return
	}

	t.Errorf("the key index in docs/configuration.md is stale; regenerate it with " +
		"HIPPOCAMPUS_UPDATE_CONFIG_INDEX=1 go test ./cmd/hippocampus -run TestConfigIndex")
}

// indexKeys is every key the index carries a row for, which TestEveryConfigKeyIsDocumented reads.
// indexRowKey matches the key cell of a row in the committed index.
var indexRowKey = regexp.MustCompile("(?m)^\\| `([^`]+)` \\|")

// indexedConfigKeys is the set of keys with a row in the committed index.
func indexedConfigKeys(t *testing.T) map[string]bool {
	t.Helper()

	source, err := os.ReadFile(filepath.Join("..", "..", "docs", "configuration.md"))
	if err != nil {
		t.Fatalf("reading configuration.md: %s", err)
	}

	text := string(source)
	start, end := strings.Index(text, configIndexBegin), strings.Index(text, configIndexEnd)

	if start < 0 || end < start {
		t.Fatalf("configuration.md has no key index between %s and %s", configIndexBegin, configIndexEnd)
	}

	keys := map[string]bool{}

	for _, match := range indexRowKey.FindAllStringSubmatch(text[start:end], -1) {
		keys[match[1]] = true
	}

	return keys
}

func indexKeys(t *testing.T) map[string]bool {
	t.Helper()

	keys := map[string]bool{}

	for key := range serviceConfigKeys(t) {
		if _, omitted := undocumentedConfigKeys[key]; !omitted {
			keys[key] = true
		}
	}

	for _, key := range indexExtraKeys {
		keys[key] = true
	}

	return keys
}

func buildConfigIndex(t *testing.T) []indexRow {
	t.Helper()

	labels, svc := wizardIndexFacts(t)
	literals := setDefaultLiterals(t)
	sections := documentationSections(t)

	viper.Reset()
	defer viper.Reset()

	setStartupDefaults()

	var rows []indexRow

	for key := range indexKeys(t) {
		line := indexNotes[key]
		if line == "" {
			line = labels[key]
		}

		if line == "" {
			t.Errorf("%s has no line for the index: add it to indexNotes (the wizard does not label it)", key)
		}

		def := "—"

		switch {

		case viper.IsSet(key):
			def = "`" + formatDefault(viper.Get(key)) + "`"

		case literals[key] != "":
			def = "`" + literals[key] + "`"

		case svc[key] != "":
			def = "`" + svc[key] + "`"

		}

		section, href := owningSection(key, sections)
		if section == "" {
			t.Errorf("%s is documented in no section of %v: write its entry", key, indexDocumentedOrder)
		}

		rows = append(rows, indexRow{key: key, def: def, line: line, section: section, sectHref: href})
	}

	sort.Slice(rows, func(i int, j int) bool { return rows[i].key < rows[j].key })

	return rows
}

func renderConfigIndex(rows []indexRow) string {
	var b strings.Builder

	b.WriteString("\n\n| Key | Default | What it does | Documented in |\n| --- | --- | --- | --- |\n")

	for _, row := range rows {
		fmt.Fprintf(&b, "| `%s` | %s | %s | [%s](%s) |\n", row.key, row.def, row.line, row.section, row.sectHref)
	}

	b.WriteString("\n")

	return b.String()
}

func formatDefault(value any) string {
	switch v := value.(type) {

	case string:
		return fmt.Sprintf("%q", v)

	case []string:
		return fmt.Sprintf("%q", v)

	}

	return fmt.Sprintf("%v", value)
}

func wizardIndexFacts(t *testing.T) (map[string]string, map[string]string) {
	t.Helper()

	source, err := os.ReadFile(filepath.Join("..", "config-wizard", "wizard", "app.js"))
	if err != nil {
		t.Fatalf("reading the wizard: %s", err)
	}

	labels := map[string]string{}

	for _, match := range wizardLabelPattern.FindAllStringSubmatch(string(source), -1) {
		labels[match[1]] = match[2]
	}

	svc := map[string]string{}

	for _, match := range wizardSvcPattern.FindAllStringSubmatch(string(source), -1) {
		svc[match[1]] = strings.TrimSpace(match[2])
	}

	return labels, svc
}

func setDefaultLiterals(t *testing.T) map[string]string {
	t.Helper()

	source, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("reading main.go: %s", err)
	}

	literals := map[string]string{}

	for _, match := range setDefaultLiteral.FindAllStringSubmatch(string(source), -1) {
		literals[match[1]] = match[2]
	}

	return literals
}

type docSection struct {
	page  string
	title string
	slug  string

	// keys scores each key the section documents: a JSON config example is a reference block and
	// weighs heavily, and each prose mention adds one.
	keys map[string]int
}

// documentationSections splits the docs into sections and records the keys each documents, by
// inline code or in a JSON example. The index itself is cut out first, or it would own every key.
func documentationSections(t *testing.T) []docSection {
	t.Helper()

	var sections []docSection

	for _, page := range indexDocumentedOrder {
		source, err := os.ReadFile(filepath.Join("..", "..", "docs", page))
		if err != nil {
			t.Fatalf("reading %s: %s", page, err)
		}

		text := string(source)

		if start, end := strings.Index(text, configIndexBegin), strings.Index(text, configIndexEnd); start >= 0 && end > start {
			text = text[:start] + text[end+len(configIndexEnd):]
		}

		var current *docSection

		// GitHub suffixes a repeated heading's anchor with -1, -2, ..., so a link to the bare slug
		// names the first of them.
		slugs := map[string]int{}

		var body strings.Builder

		flush := func() {
			if current == nil {
				return
			}

			chunk := body.String()

			for _, match := range documentedKeyPattern.FindAllStringSubmatch(chunk, -1) {
				current.keys[match[1]]++
			}

			for key := range jsonExampleKeys(chunk) {
				current.keys[key] += 10
			}

			sections = append(sections, *current)
		}

		for _, line := range strings.Split(text, "\n") {
			if match := markdownHeading.FindStringSubmatch(line); match != nil {
				flush()

				title := strings.ReplaceAll(match[2], "`", "")
				slug := headingSlug(match[2])

				if seen := slugs[slug]; seen > 0 {
					slugs[slug]++
					slug = fmt.Sprintf("%s-%d", slug, seen)
				} else {
					slugs[slug] = 1
				}

				current = &docSection{page: page, title: title, slug: slug, keys: map[string]int{}}

				body.Reset()

				continue
			}

			body.WriteString(line)
			body.WriteString("\n")
		}

		flush()
	}

	return sections
}

// owningSection is the section that documents the key most: a JSON config example outweighs any
// number of passing mentions, and a tie goes to the earlier page. indexOwners overrides it where the
// heuristic picks a passing mention. For a tier row it is the section documenting the tiers.
func owningSection(key string, sections []docSection) (string, string) {
	probe := key

	if strings.Contains(key, "<tier>") {
		probe = "rateLimit.tiers"
	}

	best, bestScore := -1, 0

	for i, section := range sections {
		if wanted, ok := indexOwners[key]; ok && section.page+"#"+section.slug != wanted {
			continue
		}

		score := section.keys[probe]

		if score == 0 && probe != key {
			for documented, n := range section.keys {
				if strings.HasPrefix(documented, probe+".") {
					score += n
				}
			}
		}

		if score > bestScore {
			best, bestScore = i, score
		}
	}

	if best >= 0 {
		section := sections[best]
		href := "#" + section.slug
		title := section.title

		if section.page != "configuration.md" {
			href = section.page + href
			title = strings.TrimSuffix(section.page, ".md") + ": " + title
		}

		return title, href
	}

	// A key documented only inside a one-line JSON object ("global": {"requestsPerSecond": ...}) is
	// owned by the section documenting its parent.
	if parent := key[:max(strings.LastIndex(key, "."), 0)]; parent != "" && parent != key && !strings.Contains(key, "<tier>") {
		return owningSection(parent, sections)
	}

	return "", ""
}

// indexOwners pins a key's section where the scoring picks a passing mention, as page#slug. A pin
// naming a section that does not document the key fails the index, so a pin cannot go stale.
var indexOwners = map[string]string{
	// The opening section says what the cycle is and how often it runs; the later mentions are asides.
	"sleep.periodSeconds":                  "consolidation.md#memory-consolidation",
	"consolidation.enabled":                "consolidation.md#memory-consolidation",
	"consolidation.capacityBytes":          "consolidation.md#capacity-target",
	"consolidation.walTriggerBytes":        "consolidation.md#checkpoint-triggered-eviction",
	"consolidation.minimumRetentionInDays": "consolidation.md#minimum-retention",
	"callbacks.backlogPolicy":              "configuration.md#when-a-forget-callback-is-an-instruction-not-a-notification",
}

// headingSlug is GitHub's anchor for a heading: lower case, backticks and punctuation dropped,
// spaces to hyphens.
func headingSlug(heading string) string {
	slug := strings.ToLower(strings.ReplaceAll(heading, "`", ""))
	slug = slugDropCharacters.ReplaceAllString(slug, "")

	return strings.ReplaceAll(slug, " ", "-")
}
