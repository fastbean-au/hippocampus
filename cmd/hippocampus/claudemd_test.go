package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// CLAUDE.md is loaded into every agent session and its "Do not break" list names, for each
// invariant, the test that enforces it (TODO-3 item 176). A named test that has been renamed or
// deleted turns the list into a promise nothing keeps, so these tests hold the file to the code: every
// test it names must exist somewhere in the repository, and every relative link must resolve.

// claudeMdTestName matches a test named in CLAUDE.md, with an optional trailing * for a family
// (TestGroupScopeIsolation*).
var claudeMdTestName = regexp.MustCompile(`\b(Test[A-Z][A-Za-z0-9_]*)(\*?)`)

// claudeMdLink matches a relative markdown link target.
var claudeMdLink = regexp.MustCompile(`\]\(([^)#:]+)(#[^)]*)?\)`)

func TestClaudeMdNamesRealTests(t *testing.T) {
	root := filepath.Join("..", "..")

	source, err := os.ReadFile(filepath.Join(root, "CLAUDE.md"))
	if err != nil {
		t.Fatalf("reading CLAUDE.md: %s", err)
	}

	defined := definedTestNames(t, root)

	for _, match := range claudeMdTestName.FindAllStringSubmatch(string(source), -1) {
		name, family := match[1], match[2] == "*"

		if name == "TestName" {
			continue // the placeholder in "go test ./hippocampus -run TestName"
		}

		found := false

		for test := range defined {
			if test == name || (family && strings.HasPrefix(test, name)) {
				found = true

				break
			}
		}

		if !found {
			t.Errorf("CLAUDE.md names %s, which no test in the repository defines - rename it to the test that now enforces the invariant", match[0])
		}
	}
}

func TestClaudeMdLinksResolve(t *testing.T) {
	root := filepath.Join("..", "..")

	source, err := os.ReadFile(filepath.Join(root, "CLAUDE.md"))
	if err != nil {
		t.Fatalf("reading CLAUDE.md: %s", err)
	}

	for _, match := range claudeMdLink.FindAllStringSubmatch(string(source), -1) {
		if _, err := os.Stat(filepath.Join(root, match[1])); err != nil {
			t.Errorf("CLAUDE.md links to %s, which does not exist", match[1])
		}
	}
}

// definedTestNames is every test function declared in any _test.go file in the repository, the
// integration modules included.
func definedTestNames(t *testing.T, root string) map[string]bool {
	t.Helper()

	declaration := regexp.MustCompile(`(?m)^func (Test[A-Za-z0-9_]*)\(`)
	names := map[string]bool{}

	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if entry.IsDir() {
			if path != root && (strings.HasPrefix(entry.Name(), ".") || entry.Name() == "node_modules") {
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

		for _, match := range declaration.FindAllStringSubmatch(string(source), -1) {
			names[match[1]] = true
		}

		return nil
	})
	if err != nil {
		t.Fatalf("walking the repository: %s", err)
	}

	return names
}
