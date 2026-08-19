package main

import (
	"context"
	"os"
	"slices"
	"testing"

	"github.com/spf13/pflag"
	"github.com/spf13/viper"
)

// setupFlags parses args onto a fresh flag set and binds them onto the global viper the cmd reads,
// so serve/run see the test's configuration without touching the global pflag.CommandLine.
func setupFlags(t *testing.T, args []string) {
	t.Helper()

	viper.Reset()

	// --health-port 0 unless the case sets it. run() starts the probe server, several of these
	// tests call run(), and the flag's production default is a real fixed port - so every one of
	// them would bind :8090 on the machine running the tests. `go test ./...` runs each cmd package
	// in a SEPARATE PROCESS, in parallel, and all five bridges share that default, so the bind is a
	// race between packages rather than something the sequential order within one of them settles.
	// That is what failed in CI here, and it fails on whichever package loses.
	if !slices.Contains(args, "--health-port") {
		args = append(append([]string{}, args...), "--health-port", "0")
	}

	fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
	if err := registerFlags(fs, args); err != nil {
		t.Fatalf("registerFlags: %v", err)
	}
}

func TestRegisterFlags_ParseError(t *testing.T) {
	fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
	fs.SetOutput(os.NewFile(0, os.DevNull))

	if err := registerFlags(fs, []string{"--not-a-flag"}); err == nil {
		t.Errorf("registerFlags should error on an unknown flag")
	}
}

func TestServe_Version(t *testing.T) {
	setupFlags(t, []string{"--version"})

	if err := serve(context.Background()); err != nil {
		t.Errorf("serve --version = %v, want nil", err)
	}
}

func TestServe_BadLogLevel(t *testing.T) {
	setupFlags(t, []string{"--log-level", "bogus", "--subject", "s"})

	if err := serve(context.Background()); err == nil {
		t.Errorf("serve should error on an invalid log level")
	}
}

func TestServe_RunsThrough(t *testing.T) {
	// A valid log level drives serve past the version/level short-circuits into run, which fails
	// fast on the dead NATS port.
	setupFlags(t, []string{"--log-level", "debug", "--subject", "s", "--nats-url", "nats://127.0.0.1:1"})

	if err := serve(context.Background()); err == nil {
		t.Errorf("serve should surface run's connect error")
	}
}

func TestRun_MissingSubject(t *testing.T) {
	setupFlags(t, []string{})

	if err := run(context.Background()); err == nil {
		t.Errorf("run should error when --subject is missing")
	}
}

func TestRun_ConnectFailsReturnsError(t *testing.T) {
	// A dead NATS port: connect fails fast, so run exercises the full happy path (Dial, store,
	// adapter construction, b.Run) and returns the connect error.
	setupFlags(t, []string{"--subject", "events.>", "--nats-url", "nats://127.0.0.1:1"})

	if err := run(context.Background()); err == nil {
		t.Errorf("run should surface the NATS connect error")
	}
}

func TestRun_DialError(t *testing.T) {
	setupFlags(t, []string{"--subject", "s", "--tls", "--tls-ca-cert", "/no/such/ca.pem"})

	if err := run(context.Background()); err == nil {
		t.Errorf("run should surface the hippocampus dial error")
	}
}

func TestTransformConfig(t *testing.T) {
	setupFlags(t, []string{"--significance", "7", "--group", "g", "--binary", "--max-body-bytes", "10"})

	cfg := transformConfig()

	if cfg.Significance != 7 || cfg.Group != "g" || !cfg.Binary || cfg.MaxBodyBytes != 10 {
		t.Errorf("transformConfig = %+v, unexpected", cfg)
	}
}

func resetCommandLine() {
	viper.Reset()
	pflag.CommandLine = pflag.NewFlagSet("nats", pflag.ContinueOnError)
	pflag.CommandLine.SetOutput(os.NewFile(0, os.DevNull))
}

func TestRealMain_VersionReturnsZero(t *testing.T) {
	resetCommandLine()

	if code := realMain([]string{"--version"}); code != 0 {
		t.Errorf("realMain --version = %d, want 0", code)
	}
}

func TestRealMain_FlagErrorReturnsOne(t *testing.T) {
	resetCommandLine()

	if code := realMain([]string{"--not-a-flag"}); code != 1 {
		t.Errorf("realMain with a bad flag = %d, want 1", code)
	}
}

func TestRealMain_ServeErrorReturnsOne(t *testing.T) {
	resetCommandLine()

	if code := realMain([]string{"--log-level", "bogus", "--subject", "s"}); code != 1 {
		t.Errorf("realMain with a bad log level = %d, want 1", code)
	}
}
