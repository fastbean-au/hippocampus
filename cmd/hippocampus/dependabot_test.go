package main

import (
	"os"
	"path/filepath"
	"sort"
	"testing"

	yaml "go.yaml.in/yaml/v3"
)

// This repository is six Go modules, and they are not six independent dependency sets. The five
// under integrations/ each carry a `replace github.com/fastbean-au/hippocampus => ../..`, so a
// module left behind by an update does not merely lag: its recorded requirements disagree with the
// root's and `go build` refuses the module outright. That is not hypothetical - a grpc security
// bump reached five of the six, skipped integrations/cli, and turned that CI job red while every
// other job stayed green.
//
// .github/dependabot.yml now names all six, which fixes the case that happened. What it cannot fix
// by itself is the NEXT one: a seventh module added without a line in that file is invisible in
// exactly the same way, and nothing about adding a module prompts anyone to open it. So this guard
// holds the file to the tree.
//
// It deliberately compares against the modules on DISK rather than a list written here, since a
// list here would be a third thing to keep in step.

const dependabotConfigPath = "../../.github/dependabot.yml"

// repoRoot is where the module walk starts - the same relative base the path above uses.
const repoRoot = "../.."

// dependabotConfig is the fragment of the schema this test reads.
type dependabotConfig struct {
	Updates []struct {
		Ecosystem   string   `yaml:"package-ecosystem"`
		Directory   string   `yaml:"directory"`
		Directories []string `yaml:"directories"`
	} `yaml:"updates"`
}

// goModuleDirs returns every directory in the repository holding a tracked go.mod, as a
// repository-root-relative path in dependabot's form ("/" for the root).
func goModuleDirs(t *testing.T) []string {
	t.Helper()

	var dirs []string

	err := filepath.WalkDir(repoRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() {
			// _build is the OpenTelemetry Collector Builder's generated output: gitignored, and
			// its go.mod is written fresh by OCB on every run, so it is not a module anything
			// could or should update. .git and node_modules are simply not ours.
			switch d.Name() {

			case "_build", ".git", "node_modules":
				return filepath.SkipDir

			}

			return nil
		}

		if d.Name() != "go.mod" {
			return nil
		}

		rel, err := filepath.Rel(repoRoot, filepath.Dir(path))
		if err != nil {
			return err
		}

		if rel == "." {
			dirs = append(dirs, "/")

			return nil
		}

		dirs = append(dirs, "/"+filepath.ToSlash(rel))

		return nil
	})
	if err != nil {
		t.Fatalf("walking the repository for go.mod files: %s", err)
	}

	sort.Strings(dirs)

	return dirs
}

// TestDependabotCoversEveryGoModule fails when a module exists that Dependabot is not told about,
// or when the config names a directory that no longer holds a module.
func TestDependabotCoversEveryGoModule(t *testing.T) {
	raw, err := os.ReadFile(dependabotConfigPath)
	if err != nil {
		t.Fatalf("reading %s: %s", dependabotConfigPath, err)
	}

	var config dependabotConfig

	if err := yaml.Unmarshal(raw, &config); err != nil {
		t.Fatalf("parsing %s: %s", dependabotConfigPath, err)
	}

	var declared []string

	for _, v := range config.Updates {
		if v.Ecosystem != "gomod" {
			continue
		}

		declared = append(declared, v.Directories...)

		if v.Directory != "" {
			declared = append(declared, v.Directory)
		}
	}

	if len(declared) == 0 {
		t.Fatalf("%s declares no gomod directories at all", dependabotConfigPath)
	}

	sort.Strings(declared)

	found := goModuleDirs(t)

	for _, v := range found {
		if !contains(declared, v) {
			t.Errorf(
				"module %s has a go.mod but is not in %s - an update reaching the other modules and not this one leaves a tree that does not build",
				v, dependabotConfigPath,
			)
		}
	}

	for _, v := range declared {
		if !contains(found, v) {
			t.Errorf("%s names %s, which holds no go.mod", dependabotConfigPath, v)
		}
	}
}

// contains reports whether values holds want. Explicit rather than a substring match, which would
// be wrong over these paths: /integrations/otel is a prefix of /integrations/otel/hippocampusexporter.
func contains(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}

	return false
}

// TestDependabotGroupsGoModulesAcrossDirectories pins the one setting that makes the six move
// together. Without `group-by: dependency-name` Dependabot opens a pull request per dependency PER
// DIRECTORY, which is the shape that produced the skew: six PRs, five merged, one forgotten.
//
// It is not a complete guarantee and is not claimed as one - group-by applies to version updates,
// while a SECURITY update is still opened per directory - but it is what keeps the modules in step
// between advisories, and losing it silently would remove that.
func TestDependabotGroupsGoModulesAcrossDirectories(t *testing.T) {
	raw, err := os.ReadFile(dependabotConfigPath)
	if err != nil {
		t.Fatalf("reading %s: %s", dependabotConfigPath, err)
	}

	var config struct {
		Updates []struct {
			Ecosystem string `yaml:"package-ecosystem"`
			Groups    map[string]struct {
				GroupBy string `yaml:"group-by"`
			} `yaml:"groups"`
		} `yaml:"updates"`
	}

	if err := yaml.Unmarshal(raw, &config); err != nil {
		t.Fatalf("parsing %s: %s", dependabotConfigPath, err)
	}

	for _, v := range config.Updates {
		if v.Ecosystem != "gomod" {
			continue
		}

		for name, group := range v.Groups {
			if group.GroupBy == "dependency-name" {
				return
			}

			t.Logf("gomod group %q has group-by %q", name, group.GroupBy)
		}
	}

	t.Errorf(
		"no gomod group in %s sets `group-by: dependency-name`, so a shared dependency is offered as one pull request per module and the six can drift apart",
		dependabotConfigPath,
	)
}
