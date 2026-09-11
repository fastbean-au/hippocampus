package main

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/spf13/pflag"
	"github.com/spf13/viper"
)

// setupFlags parses args onto a fresh flag set and binds them onto the global viper the command
// reads, so serve and run see the test's configuration without touching pflag.CommandLine.
func setupFlags(t *testing.T, args []string) {
	t.Helper()

	viper.Reset()

	fs := pflag.NewFlagSet("test", pflag.ContinueOnError)

	if err := registerFlags(fs, args); err != nil {
		t.Fatalf("registerFlags failed: %s", err.Error())
	}
}

func TestRegisterFlagsRefusesAnUnknownFlag(t *testing.T) {
	fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
	fs.SetOutput(os.NewFile(0, os.DevNull))

	if err := registerFlags(fs, []string{"--not-a-flag"}); err == nil {
		t.Error("expected an unknown flag to be refused")
	}
}

func TestVersionPrintsAndExits(t *testing.T) {
	setupFlags(t, []string{"--version"})

	if err := serve(context.Background()); err != nil {
		t.Errorf("expected --version to succeed, got %s", err.Error())
	}
}

func TestABadLogLevelIsRefused(t *testing.T) {
	setupFlags(t, []string{"--log-level", "shouting"})

	if err := serve(context.Background()); err == nil {
		t.Error("expected an invalid log level to be refused")
	}
}

func TestTheBucketIsRequired(t *testing.T) {
	setupFlags(t, []string{"--allow-anonymous"})

	err := run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "--bucket") {
		t.Errorf("expected the bucket to be required, got %v", err)
	}
}

func TestAnUnknownModeIsRefused(t *testing.T) {
	setupFlags(t, []string{"--bucket", "payloads", "--allow-anonymous", "--mode", "sideways"})

	err := run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "--mode") {
		t.Errorf("expected an invalid mode to be refused, got %v", err)
	}
}

// A presigned URL grants a read of an object to whoever holds it, so an unauthenticated gateway is
// a public read endpoint for the whole bucket. That may be wanted; it must not be reachable by
// leaving a flag unset.
func TestAnonymousServingMustBeAskedFor(t *testing.T) {
	setupFlags(t, []string{"--bucket", "payloads"})

	err := run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "--auth-token") {
		t.Errorf("expected anonymous serving to be refused unless asked for, got %v", err)
	}

	setupFlags(t, []string{"--bucket", "payloads", "--auth-token", "sekrit", "--mode", "sideways"})

	// With a token configured the mode check is reached, which is what proves the auth check passed.
	if err := run(context.Background()); err == nil || !strings.Contains(err.Error(), "--mode") {
		t.Errorf("expected a configured token to satisfy the check, got %v", err)
	}
}

func TestTheEnvironmentOverridesFlags(t *testing.T) {
	t.Setenv("HIPPOCAMPUS_OBJECT_GATEWAY_BUCKET", "from-the-environment")

	setupFlags(t, []string{"--allow-anonymous"})

	if got := viper.GetString("bucket"); got != "from-the-environment" {
		t.Errorf("expected the environment to be read, got %q", got)
	}
}
