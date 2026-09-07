package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/viper"
)

// TestPrintCheckConfig covers the three shapes the text rendering has to get right: a clean pass, a
// failure listing every problem, and a run against no file at all - the last because a check that
// says "valid" while silently examining the built-in defaults would reassure an operator about a
// file it never opened.
func TestPrintCheckConfig(t *testing.T) {
	for _, tt := range []struct {
		name    string
		report  checkConfigReport
		want    []string
		notWant []string
	}{
		{
			name:    "valid",
			report:  checkConfigReport{Status: checkConfigStatusValid, ConfigFile: "/etc/hippocampus/config.json", Driver: "postgres"},
			want:    []string{"config:  /etc/hippocampus/config.json", "driver:  postgres", "status:  valid"},
			notWant: []string{"INVALID", "built-in defaults"},
		},
		{
			name: "invalid lists every problem",
			report: checkConfigReport{
				Status:     checkConfigStatusInvalid,
				ConfigFile: "config.json",
				Driver:     "sqlite",
				Problems:   []string{"first problem", "second problem", "third problem"},
			},
			want: []string{
				"status:  INVALID - 3 problem(s)",
				"would refuse to start",
				"- first problem",
				"- second problem",
				"- third problem",
			},
		},
		{
			name:   "defaulted names the file it did not find",
			report: checkConfigReport{Status: checkConfigStatusValid, ConfigFile: "./config.json", Defaulted: true, Driver: "sqlite"},
			want:   []string{"none at './config.json'", "built-in defaults"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer

			printCheckConfig(&out, tt.report)

			for _, want := range tt.want {
				if !strings.Contains(out.String(), want) {
					t.Errorf("output does not contain %q:\n%s", want, out.String())
				}
			}

			for _, notWant := range tt.notWant {
				if strings.Contains(out.String(), notWant) {
					t.Errorf("output unexpectedly contains %q:\n%s", notWant, out.String())
				}
			}
		})
	}
}

// TestPrintCheckConfigJSONProblemsIsNeverNull pins the one thing about the JSON that a consumer
// would otherwise have to special-case: on the passing path the field is an empty array, not null,
// so `.problems | length` works without a guard on every rendering this command produces.
func TestPrintCheckConfigJSONProblemsIsNeverNull(t *testing.T) {
	var out bytes.Buffer

	report := checkConfigReport{Status: checkConfigStatusValid, ConfigFile: "config.json", Driver: "sqlite", Problems: problemStrings(nil)}

	if err := printCheckConfigJSON(&out, report); err != nil {
		t.Fatalf("printCheckConfigJSON: %s", err)
	}

	if !strings.Contains(out.String(), `"problems": []`) {
		t.Errorf("problems must render as an empty array, not null:\n%s", out.String())
	}

	var decoded checkConfigReport

	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatalf("rendering is not parseable: %s", err)
	}

	if decoded.Status != checkConfigStatusValid {
		t.Errorf("status = %q, want %q", decoded.Status, checkConfigStatusValid)
	}
}

// TestExecute_CheckConfig drives the flag wiring: --check-config validates and returns without
// starting the server, and --output selects the rendering rather than being silently ignored.
func TestExecute_CheckConfig(t *testing.T) {
	config := writeConfigFile(t, `{"storage": {"driver": "sqlite", "directory": "/var/lib/hippocampus"}}`)

	for _, tt := range []struct {
		name string
		args []string
		want string
	}{
		{name: "text is the default", args: []string{"--check-config", "-c", config}, want: "status:  valid"},
		{name: "json", args: []string{"--check-config", "--output", "json", "-c", config}, want: `"status": "valid"`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			viper.Reset()
			defer viper.Reset()
			defer log.StandardLogger().SetOutput(log.StandardLogger().Out)

			out := captureStdout(t, func() {
				execute(tt.args)
			})

			if !strings.Contains(out, tt.want) {
				t.Errorf("execute(%v) output does not contain %q:\n%s", tt.args, tt.want, out)
			}
		})
	}
}

// TestExecute_CheckConfigReportsEveryProblem is the decision this mode turns on. validateConfig used
// to return on the first failure, which is fine for a server that is about to exit and useless for a
// pre-flight: an operator would fix one thing, re-run, and find the next - once per mistake. A
// configuration with four independent faults must name four.
func TestExecute_CheckConfigReportsEveryProblem(t *testing.T) {
	viper.Reset()
	defer viper.Reset()
	defer log.StandardLogger().SetOutput(log.StandardLogger().Out)

	config := writeConfigFile(t, `{
		"consolidation": {
			"unitsOfAgeInDays": 0,
			"method": 9,
			"deletionThreshold": 10,
			"aggressiveness": 1,
			"minimumRetentionInDays": -3
		},
		"storage": {"driver": "sqlite", "directory": ""}
	}`)

	out := captureStdout(t, func() {
		withFatalPanic(t, func() {
			execute([]string{"--check-config", "--output", "json", "-c", config})
		})
	})

	var decoded checkConfigReport

	if err := json.Unmarshal([]byte(out), &decoded); err != nil {
		t.Fatalf("stdout is not parseable JSON: %s\n%s", err, out)
	}

	if decoded.Status != checkConfigStatusInvalid {
		t.Fatalf("status = %q, want %q", decoded.Status, checkConfigStatusInvalid)
	}

	for _, want := range []string{
		"consolidation.unitsOfAgeInDays",
		"consolidation.method",
		"consolidation.minimumRetentionInDays",
		"storage.directory",
	} {
		found := false

		for _, problem := range decoded.Problems {
			if strings.Contains(problem, want) {
				found = true

				break
			}
		}

		if !found {
			t.Errorf("no problem reported for %s; got %v", want, decoded.Problems)
		}
	}
}

// TestConfigProblems_TwoObjectStores pins the one rule --check-config gained with the filesystem
// archive backend. Only one object store can back Export/Import, and a configuration naming both is
// one whose author believes something the service does not do - so it is refused rather than
// resolved by precedence, which would leave the two disagreeing about where the archives went until
// somebody needed one.
func TestConfigProblems_TwoObjectStores(t *testing.T) {
	viper.Reset()
	defer viper.Reset()

	setStartupDefaults()
	viper.Set("storage.directory", t.TempDir())

	if problems := configProblems(); len(problems) != 0 {
		t.Fatalf("expected the defaults alone to be valid, got %v", problems)
	}

	viper.Set("archive.directory", t.TempDir())

	if problems := configProblems(); len(problems) != 0 {
		t.Fatalf("expected archive.directory alone to be valid, got %v", problems)
	}

	viper.Set("s3.bucket", "an-archive-bucket")

	problems := configProblems()
	if len(problems) != 1 {
		t.Fatalf("expected exactly one problem for two object stores, got %v", problems)
	}

	for _, want := range []string{"s3.bucket", "archive.directory"} {
		if !strings.Contains(problems[0].Error(), want) {
			t.Errorf("the problem does not name %s: %s", want, problems[0].Error())
		}
	}

	viper.Set("archive.directory", "")

	if problems := configProblems(); len(problems) != 0 {
		t.Fatalf("expected s3.bucket alone to be valid, got %v", problems)
	}
}

// TestExecute_CheckConfigJSONStaysParseableWithNoConfigFile is why the dispatch sits ABOVE the
// "no configuration file" warning rather than below it, where every other CLI mode's went. That
// warning is logged at Warn, this binary logs to stdout, and --output json makes stdout a data
// channel - so the ordering that reads as harmless is the difference between a document a deploy
// script can parse and one it cannot. The failing arrangement is invisible with a config file
// present, which is every other test here.
func TestExecute_CheckConfigJSONStaysParseableWithNoConfigFile(t *testing.T) {
	viper.Reset()
	defer viper.Reset()
	defer log.StandardLogger().SetOutput(log.StandardLogger().Out)

	// No -c, and the package directory holds no config.json, so this takes the configMissing path.
	out := captureStdout(t, func() {
		execute([]string{"--check-config", "--output", "json"})
	})

	var decoded checkConfigReport

	if err := json.Unmarshal([]byte(out), &decoded); err != nil {
		t.Fatalf("stdout must be parseable JSON when no config file exists: %s\n%s", err, out)
	}

	if !decoded.Defaulted {
		t.Errorf("defaulted = false, want true when no config file was found")
	}

	// The built-in defaults are a valid configuration - that is what makes running without a file a
	// supported mode - so this must pass rather than merely parse.
	if decoded.Status != checkConfigStatusValid {
		t.Errorf("status = %q, want %q; the built-in defaults must validate", decoded.Status, checkConfigStatusValid)
	}
}

// TestExecute_CheckConfigInvalidExitsNonZero: the exit status is the whole point for a deploy
// pipeline, which should not have to parse anything to gate on the answer.
func TestExecute_CheckConfigInvalidExitsNonZero(t *testing.T) {
	viper.Reset()
	defer viper.Reset()
	defer log.StandardLogger().SetOutput(log.StandardLogger().Out)

	config := writeConfigFile(t, `{"consolidation": {"method": 42}, "storage": {"driver": "sqlite", "directory": "/var/lib/hippocampus"}}`)

	// withFatalPanic fails the test if no log.Fatalf is reached, which is the assertion: an invalid
	// configuration must not return a success exit.
	out := captureStdout(t, func() {
		withFatalPanic(t, func() {
			execute([]string{"--check-config", "-c", config})
		})
	})

	if !strings.Contains(out, "INVALID") {
		t.Errorf("the report should still be rendered before the fatal:\n%s", out)
	}
}

// TestExecute_CheckConfigRejectsAnUnknownFormat fails fast rather than falling back to text, which
// would hand a script that asked for JSON something it cannot parse and no indication why.
func TestExecute_CheckConfigRejectsAnUnknownFormat(t *testing.T) {
	viper.Reset()
	defer viper.Reset()
	defer log.StandardLogger().SetOutput(log.StandardLogger().Out)

	config := writeConfigFile(t, `{"storage": {"driver": "sqlite", "directory": "/var/lib/hippocampus"}}`)

	withFatalPanic(t, func() {
		execute([]string{"--check-config", "--output", "yaml", "-c", config})
	})
}
