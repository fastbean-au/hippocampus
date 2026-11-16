package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/spf13/pflag"
	"github.com/spf13/viper"
)

// run driven as a long-running agent (TODO-3 item 201), against the stub bucket and service the
// --sweep-now tests use. What is pinned is that each path's flags reach it: --sweep-interval runs the
// sweep on a timer, --listen-port and --callback-path open the push path, and --callback-token guards
// it. The stub service does not implement the forgotten log, so the catch-up fails on every run here,
// which also pins that a failed catch-up does not stop the agent coming up.

// freePort returns a port nothing is listening on. The listener is closed before the port is used,
// so another process could take it in between; on a test machine that window is not worth a
// listener-injection seam in the command.
func freePort(t *testing.T) int {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %s", err)
	}

	port := listener.Addr().(*net.TCPAddr).Port

	_ = listener.Close()

	return port
}

// occupiedPort returns a port held by a listener on loopback for the rest of the test. A listener
// that binds all interfaces can still take it on macOS, so the case using it binds loopback too.
func occupiedPort(t *testing.T) int {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %s", err)
	}

	t.Cleanup(func() { _ = listener.Close() })

	return listener.Addr().(*net.TCPAddr).Port
}

// agentArgs is a configuration run accepts in full: the stub bucket holds kept.json and orphan.json,
// and the stub service holds only kept.json's memory. extra is appended, so a later flag overrides an
// earlier one.
func agentArgs(t *testing.T, extra ...string) (*sweepBucket, []string) {
	t.Helper()

	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")

	bucket, endpoint := newSweepBucket(t, "kept.json", "orphan.json")
	address := startHeldService(t, "payloads/kept.json")

	return bucket, append([]string{
		"--bucket", "payloads",
		"--s3-endpoint", endpoint,
		"--s3-region", "us-east-1",
		"--s3-path-style",
		"--address", address,
		"--health-port", "0",
		"--listen-port", "0",
		"--catch-up", "1h",
	}, extra...)
}

// startAgent runs run in the background and returns a function that cancels it and reports what it
// returned.
func startAgent(t *testing.T) func() error {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)

	go func() { done <- run(ctx) }()

	return func() error {
		cancel()

		select {

		case err := <-done:
			return err

		case <-time.After(15 * time.Second):
			t.Fatal("run did not return after its context was cancelled")

			return nil

		}
	}
}

// resetCommandLine gives realMain a fresh global flag set that returns its parse errors rather than
// exiting the test binary, so it can be called once per case.
func resetCommandLine() {
	viper.Reset()

	pflag.CommandLine = pflag.NewFlagSet("object-reaper", pflag.ContinueOnError)
	pflag.CommandLine.SetOutput(io.Discard)
}

func TestTheSweepRunsOnItsInterval(t *testing.T) {
	bucket, args := agentArgs(t, "--delete", "--sweep-interval", "20ms")

	setupFlags(t, args)

	stop := startAgent(t)

	deadline := time.Now().Add(5 * time.Second)

	for time.Now().Before(deadline) && len(bucket.deletions()) == 0 {
		time.Sleep(20 * time.Millisecond)
	}

	if err := stop(); !errors.Is(err, context.Canceled) {
		t.Errorf("expected a cancelled run to report the cancellation, got %v", err)
	}

	if got := bucket.deletions(); len(got) != 1 || got[0] != "orphan.json" {
		t.Errorf("expected the timed sweep to delete only orphan.json, got %v", got)
	}
}

func TestTheCallbackListenerServesItsPathBehindItsToken(t *testing.T) {
	port := freePort(t)

	_, args := agentArgs(
		t,
		"--listen-port", strconv.Itoa(port),
		"--callback-path", "/forgotten",
		"--callback-token", "sekrit",
	)

	setupFlags(t, args)

	stop := startAgent(t)

	url := fmt.Sprintf("http://127.0.0.1:%d/forgotten", port)
	client := &http.Client{Timeout: time.Second}

	var status int

	deadline := time.Now().Add(5 * time.Second)

	for time.Now().Before(deadline) {
		response, err := client.Post(url, "application/json", strings.NewReader("{}"))
		if err != nil {
			time.Sleep(20 * time.Millisecond)

			continue
		}

		_ = response.Body.Close()
		status = response.StatusCode

		break
	}

	if err := stop(); !errors.Is(err, context.Canceled) {
		t.Errorf("expected a cancelled run to report the cancellation, got %v", err)
	}

	if status != http.StatusUnauthorized {
		t.Errorf("expected a delivery without the token to be refused on --callback-path, got status %d", status)
	}
}

func TestRunSurfacesEachStartupFailure(t *testing.T) {
	cases := []struct {
		name    string
		profile string
		extra   []string
		want    string
	}{
		// The endpoint is not contacted at startup, so the bucket fails to open only when the AWS
		// configuration itself cannot be loaded - a named profile that does not exist.
		{"bucket", "no-such-profile", nil, "opening the bucket"},
		{"dial", "", []string{"--tls", "--tls-ca-cert", "/no/such/ca.pem"}, "dialling the hippocampus service"},
		{"health", "", []string{"--health-port", strconv.Itoa(occupiedPort(t)), "--health-bind-address", "127.0.0.1"}, "starting the health endpoints"},
		{"listener", "", []string{"--listen-port", strconv.Itoa(freePort(t)), "--listen-tls-cert", "/no/such/cert.pem"}, "requires both a certificate and a key"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.profile != "" {
				t.Setenv("AWS_CONFIG_FILE", filepath.Join(t.TempDir(), "config"))
				t.Setenv("AWS_PROFILE", c.profile)
			}

			_, args := agentArgs(t, c.extra...)

			setupFlags(t, args)

			// Bounded, so a failure that is not reached reports as a wrong answer rather than as an
			// agent running until the test binary times out.
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			err := run(ctx)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("expected an error containing %q, got %v", c.want, err)
			}
		})
	}
}

func TestRealMainReturnsZeroForVersion(t *testing.T) {
	resetCommandLine()

	if code := realMain(context.Background(), []string{"--version"}); code != 0 {
		t.Errorf("expected exit code 0 for --version, got %d", code)
	}
}

func TestRealMainReturnsOneForABadFlag(t *testing.T) {
	resetCommandLine()

	if code := realMain(context.Background(), []string{"--not-a-flag"}); code != 1 {
		t.Errorf("expected exit code 1 for an unknown flag, got %d", code)
	}
}

func TestRealMainReturnsOneForAStartupFailure(t *testing.T) {
	resetCommandLine()

	if code := realMain(context.Background(), nil); code != 1 {
		t.Errorf("expected exit code 1 for a missing bucket, got %d", code)
	}
}

// A cancelled context is how SIGINT/SIGTERM arrives, and an ordinary shutdown must not exit non-zero
// or every restart looks like a crash to a supervisor.
func TestRealMainReturnsZeroForAShutdown(t *testing.T) {
	_, args := agentArgs(t)

	resetCommandLine()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if code := realMain(ctx, args); code != 0 {
		t.Errorf("expected exit code 0 for a signalled shutdown, got %d", code)
	}
}
