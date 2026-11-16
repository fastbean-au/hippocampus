package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/spf13/pflag"
	"github.com/spf13/viper"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthgrpc "google.golang.org/grpc/health/grpc_health_v1"
)

// run driven past validation into the promoter loop (TODO-3 item 201). The source is a gRPC server
// answering only the health service, and the target is an address nothing listens on. What is pinned
// is that the two endpoints' flags reach the two connections the right way round: /readyz reports the
// source reachable and the target not, under the names an operator reads.

// startHealthyService serves the gRPC health service reporting "hippocampus" as SERVING, which is
// what readiness asks of each end, and nothing else.
func startHealthyService(t *testing.T) string {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %s", err)
	}

	status := health.NewServer()
	status.SetServingStatus("hippocampus", healthgrpc.HealthCheckResponse_SERVING)

	server := grpc.NewServer()
	healthgrpc.RegisterHealthServer(server, status)

	go func() { _ = server.Serve(listener) }()

	t.Cleanup(server.Stop)

	return listener.Addr().String()
}

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

// ingestorArgs is a configuration run accepts in full: a healthy source and an unreachable target.
// extra is appended, so a later flag overrides an earlier one.
func ingestorArgs(t *testing.T, extra ...string) []string {
	t.Helper()

	return append([]string{
		"--rules", writeRules(t, goodRules),
		"--source-address", startHealthyService(t),
		"--target-address", fmt.Sprintf("127.0.0.1:%d", freePort(t)),
		"--health-port", "0",
		"--call-timeout-seconds", "1",
	}, extra...)
}

// resetCommandLine gives realMain a fresh global flag set that returns its parse errors rather than
// exiting the test binary, so it can be called once per case.
func resetCommandLine() {
	viper.Reset()

	pflag.CommandLine = pflag.NewFlagSet("ingestor", pflag.ContinueOnError)
	pflag.CommandLine.SetOutput(io.Discard)
}

func TestReadinessNamesTheUnreachableEnd(t *testing.T) {
	port := freePort(t)

	setupFlags(t, ingestorArgs(
		t,
		"--health-port", strconv.Itoa(port),
		"--health-bind-address", "127.0.0.1",
		"--dry-run",
	))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)

	go func() { done <- run(ctx) }()

	url := fmt.Sprintf("http://127.0.0.1:%d/readyz", port)
	client := &http.Client{Timeout: 5 * time.Second}

	var body struct {
		Dependencies map[string]string `json:"dependencies"`
	}

	status := 0
	deadline := time.Now().Add(10 * time.Second)

	for time.Now().Before(deadline) {
		response, err := client.Get(url)
		if err != nil {
			time.Sleep(20 * time.Millisecond)

			continue
		}

		status = response.StatusCode
		err = json.NewDecoder(response.Body).Decode(&body)

		_ = response.Body.Close()

		if err != nil {
			t.Fatalf("decoding the /readyz body: %s", err)
		}

		break
	}

	cancel()

	select {

	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("expected a cancelled run to report the cancellation, got %v", err)
		}

	case <-time.After(15 * time.Second):
		t.Fatal("run did not return after its context was cancelled")

	}

	if status != http.StatusServiceUnavailable {
		t.Errorf("expected /readyz to report not ready with the target down, got status %d", status)
	}

	if body.Dependencies[sourceEndpoint] != "ok" || body.Dependencies[targetEndpoint] != "unreachable" {
		t.Errorf("expected the source ok and the target unreachable, got %v", body.Dependencies)
	}
}

func TestRunSurfacesEachStartupFailure(t *testing.T) {
	cases := []struct {
		name  string
		extra []string
		want  string
	}{
		{"source", []string{"--source-tls", "--source-tls-ca-cert", "/no/such/ca.pem"}, "dialling the source instance"},
		{"target", []string{"--target-tls", "--target-tls-ca-cert", "/no/such/ca.pem"}, "dialling the target instance"},
		{"health", []string{"--health-port", strconv.Itoa(occupiedPort(t)), "--health-bind-address", "127.0.0.1"}, "starting the health endpoints"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			setupFlags(t, ingestorArgs(t, c.extra...))

			// Bounded, so a failure that is not reached reports as a wrong answer rather than as an
			// ingestor running until the test binary times out.
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
		t.Errorf("expected exit code 1 for a missing rules file, got %d", code)
	}
}

// A cancelled context is how SIGINT/SIGTERM arrives, and an ordinary shutdown must not exit non-zero
// or every restart looks like a crash to a supervisor.
func TestRealMainReturnsZeroForAShutdown(t *testing.T) {
	args := ingestorArgs(t)

	resetCommandLine()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if code := realMain(ctx, args); code != 0 {
		t.Errorf("expected exit code 0 for a signalled shutdown, got %d", code)
	}
}
