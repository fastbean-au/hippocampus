package main

import (
	"context"
	"os"
	"slices"
	"testing"

	"github.com/spf13/pflag"
	"github.com/spf13/viper"

	blueskybridge "github.com/fastbean-au/hippocampus/integrations/eventsource/bluesky"
)

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

func resetCommandLine() {
	viper.Reset()
	pflag.CommandLine = pflag.NewFlagSet("bluesky", pflag.ContinueOnError)
	pflag.CommandLine.SetOutput(os.NewFile(0, os.DevNull))
}

// cancelledCtx returns an already-cancelled context so the bridge's reconnect loop returns before it
// dials Jetstream, letting run's happy path be exercised with no network at all.
func cancelledCtx() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	return ctx
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
	setupFlags(t, []string{"--log-level", "bogus"})

	if err := serve(context.Background()); err == nil {
		t.Errorf("serve should error on an invalid log level")
	}
}

func TestServe_RunsThrough(t *testing.T) {
	setupFlags(t, []string{"--log-level", "debug"})

	if err := serve(cancelledCtx()); err != nil {
		t.Errorf("serve through a cancelled context = %v, want nil", err)
	}
}

func TestRun_HappyPathReturnsOnCancel(t *testing.T) {
	setupFlags(t, nil)

	if err := run(cancelledCtx()); err != nil {
		t.Errorf("run with a cancelled context = %v, want nil", err)
	}
}

// TestRun_ConfiguredPathsReturnOnCancel drives thread mode, a post-less collection list and a valid
// client-credentials config through run() in ONE call.
//
// Deliberately one rather than three: each run() builds a gRPC client and installs and tears down
// the global OTEL providers, and stacking several of those alongside the TestRealMain cases below
// (which drive signal.NotifyContext) has tripped a runtime fatal in os/signal. The branches under
// test are independent of each other, so exercising them together loses nothing.
func TestRun_ConfiguredPathsReturnOnCancel(t *testing.T) {
	setupFlags(t, []string{
		"--events", "thread",
		"--group", "bluesky",
		"--collections", "app.bsky.feed.like",
		"--oidc-issuer", "http://127.0.0.1:1/realms/hippocampus",
		"--oidc-client-id", "hippocampus-gen",
		"--oidc-client-secret", "secret",
	})

	// A complete client-credentials config must not reach out to the IdP at startup, so an
	// unreachable issuer does not stop the bridge coming up.
	if err := run(cancelledCtx()); err != nil {
		t.Errorf("run = %v, want nil", err)
	}
}

func TestRun_InvalidEventsMode(t *testing.T) {
	setupFlags(t, []string{"--events", "sideways"})

	if err := run(context.Background()); err == nil {
		t.Errorf("run should reject an unknown --events mode")
	}
}

// TestRun_TooManyCollections: Jetstream caps wantedCollections at 100, and finding that out as a
// server-side rejection is a confusing way to learn about a typo in a long list.
func TestRun_TooManyCollections(t *testing.T) {
	collections := make([]string, 0, 101)

	for i := range 101 {
		collections = append(collections, string(rune('a'+i%26))+"pp.test.collection")
	}

	args := []string{"--collections", joinComma(collections)}

	setupFlags(t, args)

	if err := run(context.Background()); err == nil {
		t.Errorf("run should reject more than 100 collections")
	}
}

func TestRun_DialError(t *testing.T) {
	setupFlags(t, []string{"--tls", "--tls-ca-cert", "/no/such/ca.pem"})

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

// TestWarnOnSubscriptionScope drives every combination the warnings distinguish. They are warnings,
// so there is nothing to assert beyond reaching each branch - which is the point: the combinations
// are legal, silent, and do less than they look like they do.
func TestWarnOnSubscriptionScope(t *testing.T) {
	feed := "at://did:plc:x/app.bsky.feed.generator/news"
	dids := []string{"did:plc:news"}

	cases := []struct {
		name    string
		feed    string
		dids    []string
		capture bool
	}{
		{name: "the firehose, unfiltered and uncaptured"},
		{name: "a feed with no DID filter", feed: feed},
		{name: "a feed whose DID filter removes its own engagement", feed: feed, dids: dids},
		{name: "capture with nothing else deciding what to store", capture: true},
		{name: "capture on a filtered subscription", dids: dids, capture: true},
		{name: "capture as intended", feed: feed, capture: true},
	}

	for _, v := range cases {
		t.Run(v.name, func(t *testing.T) {
			warnOnSubscriptionScope(v.feed, v.dids, v.capture)
		})
	}
}

// TestSignificanceHeader covers the command's half of --capture-significance: the flag is delivered
// through the transformer's per-message override, so the command must point that override at the
// header the bridge stamps, and must refuse the operator's own --significance-header rather than
// silently letting one of the two win.
func TestSignificanceHeader(t *testing.T) {
	t.Run("the operator's header by default", func(t *testing.T) {
		setupFlags(t, []string{"--significance-header", "priority"})

		if got := significanceHeader(); got != "priority" {
			t.Errorf("significanceHeader = %q, want the operator's own", got)
		}
	})

	t.Run("the capture header when a capture significance is set", func(t *testing.T) {
		setupFlags(t, []string{"--capture-significance", "3"})

		if got := significanceHeader(); got != blueskybridge.CaptureSignificanceHeader {
			t.Errorf("significanceHeader = %q, want the capture header", got)
		}
	})

	t.Run("neither, unset", func(t *testing.T) {
		setupFlags(t, nil)

		if got := significanceHeader(); got != "" {
			t.Errorf("significanceHeader = %q, want no override", got)
		}
	})

	t.Run("the combination is refused", func(t *testing.T) {
		setupFlags(t, []string{"--capture-significance", "3", "--significance-header", "priority"})

		if err := run(context.Background()); err == nil {
			t.Error("expected --capture-significance with --significance-header to be refused")
		}
	})
}

func TestSlicesContain(t *testing.T) {
	in := []string{"a", "b"}

	if !slicesContain(in, "b") {
		t.Error("expected b to be found")
	}

	if slicesContain(in, "c") {
		t.Error("did not expect c to be found")
	}

	if slicesContain(nil, "a") {
		t.Error("an empty slice contains nothing")
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

	if code := realMain([]string{"--log-level", "bogus"}); code != 1 {
		t.Errorf("realMain with a bad log level = %d, want 1", code)
	}
}

// joinComma builds the repeated-flag form pflag's StringSlice parses.
func joinComma(in []string) string {
	out := ""

	for i, v := range in {
		if i > 0 {
			out += ","
		}

		out += v
	}

	return out
}

// TestRun_RejectsAnIncompleteOIDCConfig: client-credentials auth is validated at startup (no
// network), so a typo'd flag fails immediately rather than at whatever hour the first post arrives.
func TestRun_RejectsAnIncompleteOIDCConfig(t *testing.T) {
	setupFlags(t, []string{"--oidc-client-id", "hippocampus-gen"}) // no secret, no issuer

	if err := run(context.Background()); err == nil {
		t.Error("run should reject client-credentials auth with no secret or issuer")
	}
}
