package auth

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

func stubHandler(ctx context.Context, req interface{}) (interface{}, error) {
	return "ok", nil
}

// TestUnaryServerInterceptor_ValidToken verifies that a request carrying a valid bearer token in
// the authorization metadata reaches the handler.
func TestUnaryServerInterceptor_ValidToken(t *testing.T) {
	v, err := NewHMACVerifier("test-secret")
	if err != nil {
		t.Fatalf("NewHMACVerifier: %s", err)
	}

	token, err := MintToken("test-secret", "client-1", time.Hour)
	if err != nil {
		t.Fatalf("MintToken: %s", err)
	}

	interceptor := UnaryServerInterceptor(v)

	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer "+token))
	info := &grpc.UnaryServerInfo{FullMethod: "/proto.Hippocampus/GetEvents"}

	res, err := interceptor(ctx, nil, info, stubHandler)
	if err != nil {
		t.Fatalf("expected the call to succeed, got: %s", err)
	}

	if res != "ok" {
		t.Errorf("expected the handler's response to pass through, got %v", res)
	}
}

// TestUnaryServerInterceptor_MissingToken verifies that a Hippocampus RPC with no authorization
// metadata is rejected with codes.Unauthenticated.
func TestUnaryServerInterceptor_MissingToken(t *testing.T) {
	v, err := NewHMACVerifier("test-secret")
	if err != nil {
		t.Fatalf("NewHMACVerifier: %s", err)
	}

	interceptor := UnaryServerInterceptor(v)
	info := &grpc.UnaryServerInfo{FullMethod: "/proto.Hippocampus/GetEvents"}

	if _, err := interceptor(context.Background(), nil, info, stubHandler); status.Code(err) != codes.Unauthenticated {
		t.Errorf("expected codes.Unauthenticated, got %v", err)
	}
}

// TestUnaryServerInterceptor_InvalidToken verifies that an unparseable/invalid token is rejected
// with codes.Unauthenticated.
func TestUnaryServerInterceptor_InvalidToken(t *testing.T) {
	v, err := NewHMACVerifier("test-secret")
	if err != nil {
		t.Fatalf("NewHMACVerifier: %s", err)
	}

	interceptor := UnaryServerInterceptor(v)

	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer not-a-token"))
	info := &grpc.UnaryServerInfo{FullMethod: "/proto.Hippocampus/GetEvents"}

	if _, err := interceptor(ctx, nil, info, stubHandler); status.Code(err) != codes.Unauthenticated {
		t.Errorf("expected codes.Unauthenticated, got %v", err)
	}
}

// TestUnaryServerInterceptor_HealthCheckBypassesAuth verifies the critical scoping guarantee: a
// call outside the /proto.Hippocampus/ prefix - the gRPC health service in particular - reaches
// the handler with no token at all, so orchestrator liveness/readiness probes are never blocked.
func TestUnaryServerInterceptor_HealthCheckBypassesAuth(t *testing.T) {
	v, err := NewHMACVerifier("test-secret")
	if err != nil {
		t.Fatalf("NewHMACVerifier: %s", err)
	}

	interceptor := UnaryServerInterceptor(v)
	info := &grpc.UnaryServerInfo{FullMethod: "/grpc.health.v1.Health/Check"}

	res, err := interceptor(context.Background(), nil, info, stubHandler)
	if err != nil {
		t.Fatalf("expected the health check to bypass auth entirely, got: %s", err)
	}

	if res != "ok" {
		t.Errorf("expected the handler's response to pass through, got %v", res)
	}
}
