package main

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
)

// maxSamplesPerClass caps what a class can accumulate between statistics ticks; at demo rates a
// tick collects a few thousand samples, so the cap only guards against pathological bursts.
const maxSamplesPerClass = 100_000

// latencyTracker collects per-class RPC latencies between statistics ticks. Percentiles are
// interval-scoped: each tick's line reflects only the last interval, so a stall while a sleep
// cycle holds the database connection shows up in that tick rather than being averaged away over
// the whole run.
type latencyTracker struct {
	mu      sync.Mutex
	samples map[string][]time.Duration
}

type latencySummary struct {
	count int
	p50   time.Duration
	p95   time.Duration
	p99   time.Duration
	max   time.Duration
}

func newLatencyTracker() *latencyTracker {
	return &latencyTracker{samples: make(map[string][]time.Duration)}
}

// latencyClasses fixes the reporting order of the per-class log lines.
var latencyClasses = []string{"write", "read", "recall", "sleep", "other"}

// rpcClass maps a full gRPC method name onto a coarse reporting bucket, so the stats output
// stays a handful of lines. Sleep gets its own class: the manual Sleep RPC's latency is the
// consolidation cycle's duration, the single most interesting number under load.
func rpcClass(method string) string {
	name := method[strings.LastIndex(method, "/")+1:]

	switch name {

	case "StoreMemory", "StoreEvent", "EndEvent", "UpdateEventSignificance":
		return "write"

	case "GetMemories", "GetEvents", "GetEventById", "SearchMemories":
		return "read"

	case "RecallMemories":
		return "recall"

	case "Sleep":
		return "sleep"
	}

	return "other"
}

func (t *latencyTracker) observe(class string, d time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if len(t.samples[class]) >= maxSamplesPerClass {
		return
	}

	t.samples[class] = append(t.samples[class], d)
}

// drain returns each class's summary for the interval since the previous drain, and resets.
func (t *latencyTracker) drain() map[string]latencySummary {
	t.mu.Lock()
	samples := t.samples
	t.samples = make(map[string][]time.Duration)
	t.mu.Unlock()

	out := make(map[string]latencySummary, len(samples))

	for k, v := range samples {
		sort.Slice(v, func(i int, j int) bool { return v[i] < v[j] })

		out[k] = latencySummary{
			count: len(v),
			p50:   quantile(v, 0.50),
			p95:   quantile(v, 0.95),
			p99:   quantile(v, 0.99),
			max:   v[len(v)-1],
		}
	}

	return out
}

// quantile returns the nearest-rank quantile of an ascending-sorted, non-empty sample set.
func quantile(sorted []time.Duration, q float64) time.Duration {
	i := int(q * float64(len(sorted)-1))

	return sorted[i]
}

// unaryLatencyInterceptor times every RPC the generator issues, failures included — a call
// stalled behind a sleep cycle is exactly what the percentiles exist to expose.
func unaryLatencyInterceptor(t *latencyTracker) grpc.UnaryClientInterceptor {
	return func(
		ctx context.Context,
		method string,
		req any,
		reply any,
		cc *grpc.ClientConn,
		invoker grpc.UnaryInvoker,
		opts ...grpc.CallOption,
	) error {
		started := time.Now()
		err := invoker(ctx, method, req, reply, cc, opts...)
		t.observe(rpcClass(method), time.Since(started))

		return err
	}
}
