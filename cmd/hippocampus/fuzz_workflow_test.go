package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestFuzzWorkflowCoversEveryTarget holds .github/workflows/fuzz.yaml to the fuzz targets in the
// repository, in both directions (TODO-3 item 167). A target with no matrix row runs only its seeds,
// which is a fuzz test in name only; a row naming no target fails every night with "no fuzz tests".
func TestFuzzWorkflowCoversEveryTarget(t *testing.T) {
	root := filepath.Join("..", "..")

	declared := regexp.MustCompile(`(?m)^func (Fuzz[A-Za-z0-9_]+)\(f \*testing\.F\)`)
	targets := map[string]bool{}

	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if entry.IsDir() {
			switch entry.Name() {

			case ".git", ".claude", "node_modules", ".trunk":
				return filepath.SkipDir

			}

			return nil
		}

		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}

		source, err := os.ReadFile(path)
		if err != nil {
			return err
		}

		for _, match := range declared.FindAllStringSubmatch(string(source), -1) {
			targets[match[1]] = true
		}

		return nil
	})
	if err != nil {
		t.Fatalf("walking the repository: %s", err)
	}

	if len(targets) == 0 {
		t.Fatal("found no fuzz targets - the pattern no longer matches them")
	}

	workflow, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "fuzz.yaml"))
	if err != nil {
		t.Fatalf("reading the fuzz workflow: %s", err)
	}

	rows := map[string]bool{}

	for _, match := range regexp.MustCompile(`target: (Fuzz[A-Za-z0-9_]+)`).FindAllStringSubmatch(string(workflow), -1) {
		rows[match[1]] = true
	}

	for target := range targets {
		if !rows[target] {
			t.Errorf("%s has no row in .github/workflows/fuzz.yaml, so it is never fuzzed", target)
		}
	}

	for row := range rows {
		if !targets[row] {
			t.Errorf(".github/workflows/fuzz.yaml names %s, which no test declares", row)
		}
	}
}
