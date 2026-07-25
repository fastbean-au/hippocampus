package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/viper"
)

// writeConfigFile writes cfg (a JSON document) to a temp file and returns its path, so an execute()
// test can point --config_file at a known configuration without a fixture on disk.
func writeConfigFile(t *testing.T, cfg string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatalf("write config: %s", err)
	}

	return path
}

// captureStdout redirects os.Stdout for the duration of fn and returns what was written, so the
// --version banner can be asserted rather than leaking to the test log.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()

	orig := os.Stdout

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %s", err)
	}

	os.Stdout = w
	fn()

	_ = w.Close()
	os.Stdout = orig

	out, _ := io.ReadAll(r)

	return string(out)
}

// expectPanic runs fn and fails unless it panics - the observable effect of a log.Panicf startup
// failure once ExitFunc-based log.Fatal is not involved.
func expectPanic(t *testing.T, fn func()) {
	t.Helper()

	defer func() {
		if recover() == nil {
			t.Fatal("expected a panic (log.Panicf), got none")
		}
	}()

	fn()
}

// TestExecute_Version prints the build banner and returns before reading any config file.
func TestExecute_Version(t *testing.T) {
	viper.Reset()
	defer viper.Reset()

	out := captureStdout(t, func() {
		execute([]string{"--version"})
	})

	if strings.TrimSpace(out) == "" {
		t.Error("execute(--version) printed nothing")
	}
}

// TestExecute_Help returns cleanly on pflag's ErrHelp rather than treating --help as a fatal parse
// error.
func TestExecute_Help(t *testing.T) {
	viper.Reset()
	defer viper.Reset()

	// --help writes usage to the flag set's output (stdout); capture it so it does not leak.
	_ = captureStdout(t, func() {
		execute([]string{"--help"})
	})
}

// TestExecute_BadFlag fails fast (log.Panicf) on an unparseable flag instead of starting with
// defaults.
func TestExecute_BadFlag(t *testing.T) {
	viper.Reset()
	defer viper.Reset()

	expectPanic(t, func() {
		execute([]string{"--not-a-real-flag"})
	})
}

// TestExecute_ConfigReadFailure panics when the config file cannot be read, rather than starting
// with an all-zero config.
func TestExecute_ConfigReadFailure(t *testing.T) {
	viper.Reset()
	defer viper.Reset()

	expectPanic(t, func() {
		execute([]string{"--config_file", filepath.Join(t.TempDir(), "does-not-exist.json")})
	})
}

// TestExecute_ConfigParseFailure panics when the config file is present but not valid JSON.
func TestExecute_ConfigParseFailure(t *testing.T) {
	viper.Reset()
	defer viper.Reset()

	path := writeConfigFile(t, "{not valid json")

	expectPanic(t, func() {
		execute([]string{"--config_file", path})
	})
}

// TestExecute_MintTokenHappyPath mints a token from the configured signing secret and returns,
// exercising the config read, env-override, logging, and mint-token dispatch.
func TestExecute_MintTokenHappyPath(t *testing.T) {
	viper.Reset()
	defer viper.Reset()

	// initLogging (reached on this path) repoints logrus at os.Stdout; restore it afterwards so a
	// later test's log output is unaffected.
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	path := writeConfigFile(t, `{"auth":{"signingSecret":"unit-test-signing-secret-value"}}`)

	out := captureStdout(t, func() {
		execute([]string{"--mint-token", "--client-id", "unit-test", "--config_file", path})
	})

	if strings.TrimSpace(out) == "" {
		t.Error("execute(--mint-token) printed no token")
	}
}

// TestExecute_MintTokenIdpRejected fails fast under auth.method 'idp': the identity provider issues
// tokens there, so a locally minted HS256 token would be useless.
func TestExecute_MintTokenIdpRejected(t *testing.T) {
	viper.Reset()
	defer viper.Reset()

	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	path := writeConfigFile(t, `{"auth":{"method":"idp"}}`)

	withFatalPanic(t, func() {
		execute([]string{"--mint-token", "--config_file", path})
	})
}

// TestExecute_BackfillRequiresOpenSearch fails fast when --backfill-search runs without
// opensearch.enabled, rather than silently doing nothing.
func TestExecute_BackfillRequiresOpenSearch(t *testing.T) {
	viper.Reset()
	defer viper.Reset()

	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	path := writeConfigFile(t, `{}`)

	withFatalPanic(t, func() {
		execute([]string{"--backfill-search", "--config_file", path})
	})
}

// TestExecute_InvalidConfig reaches validateConfig with an empty (all-default) config and fails
// fast, covering the read/env/logging/defaults span between the CLI modes and the server start.
func TestExecute_InvalidConfig(t *testing.T) {
	viper.Reset()
	defer viper.Reset()

	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	path := writeConfigFile(t, `{}`)

	withFatalPanic(t, func() {
		execute([]string{"--config_file", path})
	})
}
