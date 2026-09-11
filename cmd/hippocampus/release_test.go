package main

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"

	yaml "go.yaml.in/yaml/v3"
)

// RELEASE.md describes what the release workflow does, job by job, and nothing executes that
// description. It is the same shape as the alert rules in alerts_test.go - a list copied into prose
// - and it drifted the same way: by 2026-09-12 the workflow had ten jobs and the document described
// five, with three of the missing ones (publish-llamaindex, publish-ingestor,
// publish-objectstore-agents) absent outright. The failure is quiet in the direction that matters,
// because a publish job nobody documents is a published artefact nobody verifies after a release.
//
// These tests hold the two together. They live here for the reason alerts_test.go does: the guard
// needs to read files across the repository rather than import anything, and this is where that
// precedent already sits.

const (
	releaseWorkflowPath = "../../.github/workflows/release.yaml"
	releaseDocPath      = "../../RELEASE.md"

	// releaseDocSection is the heading whose body must name every job. Scoping to it keeps the
	// guard away from the pre-flight above, which legitimately discusses CI jobs from another
	// workflow.
	releaseDocSection = "## What the workflow does"
)

// releaseJobMention matches how the document names a job: a backticked identifier followed by the
// word "job". Requiring that one form is what lets the reverse direction be checked at all - a
// bare backticked token is indistinguishable from an image name, a package or a matrix value, of
// which that section carries plenty.
var releaseJobMention = regexp.MustCompile("`([a-z0-9][a-z0-9-]*)`\\s+job\\b")

// releaseWorkflow is the part of the workflow this guard reads.
type releaseWorkflow struct {
	Jobs map[string]struct {
		Strategy struct {
			Matrix map[string][]string `yaml:"matrix"`
		} `yaml:"strategy"`
	} `yaml:"jobs"`
}

func loadReleaseWorkflow(t *testing.T) releaseWorkflow {
	t.Helper()

	raw, err := os.ReadFile(releaseWorkflowPath)
	if err != nil {
		t.Fatalf("reading %s: %v", releaseWorkflowPath, err)
	}

	var workflow releaseWorkflow
	if err := yaml.Unmarshal(raw, &workflow); err != nil {
		t.Fatalf("parsing %s: %v", releaseWorkflowPath, err)
	}

	if len(workflow.Jobs) == 0 {
		t.Fatalf("%s declares no jobs; the guard would pass vacuously", releaseWorkflowPath)
	}

	return workflow
}

// releaseDocBody returns the body of the section that must describe the jobs.
func releaseDocBody(t *testing.T) string {
	t.Helper()

	raw, err := os.ReadFile(releaseDocPath)
	if err != nil {
		t.Fatalf("reading %s: %v", releaseDocPath, err)
	}

	body := string(raw)

	start := strings.Index(body, releaseDocSection)
	if start < 0 {
		t.Fatalf("%s has no %q heading", releaseDocPath, releaseDocSection)
	}

	body = body[start+len(releaseDocSection):]

	if end := strings.Index(body, "\n## "); end >= 0 {
		body = body[:end]
	}

	return body
}

// documentedReleaseJobs are the jobs the document claims exist.
func documentedReleaseJobs(body string) map[string]bool {
	out := map[string]bool{}

	for _, match := range releaseJobMention.FindAllStringSubmatch(body, -1) {
		out[match[1]] = true
	}

	return out
}

// TestReleaseDocumentsEveryWorkflowJob checks both directions, because the two failures differ in
// kind. A job the document omits is an artefact that ships unannounced and unverified; a job the
// document names and the workflow no longer defines sends a reader looking for something that was
// renamed or removed. Only the first has actually happened here, which is precisely why the second
// is worth asserting rather than assuming.
func TestReleaseDocumentsEveryWorkflowJob(t *testing.T) {
	workflow := loadReleaseWorkflow(t)
	documented := documentedReleaseJobs(releaseDocBody(t))

	for name := range workflow.Jobs {
		if !documented[name] {
			t.Errorf(
				"%s defines job %q, which %s does not describe under %q\n"+
					"(name it as `%s` job, which is the form the guard reads)",
				releaseWorkflowPath, name, releaseDocPath, releaseDocSection, name,
			)
		}
	}

	for _, name := range sortedKeys(documented) {
		if _, ok := workflow.Jobs[name]; !ok {
			t.Errorf(
				"%s describes a %q job, which %s does not define",
				releaseDocPath, name, releaseWorkflowPath,
			)
		}
	}
}

// TestReleaseDocumentsEveryMatrixValue covers the other half of the drift this guard was written
// for. Two jobs publish one artefact per matrix entry, and the document has to enumerate them
// because "the bridge images" tells a reader nothing about which brokers are covered. It had
// already gone wrong in the cheapest possible way: five brokers described as four.
func TestReleaseDocumentsEveryMatrixValue(t *testing.T) {
	workflow := loadReleaseWorkflow(t)
	body := releaseDocBody(t)

	for name, job := range workflow.Jobs {
		for dimension, values := range job.Strategy.Matrix {
			for _, v := range values {
				if strings.Contains(body, fmt.Sprintf("`%s`", v)) {
					continue
				}

				t.Errorf(
					"job %q publishes %s=%q, which %s never names under %q",
					name, dimension, v, releaseDocPath, releaseDocSection,
				)
			}
		}
	}
}

// TestReleaseJobCountIsCurrent holds the one number the section states in prose. Counting the jobs
// in a sentence is the habit this whole guard exists to protect, so the sentence gets checked too
// rather than being trusted because it was correct when written - the same reason
// TestClaudeMdRuleCountIsCurrent exists.
func TestReleaseJobCountIsCurrent(t *testing.T) {
	workflow := loadReleaseWorkflow(t)
	body := releaseDocBody(t)

	numbers := map[int]string{
		8: "eight", 9: "nine", 10: "ten", 11: "eleven", 12: "twelve",
		13: "thirteen", 14: "fourteen", 15: "fifteen",
	}

	want, ok := numbers[len(workflow.Jobs)]
	if !ok {
		t.Fatalf(
			"%s has %d jobs, which this guard has no word for; extend the table",
			releaseWorkflowPath, len(workflow.Jobs),
		)
	}

	claim := fmt.Sprintf("runs **%s** jobs", want)
	if !strings.Contains(body, claim) {
		t.Errorf(
			"%s has %d jobs, so %s should say %q under %q",
			releaseWorkflowPath, len(workflow.Jobs), releaseDocPath, claim, releaseDocSection,
		)
	}
}
