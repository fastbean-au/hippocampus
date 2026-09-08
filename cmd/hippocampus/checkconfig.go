package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	log "github.com/sirupsen/logrus"
)

// checkConfig is the --check-config CLI mode: validate the resolved configuration and exit without
// opening the database or starting the server.
//
// It exists because a validation rule is only ever written against the configurations its author can
// see, and the deployments it breaks are the ones they cannot. Version 0.41.0 added a check refusing
// a non-positive consolidation.deletionThreshold; every configuration in this repository sets a
// positive one, so nothing here could have caught that two instances on the public demo were
// configured the other way and would no longer start (see TODO-2 item 88). Startup already ran this
// validation - what was missing was any way to run it WITHOUT starting, so that the answer arrives
// in a deploy pipeline rather than from a crash loop.
//
// It is deliberately the whole resolved configuration and not the file: viper's precedence is flag >
// env > file > default, so a config.json that validates on its own can still be invalid once the
// HIPPOCAMPUS_* overrides a container injects are applied, and the file alone cannot answer that.
// This runs after the same read, the same overrides and the same defaults as a real start.
//
// Nothing it does touches the store, the network, or the identity provider, so it is safe to run
// anywhere - including beside a live instance, and in a build step that has neither.
//
// Like --schema-version it renders in two forms and carries its verdict in the exit status, so a
// script that only needs the gate need not parse anything.
func checkConfig(cfg checkConfigConfig) {
	// Diagnostics to stderr for the rest of this mode, because stdout is now a data channel: in JSON
	// mode a single log line on it makes the document unparseable, and initLogging has already
	// pointed logging at stdout by the time this runs.
	log.SetOutput(os.Stderr)

	report := checkConfigReport{
		ConfigFile: cfg.ConfigFile,
		Defaulted:  cfg.ConfigMissing,
		Driver:     cfg.Driver,
		Problems:   problemStrings(configProblems()),
		Deprecated: cfg.Deprecated,
	}

	if report.Deprecated == nil {
		report.Deprecated = []string{}
	}

	report.Status = checkConfigStatusValid
	if len(report.Problems) > 0 {
		report.Status = checkConfigStatusInvalid
	}

	if cfg.JSON {
		if err := printCheckConfigJSON(os.Stdout, report); err != nil {
			log.Fatalf("failed to render the configuration report: %s", err.Error())
		}
	} else {
		printCheckConfig(os.Stdout, report)
	}

	// Non-zero on a bad configuration, deliberately: this is what a deploy pipeline gates on, and a
	// configuration the service would refuse to start on must not read as success. It shares the
	// binary's one failure exit rather than inventing a code of its own.
	if report.Status == checkConfigStatusInvalid {
		log.Fatalf("configuration is not valid: %d problem(s) found", len(report.Problems))
	}
}

// checkConfigConfig is what the --check-config mode needs from the startup path. The problems
// themselves come from viper via configProblems; these are the few facts about WHICH configuration
// was checked, which the report states so that a passing run against the wrong file is visible
// rather than reassuring.
type checkConfigConfig struct {
	ConfigFile    string
	ConfigMissing bool
	Driver        string

	// Deprecated names the legacy configuration keys still in use, which the startup path resolved
	// onto their replacements. They are reported separately from Problems and do not affect the
	// exit status: the configuration is valid, and staying silent about it here would leave the
	// pre-flight tool unable to answer the one question a deprecation raises - whether this
	// deployment is affected - until the release that stops honouring them.
	Deprecated []string

	// JSON selects the machine-readable rendering.
	JSON bool
}

// Check statuses. They are the field a script branches on, so they are a small closed set and their
// spellings are part of the output's contract.
const (
	checkConfigStatusValid   = "valid"
	checkConfigStatusInvalid = "invalid"
)

// checkConfigReport is the JSON shape and the text rendering's input.
//
// It is a projection rather than tags on anything internal, on the rule the --schema-version mode
// and the MCP bridge both follow: the wire shape is this command's contract with whatever parses it,
// and it should not change because a field was renamed somewhere behind it.
type checkConfigReport struct {
	Status     string   `json:"status"`
	ConfigFile string   `json:"config_file"`
	Defaulted  bool     `json:"defaulted"`
	Driver     string   `json:"driver"`
	Problems   []string `json:"problems"`
	Deprecated []string `json:"deprecated"`
}

// problemStrings renders the problems for the report. Always a non-nil slice, so the JSON carries
// `"problems": []` rather than `null` on the passing path - a consumer indexing into it should not
// have to special-case success.
func problemStrings(problems []error) []string {
	out := make([]string, 0, len(problems))

	for _, problem := range problems {
		out = append(out, problem.Error())
	}

	return out
}

// printCheckConfig writes the report to w. Split from the mode above so a test can read it without
// capturing the process's stdout.
func printCheckConfig(w io.Writer, report checkConfigReport) {
	if report.Defaulted {
		// Worth saying plainly: a check that passes against built-in defaults tells an operator
		// nothing about the file they thought they were checking.
		_, _ = fmt.Fprintf(w, "config:  none at '%s' - checking the built-in defaults\n", report.ConfigFile)
	} else {
		_, _ = fmt.Fprintf(w, "config:  %s\n", report.ConfigFile)
	}

	_, _ = fmt.Fprintf(w, "driver:  %s\n", report.Driver)

	for _, key := range report.Deprecated {
		_, _ = fmt.Fprintf(w, "warning: '%s' is deprecated and was read into its replacement\n", key)
	}

	if report.Status == checkConfigStatusValid {
		_, _ = fmt.Fprintf(w, "status:  valid\n")

		return
	}

	_, _ = fmt.Fprintf(w, "status:  INVALID - %d problem(s); the service would refuse to start\n\n", len(report.Problems))

	for _, problem := range report.Problems {
		_, _ = fmt.Fprintf(w, "  - %s\n", problem)
	}
}

// printCheckConfigJSON writes the machine-readable rendering.
func printCheckConfigJSON(w io.Writer, report checkConfigReport) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")

	return encoder.Encode(report)
}
