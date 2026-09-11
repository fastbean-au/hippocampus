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
	setupFlags(t, nil)

	_, err := validate()
	if err == nil || !strings.Contains(err.Error(), "--bucket") {
		t.Errorf("expected the bucket to be required, got %v", err)
	}
}

func TestAnUnknownCauseIsRefused(t *testing.T) {
	setupFlags(t, []string{"--bucket", "payloads", "--causes", "consolidation,whenever"})

	_, err := validate()
	if err == nil || !strings.Contains(err.Error(), "--causes") {
		t.Errorf("expected an unknown cause to be refused, got %v", err)
	}
}

// A reaper with every path disabled starts, passes its probes and does nothing at all, which is the
// one failure an operator has no way to see.
func TestEveryPathDisabledIsRefused(t *testing.T) {
	setupFlags(t, []string{
		"--bucket", "payloads",
		"--listen-port", "0",
		"--catch-up", "0",
		"--sweep-interval", "0",
	})

	_, err := validate()
	if err == nil || !strings.Contains(err.Error(), "every path is disabled") {
		t.Errorf("expected a do-nothing configuration to be refused, got %v", err)
	}
}

// Each path on its own is a legitimate deployment, so none of them may be refused alone.
func TestASinglePathIsEnough(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{name: "the push path", args: []string{"--listen-port", "8089", "--catch-up", "0"}},
		{name: "the pull path", args: []string{"--listen-port", "0", "--catch-up", "1h"}},
		{name: "the sweep", args: []string{"--listen-port", "0", "--catch-up", "0", "--sweep-interval", "1h"}},
		{name: "one sweep and out", args: []string{"--listen-port", "0", "--catch-up", "0", "--sweep-now"}},
	}

	for _, v := range cases {
		t.Run(v.name, func(t *testing.T) {
			setupFlags(t, append([]string{"--bucket", "payloads"}, v.args...))

			if _, err := validate(); err != nil {
				t.Errorf("expected this configuration to be accepted, got %s", err.Error())
			}
		})
	}
}

func TestTheEnvironmentOverridesFlags(t *testing.T) {
	t.Setenv("HIPPOCAMPUS_OBJECT_REAPER_BUCKET", "from-the-environment")

	setupFlags(t, nil)

	if got := viper.GetString("bucket"); got != "from-the-environment" {
		t.Errorf("expected the environment to be read, got %q", got)
	}
}

func TestDeletingIsNotTheDefault(t *testing.T) {
	setupFlags(t, []string{"--bucket", "payloads"})

	if viper.GetBool("delete") {
		t.Error("expected shadow mode to be the default")
	}
}
