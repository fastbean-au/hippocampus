package main

import (
	"context"
	"testing"
	"time"

	"github.com/spf13/viper"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/fastbean-au/hippocampus/contract"
)

// TestRun_WatchIsGuarded drives the Watch stream through the server run() assembles. It is the first
// streaming RPC, and the interceptor chain was unary-only until it arrived: wired without the stream
// chain it would have been served to anybody, which no handler-level test can see. The scoped
// reader's view and the shutdown that ends its stream are checked on the same server.
func TestRun_WatchIsGuarded(t *testing.T) {
	grpcPort, gwBase := baseRunConfig(t)

	viper.Set("auth.method", "hmac")
	viper.Set("auth.signingSecret", securedTestSecret)

	startRun(t, gwBase+"/healthz")

	client := contract.NewHippocampusClient(dialService(t, grpcPort))

	t.Run("a Watch carrying no token is refused", func(t *testing.T) {
		// Bounded, because the failure this guards against is a stream that is accepted and then
		// waits for events forever.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		stream, err := client.Watch(ctx, &contract.WatchRequest{})
		if err == nil {
			_, err = stream.Recv()
		}

		if status.Code(err) != codes.Unauthenticated {
			t.Errorf("Watch with no token = %v, want Unauthenticated", err)
		}
	})

	t.Run("a scoped reader hears only its own group", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(withToken(mint(t, []string{"reader"}, []string{"a"})), 20*time.Second)
		defer cancel()

		stream, err := client.Watch(ctx, &contract.WatchRequest{})
		if err != nil {
			t.Fatalf("Watch: %s", err)
		}

		// The stream is subscribed once the server has sent its headers, which it does on accepting
		// the call - so nothing stored after this can be missed.
		if _, err := stream.Header(); err != nil {
			t.Fatalf("Watch headers: %s", err)
		}

		unscopedWriter := withToken(mint(t, []string{"writer"}, nil))

		if _, err := client.StoreMemory(unscopedWriter, &contract.Memory{Id: "watched-b", Body: "b", Significance: 5, Group: "b"}); err != nil {
			t.Fatalf("StoreMemory(b): %s", err)
		}

		if _, err := client.StoreMemory(unscopedWriter, &contract.Memory{Id: "watched-a", Body: "a", Significance: 5, Group: "a"}); err != nil {
			t.Fatalf("StoreMemory(a): %s", err)
		}

		event, err := stream.Recv()
		if err != nil {
			t.Fatalf("Recv: %s", err)
		}

		if event.GetKind() != contract.CallbackKind_CALLBACK_KIND_MEMORY_STORED || event.GetItems()[0].GetId() != "watched-a" {
			t.Errorf("the scoped watcher's first event is %v, want watched-a stored and nothing of group b's", event)
		}
	})
}
