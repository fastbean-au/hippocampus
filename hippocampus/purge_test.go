package hippocampus

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestHTTPMiddlewareBlockWhenPurgeInProgress is a regression test: the HTTP
// gateway calls the server's methods directly and never runs the gRPC interceptor chain, so before
// this middleware existed a /v1/... request ran normally while a purge was in progress. It must be
// rejected with 503, while the open paths (health, OpenAPI) stay reachable, and everything passes
// through when no purge is running.
func TestHTTPMiddlewareBlockWhenPurgeInProgress(t *testing.T) {
	s := &Server{}

	openPaths := []string{"/healthz", "/v1/openapi.json"}

	cases := []struct {
		name     string
		purging  bool
		path     string
		wantCode int
		wantNext bool
	}{
		{name: "no purge, rpc path passes", purging: false, path: "/v1/memories", wantCode: http.StatusOK, wantNext: true},
		{name: "purge blocks rpc path", purging: true, path: "/v1/memories", wantCode: http.StatusServiceUnavailable, wantNext: false},
		{name: "purge allows healthz", purging: true, path: "/healthz", wantCode: http.StatusOK, wantNext: true},
		{name: "purge allows openapi", purging: true, path: "/v1/openapi.json", wantCode: http.StatusOK, wantNext: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s.purgeInProgress.Store(tc.purging)

			nextCalled := false
			next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				nextCalled = true
				w.WriteHeader(http.StatusOK)
			})

			handler := s.HTTPMiddlewareBlockWhenPurgeInProgress(next, openPaths)

			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, tc.path, nil))

			if rec.Code != tc.wantCode {
				t.Errorf("status = %d, want %d", rec.Code, tc.wantCode)
			}

			if nextCalled != tc.wantNext {
				t.Errorf("next handler called = %v, want %v", nextCalled, tc.wantNext)
			}
		})
	}
}

// TestInterceptorBlockWhenPurgeInProgress_Code verifies the gRPC interceptor rejects Hippocampus
// RPCs with codes.Unavailable during a purge (previously a bare fmt.Errorf, i.e. codes.Unknown),
// and leaves non-Hippocampus methods (health) and all methods outside a purge untouched.
func TestInterceptorBlockWhenPurgeInProgress_Code(t *testing.T) {
	s := &Server{}

	handlerCalled := false
	handler := func(ctx context.Context, req any) (any, error) {
		handlerCalled = true

		return "ok", nil
	}

	rpcInfo := &grpc.UnaryServerInfo{FullMethod: "/proto.Hippocampus/GetEvents"}
	healthInfo := &grpc.UnaryServerInfo{FullMethod: "/grpc.health.v1.Health/Check"}

	// Purge in progress: a Hippocampus RPC is rejected with Unavailable and the handler never runs.
	s.purgeInProgress.Store(true)

	handlerCalled = false
	_, err := s.InterceptorBlockWhenPurgeInProgress(context.Background(), nil, rpcInfo, handler)
	if status.Code(err) != codes.Unavailable {
		t.Errorf("rpc during purge: code = %s, want Unavailable", status.Code(err))
	}

	if handlerCalled {
		t.Error("rpc during purge: handler ran; expected it to be blocked")
	}

	// Health checks stay reachable during a purge.
	handlerCalled = false
	if _, err := s.InterceptorBlockWhenPurgeInProgress(context.Background(), nil, healthInfo, handler); err != nil {
		t.Errorf("health during purge: unexpected error %v", err)
	}

	if !handlerCalled {
		t.Error("health during purge: handler did not run; health must stay reachable")
	}

	// No purge: the RPC passes through.
	s.purgeInProgress.Store(false)

	handlerCalled = false
	if _, err := s.InterceptorBlockWhenPurgeInProgress(context.Background(), nil, rpcInfo, handler); err != nil {
		t.Errorf("rpc without purge: unexpected error %v", err)
	}

	if !handlerCalled {
		t.Error("rpc without purge: handler did not run")
	}
}
