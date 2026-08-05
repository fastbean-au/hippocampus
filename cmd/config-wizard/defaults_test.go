package main

import (
	"encoding/json"
	"maps"
	"os"
	"regexp"
	"strings"
	"testing"
)

// The wizard can emit a "minimal" config that carries only the keys which actually change the
// service's behaviour. That is only safe while the wizard knows exactly which keys the service
// gives a default of its own: every other key must be written out, because an absent key reads as
// its zero value and several of them (consolidation.method, aggressiveness, unitsOfAgeInDays) make
// the service refuse to start at zero. The wizard records that knowledge as `svc:` on a field, and
// the service's own list is the viper.SetDefault calls in cmd/hippocampus/main.go — two files that
// would otherwise drift apart silently, producing a config that looks right and does not start.
//
// These tests keep them in step in both directions.

var (
	setDefaultPattern = regexp.MustCompile(`viper\.SetDefault\("([^"]+)",\s*([^)]+)\)`)
	fieldKeyPattern   = regexp.MustCompile(`key: "([^"]+)"`)
	svcPattern        = regexp.MustCompile(`\bsvc:\s*([^,\n]+)`)
)

// serviceDefaults reads the defaults the service applies to absent keys.
func serviceDefaults(t *testing.T) map[string]string {
	t.Helper()

	source, err := os.ReadFile("../hippocampus/main.go")
	if err != nil {
		t.Fatalf("failed to read the service's main.go: %s", err.Error())
	}

	defaults := make(map[string]string)

	for _, match := range setDefaultPattern.FindAllStringSubmatch(string(source), -1) {
		defaults[match[1]] = normaliseLiteral(match[2])
	}

	if len(defaults) == 0 {
		t.Fatal("found no viper.SetDefault calls — the pattern no longer matches the service")
	}

	return defaults
}

// wizardFields returns each field the wizard schema declares, mapped to the service default it
// claims (an empty string meaning it claims none).
func wizardFields(t *testing.T) map[string]string {
	t.Helper()

	source, err := wizardAssets.ReadFile("wizard/app.js")
	if err != nil {
		t.Fatalf("failed to read the wizard schema: %s", err.Error())
	}

	// Everything from one `key: "..."` up to the next belongs to that field, so a `svc:` in that
	// window is that field's.
	locations := fieldKeyPattern.FindAllStringSubmatchIndex(string(source), -1)
	fields := make(map[string]string, len(locations))

	for i, location := range locations {
		key := string(source)[location[2]:location[3]]

		end := len(source)
		if i+1 < len(locations) {
			end = locations[i+1][0]
		}

		claimed := ""

		if match := svcPattern.FindStringSubmatch(string(source)[location[1]:end]); match != nil {
			claimed = normaliseLiteral(match[1])
		}

		fields[key] = claimed
	}

	if len(fields) == 0 {
		t.Fatal("found no fields in the wizard schema — the pattern no longer matches it")
	}

	return fields
}

// normaliseLiteral reduces a Go or JavaScript literal to a comparable form: 25, "sqlite", and true
// all compare across the two languages, which is as much as this needs.
func normaliseLiteral(literal string) string {
	return strings.Trim(strings.TrimSpace(literal), `"`)
}

// TestWizardKnowsEveryServiceDefault fails when the service gains (or loses) a viper.SetDefault for
// a key the wizard manages without the wizard's `svc:` following it. Without the annotation the
// wizard would write the key out needlessly — harmless — or, worse, treat a service-defaulted key
// as one it must always emit and drift from the service's own value.
func TestWizardKnowsEveryServiceDefault(t *testing.T) {
	defaults := serviceDefaults(t)
	fields := wizardFields(t)

	for key, expected := range defaults {
		claimed, managed := fields[key]
		if !managed {
			// Not every defaulted key is exposed by the wizard (opensearch.reconcileBatchSize, for
			// one); a key it does not offer cannot be emitted wrongly.
			continue
		}

		if claimed == "" {
			t.Errorf("the service defaults %q to %s, but the wizard field does not declare svc: — add it, or a minimal config will carry the key needlessly", key, expected)

			continue
		}

		if claimed != expected {
			t.Errorf("the wizard claims the service defaults %q to %q, but main.go sets %q", key, claimed, expected)
		}
	}
}

// TestWizardClaimsNoDefaultTheServiceLacks is the other direction, and the dangerous one: a field
// claiming a service default that does not exist would be omitted from a minimal config, leaving
// the service to read the key as zero. For consolidation.unitsOfAgeInDays that is a startup
// failure; for sleep.periodSeconds it is a service that never consolidates.
func TestWizardClaimsNoDefaultTheServiceLacks(t *testing.T) {
	defaults := serviceDefaults(t)

	for key, claimed := range wizardFields(t) {
		if claimed == "" {
			continue
		}

		if _, exists := defaults[key]; !exists {
			t.Errorf("the wizard claims the service defaults %q, but main.go has no viper.SetDefault for it — a minimal config would omit the key and the service would read it as zero", key)
		}
	}
}

// TestWizardManagesEveryKeyInTheExampleConfig checks the wizard against the repo's own example
// config: every key an operator can see in config.json should be one the wizard can set, or the
// wizard silently drops it on import.
func TestWizardManagesEveryKeyInTheExampleConfig(t *testing.T) {
	source, err := os.ReadFile("../../config.json")
	if err != nil {
		t.Fatalf("failed to read config.json: %s", err.Error())
	}

	fields := wizardFields(t)

	// Keys the wizard deliberately leaves out, with the reason. Each is either an alias for
	// something it does offer, or a structure a form cannot usefully collect.
	unmanaged := map[string]string{
		"auth.signingKeys": "a list of kid-tagged secrets: rotation is an operational task, not a first-configuration one",
		"auth.activeKid":   "only meaningful alongside auth.signingKeys",
		"transfer.tls":     "offered as the transfer.tls.enabled block form instead of the legacy scalar",
	}

	for key := range flattenJSON(t, source, "") {
		if _, managed := fields[key]; managed {
			continue
		}

		if _, expected := unmanaged[key]; expected {
			continue
		}

		t.Errorf("config.json carries %q, which the wizard neither offers nor lists as deliberately unmanaged", key)
	}
}

// flattenJSON turns a config document into the dotted key space the wizard and viper both use. A
// leaf is anything that is not an object, so an array (opensearch.addresses) is one key, matching
// how the wizard treats it.
func flattenJSON(t *testing.T, document []byte, prefix string) map[string]any {
	t.Helper()

	var parsed map[string]any

	if err := json.Unmarshal(document, &parsed); err != nil {
		t.Fatalf("failed to parse the config document: %s", err.Error())
	}

	return flattenMap(parsed, prefix)
}

func flattenMap(node map[string]any, prefix string) map[string]any {
	flat := make(map[string]any)

	for name, entry := range node {
		key := name

		if prefix != "" {
			key = prefix + "." + name
		}

		if nested, ok := entry.(map[string]any); ok && len(nested) > 0 {
			maps.Copy(flat, flattenMap(nested, key))

			continue
		}

		flat[key] = entry
	}

	return flat
}
