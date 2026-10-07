package main

import (
	"context"
	"testing"
	"time"

	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"

	"github.com/fastbean-au/hippocampus/contract"
)

// captureSampleLogs records log entries at info and above for the rest of the test. The level is
// pinned because the logger is global and other tests in this package change it; below info the
// sample line would never reach the hook.
func captureSampleLogs(t *testing.T) *logtest.Hook {
	t.Helper()

	level := log.GetLevel()
	log.SetLevel(log.InfoLevel)

	hook := logtest.NewGlobal()

	t.Cleanup(func() {
		hook.Reset()
		log.SetLevel(level)
	})

	return hook
}

// populationSample returns the fields of the "population sample" line reporting these totals. The
// logger is global and other tests in this package leave generators running that log their own
// samples, so the line is identified by what this test's store holds rather than by being first.
func populationSample(t *testing.T, hook *logtest.Hook, memories int32, events int32) log.Fields {
	t.Helper()

	for _, entry := range hook.AllEntries() {
		if entry.Message == "population sample" && entry.Data["mem_total"] == memories && entry.Data["evt_total"] == events {
			return entry.Data
		}
	}

	t.Fatalf("sample logged no population sample line for %d memories and %d events", memories, events)

	return nil
}

func TestSignificanceBandKey(t *testing.T) {
	want := []string{"sig_1_25", "sig_26_50", "sig_51_75", "sig_76_100"}

	for i, w := range want {
		if got := significanceBandKey(i); got != w {
			t.Errorf("significanceBandKey(%d) = %q, want %q", i, got, w)
		}
	}

	// Anything >= 3 (including out of range) falls into the default branch.
	if got := significanceBandKey(99); got != "sig_76_100" {
		t.Errorf("significanceBandKey(99) = %q, want sig_76_100", got)
	}
}

func TestCountMemoriesAndEvents(t *testing.T) {
	client := newFakeHippoClient()
	g := New(Config{Seed: 1}, client, newLatencyTracker())

	// Empty store.
	if got := g.countMemories(context.Background(), 0, 0); got != 0 {
		t.Errorf("countMemories(empty) = %d, want 0", got)
	}

	if got := g.countEvents(context.Background()); got != 0 {
		t.Errorf("countEvents(empty) = %d, want 0", got)
	}

	client.memories["m1"] = &contract.Memory{Id: "m1", Significance: 10}
	client.memories["m2"] = &contract.Memory{Id: "m2", Significance: 60}
	client.events["e1"] = &contract.Event{Id: "e1"}

	if got := g.countMemories(context.Background(), 0, 0); got != 2 {
		t.Errorf("countMemories(all) = %d, want 2", got)
	}

	if got := g.countMemories(context.Background(), 1, 25); got != 2 {
		// The fake ignores band filtering and returns everything, mirroring only the "band set"
		// code path being exercised (sigMin/sigMax > 0).
		t.Errorf("countMemories(band) = %d, want 2 (fake returns all)", got)
	}

	if got := g.countEvents(context.Background()); got != 1 {
		t.Errorf("countEvents() = %d, want 1", got)
	}
}

func TestCountMemoriesAndEventsError(t *testing.T) {
	client := newFakeHippoClient()
	client.errOn["GetMemories"] = 1
	client.errOn["GetEvents"] = 1

	g := New(Config{Seed: 1}, client, newLatencyTracker())

	if got := g.countMemories(context.Background(), 0, 0); got != 0 {
		t.Errorf("countMemories() on error = %d, want 0", got)
	}

	if got := g.countEvents(context.Background()); got != 0 {
		t.Errorf("countEvents() on error = %d, want 0", got)
	}
}

func TestSample(t *testing.T) {
	client := newFakeHippoClient()
	client.memories["m1"] = &contract.Memory{Id: "m1", Significance: 10}
	client.events["e1"] = &contract.Event{Id: "e1"}

	g := New(Config{Seed: 1}, client, newLatencyTracker())

	hook := captureSampleLogs(t)

	g.sample(context.Background())

	fields := populationSample(t, hook, 1, 1)

	if fields["mem_per_evt"] != float64(1) {
		t.Errorf("expected mem_per_evt 1, got %v", fields["mem_per_evt"])
	}

	if _, ok := fields["sig_1_25_pct"]; !ok {
		t.Error("a non-empty store must report each band's share")
	}
}

func TestSampleEmptyStore(t *testing.T) {
	client := newFakeHippoClient()
	g := New(Config{Seed: 1}, client, newLatencyTracker())

	hook := captureSampleLogs(t)

	// memTotal and evtTotal both 0: the mem_per_evt and _pct fields are skipped (divide-by-zero
	// guards).
	g.sample(context.Background())

	fields := populationSample(t, hook, 0, 0)

	for _, key := range []string{"mem_per_evt", "sig_1_25_pct", "sig_76_100_pct"} {
		if _, ok := fields[key]; ok {
			t.Errorf("an empty store must not report %s (it would divide by zero)", key)
		}
	}
}

func TestSampleLoop(t *testing.T) {
	client := newFakeHippoClient()
	client.memories["m1"] = &contract.Memory{Id: "m1", Significance: 10}

	g := New(Config{Seed: 1, SampleInterval: 5 * time.Millisecond}, client, newLatencyTracker())

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() {
		g.sampleLoop(ctx)
		close(done)
	}()

	select {

	case <-done:

	case <-time.After(2 * time.Second):
		t.Fatal("sampleLoop() did not return after context cancellation")

	}

	if client.callCount("GetMemories") == 0 {
		t.Error("expected sampleLoop() to have ticked at least once")
	}
}
