package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/grpc"
)

func TestRpcClass(t *testing.T) {
	cases := map[string]string{
		"/proto.Hippocampus/StoreMemory":             "write",
		"/proto.Hippocampus/StoreEvent":              "write",
		"/proto.Hippocampus/EndEvent":                "write",
		"/proto.Hippocampus/UpdateEventSignificance": "write",
		"/proto.Hippocampus/GetMemories":             "read",
		"/proto.Hippocampus/GetEvents":               "read",
		"/proto.Hippocampus/GetEventById":            "read",
		"/proto.Hippocampus/SearchMemories":          "read",
		"/proto.Hippocampus/RecallMemories":          "recall",
		"/proto.Hippocampus/Sleep":                   "sleep",
		"/proto.Hippocampus/Purge":                   "other",
		"NoSlash":                                    "other",
	}

	for method, want := range cases {
		if got := rpcClass(method); got != want {
			t.Errorf("rpcClass(%q) = %q, want %q", method, got, want)
		}
	}
}

func TestLatencyTrackerObserveAndDrain(t *testing.T) {
	lt := newLatencyTracker()

	lt.observe("write", 10*time.Millisecond)
	lt.observe("write", 30*time.Millisecond)
	lt.observe("write", 20*time.Millisecond)
	lt.observe("read", 5*time.Millisecond)

	summaries := lt.drain()

	write, ok := summaries["write"]
	if !ok {
		t.Fatalf("expected a write summary")
	}

	if write.count != 3 {
		t.Errorf("write.count = %d, want 3", write.count)
	}

	if write.max != 30*time.Millisecond {
		t.Errorf("write.max = %s, want 30ms", write.max)
	}

	if _, ok := summaries["read"]; !ok {
		t.Fatalf("expected a read summary")
	}

	// drain resets internal state.
	if empty := lt.drain(); len(empty) != 0 {
		t.Errorf("expected drain after drain to be empty, got %d classes", len(empty))
	}
}

func TestLatencyTrackerObserveCap(t *testing.T) {
	lt := newLatencyTracker()

	for i := 0; i < maxSamplesPerClass+10; i++ {
		lt.observe("write", time.Millisecond)
	}

	summaries := lt.drain()

	if summaries["write"].count != maxSamplesPerClass {
		t.Errorf("count = %d, want cap %d", summaries["write"].count, maxSamplesPerClass)
	}
}

func TestQuantile(t *testing.T) {
	sorted := []time.Duration{
		1 * time.Millisecond,
		2 * time.Millisecond,
		3 * time.Millisecond,
		4 * time.Millisecond,
		5 * time.Millisecond,
	}

	if got := quantile(sorted, 0); got != 1*time.Millisecond {
		t.Errorf("quantile(0) = %s, want 1ms", got)
	}

	if got := quantile(sorted, 1); got != 5*time.Millisecond {
		t.Errorf("quantile(1) = %s, want 5ms", got)
	}
}

func TestUnaryLatencyInterceptor(t *testing.T) {
	lt := newLatencyTracker()
	interceptor := unaryLatencyInterceptor(lt)

	okInvoker := func(
		ctx context.Context,
		method string,
		req any,
		reply any,
		cc *grpc.ClientConn,
		opts ...grpc.CallOption,
	) error {
		return nil
	}

	failErr := errors.New("boom")
	failInvoker := func(
		ctx context.Context,
		method string,
		req any,
		reply any,
		cc *grpc.ClientConn,
		opts ...grpc.CallOption,
	) error {
		return failErr
	}

	if err := interceptor(context.Background(), "/proto.Hippocampus/StoreMemory", nil, nil, nil, okInvoker); err != nil {
		t.Errorf("unexpected error: %s", err)
	}

	if err := interceptor(context.Background(), "/proto.Hippocampus/Sleep", nil, nil, nil, failInvoker); !errors.Is(err, failErr) {
		t.Errorf("expected failErr, got %v", err)
	}

	summaries := lt.drain()

	if summaries["write"].count != 1 {
		t.Errorf("write count = %d, want 1", summaries["write"].count)
	}

	if summaries["sleep"].count != 1 {
		t.Errorf("sleep count = %d, want 1", summaries["sleep"].count)
	}
}
