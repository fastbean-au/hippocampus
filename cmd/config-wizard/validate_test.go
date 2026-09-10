package main

import (
	"regexp"
	"strings"
	"testing"
)

// The wizard's validate() is where every "this configuration does not make sense" check lives, and
// it is written entirely against string keys - val("consolidation.capacityBytes") and friends. A
// mistyped key there fails in the worst possible way: value() returns undefined for a key it does
// not know, undefined is falsey and Number(undefined) is NaN, so every comparison the check makes
// quietly answers "no" and the warning simply never appears. Nothing in the browser says so, and
// the wizard looks like it has a check it does not have.
//
// The same is true of the step id each issue is filed under. renderIssues selects an issue by
// matching it against the current step's id, so an id that names no step hides the issue from the
// page it belongs to - it survives only on the review screen, where it is easy to miss among
// everything else.
//
// These two tests hold both against the schema. They are text-scanning rather than executing tests
// for the same reason defaults_test.go is: this file is a browser script with no module boundary,
// and what wants checking is the key space, not the arithmetic.

var (
	// A whole string literal that looks like a dotted configuration key. Anchored at both ends, so
	// a key named inside a sentence in an issue's own text - which happens constantly, since the
	// messages name the settings they are about - is not mistaken for one being read.
	configKeyPattern = regexp.MustCompile(`^[a-z][A-Za-z0-9_]*(\.[A-Za-z0-9_]+)+$`)
	stringLiteral    = regexp.MustCompile(`"([^"\\]*(?:\\.[^"\\]*)*)"`)
	addCallPattern   = regexp.MustCompile(`\badd\(\s*"([a-z]+)"\s*,\s*"([a-z]+)"\s*,`)
	stepIdPattern    = regexp.MustCompile(`(?m)^    id: "([a-z-]+)",$`)
)

// wizardFunction returns the body of a top-level function in the wizard script, found by matching
// braces from its declaration.
func wizardFunction(t *testing.T, name string) string {
	t.Helper()

	source, err := wizardAssets.ReadFile("wizard/app.js")
	if err != nil {
		t.Fatalf("failed to read the wizard schema: %s", err.Error())
	}

	text := string(source)
	start := strings.Index(text, "function "+name+"() {")

	if start < 0 {
		t.Fatalf("the wizard script no longer declares function %s()", name)
	}

	depth := 0

	for i := strings.Index(text[start:], "{") + start; i < len(text); i++ {
		switch text[i] {

		case '{':
			depth++

		case '}':
			depth--

			if depth == 0 {
				return text[start : i+1]
			}

		}
	}

	t.Fatalf("failed to find the end of function %s()", name)

	return ""
}

// TestValidateReadsOnlyDeclaredKeys fails when a check reads a key the schema does not declare.
func TestValidateReadsOnlyDeclaredKeys(t *testing.T) {
	fields := wizardFields(t)
	body := wizardFunction(t, "validate")

	for _, match := range stringLiteral.FindAllStringSubmatch(body, -1) {
		literal := match[1]

		if !configKeyPattern.MatchString(literal) {
			continue
		}

		if _, declared := fields[literal]; !declared {
			t.Errorf("validate() reads %q, which the wizard schema does not declare as a field — the check silently never fires, because an unknown key reads as undefined", literal)
		}
	}
}

// TestValidateFilesIssuesUnderRealSteps fails when an issue names a step the wizard does not have,
// which hides it from the page an operator would fix it on.
func TestValidateFilesIssuesUnderRealSteps(t *testing.T) {
	source, err := wizardAssets.ReadFile("wizard/app.js")
	if err != nil {
		t.Fatalf("failed to read the wizard schema: %s", err.Error())
	}

	steps := make(map[string]bool)

	for _, match := range stepIdPattern.FindAllStringSubmatch(string(source), -1) {
		steps[match[1]] = true
	}

	if len(steps) == 0 {
		t.Fatal("found no step ids — the pattern no longer matches the wizard schema")
	}

	levels := map[string]bool{"error": true, "warn": true, "info": true}
	body := wizardFunction(t, "validate")
	calls := addCallPattern.FindAllStringSubmatch(body, -1)

	if len(calls) == 0 {
		t.Fatal("found no add() calls in validate() — the pattern no longer matches it")
	}

	for _, call := range calls {
		if !levels[call[1]] {
			t.Errorf("validate() raises an issue at level %q, which styles.css has no rule for — use error, warn, or info", call[1])
		}

		if !steps[call[2]] {
			t.Errorf("validate() files an issue under step %q, which no step declares — it would never appear on any step page", call[2])
		}
	}
}
