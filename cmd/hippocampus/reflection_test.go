package main

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/spf13/viper"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	reflectionpb "google.golang.org/grpc/reflection/grpc_reflection_v1"
	"google.golang.org/grpc/status"
)

// TestReflectionSetting pins the derived default and its override. The default is the only thing
// here that is a judgement rather than a mechanism, and the judgement is about SURFACE rather than
// secrecy: the schema is published with the source, so what the derivation defends is that
// reflection - being a streaming RPC in an all-unary interceptor chain - reaches neither the auth
// interceptor nor either rate limiter. The override has to work in both directions, which is why
// the setting is read through viper.IsSet rather than GetBool.
func TestReflectionSetting(t *testing.T) {
	cases := []struct {
		name       string
		authMethod string
		set        bool
		value      bool
		want       bool
	}{
		{name: "on by default without auth", authMethod: "none", want: true},
		{name: "off by default under hmac", authMethod: "hmac", want: false},
		{name: "off by default under idp", authMethod: "idp", want: false},
		{name: "explicitly off without auth", authMethod: "none", set: true, value: false, want: false},
		{name: "explicitly on under idp", authMethod: "idp", set: true, value: true, want: true},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			viper.Reset()
			t.Cleanup(viper.Reset)

			if testCase.set {
				viper.Set("reflection.enabled", testCase.value)
			}

			enabled, reason := reflectionSetting(testCase.authMethod)

			if enabled != testCase.want {
				t.Errorf("reflectionSetting(%q) = %v, want %v", testCase.authMethod, enabled, testCase.want)
			}

			if reason == "" {
				t.Error("reflectionSetting returned no reason - the startup line would say nothing about which default it took")
			}
		})
	}
}

// waitForGRPCListener polls the gRPC port until it accepts a connection, so a reflection call that
// fails does so because of the registration rather than because the server had not bound yet.
func waitForGRPCListener(t *testing.T, port int) {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)

	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 500*time.Millisecond)
		if err == nil {
			_ = conn.Close()

			return
		}

		time.Sleep(50 * time.Millisecond)
	}

	t.Fatalf("gRPC listener on port %d never came up", port)
}

// listServices asks the running instance's reflection service to enumerate what it serves, and
// returns the names alongside any error the stream reported.
func listServices(t *testing.T, port int) ([]string, error) {
	t.Helper()

	conn, err := grpc.NewClient(
		fmt.Sprintf("127.0.0.1:%d", port),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	defer func() { _ = conn.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	stream, err := reflectionpb.NewServerReflectionClient(conn).ServerReflectionInfo(ctx)
	if err != nil {
		return nil, err
	}

	request := &reflectionpb.ServerReflectionRequest{
		MessageRequest: &reflectionpb.ServerReflectionRequest_ListServices{},
	}

	if err := stream.Send(request); err != nil {
		return nil, err
	}

	response, err := stream.Recv()
	if err != nil {
		return nil, err
	}

	var names []string

	for _, service := range response.GetListServicesResponse().GetService() {
		names = append(names, service.GetName())
	}

	return names, nil
}

// TestRun_ReflectionEnabledByDefaultWithoutAuth is the whole point of the feature: an
// unauthenticated instance answers a schema discovery request, so grpcurl and every gRPC GUI work
// against it with nothing handed to them.
func TestRun_ReflectionEnabledByDefaultWithoutAuth(t *testing.T) {
	grpcPort, _ := baseRunConfig(t)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- run(ctx, versionInfo{}) }()

	waitForGRPCListener(t, grpcPort)

	names, err := listServices(t, grpcPort)
	if err != nil {
		t.Fatalf("reflection call failed on an instance that should serve it: %v", err)
	}

	found := false

	for _, name := range names {
		if name != "hippocampus.v1.Hippocampus" {
			continue
		}

		found = true

		break
	}

	if !found {
		t.Errorf("reflection listed %v, which does not include hippocampus.v1.Hippocampus", names)
	}

	cancel()

	select {

	case err := <-done:
		if err != nil {
			t.Fatalf("run returned an error: %v", err)
		}

	case <-time.After(20 * time.Second):
		t.Fatal("run did not return after cancellation")

	}
}

// TestRun_ReflectionOffUnderAuth is the security half: an instance configured for anybody but its
// owner does not publish its schema to an unauthenticated socket. The reflection service is a
// streaming RPC and so never reaches the unary auth interceptor - the only thing that keeps it off
// such an instance is its not being registered at all, which is what this asserts.
func TestRun_ReflectionOffUnderAuth(t *testing.T) {
	grpcPort, _ := baseRunConfig(t)

	viper.Set("auth.method", "hmac")
	viper.Set("auth.signingSecret", "a-test-signing-secret-long-enough")

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- run(ctx, versionInfo{}) }()

	waitForGRPCListener(t, grpcPort)

	if _, err := listServices(t, grpcPort); status.Code(err) != codes.Unimplemented {
		t.Errorf("expected Unimplemented from an authenticated instance, got %v", err)
	}

	cancel()

	select {

	case err := <-done:
		if err != nil {
			t.Fatalf("run returned an error: %v", err)
		}

	case <-time.After(20 * time.Second):
		t.Fatal("run did not return after cancellation")

	}
}
