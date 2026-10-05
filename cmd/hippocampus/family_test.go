package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The family's satellite list is written down three times: the `notify-satellites` job dispatches a
// release to it, scripts/family-status.py reports on it, and RELEASE.md describes it. Nothing
// executes any of the three together, which is the shape of drift alerts_test.go and
// release_test.go already guard against - and here the consequence is specific and quiet. A
// satellite added to the dispatch and not to the status script receives bumps that nobody ever
// notices are unreleased, which is precisely what happened to all three satellites between 0.48.0
// and 0.49.0, and what `hippocampus-gen` has done for longer than that.
//
// These guards therefore hold the dispatch list to the status script in BOTH directions, and
// require the process document to keep naming the script that answers the question.

const (
	familyStatusScriptPath = "../../scripts/family-status.py"

	// familyStatusScript is the path as RELEASE.md and CLAUDE.md name it.
	familyStatusScript = "scripts/family-status.py"

	// releaseDocAfterSection is where the release's own closing checks live. The satellite check
	// belongs there rather than in the pre-flight, because at pre-flight time the satellites have
	// not been told about the release yet.
	releaseDocAfterSection = "## After the release"
)

// dispatchLoop matches the shell line in notify-satellites that names the satellites. Reading the
// loop rather than a comment is the point: the loop is what actually runs.
var dispatchLoop = regexp.MustCompile(`(?m)^\s*for repo in ([^;]+); do\s*$`)

// familyTableRow matches one row of scripts/family-status.py's REPOS table, capturing the
// repository name and whether it is dispatched to.
var familyTableRow = regexp.MustCompile(`(?m)^\s*\((?:HUB|"([^"]+)"),.*,\s*(True|False)\),\s*$`)

// dispatchedSatellites are the repositories the release workflow sends its tag to.
func dispatchedSatellites(t *testing.T) map[string]bool {
	t.Helper()

	raw, err := os.ReadFile(releaseWorkflowPath)
	if err != nil {
		t.Fatalf("reading %s: %v", releaseWorkflowPath, err)
	}

	match := dispatchLoop.FindStringSubmatch(string(raw))
	if match == nil {
		t.Fatalf(
			"%s has no `for repo in ...; do` loop; the notify-satellites job was rewritten and this guard cannot read it",
			releaseWorkflowPath,
		)
	}

	out := map[string]bool{}

	for _, name := range strings.Fields(match[1]) {
		out[name] = true
	}

	if len(out) == 0 {
		t.Fatalf("%s dispatches to no satellites; the guard would pass vacuously", releaseWorkflowPath)
	}

	return out
}

// familyStatusRepos are the repositories the status script reports on, and which of them it
// believes are dispatched to.
func familyStatusRepos(t *testing.T) (all map[string]bool, dispatched map[string]bool) {
	t.Helper()

	raw, err := os.ReadFile(familyStatusScriptPath)
	if err != nil {
		t.Fatalf("reading %s: %v", familyStatusScriptPath, err)
	}

	all, dispatched = map[string]bool{}, map[string]bool{}

	for _, row := range familyTableRow.FindAllStringSubmatch(string(raw), -1) {
		// The hub's own row names itself through the HUB constant and captures nothing.
		if row[1] == "" {
			continue
		}

		all[row[1]] = true

		if row[2] == "True" {
			dispatched[row[1]] = true
		}
	}

	if len(all) == 0 {
		t.Fatalf("%s declares no repositories; the guard would pass vacuously", familyStatusScriptPath)
	}

	return all, dispatched
}

// TestFamilyStatusCoversEveryDispatchedSatellite checks both directions, because the two failures
// differ in kind. A satellite the script omits goes stale invisibly - it is told about every
// release and nothing ever reports that it has not published one. A satellite the script claims is
// dispatched to and the workflow does not tell is the reverse: the report would say its pin is
// merely unmerged, when in fact nothing is ever going to raise it.
func TestFamilyStatusCoversEveryDispatchedSatellite(t *testing.T) {
	dispatched := dispatchedSatellites(t)
	all, claimed := familyStatusRepos(t)

	for _, name := range sortedKeys(dispatched) {
		if !all[name] {
			t.Errorf(
				"%s dispatches releases to %q, which %s does not report on at all\n"+
					"(add it to REPOS, or it receives bumps nobody notices are unreleased)",
				releaseWorkflowPath, name, familyStatusScript,
			)

			continue
		}

		if !claimed[name] {
			t.Errorf(
				"%s dispatches releases to %q, which %s lists as not dispatched to",
				releaseWorkflowPath, name, familyStatusScript,
			)
		}
	}

	for _, name := range sortedKeys(claimed) {
		if !dispatched[name] {
			t.Errorf(
				"%s says %q is dispatched to, but %s's notify-satellites job does not tell it",
				familyStatusScript, name, releaseWorkflowPath,
			)
		}
	}
}

// TestReleaseDocNamesTheFamilyStatusCheck holds the one pointer that makes the rest of this
// reachable. The script answers "is anything left to release", and the moment to ask it is after a
// release - so if the document stops naming it, the check is only ever run by whoever already
// remembers it exists, which is the state this whole change replaced.
func TestReleaseDocNamesTheFamilyStatusCheck(t *testing.T) {
	raw, err := os.ReadFile(releaseDocPath)
	if err != nil {
		t.Fatalf("reading %s: %v", releaseDocPath, err)
	}

	body := string(raw)

	start := strings.Index(body, releaseDocAfterSection)
	if start < 0 {
		t.Fatalf("%s has no %q heading", releaseDocPath, releaseDocAfterSection)
	}

	body = body[start:]

	if end := strings.Index(body, "\n## "); end >= 0 {
		body = body[:end]
	}

	if !strings.Contains(body, familyStatusScript) {
		t.Errorf(
			"%s's %q section does not name %s, so nothing tells a release to check whether the satellites still owe one",
			releaseDocPath, releaseDocAfterSection, familyStatusScript,
		)
	}
}

// TestEverySatelliteNamedInTheWorkflowIsDescribed keeps the workflow's list and the prose beside it
// honest about each other. The job's paragraph is where a reader learns what each satellite re-pins,
// and a satellite added to the loop without a sentence there is one whose failure mode nobody has
// written down.
func TestEverySatelliteNamedInTheWorkflowIsDescribed(t *testing.T) {
	body := releaseDocBody(t)

	for _, name := range sortedKeys(dispatchedSatellites(t)) {
		if !strings.Contains(body, name) {
			t.Errorf(
				"%s dispatches to %q, which %s never names under %q",
				releaseWorkflowPath, name, releaseDocPath, releaseDocSection,
			)
		}
	}
}

// familySurfaceBlock matches scripts/family-status.py's SURFACE table, and familySurfaceRow one
// entry of it: the repository name and its tuple of path prefixes.
var (
	familySurfaceBlock = regexp.MustCompile(`(?ms)^SURFACE = \{\n(.*?)^\}`)
	familySurfaceRow   = regexp.MustCompile(`(?m)^\s*"([^"]+)":\s*\(([^)]*)\),\s*$`)
	quotedString       = regexp.MustCompile(`"([^"]+)"`)
)

// familySurfaces are the path prefixes each satellite consumes, as the status script declares them.
func familySurfaces(t *testing.T) map[string][]string {
	t.Helper()

	raw, err := os.ReadFile(familyStatusScriptPath)
	if err != nil {
		t.Fatalf("reading %s: %v", familyStatusScriptPath, err)
	}

	block := familySurfaceBlock.FindStringSubmatch(string(raw))
	if block == nil {
		t.Fatalf("%s has no `SURFACE = {` table; this guard cannot read it", familyStatusScriptPath)
	}

	out := map[string][]string{}

	for _, row := range familySurfaceRow.FindAllStringSubmatch(block[1], -1) {
		for _, prefix := range quotedString.FindAllStringSubmatch(row[2], -1) {
			out[row[1]] = append(out[row[1]], prefix[1])
		}
	}

	if len(out) == 0 {
		t.Fatalf("%s's SURFACE table declares nothing; the guard would pass vacuously", familyStatusScriptPath)
	}

	return out
}

// TestEveryDispatchedSatelliteDeclaresWhatItConsumes holds the table that decides whether a release
// is dispatched at all. The two failures differ in direction: a satellite with no entry is told
// about every release, which is merely noisy, but a prefix naming no path in this repository
// matches no change ever again, so that satellite is silently never told - which is why the second
// check is the one that matters.
func TestEveryDispatchedSatelliteDeclaresWhatItConsumes(t *testing.T) {
	surfaces := familySurfaces(t)

	for _, name := range sortedKeys(dispatchedSatellites(t)) {
		if len(surfaces[name]) == 0 {
			t.Errorf(
				"%s dispatches to %q, which %s's SURFACE table does not describe\n"+
					"(declare the paths it builds against, so a release changing none of them is not dispatched)",
				releaseWorkflowPath, name, familyStatusScript,
			)
		}
	}

	for name, prefixes := range surfaces {
		for _, prefix := range prefixes {
			if _, err := os.Stat("../../" + strings.TrimSuffix(prefix, "/")); err != nil {
				t.Errorf(
					"%s's SURFACE says %q consumes %q, which does not exist in this repository - "+
						"no change would ever match it, so that satellite would never be told about a release",
					familyStatusScript, name, prefix,
				)
			}
		}
	}
}
