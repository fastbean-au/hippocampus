package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/fastbean-au/hippocampus/auth"
)

// TestInterceptorRecoverPanic verifies that a panicking handler is turned into a codes.Internal
// error rather than propagating out and crashing the process, and that a non-panicking handler is
// passed through untouched.
func TestInterceptorRecoverPanic(t *testing.T) {
	panicking := func(ctx context.Context, req interface{}) (interface{}, error) {
		panic("boom")
	}

	resp, err := InterceptorRecoverPanic(context.Background(), nil, &grpc.UnaryServerInfo{FullMethod: "/proto.Hippocampus/Test"}, panicking)
	if resp != nil {
		t.Errorf("expected a nil response after a recovered panic, got %v", resp)
	}

	if status.Code(err) != codes.Internal {
		t.Errorf("expected codes.Internal after a recovered panic, got %v", status.Code(err))
	}

	okHandler := func(ctx context.Context, req interface{}) (interface{}, error) {
		return "ok", nil
	}

	resp, err = InterceptorRecoverPanic(context.Background(), nil, &grpc.UnaryServerInfo{FullMethod: "/proto.Hippocampus/Test"}, okHandler)
	if err != nil {
		t.Errorf("expected no error from a clean handler, got %v", err)
	}

	if resp != "ok" {
		t.Errorf("expected the clean handler's response to pass through, got %v", resp)
	}
}

// TestInterceptorLogger_LogsFailures verifies that a failing RPC produces an operational log entry
// at the right level (Warn for server-fault codes, Info for client-fault codes) while a successful
// RPC stays quiet.
func TestInterceptorLogger_LogsFailures(t *testing.T) {
	cases := []struct {
		name      string
		err       error
		wantLevel log.Level
		wantEntry bool
	}{
		{name: "internal", err: status.Error(codes.Internal, "boom"), wantLevel: log.WarnLevel, wantEntry: true},
		{name: "unavailable", err: status.Error(codes.Unavailable, "down"), wantLevel: log.WarnLevel, wantEntry: true},
		{name: "not_found", err: status.Error(codes.NotFound, "missing"), wantLevel: log.InfoLevel, wantEntry: true},
		{name: "invalid_argument", err: status.Error(codes.InvalidArgument, "bad"), wantLevel: log.InfoLevel, wantEntry: true},
		{name: "success", err: nil, wantEntry: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hook := logtest.NewGlobal()
			log.SetLevel(log.InfoLevel)

			handler := func(ctx context.Context, req interface{}) (interface{}, error) {
				return nil, tc.err
			}

			_, err := InterceptorLogger(context.Background(), nil, &grpc.UnaryServerInfo{FullMethod: "/proto.Hippocampus/Test"}, handler)
			if (err != nil) != (tc.err != nil) {
				t.Fatalf("unexpected error passthrough: got %v want %v", err, tc.err)
			}

			entry := hook.LastEntry()

			if !tc.wantEntry {
				if entry != nil {
					t.Fatalf("expected no log entry for a successful RPC, got %q at %s", entry.Message, entry.Level)
				}

				return
			}

			if entry == nil {
				t.Fatal("expected a log entry for a failing RPC, got none")
			}

			if entry.Level != tc.wantLevel {
				t.Errorf("expected level %s, got %s", tc.wantLevel, entry.Level)
			}

			if _, ok := entry.Data["code"]; !ok {
				t.Error("expected the log entry to carry a 'code' field")
			}

			if _, ok := entry.Data["duration_us"]; !ok {
				t.Error("expected the log entry to carry a 'duration_us' field")
			}
		})
	}
}

// TestHTTPLoggingMiddleware verifies that a 5xx response logs at Warn while a 2xx response does not
// (it only logs at Debug, which is below the info test level), and that the captured status matches.
func TestHTTPLoggingMiddleware(t *testing.T) {
	log.SetLevel(log.InfoLevel)

	t.Run("server error logs at warn", func(t *testing.T) {
		hook := logtest.NewGlobal()

		h := httpLoggingMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))

		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/memories", nil))

		entry := hook.LastEntry()
		if entry == nil {
			t.Fatal("expected a log entry for a 500 response, got none")
		}

		if entry.Level != log.WarnLevel {
			t.Errorf("expected Warn for a 500, got %s", entry.Level)
		}

		if entry.Data["status"] != http.StatusInternalServerError {
			t.Errorf("expected status 500 captured, got %v", entry.Data["status"])
		}
	})

	t.Run("success does not log above debug", func(t *testing.T) {
		hook := logtest.NewGlobal()

		h := httpLoggingMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))

		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/memories", nil))

		if entry := hook.LastEntry(); entry != nil {
			t.Fatalf("expected no log entry above Debug for a 200, got %q at %s", entry.Message, entry.Level)
		}
	})
}

// TestRecoverMiddleware verifies that a panicking gateway handler is turned into a 500 response
// rather than propagating out, while a clean handler passes through with its own status.
func TestRecoverMiddleware(t *testing.T) {
	panicking := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	})

	rec := httptest.NewRecorder()
	recoverMiddleware(panicking).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/memories", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("expected 500 after a recovered panic, got %d", rec.Code)
	}

	clean := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	rec = httptest.NewRecorder()
	recoverMiddleware(clean).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Errorf("expected a clean handler to pass through with 200, got %d", rec.Code)
	}
}

// TestInterceptorLogger_AttributesFailureToClient verifies that when the auth interceptor has
// stashed verified claims on the context (auth enabled), a failing RPC's log entry carries the
// client_id field - the per-client audit trail InterceptorLogger documents - and that it is absent
// when no claims are present (auth disabled).
func TestInterceptorLogger_AttributesFailureToClient(t *testing.T) {
	log.SetLevel(log.InfoLevel)

	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		return nil, status.Error(codes.Internal, "boom")
	}

	t.Run("claims present", func(t *testing.T) {
		hook := logtest.NewGlobal()

		ctx := auth.ContextWithClaims(context.Background(), &auth.Claims{ClientID: "client-42"})

		_, _ = InterceptorLogger(ctx, nil, &grpc.UnaryServerInfo{FullMethod: "/proto.Hippocampus/Test"}, handler)

		entry := hook.LastEntry()
		if entry == nil {
			t.Fatal("expected a log entry for the failing RPC")
		}

		if got := entry.Data["client_id"]; got != "client-42" {
			t.Errorf("client_id = %v, want %q", got, "client-42")
		}
	})

	t.Run("no claims", func(t *testing.T) {
		hook := logtest.NewGlobal()

		_, _ = InterceptorLogger(context.Background(), nil, &grpc.UnaryServerInfo{FullMethod: "/proto.Hippocampus/Test"}, handler)

		entry := hook.LastEntry()
		if entry == nil {
			t.Fatal("expected a log entry for the failing RPC")
		}

		if _, ok := entry.Data["client_id"]; ok {
			t.Errorf("expected no client_id field without stashed claims, got %v", entry.Data["client_id"])
		}
	})
}

// TestHTTPLoggingMiddleware_AttributesToClient is the HTTP-gateway counterpart to
// TestInterceptorLogger_AttributesFailureToClient: auth.HTTPMiddleware stashes verified claims on
// the request context before httpLoggingMiddleware runs, and the client_id must appear on the log
// entry so gateway traffic gets the same per-client audit trail as the gRPC side.
func TestHTTPLoggingMiddleware_AttributesToClient(t *testing.T) {
	log.SetLevel(log.InfoLevel)

	hook := logtest.NewGlobal()

	h := httpLoggingMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))

	req := httptest.NewRequest(http.MethodPost, "/v1/memories", nil)
	req = req.WithContext(auth.ContextWithClaims(req.Context(), &auth.Claims{ClientID: "gateway-client"}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	entry := hook.LastEntry()
	if entry == nil {
		t.Fatal("expected a log entry for the 500 response")
	}

	if got := entry.Data["client_id"]; got != "gateway-client" {
		t.Errorf("client_id = %v, want %q", got, "gateway-client")
	}
}
