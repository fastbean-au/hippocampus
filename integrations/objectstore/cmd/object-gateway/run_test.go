package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/pflag"
	"github.com/spf13/viper"
	"google.golang.org/grpc"

	"github.com/fastbean-au/hippocampus/contract"
)

// run driven end to end (TODO-3 item 201): the bucket is a stub answering anything with a 200, and
// the service is a stub recording the ids it is asked to recall. What is pinned is that the flags
// reach the pieces they configure - a read under --path-prefix carrying --auth-token is answered in
// --mode, and is reinforced on the service --address names.

// recallService records the ids every RecallMemories call carries.
type recallService struct {
	contract.UnimplementedHippocampusServer

	mu  sync.Mutex
	ids []string
}

func (s *recallService) RecallMemories(
	_ context.Context,
	in *contract.RecallMemoriesRequest,
) (*contract.GetMemoriesResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.ids = append(s.ids, in.GetIds()...)

	return &contract.GetMemoriesResponse{}, nil
}

func (s *recallService) recalled() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]string{}, s.ids...)
}

func startRecallService(t *testing.T) (*recallService, string) {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %s", err)
	}

	service := &recallService{}

	server := grpc.NewServer()
	contract.RegisterHippocampusServer(server, service)

	go func() { _ = server.Serve(listener) }()

	t.Cleanup(server.Stop)

	return service, listener.Addr().String()
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

// gatewayArgs is a configuration run accepts in full, against a stub bucket and service. extra is
// appended, so a later flag overrides an earlier one.
func gatewayArgs(t *testing.T, address string, extra ...string) []string {
	t.Helper()

	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")

	bucket := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	t.Cleanup(bucket.Close)

	return append([]string{
		"--bucket", "payloads",
		"--s3-endpoint", bucket.URL,
		"--s3-region", "us-east-1",
		"--s3-path-style",
		"--address", address,
		"--auth-token", "sekrit",
		"--health-port", "0",
		"--listen-port", strconv.Itoa(freePort(t)),
	}, extra...)
}

// resetCommandLine gives realMain a fresh global flag set that returns its parse errors rather than
// exiting the test binary, so it can be called once per case.
func resetCommandLine() {
	viper.Reset()

	pflag.CommandLine = pflag.NewFlagSet("object-gateway", pflag.ContinueOnError)
	pflag.CommandLine.SetOutput(io.Discard)
}

func TestARunningGatewayServesAndReinforcesARead(t *testing.T) {
	service, address := startRecallService(t)
	port := freePort(t)

	setupFlags(t, gatewayArgs(
		t,
		address,
		"--listen-port", strconv.Itoa(port),
		"--path-prefix", "/files/",
		"--recall-batch-size", "0",
	))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)

	go func() { done <- run(ctx) }()

	url := fmt.Sprintf("http://127.0.0.1:%d/files/report.json", port)
	client := &http.Client{
		Timeout: time.Second,

		// The redirect is the answer under test; following it would send the request to the stub
		// bucket instead.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}

	var status int

	deadline := time.Now().Add(5 * time.Second)

	for time.Now().Before(deadline) {
		request, _ := http.NewRequest(http.MethodGet, url, nil)
		request.Header.Set("Authorization", "Bearer sekrit")

		response, err := client.Do(request)
		if err != nil {
			time.Sleep(20 * time.Millisecond)

			continue
		}

		_ = response.Body.Close()
		status = response.StatusCode

		break
	}

	if status != http.StatusFound && status != http.StatusTemporaryRedirect {
		t.Errorf("expected a redirect-mode read to be answered with a redirect, got status %d", status)
	}

	for time.Now().Before(deadline) && len(service.recalled()) == 0 {
		time.Sleep(20 * time.Millisecond)
	}

	if got := service.recalled(); len(got) != 1 || !strings.HasSuffix(got[0], "report.json") {
		t.Errorf("expected the read to be recalled on the service, got %v", got)
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
}

func TestRunSurfacesEachStartupFailure(t *testing.T) {
	_, address := startRecallService(t)

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
		{"gateway", "", []string{"--path-prefix", "no-slashes"}, "building the gateway"},
		{"health", "", []string{"--health-port", strconv.Itoa(occupiedPort(t)), "--health-bind-address", "127.0.0.1"}, "starting the health endpoints"},
		{"listener", "", []string{"--listen-tls-cert", "/no/such/cert.pem"}, "requires both a certificate and a key"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.profile != "" {
				t.Setenv("AWS_CONFIG_FILE", filepath.Join(t.TempDir(), "config"))
				t.Setenv("AWS_PROFILE", c.profile)
			}

			setupFlags(t, gatewayArgs(t, address, c.extra...))

			// Bounded, so a failure that is not reached reports as a wrong answer rather than as a
			// gateway serving until the test binary times out.
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

	if code := realMain(context.Background(), []string{"--allow-anonymous"}); code != 1 {
		t.Errorf("expected exit code 1 for a missing bucket, got %d", code)
	}
}

// A cancelled context is how SIGINT/SIGTERM arrives, and an ordinary shutdown must not exit non-zero
// or every restart looks like a crash to a supervisor.
func TestRealMainReturnsZeroForAShutdown(t *testing.T) {
	_, address := startRecallService(t)
	args := gatewayArgs(t, address)

	resetCommandLine()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if code := realMain(ctx, args); code != 0 {
		t.Errorf("expected exit code 0 for a signalled shutdown, got %d", code)
	}
}
