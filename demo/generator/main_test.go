package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/spf13/viper"
)

// TestRun_StartsAndStopsOnContextCancellation drives the whole startup path - flag binding, client
// construction, worker launch - and confirms cancelling the context brings it back down cleanly.
//
// No service is needed: grpc.NewClient does not dial eagerly, and the workers treat a failing RPC
// as a transient condition to log and carry on from, which is exactly what a generator pointed at a
// service that is not up yet has to do.
func TestRun_StartsAndStopsOnContextCancellation(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)

	go func() {
		done <- run(ctx, []string{
			"--address", "127.0.0.1:1",
			"--data_dir", t.TempDir(),
			"--log_level", "error",
			"--seed", "1",
			"--bursty_workers", "1",
			"--slow_workers", "0",
			"--loose_workers", "0",
			"--query_workers", "0",
			"--mutator_workers", "0",
		})
	}()

	// Let the workers actually start before pulling the rug out.
	time.Sleep(200 * time.Millisecond)
	cancel()

	select {

	case err := <-done:
		if err != nil {
			t.Errorf("run: %s", err)
		}

	case <-time.After(30 * time.Second):
		t.Fatal("run did not return after its context was cancelled")

	}
}

// TestRun_SeedsFromTheClockWhenUnset covers the zero-seed branch, which is what makes successive
// runs differ rather than replaying one identical load.
func TestRun_SeedsFromTheClockWhenUnset(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)

	go func() {
		done <- run(ctx, []string{
			"--address", "127.0.0.1:1",
			"--data_dir", t.TempDir(),
			"--log_level", "error",
			"--bursty_workers", "0",
			"--slow_workers", "0",
			"--loose_workers", "0",
			"--query_workers", "0",
			"--mutator_workers", "0",
		})
	}()

	time.Sleep(100 * time.Millisecond)
	cancel()

	select {

	case err := <-done:
		if err != nil {
			t.Errorf("run: %s", err)
		}

	case <-time.After(30 * time.Second):
		t.Fatal("run did not return after its context was cancelled")

	}
}

// TestRun_Rejections covers the two ways startup refuses before any worker is launched.
//
// The third failure point in run - grpc.NewClient - is not among them: it validates dial options
// rather than the target string, and returns no error for any address (the resolver is not consulted
// until the first RPC), so there is no address that reaches that branch.
func TestRun_Rejections(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		message string
	}{
		{
			name:    "unknown flag",
			args:    []string{"--not-a-flag"},
			message: "parse command line flags",
		},
		{
			name:    "invalid log level",
			args:    []string{"--log_level", "verbose"},
			message: "invalid log level",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			viper.Reset()
			t.Cleanup(viper.Reset)

			// A bounded context, so a case that unexpectedly gets past the refusal and starts the
			// generator fails the test rather than hanging it.
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			err := run(ctx, test.args)
			if err == nil {
				t.Fatal("expected startup to be refused")
			}

			if !strings.Contains(err.Error(), test.message) {
				t.Errorf("expected the message to mention %q, got %q", test.message, err)
			}
		})
	}
}
