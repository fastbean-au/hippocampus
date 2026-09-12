package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The five modules under integrations/ each require github.com/fastbean-au/hippocampus and each
// replace it with the repository root. The version in that require is therefore never used: inside
// this repository the replace wins, and outside it nobody can reach these modules at all - Go
// resolves a nested module from a `<dir>/vX.Y.Z` tag and only `vX.Y.Z` tags exist here, so
// `go get github.com/fastbean-au/hippocampus/integrations/eventsource@v0.47.0` fails with "unknown
// revision integrations/eventsource/v0.47.0".
//
// Inert, and it did not look inert. Four modules said v0.39.0 and one said v0.46.0 - a skew that
// nothing detected because nothing reads the field, in a repository whose .github/dependabot.yml
// exists specifically to stop these five drifting apart. Worse, a reader has no way to tell that the
// number is a leftover rather than a compatibility floor, and v0.39.0's contract genuinely lacks
// StoreMemories, which integrations/eventsource names in its client seam. So the number was both
// meaningless and wrong, in a way that reads as meaningful.
//
// v0.0.0 is the fix, and the point of it is that it cannot be mistaken for a claim. This guard keeps
// it that way, and keeps the five in step, which is the property the version field was accidentally
// pretending to have.
//
// THE ALTERNATIVE, deliberately not taken: publish `integrations/<name>/vX.Y.Z` tags so the versions
// become real and external consumers become possible. That is five more tag namespaces in a
// repository where two were enough of a problem to move three subprojects out over (TODO-2 item 113),
// and nothing has asked to import these modules as libraries. If something ever does, this guard is
// where the decision should be revisited - the versions would then have to be bumped on every
// release, and the sentence above about "nobody can reach these modules" is the thing that would
// have stopped being true.
const replacedRootVersion = "v0.0.0"

const rootModulePath = "github.com/fastbean-au/hippocampus"

var (
	// A require line for the root module, whether bare or inside a require block.
	rootRequirePattern = regexp.MustCompile(`^\s*` + regexp.QuoteMeta(rootModulePath) + `\s+(\S+)\s*$`)

	// A replace of the root module onto a relative path.
	rootReplacePattern = regexp.MustCompile(`^replace\s+` + regexp.QuoteMeta(rootModulePath) + `\s+=>\s+(\S+)\s*$`)
)

// TestReplacedRootRequirementIsInert requires every module that replaces the root module to record
// the version as v0.0.0. See the comment above for why a real-looking version there is worse than an
// obviously fake one.
func TestReplacedRootRequirementIsInert(t *testing.T) {
	for _, dir := range goModuleDirs(t) {
		if dir == "/" {
			continue
		}

		path := filepath.Join(repoRoot, strings.TrimPrefix(dir, "/"), "go.mod")

		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}

		required, replaced := rootRootDirective(string(body))

		if replaced == "" {
			t.Errorf("%s requires %s without replacing it, so it would resolve from the module "+
				"proxy - which cannot serve this module's own path either; add the replace, or "+
				"read the comment in %s and revisit the decision",
				dir, rootModulePath, "cmd/hippocampus/gomodule_test.go")

			continue
		}

		if required == "" {
			t.Errorf("%s replaces %s but does not require it", dir, rootModulePath)

			continue
		}

		if required != replacedRootVersion {
			t.Errorf("%s requires %s %s; it is replaced with %s, so the version is never used and "+
				"must be %s - a real-looking version there reads as a compatibility floor and is not one",
				dir, rootModulePath, required, replaced, replacedRootVersion)
		}
	}
}

// rootRootDirective returns the version the root module is required at and the path it is replaced
// with, either empty when absent.
func rootRootDirective(body string) (required string, replaced string) {
	for _, line := range strings.Split(body, "\n") {
		if m := rootReplacePattern.FindStringSubmatch(line); m != nil {
			replaced = m[1]

			continue
		}

		// Checked after the replace, since a replace line also contains the module path.
		if m := rootRequirePattern.FindStringSubmatch(strings.TrimSuffix(line, " // indirect")); m != nil {
			required = m[1]
		}
	}

	return required, replaced
}
