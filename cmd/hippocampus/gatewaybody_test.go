package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/spf13/viper"
)

// The gateway read whatever body a caller sent and for as long as they took to send it (TODO-3
// item 161): gateway.maxRequestBytes was opt-in, and only the header read had a deadline. The
// reason for leaving it open - a large ImportBatch over HTTP - only ever covered bodies the native
// gRPC transport would refuse at its own 4 MiB, so the same request was bounded on one transport and
// unbounded on the other.

func TestGatewayBodyLimit(t *testing.T) {
	cases := []struct {
		name            string
		set             bool
		configured      int64
		maxRecvMsgBytes int
		want            int64
	}{
		{"unset follows grpc-go's 4 MiB, doubled for JSON", false, 0, 0, 8 << 20},
		{"unset follows a raised gRPC limit", false, 0, 16 << 20, 32 << 20},
		{"an explicit value wins", true, 1 << 20, 16 << 20, 1 << 20},
		{"an explicit 0 is unbounded", true, 0, 0, 0},
		{"a negative value is unbounded, as it always was", true, -1, 0, 0},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := gatewayBodyLimit(c.set, c.configured, c.maxRecvMsgBytes); got != c.want {
				t.Errorf("gatewayBodyLimit = %d, want %d", got, c.want)
			}
		})
	}
}

func TestNewGatewayServer_BoundsTheWholeRequestRead(t *testing.T) {
	s := newGatewayServer("", 8080, http.NotFoundHandler())

	if s.ReadTimeout <= 0 {
		t.Fatal("the gateway sets no ReadTimeout, so a caller can hold a connection open with a slow body")
	}

	if s.ReadTimeout < time.Minute {
		t.Errorf("ReadTimeout %s is not generous: a legitimate body at the default cap has to fit inside it", s.ReadTimeout)
	}
}

// TestRun_GatewayRefusesAnOversizedBodyByDefault drives the real gateway with nothing configured: a
// body over the derived ceiling is refused rather than read.
func TestRun_GatewayRefusesAnOversizedBodyByDefault(t *testing.T) {
	_, gwBase := baseRunConfig(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- run(ctx, versionInfo{}) }()

	waitForOK(t, http.DefaultClient, gwBase+"/healthz")

	body := bytes.Repeat([]byte("x"), (8<<20)+1)

	res, err := http.Post(gwBase+"/v1/memories", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}

	_ = res.Body.Close()

	if res.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("an %d-byte body with no limit configured = %d, want 413", len(body), res.StatusCode)
	}

	cancel()
	<-done

	if viper.IsSet("gateway.maxRequestBytes") {
		t.Error("the test must run with gateway.maxRequestBytes unset")
	}
}

// TestMaxRequestBytesMiddleware_AnswersTooLarge: the docs have always promised a 413, but the gateway
// turns any body-read failure into InvalidArgument, so an oversized body was answered 400 - which
// tells a client its request was malformed rather than that it should send a smaller one. A body
// that declares its length is refused before the handler runs; one that does not (chunked) is
// refused when the read overflows, by rewriting the status the handler writes.
func TestMaxRequestBytesMiddleware_AnswersTooLarge(t *testing.T) {
	const limit = 16

	called := false

	// The handler stands in for the gateway: it reads the body and answers 400 on a read error.
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true

		if _, err := io.ReadAll(r.Body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)

			return
		}

		w.WriteHeader(http.StatusOK)
	})

	handler := maxRequestBytesMiddleware(next, limit)

	declared := httptest.NewRequest(http.MethodPost, "/v1/memories", strings.NewReader(strings.Repeat("a", limit+1)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, declared)

	if rec.Code != http.StatusRequestEntityTooLarge || called {
		t.Errorf("a declared over-limit body = %d (handler ran: %v), want 413 without running the handler", rec.Code, called)
	}

	chunked := httptest.NewRequest(http.MethodPost, "/v1/memories", io.NopCloser(strings.NewReader(strings.Repeat("a", limit+1))))
	chunked.ContentLength = -1
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, chunked)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("a chunked over-limit body = %d, want 413", rec.Code)
	}

	within := httptest.NewRequest(http.MethodPost, "/v1/memories", io.NopCloser(strings.NewReader("ok")))
	within.ContentLength = -1
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, within)

	if rec.Code != http.StatusOK {
		t.Errorf("a body within the limit = %d, want the handler's own 200", rec.Code)
	}
}
