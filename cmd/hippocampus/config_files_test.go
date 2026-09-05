package main

import (
	"bytes"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/spf13/viper"
)

// shippedConfigGlobs locates every configuration file this repository ships, relative to the repo
// root. Globs rather than a list, so a configuration added later is covered without anybody
// remembering to add it here - which is the failure mode this whole test exists to close.
var shippedConfigGlobs = []string{
	"config.json",
	"demo/config*.json",
	"deploy/compose/config.*.json",
	"deploy/k8s/overlays/*/config.json",
}

// TestShippedConfigsAreValid runs the service's own validation over every configuration file in this
// repository.
//
// It exists because of what 0.41.0 shipped. That release added a check refusing a non-positive
// consolidation.deletionThreshold, on reasoning that turned out not to cover a real mode; two
// instances on the public demo were configured that way and could not start. Nothing caught it,
// because a validation rule is written against the configurations its author can see and every
// config in THIS repository satisfied the new rule. This test is the floor under that: it cannot
// see configurations held elsewhere - that is what --check-config is for - but it makes the ones
// here a tripwire, and they cover all three drivers and both deployment targets.
//
// It validates defaults-then-file, exactly as execute() resolves a configuration, because viper
// falls back to a default only for an unset key: checking the file alone would validate a
// configuration the service never actually runs.
func TestShippedConfigsAreValid(t *testing.T) {
	configs := shippedConfigs(t)
	if len(configs) == 0 {
		t.Fatal("found no configuration files to validate - the globs in shippedConfigGlobs have gone stale")
	}

	defer viper.Reset()

	for _, config := range configs {
		relative, err := filepath.Rel(repoRoot, config)
		if err != nil {
			relative = config
		}

		t.Run(relative, func(t *testing.T) {
			viper.Reset()
			setStartupDefaults()

			body, err := os.ReadFile(config)
			if err != nil {
				t.Fatalf("failed to read %s: %v", relative, err)
			}

			viper.SetConfigType("json")

			if err := viper.ReadConfig(bytes.NewBuffer(body)); err != nil {
				t.Fatalf("%s is not parseable as JSON: %v", relative, err)
			}

			for _, problem := range configProblems() {
				t.Errorf("%s would be refused at startup: %v", relative, problem)
			}
		})
	}
}

// TestShippedConfigsCoverEveryDriver pins that the tripwire above is watching all three storage
// drivers. A check that is dialect-specific - and several of the storage ones are - is only covered
// if a configuration naming that driver is among the files, so a repository that quietly lost its
// mysql example would weaken the test above without failing it.
func TestShippedConfigsCoverEveryDriver(t *testing.T) {
	defer viper.Reset()

	seen := map[string]bool{}

	for _, config := range shippedConfigs(t) {
		viper.Reset()
		setStartupDefaults()

		body, err := os.ReadFile(config)
		if err != nil {
			t.Fatalf("failed to read %s: %v", config, err)
		}

		viper.SetConfigType("json")

		if err := viper.ReadConfig(bytes.NewBuffer(body)); err != nil {
			t.Fatalf("%s is not parseable as JSON: %v", config, err)
		}

		seen[viper.GetString("storage.driver")] = true
	}

	for _, driver := range []string{"sqlite", "postgres", "mysql"} {
		if !seen[driver] {
			t.Errorf("no shipped configuration uses storage.driver %q, so TestShippedConfigsAreValid does not exercise its checks", driver)
		}
	}
}

// shippedConfigs expands the globs into a sorted, de-duplicated list of paths.
func shippedConfigs(t *testing.T) []string {
	t.Helper()

	unique := map[string]bool{}

	for _, glob := range shippedConfigGlobs {
		matches, err := filepath.Glob(filepath.Join(repoRoot, glob))
		if err != nil {
			t.Fatalf("bad glob %q: %v", glob, err)
		}

		for _, match := range matches {
			unique[match] = true
		}
	}

	out := make([]string, 0, len(unique))

	for path := range unique {
		out = append(out, path)
	}

	sort.Strings(out)

	return out
}
