package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/pflag"
	"github.com/spf13/viper"
)

// setupFlags parses args onto a fresh flag set and binds them onto the global viper the cmd reads,
// so serve/run see the test's configuration without touching the global pflag.CommandLine.
func setupFlags(t *testing.T, args []string) {
	t.Helper()

	viper.Reset()

	fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
	if err := registerFlags(fs, args); err != nil {
		t.Fatalf("registerFlags: %v", err)
	}
}

// writeRules writes a rules file and returns its path.
func writeRules(t *testing.T, doc string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "rules.json")

	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatalf("writing the rules file: %s", err)
	}

	return path
}

const goodRules = `{"defaultAction":"drop","rules":[{"name":"r","expr":"event.name == 'keep'","action":"promote"}]}`

func TestRegisterFlags_ParseError(t *testing.T) {
	fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
	fs.SetOutput(os.NewFile(0, os.DevNull))

	if err := registerFlags(fs, []string{"--not-a-flag"}); err == nil {
		t.Error("registerFlags should error on an unknown flag")
	}
}

func TestServe_Version(t *testing.T) {
	setupFlags(t, []string{"--version"})

	if err := serve(context.Background()); err != nil {
		t.Errorf("serve --version = %v, want nil", err)
	}
}

func TestServe_BadLogLevel(t *testing.T) {
	setupFlags(t, []string{"--log-level", "bogus", "--rules", "whatever.json"})

	if err := serve(context.Background()); err == nil {
		t.Error("serve should error on an invalid log level")
	}
}

func TestRun_MissingRules(t *testing.T) {
	setupFlags(t, []string{"--target-address", "localhost:50052"})

	err := run(context.Background())
	if err == nil {
		t.Fatal("run should require --rules")
	}

	if !strings.Contains(err.Error(), "--rules") {
		t.Errorf("expected the error to name the flag, got %q", err.Error())
	}
}

func TestRun_MissingTargetAddress(t *testing.T) {
	setupFlags(t, []string{"--rules", writeRules(t, goodRules)})

	err := run(context.Background())
	if err == nil {
		t.Fatal("run should require --target-address")
	}

	if !strings.Contains(err.Error(), "--target-address") {
		t.Errorf("expected the error to name the flag, got %q", err.Error())
	}
}

func TestRun_InvalidOrphanPolicy(t *testing.T) {
	setupFlags(t, []string{
		"--rules", writeRules(t, goodRules),
		"--target-address", "localhost:50052",
		"--orphans", "keep-forever",
	})

	err := run(context.Background())
	if err == nil {
		t.Fatal("run should reject an unknown orphan policy")
	}

	if !strings.Contains(err.Error(), "--orphans") {
		t.Errorf("expected the error to name the flag, got %q", err.Error())
	}
}

// TestRun_BrokenRulesFileFailsStartup is the fail-closed half: a named-but-broken rules file must
// stop the process rather than start an ingestor with no admission policy. It is also checked
// BEFORE either dial, so the message is about the rules rather than about a connection.
func TestRun_BrokenRulesFileFailsStartup(t *testing.T) {
	cases := []struct {
		name string
		doc  string
	}{
		{"malformed json", `{"defaultAction":`},
		{"uncompilable expression", `{"defaultAction":"drop","rules":[{"name":"r","expr":"event.nope","action":"promote"}]}`},
		{"missing default action", `{"rules":[]}`},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			setupFlags(t, []string{
				"--rules", writeRules(t, c.doc),
				"--target-address", "localhost:50052",
			})

			err := run(context.Background())
			if err == nil {
				t.Fatal("run should fail on a broken rules file")
			}

			if !strings.Contains(err.Error(), "loading rules") {
				t.Errorf("expected a rules-loading error, got %q", err.Error())
			}
		})
	}
}

func TestRun_MissingRulesFile(t *testing.T) {
	setupFlags(t, []string{
		"--rules", filepath.Join(t.TempDir(), "absent.json"),
		"--target-address", "localhost:50052",
	})

	if err := run(context.Background()); err == nil {
		t.Error("run should fail when the rules file does not exist")
	}
}

// TestRun_CheckRules covers the linter mode, which compiles the file and exits without dialling
// anything - hence no --target-address here.
func TestRun_CheckRules(t *testing.T) {
	setupFlags(t, []string{"--rules", writeRules(t, goodRules), "--check-rules"})

	if err := run(context.Background()); err != nil {
		t.Errorf("--check-rules on a valid file = %v, want nil", err)
	}

	setupFlags(t, []string{"--rules", writeRules(t, `{"defaultAction":"nope"}`), "--check-rules"})

	if err := run(context.Background()); err == nil {
		t.Error("--check-rules should report a broken file")
	}
}

// TestServe_ReachesRun drives serve past the version/log-level short-circuits into run, which fails
// on the missing rules path.
func TestServe_ReachesRun(t *testing.T) {
	setupFlags(t, []string{"--log-level", "debug"})

	if err := serve(context.Background()); err == nil {
		t.Error("serve should surface run's validation error")
	}
}

// TestRealMain covers the process shell. It runs exactly one case, because realMain registers onto
// the global pflag.CommandLine - a second call would redefine every flag on it, and a parse error
// there exits the test binary rather than returning (pflag.CommandLine is ExitOnError). The parse
// and validation failures are covered above against flag sets this package owns.
func TestRealMain(t *testing.T) {
	viper.Reset()

	if code := realMain([]string{"--version"}); code != 0 {
		t.Errorf("expected exit code 0 for --version, got %d", code)
	}
}

// TestEndpointConfig pins that the two endpoints read their own prefixed keys rather than sharing
// one set - a source token reaching the target would send the edge's credentials to the central
// instance.
func TestEndpointConfig(t *testing.T) {
	setupFlags(t, []string{
		"--source-address", "edge:50051",
		"--source-token", "edge-token",
		"--target-address", "central:50051",
		"--target-token", "central-token",
		"--target-tls",
		"--target-tls-ca-cert", "/ca.pem",
	})

	source := endpointConfig(sourceEndpoint)
	target := endpointConfig(targetEndpoint)

	if source.Address != "edge:50051" || source.Token != "edge-token" {
		t.Errorf("unexpected source config: %+v", source)
	}

	if source.TLS {
		t.Error("the source must not inherit the target's TLS setting")
	}

	if target.Address != "central:50051" || target.Token != "central-token" {
		t.Errorf("unexpected target config: %+v", target)
	}

	if !target.TLS || target.TLSCACertFile != "/ca.pem" {
		t.Errorf("unexpected target TLS config: %+v", target)
	}
}
