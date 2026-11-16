package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// wrapsADescriptor matches an *os.File built around a raw descriptor number, which is never a
// discard sink however it is named.
var wrapsADescriptor = regexp.MustCompile(`os\.NewFile\(\s*[0-9]`)

// TestNoTestWrapsARawDescriptor walks every module's tests, the integrations included, for an
// os.NewFile around a descriptor number (TODO-3 item 201).
//
// An os.NewFile on descriptor 0 named os.DevNull reads like "a file on /dev/null" and is not one:
// the name is a label and the file is descriptor 0, the test binary's standard input. Silencing a
// flag set with it is harmless until the garbage collector finalises the *os.File, which closes
// descriptor 0 - and by then the descriptor may have been reused. Under -coverprofile it is the
// profile's, and a run that passed every test fails with "error generating coverage report: ...
// bad file descriptor". io.Discard is the sink that was meant.
func TestNoTestWrapsARawDescriptor(t *testing.T) {
	root := filepath.Join("..", "..")

	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		name := entry.Name()

		if entry.IsDir() {
			if path != root && (strings.HasPrefix(name, ".") || name == "node_modules" || name == "testdata") {
				return filepath.SkipDir
			}

			return nil
		}

		if !strings.HasSuffix(name, "_test.go") {
			return nil
		}

		source, err := os.ReadFile(path)
		if err != nil {
			return err
		}

		for i, line := range strings.Split(string(source), "\n") {
			if wrapsADescriptor.MatchString(line) {
				t.Errorf("%s:%d wraps a raw descriptor, which closes it when finalised; use io.Discard: %s",
					path, i+1, strings.TrimSpace(line))
			}
		}

		return nil
	})
	if err != nil {
		t.Fatalf("walking the repository: %s", err)
	}
}

// coverageFilter captures the pattern a coverage profile is filtered through before it is reported.
var coverageFilter = regexp.MustCompile(`grep -vE '([^']+)' "?\$?\{?[a-z]*\}?\.?(?:coverage|profile)`)

// TestCoverageFiltersAgree holds the pre-commit hook's coverage filter to the one CI reports to
// Coveralls with (TODO-3 item 201). They are two copies of one decision - which packages are not
// judged by their own coverage - and a package added to one alone makes the local report and the
// badge disagree about the same tree.
func TestCoverageFiltersAgree(t *testing.T) {
	filters := map[string]string{}

	for _, path := range []string{"hooks/pre-commit", ".github/workflows/ci.yaml"} {
		source, err := os.ReadFile(filepath.Join("..", "..", path))
		if err != nil {
			t.Fatalf("reading %s: %s", path, err)
		}

		matches := coverageFilter.FindAllStringSubmatch(string(source), -1)
		if len(matches) != 1 {
			t.Fatalf("expected one coverage filter in %s, found %d", path, len(matches))
		}

		filters[path] = matches[0][1]
	}

	if filters["hooks/pre-commit"] != filters[".github/workflows/ci.yaml"] {
		t.Errorf("the coverage filters disagree:\n  hooks/pre-commit:          %s\n  .github/workflows/ci.yaml: %s",
			filters["hooks/pre-commit"], filters[".github/workflows/ci.yaml"])
	}
}
