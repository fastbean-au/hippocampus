package main

import (
	"context"
	"os"
	"testing"

	"github.com/spf13/pflag"
	"github.com/spf13/viper"
)

func setupFlags(t *testing.T, args []string) {
	t.Helper()

	viper.Reset()

	fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
	if err := registerFlags(fs, args); err != nil {
		t.Fatalf("registerFlags: %v", err)
	}
}

func resetCommandLine() {
	viper.Reset()
	pflag.CommandLine = pflag.NewFlagSet("mqtt", pflag.ContinueOnError)
	pflag.CommandLine.SetOutput(os.NewFile(0, os.DevNull))
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
	setupFlags(t, []string{"--log-level", "bogus", "--topic", "t"})

	if err := serve(context.Background()); err == nil {
		t.Errorf("serve should error on an invalid log level")
	}
}

func TestServe_RunsThrough(t *testing.T) {
	setupFlags(t, []string{"--log-level", "debug", "--topic", "t", "--broker", "tcp://127.0.0.1:1"})

	if err := serve(context.Background()); err == nil {
		t.Errorf("serve should surface run's connect error")
	}
}

func TestRun_MissingTopic(t *testing.T) {
	setupFlags(t, []string{})

	if err := run(context.Background()); err == nil {
		t.Errorf("run should error when --topic is missing")
	}
}

func TestRun_ConnectFailsReturnsError(t *testing.T) {
	// A refused port makes paho's connect fail fast, exercising run's full happy path.
	setupFlags(t, []string{"--topic", "t", "--broker", "tcp://127.0.0.1:1"})

	if err := run(context.Background()); err == nil {
		t.Errorf("run should surface the MQTT connect error")
	}
}

func TestRun_DialError(t *testing.T) {
	setupFlags(t, []string{"--topic", "t", "--tls", "--tls-ca-cert", "/no/such/ca.pem"})

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

	if code := realMain([]string{"--log-level", "bogus", "--topic", "t"}); code != 1 {
		t.Errorf("realMain with a bad log level = %d, want 1", code)
	}
}
