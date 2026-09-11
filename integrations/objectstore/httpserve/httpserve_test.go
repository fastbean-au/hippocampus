package httpserve

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

// freePort asks the kernel for a port and gives it straight back, which is the least racy way to
// pick one for a test that must then bind it itself.
func freePort(t *testing.T) int {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("could not find a free port: %s", err.Error())
	}

	port := listener.Addr().(*net.TCPAddr).Port

	if err := listener.Close(); err != nil {
		t.Fatalf("could not release the port: %s", err.Error())
	}

	return port
}

func TestServeServesUntilItsContextIsCancelled(t *testing.T) {
	port := freePort(t)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /hello", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("hello"))
	})

	ctx, cancel := context.WithCancel(context.Background())

	errs := make(chan error, 1)

	go func() {
		errs <- Serve(ctx, Config{
			Handler:     mux,
			Name:        "test",
			BindAddress: "127.0.0.1",
			Port:        port,
		})
	}()

	url := fmt.Sprintf("http://127.0.0.1:%d/hello", port)

	if body := getEventually(t, url); body != "hello" {
		t.Errorf("expected the handler's response, got %q", body)
	}

	cancel()

	select {

	case err := <-errs:
		if err != context.Canceled {
			t.Errorf("expected a clean shutdown to report the cancellation, got %v", err)
		}

	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after its context was cancelled")

	}
}

func TestHalfATLSConfigurationIsRefused(t *testing.T) {
	if err := Serve(context.Background(), Config{TLSCertFile: "cert.pem", Name: "test"}); err == nil {
		t.Error("expected a certificate without a key to be refused")
	}

	if err := Serve(context.Background(), Config{TLSKeyFile: "key.pem", Name: "test"}); err == nil {
		t.Error("expected a key without a certificate to be refused")
	}
}

func TestAPortThatCannotBeBoundIsReported(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("could not listen: %s", err.Error())
	}

	defer func() { _ = listener.Close() }()

	port := listener.Addr().(*net.TCPAddr).Port

	if err := Serve(context.Background(), Config{
		Handler:     http.NewServeMux(),
		Name:        "test",
		BindAddress: "127.0.0.1",
		Port:        port,
	}); err == nil {
		t.Error("expected binding an occupied port to fail")
	}
}

func getEventually(t *testing.T, url string) string {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)

	for time.Now().Before(deadline) {
		response, err := http.Get(url)
		if err != nil {
			time.Sleep(10 * time.Millisecond)

			continue
		}

		body, _ := io.ReadAll(response.Body)

		_ = response.Body.Close()

		return string(body)
	}

	t.Fatal("the listener never answered")

	return ""
}
